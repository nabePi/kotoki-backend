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
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// ErrPersisterStuck means the persister queue stayed above 64 jobs, or its head job kept failing,
// for more than 30 s (§7.12.3, §7.11.4): the loop ends the session with SERVER_ERROR.
var ErrPersisterStuck = errors.New("conversation: persister stuck for more than 30 s")

// ErrPersistRejected means a job failed with an error retrying cannot fix (validation, constraint
// violation, missing row, invalid transition): an invariant is broken (§7.11.4 SERVER_ERROR).
// The persister stops writing; the reported error wraps this one and the cause.
var ErrPersistRejected = errors.New("conversation: persist job rejected")

// ErrPersisterClosed means Submit came after Close, Abort or the end of the session context.
var ErrPersisterClosed = errors.New("conversation: persister closed")

const (
	persisterQueueCap   = 64               // §7.12.3
	persisterStuckAfter = 30 * time.Second // §7.12.3: queue > 64 (or failing) for longer → SERVER_ERROR
	persistOpTimeout    = 5 * time.Second  // one attempt; the statement_timeout of the store is 5 s too
	persistBackoffFirst = 100 * time.Millisecond
	persistBackoffCap   = 2 * time.Second
	sqlStateUnique      = "23505"
)

// PersistJob is one write of the persister: InsertTurn, UpdateUser, FinalizeTurn, SessionEvent or
// AddDisconnected.
type PersistJob interface{ persistKind() string }

// InsertTurn opens a turn row (§7.6.6).
type InsertTurn struct{ Turn TurnStart }

// UpdateUser writes the user side of a turn at stt.final. A queued UpdateUser of the same turn is
// replaced by a newer one at the queued one's position (§7.12.3); the loop submits it before the
// FinalizeTurn of that turn, the only order the fenced SQL accepts (status IS NULL).
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

// AddDisconnected adds Seconds to sessions.disconnected_seconds (§7.10). Zero is a no-op.
type AddDisconnected struct{ Seconds int }

func (InsertTurn) persistKind() string      { return "insert_turn" }
func (UpdateUser) persistKind() string      { return "update_user" }
func (FinalizeTurn) persistKind() string    { return "finalize_turn" }
func (SessionEvent) persistKind() string    { return "session_event" }
func (AddDisconnected) persistKind() string { return "add_disconnected" }

// PersistSession is the part of SessionRuntime the persister uses: AddDisconnected jobs, and
// GetForAttach to read the session back when an attempt's outcome is unknown.
type PersistSession interface {
	AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) error
	GetForAttach(ctx context.Context, sessionID uuid.UUID) (session.RuntimeRecord, error)
}

// FenceLatch counts fence_lost_total once per runtime however many components detect the loss:
// the loop creates one per runtime and hands it to the persister and the redisSync.
type FenceLatch struct {
	m    *Metrics
	once sync.Once
}

// NewFenceLatch returns a latch counting on m (nil-safe).
func NewFenceLatch(m *Metrics) *FenceLatch { return &FenceLatch{m: m} }

func (l *FenceLatch) trip() { l.once.Do(l.m.fenceLostInc) }

// PersisterOption configures a Persister.
type PersisterOption func(*Persister)

// PersisterClock sets the clock of the stuck rule (default: system clock).
func PersisterClock(c clock.Clock) PersisterOption { return func(p *Persister) { p.clk = c } }

// PersisterSleep replaces the retry backoff wait (tests advance a fake clock instead).
func PersisterSleep(f func(ctx context.Context, d time.Duration) error) PersisterOption {
	return func(p *Persister) { p.sleep = f }
}

// PersisterOnFenceLost is called once, on the persister goroutine, when the fence is lost.
func PersisterOnFenceLost(f func(error)) PersisterOption {
	return func(p *Persister) { p.onFenceLost = f }
}

// PersisterOnFatal is called once, on the persister goroutine, with ErrPersisterStuck or an
// error wrapping ErrPersistRejected; both end the session with SERVER_ERROR (§7.11.4).
func PersisterOnFatal(f func(error)) PersisterOption { return func(p *Persister) { p.onFatal = f } }

// PersisterFenceLatch shares the runtime's fence-loss latch (default: one of its own).
func PersisterFenceLatch(l *FenceLatch) PersisterOption { return func(p *Persister) { p.latch = l } }

// Persister is the per-session writer of session_turns / session_events (§7.6.6, §7.7.2): jobs
// run one at a time in submission order through the fenced ports, so a turn is always inserted
// before its user side and finalized last. Submit never blocks the loop (§7.12.3).
//
// Failure semantics, by the class of the error of one attempt:
//   - session.ErrFenceLost: the queue is dropped, nothing is written again, OnFenceLost fires
//     once and the FenceLatch counts fence_lost_total; Submit, Flush and Close then return
//     session.ErrFenceLost. The loop stops the runtime (§7.9.3).
//   - permanent (business error such as validation, session.ErrNotFound, an invalid transition,
//     SQLSTATE class 22/23/42): no retry; the persister stops writing and OnFatal fires with
//     ErrPersistRejected; Submit, Flush and Close then return that error.
//   - transient (a server-reported SQLSTATE of another class, or an error whose SafeToRetry() is
//     true): nothing was committed; the same job is retried.
//   - ambiguous (attempt deadline or cancel, connection broken after sending, anything
//     unclassified): the write may have committed. The job is retried idempotently: a retried
//     InsertTurn that hits the primary key (23505) is the earlier commit; a retried FinalizeTurn
//     that matches no row is the earlier commit when GetForAttach shows the fence still ours and
//     the session open, a fence loss otherwise; AddDisconnected reads disconnected_seconds before
//     the first attempt and, after an ambiguous one, sends again only when the read-back shows it
//     did not commit. UpdateUser is naturally idempotent. A SessionEvent may be written twice
//     (an audit duplicate; the port carries no event id).
//
// Retries back off 100 ms doubling to 2 s, 5 s per attempt. When the head job has been failing,
// or more than 64 jobs have been queued, for more than 30 s, OnFatal fires once with
// ErrPersisterStuck (§7.11.4 SERVER_ERROR); the persister keeps trying until it is closed.
//
// Callbacks (OnFenceLost, OnFatal) run on the persister goroutine. They must not call back into
// the persister (Close, Abort and Flush wait for that goroutine: deadlock) and must not block: the
// loop signals itself with a non-blocking channel send.
//
// Loop usage (Tasks 18–22): NewPersister (with the runtime's FenceLatch) → Start(sessionCtx) →
// Submit(job) per §7.6.6/§7.10/§7.13.1 → Flush(ctx ≤ 5 s) before Complete/Fail/MarkHandoff →
// Close(ctx) when the runtime ends; on a fence loss reported by the redisSync, Abort().
type Persister struct {
	sid         uuid.UUID
	turns       TurnRepository
	events      SessionEventWriter
	sess        PersistSession
	fence       int64
	log         *slog.Logger
	clk         clock.Clock
	sleep       func(context.Context, time.Duration) error
	onFenceLost func(error)
	onFatal     func(error)
	latch       *FenceLatch

	mu        sync.Mutex
	queue     []PersistJob
	inflight  bool
	closed    bool  // Submit refused
	stopped   bool  // the worker has exited
	lost      bool  // fence lost
	rejected  error // wraps ErrPersistRejected
	overSince time.Time
	failSince time.Time
	fatalSent bool
	changed   chan struct{} // closed and replaced when the persister becomes idle or stops
	wake      chan struct{}
	started   bool
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewPersister creates the persister of session sid writing with fence. sess is the
// SessionRuntime. A nil log discards.
func NewPersister(sid uuid.UUID, turns TurnRepository, events SessionEventWriter, sess PersistSession, fence int64,
	m *Metrics, log *slog.Logger, opts ...PersisterOption,
) *Persister {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Persister{sid: sid, turns: turns, events: events, sess: sess, fence: fence, log: log,
		clk: clock.System{}, sleep: sleepCtx, latch: NewFenceLatch(m), changed: make(chan struct{}),
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Start runs the worker until ctx ends, Close has drained the queue, or Abort. Only the first
// call counts.
func (p *Persister) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.closed {
		return
	}
	p.started = true
	wctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	go p.run(wctx)
}

// Submit queues a job without blocking. After a fence loss it returns session.ErrFenceLost,
// after a rejection that error, after Close/Abort or the end of the session context
// ErrPersisterClosed; the job is then dropped.
func (p *Persister) Submit(job PersistJob) error {
	if job == nil {
		return errors.New("conversation: nil persist job")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.refusalLocked(); err != nil {
		return err
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
	p.kick()
	return nil
}

func (p *Persister) refusalLocked() error {
	switch {
	case p.lost:
		return session.ErrFenceLost
	case p.rejected != nil:
		return p.rejected
	case p.closed:
		return ErrPersisterClosed
	}
	return nil
}

func (p *Persister) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Pending returns the number of queued jobs, the one being written excluded.
func (p *Persister) Pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Flush waits until every submitted job is written (DRAIN_FLUSH_TIMEOUT / finish(), ≤ 5 s by
// ctx). It returns session.ErrFenceLost after a fence loss, the rejection error after one,
// ctx.Err() at the deadline, and an error when the worker stopped with jobs unwritten.
func (p *Persister) Flush(ctx context.Context) error {
	for {
		p.mu.Lock()
		switch {
		case p.lost:
			p.mu.Unlock()
			return session.ErrFenceLost
		case p.rejected != nil:
			err := p.rejected
			p.mu.Unlock()
			return err
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
// idempotent; after a fence loss it returns session.ErrFenceLost, after a rejection that error.
func (p *Persister) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	started := p.started
	p.mu.Unlock()
	p.kick()
	if started {
		select {
		case <-p.done:
		case <-ctx.Done():
			p.cancel()
			<-p.done
			p.mu.Lock()
			defer p.mu.Unlock()
			if err := p.failureLocked(); err != nil {
				return err
			}
			return ctx.Err()
		}
		p.cancel()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.failureLocked(); err != nil {
		return err
	}
	if len(p.queue) > 0 {
		return fmt.Errorf("conversation: persister stopped with %d jobs unwritten", len(p.queue))
	}
	return nil
}

func (p *Persister) failureLocked() error {
	if p.lost {
		return session.ErrFenceLost
	}
	return p.rejected
}

// Abort drops the queue and stops the worker without writing anything else and without
// counting a fence loss (the loop learned of one from the redisSync, §7.9.3). It returns once
// the worker has exited. Never call it from a persister callback.
func (p *Persister) Abort() {
	p.mu.Lock()
	p.closed = true
	p.dropQueueLocked()
	started := p.started
	p.mu.Unlock()
	if started {
		p.cancel()
		<-p.done
	}
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
		if p.lost || p.rejected != nil {
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

// outcome is the class of the error of one attempt.
type outcome int

const (
	outcomeOK        outcome = iota
	outcomeFence             // session.ErrFenceLost
	outcomePermanent         // retrying cannot help
	outcomeTransient         // definitely not committed: retry
	outcomeAmbiguous         // may have committed: retry idempotently
)

// classify sorts an attempt error. The persister cannot import pgx (§2.2), so a server error is
// recognised by its SQLState() method (pgconn.PgError) and a request that never left by
// SafeToRetry() (pgconn's connect errors).
func classify(err error) outcome {
	if err == nil {
		return outcomeOK
	}
	if errors.Is(err, session.ErrFenceLost) {
		return outcomeFence
	}
	if code := sqlState(err); code != "" {
		switch code[:min(2, len(code))] {
		case "22", "23", "42": // data exception, integrity constraint, syntax/undefined object
			return outcomePermanent
		}
		return outcomeTransient
	}
	var safe interface{ SafeToRetry() bool }
	if errors.As(err, &safe) && safe.SafeToRetry() {
		return outcomeTransient
	}
	if errors.Is(err, session.ErrNotFound) || errors.Is(err, session.ErrInvalidTransition) ||
		errs.CodeOf(err) != "" && errs.KindOf(err) == errs.KindBusiness {
		return outcomePermanent
	}
	return outcomeAmbiguous
}

func sqlState(err error) string {
	var se interface{ SQLState() string }
	if errors.As(err, &se) {
		return se.SQLState()
	}
	return ""
}

// attemptState is what the retries of one job remember.
type attemptState struct {
	ambiguous bool // an earlier attempt may have committed
	haveBase  bool
	base      int // disconnected_seconds before the first AddDisconnected attempt
}

// write runs job until it is done (true) or the worker must stop (false).
func (p *Persister) write(ctx context.Context, job PersistJob) bool {
	var st attemptState
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, persistOpTimeout)
		err := p.exec(actx, job, &st)
		cancel()
		c := classify(err)
		if c == outcomeOK {
			p.settle(time.Time{})
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		switch c {
		case outcomeFence:
			p.loseFence(ctx, err)
			return false
		case outcomePermanent:
			p.reject(ctx, job, err)
			return false
		case outcomeAmbiguous:
			st.ambiguous = true
		}
		p.log.WarnContext(ctx, "persister write failed", "session_id", p.sid, "fence", p.fence,
			"job", job.persistKind(), "attempt", attempt+1, "ambiguous", st.ambiguous, "error", err)
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

func (p *Persister) exec(ctx context.Context, job PersistJob, st *attemptState) error {
	switch j := job.(type) {
	case InsertTurn:
		err := p.turns.Insert(ctx, p.fence, j.Turn)
		if st.ambiguous && sqlState(err) == sqlStateUnique {
			return nil // the earlier attempt inserted the row
		}
		return err
	case UpdateUser:
		return p.turns.UpdateUser(ctx, p.fence, j.TurnID, j.User)
	case FinalizeTurn:
		err := p.turns.Finalize(ctx, p.fence, j.TurnID, j.Final)
		if st.ambiguous && errors.Is(err, session.ErrFenceLost) {
			// No row: either the fence moved or the earlier attempt already set the status.
			return p.stillOwned(ctx)
		}
		return err
	case SessionEvent:
		return p.events.Append(ctx, p.fence, p.sid, j.Type, j.TurnID, j.Payload)
	case AddDisconnected:
		return p.addDisconnected(ctx, j.Seconds, st)
	}
	return fmt.Errorf("conversation: unknown persist job %T", job)
}

// stillOwned is nil when the session row still carries this runtime's fence and an open status.
func (p *Persister) stillOwned(ctx context.Context) error {
	rec, err := p.sess.GetForAttach(ctx, p.sid)
	if err != nil {
		return err
	}
	return p.ownedBy(rec)
}

func (p *Persister) ownedBy(rec session.RuntimeRecord) error {
	switch rec.Status {
	case session.StatusStarting, session.StatusActive, session.StatusEnding:
		if rec.RuntimeFence == p.fence {
			return nil
		}
	}
	return session.ErrFenceLost
}

// addDisconnected sends the episode at most once as far as the read-back can tell: only this
// runtime writes disconnected_seconds while it holds the fence.
func (p *Persister) addDisconnected(ctx context.Context, n int, st *attemptState) error {
	if n == 0 {
		return nil
	}
	switch {
	case !st.haveBase:
		rec, err := p.sess.GetForAttach(ctx, p.sid)
		if err != nil {
			return err
		}
		st.base, st.haveBase = rec.DisconnectedSeconds, true
	case st.ambiguous:
		rec, err := p.sess.GetForAttach(ctx, p.sid)
		if err != nil {
			return err
		}
		if err := p.ownedBy(rec); err != nil {
			return err
		}
		if rec.DisconnectedSeconds >= st.base+n {
			return nil // the earlier attempt committed
		}
	}
	return p.sess.AddDisconnected(ctx, p.sid, p.fence, n)
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
	stuck := !p.fatalSent && (over(p.overSince, now) || over(p.failSince, now))
	if stuck {
		p.fatalSent = true
	}
	p.mu.Unlock()
	if stuck {
		p.log.Error("persister stuck", "session_id", p.sid, "fence", p.fence)
		if p.onFatal != nil {
			p.onFatal(ErrPersisterStuck)
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

func (p *Persister) dropQueueLocked() {
	for i := range p.queue {
		p.queue[i] = nil
	}
	p.queue = nil
	p.inflight = false
	p.signalLocked()
}

func (p *Persister) loseFence(ctx context.Context, err error) {
	p.mu.Lock()
	p.lost, p.closed = true, true
	p.dropQueueLocked()
	p.mu.Unlock()
	p.latch.trip()
	p.log.WarnContext(ctx, "persister fence lost", "session_id", p.sid, "fence", p.fence)
	if p.onFenceLost != nil {
		p.onFenceLost(err)
	}
}

func (p *Persister) reject(ctx context.Context, job PersistJob, err error) {
	rerr := fmt.Errorf("%w: %s: %w", ErrPersistRejected, job.persistKind(), err)
	p.mu.Lock()
	p.rejected, p.closed = rerr, true
	p.dropQueueLocked()
	first := !p.fatalSent
	p.fatalSent = true
	p.mu.Unlock()
	p.log.ErrorContext(ctx, "persister job rejected", "session_id", p.sid, "fence", p.fence,
		"job", job.persistKind(), "error", err)
	if first && p.onFatal != nil {
		p.onFatal(rerr)
	}
}

// signalLocked wakes Flush waiters.
func (p *Persister) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}
