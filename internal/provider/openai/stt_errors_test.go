package openai_test

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
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func errEvent(typ, code, msg string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": typ, "code": code, "message": msg}}
}

// expectErrorThenClosed returns the ERROR and checks CLOSED follows and the channel closes.
func expectErrorThenClosed(t *testing.T, sess provider.STTSession) *provider.Error {
	t.Helper()
	ev := nextEv2(t, sess)
	require.Equal(t, provider.STTError, ev.Type)
	require.NotNil(t, ev.Err)
	require.Equal(t, provider.STTClosed, nextEv2(t, sess).Type)
	select {
	case _, ok := <-sess.Events():
		require.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("events not closed")
	}
	return ev.Err
}

func nextEv2(t *testing.T, sess provider.STTSession) provider.STTEvent { return nextEv(t, sess) }

func TestOpenAISTTErrorMapping(t *testing.T) {
	cases := []struct {
		name, typ, code string
		kind            provider.ErrorKind
		retryable       bool
		vendor          string
	}{
		{"auth", "invalid_request_error", "invalid_api_key", provider.KindAuth, false, "invalid_api_key"},
		{"rate limit", "rate_limit_error", "rate_limit_exceeded", provider.KindRateLimited, true, "rate_limit_exceeded"},
		{"insufficient quota", "insufficient_quota", "insufficient_quota", provider.KindRateLimited, false, "insufficient_quota"},
		{"bad parameter", "invalid_request_error", "invalid_value", provider.KindInvalidRequest, false, "invalid_value"},
		{"bad event", "invalid_request_error", "unknown_parameter", provider.KindInvalidRequest, false, "unknown_parameter"},
		{"server", "server_error", "internal_error", provider.KindUnavailable, true, "internal_error"},
		{"type only", "server_error", "", provider.KindUnavailable, true, "server_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := replay.NewWSServer(t, sttScript(t,
				onRule(matchAppend, chunk(0, errEvent(c.typ, c.code, "vendor says no"))), updatedLine))
			sess := openSession(t, srv, nil)
			sendPCM(t, sess, 2)
			e := expectErrorThenClosed(t, sess)
			require.Equal(t, c.kind, e.Kind)
			require.Equal(t, c.retryable, e.Retryable)
			require.Equal(t, c.vendor, e.VendorCode)
			require.Equal(t, provider.OpenAI, e.Provider)
			require.Equal(t, provider.CapSTT, e.Capability)
		})
	}
}

func TestOpenAISTTAuthErrorFixtureAndQuotaFixture(t *testing.T) {
	for name, want := range map[string]provider.ErrorKind{"stt_error_auth": provider.KindAuth, "stt_error_quota": provider.KindRateLimited} {
		srv := replay.NewWSServer(t, recording(t, name))
		sess := openSession(t, srv, nil)
		sendPCM(t, sess, 2)
		require.Equal(t, want, expectErrorThenClosed(t, sess).Kind, name)
	}
}

func TestOpenAISTTSessionLimitIsRotationSignal(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_session_limit"))
	sess := openSession(t, srv, nil)
	e := expectErrorThenClosed(t, sess)
	require.Equal(t, provider.KindUnavailable, e.Kind)
	require.True(t, e.Retryable)
	require.Equal(t, "session_expired", e.VendorCode) // exact vendor code, not AUTH/RATE_LIMITED
}

func TestOpenAISTTSocketDropIsUnavailable(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_drop"))
	sess := openSession(t, srv, nil)
	e := expectErrorThenClosed(t, sess)
	require.Equal(t, provider.KindUnavailable, e.Kind)
	require.Equal(t, "ws_close_1011", e.VendorCode)
}

func TestOpenAISTTSendAudioBufferFull(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	err := sess.SendAudio(make([]byte, 64002)) // more than 2 s in one call
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
	require.NoError(t, sess.SendAudio(make([]byte, 64000)))
}

func TestOpenAISTTSendAfterClose(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	require.NoError(t, sess.Close())
	require.True(t, errors.Is(sess.SendAudio(make([]byte, 640)), provider.ErrSessionClosed))
	require.True(t, errors.Is(sess.Finalize(), provider.ErrSessionClosed))
}

func TestOpenAISTTCloseIdempotentWithin500ms(t *testing.T) {
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

func TestOpenAISTTEventsDropOldestPartialNeverFinal(t *testing.T) {
	var chunks []string
	chunks = append(chunks, chunk(0, evCommitted("item_1")))
	for i := 0; i < 300; i++ {
		chunks = append(chunks, chunk(0, evDelta("item_1", "あ")))
	}
	chunks = append(chunks, chunk(0, evCompleted("item_1", "終")))
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend), onRule(matchCommit, chunks...), updatedLine))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	time.Sleep(400 * time.Millisecond) // everything arrived, nobody read
	partials := 0
	var last provider.STTEvent
	for {
		ev := nextEv(t, sess)
		if ev.Type == provider.STTPartial {
			partials++
			continue
		}
		last = ev
		break
	}
	require.Equal(t, provider.STTFinal, last.Type, "FINAL must survive")
	require.Equal(t, "終", last.Text)
	require.True(t, last.FromFinalize)
	require.Less(t, partials, 300, "old PARTIALs are dropped when the consumer is slow")
}

func TestOpenAISTTCancelWithin50ms(t *testing.T) {
	t.Run("during Open", func(t *testing.T) {
		srv := replay.NewWSServer(t, sttScript(t)) // never confirms
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

func TestOpenAISTTNoGoroutineLeakAfterClose(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	base := runtime.NumGoroutine()
	sess, err := newSTT(t, srv, nil).Open(ctx, sttCfg())
	require.NoError(t, err)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Close())
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= base+2 }, 2*time.Second, 20*time.Millisecond)
}

func TestOpenAISTTKeyNotLeaked(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key sk-test-secret","code":"invalid_api_key"}}`))
	}))
	defer hs.Close()
	lb := &logBuf{}
	stt, err := openai.NewSTT(openai.Config{APIKey: "sk-test-secret", WSBaseURL: "ws" + hs.URL[4:], STTModel: "m_stt"}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	_, err = stt.Open(ctx, sttCfg())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sk-test-secret")
	require.NotContains(t, lb.String(), "sk-test-secret")
	for _, want := range []string{"provider=OPENAI", "capability=STT", "model=m_stt", "kind=AUTH", "http_status=401", "vendor_code=invalid_api_key"} {
		require.Contains(t, lb.String(), want)
	}
	// in-session error events: the key is scrubbed from the message as well
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend, chunk(0, errEvent("invalid_request_error", "invalid_api_key", "bad key sk-test-secret"))), updatedLine))
	lb2 := &logBuf{}
	stt2, err := openai.NewSTT(openai.Config{APIKey: "sk-test-secret", WSBaseURL: srv.URL, STTModel: "m_stt"}, httpx.NewClient(), bufLogger(lb2))
	require.NoError(t, err)
	sess, err := stt2.Open(ctx, sttCfg())
	require.NoError(t, err)
	sendPCM(t, sess, 2)
	e := expectErrorThenClosed(t, sess)
	require.False(t, strings.Contains(e.Error(), "sk-test-secret"))
	require.NotContains(t, lb2.String(), "sk-test-secret")
}

func TestOpenAISTTSpan(t *testing.T) {
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
	require.Equal(t, "OPENAI", attrs["provider.name"])
	require.Equal(t, "m_stt", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestOpenAISTTSpanOnError(t *testing.T) {
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
