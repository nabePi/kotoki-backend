package deepseek_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/deepseek"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

const evalSchema = `{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`

func newEval(t *testing.T, srv *replay.Server, mod func(*deepseek.Config)) *deepseek.Evaluation {
	t.Helper()
	cfg := deepseek.Config{APIKey: "dk-test", BaseURL: srv.URL}
	if mod != nil {
		mod(&cfg)
	}
	ev, err := deepseek.NewEvaluation(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return ev
}

func evalReq() provider.StructuredRequest {
	return provider.StructuredRequest{Task: "EVALUATION", Model: "m", SchemaName: "kotoki_evaluation_rubric_v1",
		JSONSchema: json.RawMessage(evalSchema), Messages: msgs(), MaxOutputTokens: 800, Temperature: 0}
}

func evalScript(t *testing.T, status int, body string) *replay.Script {
	t.Helper()
	return replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":`+itoa(status)+`,"chunks":[{"at_ms":0,"text":`+quote(body)+`}]}}}`)
}

func asProviderErr(t *testing.T, err error) *provider.Error {
	t.Helper()
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	return pe
}

func TestDeepSeekEvalRequestAndResult(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	ev := newEval(t, srv, nil)
	require.Equal(t, provider.DeepSeek, ev.Name())
	res, err := ev.GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.JSONEq(t, `{"s":77}`, string(res.JSON))
	require.Equal(t, provider.DeepSeek, res.Provider)
	require.Equal(t, "m", res.Model)
	require.Equal(t, provider.FinishStop, res.FinishReason)
	require.Equal(t, provider.TokenUsage{InputTokens: 210, OutputTokens: 6, CachedInputTokens: 200}, res.Usage)
	require.Positive(t, res.Latency)
	r := srv.Requests()[0]
	require.Equal(t, "/chat/completions", r.Path)
	require.Equal(t, "Bearer dk-test", r.Header.Get("Authorization"))
	require.JSONEq(t, `{"type":"json_object"}`, bodyField(t, r.Body, "response_format"))
	require.JSONEq(t, `{"type":"disabled"}`, bodyField(t, r.Body, "thinking"))
	require.Equal(t, "800", bodyField(t, r.Body, "max_tokens"))
	require.Equal(t, "0", bodyField(t, r.Body, "temperature"))
	require.False(t, bodyHas(t, r.Body, "max_completion_tokens"))
	require.False(t, bodyHas(t, r.Body, "stream"))
	require.NotContains(t, string(r.Body), "json_schema")
	require.NotContains(t, string(r.Body), "strict")
	srv.AssertDone()
}

func TestDeepSeekEvalInjectsSchemaInstruction(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	var body struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(srv.Requests()[0].Body, &body))
	require.Len(t, body.Messages, 3)
	require.Equal(t, "S", body.Messages[0].Content)
	require.Equal(t, "system", body.Messages[1].Role, "instruction is a system message at the end of the system block")
	require.Contains(t, body.Messages[1].Content, "JSON")
	require.Contains(t, body.Messages[1].Content, `{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`)
	require.Contains(t, body.Messages[1].Content, `{"s":0}`)
	require.Equal(t, "user", body.Messages[2].Role)
	require.Equal(t, "U", body.Messages[2].Content)
}

func TestDeepSeekEvalEmptyContentIsInvalidResponse(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_empty_content"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.True(t, pe.Retryable)
	require.Equal(t, provider.CapEvaluation, pe.Capability)
	require.Contains(t, pe.Message, "empty")
}

func TestDeepSeekEvalNullNoChoicesAndNotJSONBodies(t *testing.T) {
	for name, body := range map[string]string{
		"null":    `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`,
		"nochoic": `{"choices":[]}`,
		"notjson": `<html>`,
		"nomsg":   `{"choices":[{"finish_reason":"stop"}]}`,
	} {
		srv := replay.NewHTTPServer(t, evalScript(t, 200, body))
		_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
		pe := asProviderErr(t, err)
		require.Equal(t, provider.KindInvalidResponse, pe.Kind, name)
		require.True(t, pe.Retryable, name)
	}
}

func TestDeepSeekEvalInvalidJSON(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_invalid_json"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.True(t, pe.Retryable)
}

func TestDeepSeekEvalLengthIsInvalidResponse(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_length"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.True(t, pe.Retryable)
}

func TestDeepSeekEvalLengthWithValidJSONIsStillInvalidResponse(t *testing.T) {
	// truncation can happen to land on valid JSON; finish_reason=length is what must decide
	srv := replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{\"s\":7}"},"finish_reason":"length"}]}`))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.True(t, pe.Retryable)
	require.Contains(t, pe.Message, "truncated")
}

func TestDeepSeekEvalContentFilteredNotRetryable(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_content_filter"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindContentFiltered, pe.Kind)
	require.False(t, pe.Retryable)
}

func TestDeepSeekEvalVendorStoppedIsUnavailable(t *testing.T) {
	for _, reason := range []string{"insufficient_system_resource", "aborted"} {
		srv := replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{\"s\":1}"},"finish_reason":"`+reason+`"}]}`))
		_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
		pe := asProviderErr(t, err)
		require.Equal(t, provider.KindUnavailable, pe.Kind, reason)
		require.True(t, pe.Retryable, reason)
		require.Equal(t, reason, pe.VendorCode)
	}
}

func TestDeepSeekEvalDoesNotValidateSchema(t *testing.T) {
	// valid JSON that violates the schema (wrong type, extra property) is returned untouched:
	// schema validation belongs to the caller (M7/M8) together with the repair path.
	srv := replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{\"s\":\"not-an-int\",\"extra\":true}"},"finish_reason":"stop"}]}`))
	res, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.JSONEq(t, `{"s":"not-an-int","extra":true}`, string(res.JSON))
}

func TestDeepSeekEvalThinkingConfig(t *testing.T) {
	for cfgVal, want := range map[string]string{"": `{"type":"disabled"}`, "disabled": `{"type":"disabled"}`, "enabled": `{"type":"enabled"}`} {
		srv := replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
		_, err := newEval(t, srv, func(c *deepseek.Config) { c.EvaluationThinking = cfgVal }).GenerateJSON(ctx, evalReq())
		require.NoError(t, err)
		require.JSONEq(t, want, bodyField(t, srv.Requests()[0].Body, "thinking"), "EvaluationThinking=%q", cfgVal)
	}
	// the LLM thinking setting does not leak into Evaluation
	srv := replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	_, err := newEval(t, srv, func(c *deepseek.Config) { c.Thinking = "enabled" }).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"disabled"}`, bodyField(t, srv.Requests()[0].Body, "thinking"))

	_, err = deepseek.NewEvaluation(deepseek.Config{APIKey: "k", BaseURL: "http://x", EvaluationThinking: "max"}, nil, nil)
	require.Error(t, err)
}

func TestDeepSeekEvalTemperatureZeroSentNegativeDefaults02(t *testing.T) {
	for temp, want := range map[float64]string{0: "0", -1: "0.2", 0.5: "0.5"} {
		srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
		req := evalReq()
		req.Temperature = temp
		_, err := newEval(t, srv, nil).GenerateJSON(ctx, req)
		require.NoError(t, err)
		require.Equal(t, want, bodyField(t, srv.Requests()[0].Body, "temperature"), "temperature %v", temp)
	}
}

func TestDeepSeekEvalSeedOnlyIfSupported(t *testing.T) {
	// Step 0: the DeepSeek API reference documents no seed parameter, so it is never sent.
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	seed := int64(42)
	req := evalReq()
	req.Seed = &seed
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, req)
	require.NoError(t, err)
	require.False(t, bodyHas(t, srv.Requests()[0].Body, "seed"))
}

func TestDeepSeekEvalMaxTokensOmittedWhenUnset(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	req := evalReq()
	req.MaxOutputTokens = 0
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, req)
	require.NoError(t, err)
	require.False(t, bodyHas(t, srv.Requests()[0].Body, "max_tokens"))
}

func TestDeepSeekEvalUsage(t *testing.T) {
	srv := replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":9,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":20}}`))
	res, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.Equal(t, provider.TokenUsage{InputTokens: 50, OutputTokens: 9, CachedInputTokens: 30}, res.Usage)

	srv = replay.NewHTTPServer(t, evalScript(t, 200, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	res, err = newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.Equal(t, provider.TokenUsage{}, res.Usage, "no usage object, zero usage")
}

func TestDeepSeekEvalHTTPErrors(t *testing.T) {
	tests := []struct {
		name      string
		script    func(t *testing.T) *replay.Script
		kind      provider.ErrorKind
		retryable bool
		status    int
	}{
		{"401", func(t *testing.T) *replay.Script { return recording(t, "eval_401") }, provider.KindAuth, false, 401},
		{"402", func(t *testing.T) *replay.Script {
			return evalScript(t, 402, `{"error":{"message":"Insufficient Balance","code":"invalid_request_error"}}`)
		}, provider.KindAuth, false, 402},
		{"429", func(t *testing.T) *replay.Script {
			return evalScript(t, 429, `{"error":{"message":"slow down","code":"rate_limit_reached"}}`)
		}, provider.KindRateLimited, true, 429},
		{"400", func(t *testing.T) *replay.Script {
			return evalScript(t, 400, `{"error":{"message":"bad","code":"invalid_request_error"}}`)
		}, provider.KindInvalidRequest, false, 400},
		{"422", func(t *testing.T) *replay.Script { return evalScript(t, 422, `{"error":{"message":"bad","code":"x"}}`) }, provider.KindInvalidRequest, false, 422},
		{"500", func(t *testing.T) *replay.Script { return evalScript(t, 500, `{"error":{"message":"oops dk-test"}}`) }, provider.KindUnavailable, true, 500},
		{"503", func(t *testing.T) *replay.Script { return evalScript(t, 503, `{"error":{"message":"busy"}}`) }, provider.KindUnavailable, true, 503},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := replay.NewHTTPServer(t, tc.script(t))
			_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
			pe := asProviderErr(t, err)
			require.Equal(t, tc.kind, pe.Kind)
			require.Equal(t, tc.retryable, pe.Retryable)
			require.Equal(t, tc.status, pe.HTTPStatus)
			require.Equal(t, provider.DeepSeek, pe.Provider)
			require.Equal(t, provider.CapEvaluation, pe.Capability)
			require.NotContains(t, pe.Message, "dk-test")
			require.NotContains(t, err.Error(), "dk-test")
		})
	}
}

func TestDeepSeekEvalSingleAttempt(t *testing.T) {
	srv := replay.NewHTTPServer(t, evalScript(t, 503, `{"error":{"message":"busy"}}`))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.Error(t, err)
	require.Len(t, srv.Requests(), 1, "no internal retry")
}

func TestDeepSeekEvalRejectsBadRequestWithoutCalling(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	ev := newEval(t, srv, nil)
	for name, mod := range map[string]func(*provider.StructuredRequest){
		"schema name":    func(r *provider.StructuredRequest) { r.SchemaName = "Bad Name" },
		"schema invalid": func(r *provider.StructuredRequest) { r.JSONSchema = json.RawMessage(`{`) },
		"schema array":   func(r *provider.StructuredRequest) { r.JSONSchema = json.RawMessage(`[1]`) },
		"no messages":    func(r *provider.StructuredRequest) { r.Messages = nil },
	} {
		bad := evalReq()
		mod(&bad)
		_, err := ev.GenerateJSON(ctx, bad)
		require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind, name)
	}
	require.Empty(t, srv.Requests())
}

func TestDeepSeekEvalCancel(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":60000,"text":"{}"}]}}}`))
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := newEval(t, srv, nil).GenerateJSON(cctx, evalReq())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestDeepSeekEvalSpanAndRateLimit(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "eval_content_filter"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.Error(t, err)
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.EVALUATION").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "DEEPSEEK", attrs["provider.name"])
	require.Equal(t, "m", attrs["provider.model"])
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "CONTENT_FILTERED", attrs["provider.error_kind"])

	srv2 := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"DEEPSEEK","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"headers":{"x-ratelimit-remaining-requests":"7"},"chunks":[{"at_ms":0,"text":"{\"choices\":[{\"message\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}"}]}}}`))
	sink := &rlSink{}
	_, err = newEval(t, srv2, func(c *deepseek.Config) { c.Metrics = sink }).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.Equal(t, []rlCall{{provider.DeepSeek, provider.CapEvaluation, "requests", 7}}, sink.calls)
}

func TestDeepSeekEvalLogsNoSecretOrText(t *testing.T) {
	srv := replay.NewHTTPServer(t, evalScript(t, 400, `{"error":{"message":"rejected dk-test-secret","code":"invalid_request_error"}}`))
	lb := &logBuf{}
	ev, err := deepseek.NewEvaluation(deepseek.Config{APIKey: "dk-test-secret", BaseURL: srv.URL}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	req := evalReq()
	req.Messages = []provider.Message{{Role: provider.RoleUser, Content: "ひみつの発言"}}
	req.RequestID = "s1:4"
	_, err = ev.GenerateJSON(ctx, req)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "dk-test-secret")
	out := lb.String()
	require.NotContains(t, out, "dk-test-secret")
	require.NotContains(t, out, "ひみつの発言")
	for _, want := range []string{"provider=DEEPSEEK", "capability=EVALUATION", "model=m", "request_id=s1:4", "kind=INVALID_REQUEST", "http_status=400"} {
		require.Contains(t, out, want)
	}
}
