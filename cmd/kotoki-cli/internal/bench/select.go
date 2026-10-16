package bench

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// NoQualifierNote is the note when no candidate passes the gates: the actual numbers are reported and
// the gates stay as they are (spec 8.10.4 item 4).
const NoQualifierNote = "tidak ada yang lolos, laporkan angka aktual; gate tidak diturunkan"

// ScoredCandidate is a candidate with its measured metrics. Its gates are always evaluated from
// Metrics, never taken from the caller.
type ScoredCandidate struct {
	Candidate
	Metrics Metrics
}

// Gates evaluates the gates of the candidate's capability and role.
func (s ScoredCandidate) Gates() []Gate { return EvalGates(s.Capability, s.Role, s.Metrics) }

// Passed reports whether every gate passes.
func (s ScoredCandidate) Passed() bool { return AllPass(s.Gates()) }

func (s ScoredCandidate) cost() float64 {
	if s.Metrics.CostMissing {
		return math.Inf(1)
	}
	return s.Metrics.CostPerMinUSD
}

func (s ScoredCandidate) ttfa() float64 {
	if v, ok := s.Metrics.P95[MetricTTSFirstAudioMs]; ok {
		return v
	}
	return math.Inf(1)
}

func (s ScoredCandidate) sttCER() float64 { return (s.Metrics.CERNative + s.Metrics.CERLearner) / 2 }

// cmpTail orders by the identity, so that every ranking is deterministic.
func (s ScoredCandidate) cmpTail(o ScoredCandidate) int {
	return strings.Compare(s.Key(), o.Key())
}

// failedNote describes why a candidate did not pass, with the actual numbers.
func failedNote(s ScoredCandidate) string {
	var parts []string
	for _, g := range s.Gates() {
		if !g.Pass {
			parts = append(parts, fmt.Sprintf("%s %s (batas %s)", g.Name, fmtNum(g.Actual), fmtNum(g.Limit)))
		}
	}
	return fmt.Sprintf("%s %s gagal gate: %s", s.Capability, s.Key(), strings.Join(parts, "; "))
}

func fmtNum(v float64) string {
	if math.IsNaN(v) {
		return "tidak terukur"
	}
	return fmt.Sprintf("%.4g", v)
}

func filter(in []ScoredCandidate, c provider.Capability) (passed []ScoredCandidate, notes []string) {
	for _, s := range in {
		if s.Capability != c {
			continue
		}
		if s.Passed() {
			passed = append(passed, s)
		} else {
			notes = append(notes, failedNote(s))
		}
	}
	return passed, notes
}

func switchFlag(s ScoredCandidate) string {
	return fmt.Sprintf("%s_%s_ENABLED=true", s.Provider, s.Capability)
}

// RecommendSTT recommends the STT candidate that passes every gate with the lowest CER (mean of the
// native and learner CER; ties: lower P95 final latency, then identity). It is only a recommendation:
// nothing is selected or written. The other qualifying candidates are returned as manual backups and
// the notes carry their numbers and why the others failed. Nothing qualifies: rec is nil and the notes
// say the gates are not lowered.
func RecommendSTT(scored []ScoredCandidate) (rec *ScoredCandidate, backups []ScoredCandidate, notes []string) {
	passed, notes := filter(scored, provider.CapSTT)
	slices.SortFunc(passed, func(a, b ScoredCandidate) int {
		if c := cmpFloat(a.sttCER(), b.sttCER()); c != 0 {
			return c
		}
		if c := cmpFloat(a.Metrics.p95(MetricSTTFinalMs), b.Metrics.p95(MetricSTTFinalMs)); c != 0 {
			return c
		}
		return a.cmpTail(b)
	})
	if len(passed) == 0 {
		return nil, nil, append(notes, NoQualifierNote)
	}
	rec = &passed[0]
	backups = passed[1:]
	notes = append(notes, fmt.Sprintf("rekomendasi STT: %s (CER native %s, learner %s, final P95 %s ms); keputusan akhir manual", rec.Key(),
		fmtNum(rec.Metrics.CERNative), fmtNum(rec.Metrics.CERLearner), fmtNum(rec.Metrics.p95(MetricSTTFinalMs))))
	for _, b := range backups {
		notes = append(notes, fmt.Sprintf("backup manual: %s (%s; CER native %s, learner %s, final P95 %s ms)", switchFlag(b), b.Key(),
			fmtNum(b.Metrics.CERNative), fmtNum(b.Metrics.CERLearner), fmtNum(b.Metrics.p95(MetricSTTFinalMs))))
	}
	return rec, backups, notes
}

// RecommendLLM recommends the passing LLM candidate with the highest human quality whose cost per
// minute is at most maxCostPerMin (<= 0: 3x the cheapest passing candidate, D-016). Ties: lower cost,
// lower P95 time to first sentence. A candidate without a known price cannot be checked against the
// limit and is not recommended. Passing candidates of another vendor are the manual backups (also
// those over the cost limit, with a note). Nothing is selected automatically.
func RecommendLLM(scored []ScoredCandidate, maxCostPerMin float64) (rec *ScoredCandidate, backups []ScoredCandidate, notes []string) {
	passed, notes := filter(scored, provider.CapLLM)
	if len(passed) == 0 {
		return nil, nil, append(notes, NoQualifierNote)
	}
	limit := maxCostPerMin
	if limit <= 0 {
		cheapest := math.Inf(1)
		for _, p := range passed {
			cheapest = min(cheapest, p.cost())
		}
		limit = 3 * cheapest
		switch {
		case math.IsInf(cheapest, 1):
			notes = append(notes, "harga tidak diketahui untuk semua kandidat yang lolos: batas biaya tidak dapat dihitung")
		case cheapest == 0:
			notes = append(notes, "biaya per menit kandidat termurah 0 (harga belum terisi): batas 3x tidak informatif")
		}
	}
	var eligible []ScoredCandidate
	for _, p := range passed {
		switch {
		case p.Metrics.CostMissing:
			notes = append(notes, fmt.Sprintf("%s tidak punya harga di tabel harga: tidak direkomendasikan", p.Key()))
		case p.cost() > limit*(1+1e-9):
			notes = append(notes, fmt.Sprintf("%s lolos gate tetapi biaya %s USD/menit melebihi batas %s", p.Key(), fmtNum(p.cost()), fmtNum(limit)))
		default:
			eligible = append(eligible, p)
		}
	}
	rank := func(a, b ScoredCandidate) int {
		if c := -cmpFloat(a.Metrics.QualityMean, b.Metrics.QualityMean); c != 0 {
			return c
		}
		if c := cmpFloat(a.cost(), b.cost()); c != 0 {
			return c
		}
		if c := cmpFloat(a.Metrics.p95(MetricLLMFirstSentMs), b.Metrics.p95(MetricLLMFirstSentMs)); c != 0 {
			return c
		}
		return a.cmpTail(b)
	}
	slices.SortFunc(eligible, rank)
	if len(eligible) == 0 {
		return nil, nil, append(notes, "tidak ada kandidat yang lolos gate dengan biaya di bawah batas; gate tidak diturunkan")
	}
	rec = &eligible[0]
	slices.SortFunc(passed, rank)
	for _, p := range passed {
		if p.Provider != rec.Provider {
			backups = append(backups, p)
		}
	}
	notes = append(notes, fmt.Sprintf("rekomendasi LLM: %s (kualitas %s, %s USD/menit, batas %s); keputusan akhir manual", rec.Key(),
		fmtNum(rec.Metrics.QualityMean), fmtNum(rec.cost()), fmtNum(limit)))
	for _, b := range backups {
		notes = append(notes, fmt.Sprintf("backup manual: %s (%s; kualitas %s, %s USD/menit)", switchFlag(b), b.Key(), fmtNum(b.Metrics.QualityMean), fmtNum(b.cost())))
	}
	return rec, backups, notes
}

// CharacterSpec describes a character to pick voices for (from the seed, spec 6.13).
type CharacterSpec struct{ Code, Gender, Accent string }

// VoiceChoice is a vendor voice: the values that go into characters.provider_name/model_name/voice_id
// or voice.fallback. ProviderName is UPPERCASE; ModelName and VoiceID are taken as is.
type VoiceChoice struct{ ProviderName, ModelName, VoiceID string }

// CharacterVoice is the recommended TTS for one character (D-015). Primary is empty when no
// candidate qualifies; Fallback is nil when there is none (a TTS failure then means APOLOGY, D-042).
type CharacterVoice struct {
	Code     string
	Primary  VoiceChoice
	Fallback *VoiceChoice
	Notes    []string
}

func choiceOf(s ScoredCandidate) VoiceChoice {
	return VoiceChoice{ProviderName: string(s.Provider), ModelName: s.Model, VoiceID: s.Voice}
}

func nominates(c Candidate, code string) bool {
	return len(c.Characters) == 0 || slices.Contains(c.Characters, code)
}

// RecommendCharacterVoices recommends, per character, the main voice and the fallback voice (D-015).
// Primary: among TTS candidates with role PRIMARY that nominate the character (no list = all) and pass
// the PRIMARY gates, the highest MOS (ties: lower cost per minute, lower P95 first audio, identity).
// Fallback: the best next TTS candidate that nominates the character, passes the FALLBACK gates and
// belongs to a vendor other than Primary's (D-015); none gives a note that a failure ends in APOLOGY.
// No qualifying primary: empty Primary and a note that the gates are not lowered.
func RecommendCharacterVoices(scored []ScoredCandidate, chars []CharacterSpec) []CharacterVoice {
	var tts []ScoredCandidate
	for _, s := range scored {
		if s.Capability == provider.CapTTS {
			tts = append(tts, s)
		}
	}
	rank := func(a, b ScoredCandidate) int {
		if c := -cmpFloat(a.Metrics.MOS, b.Metrics.MOS); c != 0 {
			return c
		}
		if c := cmpFloat(a.cost(), b.cost()); c != 0 {
			return c
		}
		if c := cmpFloat(a.ttfa(), b.ttfa()); c != 0 {
			return c
		}
		return a.cmpTail(b)
	}
	slices.SortFunc(tts, rank)
	out := make([]CharacterVoice, 0, len(chars))
	for _, ch := range chars {
		cv := CharacterVoice{Code: ch.Code}
		var primary *ScoredCandidate
		for i := range tts {
			s := tts[i]
			if nominates(s.Candidate, ch.Code) && s.Role != RoleFallback && AllPass(EvalGates(provider.CapTTS, RolePrimary, s.Metrics)) {
				primary = &tts[i]
				break
			}
		}
		if primary == nil {
			cv.Notes = append(cv.Notes, NoQualifierNote)
			out = append(out, cv)
			continue
		}
		cv.Primary = choiceOf(*primary)
		for _, s := range tts {
			if s.Provider != primary.Provider && nominates(s.Candidate, ch.Code) && AllPass(EvalGates(provider.CapTTS, RoleFallback, s.Metrics)) {
				fb := choiceOf(s)
				cv.Fallback = &fb
				break
			}
		}
		if cv.Fallback == nil {
			cv.Notes = append(cv.Notes, "tanpa fallback TTS: kegagalan → APOLOGY (D-042)")
		}
		out = append(out, cv)
	}
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
