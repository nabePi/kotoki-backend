package bench

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Metric keys shared by the gates, the aggregation and the reports.
const (
	MetricSTTFinalMs      = "stt_final_ms"
	MetricLLMFirstSentMs  = "llm_first_sentence_ms"
	MetricTTSFirstAudioMs = "tts_first_audio_ms"
	MetricE2EMs           = "e2e_turn_end_to_first_audio_ms"
)

// Candidate is one (capability, vendor, model[, voice]) under test, from bench/models.yaml.
type Candidate struct {
	Capability provider.Capability
	Provider   provider.Name
	Model      string
	Voice      string
	// Role is TTS only: PRIMARY (gate of a character's main voice) or FALLBACK (gate of
	// voice.fallback). STT and LLM have a single gate set (D-017) and no role.
	Role string
	// Characters lists the character codes a TTS candidate is proposed for (e.g. HARUKA); empty = all.
	Characters []string
}

// Key identifies the candidate in the human rating sheet: PROVIDER/model[/voice].
func (c Candidate) Key() string {
	k := string(c.Provider) + "/" + c.Model
	if c.Voice != "" {
		k += "/" + c.Voice
	}
	return k
}

// Metrics are the aggregated numbers of one candidate. Rates and CER are fractions (0.08 = 8%). A
// metric that was not measured is absent from P50/P95 or NaN, and a gate on it fails (see EvalGates).
type Metrics struct {
	P50, P95              map[string]float64
	CERNative, CERLearner float64
	ErrorRate             float64 // error fraction at concurrency 100 (all failures, 429 included)
	FormatOK              float64
	QualityMean           float64 // human: mean of naturalness, level, character (1-5)
	MOS                   float64 // human (TTS naturalness 1-5)
	KanjiErrRate          float64 // human: fraction of sentences with a misread kanji
	JSONFirstPassOK       float64
	JSONAfterRepairOK     float64
	CostPerMinUSD         float64
	CostMissing           bool // no price for the candidate: CostPerMinUSD is meaningless
}

// p95 returns the P95 of key, NaN when it was not measured.
func (m Metrics) p95(key string) float64 {
	if v, ok := m.P95[key]; ok {
		return v
	}
	return math.NaN()
}

// Percentile is the nearest-rank percentile (p in 0..100) of xs: the smallest value with at least p%
// of the values at or below it. It does not modify xs. No values: NaN.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[min(max(rank, 1), len(s))-1]
}

// halfwidthKatakana maps U+FF66..U+FF9D to their full-width forms.
const halfwidthKatakana = "ヲァィゥェォャュョッーアイウエオカキクケコサシスセソタチツテトナニヌネノハヒフヘホマミムメモヤユヨラリルレロワン"

// NormalizeForCER prepares a transcript for the character error rate (spec 8.10.3): the NFKC part
// that matters here (full-width ASCII to half-width, half-width katakana to full-width with its
// voicing mark composed; no x/text dependency, D-003), removal of punctuation and white space, and
// katakana to hiragana. Case is kept.
func NormalizeForCER(s string) string {
	var pre strings.Builder
	for _, r := range s {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E: // full-width ASCII
			pre.WriteRune(r - 0xFEE0)
		case r == 0x3000:
			pre.WriteRune(' ')
		case r >= 0xFF66 && r <= 0xFF9D:
			pre.WriteRune([]rune(halfwidthKatakana)[r-0xFF66])
		case r == 0xFF9E:
			pre.WriteRune(0x3099)
		case r == 0xFF9F:
			pre.WriteRune(0x309A)
		default:
			pre.WriteRune(r)
		}
	}
	var b strings.Builder
	for _, r := range provider.NormalizeJA(pre.String()) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		switch {
		case r >= 0x30A1 && r <= 0x30F6, r == 0x30FD, r == 0x30FE: // katakana letters and iteration marks
			r -= 0x60
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Edits is the Levenshtein distance between the normalised ref and hyp, in runes, and the rune
// length of the normalised ref.
func Edits(ref, hyp string) (dist, refLen int) {
	a, b := []rune(NormalizeForCER(ref)), []rune(NormalizeForCER(hyp))
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)], len(a)
}

// CER is the character error rate: Levenshtein distance in runes over the length of the normalised
// reference. An empty reference gives 0 for an empty hypothesis and 1 otherwise.
func CER(ref, hyp string) float64 {
	d, n := Edits(ref, hyp)
	if n == 0 {
		if d == 0 {
			return 0
		}
		return 1
	}
	return float64(d) / float64(n)
}

var markupTag = regexp.MustCompile(`<[^>]*>`)

// FormatOK reports whether an LLM answer is plain Japanese text (spec 8.10.3, D-014): no tags or
// markup, no JSON/brackets/bold/code fences, and at least 90% of the non-space runes Japanese
// (kana, kanji, Japanese punctuation, full-width forms). Empty text fails.
func FormatOK(text string) bool {
	if strings.TrimSpace(text) == "" || markupTag.MatchString(text) {
		return false
	}
	if strings.ContainsAny(text, "{}[]") || strings.Contains(text, "**") || strings.Contains(text, "```") {
		return false
	}
	var ja, total int
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if isJapaneseRune(r) {
			ja++
		}
	}
	return total > 0 && float64(ja)/float64(total) >= 0.9
}

func isJapaneseRune(r rune) bool {
	return unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF) || r == 0x30FC
}

// Usage is the cost model of one minute of conversation (spec 8.10.3).
type Usage struct {
	Turns        int
	STTAudioSec  float64
	InputTokens  int // per turn
	OutputTokens int // per turn
	TTSChars     int // per turn
}

// DefaultUsage is the §8.10.3 assumption: 6 turns, 60 s of STT audio, 400 input + 80 output tokens
// and 60 TTS characters per turn.
func DefaultUsage() Usage {
	return Usage{Turns: 6, STTAudioSec: 60, InputTokens: 400, OutputTokens: 80, TTSChars: 60}
}

// CostPerMinute prices one minute of conversation for the capability of c from the price table. ok is
// false when the table has no price for the combination or the capability is not priced per minute.
func CostPerMinute(p provider.PriceTable, c Candidate, u Usage) (float64, bool) {
	switch c.Capability {
	case provider.CapLLM, provider.CapEvaluation:
		return p.LLM(c.Provider, c.Capability, c.Model, provider.TokenUsage{InputTokens: u.Turns * u.InputTokens, OutputTokens: u.Turns * u.OutputTokens})
	case provider.CapTTS:
		return p.TTS(c.Provider, c.Model, u.Turns*u.TTSChars)
	case provider.CapSTT:
		return p.Audio(c.Provider, c.Capability, c.Model, int64(u.STTAudioSec*1000))
	}
	return 0, false
}
