package providerwire_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

func fakeCfg() provider.RouterConfig {
	return provider.RouterConfig{Mode: provider.ModeFake, EmbeddingDimensions: 1536}
}

// parsedCfg is what cmd/realtime parses: the env defaults of RouterConfig with PROVIDER_MODE=fake.
func parsedCfg(t *testing.T) (provider.RouterConfig, fake.Config) {
	t.Helper()
	t.Setenv("PROVIDER_MODE", "fake")
	rc, fc, err := providerwire.ParseConfigs()
	require.NoError(t, err)
	return rc, fc
}

var realtimeCaps = []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEmbedding}

func noEnv(string) (string, bool) { return "", false }

func TestBuildFakeFillsFullRouterDeps(t *testing.T) {
	for _, role := range []provider.ProcessRole{provider.RoleRealtime, provider.RoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			rc, fc := parsedCfg(t)
			rd, err := providerwire.BuildFake(rc, fc, realtimeCaps,
				providerwire.Deps{Role: role, Metrics: provider.NewMetrics(nil), Lookup: noEnv})
			require.NoError(t, err)
			require.NotNil(t, rd.STT)
			require.NotNil(t, rd.LLM)
			require.NotNil(t, rd.Evaluation, "EVALUATION follows LLM: NewRouter needs it")
			require.NotNil(t, rd.Embedding)
			require.Len(t, rd.TTS, 2)
			require.NotNil(t, rd.TTS[provider.FakePrimary])
			require.NotNil(t, rd.TTS[provider.FakeFallback])
			require.NotNil(t, rd.Prices)
			require.NotNil(t, rd.Clock)
			require.NotNil(t, rd.Log)
			require.NotNil(t, rd.Metrics)
			require.Equal(t, role, rd.Role)

			router, err := provider.NewRouter(rc, rd)
			require.NoError(t, err)
			res, err := router.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{"こんにちは"}, Dimensions: 1536, RequestID: "t"})
			require.NoError(t, err)
			require.Len(t, res.Vectors, 1)
			require.Len(t, res.Vectors[0], 1536)
		})
	}
}

func TestBuildFakeSkipsCapabilitiesNotRequested(t *testing.T) {
	rd, err := providerwire.BuildFake(fakeCfg(), fake.Config{Seed: 1}, []provider.Capability{provider.CapEmbedding},
		providerwire.Deps{Lookup: noEnv})
	require.NoError(t, err)
	require.NotNil(t, rd.Embedding)
	require.Nil(t, rd.STT)
	require.Nil(t, rd.LLM)
	require.Nil(t, rd.Evaluation)
	require.Empty(t, rd.TTS)

	rd, err = providerwire.BuildFake(fakeCfg(), fake.Config{Seed: 1}, []provider.Capability{provider.CapTTS},
		providerwire.Deps{Lookup: noEnv})
	require.NoError(t, err)
	require.Len(t, rd.TTS, 2)
	require.Nil(t, rd.Embedding)
}

func TestBuildFakeRejectsRealMode(t *testing.T) {
	for name, rc := range map[string]provider.RouterConfig{
		"all":       {Mode: provider.ModeReal},
		"tts only":  {Mode: provider.ModeFake, ModeTTS: provider.ModeReal},
		"llm only":  {Mode: provider.ModeFake, ModeLLM: provider.ModeReal},
		"embedding": {Mode: provider.ModeFake, ModeEmbedding: provider.ModeReal},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := providerwire.BuildFake(rc, fake.Config{}, realtimeCaps, providerwire.Deps{Lookup: noEnv})
			require.ErrorIs(t, err, providerwire.ErrRealModeUnavailable)
			require.Contains(t, err.Error(), "M9")
		})
	}
}

func TestBuildFakeReportsBadFakeVariable(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if k == "FAKE_PRIMARY_LLM_LATENCY_MS" {
			return "abc", true
		}
		return "", false
	}
	_, err := providerwire.BuildFake(fakeCfg(), fake.Config{}, realtimeCaps, providerwire.Deps{Lookup: lookup})
	require.ErrorContains(t, err, "FAKE_PRIMARY_LLM_LATENCY_MS")
}
