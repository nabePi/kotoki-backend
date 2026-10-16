package bench_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func TestCER(t *testing.T) {
	require.InDelta(t, 0.0, bench.CER("こんにちは。", "コンニチハ"), 1e-9) // katakana to hiragana, punctuation dropped
	require.InDelta(t, 0.25, bench.CER("今日は雨", "今日は雪"), 1e-9)
	require.InDelta(t, 1.0/6, bench.CER("日本語の勉強", "日本語の勉"), 1e-9)
	require.InDelta(t, 0.0, bench.CER("", ""), 1e-9)
	require.InDelta(t, 1.0, bench.CER("。", "あ"), 1e-9) // empty normalised reference, non-empty hypothesis
	require.InDelta(t, 0.5, bench.CER("はい", "は"), 1e-9)
	require.InDelta(t, 1.5, bench.CER("はい", "はいあいう"), 1e-9) // insertions count, CER can exceed 1
}

func TestNormalizeForCER(t *testing.T) {
	for in, want := range map[string]string{
		"こんにちは、世界！":    "こんにちは世界",
		"コンピューター":      "こんぴゅーたー",
		"１２３円":         "123円",
		"ＡＢＣ　ｘｙｚ":      "ABCxyz",
		"ｺﾝﾆﾁﾊ":        "こんにちは",
		"ｶﾞｷﾞｸﾞ":       "がぎぐ",
		"ﾊﾟﾝ":          "ぱん",
		"え っ と\t\n":    "えっと",
		"ヴァイオリン":       "ゔぁいおりん",
		"ヽヾ":           "ゝゞ",
		"〜ね。":          "ね",
		"Hello, World": "HelloWorld",
		"ａ～ｂ":          "a~b",
	} {
		require.Equal(t, want, bench.NormalizeForCER(in), in)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = float64(100 - i) // reversed on purpose
	}
	require.InDelta(t, 95, bench.Percentile(xs, 95), 0)
	require.InDelta(t, 50, bench.Percentile(xs, 50), 0)
	require.InDelta(t, 100, bench.Percentile(xs, 100), 0)
	require.InDelta(t, 1, bench.Percentile(xs, 0), 0)
	require.InDelta(t, 1, bench.Percentile(xs, 1), 0)
	require.InDelta(t, 100, bench.Percentile(xs, 99.5), 0)
	require.InDelta(t, 100, xs[0], 0, "input must not be sorted in place")
}

func TestPercentileSmallAndEmpty(t *testing.T) {
	require.InDelta(t, 7, bench.Percentile([]float64{7}, 95), 0)
	require.InDelta(t, 2, bench.Percentile([]float64{3, 1, 2, 9}, 50), 0)
	require.InDelta(t, 9, bench.Percentile([]float64{3, 1, 2, 9}, 95), 0)
	require.True(t, math.IsNaN(bench.Percentile(nil, 95)))
}

func TestFormatOK(t *testing.T) {
	require.True(t, bench.FormatOK("こんにちは！今日はどんな一日でしたか？"))
	require.True(t, bench.FormatOK("とてもいいですね。今日はJRで東京駅まで行って、友達と会います。")) // a little Latin stays above 0.9
	require.False(t, bench.FormatOK(""))
	require.False(t, bench.FormatOK("   "))
	require.False(t, bench.FormatOK("<speak>こんにちは</speak>"))
	require.False(t, bench.FormatOK("こんにちは<b>"))
	require.False(t, bench.FormatOK("**こんにちは**"))
	require.False(t, bench.FormatOK(`{"text":"こんにちは"}`))
	require.False(t, bench.FormatOK("[こんにちは]"))
	require.False(t, bench.FormatOK("```\nこんにちは\n```"))
	require.False(t, bench.FormatOK("Hello, how are you today?"))
	require.False(t, bench.FormatOK("こんにちは hello world how are you"), "Latin ratio above 10%")
}

func TestFormatOKMarkupInsideLongJapanese(t *testing.T) {
	long := "今日はとても良い天気ですね、公園まで散歩に行って友達と一緒にお昼ご飯を食べましょう。"
	require.True(t, bench.FormatOK(long))
	for _, bad := range []string{"**" + long, long + "{}", "[" + long, long + "]", "```" + long, long + "<br>", "<" + long + ">"} {
		require.False(t, bench.FormatOK(bad), bad)
	}
	// the ratio limit: 17 Japanese runes and 3 Latin letters (85%) fail, 18 and 2 (90%) pass
	require.False(t, bench.FormatOK("あいうえおかきくけこさしすせそたちつabc"))
	require.True(t, bench.FormatOK("あいうえおかきくけこさしすせそたちつてab"))
}

func TestCostPerMinuteUsesAssumptions(t *testing.T) {
	p := stubPrices{llmIn: 1, llmOut: 2, ttsPer1M: 10, sttPerHour: 0.6}
	u := bench.DefaultUsage()
	llm, ok := bench.CostPerMinute(p, bench.Candidate{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "m"}, u)
	require.True(t, ok)
	tts, ok := bench.CostPerMinute(p, bench.Candidate{Capability: provider.CapTTS, Provider: provider.Cartesia, Model: "m"}, u)
	require.True(t, ok)
	stt, ok := bench.CostPerMinute(p, bench.Candidate{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "m"}, u)
	require.True(t, ok)
	require.InDelta(t, 6*(400.0/1e6*1+80.0/1e6*2), llm, 1e-12)
	require.InDelta(t, 6*60.0/1e6*10, tts, 1e-12)
	require.InDelta(t, 60.0/3600*0.6, stt, 1e-12)
	require.InDelta(t, 6*(400.0/1e6*1+80.0/1e6*2)+6*60.0/1e6*10+60.0/3600*0.6, llm+tts+stt, 1e-12)
}

func TestCostPerMinuteUnknownPrice(t *testing.T) {
	_, ok := bench.CostPerMinute(stubPrices{missing: true}, bench.Candidate{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "m"}, bench.DefaultUsage())
	require.False(t, ok)
	_, ok = bench.CostPerMinute(stubPrices{}, bench.Candidate{Capability: provider.CapEmbedding}, bench.DefaultUsage())
	require.False(t, ok)
}

// stubPrices is a linear price table.
type stubPrices struct {
	llmIn, llmOut, ttsPer1M, sttPerHour float64
	missing                             bool
}

func (s stubPrices) LLM(_ provider.Name, _ provider.Capability, _ string, u provider.TokenUsage) (float64, bool) {
	return float64(u.InputTokens)/1e6*s.llmIn + float64(u.OutputTokens)/1e6*s.llmOut, !s.missing
}

func (s stubPrices) TTS(_ provider.Name, _ string, chars int) (float64, bool) {
	return float64(chars) / 1e6 * s.ttsPer1M, !s.missing
}

func (s stubPrices) Audio(_ provider.Name, _ provider.Capability, _ string, ms int64) (float64, bool) {
	return float64(ms) / 3600000 * s.sttPerHour, !s.missing
}
