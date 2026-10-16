package openai_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newLLM(t *testing.T, srv *replay.Server, mod func(*openai.Config)) *openai.LLM {
	t.Helper()
	cfg := openai.Config{APIKey: "sk-test", BaseURL: srv.URL}
	if mod != nil {
		mod(&cfg)
	}
	l, err := openai.NewLLM(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return l
}

func llmReq() provider.LLMRequest {
	return provider.LLMRequest{Model: "m1", MaxTokens: 300, Temperature: 0.8, Stop: []string{"。。"}, Messages: msgs()}
}

func streamAll(t *testing.T, l *openai.LLM, req provider.LLMRequest) []provider.LLMEvent {
	t.Helper()
	st, err := l.Stream(ctx, req)
	require.NoError(t, err)
	return drain(t, st.Events())
}

func TestOpenAILLMStreamBasic(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	l := newLLM(t, srv, nil)
	require.Equal(t, provider.OpenAI, l.Name())
	evs := streamAll(t, l, llmReq())
	require.Equal(t, []string{"こんに", "ちは。"}, deltas(evs))
	done := evs[len(evs)-1]
	require.Equal(t, provider.LLMDone, done.Type)
	require.Equal(t, provider.FinishStop, done.FinishReason)
	require.Equal(t, provider.TokenUsage{InputTokens: 120, OutputTokens: 8}, *done.Usage)
	require.Equal(t, provider.OpenAI, done.Provider)
	require.Equal(t, "m1", done.Model)
	req := srv.Requests()[0]
	require.Equal(t, "Bearer sk-test", req.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"m1","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"stream":true,
	  "stream_options":{"include_usage":true},"max_completion_tokens":300,"temperature":0.8,"stop":["。。"]}`, string(req.Body))
	srv.AssertDone()
}

func TestOpenAILLMDeltaTextIsVerbatim(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\" 今日 は\\n\"},\"finish_reason\":null}]}\n\n"},{"at_ms":1,"text":"data: [DONE]\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, []string{" 今日 は\n"}, deltas(evs))
}

func TestOpenAILLMFinishLength(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_length"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, provider.FinishMaxTokens, evs[len(evs)-1].FinishReason)
}

func TestOpenAILLMContentFilterDone(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_content_filter"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMDone, last.Type, "content_filter is DONE, not ERROR")
	require.Equal(t, provider.FinishContentFiltered, last.FinishReason)
	for _, e := range evs {
		require.NotEqual(t, provider.LLMError, e.Type)
	}
}

func TestOpenAILLMUsageMissingIsEstimated(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_usage_missing"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	u := evs[len(evs)-1].Usage
	require.NotNil(t, u)
	require.True(t, u.Estimated)
	require.Equal(t, 2, u.InputTokens, "total runes of all messages (S + U)")
	require.Equal(t, 3, u.OutputTokens, "runes of the emitted text")
}

func TestOpenAILLMStopOmittedWhenEmpty(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	req := llmReq()
	req.Stop = nil
	streamAll(t, newLLM(t, srv, nil), req)
	require.False(t, bodyHas(t, srv.Requests()[0].Body, "stop"))
	require.True(t, bodyHas(t, srv.Requests()[0].Body, "temperature"))
}

func TestOpenAILLMOmitSampling(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	l := newLLM(t, srv, func(c *openai.Config) { c.OmitSampling = true; c.ReasoningEffort = "minimal" })
	streamAll(t, l, llmReq())
	body := srv.Requests()[0].Body
	require.False(t, bodyHas(t, body, "temperature"))
	require.False(t, bodyHas(t, body, "stop"))
	require.Equal(t, `"minimal"`, bodyField(t, body, "reasoning_effort"))
	require.Equal(t, "300", bodyField(t, body, "max_completion_tokens"))

	srv2 := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	streamAll(t, newLLM(t, srv2, nil), llmReq())
	require.False(t, bodyHas(t, srv2.Requests()[0].Body, "reasoning_effort"), "no effort configured, none sent")
}

func TestOpenAILLMTemperatureZeroIsSent(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	req := llmReq()
	req.Temperature = 0
	streamAll(t, newLLM(t, srv, nil), req)
	require.Equal(t, "0", bodyField(t, srv.Requests()[0].Body, "temperature"))
}

func errScript(t *testing.T, status int, headers, body string) *replay.Script {
	t.Helper()
	h := ""
	if headers != "" {
		h = `,"headers":` + headers
	}
	return replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":`+itoa(status)+h+`,"chunks":[{"at_ms":0,"text":`+quote(body)+`}]}}}`)
}

func TestOpenAILLMErrors(t *testing.T) {
	tests := []struct {
		name      string
		script    func(t *testing.T) *replay.Script
		kind      provider.ErrorKind
		retryable bool
		status    int
		code      string
		retry     time.Duration
	}{
		{"401", func(t *testing.T) *replay.Script {
			return errScript(t, 401, "", `{"error":{"message":"Incorrect API key provided: sk-test.","type":"invalid_request_error","code":"invalid_api_key"}}`)
		}, provider.KindAuth, false, 401, "invalid_api_key", 0},
		{"429 quota", func(t *testing.T) *replay.Script { return recording(t, "llm_429_quota") },
			provider.KindRateLimited, false, 429, "insufficient_quota", 0},
		{"429 rate", func(t *testing.T) *replay.Script {
			return errScript(t, 429, `{"Retry-After":"3"}`, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`)
		}, provider.KindRateLimited, true, 429, "rate_limit_exceeded", 3 * time.Second},
		{"400 policy", func(t *testing.T) *replay.Script { return recording(t, "llm_400_content_policy") },
			provider.KindContentFiltered, false, 400, "content_policy_violation", 0},
		{"400 content_filter", func(t *testing.T) *replay.Script {
			return errScript(t, 400, "", `{"error":{"message":"filtered","type":"invalid_request_error","code":"content_filter"}}`)
		}, provider.KindContentFiltered, false, 400, "content_filter", 0},
		{"400 other", func(t *testing.T) *replay.Script {
			return errScript(t, 400, "", `{"error":{"message":"Unsupported parameter: temperature","type":"invalid_request_error","code":"unsupported_value"}}`)
		}, provider.KindInvalidRequest, false, 400, "unsupported_value", 0},
		{"503", func(t *testing.T) *replay.Script {
			return errScript(t, 503, "", `{"error":{"message":"The server is overloaded","type":"server_error","code":null}}`)
		}, provider.KindUnavailable, true, 503, "server_error", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := replay.NewHTTPServer(t, tc.script(t))
			st, err := newLLM(t, srv, nil).Stream(ctx, llmReq())
			require.Error(t, err, "errors are returned synchronously, not as events")
			require.Nil(t, st)
			var pe *provider.Error
			require.ErrorAs(t, err, &pe)
			require.Equal(t, tc.kind, pe.Kind)
			require.Equal(t, tc.retryable, pe.Retryable)
			require.Equal(t, tc.status, pe.HTTPStatus)
			require.Equal(t, tc.code, pe.VendorCode)
			require.Equal(t, tc.retry, pe.RetryAfter)
			require.Equal(t, provider.OpenAI, pe.Provider)
			require.Equal(t, provider.CapLLM, pe.Capability)
			require.NotContains(t, pe.Message, "sk-test")
			require.NotContains(t, err.Error(), "sk-test")
		})
	}
}

func TestOpenAILLMTransportErrorIsUnavailable(t *testing.T) {
	l, err := openai.NewLLM(openai.Config{APIKey: "sk-test", BaseURL: "http://127.0.0.1:1"}, httpx.NewClient(), nil)
	require.NoError(t, err)
	_, err = l.Stream(ctx, llmReq())
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
}

func TestOpenAINewRequiresKeyAndBaseURL(t *testing.T) {
	_, err := openai.NewLLM(openai.Config{BaseURL: "http://x"}, nil, nil)
	require.Error(t, err)
	_, err = openai.NewLLM(openai.Config{APIKey: "k"}, nil, nil)
	require.Error(t, err)
}

func TestOpenAIConfigStringHidesKey(t *testing.T) {
	c := openai.Config{APIKey: "sk-secret-value", BaseURL: "http://x"}
	for _, s := range []string{c.String(), c.GoString()} {
		require.NotContains(t, s, "sk-secret-value")
	}
	require.NotContains(t, strings.TrimSpace(fmtV(c)), "sk-secret-value")
}

func TestOpenAILLMCancelWithin50ms(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_hang"))
	l := newLLM(t, srv, nil)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := l.Stream(cctx, llmReq())
	require.NoError(t, err)
	select {
	case ev := <-st.Events():
		require.Equal(t, provider.LLMDelta, ev.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("no first delta")
	}
	start := time.Now()
	cancel()
	var last provider.LLMEvent
	n := 0
	for ev := range st.Events() {
		last = ev
		n++
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Equal(t, 1, n, "only the CANCELLED DONE follows the cancel")
	require.Equal(t, provider.LLMDone, last.Type)
	require.Equal(t, provider.FinishCancelled, last.FinishReason)
	require.True(t, last.Usage.Estimated)
	require.NoError(t, st.Close())
	require.NoError(t, st.Close(), "Close is idempotent")
}

func TestOpenAILLMCloseCancels(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_hang"))
	st, err := newLLM(t, srv, nil).Stream(ctx, llmReq())
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, st.Close())
	evs := drain(t, st.Events())
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Equal(t, provider.FinishCancelled, evs[len(evs)-1].FinishReason)
}

func TestOpenAILLMStreamBadJSONIsInvalidResponse(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"},{"at_ms":1,"text":"data: {not json\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMError, last.Type)
	require.Equal(t, provider.KindInvalidResponse, last.Err.Kind)
	require.Equal(t, []string{"a"}, deltas(evs))
}

func TestOpenAILLMStreamCutIsUnavailable(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMError, last.Type)
	require.Equal(t, provider.KindUnavailable, last.Err.Kind)
	require.True(t, last.Err.Retryable)
}

func TestOpenAILLMRateLimitGauge(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	sink := &rlSink{}
	streamAll(t, newLLM(t, srv, func(c *openai.Config) { c.Metrics = sink }), llmReq())
	require.ElementsMatch(t, []rlCall{
		{provider.OpenAI, provider.CapLLM, "requests", 499},
		{provider.OpenAI, provider.CapLLM, "tokens", 149000},
	}, sink.calls)
}

func TestOpenAILLMRateLimitGaugeRealMetrics(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	reg := prometheus.NewRegistry()
	m := provider.NewMetrics(reg)
	streamAll(t, newLLM(t, srv, func(c *openai.Config) { c.Metrics = m }), llmReq())
	fams, err := reg.Gather()
	require.NoError(t, err)
	got := map[string]float64{}
	for _, f := range fams {
		if f.GetName() != "kotoki_provider_ratelimit_remaining" {
			continue
		}
		for _, mm := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range mm.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			require.Equal(t, "OPENAI", labels["provider"])
			require.Equal(t, "LLM", labels["capability"])
			got[labels["kind"]] = mm.GetGauge().GetValue()
		}
	}
	require.Equal(t, map[string]float64{"requests": 499, "tokens": 149000}, got)
}

func TestOpenAILLMSpan(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	streamAll(t, newLLM(t, srv, nil), llmReq())
	sp := tr.Find("provider.LLM")
	attrs := map[string]string{}
	for _, a := range sp.Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "OPENAI", attrs["provider.name"])
	require.Equal(t, "m1", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestOpenAILLMSpanOnError(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "llm_429_quota"))
	_, err := newLLM(t, srv, nil).Stream(ctx, llmReq())
	require.Error(t, err)
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.LLM").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "RATE_LIMITED", attrs["provider.error_kind"])
}

func TestOpenAILLMLogsNoSecretOrText(t *testing.T) {
	srv := replay.NewHTTPServer(t, errScript(t, 400, "", `{"error":{"message":"rejected","type":"invalid_request_error","code":"content_policy_violation"}}`))
	lb := &logBuf{}
	l, err := openai.NewLLM(openai.Config{APIKey: "sk-test-secret", BaseURL: srv.URL}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	req := llmReq()
	req.Messages = []provider.Message{{Role: provider.RoleUser, Content: "ひみつの発言"}}
	req.RequestID = "s1:3"
	_, err = l.Stream(ctx, req)
	require.Error(t, err)
	out := lb.String()
	require.NotContains(t, out, "sk-test-secret")
	require.NotContains(t, out, "ひみつの発言")
	for _, want := range []string{"provider=OPENAI", "capability=LLM", "model=m1", "request_id=s1:3", "kind=CONTENT_FILTERED", "http_status=400", "vendor_code=content_policy_violation"} {
		require.Contains(t, out, want)
	}
}

func TestOpenAILLMSyncCancelledContext(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := newLLM(t, srv, nil).Stream(cctx, llmReq())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
}
