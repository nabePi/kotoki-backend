package provider_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

var voice = provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.FakePrimary, VoiceID: "v"}}

func userReq(text string) provider.LLMRequest {
	return provider.LLMRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: text}}, MaxTokens: 100, Temperature: 0.5, RequestID: "s:1"}
}

func drainLLM(t *testing.T, s provider.LLMStream) []provider.LLMEvent {
	t.Helper()
	var out []provider.LLMEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("LLM stream did not finish")
		}
	}
}

func types(evs []provider.LLMEvent) []provider.LLMEventType {
	out := make([]provider.LLMEventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// counter returns the value of the counter series matching all labels (0 if absent).
func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// scriptLLM is a scripted LLM adapter.
type scriptLLM struct {
	name   provider.Name
	events []provider.LLMEvent
	calls  atomic.Int32
	last   atomic.Pointer[provider.LLMRequest]
	hold   bool // never finish until the context ends
}

func (s *scriptLLM) Name() provider.Name {
	if s.name == "" {
		return provider.FakePrimary
	}
	return s.name
}

type scriptStream struct{ ch chan provider.LLMEvent }

func (s scriptStream) Events() <-chan provider.LLMEvent { return s.ch }
func (scriptStream) Close() error                       { return nil }

func (s *scriptLLM) Stream(ctx context.Context, req provider.LLMRequest) (provider.LLMStream, error) {
	s.calls.Add(1)
	s.last.Store(&req)
	ch := make(chan provider.LLMEvent, 64)
	go func() {
		defer close(ch)
		if s.hold {
			<-ctx.Done()
			return
		}
		for _, ev := range s.events {
			ch <- ev
		}
	}()
	return scriptStream{ch}, nil
}

func TestGenerationLLMErrorIsFinalNoVendorSwitch(t *testing.T) {
	r, _ := newTestRouter(t, withLLMCap(fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseMid, FailAfter: 2, FailureKind: provider.KindUnavailable}))
	g := r.NewGeneration(uuid.New(), 1, voice)
	s, err := g.StreamLLM(context.Background(), userReq("こんにちは"))
	require.NoError(t, err)
	evs := drainLLM(t, s)
	require.Equal(t, []provider.LLMEventType{provider.LLMDelta, provider.LLMDelta, provider.LLMError}, types(evs))
	require.Equal(t, provider.KindUnavailable, evs[2].Err.Kind)
	sum := g.Summary()
	require.Equal(t, provider.FakePrimary, sum.LLMProvider)
	require.False(t, sum.UsedFallback)
	require.True(t, sum.LLMUsage.Estimated, "no vendor usage after a mid-stream error: estimated")
	require.Equal(t, 6, sum.LLMUsage.OutputTokens, "two 3-rune deltas")
}

func TestGenerationNoResetEventExists(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "llm.go", nil, 0)
	require.NoError(t, err)
	var names []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "LLMEventType" {
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	require.ElementsMatch(t, []string{"LLMDelta", "LLMDone", "LLMError"}, names)
	for _, n := range names {
		require.False(t, strings.Contains(strings.ToUpper(n), "RESET"))
	}
}

func TestGenerationTruncatedAfterAudioSent(t *testing.T) {
	hangAfterFirst := fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseHang, HangAfter: 1}
	run := func(t *testing.T, parentTimeout time.Duration, llmTotal time.Duration) (*provider.Router, *testEnv, []provider.LLMEvent) {
		r, env := newTestRouter(t, withLLMCap(hangAfterFirst), withCfg(func(c *provider.RouterConfig) {
			c.CBFailureThreshold = 1
			if llmTotal > 0 {
				c.TimeoutLLMTotal = llmTotal
			}
		}))
		ctx := context.Background()
		if parentTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, parentTimeout)
			defer cancel()
		}
		g := r.NewGeneration(uuid.New(), 1, voice)
		s, err := g.StreamLLM(ctx, userReq("こんにちは"))
		require.NoError(t, err)
		first := <-s.Events()
		require.Equal(t, provider.LLMDelta, first.Type)
		g.MarkAudioSent()
		rest := drainLLM(t, s)
		require.NotEmpty(t, rest)
		last := rest[len(rest)-1]
		require.Equal(t, provider.LLMDone, last.Type)
		require.Equal(t, provider.FinishTruncated, last.FinishReason, "a deadline after audio is a truncated answer, not an error")
		require.NotNil(t, last.Usage)
		return r, env, append([]provider.LLMEvent{first}, rest...)
	}

	t.Run("session deadline is not a vendor failure", func(t *testing.T) {
		r, env, _ := run(t, 120*time.Millisecond, 0)
		require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"outcome": "truncated"}))
		require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapLLM).State())
	})
	t.Run("LLM_TOTAL counts as a timeout for the breaker", func(t *testing.T) {
		r, env, _ := run(t, 0, 60*time.Millisecond)
		require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"outcome": "truncated"}))
		require.Equal(t, provider.BreakerOpen, r.BreakerForTest(provider.FakePrimary, provider.CapLLM).State())
	})
}

func TestGenerationDeadlineBeforeAudioIsError(t *testing.T) {
	r, env := newTestRouter(t, withLLMCap(fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseHang}))
	g := r.NewGeneration(uuid.New(), 1, voice)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	s, err := g.StreamLLM(ctx, userReq("こんにちは"))
	require.NoError(t, err)
	evs := drainLLM(t, s)
	require.Len(t, evs, 1)
	require.Equal(t, provider.LLMError, evs[0].Type)
	require.Equal(t, provider.KindTimeout, evs[0].Err.Kind)
	require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"outcome": "error"}))

	// the parent context is already past its deadline: fail at once, never call the vendor
	llm := &scriptLLM{}
	r2, _ := newTestRouter(t, withLLM(llm))
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	s2, err := r2.NewGeneration(uuid.New(), 1, voice).StreamLLM(expired, userReq("a"))
	require.NoError(t, err)
	evs = drainLLM(t, s2)
	require.Len(t, evs, 1)
	require.Equal(t, provider.KindTimeout, evs[0].Err.Kind)
	require.Zero(t, llm.calls.Load())
}

func TestGenerationContentFilteredFailsNoBreaker(t *testing.T) {
	t.Run("ERROR kind", func(t *testing.T) {
		r, _ := newTestRouter(t, withLLMCap(fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseBeforeFirst, FailureKind: provider.KindContentFiltered}))
		for i := 0; i < 10; i++ {
			g := r.NewGeneration(uuid.New(), int64(i), voice)
			s, err := g.StreamLLM(context.Background(), userReq("a"))
			require.NoError(t, err)
			evs := drainLLM(t, s)
			require.Len(t, evs, 1)
			require.Equal(t, provider.KindContentFiltered, evs[0].Err.Kind)
		}
		require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapLLM).State())
	})
	t.Run("DONE reason", func(t *testing.T) {
		llm := &scriptLLM{events: []provider.LLMEvent{{Type: provider.LLMDone, FinishReason: provider.FinishContentFiltered}}}
		r, _ := newTestRouter(t, withLLM(llm))
		for i := 0; i < 10; i++ {
			s, err := r.NewGeneration(uuid.New(), 1, voice).StreamLLM(context.Background(), userReq("a"))
			require.NoError(t, err)
			evs := drainLLM(t, s)
			require.Len(t, evs, 1)
			require.Equal(t, provider.LLMDone, evs[0].Type)
			require.Equal(t, provider.FinishContentFiltered, evs[0].FinishReason, "forwarded as is")
			require.NotNil(t, evs[0].Usage)
		}
		require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapLLM).State())
	})
}

func TestGenerationFirstTokenTimeout(t *testing.T) {
	r, _ := newTestRouter(t, withLLMCap(fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseHang}), withCfg(func(c *provider.RouterConfig) {
		c.TimeoutLLMFirstToken = 40 * time.Millisecond
		c.CBFailureThreshold = 1
	}))
	s, err := r.NewGeneration(uuid.New(), 1, voice).StreamLLM(context.Background(), userReq("a"))
	require.NoError(t, err)
	evs := drainLLM(t, s)
	require.Len(t, evs, 1)
	require.Equal(t, provider.KindTimeout, evs[0].Err.Kind)
	require.Equal(t, provider.BreakerOpen, r.BreakerForTest(provider.FakePrimary, provider.CapLLM).State(), "a first-token timeout is a counted failure")
}

func TestGenerationCircuitOpenFailsFast(t *testing.T) {
	llm := &scriptLLM{}
	r, env := newTestRouter(t, withLLM(llm))
	env.openBreaker(t, r, provider.FakePrimary, provider.CapLLM)
	start := time.Now()
	s, err := r.NewGeneration(uuid.New(), 1, voice).StreamLLM(context.Background(), userReq("a"))
	require.NoError(t, err)
	evs := drainLLM(t, s)
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Len(t, evs, 1)
	require.Equal(t, provider.KindUnavailable, evs[0].Err.Kind)
	require.Equal(t, "circuit_open", evs[0].Err.VendorCode)
	require.Zero(t, llm.calls.Load(), "the adapter is never called while the circuit is open")
}

func TestGenerationCancelYieldsCancelledDone(t *testing.T) {
	r, _ := newTestRouter(t, withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }),
		withFakeCfg(func(c *fake.Config) { c.LLMTokenIntervalMS = 20 }))
	ctx, cancel := context.WithCancel(context.Background())
	g := r.NewGeneration(uuid.New(), 1, voice)
	s, err := g.StreamLLM(ctx, userReq("#long"))
	require.NoError(t, err)
	<-s.Events()
	cancel()
	evs := drainLLM(t, s)
	last := evs[len(evs)-1]
	require.Equal(t, provider.LLMDone, last.Type)
	require.Equal(t, provider.FinishCancelled, last.FinishReason)
	require.NotNil(t, last.Usage)
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.FakePrimary, provider.CapLLM).State(), "a cancellation is not a failure")
}

func TestGenerationSemaphoreFullFailsLocalConcurrency(t *testing.T) {
	hold := &scriptLLM{name: provider.OpenAI, hold: true}
	r, _ := newTestRouter(t, withLLM(hold), withCfg(func(c *provider.RouterConfig) {
		c.Concurrency.OpenAILLM = 1
		c.CBFailureThreshold = 1
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := r.NewGeneration(uuid.New(), 1, voice).StreamLLM(ctx, userReq("a"))
	require.NoError(t, err)
	defer func() { _ = first.Close() }()
	require.Eventually(t, func() bool { return hold.calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)

	second, err := r.NewGeneration(uuid.New(), 2, voice).StreamLLM(context.Background(), userReq("b"))
	require.NoError(t, err)
	evs := drainLLM(t, second)
	require.Len(t, evs, 1)
	require.Equal(t, provider.KindRateLimited, evs[0].Err.Kind)
	require.Equal(t, "local_concurrency", evs[0].Err.VendorCode)
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.OpenAI, provider.CapLLM).State())
}

func TestGenerationDefaultsFromConfig(t *testing.T) {
	llm := &scriptLLM{events: []provider.LLMEvent{{Type: provider.LLMDone, FinishReason: provider.FinishStop, Usage: &provider.TokenUsage{InputTokens: 1, OutputTokens: 1}}}}
	r, _ := newTestRouter(t, withLLM(llm), withCfg(func(c *provider.RouterConfig) {
		c.LLMMaxOutputTokens, c.LLMTemperature, c.OpenAILLMModel = 77, 0.33, "gpt-4.1"
	}))
	s, err := r.NewGeneration(uuid.New(), 1, voice).StreamLLM(context.Background(), provider.LLMRequest{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "a"}}, Temperature: -1, Model: "ignored"})
	require.NoError(t, err)
	drainLLM(t, s)
	got := llm.last.Load()
	require.NotNil(t, got)
	require.Equal(t, 77, got.MaxTokens)
	require.Equal(t, 0.33, got.Temperature)
	require.Equal(t, "gpt-4.1", got.Model, "the router fills the model of the active vendor")
}

func TestGenerationSummaryOnSuccess(t *testing.T) {
	llm := &scriptLLM{events: []provider.LLMEvent{
		{Type: provider.LLMDelta, Text: "あいう"},
		{Type: provider.LLMDone, FinishReason: provider.FinishStop, Usage: &provider.TokenUsage{InputTokens: 11, OutputTokens: 3}},
	}}
	r, env := newTestRouter(t, withLLM(llm))
	g := r.NewGeneration(uuid.New(), 1, voice)
	s, err := g.StreamLLM(context.Background(), userReq("a"))
	require.NoError(t, err)
	evs := drainLLM(t, s)
	require.Equal(t, []provider.LLMEventType{provider.LLMDelta, provider.LLMDone}, types(evs))
	sum := g.Summary()
	require.Equal(t, provider.TokenUsage{InputTokens: 11, OutputTokens: 3}, sum.LLMUsage)
	require.Equal(t, 1.0, counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"outcome": "success", "capability": "LLM"}))
	require.Equal(t, 11.0, counter(t, env.Reg, "kotoki_provider_usage_total", map[string]string{"unit": "input_tokens"}))
}

func TestRouterLogsNeverContainText(t *testing.T) {
	const sentinel = "SENTINEL-秘密-123"
	llm := &scriptLLM{events: []provider.LLMEvent{
		{Type: provider.LLMDelta, Text: sentinel},
		{Type: provider.LLMDone, FinishReason: provider.FinishStop},
	}}
	r, env := newTestRouter(t, withLLM(llm))
	req := userReq(sentinel)
	req.Messages = append(req.Messages, provider.Message{Role: provider.RoleSystem, Content: sentinel})
	s, err := r.NewGeneration(uuid.New(), 1, voice).StreamLLM(context.Background(), req)
	require.NoError(t, err)
	drainLLM(t, s)
	require.NotContains(t, env.Logs.String(), sentinel)
	require.NotEmpty(t, env.Logs.String(), "the DEBUG call log exists")
	fams, err := env.Reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				require.NotContains(t, l.GetValue(), sentinel)
			}
		}
	}
}
