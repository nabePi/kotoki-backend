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
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var (
	pSession = uuid.MustParse("0193a000-0000-7000-8000-000000000001")
	pTurn    = uuid.MustParse("0193a000-0000-7000-8000-0000000000a1")
	pT0      = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
)

const pFence = int64(7)

// recTurns wraps fakes.TurnRepo: it records the order of calls, can block UpdateUser until
// released, and can fail every write while failing is set.
type recTurns struct {
	*fakes.TurnRepo
	mu      sync.Mutex
	calls   []string
	users   []string // TurnUser.Text of every UpdateUser that reached the repository
	gate    chan struct{}
	entered chan struct{}
	failing atomic.Bool
	failN   atomic.Int32 // the next failN calls fail, then the database is back
}

func newRecTurns() *recTurns {
	return &recTurns{TurnRepo: fakes.NewTurnRepo(), entered: make(chan struct{}, 64)}
}

func (r *recTurns) record(call string) error {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	if r.failing.Load() || r.failN.Add(-1) >= 0 {
		return errors.New("db down")
	}
	return nil
}

func (r *recTurns) Insert(ctx context.Context, fence int64, t conversation.TurnStart) error {
	if err := r.record("Insert"); err != nil {
		return err
	}
	return r.TurnRepo.Insert(ctx, fence, t)
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
	if err := r.record("UpdateUser"); err != nil {
		return err
	}
	return r.TurnRepo.UpdateUser(ctx, fence, id, u)
}

func (r *recTurns) Finalize(ctx context.Context, fence int64, id uuid.UUID, f conversation.TurnFinal) error {
	if err := r.record("Finalize"); err != nil {
		return err
	}
	return r.TurnRepo.Finalize(ctx, fence, id, f)
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

// discRec records AddDisconnected calls.
type discRec struct {
	mu      sync.Mutex
	seconds []int
	err     error
}

func (d *discRec) AddDisconnected(_ context.Context, sid uuid.UUID, fence int64, seconds int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	if sid != pSession || fence != pFence {
		return fmt.Errorf("unexpected sid/fence %s/%d", sid, fence)
	}
	d.seconds = append(d.seconds, seconds)
	return nil
}

func (d *discRec) got() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.seconds...)
}

type persistRig struct {
	p      *conversation.Persister
	turns  *recTurns
	events *fakes.EventWriter
	disc   *discRec
	clk    *clock.Fake
	m      *conversation.Metrics
	lost   chan error
	stuck  chan error
	cancel context.CancelFunc
}

func startPersister(t *testing.T, opts ...conversation.PersisterOption) *persistRig {
	t.Helper()
	noLeak(t)
	r := &persistRig{turns: newRecTurns(), events: fakes.NewEventWriter(), disc: &discRec{},
		clk: clock.NewFake(pT0), m: conversation.NewMetrics(nil), lost: make(chan error, 8), stuck: make(chan error, 8)}
	all := append([]conversation.PersisterOption{
		conversation.PersisterClock(r.clk),
		conversation.PersisterSleep(fastSleep(r.clk)),
		conversation.PersisterOnFenceLost(func(err error) { r.lost <- err }),
		conversation.PersisterOnStuck(func(err error) { r.stuck <- err }),
	}, opts...)
	r.p = conversation.NewPersister(pSession, r.turns, r.events, r.disc, pFence, r.m, nil, all...)
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

// waitSignal waits for one value on ch.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
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
	require.Equal(t, []int{4}, r.disc.got())

	cctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, r.p.Close(cctx))
	require.ErrorIs(t, r.p.Submit(userJob("late")), conversation.ErrPersisterClosed)
	require.Empty(t, r.lost)
	require.Empty(t, r.stuck)
}

func TestPersisterCoalesce(t *testing.T) {
	r := startPersister(t)
	require.NoError(t, r.turns.Insert(context.Background(), pFence, conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}))
	gate := make(chan struct{})
	r.turns.mu.Lock()
	r.turns.gate = gate
	r.turns.mu.Unlock()

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
	r.turns.mu.Lock()
	r.turns.gate = gate
	r.turns.mu.Unlock()

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
	r.turns.mu.Lock()
	r.turns.gate = gate
	r.turns.mu.Unlock()
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
	require.Empty(t, r.disc.got())
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
	require.Empty(t, r.stuck, "a short outage is not stuck")
	require.Less(t, r.clk.Now().Sub(pT0), 5*time.Second, "backoff starts small")
}

func TestPersisterStuckWhenFailingOver30s(t *testing.T) {
	r := startPersister(t)
	clk := r.clk
	r.turns.failing.Store(true)
	require.NoError(t, r.p.Submit(conversation.InsertTurn{Turn: conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}}))
	select {
	case err := <-r.stuck:
		require.ErrorIs(t, err, conversation.ErrPersisterStuck)
	case <-time.After(2 * time.Second):
		t.Fatal("OnStuck not called")
	}
	require.Greater(t, clk.Now().Sub(pT0), 30*time.Second, "stuck only after more than 30 s of failure")
	require.Empty(t, r.stuck, "OnStuck fires once")
}

func TestPersisterStuckWhenQueueOver64For30s(t *testing.T) {
	r := startPersister(t)
	clk := r.clk
	require.NoError(t, r.turns.Insert(context.Background(), pFence, conversation.TurnStart{ID: pTurn, SessionID: pSession, Sequence: 1, CreatedAt: pT0}))
	gate := make(chan struct{})
	r.turns.mu.Lock()
	r.turns.gate = gate
	r.turns.mu.Unlock()
	require.NoError(t, r.p.Submit(userJob("blocked")))
	waitSignal(t, r.turns.entered, "UpdateUser in flight")
	for i := range 65 {
		require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN", Payload: map[string]int{"i": i}}), "the loop is never refused")
	}
	require.Equal(t, 65, r.p.Pending())
	clk.Advance(30 * time.Second)
	require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN"}))
	require.Empty(t, r.stuck, "exactly 30 s is not stuck yet")
	clk.Advance(time.Second)
	require.NoError(t, r.p.Submit(conversation.SessionEvent{Type: "BARGE_IN"}))
	close(gate) // the worker reports after its in-flight job
	select {
	case err := <-r.stuck:
		require.ErrorIs(t, err, conversation.ErrPersisterStuck)
	case <-time.After(2 * time.Second):
		t.Fatal("OnStuck not called")
	}
	require.NoError(t, flush(t, r.p))
	require.Len(t, r.events.Events(), 67)
}

func TestPersisterCloseStopsOnContext(t *testing.T) {
	r := startPersister(t)
	gate := make(chan struct{})
	r.turns.mu.Lock()
	r.turns.gate = gate
	r.turns.mu.Unlock()
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
