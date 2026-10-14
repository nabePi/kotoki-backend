package pricing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/pricing"
)

var _ provider.PriceTable = (*pricing.Table)(nil)

func load(t *testing.T, yaml string) (*pricing.Table, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prices.yaml")
	require.NoError(t, os.WriteFile(p, []byte(yaml), 0o600))
	return pricing.Load(p)
}

func TestWildcardAndZeroPrice(t *testing.T) {
	tb, err := pricing.Load("") // embedded; every price is 0
	require.NoError(t, err)
	usd, ok := tb.TTS(provider.Cartesia, "sonic-3", 100) // entry with model "*"
	require.True(t, ok)
	require.Zero(t, usd)
	_, ok = tb.LLM(provider.OpenAI, provider.CapLLM, "model-tak-ada", provider.TokenUsage{})
	require.False(t, ok)
}

func TestEmbeddedTableCoversEveryDefaultModel(t *testing.T) {
	tb, err := pricing.Load("")
	require.NoError(t, err)
	_, ok := tb.LLM(provider.OpenAI, provider.CapLLM, "gpt-4.1", provider.TokenUsage{})
	require.True(t, ok)
	_, ok = tb.LLM(provider.DeepSeek, provider.CapLLM, "deepseek-flash", provider.TokenUsage{})
	require.True(t, ok)
	_, ok = tb.LLM(provider.OpenAI, provider.CapEvaluation, "gpt-4.1", provider.TokenUsage{})
	require.True(t, ok)
	_, ok = tb.LLM(provider.OpenAI, provider.CapEmbedding, "text-embedding-3-small", provider.TokenUsage{})
	require.True(t, ok)
	_, ok = tb.Audio(provider.OpenAI, provider.CapSTT, "gpt-live-transcribe", 1000)
	require.True(t, ok)
	_, ok = tb.TTS(provider.ElevenLabs, "Eleven v3 Conversational", 10)
	require.True(t, ok)
}

const priced = `
version: "t"
currency: USD
entries:
  - {provider: OPENAI,     capability: LLM, model: m1, input_per_1m_tokens: 2, output_per_1m_tokens: 8}
  - {provider: DEEPSEEK,   capability: LLM, model: m2, input_per_1m_tokens: 1, input_cache_hit_per_1m_tokens: 0.1, output_per_1m_tokens: 4}
  - {provider: ELEVENLABS, capability: TTS, model: "*", per_1m_characters: 100}
  - {provider: ELEVENLABS, capability: TTS, model: special, per_1m_characters: 300}
  - {provider: CARTESIA,   capability: TTS, model: "*", per_audio_hour: 36}
  - {provider: OPENAI,     capability: STT, model: s1, per_audio_hour: 3.6}
`

func TestCostFormulas(t *testing.T) {
	tb, err := load(t, priced)
	require.NoError(t, err)

	usd, ok := tb.LLM(provider.OpenAI, provider.CapLLM, "m1", provider.TokenUsage{InputTokens: 1_000_000, OutputTokens: 500_000})
	require.True(t, ok)
	require.InDelta(t, 2+4, usd, 1e-9)

	usd, ok = tb.LLM(provider.DeepSeek, provider.CapLLM, "m2", provider.TokenUsage{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 1_000_000})
	require.True(t, ok)
	require.InDelta(t, 0.6*1+0.4*0.1+4, usd, 1e-9, "cached input uses the cache-hit price")

	usd, ok = tb.TTS(provider.ElevenLabs, "anything", 2_000_000)
	require.True(t, ok)
	require.InDelta(t, 200, usd, 1e-9, "wildcard")
	usd, _ = tb.TTS(provider.ElevenLabs, "special", 1_000_000)
	require.InDelta(t, 300, usd, 1e-9, "an exact model wins over the wildcard")

	usd, ok = tb.Audio(provider.OpenAI, provider.CapSTT, "s1", 3_600_000)
	require.True(t, ok)
	require.InDelta(t, 3.6, usd, 1e-9)

	usd, ok = tb.Audio(provider.Cartesia, provider.CapTTS, "sonic", 1_800_000)
	require.True(t, ok)
	require.InDelta(t, 18, usd, 1e-9, "a TTS entry priced per audio hour")
	_, ok = tb.TTS(provider.Cartesia, "sonic", 100)
	require.False(t, ok, "no per-character price declared for this entry")
}

func TestCachedTokensWithoutCachePriceUseInputPrice(t *testing.T) {
	tb, err := load(t, priced)
	require.NoError(t, err)
	usd, _ := tb.LLM(provider.OpenAI, provider.CapLLM, "m1", provider.TokenUsage{InputTokens: 1_000_000, CachedInputTokens: 1_000_000})
	require.InDelta(t, 2, usd, 1e-9)
}

func TestLoaderRejects(t *testing.T) {
	for name, y := range map[string]string{
		"lowercase provider":   "entries:\n  - {provider: openai, capability: LLM, model: m}\n",
		"lowercase capability": "entries:\n  - {provider: OPENAI, capability: llm, model: m}\n",
		"duplicate":            "entries:\n  - {provider: OPENAI, capability: LLM, model: m}\n  - {provider: OPENAI, capability: LLM, model: m}\n",
		"unknown field":        "entries:\n  - {provider: OPENAI, capability: LLM, model: m, typo_per_1m: 1}\n",
		"empty model":          "entries:\n  - {provider: OPENAI, capability: LLM, model: \"\"}\n",
		"negative price":       "entries:\n  - {provider: OPENAI, capability: LLM, model: m, input_per_1m_tokens: -1}\n",
		"not yaml":             "entries: [",
	} {
		_, err := load(t, y)
		require.Error(t, err, name)
	}
	_, err := pricing.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestMissing(t *testing.T) {
	tb, err := load(t, priced)
	require.NoError(t, err)
	got := tb.Missing([]provider.ModelRef{
		{Provider: provider.OpenAI, Capability: provider.CapLLM, Model: "m1"},         // priced
		{Provider: provider.OpenAI, Capability: provider.CapLLM, Model: "unknown"},    // no entry
		{Provider: provider.ElevenLabs, Capability: provider.CapTTS, Model: "x"},      // wildcard, priced
		{Provider: provider.DeepSeek, Capability: provider.CapEvaluation, Model: "z"}, // no entry
	})
	require.Equal(t, []provider.ModelRef{
		{Provider: provider.OpenAI, Capability: provider.CapLLM, Model: "unknown"},
		{Provider: provider.DeepSeek, Capability: provider.CapEvaluation, Model: "z"},
	}, got)

	zero, err := pricing.Load("")
	require.NoError(t, err)
	got = zero.Missing([]provider.ModelRef{{Provider: provider.OpenAI, Capability: provider.CapLLM, Model: "gpt-4.1"}})
	require.Len(t, got, 1, "an entry whose price is 0 counts as missing")
}
