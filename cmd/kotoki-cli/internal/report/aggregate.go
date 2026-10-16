// Package report builds `kotoki-cli provider-report` (spec 8.8.3): one row per provider stack
// (STT, LLM + model, TTS + model) with latency percentiles, failure rates, an estimated cost from
// the price table and the evaluation score. It reads Postgres read-only and depends on the
// provider ports, not on a vendor.
package report

import (
	"math"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// EvalAvg are the evaluation score averages (D-096 dimensions) of the sessions whose majority
// stack is this one. A nil value means no completed evaluation with that score: the evaluations
// table is absent (M7 not deployed) or no session of the stack has one. Never an error.
type EvalAvg struct {
	Overall, Grammar, Vocabulary, Fluency, Naturalness, TaskAchievement *float64
}

// TurnAgg is the database aggregate of one stack over the report range. Latency fields are NaN
// when no turn of the stack has the metric (percentile_cont over no value).
type TurnAgg struct {
	SttProvider, LLMProvider, LLMModel, TTSProvider, TTSModel string
	Turns                                                     int
	P50TurnEndToFirstAudioMs, P95TurnEndToFirstAudioMs        float64
	P99TurnEndToFirstAudioMs                                  float64
	P95SttFinalMs, P95LLMFirstTokenMs, P95TTSFirstAudioMs     float64
	FailedRate, InterruptedRate, TTSFallbackRate              float64
	InputTokens, OutputTokens, TTSCharacters, STTAudioMs      int64
	Eval                                                      EvalAvg
}

// Row is one output line: the aggregate plus the estimated cost. Unpriced names the components
// (STT, LLM, TTS) the price table could not price; they count as 0 in the cost.
type Row struct {
	TurnAgg
	AvgCostUSDPerTurn      float64
	CostUSDPerActiveMinute float64
	Unpriced               []string
}

// Build prices every aggregate. Cost of a stack = LLM tokens + TTS characters + STT audio time.
// The active minutes are approximated by the STT audio sent (sum of stt_audio_ms / 60000); the
// cost per active minute is NaN when the stack has no STT audio. The STT model is not stored per
// turn, so STT audio is looked up with an empty model, which only a wildcard price entry matches.
func Build(aggs []TurnAgg, p provider.PriceTable) []Row {
	rows := make([]Row, 0, len(aggs))
	for _, a := range aggs {
		r := Row{TurnAgg: a, CostUSDPerActiveMinute: math.NaN(), AvgCostUSDPerTurn: math.NaN()}
		var total float64
		if usd, ok := p.LLM(provider.Name(a.LLMProvider), provider.CapLLM, a.LLMModel,
			provider.TokenUsage{InputTokens: int(a.InputTokens), OutputTokens: int(a.OutputTokens)}); ok {
			total += usd
		} else {
			r.Unpriced = append(r.Unpriced, "LLM")
		}
		if usd, ok := p.TTS(provider.Name(a.TTSProvider), a.TTSModel, int(a.TTSCharacters)); ok {
			total += usd
		} else {
			r.Unpriced = append(r.Unpriced, "TTS")
		}
		if usd, ok := p.Audio(provider.Name(a.SttProvider), provider.CapSTT, "", a.STTAudioMs); ok {
			total += usd
		} else {
			r.Unpriced = append(r.Unpriced, "STT")
		}
		if a.Turns > 0 {
			r.AvgCostUSDPerTurn = total / float64(a.Turns)
		}
		if a.STTAudioMs > 0 {
			r.CostUSDPerActiveMinute = total / (float64(a.STTAudioMs) / 60000)
		}
		rows = append(rows, r)
	}
	return rows
}
