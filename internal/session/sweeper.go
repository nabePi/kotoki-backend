package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// Limits and thresholds of spec 5.6.4 and 9.9.2.
const (
	sweepBatch            = 200  // sessions per rule per tick
	heartbeatScanLimit    = 5000 // ACTIVE sessions read for rule (b)
	heartbeatReadBatch    = 500  // keys per MGET
	massLossMinCandidates = 20
	expiryGrace           = 15 * time.Second
	endingStuckAfter      = 60 * time.Second
	startingStuckAfter    = 30 * time.Second
	handoffStaleAfter     = 30 * time.Second
)

// RuleResult counts what one rule did in one tick; Skipped is set when a safeguard kept the rule
// from running at all.
type RuleResult struct {
	OK, Noop, Error int
	Skipped         bool
}

// SweepReport is what one Tick did, per rule ("e", "a", "b", "c", "d").
type SweepReport struct {
	Rule map[string]RuleResult
}

// Sweeper closes sessions whose owner is gone or whose timers ran out (spec 5.6.4). Every session
// goes through its own Service call (own transaction, fence nil, actor SWEEPER); an error on one
// session is logged and never stops the loop. Mechanics of the job (interval, advisory lock) are M6's.
type Sweeper struct {
	svc   *Service
	repo  Repository
	hb    HeartbeatReader
	clk   clock.Clock
	log   *slog.Logger
	guard *HeartbeatGuard
}

// NewSweeper builds the sweeper. hb.Read with no ids must work as a connectivity probe: it is how
// the guard learns whether Redis is reachable at the start of a tick.
func NewSweeper(s *Service, r Repository, hb HeartbeatReader, c clock.Clock, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sweeper{svc: s, repo: r, hb: hb, clk: c, log: log, guard: &HeartbeatGuard{}}
}

// Tick runs the rules in the order of spec 9.9.2: (e) (a) (b) (c) (d). Rules that need only
// Postgres run even when Redis is down.
func (w *Sweeper) Tick(ctx context.Context) (SweepReport, error) {
	rep := SweepReport{Rule: map[string]RuleResult{}}
	now := w.clk.Now()
	for _, rule := range []struct {
		name string
		run  func(context.Context, time.Time) (RuleResult, error)
	}{
		{"e", w.ruleHandoff}, {"a", w.ruleExpired}, {"b", w.ruleHeartbeat}, {"c", w.ruleEnding}, {"d", w.ruleStarting},
	} {
		res, err := rule.run(ctx, now)
		rep.Rule[rule.name] = res
		if err != nil {
			w.log.ErrorContext(ctx, "sweeper rule failed", "rule", rule.name, "error", err.Error())
		}
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
	}
	return rep, nil
}

// settle classifies the outcome of one session: a transition that lost to the owner (ErrInvalidTransition,
// ErrFenceLost, not found) is a no-op, anything else an error.
func (w *Sweeper) settle(ctx context.Context, rule string, id uuid.UUID, res *RuleResult, err error) {
	switch {
	case err == nil:
		res.OK++
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrFenceLost), errors.Is(err, ErrNotFound):
		res.Noop++
	default:
		res.Error++
		w.log.ErrorContext(ctx, "sweeper action failed", "rule", rule, "session_id", id.String(), "error", err.Error())
	}
}

// ruleHandoff (e): the runtime handed the session off and nobody resumed it for 30 s.
func (w *Sweeper) ruleHandoff(ctx context.Context, now time.Time) (RuleResult, error) {
	var res RuleResult
	cutoff := now.Add(-handoffStaleAfter)
	ids, err := w.repo.ListHandoffStale(ctx, cutoff, sweepBatch)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		_, changed, err := w.svc.EndStaleHandoff(ctx, id, cutoff)
		if err == nil && !changed {
			res.Noop++ // a new runtime claimed it first
			continue
		}
		if err == nil {
			_, err = w.svc.Finalize(ctx, id, nil, ActorSweeper)
		}
		w.settle(ctx, "e", id, &res, err)
	}
	return res, nil
}

// ruleExpired (a): ACTIVE well past expires_at, so the owner's timer did not fire.
func (w *Sweeper) ruleExpired(ctx context.Context, now time.Time) (RuleResult, error) {
	var res RuleResult
	ids, err := w.repo.ListExpired(ctx, now.Add(-expiryGrace), sweepBatch)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		_, err := w.svc.BeginEnding(ctx, id, nil, EndDurationElapsed, "", ActorSweeper)
		if err == nil {
			_, err = w.svc.Finalize(ctx, id, nil, ActorSweeper)
		}
		w.settle(ctx, "a", id, &res, err)
	}
	return res, nil
}

// ruleEnding (c): ENDING for over a minute, the runtime never finalized.
func (w *Sweeper) ruleEnding(ctx context.Context, now time.Time) (RuleResult, error) {
	var res RuleResult
	ids, err := w.repo.ListStuckEnding(ctx, now.Add(-endingStuckAfter), sweepBatch)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		_, err := w.svc.Finalize(ctx, id, nil, ActorSweeper)
		w.settle(ctx, "c", id, &res, err)
	}
	return res, nil
}

// ruleStarting (d): STARTING for over 30 s.
func (w *Sweeper) ruleStarting(ctx context.Context, now time.Time) (RuleResult, error) {
	var res RuleResult
	ids, err := w.repo.ListStuckStarting(ctx, now.Add(-startingStuckAfter), sweepBatch)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		_, err := w.svc.FailStart(ctx, id, nil, EndServerError, "starting_stuck", ActorSweeper)
		w.settle(ctx, "d", id, &res, err)
	}
	return res, nil
}

// ruleHeartbeat (b): the owner's heartbeat is gone. It is the only rule that trusts Redis, so it has
// two safeguards (spec 9.9.2): the HeartbeatGuard, and a skip when most ACTIVE sessions look dead at
// once, which points at a Redis flush or failover rather than at crashed pods.
func (w *Sweeper) ruleHeartbeat(ctx context.Context, now time.Time) (RuleResult, error) {
	var res RuleResult
	_, probe := w.hb.Read(ctx, nil)
	w.guard.Observe(probe == nil, now)
	if !w.guard.Enabled(now) {
		res.Skipped = true
		return res, nil
	}
	active, err := w.repo.ListActiveForHeartbeat(ctx, heartbeatScanLimit)
	if err != nil {
		return res, err
	}
	beats := map[uuid.UUID]time.Time{}
	for i := 0; i < len(active); i += heartbeatReadBatch {
		end := min(i+heartbeatReadBatch, len(active))
		ids := make([]uuid.UUID, 0, end-i)
		for _, a := range active[i:end] {
			ids = append(ids, a.ID)
		}
		got, err := w.hb.Read(ctx, ids)
		if err != nil {
			w.guard.Observe(false, now)
			res.Skipped = true
			return res, nil
		}
		for id, at := range got {
			beats[id] = at
		}
	}
	w.guard.Observe(true, now)

	type candidate struct {
		ref  ActiveRef
		last time.Time
	}
	var cands []candidate
	for _, a := range active {
		last, ok := beats[a.ID]
		if ok && now.Sub(last) <= heartbeatTimeout {
			continue
		}
		if !a.StartedAt.Before(now.Add(-heartbeatTimeout)) {
			continue // too young to judge
		}
		if !ok {
			last = now.Add(-heartbeatTimeout)
		}
		if last.Before(a.StartedAt) {
			last = a.StartedAt
		}
		cands = append(cands, candidate{a, last})
	}
	if len(cands) >= massLossMinCandidates && len(cands)*2 >= len(active) {
		w.log.ErrorContext(ctx, "sweeper.mass_heartbeat_loss", "candidates", len(cands), "active", len(active))
		res.Skipped = true
		return res, nil
	}
	for _, c := range cands {
		_, err := w.svc.FailActive(ctx, c.ref.ID, c.last, "heartbeat_lost")
		w.settle(ctx, "b", c.ref.ID, &res, err)
	}
	return res, nil
}
