package openai_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

const evalSchema = `{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`

func newEval(t *testing.T, srv *replay.Server, mod func(*openai.Config)) *openai.Evaluation {
	t.Helper()
	cfg := openai.Config{APIKey: "sk-test", BaseURL: srv.URL}
	if mod != nil {
		mod(&cfg)
	}
	ev, err := openai.NewEvaluation(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return ev
}

func evalReq() provider.StructuredRequest {
	return provider.StructuredRequest{Task: "EVALUATION", Model: "m", SchemaName: "kotoki_evaluation_rubric_v1",
		JSONSchema: json.RawMessage(evalSchema), Messages: msgs(), MaxOutputTokens: 800, Temperature: 0}
}

func TestOpenAIEvalRequestAndResult(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	ev := newEval(t, srv, nil)
	require.Equal(t, provider.OpenAI, ev.Name())
	res, err := ev.GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.JSONEq(t, `{"s":77}`, string(res.JSON))
	body := srv.Requests()[0].Body
	require.JSONEq(t, `{"type":"json_schema","json_schema":{"name":"kotoki_evaluation_rubric_v1","strict":true,"schema":`+evalSchema+`}}`,
		bodyField(t, body, "response_format"))
	require.JSONEq(t, `{"model":"m","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],
	  "max_completion_tokens":800,"temperature":0,"response_format":`+bodyField(t, body, "response_format")+`}`, string(body))
	require.Equal(t, provider.FinishStop, res.FinishReason)
	require.Equal(t, provider.OpenAI, res.Provider)
	require.Equal(t, "m", res.Model)
	require.Equal(t, provider.TokenUsage{InputTokens: 210, OutputTokens: 6}, res.Usage)
	require.Positive(t, res.Latency)
	srv.AssertDone()
}

func TestOpenAIEvalTemperatureZeroSentNegativeDefaults02(t *testing.T) {
	for temp, want := range map[float64]string{0: "0", -1: "0.2", 0.5: "0.5"} {
		srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
		req := evalReq()
		req.Temperature = temp
		_, err := newEval(t, srv, nil).GenerateJSON(ctx, req)
		require.NoError(t, err)
		require.Equal(t, want, bodyField(t, srv.Requests()[0].Body, "temperature"), "temperature %v", temp)
	}
}

func TestOpenAIEvalSeedSent(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	seed := int64(42)
	req := evalReq()
	req.Seed = &seed
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "42", bodyField(t, srv.Requests()[0].Body, "seed"))

	srv2 := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	_, err = newEval(t, srv2, nil).GenerateJSON(ctx, evalReq())
	require.NoError(t, err)
	require.False(t, bodyHas(t, srv2.Requests()[0].Body, "seed"))
}

func asProviderErr(t *testing.T, err error) *provider.Error {
	t.Helper()
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	return pe
}

func TestOpenAIEvalRefusalIsContentFiltered(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_refusal"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindContentFiltered, pe.Kind)
	require.False(t, pe.Retryable)
	require.NotContains(t, pe.Message, "cannot assist", "the refusal text is model output, not logged in errors")
}

func TestOpenAIEvalLengthIsInvalidResponse(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_length"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.True(t, pe.Retryable)
}

func TestOpenAIEvalInvalidJSON(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_invalid_json"))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.True(t, pe.Retryable)
}

func TestOpenAIEvalEmptyContentAndNoChoices(t *testing.T) {
	for name, body := range map[string]string{
		"empty":   `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
		"null":    `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`,
		"nochoic": `{"choices":[]}`,
		"notjson": `<html>`,
	} {
		srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":`+quote(body)+`}]}}}`))
		_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
		pe := asProviderErr(t, err)
		require.Equal(t, provider.KindInvalidResponse, pe.Kind, name)
		require.True(t, pe.Retryable, name)
	}
}

func TestOpenAIEvalContentFilterFinish(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"{\"choices\":[{\"message\":{\"content\":\"\"},\"finish_reason\":\"content_filter\"}]}"}]}}}`))
	_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
	require.Equal(t, provider.KindContentFiltered, asProviderErr(t, err).Kind)
}

func TestOpenAIEvalHTTPErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   provider.ErrorKind
		retry  bool
	}{
		{401, `{"error":{"message":"bad key sk-test","code":"invalid_api_key"}}`, provider.KindAuth, false},
		{429, `{"error":{"message":"quota","code":"insufficient_quota"}}`, provider.KindRateLimited, false},
		{400, `{"error":{"message":"Invalid schema","code":"invalid_json_schema"}}`, provider.KindInvalidRequest, false},
		{500, `{"error":{"message":"oops","type":"server_error"}}`, provider.KindUnavailable, true},
	}
	for _, c := range cases {
		srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":`+itoa(c.status)+`,"chunks":[{"at_ms":0,"text":`+quote(c.body)+`}]}}}`))
		_, err := newEval(t, srv, nil).GenerateJSON(ctx, evalReq())
		pe := asProviderErr(t, err)
		require.Equal(t, c.kind, pe.Kind, c.status)
		require.Equal(t, c.retry, pe.Retryable, c.status)
		require.Equal(t, provider.CapEvaluation, pe.Capability)
		require.NotContains(t, pe.Message, "sk-test")
	}
}

func TestOpenAIEvalRejectsBadRequestWithoutCalling(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "eval_basic"))
	ev := newEval(t, srv, nil)
	bad := evalReq()
	bad.SchemaName = "NOT A VALID NAME!"
	_, err := ev.GenerateJSON(ctx, bad)
	require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind)
	bad = evalReq()
	bad.JSONSchema = json.RawMessage(`{nope`)
	_, err = ev.GenerateJSON(ctx, bad)
	require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind)
	bad = evalReq()
	bad.Messages = nil
	_, err = ev.GenerateJSON(ctx, bad)
	require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind)
	require.Empty(t, srv.Requests())
}

func TestOpenAIEvalCancel(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"EVALUATION","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/chat/completions"},"respond":{"status":200,"chunks":[{"at_ms":60000,"text":"{}"}]}}}`))
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := newEval(t, srv, nil).GenerateJSON(cctx, evalReq())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestOpenAIEvalSpanAndRateLimit(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewHTTPServer(t, recording(t, "eval_refusal"))
	sink := &rlSink{}
	_, err := newEval(t, srv, func(c *openai.Config) { c.Metrics = sink }).GenerateJSON(ctx, evalReq())
	require.Error(t, err)
	attrs := map[string]string{}
	for _, a := range tr.Find("provider.EVALUATION").Attributes {
		attrs[string(a.Key)] = a.Value.String()
	}
	require.Equal(t, "OPENAI", attrs["provider.name"])
	require.Equal(t, "m", attrs["provider.model"])
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "CONTENT_FILTERED", attrs["provider.error_kind"])
	require.Equal(t, []rlCall{{provider.OpenAI, provider.CapEvaluation, "requests", 99}}, sink.calls)
}
