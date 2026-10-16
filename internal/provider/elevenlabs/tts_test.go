package elevenlabs_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newTTS(t *testing.T, srv *replay.Server, mod func(*elevenlabs.Config)) *elevenlabs.TTS {
	t.Helper()
	cfg := elevenlabs.Config{APIKey: "xi-test-key", BaseURL: srv.URL}
	if mod != nil {
		mod(&cfg)
	}
	tts, err := elevenlabs.NewTTS(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return tts
}

func ttsReq() provider.TTSRequest {
	return provider.TTSRequest{Text: "こんにちは。", Language: "ja", PreviousText: "はい。", Model: "m_flash", VoiceID: "VOICE1", SpeakingRate: 1.0}
}

func synth(t *testing.T, tts *elevenlabs.TTS, req provider.TTSRequest) []provider.TTSChunk {
	t.Helper()
	st, err := tts.Synthesize(ctx, req)
	require.NoError(t, err)
	return drainTTS(t, st.Chunks())
}

func TestELTTSRequest(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
	tts := newTTS(t, srv, nil)
	require.Equal(t, provider.ElevenLabs, tts.Name())
	chunks := synth(t, tts, ttsReq())
	r := srv.Requests()[0]
	require.Equal(t, "/v1/text-to-speech/VOICE1/stream", r.Path)
	require.Equal(t, "pcm_24000", r.Query.Get("output_format"))
	require.Empty(t, r.Query.Get("optimize_streaming_latency"))
	require.Equal(t, "xi-test-key", r.Header.Get("xi-api-key"))
	require.JSONEq(t, `{"text":"こんにちは。","model_id":"m_flash","language_code":"ja","voice_settings":{"speed":1.0},"previous_text":"はい。","apply_text_normalization":"auto"}`, string(r.Body))
	last := chunks[len(chunks)-1]
	require.True(t, last.Final)
	require.Equal(t, 6, last.Usage.Characters)
	require.Equal(t, int64(totalBytes(chunks)/48), last.Usage.AudioMs)
	require.Equal(t, int64(125), last.Usage.AudioMs)
	require.Equal(t, 6000, totalBytes(chunks))
	require.Equal(t, provider.ElevenLabs, last.Provider)
	require.Equal(t, "m_flash", last.Model)
	srv.AssertDone()
}

func TestELTTSPreviousTextOmittedWhenEmpty(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
	req := ttsReq()
	req.PreviousText = ""
	synth(t, newTTS(t, srv, nil), req)
	_, ok := bodyField(t, srv.Requests()[0].Body, "previous_text")
	require.False(t, ok)
}

func TestELTTSMultilingualV2OmitsLanguageCode(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
	req := ttsReq()
	req.Model = "x_multilingual_v2"
	synth(t, newTTS(t, srv, nil), req)
	_, ok := bodyField(t, srv.Requests()[0].Body, "language_code")
	require.False(t, ok)
	v, _ := bodyField(t, srv.Requests()[0].Body, "model_id")
	require.Equal(t, `"x_multilingual_v2"`, v)
}

func TestELTTSOnlySpeedInVoiceSettings(t *testing.T) {
	for rate, want := range map[float64]string{0: `{"speed":1}`, 0.80: `{"speed":0.8}`, 1.05: `{"speed":1.05}`, 0.5: `{"speed":0.7}`, 2.0: `{"speed":1.2}`, 0.7: `{"speed":0.7}`, 1.2: `{"speed":1.2}`} {
		srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
		req := ttsReq()
		req.SpeakingRate = rate
		synth(t, newTTS(t, srv, nil), req)
		v, ok := bodyField(t, srv.Requests()[0].Body, "voice_settings")
		require.True(t, ok)
		require.JSONEq(t, want, v, "rate %v", rate)
	}
}

func TestELTTSEmptyModelNotGuessed(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"TTS","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/text-to-speech/VOICE1/stream"},"respond":{"status":400,"chunks":[{"at_ms":0,"text":"{\"detail\":{\"status\":\"invalid_model_id\",\"message\":\"bad model\"}}"}]}}}`))
	req := ttsReq()
	req.Model = ""
	_, err := newTTS(t, srv, nil).Synthesize(ctx, req)
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(err))
	v, ok := bodyField(t, srv.Requests()[0].Body, "model_id")
	require.True(t, ok)
	require.Equal(t, `""`, v, "model comes from characters.model_name, never defaulted")
}

func TestELTTSVoiceAndModelFromRequestNotConfig(t *testing.T) {
	for _, c := range []struct{ voice, model string }{{"VOICE1", "m_a"}, {"VOICE1", "m_b"}} {
		srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
		req := ttsReq()
		req.Model = c.model
		synth(t, newTTS(t, srv, nil), req)
		v, _ := bodyField(t, srv.Requests()[0].Body, "model_id")
		require.Equal(t, `"`+c.model+`"`, v)
	}
	// A different character: different voice id in the path, same adapter instance.
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"TTS","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/text-to-speech/OTHERVOICE/stream"},"respond":{"status":200,"chunks":[{"at_ms":0,"b64":"AAA="}]}}}`))
	tts := newTTS(t, srv, nil)
	req := ttsReq()
	req.VoiceID = "OTHERVOICE"
	synth(t, tts, req)
	require.Equal(t, "/v1/text-to-speech/OTHERVOICE/stream", srv.Requests()[0].Path)
}

func TestELTTSOddByteBoundary(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_odd_bytes"))
	chunks := synth(t, newTTS(t, srv, nil), ttsReq())
	for i, c := range chunks {
		require.Zero(t, len(c.PCM)%2, "chunk %d", i)
	}
	require.Equal(t, 2000, totalBytes(chunks))
	require.Equal(t, int64(2000/48), chunks[len(chunks)-1].Usage.AudioMs)
}

func TestELTTSDanglingOddByteIsDropped(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"TTS","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/text-to-speech/VOICE1/stream"},"respond":{"status":200,"chunks":[{"at_ms":0,"b64":"AAEC"}]}}}`))
	chunks := synth(t, newTTS(t, srv, nil), ttsReq())
	require.Equal(t, 2, totalBytes(chunks), "3 bytes: the unpaired last byte cannot be a sample")
	require.True(t, chunks[len(chunks)-1].Final)
}

func TestELTTSErrors(t *testing.T) {
	tests := []struct {
		name      string
		script    func(t *testing.T) *replay.Script
		kind      provider.ErrorKind
		status    int
		code      string
		retryable bool
	}{
		{"voice_not_found", func(t *testing.T) *replay.Script { return recording(t, "tts_voice_not_found_404") }, provider.KindInvalidRequest, 404, "voice_not_found", false},
		{"quota", func(t *testing.T) *replay.Script { return recording(t, "tts_quota_exceeded_429") }, provider.KindRateLimited, 429, "quota_exceeded", false},
		{"rate_limited_plain", func(t *testing.T) *replay.Script {
			return errScript(t, 429, `{"detail":{"status":"too_many_concurrent_requests","message":"m"}}`)
		}, provider.KindRateLimited, 429, "too_many_concurrent_requests", true},
		{"auth", func(t *testing.T) *replay.Script {
			return errScript(t, 401, `{"detail":{"status":"invalid_api_key","message":"m"}}`)
		}, provider.KindAuth, 401, "invalid_api_key", false},
		{"unavailable", func(t *testing.T) *replay.Script {
			return errScript(t, 500, `{"detail":{"status":"internal","message":"m"}}`)
		}, provider.KindUnavailable, 500, "internal", true},
		{"validation_list", func(t *testing.T) *replay.Script {
			return errScript(t, 422, `{"detail":[{"loc":["body"],"msg":"x","type":"y"}]}`)
		}, provider.KindInvalidRequest, 422, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := replay.NewHTTPServer(t, tc.script(t))
			_, err := newTTS(t, srv, nil).Synthesize(ctx, ttsReq())
			var pe *provider.Error
			require.ErrorAs(t, err, &pe)
			require.Equal(t, tc.kind, pe.Kind)
			require.Equal(t, tc.status, pe.HTTPStatus)
			require.Equal(t, tc.code, pe.VendorCode)
			require.Equal(t, tc.retryable, pe.Retryable)
			require.Equal(t, provider.ElevenLabs, pe.Provider)
			require.Equal(t, provider.CapTTS, pe.Capability)
		})
	}
}

func errScript(t *testing.T, status int, body string) *replay.Script {
	t.Helper()
	return replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"TTS","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/text-to-speech/VOICE1/stream"},"respond":{"status":`+itoa(status)+`,"chunks":[{"at_ms":0,"text":`+quote(body)+`}]}}}`)
}

func TestELTTSMidStreamError(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_mid_error"))
	chunks := synth(t, newTTS(t, srv, nil), ttsReq())
	require.GreaterOrEqual(t, len(chunks), 2)
	last := chunks[len(chunks)-1]
	require.NotNil(t, last.Err)
	require.Equal(t, provider.KindUnavailable, last.Err.Kind)
	require.True(t, last.Err.Retryable)
	require.False(t, last.Final)
	for _, c := range chunks[:len(chunks)-1] {
		require.Nil(t, c.Err)
		require.NotEmpty(t, c.PCM)
	}
}

func TestELTTSCancelWithin50ms(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_hang"))
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := newTTS(t, srv, nil).Synthesize(cctx, ttsReq())
	require.NoError(t, err)
	select {
	case c := <-st.Chunks():
		require.NotEmpty(t, c.PCM)
	case <-time.After(5 * time.Second):
		t.Fatal("no first chunk")
	}
	start := time.Now()
	cancel()
	n := 0
	for range st.Chunks() {
		n++
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Zero(t, n, "nothing is delivered after cancel")
	require.NoError(t, st.Close())
	require.NoError(t, st.Close())
}

func TestELTTSCloseCancels(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_hang"))
	st, err := newTTS(t, srv, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, st.Close())
	for c := range st.Chunks() {
		require.False(t, c.Final)
		require.Nil(t, c.Err)
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
}

func TestELTTSSyncCancelledContext(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := newTTS(t, srv, nil).Synthesize(cctx, ttsReq())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
}

func TestELTTSSpan(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "tts_basic"))
	synth(t, newTTS(t, srv, nil), ttsReq())
	attrs := map[string]string{}
	require.Eventually(t, func() bool {
		sp := tr.Find("provider.TTS")
		for _, a := range sp.Attributes {
			attrs[string(a.Key)] = a.Value.String()
		}
		return attrs["provider.outcome"] != ""
	}, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, "ELEVENLABS", attrs["provider.name"])
	require.Equal(t, "m_flash", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestELTTSSpanOnError(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "tts_quota_exceeded_429"))
	_, err := newTTS(t, srv, nil).Synthesize(ctx, ttsReq())
	require.Error(t, err)
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.TTS").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "RATE_LIMITED", attrs["provider.error_kind"])
}

func TestELTTSAPIKeyNotInLogsOrErrors(t *testing.T) {
	srv := replay.NewHTTPServer(t, errScript(t, 400, `{"detail":{"status":"invalid_request","message":"rejected k-secret-key-123"}}`))
	lb := &logBuf{}
	tts, err := elevenlabs.NewTTS(elevenlabs.Config{APIKey: "k-secret-key-123", BaseURL: srv.URL}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	req := ttsReq()
	req.Text = "ひみつの文。"
	req.RequestID = "s1:3:0"
	_, err = tts.Synthesize(ctx, req)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "k-secret-key-123")
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.NotContains(t, pe.Message, "k-secret-key-123")
	out := lb.String()
	require.NotContains(t, out, "k-secret-key-123")
	require.NotContains(t, out, "ひみつの文")
	for _, want := range []string{"provider=ELEVENLABS", "capability=TTS", "model=m_flash", "request_id=s1:3:0", "kind=INVALID_REQUEST", "http_status=400", "vendor_code=invalid_request"} {
		require.Contains(t, out, want)
	}
}

func TestELConfigStringHidesKey(t *testing.T) {
	c := elevenlabs.Config{APIKey: "k-secret-value", BaseURL: "http://x"}
	require.NotContains(t, c.String(), "k-secret-value")
	require.NotContains(t, c.GoString(), "k-secret-value")
}

func TestELTTSConfigErrors(t *testing.T) {
	_, err := elevenlabs.NewTTS(elevenlabs.Config{BaseURL: "http://x"}, nil, nil)
	require.Error(t, err)
	_, err = elevenlabs.NewTTS(elevenlabs.Config{APIKey: "k"}, nil, nil)
	require.Error(t, err)
}

func TestELTTSRateLimitGauge(t *testing.T) {
	sink := &rlSink{}
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"ELEVENLABS","capability":"TTS","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/text-to-speech/VOICE1/stream"},"respond":{"status":200,"headers":{"x-ratelimit-remaining-requests":"49","x-ratelimit-remaining-tokens":"900"},"chunks":[{"at_ms":0,"b64":"AAA="}]}}}`))
	synth(t, newTTS(t, srv, func(c *elevenlabs.Config) { c.Metrics = sink }), ttsReq())
	require.ElementsMatch(t, []rlCall{
		{provider.ElevenLabs, provider.CapTTS, "requests", 49},
		{provider.ElevenLabs, provider.CapTTS, "tokens", 900},
	}, sink.calls)
}

func TestELTTSMetricsImplementedByProviderMetrics(_ *testing.T) {
	var _ elevenlabs.RateLimitSink = provider.NewMetrics(prometheus.NewRegistry())
}

func TestELTTSConformance(t *testing.T) {
	providertest.TTSSuite(t, func(t *testing.T) provider.TTSProvider {
		return newTTS(t, replay.NewHTTPServer(t, recording(t, "tts_conformance")), nil)
	}, ttsReq)
}
