package provider_test

import (
	"errors"
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

func TestTTSCandidatesFromVoiceSelection(t *testing.T) {
	r, _ := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	v := provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.Cartesia, ModelName: "sonic", VoiceID: "v1"},
		Fallback: &provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "eleven", VoiceID: "v2"},
	}
	got, err := r.TTSCandidatesForTest(v)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, provider.Cartesia, got[0].Ref.Provider)
	require.Equal(t, provider.ElevenLabs, got[1].Ref.Provider)
	require.False(t, got[1].ViaCircuitOpen)

	v.Fallback = nil
	got, err = r.TTSCandidatesForTest(v)
	require.NoError(t, err)
	require.Len(t, got, 1)

	r2, _ := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs), withCfg(func(c *provider.RouterConfig) { c.TTSFallbackEnabled = false }))
	v.Fallback = &provider.VoiceRef{Provider: provider.ElevenLabs, VoiceID: "v2"}
	got, err = r2.TTSCandidatesForTest(v)
	require.NoError(t, err)
	require.Len(t, got, 1, "PROVIDER_TTS_FALLBACK_ENABLED=false ignores voice.fallback")
}

func TestTTSBreakerSharedAcrossCharacters(t *testing.T) {
	r, env := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	env.openBreaker(t, r, provider.Cartesia, provider.CapTTS)
	a := provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, VoiceID: "character-a"}}
	b := provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, VoiceID: "character-b"}}
	for _, v := range []provider.VoiceSelection{a, b} {
		_, err := r.TTSCandidatesForTest(v)
		require.Equal(t, "circuit_open", ventorCode(err), "one breaker per (vendor, capability), shared by every character")
	}
}

func ventorCode(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.VendorCode
	}
	return ""
}

func TestTTSCandidatesSkipOpenPrimaryToCharacterFallback(t *testing.T) {
	r, env := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	env.openBreaker(t, r, provider.Cartesia, provider.CapTTS)
	v := provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.Cartesia, VoiceID: "v1"},
		Fallback: &provider.VoiceRef{Provider: provider.ElevenLabs, VoiceID: "v2"},
	}
	got, err := r.TTSCandidatesForTest(v)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, provider.ElevenLabs, got[0].Ref.Provider)
	require.True(t, got[0].ViaCircuitOpen)
}

func TestTTSAllCircuitsOpenIsCircuitOpen(t *testing.T) {
	r, env := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	env.openBreaker(t, r, provider.Cartesia, provider.CapTTS)
	env.openBreaker(t, r, provider.ElevenLabs, provider.CapTTS)
	v := provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.Cartesia},
		Fallback: &provider.VoiceRef{Provider: provider.ElevenLabs},
	}
	_, err := r.TTSCandidatesForTest(v)
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindUnavailable, pe.Kind)
	require.Equal(t, "circuit_open", pe.VendorCode)
}

func TestTTSOpenPrimaryWithoutFallbackIsCircuitOpen(t *testing.T) {
	r, env := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	env.openBreaker(t, r, provider.Cartesia, provider.CapTTS)
	_, err := r.TTSCandidatesForTest(provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia}})
	require.Equal(t, "circuit_open", ventorCode(err))
}

func TestTTSHalfOpenPrimaryIsUsableAgain(t *testing.T) {
	r, env := newTestRouter(t, withTTSVendors(provider.Cartesia, provider.ElevenLabs))
	env.openBreaker(t, r, provider.Cartesia, provider.CapTTS)
	env.Clock.Advance(env.Cfg.CBOpenDuration)
	got, err := r.TTSCandidatesForTest(provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia}})
	require.NoError(t, err, "after the open duration the primary may be probed again")
	require.Len(t, got, 1)
	require.Equal(t, provider.BreakerOpen, r.BreakerForTest(provider.Cartesia, provider.CapTTS).State(), "peeking must not consume the probe")
}

func TestTTSUnregisteredVendorIsVoiceBindingMissing(t *testing.T) {
	r, _ := newTestRouter(t, withTTSVendors(provider.ElevenLabs))
	_, err := r.TTSCandidatesForTest(provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia}})
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "voice_binding_missing", pe.VendorCode)
	require.Equal(t, provider.KindInvalidRequest, pe.Kind)

	got, err := r.TTSCandidatesForTest(provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.Cartesia},
		Fallback: &provider.VoiceRef{Provider: provider.ElevenLabs},
	})
	require.NoError(t, err, "an unregistered primary is skipped when the fallback is registered")
	require.Len(t, got, 1)
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
