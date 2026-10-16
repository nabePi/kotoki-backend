package providerwire_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

func asm(t *testing.T, cfg provider.RouterConfig, caps []provider.Capability) (providerwire.Assembled, error) {
	t.Helper()
	return providerwire.Assemble(cfg, fake.Config{}, caps, providerwire.Deps{Role: provider.RoleRealtime, Metrics: provider.NewMetrics(nil), Lookup: noEnv})
}

func TestAssembleRealtimeRealModeBuildsAWorkingRouter(t *testing.T) {
	a, err := asm(t, realConfig(t), realtimeCaps)
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, a.Deps.STT.Name())
	require.Equal(t, provider.OpenAI, a.Deps.LLM.Name())
	require.Equal(t, provider.OpenAI, a.Deps.Evaluation.Name())
	require.Equal(t, provider.OpenAI, a.Deps.Embedding.Name())
	require.Len(t, a.Deps.TTS, 2)
	require.True(t, a.RealTTS)
	require.Equal(t, []string{"CARTESIA", "ELEVENLABS"}, a.TTSVendors)
	_, err = provider.NewRouter(a.Config, a.Deps)
	require.NoError(t, err)
}

func TestAssembleFakeModeIsAllFakes(t *testing.T) {
	cfg := realConfig(t)
	cfg.Mode = "fake"
	cfg.OpenAIAPIKey = ""
	a, err := asm(t, cfg, realtimeCaps)
	require.NoError(t, err)
	require.False(t, a.RealTTS)
	require.Empty(t, a.TTSVendors)
	require.Contains(t, a.Deps.TTS, provider.FakePrimary)
	_, err = provider.NewRouter(a.Config, a.Deps)
	require.NoError(t, err)
}

func TestAssembleWorkerUsesFakeStandInsForSTTAndTTS(t *testing.T) {
	cfg := realConfig(t)
	cfg.ElevenLabsAPIKey, cfg.CartesiaAPIKey = "", "" // the worker never calls STT/TTS: no vendor key needed for them
	a, err := asm(t, cfg, []provider.Capability{provider.CapLLM, provider.CapEmbedding})
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, a.Deps.LLM.Name())
	require.Equal(t, provider.OpenAI, a.Deps.Evaluation.Name())
	require.Equal(t, provider.OpenAI, a.Deps.Embedding.Name())
	require.False(t, a.RealTTS)
	require.Contains(t, a.Deps.TTS, provider.FakePrimary)
	require.Equal(t, provider.ModeFake, a.Config.ModeFor(provider.CapSTT))
	require.Equal(t, provider.ModeFake, a.Config.ModeFor(provider.CapTTS))
	require.Equal(t, provider.ModeReal, a.Config.ModeFor(provider.CapLLM))
	_, err = provider.NewRouter(a.Config, a.Deps)
	require.NoError(t, err)
}

func TestAssembleMixedModes(t *testing.T) {
	cfg := realConfig(t)
	cfg.ModeLLM = "fake" // LLM and EVALUATION fake, the rest real
	a, err := asm(t, cfg, realtimeCaps)
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, a.Deps.STT.Name())
	require.NotEqual(t, provider.OpenAI, a.Deps.LLM.Name())
	require.NotEqual(t, provider.OpenAI, a.Deps.Evaluation.Name())
	require.True(t, a.RealTTS)
}

func TestAssembleRejectsInvalidRealConfigBeforeBuilding(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAIAPIKey = ""
	_, err := asm(t, cfg, realtimeCaps)
	require.ErrorContains(t, err, "OPENAI_API_KEY")
	cfg = realConfig(t)
	cfg.OpenAISTTEnabled, cfg.CartesiaSTTEnabled = true, true
	_, err = asm(t, cfg, realtimeCaps)
	require.ErrorContains(t, err, "CARTESIA_STT_ENABLED")
}

// The adapters publish the vendor rate-limit headroom into the pod's provider.Metrics (Config.Metrics).
func TestAssembleWiresRateLimitMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-remaining-requests", "42")
		w.WriteHeader(http.StatusInternalServerError) // the body does not matter: only the header is read
	}))
	t.Cleanup(srv.Close)
	cfg := realConfig(t)
	cfg.OpenAIBaseURL = srv.URL
	reg := prometheus.NewRegistry()
	a, err := providerwire.Assemble(cfg, fake.Config{}, []provider.Capability{provider.CapEmbedding},
		providerwire.Deps{Role: provider.RoleWorker, Metrics: provider.NewMetrics(reg), Lookup: noEnv})
	require.NoError(t, err)
	_, _ = a.Deps.Embedding.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{"x"}, Model: "m"})
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var got float64 = -1
	for _, mf := range mfs {
		if mf.GetName() == "kotoki_provider_ratelimit_remaining" {
			for _, m := range mf.GetMetric() {
				got = m.GetGauge().GetValue()
			}
		}
	}
	require.Equal(t, 42.0, got)
}
