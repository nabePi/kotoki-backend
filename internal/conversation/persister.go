package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// ErrPersisterStuck means the persister queue stayed above 64 jobs, or its head job kept failing,
// for more than 30 s (§7.12.3, §7.11.4): the loop ends the session with SERVER_ERROR.
var ErrPersisterStuck = errors.New("conversation: persister stuck for more than 30 s")

// ErrPersisterClosed means Submit came after Close or after the session context ended.
var ErrPersisterClosed = errors.New("conversation: persister closed")

const (
	persisterQueueCap   = 64               // §7.12.3
	persisterStuckAfter = 30 * time.Second // §7.12.3: queue > 64 (or failing) for longer → SERVER_ERROR
	persistOpTimeout    = 5 * time.Second  // one attempt; the statement_timeout of the store is 5 s too
	persistBackoffFirst = 100 * time.Millisecond
	persistBackoffCap   = 2 * time.Second
)

// PersistJob is one write of the persister: InsertTurn, UpdateUser, FinalizeTurn, SessionEvent or
// AddDisconnected.
type PersistJob interface{ persistKind() string }

// InsertTurn opens a turn row (§7.6.6).
type InsertTurn struct{ Turn TurnStart }

// UpdateUser writes the user side of a turn at stt.final. A queued UpdateUser of the same turn is
// replaced by a newer one (§7.12.3).
type UpdateUser struct {
	TurnID uuid.UUID
	User   TurnUser
}

// FinalizeTurn closes a turn.
type FinalizeTurn struct {
	TurnID uuid.UUID
	Final  TurnFinal
}

// SessionEvent appends a session_events row (§7.13.1 table); TurnID is set for BARGE_IN and
// TURN_FAILED. The payload never carries transcript text.
type SessionEvent struct {
	Type    string
	TurnID  *uuid.UUID
	Payload any
}

// AddDisconnected adds Seconds to sessions.disconnected_seconds (§7.10).
type AddDisconnected struct{ Seconds int }

func (InsertTurn) persistKind() string      { return "insert_turn" }
func (UpdateUser) persistKind() string      { return "update_user" }
func (FinalizeTurn) persistKind() string    { return "finalize_turn" }
func (SessionEvent) persistKind() string    { return "session_event" }
func (AddDisconnected) persistKind() string { return "add_disconnected" }

// DisconnectedAdder is the part of SessionRuntime behind AddDisconnected jobs.
type DisconnectedAdder interface {
	AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) error
}

// PersisterOption configures a Persister.
type PersisterOption func(*Persister)

// PersisterClock sets the clock of the stuck rule (default: system clock).
func PersisterClock(c clock.Clock) PersisterOption { return func(p *Persister) { p.clk = c } }

// PersisterSleep replaces the retry backoff wait (tests advance a fake clock instead).
func PersisterSleep(f func(ctx context.Context, d time.Duration) error) PersisterOption {
	return func(p *Persister) { p.sleep = f }
}

// PersisterOnFenceLost is called once, from the persister goroutine, when a write returned
// session.ErrFenceLost. It must not block (e.g. a non-blocking send to the loop).
func PersisterOnFenceLost(f func(error)) PersisterOption {
	return func(p *Persister) { p.onFenceLost = f }
}

// PersisterOnStuck is called once, from the persister goroutine, with ErrPersisterStuck. It must
// not block.
func PersisterOnStuck(f func(error)) PersisterOption { return func(p *Persister) { p.onStuck = f } }

// Persister is the per-session writer of session_turns / session_events (§7.6.6, §7.7.2): jobs
// run one at a time in submission order through the fenced ports, so a turn is always inserted
// before its user side and finalized last. Submit never blocks the loop (§7.12.3).
//
// Failure semantics:
//   - session.ErrFenceLost from any write: the queue is dropped, nothing is written again,
//     OnFenceLost fires once and fence_lost_total is incremented; Submit, Flush and Close then
//     return session.ErrFenceLost. The loop stops the runtime (§7.9.3) and must not count the
//     metric again.
//   - any other error: the same job is retried (backoff 100 ms doubling to 2 s, 5 s per attempt)
//     so the order is kept. When the head job has been failing, or more than 64 jobs have been
//     queued, for more than 30 s, OnStuck fires once with ErrPersisterStuck (§7.11.4 SERVER_ERROR);
//     the persister keeps trying until it is closed.
//
// Loop usage (Tasks 18–22): NewPersister → Start(sessionCtx) → Submit(job) per §7.6.6/§7.10/§7.13.1
// → Flush(ctx ≤ 5 s) before Complete/Fail/MarkHandoff → Close(ctx) when the runtime ends.
type Persister struct {
	sid         uuid.UUID
	turns       TurnRepository
	events      SessionEventWriter
	disc        DisconnectedAdder
	fence       int64
	m           *Metrics
	log         *slog.Logger
	clk         clock.Clock
	sleep       func(context.Context, time.Duration) error
	onFenceLost func(error)
	onStuck     func(error)

	mu        sync.Mutex
	queue     []PersistJob
	inflight  bool
	closed    bool // Submit refused
	stopped   bool // the worker has exited
	lost      bool
	overSince time.Time // queue above persisterQueueCap since
	failSince time.Time // head job failing since
	stuckSent bool
	changed   chan struct{} // closed and replaced when the persister becomes idle or stops
	wake      chan struct{}
	started   bool
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewPersister creates the persister of session sid writing with fence. sess receives the
// AddDisconnected jobs (the SessionRuntime). A nil log discards.
func NewPersister(sid uuid.UUID, turns TurnRepository, events SessionEventWriter, sess DisconnectedAdder, fence int64,
	m *Metrics, log *slog.Logger, opts ...PersisterOption,
) *Persister {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Persister{sid: sid, turns: turns, events: events, disc: sess, fence: fence, m: m, log: log,
		clk: clock.System{}, sleep: sleepCtx, changed: make(chan struct{}), wake: make(chan struct{}, 1),
		done: make(chan struct{})}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Start runs the worker until ctx ends or Close has drained the queue. Only the first call counts.
func (p *Persister) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true
	wctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	go p.run(wctx)
}

// Submit queues a job without blocking. It returns session.ErrFenceLost after a fence loss and
// ErrPersisterClosed after Close or the end of the session context; the job is then dropped.
func (p *Persister) Submit(job PersistJob) error {
	if job == nil {
		return errors.New("conversation: nil persist job")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.lost:
		return session.ErrFenceLost
	case p.closed:
		return ErrPersisterClosed
	}
	if u, ok := job.(UpdateUser); ok {
		for i, q := range p.queue {
			if qu, ok := q.(UpdateUser); ok && qu.TurnID == u.TurnID {
				p.queue[i] = u // the newest user side wins, at the position of the queued one
				return nil
			}
		}
	}
	p.queue = append(p.queue, job)
	p.trackQueueLocked(p.clk.Now())
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

// Pending returns the number of queued jobs, the one being written excluded.
func (p *Persister) Pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Flush waits until every submitted job is written (DRAIN_FLUSH_TIMEOUT / finish(), ≤ 5 s by
// ctx). It returns session.ErrFenceLost after a fence loss, ctx.Err() at the deadline, and an
// error when the worker stopped with jobs unwritten.
func (p *Persister) Flush(ctx context.Context) error {
	for {
		p.mu.Lock()
		switch {
		case p.lost:
			p.mu.Unlock()
			return session.ErrFenceLost
		case len(p.queue) == 0 && !p.inflight:
			p.mu.Unlock()
			return nil
		case p.stopped:
			n := len(p.queue)
			p.mu.Unlock()
			return fmt.Errorf("conversation: persister stopped with %d jobs unwritten", n)
		}
		ch := p.changed
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close refuses further jobs, lets the worker write what is queued and stops it. When ctx ends
// first the worker is cancelled, the rest is dropped and ctx.Err() is returned. Close is
// idempotent; after a fence loss it returns session.ErrFenceLost.
func (p *Persister) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	started := p.started
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	if started {
		select {
		case <-p.done:
		case <-ctx.Done():
			p.cancel()
			<-p.done
			if p.fenceLost() {
				return session.ErrFenceLost
			}
			return ctx.Err()
		}
		p.cancel()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.lost:
		return session.ErrFenceLost
	case len(p.queue) > 0:
		return fmt.Errorf("conversation: persister stopped with %d jobs unwritten", len(p.queue))
	}
	return nil
}

func (p *Persister) fenceLost() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lost
}

func (p *Persister) run(ctx context.Context) {
	defer close(p.done)
	defer func() {
		p.mu.Lock()
		p.closed, p.stopped, p.inflight = true, true, false
		p.signalLocked()
		p.mu.Unlock()
	}()
	for {
		job, ok := p.next(ctx)
		if !ok || !p.write(ctx, job) {
			return
		}
	}
}

// next takes the head job, waiting for one; false when the worker must stop.
func (p *Persister) next(ctx context.Context) (PersistJob, bool) {
	for {
		p.mu.Lock()
		if p.lost {
			p.mu.Unlock()
			return nil, false
		}
		if len(p.queue) > 0 {
			job := p.queue[0]
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.inflight = true
			p.trackQueueLocked(p.clk.Now())
			p.mu.Unlock()
			return job, true
		}
		if p.closed {
			p.mu.Unlock()
			return nil, false
		}
		p.signalLocked() // idle
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-p.wake:
		}
	}
}

// write runs job until it succeeds (true) or the worker must stop (false).
func (p *Persister) write(ctx context.Context, job PersistJob) bool {
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, persistOpTimeout)
		err := p.exec(actx, job)
		cancel()
		switch {
		case err == nil:
			p.settle(time.Time{})
			return true
		case errors.Is(err, session.ErrFenceLost):
			p.loseFence(ctx, err)
			return false
		case ctx.Err() != nil:
			return false
		}
		p.log.WarnContext(ctx, "persister write failed", "session_id", p.sid, "fence", p.fence,
			"job", job.persistKind(), "attempt", attempt+1, "error", err)
		p.settle(p.clk.Now())
		if p.sleep(ctx, backoff(attempt)) != nil {
			return false
		}
	}
}

func backoff(attempt int) time.Duration {
	d := persistBackoffFirst
	for range attempt {
		d *= 2
		if d >= persistBackoffCap {
			return persistBackoffCap
		}
	}
	return d
}

func (p *Persister) exec(ctx context.Context, job PersistJob) error {
	switch j := job.(type) {
	case InsertTurn:
		return p.turns.Insert(ctx, p.fence, j.Turn)
	case UpdateUser:
		return p.turns.UpdateUser(ctx, p.fence, j.TurnID, j.User)
	case FinalizeTurn:
		return p.turns.Finalize(ctx, p.fence, j.TurnID, j.Final)
	case SessionEvent:
		return p.events.Append(ctx, p.fence, p.sid, j.Type, j.TurnID, j.Payload)
	case AddDisconnected:
		return p.disc.AddDisconnected(ctx, p.sid, p.fence, j.Seconds)
	}
	return fmt.Errorf("conversation: unknown persist job %T", job)
}

// settle records the outcome of an attempt: failedAt is zero on success. It then applies the
// stuck rule and reports it once.
func (p *Persister) settle(failedAt time.Time) {
	p.mu.Lock()
	if failedAt.IsZero() {
		p.inflight = false
		p.failSince = time.Time{}
		if len(p.queue) == 0 {
			p.signalLocked()
		}
	} else if p.failSince.IsZero() {
		p.failSince = failedAt
	}
	now := p.clk.Now()
	stuck := !p.stuckSent && (over(p.overSince, now) || over(p.failSince, now))
	if stuck {
		p.stuckSent = true
	}
	p.mu.Unlock()
	if stuck {
		p.log.Error("persister stuck", "session_id", p.sid, "fence", p.fence)
		if p.onStuck != nil {
			p.onStuck(ErrPersisterStuck)
		}
	}
}

func over(since, now time.Time) bool {
	return !since.IsZero() && now.Sub(since) > persisterStuckAfter
}

func (p *Persister) trackQueueLocked(now time.Time) {
	switch {
	case len(p.queue) <= persisterQueueCap:
		p.overSince = time.Time{}
	case p.overSince.IsZero():
		p.overSince = now
	}
}

func (p *Persister) loseFence(ctx context.Context, err error) {
	p.mu.Lock()
	p.lost, p.closed, p.inflight = true, true, false
	for i := range p.queue {
		p.queue[i] = nil
	}
	p.queue = nil
	p.signalLocked()
	p.mu.Unlock()
	p.m.fenceLostInc()
	p.log.WarnContext(ctx, "persister fence lost", "session_id", p.sid, "fence", p.fence)
	if p.onFenceLost != nil {
		p.onFenceLost(err)
	}
}

// signalLocked wakes Flush waiters.
func (p *Persister) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}
