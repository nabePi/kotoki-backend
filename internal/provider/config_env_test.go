package provider_test

import (
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func parseEnv(t *testing.T, environ map[string]string) provider.RouterConfig {
	t.Helper()
	var c provider.RouterConfig
	require.NoError(t, env.ParseWithOptions(&c, env.Options{Environment: environ}))
	return c
}

func TestRouterConfigEnvDefaults(t *testing.T) {
	c := parseEnv(t, map[string]string{})
	require.True(t, c.OpenAISTTEnabled)
	require.False(t, c.ElevenLabsSTTEnabled)
	require.False(t, c.CartesiaSTTEnabled)
	require.True(t, c.OpenAILLMEnabled)
	require.False(t, c.DeepSeekLLMEnabled)
	require.True(t, c.ElevenLabsTTSEnabled && c.CartesiaTTSEnabled)
	require.Equal(t, "gpt-4.1", c.OpenAILLMModel)
	require.Equal(t, "deepseek-flash", c.DeepSeekLLMModel)
	require.Equal(t, "gpt-live-transcribe", c.OpenAISTTModel)
	require.Equal(t, "ink-2", c.CartesiaSTTModel)
	require.Equal(t, 300, c.LLMMaxOutputTokens)
	require.Equal(t, 5, c.CBFailureThreshold)
	require.Equal(t, 30*time.Second, c.CBWindow)
	require.Equal(t, 1500*time.Millisecond, c.TimeoutSTTFinal)
	require.Equal(t, 125, c.Concurrency.Limit(provider.ElevenLabs, provider.CapSTT))
	require.Equal(t, 60, c.Concurrency.Limit(provider.OpenAI, provider.CapLLM))
	require.Equal(t, 80, c.Concurrency.Limit(provider.Cartesia, provider.CapTTS))
	require.Equal(t, "2026-08-14", c.CartesiaVersion)
	require.Equal(t, provider.ModeReal, c.Mode)
	require.True(t, c.TTSFallbackEnabled)
	require.Equal(t, 15*time.Second, c.SessionGrace)
}

func TestRouterConfigEnvOverrides(t *testing.T) {
	c := parseEnv(t, map[string]string{
		"PROVIDER_CB_WINDOW": "10s", "CARTESIA_STT_ENABLED": "true", "OPENAI_STT_ENABLED": "false",
		"PROVIDER_CONCURRENCY_OPENAI_LLM": "12", "OPENAI_API_KEY": "sk-x",
	})
	require.Equal(t, 10*time.Second, c.CBWindow)
	n, err := c.ActiveSTT()
	require.NoError(t, err)
	require.Equal(t, provider.Cartesia, n)
	require.Equal(t, 12, c.Concurrency.OpenAILLM)
	require.Equal(t, "sk-x", c.OpenAIAPIKey.Reveal())
}

func TestRouterConfigEnvRejectsInvalidBool(t *testing.T) {
	var c provider.RouterConfig
	err := env.ParseWithOptions(&c, env.Options{Environment: map[string]string{"OPENAI_STT_ENABLED": "yes"}})
	require.Error(t, err)
	require.ErrorContains(t, err, "OpenAISTTEnabled")
}

func TestUnknownVendorSelectionEnvIgnored(t *testing.T) {
	c := parseEnv(t, map[string]string{"PROVIDER_STT_PRIMARY": "ELEVENLABS", "PROVIDER_TTS_PRIMARY": "CARTESIA", "PROVIDER_PROFILE_X_TTS_PRIMARY": "Y"})
	n, err := c.ActiveSTT()
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, n, "the removed selection variables have no effect")
}
