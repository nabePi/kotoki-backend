package report_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/report"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// priceStub prices with flat rates and records the lookups.
type priceStub struct {
	llmIn, llmOut, ttsPer1M, sttPerHour float64
	noLLM, noTTS, noSTT                 bool
	sttModel                            *string
}

func (p priceStub) LLM(_ provider.Name, c provider.Capability, _ string, u provider.TokenUsage) (float64, bool) {
	if p.noLLM || c != provider.CapLLM {
		return 0, false
	}
	return float64(u.InputTokens)/1e6*p.llmIn + float64(u.OutputTokens)/1e6*p.llmOut, true
}

func (p priceStub) TTS(_ provider.Name, _ string, chars int) (float64, bool) {
	return float64(chars) / 1e6 * p.ttsPer1M, !p.noTTS
}

func (p priceStub) Audio(_ provider.Name, c provider.Capability, model string, ms int64) (float64, bool) {
	if p.sttModel != nil {
		*p.sttModel = model
	}
	return float64(ms) / 3.6e6 * p.sttPerHour, !p.noSTT && c == provider.CapSTT
}

func stack() report.TurnAgg {
	return report.TurnAgg{LLMProvider: "OPENAI", LLMModel: "m", TTSProvider: "ELEVENLABS", TTSModel: "t", SttProvider: "ELEVENLABS",
		Turns: 10, InputTokens: 4000, OutputTokens: 800, TTSCharacters: 600, STTAudioMs: 600000}
}

func TestBuildCost(t *testing.T) {
	rows := report.Build([]report.TurnAgg{stack()}, priceStub{llmIn: 1, llmOut: 2, ttsPer1M: 10, sttPerHour: 0.6})
	require.Len(t, rows, 1)
	total := 4000.0/1e6*1 + 800.0/1e6*2 + 600.0/1e6*10 + 600000.0/3.6e6*0.6
	require.InDelta(t, total/10, rows[0].AvgCostUSDPerTurn, 1e-9)
	require.InDelta(t, total/10, rows[0].CostUSDPerActiveMinute, 1e-9, "600000 ms = 10 active minutes, 10 turns")
	require.Empty(t, rows[0].Unpriced)
}

func TestBuildCostPerActiveMinuteUsesSTTAudio(t *testing.T) {
	a := stack()
	a.STTAudioMs = 120000 // 2 minutes
	rows := report.Build([]report.TurnAgg{a}, priceStub{llmIn: 1, llmOut: 2, ttsPer1M: 10, sttPerHour: 0.6})
	total := 4000.0/1e6*1 + 800.0/1e6*2 + 600.0/1e6*10 + 120000.0/3.6e6*0.6
	require.InDelta(t, total/2, rows[0].CostUSDPerActiveMinute, 1e-9)
	require.InDelta(t, total/10, rows[0].AvgCostUSDPerTurn, 1e-9)
}

func TestBuildNoAudioNoPerMinuteCost(t *testing.T) {
	a := stack()
	a.STTAudioMs = 0
	rows := report.Build([]report.TurnAgg{a}, priceStub{llmIn: 1})
	require.True(t, math.IsNaN(rows[0].CostUSDPerActiveMinute))
	require.False(t, math.IsNaN(rows[0].AvgCostUSDPerTurn))
}

func TestBuildFlagsUnpricedComponents(t *testing.T) {
	rows := report.Build([]report.TurnAgg{stack()}, priceStub{llmIn: 1, noLLM: true, noTTS: true, noSTT: true})
	require.Equal(t, []string{"LLM", "TTS", "STT"}, rows[0].Unpriced)
	require.InDelta(t, 0, rows[0].AvgCostUSDPerTurn, 1e-12, "unpriced counts as 0")
	rows = report.Build([]report.TurnAgg{stack()}, priceStub{noTTS: true})
	require.Equal(t, []string{"TTS"}, rows[0].Unpriced)
}

func TestBuildLooksUpSTTWithoutModel(t *testing.T) {
	var got = "sentinel"
	report.Build([]report.TurnAgg{stack()}, priceStub{sttModel: &got})
	require.Empty(t, got, "session_turns has no stt model: only a wildcard entry can match")
}

func TestBuildKeepsOrderAndStackFields(t *testing.T) {
	a, b := stack(), stack()
	b.LLMModel = "other"
	rows := report.Build([]report.TurnAgg{a, b}, priceStub{})
	require.Equal(t, "m", rows[0].LLMModel)
	require.Equal(t, "other", rows[1].LLMModel)
}

func parse(t *testing.T, raw string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(raw))
	r.Comment = '#'
	recs, err := r.ReadAll()
	require.NoError(t, err)
	return recs
}

func TestCSVColumns(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, report.WriteCSV(&buf, report.Comment("2026-10-17"), nil))
	require.True(t, strings.HasPrefix(buf.String(), "# "), "first line is the comment about the cost approximation")
	require.Contains(t, strings.SplitN(buf.String(), "\n", 2)[0], "sum(stt_audio_ms)/60000")
	require.Contains(t, strings.SplitN(buf.String(), "\n", 2)[0], "2026-10-17")
	recs := parse(t, buf.String())
	require.Len(t, recs, 1)
	require.Equal(t, strings.Split("stt_provider,llm_provider,llm_model,tts_provider,tts_model,turns,"+
		"p50_turn_end_to_first_audio_ms,p95_turn_end_to_first_audio_ms,p99_turn_end_to_first_audio_ms,"+
		"p95_stt_final_ms,p95_llm_first_token_ms,p95_tts_first_audio_ms,failed_rate,interrupted_rate,tts_fallback_rate,"+
		"avg_cost_usd_per_turn,cost_usd_per_active_minute,avg_eval_overall_score,avg_eval_grammar,avg_eval_vocabulary,"+
		"avg_eval_fluency,avg_eval_naturalness,avg_eval_task_achievement", ","), recs[0])
}

func fp(v float64) *float64 { return &v }

func TestCSVValuesAndEmptyCells(t *testing.T) {
	a := stack()
	a.P50TurnEndToFirstAudioMs, a.P95TurnEndToFirstAudioMs, a.P99TurnEndToFirstAudioMs = 400, 812.345, 1200
	a.P95SttFinalMs, a.P95LLMFirstTokenMs, a.P95TTSFirstAudioMs = 150, math.NaN(), 300.06
	a.FailedRate, a.InterruptedRate, a.TTSFallbackRate = 0.1, 0.25, 0
	a.Eval = report.EvalAvg{Overall: fp(70.126), Grammar: fp(72)}
	var buf bytes.Buffer
	require.NoError(t, report.WriteCSV(&buf, "", report.Build([]report.TurnAgg{a}, priceStub{llmIn: 1, llmOut: 2, ttsPer1M: 10, sttPerHour: 0.6})))
	recs := parse(t, buf.String())
	require.Len(t, recs, 2)
	got := map[string]string{}
	for i, c := range recs[0] {
		got[c] = recs[1][i]
	}
	assert.Equal(t, "ELEVENLABS", got["stt_provider"])
	assert.Equal(t, "OPENAI", got["llm_provider"])
	assert.Equal(t, "m", got["llm_model"])
	assert.Equal(t, "t", got["tts_model"])
	assert.Equal(t, "10", got["turns"])
	assert.Equal(t, "400", got["p50_turn_end_to_first_audio_ms"])
	assert.Equal(t, "812.3", got["p95_turn_end_to_first_audio_ms"])
	assert.Equal(t, "1200", got["p99_turn_end_to_first_audio_ms"])
	assert.Equal(t, "150", got["p95_stt_final_ms"])
	assert.Equal(t, "", got["p95_llm_first_token_ms"], "NaN is an empty cell")
	assert.Equal(t, "300.1", got["p95_tts_first_audio_ms"])
	assert.Equal(t, "0.1", got["failed_rate"])
	assert.Equal(t, "0.25", got["interrupted_rate"])
	assert.Equal(t, "0", got["tts_fallback_rate"])
	assert.NotEmpty(t, got["avg_cost_usd_per_turn"])
	assert.Equal(t, "70.13", got["avg_eval_overall_score"])
	assert.Equal(t, "72", got["avg_eval_grammar"])
	assert.Equal(t, "", got["avg_eval_fluency"], "no evaluation is an empty cell, not 0")
}

func TestCSVRejectsMultilineComment(t *testing.T) {
	require.Error(t, report.WriteCSV(&bytes.Buffer{}, "a\nb", nil))
}

func TestWriteJSON(t *testing.T) {
	a := stack()
	a.P95SttFinalMs = math.NaN()
	a.Eval = report.EvalAvg{Overall: fp(70)}
	var buf bytes.Buffer
	require.NoError(t, report.WriteJSON(&buf, report.Build([]report.TurnAgg{a}, priceStub{})))
	var got []map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	require.Len(t, got[0], len(report.Columns))
	require.Equal(t, "OPENAI", got[0]["llm_provider"])
	require.EqualValues(t, 10, got[0]["turns"])
	require.Nil(t, got[0]["p95_stt_final_ms"])
	require.EqualValues(t, 70, got[0]["avg_eval_overall_score"])
	require.Nil(t, got[0]["avg_eval_grammar"])

	buf.Reset()
	require.NoError(t, report.WriteJSON(&buf, nil))
	require.Equal(t, "[]", strings.TrimSpace(buf.String()), "no rows is an empty array, not null")
}
