package session_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// recordingRepo notes which candidate queries the sweeper issued and with what limit.
type recordingRepo struct {
	session.Repository
	mu     sync.Mutex
	calls  []string
	limits map[string]int
}

func (r *recordingRepo) note(name string, limit int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
	if r.limits == nil {
		r.limits = map[string]int{}
	}
	r.limits[name] = limit
}

func (r *recordingRepo) ListHandoffStale(ctx context.Context, t time.Time, l int) ([]uuid.UUID, error) {
	r.note("e", l)
	return r.Repository.ListHandoffStale(ctx, t, l)
}

func (r *recordingRepo) ListExpired(ctx context.Context, t time.Time, l int) ([]uuid.UUID, error) {
	r.note("a", l)
	return r.Repository.ListExpired(ctx, t, l)
}

func (r *recordingRepo) ListActiveForHeartbeat(ctx context.Context, l int) ([]session.ActiveRef, error) {
	r.note("b", l)
	return r.Repository.ListActiveForHeartbeat(ctx, l)
}

func (r *recordingRepo) ListStuckEnding(ctx context.Context, t time.Time, l int) ([]uuid.UUID, error) {
	r.note("c", l)
	return r.Repository.ListStuckEnding(ctx, t, l)
}

func (r *recordingRepo) ListStuckStarting(ctx context.Context, t time.Time, l int) ([]uuid.UUID, error) {
	r.note("d", l)
	return r.Repository.ListStuckStarting(ctx, t, l)
}

type sweepEnv struct {
	*env
	hb   *fakes.Heartbeats
	repo *recordingRepo
	w    *session.Sweeper
}

func newSweepEnv(t *testing.T, available int64) *sweepEnv {
	t.Helper()
	e := newEnv(t, available)
	s := &sweepEnv{env: e, hb: fakes.NewHeartbeats(), repo: &recordingRepo{Repository: e.store.Reader()}}
	s.w = session.NewSweeper(e.svc, s.repo, s.hb, e.clk, slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return s
}

// activeFor walks a session of another user to ACTIVE at the current fake time.
func (e *env) activeFor(t *testing.T, duration int) (session.Session, int64, uuid.UUID) {
	t.Helper()
	uid := uuid.Must(uuid.NewV7())
	require.NoError(t, e.bal.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return balance.OpenAccount(ctx, tx, e.ids, uid, int64(duration)*2, e.clk.Now())
	}))
	in := e.input("key-" + uuid.NewString()[:8])
	in.UserID, in.DurationSeconds = uid, duration
	res, err := e.svc.Create(context.Background(), in)
	require.NoError(t, err)
	fence := e.claim(t, res.Session.ID, session.StatusReserved)
	_, err = e.svc.BeginStart(context.Background(), res.Session.ID, fence)
	require.NoError(t, err)
	got, err := e.svc.Activate(context.Background(), res.Session.ID, fence)
	require.NoError(t, err)
	return got, fence, uid
}

func (s *sweepEnv) tick(t *testing.T, now time.Time) session.SweepReport {
	t.Helper()
	s.clk.Set(now)
	rep, err := s.w.Tick(context.Background())
	require.NoError(t, err)
	return rep
}

func TestSweeperRuleAExpired(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 300)
	_, err := s.svc.AddDisconnected(context.Background(), sess.ID, fence, 20)
	require.NoError(t, err)

	rep := s.tick(t, at(10, 5, 14, 0)) // expires_at + 14 s
	require.Zero(t, rep.Rule["a"].OK+rep.Rule["a"].Noop+rep.Rule["a"].Error)
	require.Equal(t, session.StatusActive, s.store.Session(sess.ID).Status)

	rep = s.tick(t, at(10, 5, 16, 0)) // + 16 s
	require.Equal(t, 1, rep.Rule["a"].OK)
	got := s.store.Session(sess.ID)
	require.Equal(t, session.StatusCompleted, got.Status)
	require.Equal(t, session.EndDurationElapsed, *got.EndReason)
	require.Equal(t, 280, got.ChargedSeconds, "example 7: 300 s minus 20 s disconnected, cut at expires_at")
	require.Equal(t, at(10, 5, 16, 0), *got.EndedAt)
	require.Equal(t, session.ActorSweeper, s.store.Events(sess.ID)[len(s.store.Events(sess.ID))-1].Actor)
	require.Equal(t, "WORKER", s.store.Outbox.Events()[0].Producer)
}

func TestSweeperRuleCEndingStuck(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 600)
	s.clk.Set(at(10, 7, 0, 0))
	_, err := s.svc.BeginEnding(context.Background(), sess.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)

	rep := s.tick(t, at(10, 7, 59, 0))
	require.Zero(t, rep.Rule["c"].OK)
	require.Equal(t, session.StatusEnding, s.store.Session(sess.ID).Status)
	rep = s.tick(t, at(10, 8, 1, 0))
	require.Equal(t, 1, rep.Rule["c"].OK)
	got := s.store.Session(sess.ID)
	require.Equal(t, session.StatusCompleted, got.Status)
	require.Equal(t, 420, got.ChargedSeconds, "billable_end stays at ending_at")
	require.Equal(t, session.EndUserEnded, *got.EndReason, "the stored reason is kept")
}

func TestSweeperRuleDStartingStuck(t *testing.T) {
	s := newSweepEnv(t, 1800)
	s.clk.Set(tenAM)
	sess := s.reserved(t, "key-0000001")
	fence := s.claim(t, sess.ID, session.StatusReserved)
	_, err := s.svc.BeginStart(context.Background(), sess.ID, fence)
	require.NoError(t, err)

	rep := s.tick(t, at(10, 0, 29, 0))
	require.Zero(t, rep.Rule["d"].OK)
	rep = s.tick(t, at(10, 0, 31, 0))
	require.Equal(t, 1, rep.Rule["d"].OK)
	got := s.store.Session(sess.ID)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Equal(t, session.EndServerError, *got.EndReason)
	require.Equal(t, "starting_stuck", *got.FailureDetail)
	require.Zero(t, got.ChargedSeconds)
	require.Equal(t, int64(1800), s.bal.Account(s.userID).AvailableSeconds)
	require.Zero(t, s.store.Outbox.Len())
}

func TestSweeperRuleEHandoffNotResumedExample10(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 600)
	s.clk.Set(at(10, 3, 0, 0))
	_, err := s.svc.MarkHandoff(context.Background(), sess.ID, fence)
	require.NoError(t, err)

	rep := s.tick(t, at(10, 3, 29, 0))
	require.Zero(t, rep.Rule["e"].OK)
	rep = s.tick(t, at(10, 3, 31, 0))
	require.Equal(t, 1, rep.Rule["e"].OK)
	got := s.store.Session(sess.ID)
	require.Equal(t, session.StatusCompleted, got.Status)
	require.Equal(t, session.EndReconnectTimeout, *got.EndReason)
	require.Equal(t, at(10, 3, 0, 0), *got.EndingAt, "ending_at = handoff_at")
	require.Equal(t, 180, got.ChargedSeconds, "example 10")
}

func TestSweeperRuleEOnlyTakesStaleHandoffs(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 600)
	s.clk.Set(at(10, 3, 0, 0))
	_, err := s.svc.MarkHandoff(context.Background(), sess.ID, fence)
	require.NoError(t, err)
	// a new runtime resumes just before the sweeper looks: handoff_at is cleared
	_, _, err = s.svc.ClaimRuntime(context.Background(), sess.ID, []session.Status{session.StatusActive})
	require.NoError(t, err)
	got, changed, err := s.svc.EndStaleHandoff(context.Background(), sess.ID, at(10, 3, 1, 0))
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, session.StatusActive, got.Status, "the resumed session is left alone")
}

func TestSweeperHandoffSessionPastExpiryIsClosedByRuleE(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 300)
	s.clk.Set(at(10, 4, 0, 0))
	_, err := s.svc.MarkHandoff(context.Background(), sess.ID, fence)
	require.NoError(t, err)

	rep := s.tick(t, at(10, 6, 0, 0)) // past both handoff + 30 s and expires_at + 15 s
	require.Equal(t, 1, rep.Rule["e"].OK)
	require.Zero(t, rep.Rule["a"].OK, "rule (a) finds nothing: (e) came first")
	got := s.store.Session(sess.ID)
	require.Equal(t, session.EndReconnectTimeout, *got.EndReason)
	require.Equal(t, at(10, 4, 0, 0), *got.EndingAt)
	require.Equal(t, 240, got.ChargedSeconds)
}

func TestSweeperRuleBHeartbeatExample9(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, _ := s.active(t, 900)
	s.hb.Beat(sess.ID, at(10, 5, 0, 0))

	rep := s.tick(t, at(10, 5, 10, 0)) // Redis seen for the first time: (b) stays off
	require.True(t, rep.Rule["b"].Skipped)
	require.Equal(t, session.StatusActive, s.store.Session(sess.ID).Status)
	rep = s.tick(t, at(10, 5, 40, 0))
	require.True(t, rep.Rule["b"].Skipped, "30 s of health is not 45 s")
	rep = s.tick(t, at(10, 6, 0, 0))
	require.False(t, rep.Rule["b"].Skipped)
	require.Equal(t, 1, rep.Rule["b"].OK)

	got := s.store.Session(sess.ID)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Equal(t, session.EndServerError, *got.EndReason)
	require.Equal(t, at(10, 5, 0, 0), *got.EndedAt, "ended_at = the last heartbeat")
	require.Equal(t, 270, got.ChargedSeconds, "example 9")
	require.Equal(t, "heartbeat_lost", *got.FailureDetail)
}

func TestSweeperRuleBMissingKeyUsesNowMinus45sButNotBeforeStart(t *testing.T) {
	s := newSweepEnv(t, 1800)
	old, _ := s.active(t, 900) // started 10:00:00
	_ = s.tick(t, at(10, 5, 0, 0))
	rep := s.tick(t, at(10, 6, 0, 0))
	require.Equal(t, 1, rep.Rule["b"].OK)
	require.Equal(t, at(10, 5, 15, 0), *s.store.Session(old.ID).EndedAt, "now − 45 s")
	require.Equal(t, 285, s.store.Session(old.ID).ChargedSeconds, "315 s active minus 30 s for a service fault")

	// a session younger than 45 s is never a candidate, however silent
	young, _, _ := s.activeFor(t, 300)
	s.hb.SetDown(false)
	rep = s.tick(t, s.clk.Now().Add(20*time.Second))
	require.Zero(t, rep.Rule["b"].OK+rep.Rule["b"].Noop+rep.Rule["b"].Error)
	require.Equal(t, session.StatusActive, s.store.Session(young.ID).Status)
}

func TestSweeperRuleBNeverTouchesHandedOffSessions(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 900)
	s.clk.Set(at(10, 0, 50, 0))
	_, err := s.svc.MarkHandoff(context.Background(), sess.ID, fence)
	require.NoError(t, err)
	_ = s.tick(t, at(10, 1, 0, 0))
	rep := s.tick(t, at(10, 1, 20, 0)) // (e) needs 30 s after the handoff: not yet
	require.Zero(t, rep.Rule["b"].OK+rep.Rule["b"].Noop+rep.Rule["b"].Error)
	require.Equal(t, session.StatusActive, s.store.Session(sess.ID).Status)
}

func TestTickRuleOrderEABCD(t *testing.T) {
	s := newSweepEnv(t, 1800)
	s.tick(t, tenAM) // the guard is off during the first 45 s of Redis health: (b) does not even list
	require.Equal(t, []string{"e", "a", "c", "d"}, s.repo.calls)
	s.repo.calls = nil
	s.tick(t, tenAM.Add(time.Minute))
	require.Equal(t, []string{"e", "a", "b", "c", "d"}, s.repo.calls)
	for rule, limit := range s.repo.limits {
		if rule == "b" {
			require.Equal(t, 5000, limit)
		} else {
			require.Equal(t, 200, limit, rule)
		}
	}
}

func TestSweeperMassHeartbeatLossIsSkipped(t *testing.T) {
	s := newSweepEnv(t, 1800)
	s.clk.Set(tenAM)
	var ids []uuid.UUID
	for i := 0; i < 40; i++ {
		sess, _, _ := s.activeFor(t, 300)
		ids = append(ids, sess.ID)
	}
	// the first 10 sessions keep beating; 30 of 40 look dead
	for _, id := range ids[:10] {
		s.hb.Beat(id, at(10, 0, 55, 0))
	}
	_ = s.tick(t, at(10, 0, 50, 0))
	rep := s.tick(t, at(10, 1, 40, 0))
	require.True(t, rep.Rule["b"].Skipped)
	require.Contains(t, s.logs.String(), "sweeper.mass_heartbeat_loss")
	for _, id := range ids {
		require.Equal(t, session.StatusActive, s.store.Session(id).Status)
	}
}

func TestSweeperHandedOffSessionsDoNotCountInTheMassLossRatio(t *testing.T) {
	s := newSweepEnv(t, 1800)
	s.clk.Set(tenAM)
	var live []uuid.UUID
	for i := 0; i < 30; i++ { // 20 of these 30 lose their heartbeat: 67 %
		sess, _, _ := s.activeFor(t, 300)
		live = append(live, sess.ID)
	}
	for i := 0; i < 20; i++ { // handed off: not in numerator or denominator
		sess, fence, _ := s.activeFor(t, 300)
		_, err := s.svc.MarkHandoff(context.Background(), sess.ID, fence)
		require.NoError(t, err)
	}
	for _, id := range live[:10] {
		s.hb.Beat(id, at(10, 0, 55, 0))
	}
	_ = s.tick(t, at(10, 0, 50, 0))
	rep := s.tick(t, at(10, 1, 40, 0))
	require.True(t, rep.Rule["b"].Skipped, "20 of 30 non-handoff sessions is mass loss; counting the 20 handed off would hide it")
}

func TestSweeperNineteenOfTwentyAreStillProcessed(t *testing.T) {
	s := newSweepEnv(t, 1800)
	s.clk.Set(tenAM)
	var ids []uuid.UUID
	for i := 0; i < 20; i++ {
		sess, _, _ := s.activeFor(t, 300)
		ids = append(ids, sess.ID)
	}
	s.hb.Beat(ids[0], at(10, 1, 30, 0))
	_ = s.tick(t, at(10, 0, 50, 0))
	rep := s.tick(t, at(10, 1, 40, 0))
	require.False(t, rep.Rule["b"].Skipped, "19 candidates is below the threshold of 20")
	require.Equal(t, 19, rep.Rule["b"].OK)
	require.Equal(t, session.StatusActive, s.store.Session(ids[0]).Status)
}

func TestSweeperRedisDownKeepsPostgresRulesRunning(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, _ := s.active(t, 300)
	s.hb.SetDown(true)
	rep := s.tick(t, at(10, 5, 16, 0))
	require.True(t, rep.Rule["b"].Skipped)
	require.Equal(t, 1, rep.Rule["a"].OK, "rule (a) needs only Postgres")
	require.Equal(t, session.StatusCompleted, s.store.Session(sess.ID).Status)

	// and a heartbeat read failing in the middle of (b) cancels (b) for that tick
	s.hb.SetDown(false)
	other, _, _ := s.activeFor(t, 900)
	_ = s.tick(t, s.clk.Now().Add(time.Second))
	s.hb.FailNext("Read", errors.New("conn reset")) // the probe fails: guard resets
	rep = s.tick(t, s.clk.Now().Add(time.Minute))
	require.True(t, rep.Rule["b"].Skipped)
	require.Equal(t, session.StatusActive, s.store.Session(other.ID).Status)
}

func TestSweeperOneSessionErrorDoesNotStopTheLoop(t *testing.T) {
	s := newSweepEnv(t, 1800)
	a, _ := s.active(t, 300)
	b, _, _ := s.activeFor(t, 300)
	s.store.FailNext("Sessions.CAS", errors.New("deadlock"))
	rep := s.tick(t, at(10, 5, 16, 0))
	require.Equal(t, 1, rep.Rule["a"].Error)
	require.Equal(t, 1, rep.Rule["a"].OK, "the other session was still handled")
	states := map[session.Status]int{s.store.Session(a.ID).Status: 1}
	states[s.store.Session(b.ID).Status]++
	require.Equal(t, 1, states[session.StatusCompleted])

	rep = s.tick(t, at(10, 5, 30, 0))
	require.Equal(t, 1, rep.Rule["a"].OK, "the next tick finishes the one that failed")
}

func TestSweeperLosingToTheOwnerIsANoop(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, fence := s.active(t, 300)
	s.clk.Set(at(10, 5, 16, 0))
	// the owner finishes right between the candidate query and the sweeper's action
	_, err := s.svc.BeginEnding(context.Background(), sess.ID, &fence, session.EndDurationElapsed, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = s.svc.Finalize(context.Background(), sess.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	rep, err := s.w.Tick(context.Background())
	require.NoError(t, err)
	require.Zero(t, rep.Rule["a"].Error, "an already finished session is not a candidate any more")
	require.Equal(t, 1, s.store.Outbox.Len())
}

func TestSweepReport_HeartbeatFlags(t *testing.T) {
	s := newSweepEnv(t, 1800)
	s.clk.Set(tenAM)
	var ids []uuid.UUID
	for i := 0; i < 40; i++ {
		sess, _, _ := s.activeFor(t, 300)
		ids = append(ids, sess.ID)
	}
	for _, id := range ids[:10] {
		s.hb.Beat(id, at(10, 0, 55, 0))
	}
	rep := s.tick(t, at(10, 0, 50, 0))
	require.True(t, rep.HeartbeatSuspended, "the guard has only just seen Redis")
	require.False(t, rep.MassHeartbeatLoss)

	rep = s.tick(t, at(10, 1, 40, 0))
	require.True(t, rep.MassHeartbeatLoss)
	require.False(t, rep.HeartbeatSuspended)
	require.Equal(t, 30, rep.HeartbeatCandidates)
	require.Equal(t, 40, rep.ActiveNonHandoff)

	// Redis going away suspends (b) and clears the mass-loss flag of the previous tick
	s.hb.SetDown(true)
	rep = s.tick(t, at(10, 1, 50, 0))
	require.True(t, rep.HeartbeatSuspended)
	require.False(t, rep.MassHeartbeatLoss)
}

func TestSweepReport_HealthyTickHasNoFlags(t *testing.T) {
	s := newSweepEnv(t, 1800)
	sess, _ := s.active(t, 900)
	s.hb.Beat(sess.ID, at(10, 1, 30, 0))
	_ = s.tick(t, at(10, 0, 50, 0))
	rep := s.tick(t, at(10, 1, 40, 0))
	require.False(t, rep.HeartbeatSuspended)
	require.False(t, rep.MassHeartbeatLoss)
	require.Zero(t, rep.HeartbeatCandidates)
	require.Equal(t, 1, rep.ActiveNonHandoff)
}
