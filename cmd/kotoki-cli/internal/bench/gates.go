package bench

import (
	"math"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Hard gates of spec 8.10.4 (D-041). There is one gate set for STT and LLM because no vendor
// fallback exists (D-017); only TTS has a second set for voice.fallback. The values MUST NOT be
// lowered without a D-100 update.
const (
	GateSTTFinalMs         = 300.0
	GateCERNative          = 0.08
	GateCERLearner         = 0.15
	GateLLMFirstSentMs     = 800.0
	GateLLMFormatOK        = 0.97
	GateLLMQuality         = 4.0
	GateLLMJSONAfterRepair = 0.98
	GateTTSFirstAudioMs    = 400.0
	GateTTSMOS             = 4.0
	GateTTSKanjiErr        = 0.03
	GateTTSFBFirstAudioMs  = 600.0
	GateTTSFBMOS           = 3.5
	GateTTSFBKanjiErr      = 0.05
	GateErrorRateConc100   = 0.01
)

// Roles of a TTS candidate.
const (
	RolePrimary  = "PRIMARY"
	RoleFallback = "FALLBACK"
)

// Gate is the verdict on one metric. Actual is NaN (and Pass false) when the metric was not
// measured: a gate never passes on missing data.
type Gate struct {
	Name   string
	Pass   bool
	Actual float64
	Limit  float64
}

func atMost(name string, actual, limit float64) Gate {
	return Gate{Name: name, Pass: !math.IsNaN(actual) && actual <= limit, Actual: actual, Limit: limit}
}

func atLeast(name string, actual, limit float64) Gate {
	return Gate{Name: name, Pass: !math.IsNaN(actual) && actual >= limit, Actual: actual, Limit: limit}
}

// EvalGates evaluates the gates of the capability on m. role matters only for TTS: FALLBACK uses the
// voice.fallback gates (MOS >= 3.5, misread <= 5%, first audio <= 600 ms), anything else the gates of
// a character's main voice. EVALUATION and EMBEDDING have no benchmark gate. Every capability also
// carries the error rate at concurrency 100 (<= 1%).
func EvalGates(c provider.Capability, role string, m Metrics) []Gate {
	var g []Gate
	switch c {
	case provider.CapSTT:
		g = []Gate{
			atMost(MetricSTTFinalMs, m.p95(MetricSTTFinalMs), GateSTTFinalMs),
			atMost("cer_native", m.CERNative, GateCERNative),
			atMost("cer_learner", m.CERLearner, GateCERLearner),
		}
	case provider.CapLLM:
		g = []Gate{
			atMost(MetricLLMFirstSentMs, m.p95(MetricLLMFirstSentMs), GateLLMFirstSentMs),
			atLeast("format_ok", m.FormatOK, GateLLMFormatOK),
			atLeast("quality_mean", m.QualityMean, GateLLMQuality),
			atLeast("json_after_repair_ok", m.JSONAfterRepairOK, GateLLMJSONAfterRepair),
		}
	case provider.CapTTS:
		if role == RoleFallback {
			g = []Gate{
				atMost(MetricTTSFirstAudioMs, m.p95(MetricTTSFirstAudioMs), GateTTSFBFirstAudioMs),
				atLeast("mos", m.MOS, GateTTSFBMOS),
				atMost("kanji_err_rate", m.KanjiErrRate, GateTTSFBKanjiErr),
			}
		} else {
			g = []Gate{
				atMost(MetricTTSFirstAudioMs, m.p95(MetricTTSFirstAudioMs), GateTTSFirstAudioMs),
				atLeast("mos", m.MOS, GateTTSMOS),
				atMost("kanji_err_rate", m.KanjiErrRate, GateTTSKanjiErr),
			}
		}
	default:
		return nil
	}
	return append(g, atMost("error_rate_conc100", m.ErrorRate, GateErrorRateConc100))
}

// AllPass reports whether every gate passed (and there is at least one).
func AllPass(gates []Gate) bool {
	for _, g := range gates {
		if !g.Pass {
			return false
		}
	}
	return len(gates) > 0
}
