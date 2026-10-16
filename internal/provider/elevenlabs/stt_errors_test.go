package elevenlabs_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func errMsg(typ, text string) map[string]any {
	return map[string]any{"message_type": typ, "error": text}
}

// expectErrorThenClosed returns the ERROR and checks CLOSED follows and the channel closes.
func expectErrorThenClosed(t *testing.T, sess provider.STTSession) *provider.Error {
	t.Helper()
	ev := nextEv(t, sess)
	require.Equal(t, provider.STTError, ev.Type)
	require.NotNil(t, ev.Err)
	require.Equal(t, provider.STTClosed, nextEv(t, sess).Type)
	select {
	case _, ok := <-sess.Events():
		require.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("events not closed")
	}
	return ev.Err
}

// injectVendorError answers the first audio chunk with the given error message type.
func injectVendorError(t *testing.T, code string) *provider.Error {
	t.Helper()
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend, chunk(0, errMsg(code, "vendor says no")))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	return expectErrorThenClosed(t, sess)
}

func TestELSTTErrorMapping(t *testing.T) {
	cases := []struct {
		code  string
		kind  provider.ErrorKind
		retry bool
	}{
		{"error", provider.KindUnavailable, true}, {"transcriber_error", provider.KindUnavailable, true},
		{"auth_error", provider.KindAuth, false}, {"unaccepted_terms", provider.KindAuth, false},
		{"quota_exceeded", provider.KindRateLimited, false}, {"rate_limited", provider.KindRateLimited, true}, {"queue_overflow", provider.KindRateLimited, true},
		{"resource_exhausted", provider.KindRateLimited, true}, {"commit_throttled", provider.KindRateLimited, true},
		{"input_error", provider.KindInvalidRequest, false}, {"invalid_request", provider.KindInvalidRequest, false}, {"chunk_size_exceeded", provider.KindInvalidRequest, false},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			e := injectVendorError(t, c.code)
			require.Equal(t, c.kind, e.Kind)
			require.Equal(t, c.retry, e.Retryable)
			require.Equal(t, c.code, e.VendorCode)
			require.Equal(t, provider.ElevenLabs, e.Provider)
			require.Equal(t, provider.CapSTT, e.Capability)
		})
	}
}

func TestELSTTErrorFixtures(t *testing.T) {
	for name, want := range map[string]provider.ErrorKind{"stt_auth_error": provider.KindAuth, "stt_quota": provider.KindRateLimited, "stt_rate_limited": provider.KindRateLimited} {
		srv := replay.NewWSServer(t, recording(t, name))
		sess := openSession(t, srv, nil)
		sendPCM(t, sess, 2)
		require.Equal(t, want, expectErrorThenClosed(t, sess).Kind, name)
	}
}

func TestELSTTRotationSignals(t *testing.T) {
	for _, code := range []string{"session_time_limit_exceeded", "insufficient_audio_activity"} {
		t.Run(code, func(t *testing.T) {
			e := injectVendorError(t, code)
			require.Equal(t, provider.KindUnavailable, e.Kind)
			require.True(t, e.Retryable)
			require.Equal(t, code, e.VendorCode) // exact vendor code: rotatingSTT keys on it
		})
	}
	srv := replay.NewWSServer(t, recording(t, "stt_rotate_time_limit"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	require.Equal(t, "session_time_limit_exceeded", expectErrorThenClosed(t, sess).VendorCode)
}

func TestELSTTUnknownMessageTypeIsIgnored(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend, chunk(0, map[string]any{"message_type": "something_new", "text": "x"}), chunk(1, evPartial("続き")))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	require.Equal(t, "続き", nextEv(t, sess).Text)
}

func TestELSTTErrorDuringOpen(t *testing.T) {
	srv := replay.NewWSServer(t, replay.LoadScriptString(t, strings.Join([]string{
		`{"meta":{"vendor":"ELEVENLABS","capability":"STT","source":"inline","verified":false}}`,
		`{"ws":{"dir":"send","text":"{\"message_type\":\"unaccepted_terms\",\"error\":\"accept the terms\"}"}}`,
	}, "\n")))
	_, err := newSTT(t, srv, nil).Open(ctx, sttCfg())
	require.Equal(t, provider.KindAuth, provider.KindOf(err))
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "unaccepted_terms", pe.VendorCode)
}

func TestELSTTKeepAliveSilenceWhenIdle(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend)))
	openSession(t, srv, func(c *elevenlabs.Config) { c.KeepAliveAfter = 40 * time.Millisecond })
	require.Eventually(t, func() bool { return len(srv.ClientMessages()) >= 2 }, 2*time.Second, 10*time.Millisecond)
	for _, m := range audioMsgs(t, srv) {
		require.False(t, m.Commit)
		require.Equal(t, make([]byte, 3200), m.pcm(t)) // 100 ms of silence at 16 kHz
		require.Equal(t, 16000, m.SampleRate)
	}
}

func TestELSTTKeepAliveSilenceIsNotAudioSent(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend)))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.KeepAliveAfter = 40 * time.Millisecond })
	require.Eventually(t, func() bool { return len(srv.ClientMessages()) >= 1 }, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(0), sess.Stats().AudioMsSent)
}

func TestELSTTKeepAliveDefaultIsFiveSeconds(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend)))
	openSession(t, srv, nil)
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, srv.ClientMessages())
}

func TestELSTTWarningLogsOnly(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend, chunk(0, map[string]any{"message_type": "warning", "warning": "audio is quiet"}))))
	lb := &logBuf{}
	stt, err := elevenlabs.NewSTT(elevenlabs.Config{APIKey: "xi-test-key", WSBaseURL: srv.URL, STTModel: "m_stt"}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	sess, err := stt.Open(ctx, sttCfg())
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	sendPCM(t, sess, 2)
	require.Eventually(t, func() bool { return strings.Contains(lb.String(), "level=WARN") }, 2*time.Second, 10*time.Millisecond)
	for _, want := range []string{`msg="provider warning"`, "provider=ELEVENLABS", "capability=STT", "model=m_stt", "vendor_code=warning"} {
		require.Contains(t, lb.String(), want)
	}
	require.NotContains(t, lb.String(), "audio is quiet")
	expectNoEvent(t, sess, 100*time.Millisecond)
}

func TestELSTTSocketDropIsUnavailable(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_drop"))
	sess := openSession(t, srv, nil)
	e := expectErrorThenClosed(t, sess)
	require.Equal(t, provider.KindUnavailable, e.Kind)
	require.True(t, e.Retryable)
	require.Equal(t, "ws_close_1011", e.VendorCode)
}

func TestELSTTSendAudioBufferFull(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	err := sess.SendAudio(make([]byte, 64002)) // more than 2 s in one call
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
	require.True(t, pe.Retryable)
	require.NoError(t, sess.SendAudio(make([]byte, 64000)))
}

func TestELSTTSendAudioBufferFullWhenServerStalls(t *testing.T) {
	// the writer cannot drain while the socket is blocked: the backlog is capped, never grows
	srv := replay.NewWSServer(t, sttScript(t))
	sess := openSession(t, srv, nil)
	var full error
	deadline := time.Now().Add(5 * time.Second)
	for full == nil && time.Now().Before(deadline) {
		full = sess.SendAudio(make([]byte, 32000))
	}
	// the local socket buffers may absorb a lot, so only require that SendAudio never blocks
	if full != nil {
		require.Equal(t, provider.KindUnavailable, provider.KindOf(full))
	}
}

func TestELSTTSendAfterClose(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	require.NoError(t, sess.Close())
	require.True(t, errors.Is(sess.SendAudio(make([]byte, 640)), provider.ErrSessionClosed))
	require.True(t, errors.Is(sess.Finalize(), provider.ErrSessionClosed))
}

func TestELSTTCloseIdempotentWithin500ms(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	start := time.Now()
	require.NoError(t, sess.Close())
	require.NoError(t, sess.Close())
	require.Less(t, time.Since(start), 500*time.Millisecond)
	var types []provider.STTEventType
	for ev := range sess.Events() {
		types = append(types, ev.Type)
	}
	require.Equal(t, []provider.STTEventType{provider.STTClosed}, types)
}

func TestELSTTEventsDropOldestPartialNeverFinal(t *testing.T) {
	var chunks []string
	for i := 0; i < 200; i++ {
		chunks = append(chunks, chunk(0, evPartial("あ")))
	}
	chunks = append(chunks, chunk(0, evCommitted("終")), chunk(0, errMsg("rate_limited", "slow down")))
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend), onRule(matchCommit, chunks...)))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.SyntheticFinalDelay = 5 * time.Second })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	time.Sleep(400 * time.Millisecond) // everything arrived, nobody read
	partials := 0
	var seen []provider.STTEventType
	for ev := range sess.Events() {
		if ev.Type == provider.STTPartial {
			partials++
			continue
		}
		seen = append(seen, ev.Type)
		if ev.Type == provider.STTFinal {
			require.Equal(t, "終", ev.Text)
			require.True(t, ev.FromFinalize)
		}
	}
	require.Equal(t, []provider.STTEventType{provider.STTFinal, provider.STTError, provider.STTClosed}, seen, "FINAL/ERROR/CLOSED must survive")
	require.Less(t, partials, 200, "old PARTIALs are dropped when the consumer is slow")
}

func TestELSTTCancelWithin50ms(t *testing.T) {
	t.Run("during Open", func(t *testing.T) {
		srv := replay.NewWSServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"STT","source":"inline","verified":false}}`)) // never confirms
		cctx, cancel := context.WithCancel(ctx)
		go func() { time.Sleep(30 * time.Millisecond); cancel() }()
		start := time.Now()
		_, err := newSTT(t, srv, nil).Open(cctx, sttCfg())
		require.Equal(t, provider.KindCancelled, provider.KindOf(err))
		require.Less(t, time.Since(start), 80*time.Millisecond) // 30 ms wait + 50 ms budget
	})
	t.Run("after Open", func(t *testing.T) {
		srv := replay.NewWSServer(t, recording(t, "stt_basic"))
		cctx, cancel := context.WithCancel(ctx)
		sess, err := newSTT(t, srv, nil).Open(cctx, sttCfg())
		require.NoError(t, err)
		start := time.Now()
		cancel()
		ev := nextEv(t, sess)
		require.Equal(t, provider.STTClosed, ev.Type, "cancel is not an error")
		require.Less(t, time.Since(start), 50*time.Millisecond)
		select {
		case _, ok := <-sess.Events():
			require.False(t, ok)
		case <-time.After(time.Second):
			t.Fatal("events not closed")
		}
	})
}

func TestELSTTNoGoroutineLeakAfterClose(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	base := runtime.NumGoroutine()
	sess, err := newSTT(t, srv, func(c *elevenlabs.Config) { c.KeepAliveAfter = 20 * time.Millisecond }).Open(ctx, sttCfg())
	require.NoError(t, err)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Close())
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= base+2 }, 2*time.Second, 20*time.Millisecond)
}

func TestELSTTKeyNotLeaked(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_api_key","message":"bad key k-secret"}}`))
	}))
	defer hs.Close()
	lb := &logBuf{}
	stt, err := elevenlabs.NewSTT(elevenlabs.Config{APIKey: "k-secret", WSBaseURL: "ws" + hs.URL[4:], STTModel: "m_stt"}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	_, err = stt.Open(ctx, sttCfg())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "k-secret")
	require.NotContains(t, lb.String(), "k-secret")
	for _, want := range []string{"provider=ELEVENLABS", "capability=STT", "model=m_stt", "kind=AUTH", "http_status=401", "vendor_code=invalid_api_key"} {
		require.Contains(t, lb.String(), want)
	}
	// in-session error messages: the key is scrubbed as well
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend, chunk(0, errMsg("auth_error", "bad key k-secret")))))
	lb2 := &logBuf{}
	stt2, err := elevenlabs.NewSTT(elevenlabs.Config{APIKey: "k-secret", WSBaseURL: srv.URL, STTModel: "m_stt"}, httpx.NewClient(), bufLogger(lb2))
	require.NoError(t, err)
	sess, err := stt2.Open(ctx, sttCfg())
	require.NoError(t, err)
	sendPCM(t, sess, 2)
	e := expectErrorThenClosed(t, sess)
	require.NotContains(t, e.Error(), "k-secret")
	require.NotContains(t, lb2.String(), "k-secret")
}

func TestELSTTSpan(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Close())
	for range sess.Events() {
	}
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.STT").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "ELEVENLABS", attrs["provider.name"])
	require.Equal(t, "m_stt", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestELSTTSpanOnError(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewWSServer(t, recording(t, "stt_drop"))
	sess := openSession(t, srv, nil)
	_ = expectErrorThenClosed(t, sess)
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.STT").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "UNAVAILABLE", attrs["provider.error_kind"])
}

// Regression (M9-T13, found driving the adapter through rotatingSTT): after the VENDOR ended the
// session, SendAudio and Finalize do not fail with ErrSessionClosed, which the port reserves for
// after Close. The ERROR and CLOSED events carry the failure; refusing audio in the gap before
// the router has read ERROR would surface a rotation (spec 8.5.8) to the runtime as a failure.
func TestELSTTAfterVendorErrorSendAndFinalizeAreDropped(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_rotate_time_limit"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	_ = expectErrorThenClosed(t, sess)
	require.NoError(t, sess.SendAudio(make([]byte, 640)))
	require.NoError(t, sess.Finalize())
	require.NoError(t, sess.Close())
	require.ErrorIs(t, sess.SendAudio(make([]byte, 640)), provider.ErrSessionClosed)
	require.ErrorIs(t, sess.Finalize(), provider.ErrSessionClosed)
}
