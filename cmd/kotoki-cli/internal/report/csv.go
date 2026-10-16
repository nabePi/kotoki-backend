package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Columns are the fixed CSV columns (spec 8.8.3), in order.
var Columns = []string{
	"stt_provider", "llm_provider", "llm_model", "tts_provider", "tts_model", "turns",
	"p50_turn_end_to_first_audio_ms", "p95_turn_end_to_first_audio_ms", "p99_turn_end_to_first_audio_ms",
	"p95_stt_final_ms", "p95_llm_first_token_ms", "p95_tts_first_audio_ms",
	"failed_rate", "interrupted_rate", "tts_fallback_rate",
	"avg_cost_usd_per_turn", "cost_usd_per_active_minute",
	"avg_eval_overall_score", "avg_eval_grammar", "avg_eval_vocabulary", "avg_eval_fluency",
	"avg_eval_naturalness", "avg_eval_task_achievement",
}

// Comment is the first line of the CSV (a `#` comment, readers need Comment='#'): what the cost
// columns mean. priceVersion is the version of the price table, "" when unknown.
func Comment(priceVersion string) string {
	c := "# cost = LLM tokens + TTS characters + STT audio x price table; active minute = sum(stt_audio_ms)/60000 (approximation); empty = no data"
	if priceVersion != "" {
		c += "; prices version " + priceVersion
	}
	return c
}

func fnum(v float64, digits int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	p := math.Pow10(digits)
	return strconv.FormatFloat(math.Round(v*p)/p, 'f', -1, 64)
}

func fptr(v *float64) string {
	if v == nil {
		return ""
	}
	return fnum(*v, 2)
}

func (r Row) record() []string {
	return []string{
		r.SttProvider, r.LLMProvider, r.LLMModel, r.TTSProvider, r.TTSModel, strconv.Itoa(r.Turns),
		fnum(r.P50TurnEndToFirstAudioMs, 1), fnum(r.P95TurnEndToFirstAudioMs, 1), fnum(r.P99TurnEndToFirstAudioMs, 1),
		fnum(r.P95SttFinalMs, 1), fnum(r.P95LLMFirstTokenMs, 1), fnum(r.P95TTSFirstAudioMs, 1),
		fnum(r.FailedRate, 4), fnum(r.InterruptedRate, 4), fnum(r.TTSFallbackRate, 4),
		fnum(r.AvgCostUSDPerTurn, 6), fnum(r.CostUSDPerActiveMinute, 6),
		fptr(r.Eval.Overall), fptr(r.Eval.Grammar), fptr(r.Eval.Vocabulary), fptr(r.Eval.Fluency),
		fptr(r.Eval.Naturalness), fptr(r.Eval.TaskAchievement),
	}
}

// WriteCSV writes comment (when not empty; one line), the header and one record per row.
func WriteCSV(w io.Writer, comment string, rows []Row) error {
	if comment != "" {
		if strings.ContainsAny(comment, "\r\n") {
			return fmt.Errorf("report: comment must be one line")
		}
		if _, err := io.WriteString(w, comment+"\n"); err != nil {
			return err
		}
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(Columns); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write(r.record()); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteJSON writes the rows as one JSON array of objects keyed like the CSV columns; empty
// values (NaN, no evaluation) are null.
func WriteJSON(w io.Writer, rows []Row) error {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		rec := r.record()
		obj := make(map[string]any, len(Columns))
		for i, c := range Columns {
			switch {
			case i < 5:
				obj[c] = rec[i]
			case rec[i] == "":
				obj[c] = nil
			case c == "turns":
				obj[c] = r.Turns
			default:
				f, err := strconv.ParseFloat(rec[i], 64)
				if err != nil {
					return fmt.Errorf("report: column %s: %w", c, err)
				}
				obj[c] = f
			}
		}
		out = append(out, obj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
