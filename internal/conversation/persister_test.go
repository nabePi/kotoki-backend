package conversation_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var (
	pSession = uuid.MustParse("0193a000-0000-7000-8000-000000000001")
	pTurn    = uuid.MustParse("0193a000-0000-7000-8000-0000000000a1")
	pT0      = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
)

const pFence = int64(7)

// sqlStateErr stands for a pgconn.PgError: the server answered with an error, so nothing was
// committed. The persister only sees the SQLState method (it must not import pgx).
type sqlStateErr struct{ code string }

func (e sqlStateErr) Error() string    { return "ERROR: statement failed (SQLSTATE " + e.code + ")" }
func (e sqlStateErr) SQLState() string { return e.code }

// safeErr stands for a pgconn error with SafeToRetry() == true: the request never left.
type safeErr struct{}

func (safeErr) Error() string     { return "dial tcp 10.0.0.1:5432: connect: connection refused" }
func (safeErr) SafeToRetry() bool { return true }

func pgErr(code string) error {
	return errs.Wrap(sqlStateErr{code}, errs.CodeInternalError, "write failed")
}

var (
	errDBDown = errs.Wrap(safeErr{}, errs.CodeInternalError, "write failed")
	// errAmbiguous: the attempt timed out; the commit may or may not have happened.
	errAmbiguous = errs.Wrap(context.DeadlineExceeded, errs.CodeInternalError, "write failed")
	// errConnReset: the connection broke after the statement was sent.
	errConnReset = errs.Wrap(errors.New("read tcp 10.0.0.2:41000->10.0.0.1:5432: read: connection reset by peer"),
		errs.CodeInternalError, "write failed")
	errValidation = errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid seconds"), map[string]any{"field": "seconds"})
)

// step scripts one call of a fake: the error it returns and whether the write committed first.
type step struct {
	err    error
	commit bool
}

type script struct {
	mu    sync.Mutex
	steps map[string][]step
}

func (s *script) add(call string, st ...step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.steps == nil {
		s.steps = map[string][]step{}
	}
	s.steps[call] = append(s.steps[call], st...)
}

func (s *script) pop(call string) (step, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.steps[call]
	if len(q) == 0 {
		return step{}, false
	}
	s.steps[call] = q[1:]
	return q[0], true
}

// run applies the scripted step of call around the real write.
func (s *script) run(call string, write func() error) error {
	st, ok := s.pop(call)
	if !ok {
		return write()
	}
	if st.commit {
		if err := write(); err != nil {
			return err
		}
	}
	return st.err
}

// recTurns wraps fakes.TurnRepo: it records the order of calls, can block UpdateUser until
// released, fails every write while failing is set, and follows a per-call script.
type recTurns struct {
	*fakes.TurnRepo
	script
	mu      sync.Mutex
	calls   []string
	users   []string // TurnUser.Text of every UpdateUser that reached the repository
	gate    chan struct{}
	entered chan struct{}
	failing atomic.Bool
	failN   atomic.Int32 // the next failN calls fail, then the database is back
}

func newRecTurns() *recTurns {
	return &recTurns{TurnRepo: fakes.NewTurnRepo(), entered: make(chan struct{}, 128)}
}

func (r *recTurns) do(call string, write func() error) error {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	if r.failing.Load() || r.failN.Add(-1) >= 0 {
		return errDBDown
	}
	return r.run(call, write)
}

func (r *recTurns) Insert(ctx context.Context, fence int64, t conversation.TurnStart) error {
	return r.do("Insert", func() error { return r.TurnRepo.Insert(ctx, fence, t) })
}

func (r *recTurns) UpdateUser(ctx context.Context, fence int64, id uuid.UUID, u conversation.TurnUser) error {
	r.mu.Lock()
	r.users = append(r.users, u.Text)
	gate := r.gate
	r.mu.Unlock()
	r.entered <- struct{}{}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.do("UpdateUser", func() error { return r.TurnRepo.UpdateUser(ctx, fence, id, u) })
}

func (r *recTurns) Finalize(ctx context.Context, fence int64, id uuid.UUID, f conversation.TurnFinal) error {
	return r.do("Finalize", func() error { return r.TurnRepo.Finalize(ctx, fence, id, f) })
}

func (r *recTurns) setGate(g chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gate = g
}

func (r *recTurns) callLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recTurns) userTexts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.users...)
}

// recEvents wraps fakes.EventWriter with a per-call script.
type recEvents struct {
	*fakes.EventWriter
	script
}

func (w *recEvents) Append(ctx context.Context, fence int64, sid uuid.UUID, typ string, turnID *uuid.UUID, payload any) error {
	return w.run("Append", func() error { return w.EventWriter.Append(ctx, fence, sid, typ, turnID, payload) })
}

// sessFake is the PersistSession: disconnected_seconds, fence and status of the session row.
type sessFake struct {
	script
	mu     sync.Mutex
	fence  int64
	status session.Status
	disc   int
	reads  int
	writes int
	// clamped models AddSessionDisconnected (db/queries/sessions.sql): each committed call adds
	// the next value instead of the full seconds (the part after expires_at is dropped, the total
	// is clamped at duration_seconds).
	clamped []int
}

func (s *sessFake) AddDisconnected(_ context.Context, sid uuid.UUID, fence int64, seconds int) error {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return s.run("AddDisconnected", func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sid != pSession || fence != s.fence {
			return session.ErrFenceLost
		}
		if len(s.clamped) > 0 {
			seconds, s.clamped = s.clamped[0], s.clamped[1:]
		}
		s.disc += seconds
		return nil
	})
}

func (s *sessFake) GetForAttach(_ context.Context, sid uuid.UUID) (session.RuntimeRecord, error) {
	if err := s.run("GetForAttach", func() error { return nil }); err != nil {
		return session.RuntimeRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return session.RuntimeRecord{ID: sid, Status: s.status, RuntimeFence: s.fence, DisconnectedSeconds: s.disc}, nil
}

func (s *sessFake) state() (disc, reads, writes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disc, s.reads, s.writes
}

func (s *sessFake) setFence(f int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fence = f
}

type fatalSig struct {
	err error
	at  time.Time
}

type persistRig struct {
	p      *conversation.Persister
	turns  *recTurns
	events *recEvents
	sess   *sessFake
	clk    *clock.Fake
	m      *conversation.Metrics
	lost   chan error
	fatal  chan fatalSig
	cancel context.CancelFunc
}

func startPersister(t *testing.T, opts ...conversation.PersisterOption) *persistRig {
	t.Helper()
	noLeak(t)
	r := &persistRig{turns: newRecTurns(), events: &recEvents{EventWriter: fakes.NewEventWriter()},
		sess: &sessFake{fence: pFence, status: session.StatusActive},
		clk:  clock.NewFake(pT0), m: conversation.NewMetrics(nil), lost: make(chan error, 8), fatal: make(chan fatalSig, 8)}
	all := append([]conversation.PersisterOption{
		conversation.PersisterClock(r.clk),
		conversation.PersisterSleep(fastSleep(r.clk)),
		conversation.PersisterOnFenceLost(func(err error) { r.lost <- err }),
		conversation.PersisterOnFatal(func(err error) { r.fatal <- fatalSig{err: err, at: r.clk.Now()} }),
	}, opts...)
	r.p = conversation.NewPersister(pSession, r.turns, r.events, r.sess, pFence, r.m, nil, all...)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.p.Start(ctx)
	t.Cleanup(func() {
		cancel()
		cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
		defer ccancel()
		_ = r.p.Close(cctx)
	})
	return r
}

func userJob(text string) conversation.UpdateUser {
	return conversation.UpdateUser{TurnID: pTurn, User: conversation.TurnUser{CreatedAt: pT0, Text: text}}
}

func insertJob() conversation.InsertTurn {
	return conversation.InsertTurn{Turn: conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}}
}

func finalJob(st session.TurnStatus) conversation.FinalizeTurn {
	return conversation.FinalizeTurn{TurnID: pTurn, Final: conversation.TurnFinal{CreatedAt: pT0, Status: st, CompletedAt: pT0}}
}

// waitSignal waits for one value on ch.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitFatal(t *testing.T, r *persistRig) fatalSig {
	t.Helper()
	select {
	case f := <-r.fatal:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("OnFatal not called")
		return fatalSig{}
	}
}

func flush(t *testing.T, p *conversation.Persister) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.Flush(ctx)
}

func TestPersisterOrdered(t *testing.T) {
	r := startPersister(t)
	require.NoError(t, r.p.Submit(conversation.InsertTurn{Turn: conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}}))
	require.NoError(t, r.p.Submit(userJob("こんにちは")))
	require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN", TurnID: &pTurn, Payload: map[string]int{"generation": 2}}))
	require.NoError(t, r.p.Submit(conversation.FinalizeTurn{TurnID: pTurn, Final: conversation.TurnFinal{CreatedAt: pT0, Status: session.TurnCompleted, CompletedAt: pT0}}))
	require.NoError(t, r.p.Submit(conversation.AddDisconnected{Seconds: 4}))
	require.NoError(t, flush(t, r.p))

	require.Equal(t, []string{"Insert", "UpdateUser", "Finalize"}, r.turns.callLog(), "Insert < UpdateUser < Finalize")
	turns := r.turns.Turns(pSession)
	require.Len(t, turns, 1)
	require.Equal(t, "こんにちは", *turns[0].UserText)
	require.Equal(t, session.TurnCompleted, *turns[0].Status)
	evs := r.events.Events()
	require.Len(t, evs, 1)
	require.Equal(t, "BARGE_IN", evs[0].Type)
	require.Equal(t, pFence, evs[0].Fence)
	require.Equal(t, pTurn, *evs[0].TurnID)
	disc, _, _ := r.sess.state()
	require.Equal(t, 4, disc)

	cctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, r.p.Close(cctx))
	require.ErrorIs(t, r.p.Submit(userJob("late")), conversation.ErrPersisterClosed)
	require.Empty(t, r.lost)
	require.Empty(t, r.fatal)
}

func TestPersisterCoalesce(t *testing.T) {
	r := startPersister(t)
	require.NoError(t, r.turns.Insert(context.Background(), pFence, conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}))
	gate := make(chan struct{})
	r.turns.setGate(gate)

	require.NoError(t, r.p.Submit(userJob("u0")))
	waitSignal(t, r.turns.entered, "first UpdateUser in flight")
	for i := 1; i < 50; i++ {
		require.NoError(t, r.p.Submit(userJob(fmt.Sprintf("u%d", i))))
	}
	require.Equal(t, 1, r.p.Pending(), "49 queued UpdateUser of one turn are merged into one job")
	close(gate)
	require.NoError(t, flush(t, r.p))

	texts := r.turns.userTexts()
	require.LessOrEqual(t, len(texts), 2)
	require.Equal(t, "u49", texts[len(texts)-1], "the newest update wins")
	require.Equal(t, "u49", *r.turns.Turns(pSession)[0].UserText)
}

func TestPersisterCoalesceKeepsOtherTurnsAndOrder(t *testing.T) {
	r := startPersister(t)
	other := uuid.MustParse("0193a000-0000-7000-8000-0000000000a2")
	ctx := context.Background()
	require.NoError(t, r.turns.Insert(ctx, pFence, conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}))
	require.NoError(t, r.turns.Insert(ctx, pFence, conversation.TurnStart{ID: other, SessionID: pSession, Sequence: 2, CreatedAt: pT0}))
	gate := make(chan struct{})
	r.turns.setGate(gate)

	require.NoError(t, r.p.Submit(userJob("a0")))
	waitSignal(t, r.turns.entered, "first UpdateUser in flight")
	require.NoError(t, r.p.Submit(userJob("a1")))
	require.NoError(t, r.p.Submit(conversation.UpdateUser{TurnID: other, User: conversation.TurnUser{CreatedAt: pT0, Text: "b1"}}))
	require.NoError(t, r.p.Submit(userJob("a2")))
	require.Equal(t, 2, r.p.Pending(), "only updates of the same turn merge")
	close(gate)
	require.NoError(t, flush(t, r.p))
	require.Equal(t, []string{"a0", "a2", "b1"}, r.turns.userTexts(), "the merged job keeps the queue position of the first")
}

func TestPersisterFenceLost(t *testing.T) {
	r := startPersister(t)
	gate := make(chan struct{})
	r.turns.setGate(gate)
	r.turns.SetFence(pFence + 1) // another pod claimed the session
	require.NoError(t, r.p.Submit(userJob("first")))
	waitSignal(t, r.turns.entered, "UpdateUser in flight")
	// Queued behind the write that will hit the lost fence.
	require.NoError(t, r.p.Submit(conversation.InsertTurn{Turn: conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}}))
	require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "TURN_FAILED", TurnID: &pTurn, Payload: map[string]int{"turn_seq": 1}}))
	require.NoError(t, r.p.Submit(conversation.AddDisconnected{Seconds: 3}))
	close(gate)

	select {
	case err := <-r.lost:
		require.ErrorIs(t, err, session.ErrFenceLost)
	case <-time.After(2 * time.Second):
		t.Fatal("OnFenceLost not called")
	}
	require.ErrorIs(t, flush(t, r.p), session.ErrFenceLost)
	require.ErrorIs(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN"}), session.ErrFenceLost)
	require.Empty(t, r.events.Events(), "nothing is written after the fence is lost")
	_, reads, writes := r.sess.state()
	require.Zero(t, writes)
	require.Zero(t, reads, "a definite fence loss is not re-checked")
	require.Equal(t, []string{"UpdateUser"}, r.turns.callLog(), "the queued jobs are never sent")
	require.Empty(t, r.lost, "OnFenceLost fires once")
	require.InDelta(t, 1, r.m.FenceLostTotal(), 0)
	cctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorIs(t, r.p.Close(cctx), session.ErrFenceLost)
}

func TestPersisterFenceLostFromEvents(t *testing.T) {
	r := startPersister(t)
	r.events.SetFence(pFence + 1)
	require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "OWNER_CHANGED", Payload: map[string]string{"reason": "START"}}))
	if err := r.p.Submit(conversation.InsertTurn{Turn: conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}}); err != nil {
		require.ErrorIs(t, err, session.ErrFenceLost, "refused when the loss came first")
	}
	select {
	case err := <-r.lost:
		require.ErrorIs(t, err, session.ErrFenceLost)
	case <-time.After(2 * time.Second):
		t.Fatal("OnFenceLost not called")
	}
	require.ErrorIs(t, flush(t, r.p), session.ErrFenceLost)
	require.Empty(t, r.turns.callLog())
}

// fastSleep advances the fake clock instead of sleeping.
func fastSleep(clk *clock.Fake) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		clk.Advance(d)
		return ctx.Err()
	}
}

func TestPersisterRetryThenSuccess(t *testing.T) {
	r := startPersister(t)
	r.turns.failN.Store(3)
	require.NoError(t, r.p.Submit(conversation.InsertTurn{Turn: conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}}))
	require.NoError(t, r.p.Submit(conversation.FinalizeTurn{TurnID: pTurn, Final: conversation.TurnFinal{CreatedAt: pT0, Status: session.TurnFailed, CompletedAt: pT0}}))
	require.NoError(t, flush(t, r.p))
	require.Equal(t, []string{"Insert", "Insert", "Insert", "Insert", "Finalize"}, r.turns.callLog(),
		"the failed job is retried before the next one runs")
	require.Equal(t, session.TurnFailed, *r.turns.Turns(pSession)[0].Status)
	require.Empty(t, r.lost)
	require.Empty(t, r.fatal, "a short outage is not stuck")
	require.Less(t, r.clk.Now().Sub(pT0), 5*time.Second, "backoff starts small")
}

// queueBlocked holds the worker on an UpdateUser and queues n events behind it at the current
// fake time.
func queueBlocked(t *testing.T, r *persistRig, n int) chan struct{} {
	t.Helper()
	require.NoError(t, r.turns.TurnRepo.Insert(context.Background(), pFence, insertJob().Turn))
	gate := make(chan struct{})
	r.turns.setGate(gate)
	require.NoError(t, r.p.Submit(userJob("blocked")))
	waitSignal(t, r.turns.entered, "UpdateUser in flight")
	for i := range n {
		require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN", Payload: map[string]int{"i": i}}), "the loop is never refused")
	}
	require.Equal(t, n, r.p.Pending())
	return gate
}

func TestPersisterQueueOf64IsNotStuck(t *testing.T) {
	r := startPersister(t)
	gate := queueBlocked(t, r, 64)
	r.clk.Advance(31 * time.Second)
	close(gate) // the worker applies the stuck rule after its in-flight job
	require.NoError(t, flush(t, r.p))
	require.Empty(t, r.fatal, "64 queued jobs are at the cap, not above it")
	require.Len(t, r.events.Events(), 64)
}

func TestPersisterQueueAbove64ForExactly30sIsNotStuck(t *testing.T) {
	r := startPersister(t)
	gate := queueBlocked(t, r, 65)
	r.clk.Advance(30 * time.Second)
	close(gate)
	require.NoError(t, flush(t, r.p))
	require.Empty(t, r.fatal, "exactly 30 s above 64 is not more than 30 s")
}

func TestPersisterQueueAbove64ForOver30sIsStuck(t *testing.T) {
	r := startPersister(t)
	gate := queueBlocked(t, r, 65)
	r.clk.Advance(30*time.Second + time.Millisecond)
	close(gate)
	f := waitFatal(t, r)
	require.ErrorIs(t, f.err, conversation.ErrPersisterStuck)
	require.NoError(t, flush(t, r.p), "a stuck persister keeps writing")
	require.Len(t, r.events.Events(), 65)
	require.Empty(t, r.fatal, "OnFatal fires once")
}

func TestPersisterFailingForOver30sIsStuck(t *testing.T) {
	var sleeps atomic.Int32
	var clk atomic.Pointer[clock.Fake]
	r := startPersister(t, conversation.PersisterSleep(func(ctx context.Context, _ time.Duration) error {
		if sleeps.Add(1) == 1 {
			clk.Load().Advance(30 * time.Second) // the second attempt fails exactly 30 s after the first
		} else {
			clk.Load().Advance(time.Millisecond)
		}
		return ctx.Err()
	}))
	clk.Store(r.clk)
	r.turns.failing.Store(true)
	require.NoError(t, r.p.Submit(insertJob()))
	f := waitFatal(t, r)
	require.ErrorIs(t, f.err, conversation.ErrPersisterStuck)
	require.Equal(t, pT0.Add(30*time.Second+time.Millisecond), f.at, "stuck only once failing for more than 30 s")
	require.Empty(t, r.fatal, "OnFatal fires once")
}

func TestPersisterTransientServerErrorRetried(t *testing.T) {
	r := startPersister(t)
	r.turns.add("Insert", step{err: pgErr("40001")}, step{err: pgErr("55P03")})
	require.NoError(t, r.p.Submit(insertJob()))
	require.NoError(t, flush(t, r.p))
	require.Equal(t, []string{"Insert", "Insert", "Insert"}, r.turns.callLog())
	require.Len(t, r.turns.Turns(pSession), 1)
	require.Empty(t, r.fatal)
}

func TestPersisterAmbiguousInsertDuplicateIsSuccess(t *testing.T) {
	r := startPersister(t)
	// The first attempt committed but timed out; the retry hits the primary key.
	r.turns.add("Insert", step{err: errAmbiguous, commit: true}, step{err: pgErr("23505")})
	require.NoError(t, r.p.Submit(insertJob()))
	require.NoError(t, r.p.Submit(userJob("x")))
	require.NoError(t, flush(t, r.p))
	require.Equal(t, []string{"Insert", "Insert", "UpdateUser"}, r.turns.callLog())
	require.Empty(t, r.fatal)
	require.Empty(t, r.lost)
}

func TestPersisterDuplicateInsertWithoutAmbiguityIsRejected(t *testing.T) {
	r := startPersister(t)
	r.turns.add("Insert", step{err: pgErr("23505")})
	require.NoError(t, r.p.Submit(insertJob()))
	if err := r.p.Submit(userJob("x")); err != nil { // the rejection may already have happened
		require.ErrorIs(t, err, conversation.ErrPersistRejected)
	}
	f := waitFatal(t, r)
	require.ErrorIs(t, f.err, conversation.ErrPersistRejected)
	require.Equal(t, pT0, f.at, "a permanent error fails fast, without backoff")
	require.ErrorIs(t, flush(t, r.p), conversation.ErrPersistRejected)
	require.ErrorIs(t, r.p.Submit(userJob("y")), conversation.ErrPersistRejected)
	require.Equal(t, []string{"Insert"}, r.turns.callLog(), "nothing is written after a rejection")
	require.Empty(t, r.lost)
	require.Zero(t, r.m.FenceLostTotal())
}

func TestPersisterDuplicateAfterDefiniteFailureIsRejected(t *testing.T) {
	r := startPersister(t)
	// The first attempt never reached the server, so a duplicate on the retry is not ours.
	r.turns.add("Insert", step{err: errDBDown}, step{err: pgErr("23505")})
	require.NoError(t, r.p.Submit(insertJob()))
	f := waitFatal(t, r)
	require.ErrorIs(t, f.err, conversation.ErrPersistRejected)
}

func TestPersisterPermanentErrorsFailFast(t *testing.T) {
	cases := map[string]struct {
		arm func(r *persistRig)
		job conversation.PersistJob
	}{
		"validation": {func(r *persistRig) { r.sess.add("AddDisconnected", step{err: errValidation}) }, conversation.AddDisconnected{Seconds: -1}},
		"check violation": {func(r *persistRig) { r.events.add("Append", step{err: pgErr("23514")}) },
			conversation.SessionEvent{Type: "BARGE_IN"}},
		"data exception": {func(r *persistRig) { r.events.add("Append", step{err: pgErr("22003")}) },
			conversation.SessionEvent{Type: "BARGE_IN"}},
		"turn not found": {func(*persistRig) {}, userJob("no such turn")},
		"invalid transition": {func(r *persistRig) {
			r.sess.add("AddDisconnected", step{err: &session.TransitionError{From: session.StatusCompleted, To: session.StatusActive}})
		}, conversation.AddDisconnected{Seconds: 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := startPersister(t)
			tc.arm(r)
			require.NoError(t, r.p.Submit(tc.job))
			f := waitFatal(t, r)
			require.ErrorIs(t, f.err, conversation.ErrPersistRejected)
			require.Equal(t, pT0, f.at, "no retry for 30 s")
			cctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.ErrorIs(t, r.p.Close(cctx), conversation.ErrPersistRejected)
		})
	}
}

func TestPersisterAmbiguousFinalizeIsNotFenceLoss(t *testing.T) {
	r := startPersister(t)
	require.NoError(t, r.turns.TurnRepo.Insert(context.Background(), pFence, insertJob().Turn))
	// The commit went through but the connection broke; the retry matches no row because the
	// turn is final already (status IS NULL), which the repository reports as ErrFenceLost.
	r.turns.add("Finalize", step{err: errConnReset, commit: true}, step{err: session.ErrFenceLost})
	require.NoError(t, r.p.Submit(finalJob(session.TurnCompleted)))
	require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "TURN_FAILED"}))
	require.NoError(t, flush(t, r.p))
	require.Empty(t, r.lost, "the session is still ours: not a fence loss")
	require.Zero(t, r.m.FenceLostTotal())
	require.Empty(t, r.fatal)
	_, reads, _ := r.sess.state()
	require.Positive(t, reads, "ownership is re-checked before deciding")
	require.Equal(t, session.TurnCompleted, *r.turns.Turns(pSession)[0].Status)
	require.Len(t, r.events.Events(), 1)
}

func TestPersisterAmbiguousFinalizeThenFenceReallyLost(t *testing.T) {
	r := startPersister(t)
	require.NoError(t, r.turns.TurnRepo.Insert(context.Background(), pFence, insertJob().Turn))
	r.turns.add("Finalize", step{err: errAmbiguous}, step{err: session.ErrFenceLost})
	r.sess.setFence(pFence + 1)
	require.NoError(t, r.p.Submit(finalJob(session.TurnCompleted)))
	select {
	case err := <-r.lost:
		require.ErrorIs(t, err, session.ErrFenceLost)
	case <-time.After(2 * time.Second):
		t.Fatal("OnFenceLost not called")
	}
	require.ErrorIs(t, flush(t, r.p), session.ErrFenceLost)
}

func TestPersisterAddDisconnectedAmbiguousCommittedCountsOnce(t *testing.T) {
	r := startPersister(t)
	r.sess.mu.Lock()
	r.sess.disc = 10
	r.sess.mu.Unlock()
	r.sess.add("AddDisconnected", step{err: errAmbiguous, commit: true})
	require.NoError(t, r.p.Submit(conversation.AddDisconnected{Seconds: 4}))
	require.NoError(t, flush(t, r.p))
	disc, _, writes := r.sess.state()
	require.Equal(t, 14, disc, "the episode is counted once")
	require.Equal(t, 1, writes, "not sent again once the read-back shows it committed")
	require.Empty(t, r.fatal)
}

func TestPersisterAddDisconnectedAmbiguousNotCommittedRetried(t *testing.T) {
	r := startPersister(t)
	r.sess.mu.Lock()
	r.sess.disc = 10 // the read-back compares with the value before the first attempt, not with 0
	r.sess.mu.Unlock()
	r.sess.add("AddDisconnected", step{err: errConnReset})
	require.NoError(t, r.p.Submit(conversation.AddDisconnected{Seconds: 4}))
	require.NoError(t, flush(t, r.p))
	disc, _, writes := r.sess.state()
	require.Equal(t, 14, disc)
	require.Equal(t, 2, writes)
}

func TestPersisterAddDisconnectedClampedCommitNotResent(t *testing.T) {
	r := startPersister(t)
	r.sess.mu.Lock()
	r.sess.clamped = []int{49, 48} // the SQL keeps 49 of 60 s; a later re-send would keep 48
	r.sess.mu.Unlock()
	r.sess.add("AddDisconnected", step{err: errAmbiguous, commit: true})
	require.NoError(t, r.p.Submit(conversation.AddDisconnected{Seconds: 60}))
	require.NoError(t, flush(t, r.p))
	disc, _, writes := r.sess.state()
	require.Equal(t, 49, disc, "a committed, clamped episode is not added again")
	require.Equal(t, 1, writes)
	require.Empty(t, r.fatal)
}

func TestPersisterAddDisconnectedClampedNotCommittedRetried(t *testing.T) {
	r := startPersister(t)
	r.sess.mu.Lock()
	r.sess.disc = 10
	r.sess.clamped = []int{49}
	r.sess.mu.Unlock()
	r.sess.add("AddDisconnected", step{err: errAmbiguous}) // timed out before committing
	require.NoError(t, r.p.Submit(conversation.AddDisconnected{Seconds: 60}))
	require.NoError(t, flush(t, r.p))
	disc, _, writes := r.sess.state()
	require.Equal(t, 59, disc, "sent again and added once")
	require.Equal(t, 2, writes)
}

func TestPersisterUpdateUserMergeStaysBeforeFinalize(t *testing.T) {
	r := startPersister(t)
	require.NoError(t, r.turns.TurnRepo.Insert(context.Background(), pFence, insertJob().Turn))
	gate := make(chan struct{})
	r.turns.setGate(gate)
	require.NoError(t, r.p.Submit(userJob("u0")))
	waitSignal(t, r.turns.entered, "UpdateUser in flight")
	require.NoError(t, r.p.Submit(userJob("u1")))
	require.NoError(t, r.p.Submit(finalJob(session.TurnCompleted)))
	require.NoError(t, r.p.Submit(userJob("u2")))
	require.Equal(t, 2, r.p.Pending(), "u2 replaces u1 at its place, ahead of the Finalize")
	close(gate)
	require.NoError(t, flush(t, r.p))
	require.Equal(t, []string{"UpdateUser", "UpdateUser", "Finalize"}, r.turns.callLog(),
		"the user side is written while status IS NULL, as the fenced SQL requires")
	require.Equal(t, []string{"u0", "u2"}, r.turns.userTexts())
}

func TestPersisterAbortDropsQueueWithoutWriting(t *testing.T) {
	r := startPersister(t)
	gate := queueBlocked(t, r, 3)
	defer close(gate)
	r.p.Abort()
	require.Zero(t, r.p.Pending(), "the queue is dropped")
	require.ErrorIs(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN"}), conversation.ErrPersisterClosed)
	require.Empty(t, r.events.Events())
	require.Empty(t, r.lost)
	require.Zero(t, r.m.FenceLostTotal(), "Abort after a fence loss seen elsewhere does not count it again")
}

func TestPersisterCloseStopsOnContext(t *testing.T) {
	r := startPersister(t)
	r.turns.setGate(make(chan struct{}))
	require.NoError(t, r.p.Submit(userJob("never")))
	waitSignal(t, r.turns.entered, "UpdateUser in flight")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.ErrorIs(t, r.p.Close(ctx), context.DeadlineExceeded, "Close gives up at its deadline")
	require.Less(t, time.Since(start), time.Second)
}

func TestPersisterStopsWithContext(t *testing.T) {
	r := startPersister(t)
	r.cancel()
	require.Eventually(t, func() bool { return r.p.Submit(userJob("x")) != nil }, time.Second, time.Millisecond,
		"after the session context ends nothing is accepted")
}
