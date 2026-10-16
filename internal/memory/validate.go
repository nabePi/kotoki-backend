package memory

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Rejection reasons of the 12 validation rules (spec 11.5.3); the sensitive ones are
// ReasonSensitive* (sensitive.go). They are the closed label set of
// kotoki_memory_candidates_rejected_total.
const (
	ReasonSensitiveFlagged = "SENSITIVE_FLAGGED"
	ReasonKeyNotAllowed    = "KEY_NOT_ALLOWED"
	ReasonAssumption       = "ASSUMPTION"
	ReasonNoUserEvidence   = "NO_USER_EVIDENCE"
	ReasonSingleOccurrence = "SINGLE_OCCURRENCE"
	ReasonInjection        = "INJECTION"
	ReasonNotJapanese      = "NOT_JAPANESE"
	ReasonLength           = "LENGTH"
	ReasonLowConfidence    = "LOW_CONFIDENCE"
	ReasonBatchDuplicate   = "BATCH_DUPLICATE"
	ReasonOverLimit        = "OVER_LIMIT"
)

// Limits of the validation rules.
const (
	MaxCandidates       = 12 // non-episode candidates per extraction
	MaxContentRunes     = 60
	MaxDisplayRunes     = 160
	MaxEpisodeContent   = 150
	MaxEpisodeDisplay   = 300
	DefaultValidDays    = 60
	MaxValidDays        = 120
	minPatternTurns     = 2
	evidenceInferred    = "INFERRED"
	confThresholdFact   = 0.75
	confThresholdPref   = 0.70
	confThresholdLearn  = 0.70
	confThresholdEpisod = 0.60
)

var japaneseRE = regexp.MustCompile(`[\p{Hiragana}\p{Katakana}\p{Han}]`)

// Candidate is a validated memory ready for ApplyCandidate; Embedding is filled by the worker.
type Candidate struct {
	Category     Category
	Key          string
	ContentJA    string
	DisplayID    string
	EvidenceType string
	Confidence   float64
	Importance   float64
	IsTemporary  bool
	ValidDays    *int
	TurnIDs      []uuid.UUID
	Embedding    []float32
}

// ConfidenceThreshold is the minimum confidence to store a candidate of category c (rule 10).
func ConfidenceThreshold(c Category) float64 {
	switch c {
	case CategoryProfileFact:
		return confThresholdFact
	case CategoryPreference:
		return confThresholdPref
	case CategoryLearningPattern:
		return confThresholdLearn
	default:
		return confThresholdEpisod
	}
}

// oneLine trims a text and replaces newlines by spaces (rule 9).
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s))
}

func clamp01(v float64) float64 { return min(1, max(0, v)) }

// evidenceTurns maps source_turn_seqs to the ids of turns that carry user text, de-duplicated and
// ordered by seq (rule 4). turns holds only turns with a non-empty user_text.
func evidenceTurns(seqs []int, turns map[int]uuid.UUID) (ids []uuid.UUID, distinct int) {
	valid := make([]int, 0, len(seqs))
	for _, s := range seqs {
		if _, ok := turns[s]; ok && !slices.Contains(valid, s) {
			valid = append(valid, s)
		}
	}
	slices.Sort(valid)
	for _, s := range valid {
		ids = append(ids, turns[s])
	}
	return ids, len(valid)
}

// textRules applies rules 6-9 to the texts of a candidate and returns the cleaned texts and the
// reason of the first failure ("" when the texts pass). Episode limits differ (episode true).
func textRules(c Category, key, content, display string, episode bool) (cleanContent, cleanDisplay, reason string) {
	if r, hit := CheckSensitive(c, key, content, display); hit {
		return "", "", r
	}
	if CheckInjection(key, content, display) {
		return "", "", ReasonInjection
	}
	if !japaneseRE.MatchString(content) {
		return "", "", ReasonNotJapanese
	}
	maxC, maxD := MaxContentRunes, MaxDisplayRunes
	if episode {
		maxC, maxD = MaxEpisodeContent, MaxEpisodeDisplay
	}
	cleanContent, cleanDisplay = oneLine(content), oneLine(display)
	nc, nd := utf8.RuneCountInString(cleanContent), utf8.RuneCountInString(cleanDisplay)
	if nc > maxC || nd < 1 || nd > maxD { // nc >= 1 holds: the content has a kana or kanji
		return "", "", ReasonLength
	}
	return cleanContent, cleanDisplay, ""
}

// validateMemory applies rules 1-10 in order to one LLM candidate.
func validateMemory(r RawCandidate, turns map[int]uuid.UUID) (Candidate, string) {
	if r.Sensitive { // 1
		return Candidate{}, ReasonSensitiveFlagged
	}
	cat, key := Category(r.Category), NormalizeKey(r.Key)
	if cat == CategoryEpisodic || !KeyAllowed(cat, key) { // 2: episodes are server-made, never LLM memories
		return Candidate{}, ReasonKeyNotAllowed
	}
	if (cat == CategoryProfileFact || cat == CategoryPreference) && r.EvidenceType == evidenceInferred { // 3
		return Candidate{}, ReasonAssumption
	}
	ids, distinct := evidenceTurns(r.SourceTurnSeqs, turns)
	if distinct == 0 { // 4
		return Candidate{}, ReasonNoUserEvidence
	}
	if cat == CategoryLearningPattern && distinct < minPatternTurns { // 5
		return Candidate{}, ReasonSingleOccurrence
	}
	content, display, reason := textRules(cat, key, r.ContentJA, r.DisplayTextID, false) // 6-9
	if reason != "" {
		return Candidate{}, reason
	}
	conf := clamp01(r.Confidence)
	if conf < ConfidenceThreshold(cat) { // 10
		return Candidate{}, ReasonLowConfidence
	}
	c := Candidate{Category: cat, Key: key, ContentJA: content, DisplayID: display, EvidenceType: r.EvidenceType,
		Confidence: conf, Importance: clamp01(r.Importance), IsTemporary: r.IsTemporary, TurnIDs: ids}
	if r.IsTemporary {
		days := DefaultValidDays
		if r.ValidDays != nil {
			days = *r.ValidDays
		}
		days = min(MaxValidDays, max(1, days))
		c.ValidDays = &days
	}
	return c, ""
}

// validateEpisode applies the rules that concern an episode (4, 6-10) and builds its candidate.
func validateEpisode(e RawEpisode, turns map[int]uuid.UUID, sessionID uuid.UUID) (Candidate, string) {
	ids, distinct := evidenceTurns(e.SourceTurnSeqs, turns)
	if distinct == 0 {
		return Candidate{}, ReasonNoUserEvidence
	}
	content, display, reason := textRules(CategoryEpisodic, "", e.ContentJA, e.DisplayTextID, true)
	if reason != "" {
		return Candidate{}, reason
	}
	conf := clamp01(e.Confidence)
	if conf < ConfidenceThreshold(CategoryEpisodic) {
		return Candidate{}, ReasonLowConfidence
	}
	return Candidate{Category: CategoryEpisodic, Key: "episode:" + sessionID.String(), ContentJA: content, DisplayID: display,
		EvidenceType: evidenceInferred, Confidence: conf, Importance: clamp01(e.Importance), TurnIDs: ids}, ""
}

var categoryOrder = map[Category]int{CategoryProfileFact: 0, CategoryPreference: 1, CategoryLearningPattern: 2, CategoryEpisodic: 3}

// ValidateCandidates applies the 12 deterministic rules of spec 11.5.3 to an LLM output. turns maps
// the seq of every turn with a non-empty user text to its id. Every rejected candidate is counted
// on m with its reason (m may be nil). The result is ordered PROFILE_FACT, PREFERENCE,
// LEARNING_PATTERN, EPISODIC and by confidence descending within a category; it holds at most 12
// memories and one episode.
func ValidateCandidates(out ExtractionOutput, turns map[int]uuid.UUID, sessionID uuid.UUID, m *Metrics) []Candidate {
	var valid []Candidate
	for _, r := range out.Memories {
		c, reason := validateMemory(r, turns)
		if reason != "" {
			m.Rejected(reason)
			continue
		}
		valid = append(valid, c)
	}

	valid = dedupBatch(valid, m)    // 11
	if len(valid) > MaxCandidates { // 12
		type scored struct {
			c     Candidate
			score float64
			idx   int
		}
		s := make([]scored, len(valid))
		for i, c := range valid {
			s[i] = scored{c, c.Confidence * c.Importance, i}
		}
		slices.SortStableFunc(s, func(a, b scored) int { return cmp.Compare(b.score, a.score) })
		for range s[MaxCandidates:] {
			m.Rejected(ReasonOverLimit)
		}
		s = s[:MaxCandidates]
		slices.SortFunc(s, func(a, b scored) int { return cmp.Compare(a.idx, b.idx) })
		valid = valid[:0]
		for _, x := range s {
			valid = append(valid, x.c)
		}
	}

	if out.Episode != nil {
		if c, reason := validateEpisode(*out.Episode, turns, sessionID); reason != "" {
			m.Rejected(reason)
		} else {
			valid = append(valid, c)
		}
	}
	slices.SortStableFunc(valid, func(a, b Candidate) int {
		if d := cmp.Compare(categoryOrder[a.Category], categoryOrder[b.Category]); d != 0 {
			return d
		}
		return cmp.Compare(b.Confidence, a.Confidence)
	})
	for _, c := range valid {
		m.Candidate(c.Category)
	}
	return valid
}

// dedupBatch keeps one candidate per (category, key): the highest confidence, then the highest
// importance, then the first (rule 11). Each dropped candidate is counted.
func dedupBatch(in []Candidate, m *Metrics) []Candidate {
	type slot struct {
		cat Category
		key string
	}
	best := map[slot]int{}
	for i, c := range in {
		s := slot{c.Category, c.Key}
		j, seen := best[s]
		if !seen {
			best[s] = i
			continue
		}
		if c.Confidence > in[j].Confidence || (c.Confidence == in[j].Confidence && c.Importance > in[j].Importance) {
			best[s] = i
		}
	}
	out := make([]Candidate, 0, len(best))
	for i, c := range in {
		if best[slot{c.Category, c.Key}] == i {
			out = append(out, c)
		} else {
			m.Rejected(ReasonBatchDuplicate)
		}
	}
	return out
}
