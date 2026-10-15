package conversation_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

const rsPod = "pod-a"

var errRedisDown = errors.New("redis: connection refused")

// flakyStore wraps fakes.RuntimeStore: it records every write call with its TTL and fails all of
// them while down is set.
type flakyStore struct {
	*fakes.RuntimeStore
	down  atomic.Bool
	block atomic.Bool // writes wait for ctx to end (a hung Redis)
	mu    sync.Mutex
	calls []string
	ttls  map[string]time.Duration
}

func newFlakyStore() *flakyStore {
	return &flakyStore{RuntimeStore: fakes.NewRuntimeStore(), ttls: map[string]time.Duration{}}
}

func (s *flakyStore) note(ctx context.Context, call string, ttl time.Duration) error {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	if ttl != 0 {
		s.ttls[call] = ttl
	}
	s.mu.Unlock()
	if s.block.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.down.Load() {
		return errRedisDown
	}
	return nil
}

func (s *flakyStore) callLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *flakyStore) count(call string) int {
	n := 0
	for _, c := range s.callLog() {
		if c == call {
			n++
		}
	}
	return n
}

func (s *flakyStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func (s *flakyStore) ttl(call string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttls[call]
}

func (s *flakyStore) AcquireOwner(ctx context.Context, sid uuid.UUID, pod string, fence int64, ttl time.Duration) (bool, error) {
	if err := s.note(ctx, "AcquireOwner", ttl); err != nil {
		return false, err
	}
	return s.RuntimeStore.AcquireOwner(ctx, sid, pod, fence, ttl)
}

func (s *flakyStore) ReleaseOwner(ctx context.Context, sid uuid.UUID, pod string, fence int64) error {
	if err := s.note(ctx, "ReleaseOwner", 0); err != nil {
		return err
	}
	return s.RuntimeStore.ReleaseOwner(ctx, sid, pod, fence)
}

func (s *flakyStore) WriteHeartbeat(ctx context.Context, sid uuid.UUID, at time.Time, ttl time.Duration) error {
	if err := s.note(ctx, "WriteHeartbeat", ttl); err != nil {
		return err
	}
	return s.RuntimeStore.WriteHeartbeat(ctx, sid, at, ttl)
}

func (s *flakyStore) WriteRT(ctx context.Context, sid uuid.UUID, rt rtstate.RT, ttl time.Duration) error {
	if err := s.note(ctx, "WriteRT", ttl); err != nil {
		return err
	}
	return s.RuntimeStore.WriteRT(ctx, sid, rt, ttl)
}

func (s *flakyStore) WriteTurn(ctx context.Context, sid uuid.UUID, t rtstate.TurnSnap, ttl time.Duration) error {
	if err := s.note(ctx, "WriteTurn", ttl); err != nil {
		return err
	}
	return s.RuntimeStore.WriteTurn(ctx, sid, t, ttl)
}

func (s *flakyStore) DeleteTurn(ctx context.Context, sid uuid.UUID) error {
	if err := s.note(ctx, "DeleteTurn", 0); err != nil {
		return err
	}
	return s.RuntimeStore.DeleteTurn(ctx, sid)
}

func (s *flakyStore) AppendEvent(ctx context.Context, sid uuid.UUID, e rtstate.EventEntry) error {
	if err := s.note(ctx, "AppendEvent", 0); err != nil {
		return err
	}
	return s.RuntimeStore.AppendEvent(ctx, sid, e)
}

func (s *flakyStore) WriteSummary(ctx context.Context, sid uuid.UUID, sum rtstate.Summary, ttl time.Duration) error {
	if err := s.note(ctx, "WriteSummary", ttl); err != nil {
		return err
	}
	return s.RuntimeStore.WriteSummary(ctx, sid, sum, ttl)
}

// stepTicker is a Ticker the test fires by hand.
type stepTicker struct {
	c      chan time.Time
	period atomic.Int64
}

func (s *stepTicker) C() <-chan time.Time { return s.c }
func (*stepTicker) Stop()                 {}

type syncRig struct {
	s      *conversation.RedisSync
	store  *flakyStore
	clk    *clock.Fake
	m      *conversation.Metrics
	tick   *stepTicker
	passed chan struct{}
	lost   chan error
	cancel context.CancelFunc
}

func startSync(t *testing.T, opts ...conversation.RedisSyncOption) *syncRig {
	t.Helper()
	noLeak(t)
	r := &syncRig{store: newFlakyStore(), clk: clock.NewFake(pT0), m: conversation.NewMetrics(nil),
		tick: &stepTicker{c: make(chan time.Time)}, passed: make(chan struct{}, 1), lost: make(chan error, 8)}
	all := append([]conversation.RedisSyncOption{
		conversation.RedisSyncTicker(func(d time.Duration) conversation.Ticker { r.tick.period.Store(int64(d)); return r.tick }),
		conversation.RedisSyncOnFenceLost(func(err error) { r.lost <- err }),
		conversation.RedisSyncPassHook(func() { r.passed <- struct{}{} }),
	}, opts...)
	r.s = conversation.NewRedisSync(r.store, r.clk, pSession, rsPod, pFence, r.m, nil, all...)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.s.Start(ctx)
	waitSignal(t, r.passed, "first pass") // Start runs one pass right away (owner refresh + heartbeat)
	t.Cleanup(func() {
		cancel()
		r.s.Abort()
	})
	return r
}

// step advances the fake clock by d, fires one tick and waits for the pass it triggers.
func (r *syncRig) step(t *testing.T, d time.Duration) {
	t.Helper()
	r.clk.Advance(d)
	select {
	case r.tick.c <- r.clk.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("redisSync did not take the tick")
	}
	waitSignal(t, r.passed, "redisSync pass")
}

func activeRT(status string) rtstate.RT {
	return rtstate.RT{Status: status, TurnState: "LISTENING", AcceptingTurns: true, Connected: true,
		ExpiresAtMS: pT0.Add(10 * time.Minute).UnixMilli(), NextTurnSeq: 1}
}

func readRT(t *testing.T, st rtstate.Store) rtstate.RT {
	t.Helper()
	rt, ok, err := st.ReadRT(context.Background(), pSession)
	require.NoError(t, err)
	require.True(t, ok)
	return rt
}

func durableEvent(seq int64, typ string) (protocol.ServerEvent, []byte) {
	ev := protocol.ServerEvent{Type: typ, ServerSeq: seq, SessionID: pSession, TS: pT0}
	switch typ {
	case "turn.completed":
		ev.Payload = protocol.TurnCompleted{}
	case "ai.text_delta":
		ev.Payload = protocol.AITextDelta{}
	case "stt.partial":
		ev.Payload = protocol.STTPartial{}
	case "balance.tick":
		ev.Payload = protocol.BalanceTick{}
	}
	env, err := protocol.MarshalServer(ev)
	if err != nil {
		panic(err)
	}
	return ev, env
}

func errorEvent(seq int64, fatal bool) (protocol.ServerEvent, []byte) {
	ev := protocol.ServerEvent{Type: "error", ServerSeq: seq, SessionID: pSession, TS: pT0,
		Payload: protocol.ErrorPayload{Error: protocol.ErrorBody{Code: "INVALID_COMMAND", Message: "x"}, Fatal: fatal}}
	env, err := protocol.MarshalServer(ev)
	if err != nil {
		panic(err)
	}
	return ev, env
}

func TestRedisSyncCoalesce(t *testing.T) {
	r := startSync(t)
	require.Equal(t, 1, r.store.count("AcquireOwner"), "the first pass refreshes the owner")
	require.Equal(t, 1, r.store.count("WriteHeartbeat"), "and writes the heartbeat")
	for i := range 10 {
		rt := activeRT("ACTIVE")
		rt.NextTurnSeq = i + 1
		r.s.Update(rt)
		r.step(t, 9*time.Millisecond) // 10 updates within 90 ms
	}
	require.Zero(t, r.store.count("WriteRT"), "no write before 100 ms")
	r.step(t, 10*time.Millisecond) // 100 ms after the first update
	require.Equal(t, 1, r.store.count("WriteRT"), "10 updates in 100 ms → one HSET")
	got := readRT(t, r.store)
	require.Equal(t, 10, got.NextTurnSeq, "the newest snapshot is written")
	require.Equal(t, rsPod, got.PodID)
	require.Equal(t, pFence, got.Fence)
	require.Equal(t, r.clk.Now().UnixMilli(), got.UpdatedAtMS)
	r.step(t, time.Second)
	require.Equal(t, 1, r.store.count("WriteRT"), "nothing new, nothing written")
	require.Equal(t, 50*time.Millisecond, time.Duration(r.tick.period.Load()))
}

func TestRedisSyncTurnCoalesce250ms(t *testing.T) {
	r := startSync(t)
	for i := range 5 {
		r.s.UpdateTurn(rtstate.TurnSnap{TurnID: pTurn, TurnSeq: 1, AssistantText: "あ", SpokenSentences: i})
		r.step(t, 50*time.Millisecond)
	}
	require.Equal(t, 1, r.store.count("WriteTurn"), "the first write happens 250 ms after the first change")
	snap, ok, err := r.store.ReadTurn(context.Background(), pSession)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 4, snap.SpokenSentences)
	r.s.ClearTurn()
	r.step(t, 50*time.Millisecond)
	require.Equal(t, 1, r.store.count("DeleteTurn"), "DEL when the turn is final, without waiting")
	_, ok, err = r.store.ReadTurn(context.Background(), pSession)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestRedisSyncTTL(t *testing.T) {
	r := startSync(t, conversation.RedisSyncSessionDuration(10*time.Minute))
	require.Equal(t, 15*time.Second, r.store.ttl("AcquireOwner"), "owner TTL 15 s")
	require.Equal(t, 60*time.Second, r.store.ttl("WriteHeartbeat"), "hb TTL 60 s")

	pre := activeRT("STARTING")
	pre.ExpiresAtMS = 0
	r.s.Update(pre)
	r.step(t, 100*time.Millisecond)
	require.Equal(t, 10*time.Minute+600*time.Second, r.store.ttl("WriteRT"), "before ACTIVE: duration + 600 s")

	r.s.Update(activeRT("ACTIVE")) // expires at t0+10 min
	r.s.UpdateSummary(rtstate.Summary{Text: "要約", CoveredUntilSeq: 3})
	r.s.UpdateTurn(rtstate.TurnSnap{TurnID: pTurn, TurnSeq: 1})
	r.step(t, 250*time.Millisecond)
	want := pT0.Add(10*time.Minute).Sub(r.clk.Now()) + 300*time.Second
	require.Equal(t, want, r.store.ttl("WriteRT"), "TTL_S = expires_at − now + 300 s")
	require.Equal(t, want, r.store.ttl("WriteTurn"))
	require.Equal(t, want, r.store.ttl("WriteSummary"))

	r.step(t, 20*time.Minute) // past expires_at
	r.s.Update(activeRT("ENDING"))
	r.step(t, 100*time.Millisecond)
	require.Equal(t, 300*time.Second, r.store.ttl("WriteRT"), "TTL_S never below 300 s")
}

func TestRedisSyncHeartbeatAndOwnerEvery5s(t *testing.T) {
	r := startSync(t)
	r.step(t, 4900*time.Millisecond)
	require.Equal(t, 1, r.store.count("WriteHeartbeat"))
	require.Equal(t, 1, r.store.count("AcquireOwner"))
	r.step(t, 100*time.Millisecond)
	require.Equal(t, 2, r.store.count("WriteHeartbeat"), "hb every 5 s")
	require.Equal(t, 2, r.store.count("AcquireOwner"), "owner refresh every 5 s")
	at, ok, err := r.store.ReadHeartbeat(context.Background(), pSession)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, r.clk.Now().UnixMilli(), at.UnixMilli())
	pod, fence, ok := r.store.Owner(pSession)
	require.True(t, ok)
	require.Equal(t, rsPod, pod)
	require.Equal(t, pFence, fence)
}

func TestEventsOnlyDurable(t *testing.T) {
	r := startSync(t)
	send := func(ev protocol.ServerEvent, env []byte) { r.s.Event(ev, env) }
	send(durableEvent(1, "turn.completed"))
	send(durableEvent(2, "ai.text_delta"))
	send(durableEvent(3, "stt.partial"))
	send(errorEvent(4, false)) // non-fatal error: durable (§7.4.3)
	send(errorEvent(5, true))  // fatal error: transient
	send(durableEvent(6, "balance.tick"))
	replayed, renv := durableEvent(7, "turn.completed")
	replayed.Replay = true // a replayed event is already in the stream
	send(replayed, renv)
	send(durableEvent(8, "turn.completed"))
	r.step(t, 50*time.Millisecond)

	evs, err := r.store.ReadEvents(context.Background(), pSession)
	require.NoError(t, err)
	var seqs, prev []int64
	for _, e := range evs {
		seqs = append(seqs, e.Seq)
		prev = append(prev, e.PrevSeq)
	}
	require.Equal(t, []int64{1, 4, 8}, seqs, "only durable events enter the replay stream")
	require.Equal(t, []int64{0, 1, 4}, prev, "prev_seq chains the durable events")
	require.Equal(t, "error", evs[1].Type)
	_, wantEnv := errorEvent(4, false)
	require.JSONEq(t, string(wantEnv), string(evs[1].Env), "env is the complete envelope")
}

func TestRedisSyncLastDurableSeqOnTakeover(t *testing.T) {
	r := startSync(t, conversation.RedisSyncLastDurableSeq(41))
	r.s.Event(durableEvent(42, "turn.completed"))
	r.step(t, 50*time.Millisecond)
	evs, err := r.store.ReadEvents(context.Background(), pSession)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, int64(41), evs[0].PrevSeq)
}

func TestRedisDownKeepsRunning(t *testing.T) {
	r := startSync(t)
	rt1 := activeRT("ACTIVE")
	r.s.Update(rt1)
	r.s.UpdateTurn(rtstate.TurnSnap{TurnID: pTurn, TurnSeq: 1, UserText: "一"})
	r.s.UpdateSummary(rtstate.Summary{Text: "要約1", CoveredUntilSeq: 1})
	r.s.Event(durableEvent(1, "turn.completed"))
	r.step(t, 250*time.Millisecond)
	require.Equal(t, 1, r.store.count("WriteRT"))
	require.Equal(t, 1, r.store.count("AppendEvent"))

	// Redis goes away.
	r.store.down.Store(true)
	r.store.reset()
	rt2 := activeRT("ACTIVE")
	rt2.NextTurnSeq = 2
	r.s.Update(rt2)
	r.s.Event(durableEvent(2, "turn.completed"))
	r.step(t, 250*time.Millisecond) // the writes fail: Redis is marked down
	require.NotEmpty(t, r.store.callLog())
	r.store.reset()

	// While down: nothing blocks, nothing is attempted before the next probe (5 s).
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt3 := activeRT("ACTIVE")
		rt3.NextTurnSeq = 3
		rt3.Connected = false
		r.s.Update(rt3)
		r.s.UpdateTurn(rtstate.TurnSnap{TurnID: pTurn, TurnSeq: 3, UserText: "三"})
		r.s.UpdateSummary(rtstate.Summary{Text: "要約3", CoveredUntilSeq: 3})
	}()
	waitSignal(t, done, "updates while Redis is down")
	r.step(t, time.Second)
	r.step(t, time.Second)
	require.Empty(t, r.store.callLog(), "no Redis traffic while down until the next probe")
	r.step(t, 2*time.Second) // 4.25 s after the failure: still before the probe
	require.Empty(t, r.store.callLog())
	r.step(t, 1*time.Second) // probe fails again
	require.NotEmpty(t, r.store.callLog())
	r.s.Event(durableEvent(3, "turn.completed")) // emitted while down: lost for good

	// Redis comes back: the next probe rewrites everything with the newest snapshot.
	r.store.down.Store(false)
	r.store.reset()
	r.step(t, 5*time.Second)
	calls := r.store.callLog()
	require.Equal(t, "AcquireOwner", calls[0], "owner first, via the Lua script")
	for _, c := range []string{"WriteHeartbeat", "WriteRT", "WriteTurn", "WriteSummary"} {
		require.Contains(t, calls, c)
	}
	require.NotContains(t, calls, "AppendEvent", "missed replay events are not rewritten")
	got := readRT(t, r.store)
	require.Equal(t, 3, got.NextTurnSeq)
	require.False(t, got.Connected)
	snap, _, err := r.store.ReadTurn(context.Background(), pSession)
	require.NoError(t, err)
	require.Equal(t, "三", snap.UserText)
	sum, _, err := r.store.ReadSummary(context.Background(), pSession)
	require.NoError(t, err)
	require.Equal(t, "要約3", sum.Text)
	at, _, err := r.store.ReadHeartbeat(context.Background(), pSession)
	require.NoError(t, err)
	require.Equal(t, r.clk.Now().UnixMilli(), at.UnixMilli())

	// The stream resumes; prev_seq exposes the gap so a resume reports UNAVAILABLE.
	r.s.Event(durableEvent(4, "turn.completed"))
	r.step(t, 50*time.Millisecond)
	evs, err := r.store.ReadEvents(context.Background(), pSession)
	require.NoError(t, err)
	require.Len(t, evs, 2)
	require.Equal(t, int64(1), evs[0].Seq)
	require.Equal(t, int64(4), evs[1].Seq)
	require.Equal(t, int64(3), evs[1].PrevSeq)
	require.Empty(t, r.lost, "a Redis outage is not a fence loss")
}

func TestRedisSyncOpTimeout200ms(t *testing.T) {
	var mu sync.Mutex
	var asked []time.Duration
	r := startSync(t, conversation.RedisSyncTimeoutFunc(func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		mu.Lock()
		asked = append(asked, d)
		mu.Unlock()
		return context.WithTimeout(ctx, time.Millisecond) // a hung Redis gives up quickly in the test
	}))
	r.store.block.Store(true)
	r.s.Update(activeRT("ACTIVE"))
	r.step(t, 100*time.Millisecond) // returns although WriteRT hangs
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, asked)
	for _, d := range asked {
		require.Equal(t, 200*time.Millisecond, d, "every Redis operation is bounded by 200 ms")
	}
}

func TestOwnerRefreshLost(t *testing.T) {
	r := startSync(t)
	ok, err := r.store.RuntimeStore.AcquireOwner(context.Background(), pSession, "pod-b", pFence+1, 15*time.Second)
	require.NoError(t, err)
	require.True(t, ok) // a takeover with a newer fence
	r.s.Update(activeRT("ACTIVE"))
	r.clk.Advance(5 * time.Second)
	r.tick.c <- r.clk.Now()
	select {
	case err := <-r.lost:
		require.ErrorIs(t, err, session.ErrFenceLost)
	case <-time.After(2 * time.Second):
		t.Fatal("OnFenceLost not called")
	}
	waitSignal(t, r.passed, "pass")
	require.Zero(t, r.store.count("WriteRT"), "nothing is written once the owner refresh fails")
	require.InDelta(t, 1, r.m.FenceLostTotal(), 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.ErrorIs(t, r.s.Flush(ctx), session.ErrFenceLost)
	require.Zero(t, r.store.count("WriteRT"))
	pod, _, _ := r.store.Owner(pSession)
	require.Equal(t, "pod-b", pod)
	require.Empty(t, r.lost, "OnFenceLost fires once")
}

func TestRedisSyncFlushAndRelease(t *testing.T) {
	r := startSync(t)
	rt := activeRT("ACTIVE")
	rt.Connected = false
	r.s.Update(rt)
	r.s.UpdateTurn(rtstate.TurnSnap{TurnID: pTurn, TurnSeq: 1})
	r.s.UpdateSummary(rtstate.Summary{Text: "要約", CoveredUntilSeq: 2})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.store.reset()
	require.NoError(t, r.s.Flush(ctx), "Flush writes now, without waiting for the coalesce window")
	for _, c := range []string{"WriteHeartbeat", "WriteRT", "WriteTurn", "WriteSummary"} {
		require.Equal(t, 1, r.store.count(c), c)
	}
	require.False(t, readRT(t, r.store).Connected)

	done := activeRT("COMPLETED")
	r.s.Update(done)
	r.s.ClearTurn()
	require.NoError(t, r.s.Release(ctx))
	require.Equal(t, "COMPLETED", readRT(t, r.store).Status, "the final rt is written before the owner is released")
	_, _, ok := r.store.Owner(pSession)
	require.False(t, ok, "owner released")
	r.store.reset()
	r.clk.Advance(10 * time.Second)
	select {
	case r.tick.c <- r.clk.Now():
		t.Fatal("the sync loop still runs after Release")
	case <-time.After(20 * time.Millisecond):
	}
	require.Empty(t, r.store.callLog(), "no heartbeat after Release")
}

func TestRedisSyncFlushWhileDownFails(t *testing.T) {
	r := startSync(t)
	r.store.down.Store(true)
	r.s.Update(activeRT("ACTIVE"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(t, r.s.Flush(ctx))
	require.Empty(t, r.lost)
}

func TestRedisSyncAcquire(t *testing.T) {
	r := startSync(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, r.s.Acquire(ctx))
	pod, fence, ok := r.store.Owner(pSession)
	require.True(t, ok)
	require.Equal(t, rsPod, pod)
	require.Equal(t, pFence, fence)

	ok, err := r.store.RuntimeStore.AcquireOwner(ctx, pSession, "pod-b", pFence+1, 15*time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorIs(t, r.s.Acquire(ctx), session.ErrFenceLost)
	select {
	case err := <-r.lost:
		require.ErrorIs(t, err, session.ErrFenceLost)
	case <-time.After(2 * time.Second):
		t.Fatal("OnFenceLost not called")
	}
}

func TestRedisSyncTuningOverrides(t *testing.T) {
	r := startSync(t, conversation.RedisSyncTuning(config.RealtimeTuning{OwnerTTL: 20 * time.Second, HeartbeatTTL: 90 * time.Second,
		OwnerRefreshInterval: 2 * time.Second, HeartbeatInterval: 3 * time.Second}))
	require.Equal(t, 20*time.Second, r.store.ttl("AcquireOwner"))
	require.Equal(t, 90*time.Second, r.store.ttl("WriteHeartbeat"))
	r.step(t, 2*time.Second)
	require.Equal(t, 2, r.store.count("AcquireOwner"), "owner refresh every OWNER_REFRESH_INTERVAL")
	require.Equal(t, 1, r.store.count("WriteHeartbeat"))
	r.step(t, time.Second)
	require.Equal(t, 2, r.store.count("WriteHeartbeat"), "hb every HEARTBEAT_INTERVAL")
}
