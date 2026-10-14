package provider_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func TestSingleVendorOpenBreakerFailsFast(t *testing.T) {
	r, env := newTestRouter(t)
	env.openBreaker(t, r, provider.FakePrimary, provider.CapLLM)
	_, err := r.AdmitForTest(provider.CapLLM)
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, pe.Kind)
	require.Equal(t, "circuit_open", pe.VendorCode)
	require.True(t, pe.Retryable)
	require.Equal(t, provider.FakePrimary, pe.Provider)

	_, err = r.AdmitForTest(provider.CapSTT)
	require.NoError(t, err, "other capabilities have their own breaker")
}

func TestAdmitClosedBreakerAndLocalConcurrency(t *testing.T) {
	r, _ := newTestRouter(t, withCfg(func(c *provider.RouterConfig) { c.Concurrency.OpenAISTT = 1 }))
	_, err := r.AdmitForTest(provider.CapSTT)
	require.NoError(t, err)
}

func TestNewRouterRejectsMissingAdapter(t *testing.T) {
	for name, mutate := range map[string]func(*provider.RouterDeps){
		"STT":        func(d *provider.RouterDeps) { d.STT = nil },
		"LLM":        func(d *provider.RouterDeps) { d.LLM = nil },
		"Evaluation": func(d *provider.RouterDeps) { d.Evaluation = nil },
		"Embedding":  func(d *provider.RouterDeps) { d.Embedding = nil },
		"TTS":        func(d *provider.RouterDeps) { d.TTS = nil },
	} {
		_, env := newTestRouter(t)
		d := env.Deps
		mutate(&d)
		_, err := provider.NewRouter(env.Cfg, d)
		require.Error(t, err, name)
	}
}

func TestNewRouterRejectsInvalidConfig(t *testing.T) {
	_, env := newTestRouter(t)
	cfg := validRealConfig()
	cfg.CartesiaSTTEnabled = true // two STT vendors enabled in real mode
	_, err := provider.NewRouter(cfg, env.Deps)
	require.ErrorContains(t, err, "OPENAI_STT_ENABLED")
}

func TestHealthSnapshotListsAllKeys(t *testing.T) {
	r, _ := newTestRouter(t)
	snap := r.HealthSnapshot()
	type k struct {
		p provider.Name
		c provider.Capability
	}
	got := map[k]provider.ProviderHealth{}
	for _, row := range snap {
		got[k{row.Provider, row.Capability}] = row
	}
	for _, want := range []k{
		{provider.FakePrimary, provider.CapSTT}, {provider.FakePrimary, provider.CapLLM}, {provider.FakePrimary, provider.CapEvaluation},
		{provider.FakePrimary, provider.CapEmbedding}, {provider.FakePrimary, provider.CapTTS}, {provider.FakeFallback, provider.CapTTS},
	} {
		row, ok := got[want]
		require.True(t, ok, "%v missing", want)
		require.Equal(t, "CLOSED", row.Circuit)
		require.Positive(t, row.ConcurrencyLimit)
	}
	require.Len(t, snap, 6)
}

func TestHealthSnapshotShowsOpenCircuit(t *testing.T) {
	r, env := newTestRouter(t)
	env.openBreaker(t, r, provider.FakePrimary, provider.CapLLM)
	for _, row := range r.HealthSnapshot() {
		if row.Provider == provider.FakePrimary && row.Capability == provider.CapLLM {
			require.Equal(t, "OPEN", row.Circuit)
			return
		}
	}
	t.Fatal("row missing")
}

func TestBreakerTransitionIsLoggedAndCounted(t *testing.T) {
	r, env := newTestRouter(t)
	env.openBreaker(t, r, provider.FakePrimary, provider.CapSTT)
	require.Contains(t, env.Logs.String(), "provider circuit state changed")
	fams, err := env.Reg.Gather()
	require.NoError(t, err)
	found := false
	for _, f := range fams {
		found = found || f.GetName() == "kotoki_provider_circuit_transitions_total"
	}
	require.True(t, found)
}

func TestModelSelection(t *testing.T) {
	r, _ := newTestRouter(t, withLLMName(provider.OpenAI), withCfg(func(c *provider.RouterConfig) {
		c.OpenAILLMModel, c.DeepSeekLLMModel = "gpt-4.1", "deepseek-flash"
	}))
	require.Equal(t, "gpt-4.1", r.LLMModelForTest())
	require.Equal(t, "explicit", r.TextModelForTest(provider.TextRequest{Model: "explicit"}))
	require.Equal(t, "gpt-4.1", r.TextModelForTest(provider.TextRequest{Task: "SUMMARY"}))

	d, _ := newTestRouter(t, withLLMName(provider.DeepSeek), withCfg(func(c *provider.RouterConfig) {
		c.OpenAILLMModel, c.DeepSeekLLMModel = "gpt-4.1", "deepseek-flash"
		c.OpenAILLMEnabled, c.DeepSeekLLMEnabled = false, true
		c.RollingSummaryLLMModel = "cheap-model"
	}))
	require.Equal(t, "deepseek-flash", d.LLMModelForTest())
	require.Equal(t, "cheap-model", d.TextModelForTest(provider.TextRequest{Task: "ROLLING_SUMMARY"}))
	require.Equal(t, "deepseek-flash", d.TextModelForTest(provider.TextRequest{Task: "SUMMARY"}))
	require.Equal(t, "explicit", d.TextModelForTest(provider.TextRequest{Task: "ROLLING_SUMMARY", Model: "explicit"}))
}

func TestTTSVendorsListsRegistered(t *testing.T) {
	r, _ := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	require.Equal(t, []provider.Name{provider.Cartesia, provider.ElevenLabs}, r.TTSVendorsForTest())
}
