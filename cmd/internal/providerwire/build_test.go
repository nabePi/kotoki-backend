package providerwire_test

import (
	"log/slog"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

var allCaps = []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapEvaluation, provider.CapEmbedding, provider.CapTTS}

// realConfig is a RouterConfig in mode real with the D-017 defaults, fake keys, and no vendor
// reachable: building adapters never dials (Open/first request does).
func realConfig(t *testing.T) provider.RouterConfig {
	t.Helper()
	for _, k := range []string{"PROVIDER_MODE", "PROVIDER_MODE_STT", "PROVIDER_MODE_LLM", "PROVIDER_MODE_TTS", "PROVIDER_MODE_EVALUATION", "PROVIDER_MODE_EMBEDDING"} {
		t.Setenv(k, "")
	}
	t.Setenv("PROVIDER_MODE", "real")
	rc, _, err := providerwire.ParseConfigs()
	require.NoError(t, err)
	rc.OpenAIAPIKey, rc.DeepSeekAPIKey = "sk-test-openai", "sk-test-deepseek"
	rc.ElevenLabsAPIKey, rc.CartesiaAPIKey = "xi-test-key", "ca-test-key"
	rc.OpenAIBaseURL, rc.OpenAIWSBaseURL = "http://127.0.0.1:1/v1", "ws://127.0.0.1:1/realtime"
	rc.DeepSeekBaseURL = "http://127.0.0.1:1"
	rc.ElevenLabsBaseURL, rc.ElevenLabsWSBaseURL = "http://127.0.0.1:1", "ws://127.0.0.1:1"
	rc.CartesiaBaseURL, rc.CartesiaWSBaseURL = "http://127.0.0.1:1", "ws://127.0.0.1:1"
	return rc
}

func TestBuildDefaultsOpenAISTTAndLLM(t *testing.T) {
	cfg := realConfig(t)
	d, err := providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, d.STT.Name())
	require.Equal(t, provider.OpenAI, d.LLM.Name())
	require.Equal(t, provider.OpenAI, d.Evaluation.Name())
	require.Equal(t, provider.OpenAI, d.Embedding.Name())
	require.Equal(t, []string{"CARTESIA", "ELEVENLABS"}, d.TTSVendors())
}

func TestBuildSelectsVendorBySwitch(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*provider.RouterConfig)
		stt, llm provider.Name
	}{
		{"elevenlabs stt", func(c *provider.RouterConfig) { c.OpenAISTTEnabled, c.ElevenLabsSTTEnabled = false, true }, provider.ElevenLabs, provider.OpenAI},
		{"cartesia stt", func(c *provider.RouterConfig) { c.OpenAISTTEnabled, c.CartesiaSTTEnabled = false, true }, provider.Cartesia, provider.OpenAI},
		{"deepseek llm", func(c *provider.RouterConfig) { c.OpenAILLMEnabled, c.DeepSeekLLMEnabled = false, true }, provider.OpenAI, provider.DeepSeek},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := realConfig(t)
			tc.mutate(&cfg)
			// Only the selected adapters are built: a vendor that is not selected needs no key.
			if tc.stt != provider.ElevenLabs {
				cfg.ElevenLabsAPIKey = ""
				cfg.ElevenLabsTTSEnabled = false
			}
			if tc.llm != provider.DeepSeek {
				cfg.DeepSeekAPIKey = ""
			}
			d, err := providerwire.Build(cfg, allCaps, slog.Default())
			require.NoError(t, err)
			require.Equal(t, tc.stt, d.STT.Name())
			require.Equal(t, tc.llm, d.LLM.Name())
			require.Equal(t, tc.llm, d.Evaluation.Name(), "Evaluation follows the LLM vendor")
		})
	}
}

func TestBuildDoesNotNeedKeysOfUnselectedVendors(t *testing.T) {
	cfg := realConfig(t)
	cfg.DeepSeekAPIKey = "" // DeepSeek is not enabled
	_, err := providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	cfg = realConfig(t)
	cfg.OpenAIAPIKey = ""
	_, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.Error(t, err, "an enabled vendor without a key cannot be built")
	require.NotContains(t, err.Error(), "sk-")
}

func TestBuildRejectsNotExactlyOneSTT(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAISTTEnabled = false
	_, err := providerwire.Build(cfg, allCaps, slog.Default())
	require.ErrorContains(t, err, "OPENAI_STT_ENABLED")
	require.ErrorContains(t, err, "ELEVENLABS_STT_ENABLED")
	require.ErrorContains(t, err, "CARTESIA_STT_ENABLED")
	cfg = realConfig(t)
	cfg.CartesiaSTTEnabled = true
	_, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.ErrorContains(t, err, "OPENAI_STT_ENABLED")
}

func TestBuildRejectsNotExactlyOneLLM(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {true, true}} {
		cfg := realConfig(t)
		cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = flags[0], flags[1]
		_, err := providerwire.Build(cfg, allCaps, slog.Default())
		require.ErrorContains(t, err, "OPENAI_LLM_ENABLED")
		require.ErrorContains(t, err, "DEEPSEEK_LLM_ENABLED")
	}
}

func TestActiveVendor(t *testing.T) {
	cfg := realConfig(t)
	for cp, want := range map[provider.Capability]provider.Name{
		provider.CapSTT: provider.OpenAI, provider.CapLLM: provider.OpenAI,
		provider.CapEvaluation: provider.OpenAI, provider.CapEmbedding: provider.OpenAI,
	} {
		got, err := providerwire.ActiveVendor(cfg, cp)
		require.NoError(t, err, cp)
		require.Equal(t, want, got, cp)
	}
	cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = false, true
	got, err := providerwire.ActiveVendor(cfg, provider.CapEvaluation)
	require.NoError(t, err)
	require.Equal(t, provider.DeepSeek, got, "EVALUATION follows LLM")
	got, err = providerwire.ActiveVendor(cfg, provider.CapEmbedding)
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, got, "EMBEDDING is always OPENAI")
	_, err = providerwire.ActiveVendor(cfg, provider.CapTTS)
	require.Error(t, err, "TTS has no single active vendor")
}

func TestBuildSkipsFakeCapabilities(t *testing.T) {
	cfg := realConfig(t)
	cfg.ModeTTS = "fake"
	cfg.ElevenLabsTTSEnabled, cfg.CartesiaTTSEnabled = false, false // not validated for a fake capability
	d, err := providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Empty(t, d.TTS)
	require.Empty(t, d.TTSVendors())
	require.NotNil(t, d.STT)

	cfg = realConfig(t)
	cfg.ModeSTT = "fake"
	cfg.OpenAISTTEnabled = false
	d, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Nil(t, d.STT)
}

func TestBuildOnlyRequestedCapabilities(t *testing.T) {
	cfg := realConfig(t)
	d, err := providerwire.Build(cfg, []provider.Capability{provider.CapEmbedding}, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, d.Embedding)
	require.Nil(t, d.STT)
	require.Nil(t, d.LLM)
	require.Nil(t, d.Evaluation)
	require.Empty(t, d.TTS)
}

func TestBuildAllEnabledTTSAdapters(t *testing.T) {
	cfg := realConfig(t)
	d, err := providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Len(t, d.TTS, 2)
	require.Equal(t, provider.ElevenLabs, d.TTS[provider.ElevenLabs].Name())
	require.Equal(t, provider.Cartesia, d.TTS[provider.Cartesia].Name())

	cfg.CartesiaTTSEnabled = false
	d, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Equal(t, []string{"ELEVENLABS"}, d.TTSVendors())
	require.NotContains(t, d.TTS, provider.Cartesia)

	cfg = realConfig(t)
	cfg.ElevenLabsTTSEnabled = false
	d, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Equal(t, []string{"CARTESIA"}, d.TTSVendors())

	cfg.CartesiaTTSEnabled = false
	_, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.ErrorContains(t, err, "ELEVENLABS_TTS_ENABLED")
	require.ErrorContains(t, err, "CARTESIA_TTS_ENABLED")
}

func TestBuildCartesiaPoolSizeFromConcurrency(t *testing.T) {
	cfg := realConfig(t)
	cfg.Concurrency.CartesiaTTS = 3
	d, err := providerwire.Build(cfg, []provider.Capability{provider.CapTTS}, slog.Default())
	require.NoError(t, err)
	require.Equal(t, 3, d.TTS[provider.Cartesia].(interface{ PoolSize() int }).PoolSize())
}

func TestBuildEmbeddingAlwaysOpenAI(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = false, true
	d, err := providerwire.Build(cfg, allCaps, slog.Default())
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, d.Embedding.Name())

	cfg = realConfig(t)
	cfg.OpenAIEmbeddingEnabled = false
	_, err = providerwire.Build(cfg, allCaps, slog.Default())
	require.ErrorContains(t, err, "OPENAI_EMBEDDING_ENABLED")
}

func TestBuildLLMImpliesEvaluation(t *testing.T) {
	cfg := realConfig(t)
	d, err := providerwire.Build(cfg, []provider.Capability{provider.CapLLM}, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, d.LLM)
	require.NotNil(t, d.Evaluation, "the router needs EVALUATION with the same vendor as LLM")
}

func TestBuildErrorNeverLeaksKey(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAILLMModel = ""
	cfg.OpenAISTTModel = ""
	_, err := providerwire.Build(cfg, allCaps, slog.Default())
	if err != nil {
		require.NotContains(t, err.Error(), "sk-test-openai")
	}
}

func TestNoLegacyVendorPackages(t *testing.T) {
	src, err := os.ReadFile("build.go")
	require.NoError(t, err)
	require.False(t, regexp.MustCompile(`provider/(deepgram|gemini)`).Match(src))
	require.NotContains(t, string(src), "openai.NewTTS")
}

func TestBuildEvaluationAloneBuildsEvaluation(t *testing.T) {
	d, err := providerwire.Build(realConfig(t), []provider.Capability{provider.CapEvaluation}, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, d.Evaluation)
}
