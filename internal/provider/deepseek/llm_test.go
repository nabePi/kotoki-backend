package deepseek_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/deepseek"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newLLM(t *testing.T, srv *replay.Server, mod func(*deepseek.Config)) *deepseek.LLM {
	t.Helper()
	cfg := deepseek.Config{APIKey: "dk-test", BaseURL: srv.URL}
	if mod != nil {
		mod(&cfg)
	}
	l, err := deepseek.NewLLM(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return l
}

func llmReq() provider.LLMRequest {
	return provider.LLMRequest{Model: "m1", MaxTokens: 300, Temperature: 0.8, Stop: []string{"。。"}, Messages: msgs()}
}

func streamAll(t *testing.T, l *deepseek.LLM, req provider.LLMRequest) []provider.LLMEvent {
	t.Helper()
	st, err := l.Stream(ctx, req)
	require.NoError(t, err)
	return drain(t, st.Events())
}

func TestDeepSeekLLMStreamBasic(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	l := newLLM(t, srv, nil)
	require.Equal(t, provider.DeepSeek, l.Name())
	evs := streamAll(t, l, llmReq())
	require.Equal(t, []string{"こんに", "ちは。"}, deltas(evs))
	done := evs[len(evs)-1]
	require.Equal(t, provider.LLMDone, done.Type)
	require.Equal(t, provider.FinishStop, done.FinishReason)
	require.Equal(t, provider.TokenUsage{InputTokens: 120, OutputTokens: 8, CachedInputTokens: 100}, *done.Usage)
	require.Equal(t, provider.DeepSeek, done.Provider)
	require.Equal(t, "m1", done.Model)
	req := srv.Requests()[0]
	require.Equal(t, "/chat/completions", req.Path)
	require.Equal(t, "Bearer dk-test", req.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"m1","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"stream":true,
	  "stream_options":{"include_usage":true},"max_tokens":300,"temperature":0.8,"stop":["。。"],"thinking":{"type":"disabled"}}`, string(req.Body))
	srv.AssertDone()
}

func TestDeepSeekLLMDeltaTextIsVerbatim(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\" 今日 は\\n\"},\"finish_reason\":null}]}\n\n"},{"at_ms":1,"text":"data: [DONE]\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, []string{" 今日 は\n"}, deltas(evs))
}

func TestDeepSeekLLMUsesMaxTokensNotMaxCompletionTokens(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	streamAll(t, newLLM(t, srv, nil), llmReq())
	body := srv.Requests()[0].Body
	require.Equal(t, "300", bodyField(t, body, "max_tokens"))
	require.False(t, bodyHas(t, body, "max_completion_tokens"))

	srv2 := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	req := llmReq()
	req.MaxTokens = 0
	streamAll(t, newLLM(t, srv2, nil), req)
	require.False(t, bodyHas(t, srv2.Requests()[0].Body, "max_tokens"), "unset limit is left to the vendor default")
}

func TestDeepSeekLLMStopOmittedWhenEmpty(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	req := llmReq()
	req.Stop = nil
	streamAll(t, newLLM(t, srv, nil), req)
	require.False(t, bodyHas(t, srv.Requests()[0].Body, "stop"))
	require.True(t, bodyHas(t, srv.Requests()[0].Body, "temperature"))
}

func TestDeepSeekLLMTemperatureZeroIsSent(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	req := llmReq()
	req.Temperature = 0
	streamAll(t, newLLM(t, srv, nil), req)
	require.Equal(t, "0", bodyField(t, srv.Requests()[0].Body, "temperature"))
}

func TestDeepSeekLLMThinkingConfig(t *testing.T) {
	for cfgVal, want := range map[string]string{"": `{"type":"disabled"}`, "disabled": `{"type":"disabled"}`, "enabled": `{"type":"enabled"}`} {
		srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
		streamAll(t, newLLM(t, srv, func(c *deepseek.Config) { c.Thinking = cfgVal }), llmReq())
		require.JSONEq(t, want, bodyField(t, srv.Requests()[0].Body, "thinking"), "Thinking=%q", cfgVal)
	}
	_, err := deepseek.NewLLM(deepseek.Config{APIKey: "k", BaseURL: "http://x", Thinking: "high"}, nil, nil)
	require.Error(t, err, "unknown thinking mode is a configuration error")
}

func TestDeepSeekLLMUsageOnLastContentChunk(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_usage_last_chunk"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, []string{"はい", "。"}, deltas(evs))
	u := evs[len(evs)-1].Usage
	require.NotNil(t, u)
	require.False(t, u.Estimated)
	require.Equal(t, provider.TokenUsage{InputTokens: 120, OutputTokens: 8, CachedInputTokens: 100}, *u)
}

func TestDeepSeekLLMUsageMissingIsEstimated(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\"はい。\"},\"finish_reason\":null}],\"usage\":null}\n\n"},{"at_ms":1,"text":"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n"},{"at_ms":2,"text":"data: [DONE]\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	u := evs[len(evs)-1].Usage
	require.NotNil(t, u)
	require.True(t, u.Estimated)
	require.Equal(t, 2, u.InputTokens, "total runes of all messages (S + U)")
	require.Equal(t, 3, u.OutputTokens, "runes of the emitted text")
}

func TestDeepSeekLLMIgnoresKeepAliveComment(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_keepalive_comment"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, []string{"やあ"}, deltas(evs))
	require.Len(t, evs, 2, "one DELTA and one DONE; comments produce nothing")
	require.Equal(t, provider.LLMDone, evs[1].Type)
}

func TestDeepSeekLLMIgnoresReasoningContent(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_reasoning_content"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, []string{"答え"}, deltas(evs))
	for _, e := range evs {
		require.NotContains(t, e.Text, "考え")
	}
}

func TestDeepSeekLLMFinishReasons(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_length"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, provider.FinishMaxTokens, evs[len(evs)-1].FinishReason)

	srv = replay.NewHTTPServer(t, recording(t, "llm_content_filter"))
	evs = streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMDone, last.Type, "content_filter is DONE, not ERROR (no vendor fallback, D-017)")
	require.Equal(t, provider.FinishContentFiltered, last.FinishReason)
	for _, e := range evs {
		require.NotEqual(t, provider.LLMError, e.Type)
	}
}

func TestDeepSeekLLMVendorStoppedIsUnavailable(t *testing.T) {
	for _, reason := range []string{"insufficient_system_resource", "aborted"} {
		body := `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\"こん\"},\"finish_reason\":null}]}\n\n"},{"at_ms":1,"text":"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"` + reason + `\"}]}\n\n"},{"at_ms":2,"text":"data: [DONE]\n\n"}]}}}`
		srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, body))
		evs := streamAll(t, newLLM(t, srv, nil), llmReq())
		last := evs[len(evs)-1]
		require.Equal(t, provider.LLMError, last.Type, reason)
		require.Equal(t, provider.KindUnavailable, last.Err.Kind, reason)
		require.True(t, last.Err.Retryable, reason)
		require.Equal(t, reason, last.Err.VendorCode)
		for _, e := range evs {
			require.NotEqual(t, provider.LLMDone, e.Type, "no DONE after a vendor abort")
		}
	}
	srv := replay.NewHTTPServer(t, recording(t, "llm_insufficient_resource"))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	require.Equal(t, provider.KindUnavailable, evs[len(evs)-1].Err.Kind)
}

func TestDeepSeekLLMSystemOnlyRequest(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_system_only"))
	req := llmReq()
	req.Messages = []provider.Message{{Role: provider.RoleSystem, Content: "S"}}
	evs := streamAll(t, newLLM(t, srv, nil), req)
	require.Equal(t, []string{"では始めましょう。"}, deltas(evs))
	// Step 0(g): DeepSeek docs do not say system-only is rejected, so it is sent as is.
	require.JSONEq(t, `[{"role":"system","content":"S"}]`, bodyField(t, srv.Requests()[0].Body, "messages"))
	srv.AssertDone()
}

func errScript(t *testing.T, status int, headers, body string) *replay.Script {
	t.Helper()
	h := ""
	if headers != "" {
		h = `,"headers":` + headers
	}
	return replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":`+itoa(status)+h+`,"chunks":[{"at_ms":0,"text":`+quote(body)+`}]}}}`)
}

func TestDeepSeekLLMErrors(t *testing.T) {
	tests := []struct {
		name      string
		script    func(t *testing.T) *replay.Script
		kind      provider.ErrorKind
		retryable bool
		status    int
		code      string
		retry     time.Duration
	}{
		{"401", func(t *testing.T) *replay.Script { return recording(t, "llm_401") },
			provider.KindAuth, false, 401, "invalid_request_error", 0},
		{"403", func(t *testing.T) *replay.Script {
			return errScript(t, 403, "", `{"error":{"message":"forbidden","type":"x","code":"forbidden"}}`)
		}, provider.KindAuth, false, 403, "forbidden", 0},
		{"402 balance", func(t *testing.T) *replay.Script { return recording(t, "llm_402_balance") },
			provider.KindAuth, false, 402, "invalid_request_error", 0},
		{"429", func(t *testing.T) *replay.Script { return recording(t, "llm_429") },
			provider.KindRateLimited, true, 429, "rate_limit_reached", 2 * time.Second},
		{"429 no retry-after", func(t *testing.T) *replay.Script {
			return errScript(t, 429, "", `{"error":{"message":"Rate Limit Reached","code":"rate_limit_reached"}}`)
		}, provider.KindRateLimited, true, 429, "rate_limit_reached", 0},
		{"400", func(t *testing.T) *replay.Script { return recording(t, "llm_400") },
			provider.KindInvalidRequest, false, 400, "invalid_request_error", 0},
		{"422", func(t *testing.T) *replay.Script {
			return errScript(t, 422, "", `{"error":{"message":"Failed to deserialize","code":"invalid_request_error"}}`)
		}, provider.KindInvalidRequest, false, 422, "invalid_request_error", 0},
		{"400 content_policy", func(t *testing.T) *replay.Script {
			return errScript(t, 400, "", `{"error":{"message":"filtered","code":"content_policy_violation"}}`)
		}, provider.KindContentFiltered, false, 400, "content_policy_violation", 0},
		{"500", func(t *testing.T) *replay.Script {
			return errScript(t, 500, "", `{"error":{"message":"oops","code":null}}`)
		}, provider.KindUnavailable, true, 500, "", 0},
		{"503", func(t *testing.T) *replay.Script { return recording(t, "llm_503") },
			provider.KindUnavailable, true, 503, "server_error", 0},
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
			require.Equal(t, provider.DeepSeek, pe.Provider)
			require.Equal(t, provider.CapLLM, pe.Capability)
			require.NotContains(t, pe.Message, "dk-test")
			require.NotContains(t, pe.VendorCode, "dk-test")
			require.NotContains(t, err.Error(), "dk-test")
		})
	}
}

func TestDeepSeekLLMScrubsKeyFromVendorFields(t *testing.T) {
	srv := replay.NewHTTPServer(t, errScript(t, 401, "", `{"error":{"message":"bad key dk-test","code":"dk-test"}}`))
	_, err := newLLM(t, srv, nil).Stream(ctx, llmReq())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.NotContains(t, pe.Message, "dk-test")
	require.NotContains(t, pe.VendorCode, "dk-test")
	require.Contains(t, pe.Message, "[redacted]")
}

func TestDeepSeekLLMTransportErrorIsUnavailable(t *testing.T) {
	l, err := deepseek.NewLLM(deepseek.Config{APIKey: "dk-test", BaseURL: "http://127.0.0.1:1"}, httpx.NewClient(), nil)
	require.NoError(t, err)
	_, err = l.Stream(ctx, llmReq())
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
}

func TestDeepSeekNewRequiresKeyAndBaseURL(t *testing.T) {
	_, err := deepseek.NewLLM(deepseek.Config{BaseURL: "http://x"}, nil, nil)
	require.Error(t, err)
	_, err = deepseek.NewLLM(deepseek.Config{APIKey: "k"}, nil, nil)
	require.Error(t, err)
}

func TestDeepSeekConfigStringHidesKey(t *testing.T) {
	c := deepseek.Config{APIKey: "dk-secret-value", BaseURL: "http://x"}
	for _, s := range []string{c.String(), c.GoString()} {
		require.NotContains(t, s, "dk-secret-value")
	}
	require.NotContains(t, strings.TrimSpace(fmtV(c)), "dk-secret-value")
}

func TestDeepSeekLLMCancelWithin50ms(t *testing.T) {
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

func TestDeepSeekLLMCloseCancels(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_hang"))
	st, err := newLLM(t, srv, nil).Stream(ctx, llmReq())
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, st.Close())
	evs := drain(t, st.Events())
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Equal(t, provider.FinishCancelled, evs[len(evs)-1].FinishReason)
}

func TestDeepSeekLLMStreamBadJSONIsInvalidResponse(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"},{"at_ms":1,"text":"data: {not json\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMError, last.Type)
	require.Equal(t, provider.KindInvalidResponse, last.Err.Kind)
	require.Equal(t, []string{"a"}, deltas(evs))
}

func TestDeepSeekLLMStreamCutIsUnavailable(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMError, last.Type)
	require.Equal(t, provider.KindUnavailable, last.Err.Kind)
	require.True(t, last.Err.Retryable)
}

func TestDeepSeekLLMMidStreamErrorObject(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"error\":{\"message\":\"boom dk-test\",\"code\":\"server_error\"}}\n\n"}]}}}`))
	evs := streamAll(t, newLLM(t, srv, nil), llmReq())
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMError, last.Type)
	require.Equal(t, provider.KindUnavailable, last.Err.Kind)
	require.True(t, last.Err.Retryable)
	require.Equal(t, "server_error", last.Err.VendorCode)
}

func TestDeepSeekLLMRateLimitGauge(t *testing.T) {
	script := replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"headers":{"x-ratelimit-remaining-requests":"49","x-ratelimit-remaining-tokens":"900"},"chunks":[{"at_ms":0,"text":"data: [DONE]\n\n"}]}}}`)
	srv := replay.NewHTTPServer(t, script)
	sink := &rlSink{}
	streamAll(t, newLLM(t, srv, func(c *deepseek.Config) { c.Metrics = sink }), llmReq())
	require.ElementsMatch(t, []rlCall{
		{provider.DeepSeek, provider.CapLLM, "requests", 49},
		{provider.DeepSeek, provider.CapLLM, "tokens", 900},
	}, sink.calls)

	// Without the (undocumented) headers nothing is published.
	srv2 := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	sink2 := &rlSink{}
	streamAll(t, newLLM(t, srv2, func(c *deepseek.Config) { c.Metrics = sink2 }), llmReq())
	require.Empty(t, sink2.calls)
}

func TestDeepSeekLLMRateLimitGaugeRealMetrics(t *testing.T) {
	script := replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"headers":{"x-ratelimit-remaining-requests":"49"},"chunks":[{"at_ms":0,"text":"data: [DONE]\n\n"}]}}}`)
	srv := replay.NewHTTPServer(t, script)
	reg := prometheus.NewRegistry()
	m := provider.NewMetrics(reg)
	streamAll(t, newLLM(t, srv, func(c *deepseek.Config) { c.Metrics = m }), llmReq())
	fams, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, f := range fams {
		if f.GetName() != "kotoki_provider_ratelimit_remaining" {
			continue
		}
		for _, mm := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range mm.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			require.Equal(t, "DEEPSEEK", labels["provider"])
			require.Equal(t, "LLM", labels["capability"])
			require.Equal(t, "requests", labels["kind"])
			require.InDelta(t, 49, mm.GetGauge().GetValue(), 0)
			found = true
		}
	}
	require.True(t, found)
}

func TestDeepSeekLLMSpan(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	streamAll(t, newLLM(t, srv, nil), llmReq())
	sp := tr.Find("provider.LLM")
	attrs := map[string]string{}
	for _, a := range sp.Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "DEEPSEEK", attrs["provider.name"])
	require.Equal(t, "m1", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestDeepSeekLLMSpanOnError(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "llm_402_balance"))
	_, err := newLLM(t, srv, nil).Stream(ctx, llmReq())
	require.Error(t, err)
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.LLM").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "AUTH", attrs["provider.error_kind"])
}

func TestDeepSeekLLMAPIKeyNotInLogsOrErrors(t *testing.T) {
	srv := replay.NewHTTPServer(t, errScript(t, 400, "", `{"error":{"message":"rejected dk-test-secret","code":"invalid_request_error"}}`))
	lb := &logBuf{}
	l, err := deepseek.NewLLM(deepseek.Config{APIKey: "dk-test-secret", BaseURL: srv.URL}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	req := llmReq()
	req.Messages = []provider.Message{{Role: provider.RoleUser, Content: "ひみつの発言"}}
	req.RequestID = "s1:3"
	_, err = l.Stream(ctx, req)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "dk-test-secret")
	out := lb.String()
	require.NotContains(t, out, "dk-test-secret")
	require.NotContains(t, out, "ひみつの発言")
	for _, want := range []string{"provider=DEEPSEEK", "capability=LLM", "model=m1", "request_id=s1:3", "kind=INVALID_REQUEST", "http_status=400", "vendor_code=invalid_request_error"} {
		require.Contains(t, out, want)
	}
}

func TestDeepSeekLLMSyncCancelledContext(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "llm_basic"))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := newLLM(t, srv, nil).Stream(cctx, llmReq())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
}
