package provider_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// validRealConfig is the default configuration plus the API keys real mode demands.
func validRealConfig() provider.RouterConfig {
	return provider.RouterConfig{
		Mode:             provider.ModeReal,
		OpenAISTTEnabled: true, OpenAILLMEnabled: true, ElevenLabsTTSEnabled: true, CartesiaTTSEnabled: true, OpenAIEmbeddingEnabled: true,
		OpenAISTTModel: "m", ElevenLabsSTTModel: "m", CartesiaSTTModel: "m", OpenAILLMModel: "m", DeepSeekLLMModel: "m",
		OpenAIEvaluationModel: "m", DeepSeekEvaluationModel: "m", OpenAIEmbeddingModel: "m", EmbeddingDimensions: 1536,
		LLMMaxOutputTokens: 300,
		TimeoutSTTConnect:  time.Second, TimeoutSTTFinal: time.Second, TimeoutLLMFirstToken: time.Second, TimeoutLLMTotal: time.Second,
		TimeoutTTSFirstAudio: time.Second, TimeoutTTSSentenceTotal: time.Second, TimeoutEvaluation: time.Second, TimeoutEmbedding: time.Second,
		SessionGrace: time.Second, CBFailureThreshold: 5, CBWindow: time.Second, CBOpenDuration: time.Second, CBHalfOpenProbes: 1,
		OpenAIAPIKey: "k", DeepSeekAPIKey: "k", ElevenLabsAPIKey: "k", CartesiaAPIKey: "k",
		Concurrency: provider.ConcurrencyConfig{OpenAISTT: 1, ElevenLabsSTT: 1, CartesiaSTT: 1, OpenAILLM: 1, DeepSeekLLM: 1,
			ElevenLabsTTS: 1, CartesiaTTS: 1, OpenAIEvaluation: 1, DeepSeekEvaluation: 1, OpenAIEmbedding: 1},
	}
}

func TestRouterConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*provider.RouterConfig)
		want   string // "" = valid
	}{
		{"valid", func(*provider.RouterConfig) {}, ""},
		{"no STT", func(c *provider.RouterConfig) { c.OpenAISTTEnabled = false }, "exactly one of OPENAI_STT_ENABLED, ELEVENLABS_STT_ENABLED, CARTESIA_STT_ENABLED must be true"},
		{"two STT", func(c *provider.RouterConfig) { c.CartesiaSTTEnabled = true }, "exactly one of OPENAI_STT_ENABLED"},
		{"two LLM", func(c *provider.RouterConfig) { c.DeepSeekLLMEnabled = true }, "OPENAI_LLM_ENABLED, DEEPSEEK_LLM_ENABLED"},
		{"no LLM", func(c *provider.RouterConfig) { c.OpenAILLMEnabled = false }, "OPENAI_LLM_ENABLED, DEEPSEEK_LLM_ENABLED"},
		{"no TTS", func(c *provider.RouterConfig) { c.ElevenLabsTTSEnabled, c.CartesiaTTSEnabled = false, false }, "ELEVENLABS_TTS_ENABLED, CARTESIA_TTS_ENABLED"},
		{"embedding off", func(c *provider.RouterConfig) { c.OpenAIEmbeddingEnabled = false }, "OPENAI_EMBEDDING_ENABLED"},
		{"no openai key", func(c *provider.RouterConfig) { c.OpenAIAPIKey = "" }, "OPENAI_API_KEY"},
		{"deepseek without key", func(c *provider.RouterConfig) {
			c.OpenAILLMEnabled, c.DeepSeekLLMEnabled, c.DeepSeekAPIKey = false, true, ""
		}, "DEEPSEEK_API_KEY"},
		{"elevenlabs tts without key", func(c *provider.RouterConfig) { c.ElevenLabsAPIKey = "" }, "ELEVENLABS_API_KEY"},
		{"cartesia tts without key", func(c *provider.RouterConfig) { c.CartesiaAPIKey = "" }, "CARTESIA_API_KEY"},
		{"elevenlabs stt without key", func(c *provider.RouterConfig) {
			c.OpenAISTTEnabled, c.ElevenLabsSTTEnabled, c.ElevenLabsTTSEnabled, c.ElevenLabsAPIKey = false, true, false, ""
		}, "ELEVENLABS_API_KEY"},
		{"cartesia stt without key", func(c *provider.RouterConfig) {
			c.OpenAISTTEnabled, c.CartesiaSTTEnabled, c.CartesiaTTSEnabled, c.CartesiaAPIKey = false, true, false, ""
		}, "CARTESIA_API_KEY"},
		{"empty stt model", func(c *provider.RouterConfig) { c.OpenAISTTModel = "" }, "OPENAI_STT_MODEL"},
		{"empty llm model", func(c *provider.RouterConfig) { c.OpenAILLMModel = "" }, "OPENAI_LLM_MODEL"},
		{"empty evaluation model", func(c *provider.RouterConfig) { c.OpenAIEvaluationModel = "" }, "OPENAI_EVALUATION_MODEL"},
		{"embedding dimensions", func(c *provider.RouterConfig) { c.EmbeddingDimensions = 1024 }, "EMBEDDING_DIMENSIONS"},
		{"timeout zero", func(c *provider.RouterConfig) { c.TimeoutLLMTotal = 0 }, "PROVIDER_TIMEOUT_LLM_TOTAL"},
		{"concurrency zero", func(c *provider.RouterConfig) { c.Concurrency.OpenAILLM = 0 }, "PROVIDER_CONCURRENCY_OPENAI_LLM"},
		{"bad mode", func(c *provider.RouterConfig) { c.Mode = "mock" }, "PROVIDER_MODE"},
		{"fake STT ignores STT flags and keys", func(c *provider.RouterConfig) {
			c.ModeSTT, c.OpenAISTTEnabled, c.OpenAISTTModel = provider.ModeFake, false, ""
		}, ""},
		{"all fake needs no keys", func(c *provider.RouterConfig) {
			c.Mode, c.OpenAISTTEnabled, c.OpenAILLMEnabled, c.OpenAIEmbeddingEnabled = provider.ModeFake, false, false, false
			c.ElevenLabsTTSEnabled, c.CartesiaTTSEnabled = false, false
			c.OpenAIAPIKey, c.DeepSeekAPIKey, c.ElevenLabsAPIKey, c.CartesiaAPIKey = "", "", "", ""
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validRealConfig()
			tc.mutate(&c)
			err := c.Validate()
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestExactlyOneSTTEnabled(t *testing.T) {
	c := validRealConfig()
	c.OpenAISTTEnabled, c.CartesiaSTTEnabled = false, true
	require.NoError(t, c.Validate())
	n, _ := c.ActiveSTT()
	require.Equal(t, provider.Cartesia, n)
	c.OpenAISTTEnabled = true
	require.ErrorContains(t, c.Validate(), "exactly one of OPENAI_STT_ENABLED, ELEVENLABS_STT_ENABLED, CARTESIA_STT_ENABLED must be true")
}

func TestActiveVendorsAndModels(t *testing.T) {
	c := validRealConfig()
	c.OpenAISTTModel, c.OpenAILLMModel, c.OpenAIEvaluationModel = "stt-o", "llm-o", "eval-o"
	c.DeepSeekLLMModel, c.DeepSeekEvaluationModel = "llm-d", "eval-d"
	require.Equal(t, "stt-o", c.STTModel())
	require.Equal(t, "llm-o", c.LLMModel())
	require.Equal(t, "eval-o", c.EvaluationModel())
	c.OpenAILLMEnabled, c.DeepSeekLLMEnabled = false, true
	n, err := c.ActiveLLM()
	require.NoError(t, err)
	require.Equal(t, provider.DeepSeek, n)
	require.Equal(t, "llm-d", c.LLMModel())
	require.Equal(t, "eval-d", c.EvaluationModel())
	require.Equal(t, []provider.Name{provider.ElevenLabs, provider.Cartesia}, c.TTSVendors())
	c.CartesiaTTSEnabled = false
	require.Equal(t, []provider.Name{provider.ElevenLabs}, c.TTSVendors())
}

func TestModeFor(t *testing.T) {
	c := provider.RouterConfig{Mode: provider.ModeReal, ModeLLM: provider.ModeFake}
	require.Equal(t, provider.ModeFake, c.ModeFor(provider.CapLLM))
	require.Equal(t, provider.ModeFake, c.ModeFor(provider.CapEvaluation), "the LLM override covers EVALUATION")
	require.Equal(t, provider.ModeReal, c.ModeFor(provider.CapSTT))
	c.ModeEvaluation = provider.ModeReal
	require.Equal(t, provider.ModeReal, c.ModeFor(provider.CapEvaluation), "an explicit EVALUATION override wins over the LLM one")
}

func TestConcurrencyLimit(t *testing.T) {
	var c provider.ConcurrencyConfig
	require.Equal(t, 125, c.Limit(provider.FakePrimary, provider.CapSTT), "FAKE_* uses the capability default")
	require.Equal(t, 60, c.Limit(provider.FakePrimary, provider.CapLLM))
	require.Equal(t, 80, c.Limit(provider.FakeFallback, provider.CapTTS))
	require.Equal(t, 8, c.Limit(provider.FakePrimary, provider.CapEvaluation))
	require.Equal(t, 16, c.Limit(provider.FakePrimary, provider.CapEmbedding))
	c.CartesiaTTS = 7
	require.Equal(t, 7, c.Limit(provider.Cartesia, provider.CapTTS))
}

func TestSecretNeverPrints(t *testing.T) {
	c := validRealConfig()
	c.OpenAIAPIKey = "sk-super-secret"
	for _, s := range []string{
		strings.TrimSpace(sprint("%v", c)), sprint("%+v", c), sprint("%#v", c), sprint("%s", c.OpenAIAPIKey),
	} {
		require.NotContains(t, s, "sk-super-secret")
	}
	require.Equal(t, "sk-super-secret", c.OpenAIAPIKey.Reveal())
}

func TestRouterConfigHasNoVendorSelectionOrTTSProfile(t *testing.T) {
	typ := reflect.TypeOf(provider.RouterConfig{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := f.Name
		require.False(t, strings.HasSuffix(name, "Primary") || strings.HasSuffix(name, "Fallback") && name != "TTSFallbackEnabled", name)
		require.NotContains(t, name, "TTSModel")
		require.False(t, strings.HasPrefix(name, "TTSPrimary") || strings.HasPrefix(name, "TTSFallback") && name != "TTSFallbackEnabled", name)
		tag := f.Tag.Get("env")
		require.NotContains(t, tag, "PROVIDER_STT_")
		require.NotContains(t, tag, "PROVIDER_LLM_")
		require.NotContains(t, tag, "PROVIDER_PROFILE_")
		require.NotContains(t, tag, "TTS_PRIMARY")
		require.NotContains(t, tag, "FAKE_PRIMARY")
	}
}
