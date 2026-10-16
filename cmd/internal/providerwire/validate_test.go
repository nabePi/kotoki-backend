package providerwire_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func TestValidateRealAcceptsDefaults(t *testing.T) {
	require.NoError(t, providerwire.ValidateReal(realConfig(t), allCaps))
}

func TestValidateRealExactlyOneSTT(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAISTTEnabled = false
	err := providerwire.ValidateReal(cfg, allCaps)
	require.ErrorContains(t, err, "OPENAI_STT_ENABLED")
	require.ErrorContains(t, err, "ELEVENLABS_STT_ENABLED")
	require.ErrorContains(t, err, "CARTESIA_STT_ENABLED")
	cfg = realConfig(t)
	cfg.ElevenLabsSTTEnabled = true
	require.Error(t, providerwire.ValidateReal(cfg, allCaps)) // two true
	cfg = realConfig(t)
	cfg.ModeSTT = "fake"
	cfg.OpenAISTTEnabled = false
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // STT fake: the flags are not validated
	cfg = realConfig(t)
	cfg.OpenAISTTEnabled = false
	require.NoError(t, providerwire.ValidateReal(cfg, []provider.Capability{provider.CapLLM, provider.CapEmbedding}),
		"a capability the process does not use is not validated")
}

func TestValidateRealExactlyOneLLM(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {true, true}} {
		cfg := realConfig(t)
		cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = flags[0], flags[1]
		err := providerwire.ValidateReal(cfg, allCaps)
		require.ErrorContains(t, err, "OPENAI_LLM_ENABLED")
		require.ErrorContains(t, err, "DEEPSEEK_LLM_ENABLED")
	}
	cfg := realConfig(t)
	cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = false, true
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
}

func TestValidateRealTTSAtLeastOne(t *testing.T) {
	cfg := realConfig(t)
	cfg.ElevenLabsTTSEnabled, cfg.CartesiaTTSEnabled = false, false
	err := providerwire.ValidateReal(cfg, allCaps)
	require.ErrorContains(t, err, "ELEVENLABS_TTS_ENABLED")
	require.ErrorContains(t, err, "CARTESIA_TTS_ENABLED")
	cfg.CartesiaTTSEnabled = true
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
	cfg.ElevenLabsTTSEnabled, cfg.CartesiaTTSEnabled = true, false
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
}

func TestValidateRealRejectsMissingKeys(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAIAPIKey = ""
	err := providerwire.ValidateReal(cfg, allCaps)
	require.ErrorContains(t, err, "OPENAI_API_KEY")
	require.NotContains(t, err.Error(), "sk-")

	cfg = realConfig(t)
	cfg.DeepSeekLLMEnabled, cfg.OpenAILLMEnabled = true, false
	cfg.DeepSeekAPIKey = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "DEEPSEEK_API_KEY")

	cfg = realConfig(t)
	cfg.DeepSeekLLMEnabled, cfg.OpenAILLMEnabled, cfg.OpenAISTTEnabled, cfg.ElevenLabsSTTEnabled = true, false, false, true
	cfg.OpenAIAPIKey = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_API_KEY") // embedding still needs OpenAI

	cfg = realConfig(t)
	cfg.ElevenLabsAPIKey = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "ELEVENLABS_API_KEY") // TTS enabled
	cfg = realConfig(t)
	cfg.CartesiaAPIKey = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "CARTESIA_API_KEY")
	cfg = realConfig(t)
	cfg.OpenAISTTEnabled, cfg.CartesiaSTTEnabled = false, true
	cfg.CartesiaTTSEnabled = false
	cfg.CartesiaAPIKey = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "CARTESIA_API_KEY") // STT Cartesia

	cfg = realConfig(t)
	cfg.CartesiaTTSEnabled = false
	cfg.CartesiaAPIKey = ""
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // vendor not enabled: no key
	require.NoError(t, providerwire.ValidateReal(realConfig(t), allCaps))

	cfg = realConfig(t)
	cfg.ModeLLM = "fake"
	cfg.OpenAILLMEnabled = false
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // LLM fake: not validated
}

func TestValidateRealRejectsEmptyModel(t *testing.T) {
	cfg := realConfig(t)
	cfg.OpenAISTTModel = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_STT_MODEL")
	cfg = realConfig(t)
	cfg.OpenAILLMModel = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_LLM_MODEL")
	cfg = realConfig(t)
	cfg.OpenAIEvaluationModel = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_EVALUATION_MODEL")
	cfg = realConfig(t)
	cfg.OpenAIEmbeddingModel = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_EMBEDDING_MODEL")
	cfg = realConfig(t)
	cfg.DeepSeekLLMModel = "" // DeepSeek is not active: may be empty
	cfg.ElevenLabsSTTModel = ""
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
	cfg = realConfig(t)
	cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = false, true
	cfg.DeepSeekEvaluationModel = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "DEEPSEEK_EVALUATION_MODEL")
}

func TestValidateRealCartesiaVersionAndEmbedding(t *testing.T) {
	cfg := realConfig(t)
	cfg.CartesiaVersion = ""
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "CARTESIA_VERSION")
	cfg.CartesiaTTSEnabled = false
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps), "no Cartesia capability enabled")
	cfg = realConfig(t)
	cfg.EmbeddingDimensions = 768
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "EMBEDDING_DIMENSIONS")
	cfg = realConfig(t)
	cfg.OpenAIEmbeddingEnabled = false
	require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_EMBEDDING_ENABLED")
}

func TestValidateRealFakeModeValidatesNothingVendorSpecific(t *testing.T) {
	cfg := realConfig(t)
	cfg.Mode = "fake"
	cfg.OpenAISTTEnabled, cfg.OpenAILLMEnabled = false, false
	cfg.OpenAIAPIKey = ""
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
}

func TestValidateRealOnlyChecksCapabilitiesInMode(t *testing.T) {
	// Cartesia STT enabled but STT is fake: no Cartesia capability is real, so CARTESIA_VERSION is free.
	cfg := realConfig(t)
	cfg.ModeSTT = "fake"
	cfg.OpenAISTTEnabled, cfg.CartesiaSTTEnabled = false, true
	cfg.CartesiaTTSEnabled = false
	cfg.CartesiaVersion = ""
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
	// Cartesia TTS enabled but TTS is fake.
	cfg = realConfig(t)
	cfg.ModeTTS = "fake"
	cfg.CartesiaVersion = ""
	require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
	// Embedding is not a capability of this process: its model and dimensions are not checked.
	cfg = realConfig(t)
	cfg.OpenAIEmbeddingModel, cfg.EmbeddingDimensions = "", 768
	require.NoError(t, providerwire.ValidateReal(cfg, []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS}))
	require.Error(t, providerwire.ValidateReal(cfg, allCaps))
}
