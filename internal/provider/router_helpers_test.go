package provider_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// testEnv is what the router tests need besides the router itself.
type testEnv struct {
	Clock  *clock.Fake
	Reg    *prometheus.Registry
	Logs   *bytes.Buffer
	Sleeps *sleepLog
	Cfg    provider.RouterConfig
	Deps   provider.RouterDeps
	Faults *faultBox
}

type sleepLog struct{ got []time.Duration }

func (s *sleepLog) sleep(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

// faultBox is a fake.FaultSource whose document tests can swap.
type faultBox struct{ set *fake.FaultSet }

func (f *faultBox) Current() *fake.FaultSet { return f.set }

type testOpt func(*testEnv, *testBuild)

type testBuild struct {
	ttsNames []provider.Name
	llmName  provider.Name
	llmCap   fake.CapConfig
	llm      provider.LLMProvider // replaces the fake LLM when set
}

func withCfg(f func(*provider.RouterConfig)) testOpt {
	return func(e *testEnv, _ *testBuild) { f(&e.Cfg) }
}

func withTTSVendors(names ...provider.Name) testOpt {
	return func(_ *testEnv, b *testBuild) { b.ttsNames = names }
}

// withLLMCap configures the fake LLM's failure behaviour.
func withLLMCap(c fake.CapConfig) testOpt { return func(_ *testEnv, b *testBuild) { b.llmCap = c } }

// withLLM replaces the LLM adapter (scripted stubs).
func withLLM(p provider.LLMProvider) testOpt { return func(_ *testEnv, b *testBuild) { b.llm = p } }

func withLLMName(n provider.Name) testOpt { return func(_ *testEnv, b *testBuild) { b.llmName = n } }

// fakeConfig is a valid all-fake RouterConfig.
func fakeConfig() provider.RouterConfig {
	c := validRealConfig()
	c.Mode = provider.ModeFake
	c.TTSFallbackEnabled = true
	return c
}

// newTestRouter wires FAKE_PRIMARY for every capability and FAKE_FALLBACK for TTS, a fake
// clock, recording Sleep, identity Jitter, a private registry and a JSON log buffer.
func newTestRouter(t *testing.T, opts ...testOpt) (*provider.Router, *testEnv) {
	t.Helper()
	env := &testEnv{Clock: clock.NewFake(t0), Reg: prometheus.NewRegistry(), Logs: &bytes.Buffer{}, Sleeps: &sleepLog{},
		Cfg: fakeConfig(), Faults: &faultBox{}}
	build := &testBuild{ttsNames: []provider.Name{provider.FakePrimary, provider.FakeFallback}, llmName: provider.FakePrimary}
	for _, o := range opts {
		o(env, build)
	}
	log := slog.New(slog.NewJSONHandler(env.Logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fd := fake.Deps{Clock: env.Clock, Faults: env.Faults, Log: log, Seed: 42}
	fcfg := fake.Config{TTSChunkIntervalMS: 0, TTSMsPerChar: 120}

	stt, err := fake.NewSTT(provider.FakePrimary, fake.CapConfig{}, fcfg, fd)
	require.NoError(t, err)
	tts := map[provider.Name]provider.TTSProvider{}
	for _, n := range build.ttsNames {
		tts[n] = fake.NewTTS(n, fake.CapConfig{}, fcfg, fd)
	}
	var llm provider.LLMProvider = namedLLM{fake.NewLLM(provider.FakePrimary, build.llmCap, fcfg, fd), build.llmName}
	if build.llm != nil {
		llm = build.llm
	}
	env.Deps = provider.RouterDeps{
		STT: stt, LLM: llm,
		Evaluation: fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fcfg, fd),
		TTS:        tts, Embedding: fake.NewEmbedding(provider.FakePrimary, fake.CapConfig{}, fcfg, fd),
		Clock: env.Clock, Log: log, Role: provider.RoleRealtime, Metrics: provider.NewMetrics(env.Reg),
		Sleep: env.Sleeps.sleep, Jitter: func(d time.Duration) time.Duration { return d },
	}
	r, err := provider.NewRouter(env.Cfg, env.Deps)
	require.NoError(t, err)
	return r, env
}

// namedLLM reports a different vendor name for the same fake LLM.
type namedLLM struct {
	*fake.LLM
	name provider.Name
}

func (n namedLLM) Name() provider.Name { return n.name }

// openBreaker records enough failures to open the breaker of (p, c).
func (e *testEnv) openBreaker(t *testing.T, r *provider.Router, p provider.Name, c provider.Capability) {
	t.Helper()
	b := r.BreakerForTest(p, c)
	for i := 0; i < e.Cfg.CBFailureThreshold; i++ {
		tk, ok := b.Allow()
		require.True(t, ok)
		tk.Done(&provider.Error{Kind: provider.KindUnavailable})
	}
	require.Equal(t, provider.BreakerOpen, b.State())
}
