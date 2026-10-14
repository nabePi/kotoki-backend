package provider_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const bytesPerMs = provider.STTSampleRate * 2 / 1000 // 32

// recConn is a recording STT connection the test drives by hand.
type recConn struct {
	id       int
	mu       sync.Mutex
	audio    []byte
	sends    []int
	finalize int
	closed   bool
	events   chan provider.STTEvent
	gate     chan struct{} // when set, SendAudio waits for a token
	evOnce   sync.Once
	// closeEvents makes Close behave like a real vendor: send CLOSED and end the channel.
	closeEvents bool
}

func (c *recConn) SendAudio(pcm []byte) error {
	if c.gate != nil {
		<-c.gate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return provider.ErrSessionClosed
	}
	c.audio = append(c.audio, pcm...)
	c.sends = append(c.sends, len(pcm))
	return nil
}

func (c *recConn) Finalize() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalize++
	return nil
}

func (c *recConn) Events() <-chan provider.STTEvent { return c.events }

func (c *recConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if c.closeEvents {
		c.evOnce.Do(func() {
			c.events <- provider.STTEvent{Type: provider.STTClosed}
			close(c.events)
		})
	}
	return nil
}

func (c *recConn) Stats() provider.STTStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return provider.STTStats{AudioMsSent: int64(len(c.audio) / bytesPerMs)}
}

func (c *recConn) audioLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.audio)
}

func (c *recConn) finalizes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finalize
}

func (c *recConn) push(ev provider.STTEvent) { c.events <- ev }

func (c *recConn) endEvents() { c.evOnce.Do(func() { close(c.events) }) }

func closingVendor(c *recConn) { c.closeEvents = true }

// recSTT hands out recConns.
type recSTT struct {
	name      provider.Name
	mu        sync.Mutex
	conns     []*recConn
	failOpen  func(n int) error // n = 1-based Open call number
	hangOpen  bool
	configure func(*recConn)
	opens     atomic.Int32
}

func (r *recSTT) Name() provider.Name {
	if r.name == "" {
		return provider.FakePrimary
	}
	return r.name
}

func (r *recSTT) Model() string { return "rec" }

func (r *recSTT) Open(ctx context.Context, _ provider.STTSessionConfig) (provider.STTSession, error) {
	n := int(r.opens.Add(1))
	if r.hangOpen {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if r.failOpen != nil {
		if err := r.failOpen(n); err != nil {
			return nil, err
		}
	}
	c := &recConn{id: n, events: make(chan provider.STTEvent, 64)}
	if r.configure != nil {
		r.configure(c)
	}
	r.mu.Lock()
	r.conns = append(r.conns, c)
	r.mu.Unlock()
	return c, nil
}

func (r *recSTT) conn(i int) *recConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[i]
}

func (r *recSTT) connCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *recSTT) cleanup(t *testing.T) {
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.conns {
			c.endEvents()
		}
	})
}

func sttCfg() provider.STTSessionConfig {
	return provider.STTSessionConfig{SessionID: uuid.New(), Language: "ja", SampleRate: provider.STTSampleRate}
}

// sendMs feeds ms of audio in 20 ms chunks (tagged so replay content can be checked).
func sendMs(t *testing.T, s provider.STTSession, ms int) {
	t.Helper()
	for i := 0; i < ms/20; i++ {
		require.NoError(t, s.SendAudio(make([]byte, 640)))
	}
}

func nextEv(t *testing.T, s provider.STTSession) provider.STTEvent {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		require.True(t, ok)
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no STT event")
	}
	return provider.STTEvent{}
}

func noEvent(t *testing.T, s provider.STTSession, d time.Duration) {
	t.Helper()
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(d):
	}
}

func final(start, end int64, text string, fromFinalize bool) provider.STTEvent {
	return provider.STTEvent{Type: provider.STTFinal, Text: text, StartMs: start, EndMs: end, FromFinalize: fromFinalize}
}

func vendorErr(code string) provider.STTEvent {
	return provider.STTEvent{Type: provider.STTError, Err: &provider.Error{Provider: provider.FakePrimary, Capability: provider.CapSTT, Kind: provider.KindUnavailable, VendorCode: code}}
}

func openRec(t *testing.T, rec *recSTT, opts ...testOpt) (*provider.Router, *testEnv, provider.RoutedSTTSession) {
	t.Helper()
	rec.cleanup(t)
	r, env := newTestRouter(t, append([]testOpt{withSTT(rec)}, opts...)...)
	s, err := r.OpenSTT(context.Background(), sttCfg())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return r, env, s
}

// rotateAfterAudio sends 4000 ms, delivers a FINAL ending at 1500 ms and makes the vendor
// close the connection for its time limit; it returns once the new connection exists.
func rotateAfterAudio(t *testing.T, rec *recSTT, s provider.RoutedSTTSession, code string) {
	t.Helper()
	sendMs(t, s, 4000)
	rec.conn(0).push(final(0, 1500, "あ", false))
	require.Equal(t, provider.STTFinal, nextEv(t, s).Type)
	rec.conn(0).push(vendorErr(code))
	require.Eventually(t, func() bool { return s.Stats().Reconnects == 1 }, 2*time.Second, time.Millisecond)
}

func TestRotationSameVendorReplaysFromLastFinal(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	rotateAfterAudio(t, rec, s, "session_time_limit_exceeded")
	require.Equal(t, provider.FakePrimary, s.Provider())
	require.EqualValues(t, 2, rec.opens.Load(), "a new connection to the same vendor")
	require.Eventually(t, func() bool { return rec.conn(1).audioLen() == 2500*bytesPerMs }, 2*time.Second, time.Millisecond,
		"the replay is the audio after the last FINAL: 4000 − 1500 ms")
}

func TestRotationReplayTruncatedToRing(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 20000) // 20 s without a FINAL
	rec.conn(0).push(vendorErr("session_time_limit_exceeded"))
	require.Eventually(t, func() bool { return s.Stats().Reconnects == 1 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return rec.conn(1).audioLen() == provider.RotationBufferBytesForTest }, 3*time.Second, time.Millisecond,
		"only the last 15 s (480000 bytes) can be replayed")
}

func TestRotationEventOffsets(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	rotateAfterAudio(t, rec, s, "session_time_limit_exceeded")
	rec.conn(1).push(final(100, 900, "い", false))
	ev := nextEv(t, s)
	require.Equal(t, provider.STTFinal, ev.Type)
	require.EqualValues(t, 1600, ev.StartMs, "100 ms of the new connection = 1500 + 100 logical ms")
	require.EqualValues(t, 2400, ev.EndMs)
	require.Equal(t, provider.FakePrimary, ev.Provider)
	rec.conn(1).push(provider.STTEvent{Type: provider.STTPartial, Text: "う", StartMs: 50, EndMs: 60,
		Words: []provider.STTWord{{Text: "う", StartMs: 50, EndMs: 60}}})
	p := nextEv(t, s)
	require.EqualValues(t, 1550, p.StartMs)
	require.EqualValues(t, 1550, p.Words[0].StartMs)
}

func TestRotationOldFinalFromFinalizeDropped(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	rotateAfterAudio(t, rec, s, "session_time_limit_exceeded")
	rec.conn(0).push(final(0, 3000, "古い", true)) // the old connection answers a Finalize late
	noEvent(t, s, 100*time.Millisecond)
	rec.conn(0).push(provider.STTEvent{Type: provider.STTPartial, Text: "古い"})
	noEvent(t, s, 50*time.Millisecond)
	rec.conn(1).push(final(0, 500, "新しい", true))
	require.Equal(t, "新しい", nextEv(t, s).Text)
}

func TestRotationFinalizeDuringReplayDeferred(t *testing.T) {
	rec := &recSTT{configure: func(c *recConn) {
		if c.id == 2 {
			c.gate = make(chan struct{}, 64)
		}
	}}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 1000)
	rec.conn(0).push(vendorErr("session_time_limit_exceeded"))
	require.Eventually(t, func() bool { return rec.connCount() == 2 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return s.Stats().Reconnects == 1 }, 2*time.Second, time.Millisecond)

	require.NoError(t, s.Finalize()) // during the replay: deferred
	require.NoError(t, s.SendAudio(make([]byte, 640)))
	require.Zero(t, rec.conn(1).finalizes(), "Finalize waits for the backlog")
	for i := 0; i < 20; i++ { // let the replay run: 32000 backlog bytes + 640 live bytes
		rec.conn(1).gate <- struct{}{}
	}
	require.Eventually(t, func() bool { return rec.conn(1).finalizes() == 1 }, 2*time.Second, time.Millisecond)
	require.Equal(t, 1000*bytesPerMs+640, rec.conn(1).audioLen(), "backlog first, then the audio that arrived during the replay, then Finalize")
}

func TestRotationReplayChunks3200(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	rotateAfterAudio(t, rec, s, "session_time_limit_exceeded")
	require.Eventually(t, func() bool { return rec.conn(1).audioLen() == 2500*bytesPerMs }, 2*time.Second, time.Millisecond)
	c := rec.conn(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.sends {
		require.Equal(t, 3200, n, "the backlog goes out in 100 ms chunks")
	}
}

func TestRotationTriggers(t *testing.T) {
	for _, code := range []string{"session_time_limit_exceeded", "insufficient_audio_activity"} {
		t.Run(code, func(t *testing.T) {
			rec := &recSTT{}
			r, env, s := openRec(t, rec, withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
			rotateAfterAudio(t, rec, s, code)
			require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State(), "a rotation is not a failure")
			require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_stt_rotations_total", map[string]string{"provider": "FAKE_PRIMARY"}))
			require.Eventually(t, func() bool {
				fams, _ := env.Reg.Gather()
				for _, f := range fams {
					if f.GetName() == "kotoki_stt_replay_seconds" && f.GetMetric()[0].GetHistogram().GetSampleCount() == 1 {
						return true
					}
				}
				return false
			}, 2*time.Second, 5*time.Millisecond)
			noEvent(t, s, 30*time.Millisecond) // nothing reaches the runtime
		})
	}
}

func TestVendorErrorForwardedNoSwitch(t *testing.T) {
	t.Run("vendor ERROR", func(t *testing.T) {
		rec := &recSTT{}
		r, _, s := openRec(t, rec, withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
		rec.conn(0).push(vendorErr("boom"))
		ev := nextEv(t, s)
		require.Equal(t, provider.STTError, ev.Type)
		require.Equal(t, "boom", ev.Err.VendorCode, "forwarded as is")
		require.Equal(t, provider.STTClosed, nextEv(t, s).Type)
		require.EqualValues(t, 1, rec.opens.Load(), "no other connection or vendor is tried")
		require.Equal(t, provider.BreakerOpen, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State())
		require.ErrorIs(t, s.SendAudio(make([]byte, 640)), provider.ErrSessionClosed)
	})
	t.Run("unexpected close", func(t *testing.T) {
		rec := &recSTT{}
		_, _, s := openRec(t, rec)
		rec.conn(0).endEvents()
		ev := nextEv(t, s)
		require.Equal(t, provider.STTError, ev.Type)
		require.Equal(t, provider.KindUnavailable, ev.Err.Kind)
		require.EqualValues(t, 1, rec.opens.Load())
	})
}

func TestRotationOpenFailureForwardsError(t *testing.T) {
	rec := &recSTT{failOpen: func(n int) error {
		if n == 2 {
			return &provider.Error{Provider: provider.FakePrimary, Capability: provider.CapSTT, Kind: provider.KindAuth}
		}
		return nil
	}}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 200)
	rec.conn(0).push(vendorErr("session_time_limit_exceeded"))
	ev := nextEv(t, s)
	require.Equal(t, provider.STTError, ev.Type)
	require.Equal(t, provider.KindAuth, ev.Err.Kind)
	require.EqualValues(t, 2, rec.opens.Load(), "one rotation attempt, no other")
}

func TestOpenSTTFailureReturnsError(t *testing.T) {
	rec := &recSTT{failOpen: func(int) error {
		return &provider.Error{Provider: provider.FakePrimary, Capability: provider.CapSTT, Kind: provider.KindUnavailable}
	}}
	r, _ := newTestRouter(t, withSTT(rec), withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
	_, err := r.OpenSTT(context.Background(), sttCfg())
	var pe *provider.Error
	require.True(t, errors.As(err, &pe))
	require.Equal(t, provider.KindUnavailable, pe.Kind)
	require.EqualValues(t, 1, rec.opens.Load())
	require.Equal(t, provider.BreakerOpen, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State(), "a failed Open is a counted failure")
}

func TestFinalTimeoutThreeConsecutive(t *testing.T) {
	rec := &recSTT{}
	r, _, s := openRec(t, rec, withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
	s.ReportFinalTimeout()
	s.ReportFinalTimeout()
	noEvent(t, s, 30*time.Millisecond)
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State(), "two are not enough")

	rec.conn(0).push(final(0, 100, "", true)) // an on-time FINAL{FromFinalize} resets the count
	require.Equal(t, provider.STTFinal, nextEv(t, s).Type)
	s.ReportFinalTimeout()
	s.ReportFinalTimeout()
	noEvent(t, s, 30*time.Millisecond)

	s.ReportFinalTimeout()
	ev := nextEv(t, s)
	require.Equal(t, provider.STTError, ev.Type)
	require.Equal(t, provider.KindTimeout, ev.Err.Kind)
	require.Equal(t, "final_timeout", ev.Err.VendorCode)
	require.Equal(t, provider.BreakerOpen, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State())
}

func TestRotationRingMemory480000(t *testing.T) {
	require.Equal(t, 480000, provider.RotationBufferBytesForTest)
}

func TestSTTAudioMsSentIncludesReplay(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	rotateAfterAudio(t, rec, s, "session_time_limit_exceeded")
	require.Eventually(t, func() bool { return s.Stats().AudioMsSent == 4000+2500 }, 2*time.Second, time.Millisecond,
		"the vendor bills the replayed audio too")
	require.False(t, s.Stats().OpenedAt.IsZero())
}

func TestSTTConnectTimeout3s(t *testing.T) {
	rec := &recSTT{hangOpen: true}
	r, _ := newTestRouter(t, withSTT(rec), withCfg(func(c *provider.RouterConfig) { c.TimeoutSTTConnect = 40 * time.Millisecond }))
	start := time.Now()
	_, err := r.OpenSTT(context.Background(), sttCfg())
	require.Equal(t, provider.KindTimeout, provider.KindOf(err))
	require.Less(t, time.Since(start), time.Second)
}

func TestSTTSemaphoreTryNoWait(t *testing.T) {
	rec := &recSTT{name: provider.OpenAI}
	rec.cleanup(t)
	r, _ := newTestRouter(t, withSTT(rec), withCfg(func(c *provider.RouterConfig) { c.Concurrency.OpenAISTT = 1 }))
	first, err := r.OpenSTT(context.Background(), sttCfg())
	require.NoError(t, err)

	start := time.Now()
	_, err = r.OpenSTT(context.Background(), sttCfg())
	var pe *provider.Error
	require.True(t, errors.As(err, &pe))
	require.Equal(t, "local_concurrency", pe.VendorCode)
	require.Less(t, time.Since(start), 50*time.Millisecond, "no waiting for an STT slot")
	require.EqualValues(t, 1, rec.opens.Load())

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool {
		s, err := r.OpenSTT(context.Background(), sttCfg())
		if err == nil {
			_ = s.Close()
		}
		return err == nil
	}, time.Second, 5*time.Millisecond, "closing the session frees its slot")
}

func TestSTTCircuitOpenFailsFast(t *testing.T) {
	rec := &recSTT{}
	r, env := newTestRouter(t, withSTT(rec))
	env.openBreaker(t, r, provider.FakePrimary, provider.CapSTT)
	_, err := r.OpenSTT(context.Background(), sttCfg())
	var pe *provider.Error
	require.True(t, errors.As(err, &pe))
	require.Equal(t, "circuit_open", pe.VendorCode)
	require.Zero(t, rec.opens.Load())
}

func TestSTTCloseEmitsClosedOnce(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Equal(t, provider.STTClosed, nextEv(t, s).Type)
	_, ok := <-s.Events()
	require.False(t, ok, "the channel closes after CLOSED")
}

func drainTypes(t *testing.T, s provider.STTSession) []provider.STTEventType {
	t.Helper()
	var out []provider.STTEventType
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev.Type)
		case <-timeout:
			t.Fatal("events channel did not close")
		}
	}
}

func TestVendorErrorCountedAndForwardedOnce(t *testing.T) { // review C1
	rec := &recSTT{configure: closingVendor}
	r, _, s := openRec(t, rec, withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 2 }))
	rec.conn(0).push(vendorErr("boom"))
	rec.conn(0).endEvents() // the vendor ends the channel right after the ERROR
	require.Equal(t, []provider.STTEventType{provider.STTError, provider.STTClosed}, drainTypes(t, s),
		"one ERROR then CLOSED: the stream end that follows is expected, not a second failure")
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State(), "one failure, counted once")
}

func TestNormalCloseNeverCountsAFailure(t *testing.T) { // review C1
	for i := 0; i < 100; i++ {
		rec := &recSTT{configure: closingVendor}
		r, _ := newTestRouter(t, withSTT(rec), withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
		s, err := r.OpenSTT(context.Background(), sttCfg())
		require.NoError(t, err)
		require.NoError(t, s.Close())
		require.Equal(t, []provider.STTEventType{provider.STTClosed}, drainTypes(t, s), "iteration %d", i)
		require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State(), "iteration %d", i)
	}
}

func TestRotationTriggerNeverRefusesAudio(t *testing.T) { // review I1
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 200)
	rec.conn(0).push(vendorErr("session_time_limit_exceeded"))
	for i := 0; i < 200; i++ {
		require.NoError(t, s.SendAudio(make([]byte, 640)), "the session is alive: audio goes to the buffer, never ErrSessionClosed")
	}
	require.Eventually(t, func() bool { return s.Stats().Reconnects == 1 }, 2*time.Second, time.Millisecond)
}

func TestRotationRepeatsUnansweredFinalize(t *testing.T) { // review I2
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 500)
	require.NoError(t, s.Finalize())
	require.Equal(t, 1, rec.conn(0).finalizes())
	rec.conn(0).push(vendorErr("insufficient_audio_activity")) // rotation before the FINAL came
	require.Eventually(t, func() bool { return rec.connCount() == 2 && rec.conn(1).finalizes() == 1 }, 2*time.Second, time.Millisecond,
		"the runtime cannot see a rotation, so the router repeats the Finalize on the new connection")

	rec.conn(1).push(final(0, 500, "あ", true))
	require.Equal(t, provider.STTFinal, nextEv(t, s).Type)
	rec.conn(1).push(vendorErr("session_time_limit_exceeded")) // answered Finalize: nothing to repeat
	require.Eventually(t, func() bool { return rec.connCount() == 3 }, 2*time.Second, time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	require.Zero(t, rec.conn(2).finalizes())
}

func TestRotationDropsLateFinalsOfOldConnection(t *testing.T) { // review I3
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	rotateAfterAudio(t, rec, s, "session_time_limit_exceeded")
	rec.conn(0).push(final(1500, 3000, "二重", false)) // would be transcribed again by the replay
	noEvent(t, s, 100*time.Millisecond)
}

func TestRotationTriggerDuringReplayRotatesAgain(t *testing.T) { // review I4
	rec := &recSTT{configure: func(c *recConn) {
		if c.id == 2 {
			c.gate = make(chan struct{}, 256)
		}
	}}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 1000)
	rec.conn(0).push(vendorErr("session_time_limit_exceeded"))
	require.Eventually(t, func() bool { return rec.connCount() == 2 }, 2*time.Second, time.Millisecond)
	rec.conn(1).push(vendorErr("session_time_limit_exceeded")) // the new connection is cut during its replay
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 64; i++ {
		rec.conn(1).gate <- struct{}{}
	}
	require.Eventually(t, func() bool { return rec.connCount() == 3 && s.Stats().Reconnects == 2 }, 3*time.Second, time.Millisecond)
	noEvent(t, s, 50*time.Millisecond) // no STTError reached the runtime
	require.Eventually(t, func() bool { return rec.conn(2).audioLen() == 1000*bytesPerMs }, 3*time.Second, time.Millisecond)
}

func TestRotationReplayStartClampedToWrittenAudio(t *testing.T) {
	rec := &recSTT{}
	_, _, s := openRec(t, rec)
	sendMs(t, s, 1000)
	rec.conn(0).push(final(0, 5000, "あ", false)) // a FINAL that claims more audio than was ever sent
	require.Equal(t, provider.STTFinal, nextEv(t, s).Type)
	rec.conn(0).push(vendorErr("session_time_limit_exceeded"))
	require.Eventually(t, func() bool { return s.Stats().Reconnects == 1 }, 2*time.Second, time.Millisecond)
	require.NoError(t, s.SendAudio(make([]byte, 640)))
	require.Eventually(t, func() bool { return rec.conn(1).audioLen() == 640 }, 2*time.Second, time.Millisecond, "live audio is not skipped")
}

func TestFinalTimeoutAfterCloseIsIgnored(t *testing.T) {
	rec := &recSTT{}
	r, _, s := openRec(t, rec, withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
	require.NoError(t, s.Close())
	for i := 0; i < 3; i++ {
		s.ReportFinalTimeout()
	}
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapSTT).State())
}
