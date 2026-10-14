package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func withStubs(eval provider.EvaluationProvider, embed provider.EmbeddingProvider) testOpt {
	return withDeps(func(d *provider.RouterDeps) {
		if eval != nil {
			d.Evaluation = eval
		}
		if embed != nil {
			d.Embedding = embed
		}
	})
}

func perr(kind provider.ErrorKind, retry bool) *provider.Error {
	return &provider.Error{Provider: provider.FakePrimary, Capability: provider.CapEvaluation, Kind: kind, Retryable: retry}
}

// stubEval answers GenerateJSON from a per-call script (nil = success).
type stubEval struct {
	name   provider.Name
	script []error
	calls  atomic.Int32
	hold   chan struct{}
	last   atomic.Pointer[provider.StructuredRequest]
	usage  provider.TokenUsage
	json   string
}

func (s *stubEval) Name() provider.Name {
	if s.name == "" {
		return provider.FakePrimary
	}
	return s.name
}

func (s *stubEval) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	n := int(s.calls.Add(1))
	s.last.Store(&req)
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return provider.StructuredResult{}, ctx.Err()
		}
	}
	if n-1 < len(s.script) && s.script[n-1] != nil {
		return provider.StructuredResult{}, s.script[n-1]
	}
	body := s.json
	if body == "" {
		body = `{"ok":true}`
	}
	return provider.StructuredResult{JSON: json.RawMessage(body), Usage: s.usage, FinishReason: provider.FinishStop}, nil
}

func evalRequest() provider.StructuredRequest {
	return provider.StructuredRequest{Task: "EVALUATION", SchemaName: "kotoki_fake_example", Temperature: -1, MaxOutputTokens: 100,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}, RequestID: "EVALUATION:s"}
}

func TestGenerateJSONRetriesSameVendorThenFails(t *testing.T) {
	ev := &stubEval{script: []error{perr(provider.KindUnavailable, true), perr(provider.KindUnavailable, true), perr(provider.KindUnavailable, true)}}
	r, env := newTestRouter(t, withRole(provider.RoleWorker), withStubs(ev, nil))
	_, err := r.GenerateJSON(context.Background(), evalRequest())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, pe.Kind)
	require.EqualValues(t, 3, ev.calls.Load())
	require.Equal(t, []time.Duration{time.Second, 4 * time.Second}, env.Sleeps.got, "backoff 1 s, 4 s (identity jitter)")
}

func TestGenerateJSONSucceedsOnSecondAttempt(t *testing.T) {
	ev := &stubEval{script: []error{perr(provider.KindTimeout, true)}}
	r, env := newTestRouter(t, withStubs(ev, nil))
	res, err := r.GenerateJSON(context.Background(), evalRequest())
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(res.JSON))
	require.EqualValues(t, 2, ev.calls.Load())
	require.Equal(t, []time.Duration{time.Second}, env.Sleeps.got)
	got := ev.last.Load()
	require.Equal(t, 0.2, got.Temperature, "a negative temperature becomes 0.2")
	require.Equal(t, "m", got.Model, "filled from <VENDOR>_EVALUATION_MODEL")
}

func TestRetryAfterHonoredUpTo30s(t *testing.T) {
	for retryAfter, want := range map[time.Duration]time.Duration{7 * time.Second: 7 * time.Second, 90 * time.Second: 30 * time.Second} {
		e := perr(provider.KindRateLimited, true)
		e.RetryAfter = retryAfter
		ev := &stubEval{script: []error{e}}
		r, env := newTestRouter(t, withStubs(ev, nil))
		_, err := r.GenerateJSON(context.Background(), evalRequest())
		require.NoError(t, err)
		require.Equal(t, []time.Duration{want}, env.Sleeps.got, "Retry-After replaces the backoff, capped at 30 s")
	}
}

func TestNonRetryableFailsImmediately(t *testing.T) {
	for _, kind := range []provider.ErrorKind{provider.KindAuth, provider.KindInvalidRequest, provider.KindContentFiltered} {
		ev := &stubEval{script: []error{perr(kind, false)}}
		r, env := newTestRouter(t, withStubs(ev, nil))
		_, err := r.GenerateJSON(context.Background(), evalRequest())
		require.Equal(t, kind, provider.KindOf(err))
		require.EqualValues(t, 1, ev.calls.Load(), kind)
		require.Empty(t, env.Sleeps.got)
	}
}

func TestCancelledReturnsImmediately(t *testing.T) {
	ev := &stubEval{script: []error{perr(provider.KindCancelled, false)}}
	r, env := newTestRouter(t, withStubs(ev, nil))
	_, err := r.GenerateJSON(context.Background(), evalRequest())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
	require.EqualValues(t, 1, ev.calls.Load())
	require.Empty(t, env.Sleeps.got)

	ctx, cancel := context.WithCancel(context.Background())
	ev2 := &stubEval{hold: make(chan struct{})}
	r2, _ := newTestRouter(t, withStubs(ev2, nil))
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err = r2.GenerateJSON(ctx, evalRequest())
	require.Equal(t, provider.KindCancelled, provider.KindOf(err))
	require.EqualValues(t, 1, ev2.calls.Load())
}

func TestGenerateJSONInvalidResponseRetried(t *testing.T) {
	ev := &stubEval{script: []error{perr(provider.KindInvalidResponse, true)}}
	r, _ := newTestRouter(t, withStubs(ev, nil))
	_, err := r.GenerateJSON(context.Background(), evalRequest())
	require.NoError(t, err)
	require.EqualValues(t, 2, ev.calls.Load(), "INVALID_RESPONSE is retried on the same vendor")

	bad := &stubEval{json: `{not json`}
	r2, _ := newTestRouter(t, withStubs(bad, nil))
	_, err = r2.GenerateJSON(context.Background(), evalRequest())
	require.Equal(t, provider.KindInvalidResponse, provider.KindOf(err))
	require.EqualValues(t, 3, bad.calls.Load(), "a body that is not JSON is INVALID_RESPONSE and retried")
}

// stubEmbed answers Embed from a per-call script.
type stubEmbed struct {
	script []error
	calls  atomic.Int32
}

func (s *stubEmbed) Name() provider.Name { return provider.FakePrimary }

func (s *stubEmbed) Embed(_ context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	n := int(s.calls.Add(1))
	if n-1 < len(s.script) && s.script[n-1] != nil {
		return provider.EmbeddingResult{}, s.script[n-1]
	}
	return provider.EmbeddingResult{Vectors: make([][]float32, len(req.Texts)), Usage: provider.TokenUsage{InputTokens: 3}}, nil
}

func TestEmbedThreeAttemptsBackoff500ms2s(t *testing.T) {
	em := &stubEmbed{script: []error{perr(provider.KindUnavailable, true), perr(provider.KindUnavailable, true), perr(provider.KindUnavailable, true)}}
	r, env := newTestRouter(t, withStubs(nil, em))
	_, err := r.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{"a"}})
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
	require.EqualValues(t, 3, em.calls.Load())
	require.Equal(t, []time.Duration{500 * time.Millisecond, 2 * time.Second}, env.Sleeps.got)

	ok := &stubEmbed{}
	r2, _ := newTestRouter(t, withStubs(nil, ok))
	res, err := r2.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{"a"}})
	require.NoError(t, err)
	require.Equal(t, "m", res.Model, "the embedding model comes from OPENAI_EMBEDDING_MODEL")
}

// seqLLM scripts CompleteText attempts: each call yields the next plan.
type seqLLM struct {
	calls atomic.Int32
	plans []func() ([]provider.LLMEvent, error)
	onRun func()
}

func (s *seqLLM) Name() provider.Name { return provider.FakePrimary }

func (s *seqLLM) Stream(_ context.Context, _ provider.LLMRequest) (provider.LLMStream, error) {
	n := int(s.calls.Add(1))
	if s.onRun != nil {
		s.onRun()
	}
	evs, err := s.plans[min(n-1, len(s.plans)-1)]()
	if err != nil {
		return nil, err
	}
	ch := make(chan provider.LLMEvent, len(evs)+1)
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return scriptStream{ch}, nil
}

func okPlan(text string) func() ([]provider.LLMEvent, error) {
	return func() ([]provider.LLMEvent, error) {
		return []provider.LLMEvent{{Type: provider.LLMDelta, Text: text},
			{Type: provider.LLMDone, FinishReason: provider.FinishStop, Usage: &provider.TokenUsage{InputTokens: 5, OutputTokens: 2}}}, nil
	}
}

func failPlan() func() ([]provider.LLMEvent, error) {
	return func() ([]provider.LLMEvent, error) {
		return nil, &provider.Error{Provider: provider.FakePrimary, Capability: provider.CapLLM, Kind: provider.KindUnavailable, Retryable: true}
	}
}

func TestCompleteTextWorkerThreeAttempts(t *testing.T) {
	llm := &seqLLM{plans: []func() ([]provider.LLMEvent, error){failPlan(), failPlan(), okPlan("まとめ")}}
	r, env := newTestRouter(t, withLLM(llm), withRole(provider.RoleWorker), withCfg(func(c *provider.RouterConfig) { c.OpenAILLMModel = "gpt-4.1" }))
	llm.onRun = func() { env.Clock.Advance(40 * time.Millisecond) }
	res, err := r.CompleteText(context.Background(), provider.TextRequest{Task: "SUMMARY", Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}, MaxTokens: 50})
	require.NoError(t, err)
	require.Equal(t, "まとめ", res.Text)
	require.EqualValues(t, 3, llm.calls.Load())
	require.Equal(t, []time.Duration{time.Second, 4 * time.Second}, env.Sleeps.got)
	require.Equal(t, "gpt-4.1", res.Model)
	require.Equal(t, provider.FinishStop, res.FinishReason)
	require.Equal(t, 5, res.Usage.InputTokens)
	require.Equal(t, 120*time.Millisecond, res.Latency, "latency covers all attempts")
}

func TestCompleteTextRealtimeSingleAttempt(t *testing.T) {
	llm := &seqLLM{plans: []func() ([]provider.LLMEvent, error){failPlan(), okPlan("x")}}
	r, env := newTestRouter(t, withLLM(llm), withRole(provider.RoleRealtime))
	_, err := r.CompleteText(context.Background(), provider.TextRequest{Task: "ROLLING_SUMMARY", Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
	require.Error(t, err)
	require.EqualValues(t, 1, llm.calls.Load(), "the realtime process does not retry")
	require.Empty(t, env.Sleeps.got)
}

func TestCompleteTextContentFilteredAndStreamErrors(t *testing.T) {
	llm := &seqLLM{plans: []func() ([]provider.LLMEvent, error){
		func() ([]provider.LLMEvent, error) {
			return []provider.LLMEvent{{Type: provider.LLMDone, FinishReason: provider.FinishContentFiltered}}, nil
		}}}
	r, _ := newTestRouter(t, withLLM(llm), withRole(provider.RoleWorker))
	_, err := r.CompleteText(context.Background(), provider.TextRequest{Task: "SUMMARY", Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
	require.Equal(t, provider.KindContentFiltered, provider.KindOf(err))
	require.EqualValues(t, 1, llm.calls.Load(), "content-filtered is not retried")

	llm2 := &seqLLM{plans: []func() ([]provider.LLMEvent, error){
		func() ([]provider.LLMEvent, error) {
			return []provider.LLMEvent{{Type: provider.LLMError, Err: &provider.Error{Kind: provider.KindAuth}}}, nil
		}}}
	r2, _ := newTestRouter(t, withLLM(llm2), withRole(provider.RoleWorker))
	_, err = r2.CompleteText(context.Background(), provider.TextRequest{Task: "SUMMARY", Messages: []provider.Message{{Role: provider.RoleUser, Content: "x"}}})
	require.Equal(t, provider.KindAuth, provider.KindOf(err))
}

func TestEvaluationSingleAttempt(t *testing.T) {
	ev := &stubEval{script: []error{perr(provider.KindUnavailable, true)}}
	r, env := newTestRouter(t, withStubs(ev, nil), withRole(provider.RoleWorker))
	require.Implements(t, (*provider.EvaluationProvider)(nil), r.Evaluation(), "the wrapper satisfies the port")
	_, err := r.Evaluation().GenerateJSON(context.Background(), evalRequest())
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
	require.EqualValues(t, 1, ev.calls.Load(), "Evaluation() makes exactly one attempt per call")
	require.Empty(t, env.Sleeps.got)
	require.Equal(t, "m", ev.last.Load().Model)
	require.Equal(t, provider.FakePrimary, r.Evaluation().Name())

	ev2 := &stubEval{}
	r2, env2 := newTestRouter(t, withStubs(ev2, nil))
	env2.openBreaker(t, r2, provider.FakePrimary, provider.CapEvaluation)
	_, err = r2.Evaluation().GenerateJSON(context.Background(), evalRequest())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, pe.Kind)
	require.Equal(t, "circuit_open", pe.VendorCode)
	require.True(t, pe.Retryable)
	require.Zero(t, ev2.calls.Load(), "an open circuit never reaches the vendor")
}

func TestAsyncSemaphoreWait30sThenRateLimited(t *testing.T) {
	ev := &stubEval{name: provider.OpenAI, hold: make(chan struct{})}
	r, _ := newTestRouter(t, withStubs(ev, nil), withRole(provider.RoleWorker),
		withCfg(func(c *provider.RouterConfig) { c.Concurrency.OpenAIEvaluation = 1 }),
		withDeps(func(d *provider.RouterDeps) { d.AsyncWait = 60 * time.Millisecond }))
	done := make(chan error, 1)
	go func() {
		_, err := r.GenerateJSON(context.Background(), evalRequest())
		done <- err
	}()
	require.Eventually(t, func() bool { return ev.calls.Load() == 1 }, 2*time.Second, 2*time.Millisecond)

	start := time.Now()
	_, err := r.GenerateJSON(context.Background(), evalRequest())
	var pe *provider.Error
	require.True(t, errors.As(err, &pe))
	require.Equal(t, provider.KindRateLimited, pe.Kind)
	require.Equal(t, "local_concurrency", pe.VendorCode)
	require.True(t, pe.Retryable)
	require.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "waited for a slot first")
	close(ev.hold)
	require.NoError(t, <-done)
	require.EqualValues(t, 1, ev.calls.Load())
}

type stubPrices struct{}

func (stubPrices) LLM(_ provider.Name, _ provider.Capability, _ string, u provider.TokenUsage) (float64, bool) {
	return float64(u.InputTokens+u.OutputTokens) * 0.001, true
}
func (stubPrices) TTS(provider.Name, string, int) (float64, bool) { return 0, false }
func (stubPrices) Audio(provider.Name, provider.Capability, string, int64) (float64, bool) {
	return 0, false
}

func TestAsyncMetricsAndCost(t *testing.T) {
	ev := &stubEval{usage: provider.TokenUsage{InputTokens: 100, OutputTokens: 50}}
	r, env := newTestRouter(t, withStubs(ev, nil), withDeps(func(d *provider.RouterDeps) { d.Prices = stubPrices{} }))
	_, err := r.GenerateJSON(context.Background(), evalRequest())
	require.NoError(t, err)
	require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"task": "EVALUATION", "outcome": "success", "capability": "EVALUATION"}))
	require.Equal(t, 100.0, counter(t, env.Reg, "kotoki_provider_usage_total", map[string]string{"unit": "input_tokens", "capability": "EVALUATION"}))
	require.InDelta(t, 0.15, counter(t, env.Reg, "kotoki_provider_cost_usd_total", map[string]string{"capability": "EVALUATION"}), 1e-9)
}
