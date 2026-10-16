package bench_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func gateOf(gs []bench.Gate, name string) (bench.Gate, bool) {
	for _, g := range gs {
		if g.Name == name {
			return g, true
		}
	}
	return bench.Gate{}, false
}

func passed(t *testing.T, gs []bench.Gate, name string) bool {
	t.Helper()
	g, ok := gateOf(gs, name)
	require.True(t, ok, "gate %s missing", name)
	return g.Pass
}

func TestGatesSingleSetForSTTAndLLM(t *testing.T) {
	m := bench.Metrics{P95: map[string]float64{"stt_final_ms": 320}, CERNative: 0.07, CERLearner: 0.14, ErrorRate: 0.005}
	g := bench.EvalGates(provider.CapSTT, "", m)
	require.False(t, passed(t, g, "stt_final_ms")) // 320 > 300; STT has no 450 ms fallback gate
	require.True(t, passed(t, g, "cer_native"))
	require.True(t, passed(t, g, "cer_learner"))
	require.True(t, passed(t, g, "error_rate_conc100"))
	// the role changes nothing for STT or LLM
	require.Equal(t, g, bench.EvalGates(provider.CapSTT, "FALLBACK", m))

	gt := bench.EvalGates(provider.CapTTS, "FALLBACK", bench.Metrics{P95: map[string]float64{"tts_first_audio_ms": 550}, MOS: 3.6, KanjiErrRate: 0.04})
	require.True(t, bench.AllPass(gt)) // the voice.fallback gates exist for TTS only
}

func TestGateLimits(t *testing.T) {
	stt := func(mut func(*bench.Metrics)) []bench.Gate {
		m := bench.Metrics{P95: map[string]float64{"stt_final_ms": 300}, CERNative: 0.08, CERLearner: 0.15, ErrorRate: 0.01}
		mut(&m)
		return bench.EvalGates(provider.CapSTT, "", m)
	}
	require.True(t, bench.AllPass(stt(func(*bench.Metrics) {})), "the limits themselves pass")
	require.False(t, passed(t, stt(func(m *bench.Metrics) { m.P95["stt_final_ms"] = 300.1 }), "stt_final_ms"))
	require.False(t, passed(t, stt(func(m *bench.Metrics) { m.CERNative = 0.0801 }), "cer_native"))
	require.False(t, passed(t, stt(func(m *bench.Metrics) { m.CERLearner = 0.1501 }), "cer_learner"))
	require.False(t, passed(t, stt(func(m *bench.Metrics) { m.ErrorRate = 0.0101 }), "error_rate_conc100"))

	llm := func(mut func(*bench.Metrics)) []bench.Gate {
		m := bench.Metrics{P95: map[string]float64{"llm_first_sentence_ms": 800}, FormatOK: 0.97, QualityMean: 4.0, JSONAfterRepairOK: 0.98}
		mut(&m)
		return bench.EvalGates(provider.CapLLM, "", m)
	}
	require.True(t, bench.AllPass(llm(func(*bench.Metrics) {})))
	require.False(t, passed(t, llm(func(m *bench.Metrics) { m.P95["llm_first_sentence_ms"] = 801 }), "llm_first_sentence_ms"))
	require.False(t, passed(t, llm(func(m *bench.Metrics) { m.FormatOK = 0.969 }), "format_ok"))
	require.False(t, passed(t, llm(func(m *bench.Metrics) { m.QualityMean = 3.99 }), "quality_mean"))
	require.False(t, passed(t, llm(func(m *bench.Metrics) { m.JSONAfterRepairOK = 0.979 }), "json_after_repair_ok"))

	tts := func(role string, mut func(*bench.Metrics)) []bench.Gate {
		m := bench.Metrics{P95: map[string]float64{"tts_first_audio_ms": 400}, MOS: 4.0, KanjiErrRate: 0.03}
		mut(&m)
		return bench.EvalGates(provider.CapTTS, role, m)
	}
	require.True(t, bench.AllPass(tts("PRIMARY", func(*bench.Metrics) {})))
	require.False(t, passed(t, tts("PRIMARY", func(m *bench.Metrics) { m.P95["tts_first_audio_ms"] = 401 }), "tts_first_audio_ms"))
	require.False(t, passed(t, tts("PRIMARY", func(m *bench.Metrics) { m.MOS = 3.99 }), "mos"))
	require.False(t, passed(t, tts("PRIMARY", func(m *bench.Metrics) { m.KanjiErrRate = 0.031 }), "kanji_err_rate"))
	require.False(t, passed(t, tts("", func(m *bench.Metrics) { m.MOS = 3.6 }), "mos"), "no role means the main-voice gates")

	// voice.fallback gates: MOS >= 3.5, misread <= 5%, first audio <= 600 ms
	fb := func(mut func(*bench.Metrics)) []bench.Gate {
		m := bench.Metrics{P95: map[string]float64{"tts_first_audio_ms": 600}, MOS: 3.5, KanjiErrRate: 0.05}
		mut(&m)
		return bench.EvalGates(provider.CapTTS, "FALLBACK", m)
	}
	require.True(t, bench.AllPass(fb(func(*bench.Metrics) {})))
	require.False(t, passed(t, fb(func(m *bench.Metrics) { m.P95["tts_first_audio_ms"] = 601 }), "tts_first_audio_ms"))
	require.False(t, passed(t, fb(func(m *bench.Metrics) { m.MOS = 3.49 }), "mos"))
	require.False(t, passed(t, fb(func(m *bench.Metrics) { m.KanjiErrRate = 0.051 }), "kanji_err_rate"))
}

func TestGateReportsActualAndLimit(t *testing.T) {
	g := bench.EvalGates(provider.CapSTT, "", bench.Metrics{P95: map[string]float64{"stt_final_ms": 320}, CERNative: 0.07, CERLearner: 0.14})
	f, _ := gateOf(g, "stt_final_ms")
	require.InDelta(t, 320, f.Actual, 0)
	require.InDelta(t, 300, f.Limit, 0)
	c, _ := gateOf(g, "cer_learner")
	require.InDelta(t, 0.14, c.Actual, 1e-12)
	require.InDelta(t, 0.15, c.Limit, 1e-12)
}

func TestGateNeverPassesOnMissingData(t *testing.T) {
	g := bench.EvalGates(provider.CapSTT, "", bench.Metrics{CERNative: 0.01, CERLearner: 0.01})
	f, _ := gateOf(g, "stt_final_ms")
	require.False(t, f.Pass, "a P95 that was not measured must fail")
	require.True(t, math.IsNaN(f.Actual))
	require.False(t, bench.AllPass(g))

	g = bench.EvalGates(provider.CapSTT, "", bench.Metrics{P95: map[string]float64{"stt_final_ms": 100}, CERNative: math.NaN(), CERLearner: 0.1, ErrorRate: 0})
	require.False(t, passed(t, g, "cer_native"))
	g = bench.EvalGates(provider.CapLLM, "", bench.Metrics{P95: map[string]float64{"llm_first_sentence_ms": 100}, FormatOK: 1, JSONAfterRepairOK: 1})
	require.False(t, passed(t, g, "quality_mean"), "no human rating yet means the quality gate fails")
}

func TestGatesOtherCapabilitiesHaveNone(t *testing.T) {
	require.Empty(t, bench.EvalGates(provider.CapEmbedding, "", bench.Metrics{}))
	require.Empty(t, bench.EvalGates(provider.CapEvaluation, "", bench.Metrics{}))
	require.False(t, bench.AllPass(nil))
}

// TestGateConstantsAreTheSpecValues pins the numbers of 8.10.4: they must not be lowered without D-100.
func TestGateConstantsAreTheSpecValues(t *testing.T) {
	require.InDelta(t, 300, bench.GateSTTFinalMs, 0)
	require.InDelta(t, 0.08, bench.GateCERNative, 0)
	require.InDelta(t, 0.15, bench.GateCERLearner, 0)
	require.InDelta(t, 800, bench.GateLLMFirstSentMs, 0)
	require.InDelta(t, 0.97, bench.GateLLMFormatOK, 0)
	require.InDelta(t, 4.0, bench.GateLLMQuality, 0)
	require.InDelta(t, 0.98, bench.GateLLMJSONAfterRepair, 0)
	require.InDelta(t, 400, bench.GateTTSFirstAudioMs, 0)
	require.InDelta(t, 4.0, bench.GateTTSMOS, 0)
	require.InDelta(t, 0.03, bench.GateTTSKanjiErr, 0)
	require.InDelta(t, 600, bench.GateTTSFBFirstAudioMs, 0)
	require.InDelta(t, 3.5, bench.GateTTSFBMOS, 0)
	require.InDelta(t, 0.05, bench.GateTTSFBKanjiErr, 0)
	require.InDelta(t, 0.01, bench.GateErrorRateConc100, 0)
}
