package cartesia_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func errMsg(status int, code string) string {
	m := `{"type":"error","status_code":` + itoa(status) + `,"title":"T","message":"vendor message"`
	if code != "" {
		m += `,"error_code":"` + code + `"`
	}
	return m + `,"request_id":"r"}`
}

// injectErr makes the vendor answer the first audio message with an error message.
func injectErr(t *testing.T, status int, code string) provider.STTEvent {
	t.Helper()
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary, chunk(0, errMsg(status, code)))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	ev := nextEv(t, sess)
	require.Equal(t, provider.STTError, ev.Type)
	closed := nextEv(t, sess)
	require.Equal(t, provider.STTClosed, closed.Type)
	return ev
}

func TestCartesiaSTTErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		code   string
		kind   provider.ErrorKind
		retry  bool
	}{
		{401, "invalid_api_key", provider.KindAuth, false},
		{403, "", provider.KindAuth, false},
		{429, "rate_limit_exceeded", provider.KindRateLimited, true},
		{429, "insufficient_credits", provider.KindRateLimited, false}, // GUESS: quota code hints
		{400, "model_not_found", provider.KindInvalidRequest, false},
		{404, "", provider.KindInvalidRequest, false},
		{422, "bad_param", provider.KindInvalidRequest, false},
		{500, "internal", provider.KindUnavailable, true},
		{503, "", provider.KindUnavailable, true},
		{408, "", provider.KindTimeout, true},
	}
	for _, c := range cases {
		t.Run(itoa(c.status)+"/"+c.code, func(t *testing.T) {
			ev := injectErr(t, c.status, c.code)
			require.Equal(t, c.kind, ev.Err.Kind)
			require.Equal(t, c.retry, ev.Err.Retryable)
			require.Equal(t, c.code, ev.Err.VendorCode)
			require.Equal(t, c.status, ev.Err.HTTPStatus)
			require.Equal(t, provider.Cartesia, ev.Err.Provider)
			require.Equal(t, provider.CapSTT, ev.Err.Capability)
			require.Equal(t, "vendor message", ev.Err.Message)
		})
	}
}

func TestCartesiaSTTErrorFixtures(t *testing.T) {
	for name, want := range map[string]provider.ErrorKind{"stt_error_401": provider.KindAuth, "stt_error_429": provider.KindRateLimited} {
		t.Run(name, func(t *testing.T) {
			srv := replay.NewWSServer(t, recording(t, name))
			sess := openSession(t, srv, nil)
			sendPCM(t, sess, 5)
			ev := nextEv(t, sess)
			require.Equal(t, provider.STTError, ev.Type)
			require.Equal(t, want, ev.Err.Kind)
		})
	}
}

func TestCartesiaSTTSessionLimitIsRotationSignal(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_session_limit"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	ev := nextEv(t, sess)
	require.Equal(t, provider.STTError, ev.Type)
	require.Equal(t, provider.KindUnavailable, ev.Err.Kind) // not TIMEOUT although the status is 408
	require.True(t, ev.Err.Retryable)
	require.Equal(t, "session_time_limit_exceeded", ev.Err.VendorCode)
	require.Equal(t, provider.STTClosed, nextEv(t, sess).Type)
}

func TestCartesiaSTTSocketDropIsUnavailable(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_drop"))
	sess := openSession(t, srv, nil)
	ev := nextEv(t, sess)
	require.Equal(t, provider.STTError, ev.Type)
	require.Equal(t, provider.KindUnavailable, ev.Err.Kind)
	require.True(t, ev.Err.Retryable)
	require.Equal(t, "ws_close_1011", ev.Err.VendorCode)
	require.Equal(t, provider.STTClosed, nextEv(t, sess).Type)
}

func TestCartesiaSTTOpenAuthFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   provider.ErrorKind
		code   string
	}{
		{401, `{"status_code":401,"title":"Unauthorized","message":"bad key ` + testKey + `","error_code":"bad_key"}`, provider.KindAuth, "bad_key"},
		{403, ``, provider.KindAuth, ""},
		{503, `not json`, provider.KindUnavailable, ""},
	} {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		defer hs.Close()
		lb := &logBuf{}
		stt, err := cartesia.NewSTT(cartesia.Config{APIKey: testKey, WSBaseURL: "ws" + hs.URL[len("http"):], STTModel: "m_ink", Version: "v1"}, httpx.NewClient(), bufLogger(lb))
		require.NoError(t, err)
		_, err = stt.Open(ctx, sttCfg())
		var pe *provider.Error
		require.ErrorAs(t, err, &pe)
		require.Equal(t, tc.kind, pe.Kind)
		require.Equal(t, tc.status, pe.HTTPStatus)
		require.Equal(t, tc.code, pe.VendorCode)
		require.NotContains(t, pe.Message, testKey)
		require.NotContains(t, lb.String(), testKey)
		require.Contains(t, lb.String(), "provider call failed")
	}
}

func TestCartesiaSTTKeepAliveWhenIdle(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	sess := openSession(t, srv, func(c *cartesia.Config) { c.KeepAliveAfter = 40 * time.Millisecond })
	msgs := waitMsgs(t, srv, 1)
	require.False(t, msgs[0].Text)
	require.Equal(t, make([]byte, 3200), msgs[0].Data) // 100 ms of silence
	require.EqualValues(t, 0, sess.Stats().AudioMsSent, "silence is not audio sent")
	for _, m := range srv.ClientMessages() {
		require.NotEqual(t, "finalize", string(m.Data))
	}
}

func TestCartesiaSTTNoKeepAliveByDefault(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	sess := openSession(t, srv, nil)
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, srv.ClientMessages())
	_ = sess
}

func TestCartesiaSTTSendAudioBufferFull(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	sess := openSession(t, srv, nil)
	// more than 2 s (64.000 bytes) in one go cannot fit whatever the writer already drained
	err := sess.SendAudio(make([]byte, 64002))
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, pe.Kind)
	require.True(t, pe.Retryable)
	require.NoError(t, sess.SendAudio(make([]byte, 64000)), "exactly 2 s still fits")
	require.EqualValues(t, 2000, sess.Stats().AudioMsSent, "refused audio is not counted")
}

func TestCartesiaSTTSendAfterClose(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Close())
	require.ErrorIs(t, sess.SendAudio(make([]byte, 640)), provider.ErrSessionClosed)
	require.ErrorIs(t, sess.Finalize(), provider.ErrSessionClosed)
}

func TestCartesiaSTTEventsDropOldestPartialNeverFinal(t *testing.T) {
	var sends []string
	for i := 0; i < 200; i++ {
		sends = append(sends, chunk(0, `{"type":"transcript","is_final":false,"text":"p`+itoa(i)+`"}`))
	}
	sends = append(sends, chunk(0, `{"type":"transcript","is_final":true,"text":"最終"}`), chunk(0, errMsg(500, "boom")))
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary, sends...)))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	// consumer stays away until everything arrived
	require.Eventually(t, func() bool { return len(sess.Events()) == 64 }, 3*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	var finals, errs, closed, partials int
	var lastPartial string
	for ev := range sess.Events() {
		switch ev.Type {
		case provider.STTFinal:
			finals++
		case provider.STTError:
			errs++
		case provider.STTClosed:
			closed++
		case provider.STTPartial:
			partials++
			lastPartial = ev.Text
		}
	}
	require.Equal(t, 1, finals)
	require.Equal(t, 1, errs)
	require.Equal(t, 1, closed)
	require.Equal(t, "p199", lastPartial, "the newest partial survives, the oldest were dropped")
	require.Less(t, partials, 200)
}

func TestCartesiaSTTCancelWithin50ms(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	cctx, cancel := context.WithCancel(ctx)
	stt := newSTT(t, srv, nil)
	sess, err := stt.Open(cctx, sttCfg())
	require.NoError(t, err)
	start := time.Now()
	cancel()
	var last provider.STTEvent
	for ev := range sess.Events() {
		last = ev
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Equal(t, provider.STTClosed, last.Type)
}

func TestCartesiaSTTOpenCancelledContext(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := newSTT(t, srv, nil).Open(cctx, sttCfg())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
}

func TestCartesiaSTTKeyNotLeaked(t *testing.T) {
	lb := &logBuf{}
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary,
		chunk(0, `{"type":"error","status_code":401,"title":"T","message":"key `+testKey+` rejected","error_code":"`+testKey+`"}`))))
	stt, err := cartesia.NewSTT(cartesia.Config{APIKey: testKey, WSBaseURL: srv.URL, STTModel: "m_ink", Version: "v1"}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	sess, err := stt.Open(ctx, sttCfg())
	require.NoError(t, err)
	sendPCM(t, sess, 5)
	ev := nextEv(t, sess)
	require.Equal(t, provider.STTError, ev.Type)
	require.NotContains(t, ev.Err.Message, testKey)
	require.NotContains(t, ev.Err.VendorCode, testKey)
	require.NotContains(t, ev.Err.Error(), testKey)
	require.NotContains(t, lb.String(), testKey)
	cfg := cartesia.Config{APIKey: testKey}
	require.NotContains(t, cfg.String(), testKey)
	require.NotContains(t, fmt.Sprintf("%#v %v %+v", cfg, cfg, cfg), testKey)
	tb := &logBuf{}
	slog.New(slog.NewTextHandler(tb, nil)).Info("cfg", "config", cfg)
	require.NotContains(t, tb.String(), testKey)
}

func spanAttrs(tr *testutil.TraceRecorder, name string) map[string]string {
	attrs := map[string]string{}
	for _, a := range tr.Find(name).Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	return attrs
}

func TestCartesiaSTTSpan(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Close())
	for range sess.Events() {
	}
	attrs := spanAttrs(tr, "provider.STT")
	require.Equal(t, "CARTESIA", attrs["provider.name"])
	require.Equal(t, "m_ink", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestCartesiaSTTSpanOnError(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewWSServer(t, recording(t, "stt_drop"))
	sess := openSession(t, srv, nil)
	require.Equal(t, provider.STTError, nextEv(t, sess).Type)
	require.Equal(t, provider.STTClosed, nextEv(t, sess).Type)
	attrs := spanAttrs(tr, "provider.STT")
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "UNAVAILABLE", attrs["provider.error_kind"])
}
