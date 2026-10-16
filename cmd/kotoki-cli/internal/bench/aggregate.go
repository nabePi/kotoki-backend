package bench

import (
	"math"
	"slices"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// ConcurrencyGateStep is the load step the error-rate gate reads (spec 8.10.4: concurrency 100).
const ConcurrencyGateStep = 100

func (r Result) isCandidate(c Candidate) bool {
	return r.Capability == string(c.Capability) && r.Provider == string(c.Provider) && r.Model == c.Model && r.Voice == c.Voice && r.Role == c.Role
}

// CandidatesOf lists the distinct candidates that have rows (E2E stacks excluded), in a stable order.
func CandidatesOf(rs []Result) []Candidate {
	seen := map[string]bool{}
	var out []Candidate
	for _, r := range rs {
		if r.Capability == string(CapE2E) {
			continue
		}
		c := Candidate{Capability: provider.Capability(r.Capability), Provider: provider.Name(r.Provider), Model: r.Model, Voice: r.Voice, Role: r.Role}
		k := candKey(c)
		if !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b Candidate) int { return strings.Compare(candKey(a), candKey(b)) })
	return out
}

func candKey(c Candidate) string {
	return strings.Join([]string{string(c.Capability), string(c.Provider), c.Model, c.Voice, c.Role}, "|")
}

// Aggregate turns the rows of one candidate into Metrics. Rows with an error kind are left out of
// every measurement (they count in the error rate of the concurrency suite and are reported by
// error_pct in the summary); a metric without a single measurement stays absent (P95) or NaN, so its
// gate fails. CER is the corpus rate of its set: total edits over total reference runes.
func Aggregate(c Candidate, rs []Result) Metrics {
	nan := math.NaN()
	m := Metrics{P50: map[string]float64{}, P95: map[string]float64{}, CERNative: nan, CERLearner: nan, ErrorRate: nan, FormatOK: nan,
		QualityMean: nan, MOS: nan, KanjiErrRate: nan, JSONFirstPassOK: nan, JSONAfterRepairOK: nan, CostMissing: true}
	var final, firstSent, firstAudio []float64
	var edN, refN, edL, refL float64
	var fmtOK, fmtN, jFirst, jAfter, jN float64
	for _, r := range rs {
		if !r.isCandidate(c) {
			continue
		}
		switch r.Suite {
		case SuiteHuman:
			setIf(&m.QualityMean, r.Values, "quality_mean")
			setIf(&m.MOS, r.Values, "mos")
			setIf(&m.KanjiErrRate, r.Values, "kanji_err_rate")
			continue
		case SuiteCost:
			if v, ok := r.Values["cost_per_min_usd"]; ok && !r.Flags["cost_missing"] {
				m.CostPerMinUSD, m.CostMissing = v, false
			}
			continue
		case SuiteConcurrency:
			if r.ErrorKind == "" && int(r.Values["step"]) == ConcurrencyGateStep {
				m.ErrorRate = r.Values["error_pct"] / 100
			}
			continue
		}
		if r.ErrorKind != "" {
			continue
		}
		switch r.Suite {
		case SuiteSTT:
			final = append(final, r.Values["final_after_finalize_ms"])
			switch r.Set {
			case SetSTTNative:
				edN += r.Values["cer_edits"]
				refN += r.Values["cer_ref_len"]
			case SetSTTLearner:
				edL += r.Values["cer_edits"]
				refL += r.Values["cer_ref_len"]
			}
		case SuiteLLM:
			firstSent = append(firstSent, r.Values["first_sentence_ms"])
			fmtN++
			if r.Flags["format_ok"] {
				fmtOK++
			}
		case SuiteLLMJSON:
			jN++
			if r.Flags["first_pass_ok"] {
				jFirst++
			}
			if r.Flags["after_repair_ok"] {
				jAfter++
			}
		case SuiteTTS:
			firstAudio = append(firstAudio, r.Values["first_audio_ms"])
		}
	}
	put := func(key string, xs []float64) {
		if len(xs) > 0 {
			m.P50[key], m.P95[key] = Percentile(xs, 50), Percentile(xs, 95)
		}
	}
	put(MetricSTTFinalMs, final)
	put(MetricLLMFirstSentMs, firstSent)
	put(MetricTTSFirstAudioMs, firstAudio)
	if refN > 0 {
		m.CERNative = edN / refN
	}
	if refL > 0 {
		m.CERLearner = edL / refL
	}
	if fmtN > 0 {
		m.FormatOK = fmtOK / fmtN
	}
	if jN > 0 {
		m.JSONFirstPassOK, m.JSONAfterRepairOK = jFirst/jN, jAfter/jN
	}
	return m
}

func setIf(dst *float64, vals map[string]float64, key string) {
	if v, ok := vals[key]; ok {
		*dst = v
	}
}

// ScoreAll aggregates every candidate of rs into a ScoredCandidate.
func ScoreAll(rs []Result) []ScoredCandidate {
	var out []ScoredCandidate
	for _, c := range CandidatesOf(rs) {
		out = append(out, ScoredCandidate{Candidate: c, Metrics: Aggregate(c, rs)})
	}
	return out
}

// CostResult is the SuiteCost row of a candidate: the price of one minute of the §8.10.3 model, or a
// missing-price mark when the price table has none.
func CostResult(c Candidate, prices provider.PriceTable, u Usage) Result {
	r := newResult(SuiteCost, c, Item{}, 1)
	if v, ok := CostPerMinute(prices, c, u); ok {
		r.Values["cost_per_min_usd"] = v
	} else {
		r.Flags["cost_missing"] = true
	}
	return r
}
