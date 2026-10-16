package evaluation

import "math"

// Req is how a finding field is constrained for a finding type.
type Req int

// Field requirements.
const (
	ReqRequired Req = iota
	ReqOptional
	ReqNull
)

// FindingSpec is one row of the finding-type table (spec 10.1). For STRENGTH the Dimension is
// empty: the LLM chooses one of the five.
type FindingSpec struct {
	Dimension                    Dimension
	TurnSeq, Original, Corrected Req
}

// AllFindingTypes lists the 16 types in spec order.
var AllFindingTypes = []FindingType{
	FindingParticleError, FindingConjugationError, FindingTenseAspectError, FindingSentenceStructureError,
	FindingWordChoiceError, FindingNonJapaneseWord, FindingCollocationError,
	FindingPolitenessMismatch, FindingUnnaturalExpression,
	FindingExcessiveFiller, FindingFragmentedUtterance, FindingOverlyShortResponse,
	FindingOffTopicResponse, FindingMisunderstoodPrompt, FindingObjectiveNotMet,
	FindingStrength,
}

func errSpec(d Dimension) FindingSpec {
	return FindingSpec{Dimension: d, TurnSeq: ReqRequired, Original: ReqRequired, Corrected: ReqRequired}
}

func softSpec(d Dimension) FindingSpec {
	return FindingSpec{Dimension: d, TurnSeq: ReqRequired, Original: ReqRequired, Corrected: ReqOptional}
}

var specs = map[FindingType]FindingSpec{
	FindingParticleError:          errSpec(DimGrammar),
	FindingConjugationError:       errSpec(DimGrammar),
	FindingTenseAspectError:       errSpec(DimGrammar),
	FindingSentenceStructureError: errSpec(DimGrammar),
	FindingWordChoiceError:        errSpec(DimVocabulary),
	FindingNonJapaneseWord:        errSpec(DimVocabulary),
	FindingCollocationError:       errSpec(DimVocabulary),
	FindingPolitenessMismatch:     errSpec(DimNaturalness),
	FindingUnnaturalExpression:    errSpec(DimNaturalness),
	FindingExcessiveFiller:        softSpec(DimFluency),
	FindingFragmentedUtterance:    softSpec(DimFluency),
	FindingOverlyShortResponse:    softSpec(DimFluency),
	FindingOffTopicResponse:       softSpec(DimTaskAchievement),
	FindingMisunderstoodPrompt:    softSpec(DimTaskAchievement),
	FindingObjectiveNotMet: {
		Dimension: DimTaskAchievement, TurnSeq: ReqNull, Original: ReqNull, Corrected: ReqNull,
	},
	FindingStrength: {TurnSeq: ReqOptional, Original: ReqOptional, Corrected: ReqNull},
}

// SpecOf returns the spec of t; false for an unknown type.
func SpecOf(t FindingType) (FindingSpec, bool) {
	s, ok := specs[t]
	return s, ok
}

// Scores are the per-dimension scores (0–100); nil means the dimension was not scored.
type Scores struct {
	Grammar, Vocabulary, Fluency, Naturalness, TaskAchievement *int
}

// ComputeOverall is round(mean(scores), 2) over the scored dimensions (D-096); nil when none is
// scored (insufficient evidence).
func ComputeOverall(s Scores) *float64 {
	sum, n := 0, 0
	for _, p := range []*int{s.Grammar, s.Vocabulary, s.Fluency, s.Naturalness, s.TaskAchievement} {
		if p != nil {
			sum += *p
			n++
		}
	}
	if n == 0 {
		return nil
	}
	v := math.Round(float64(sum)/float64(n)*100) / 100
	return &v
}

// Stats are the session statistics the eligibility check uses (spec 10.4).
type Stats struct {
	UserTurns         int
	UserSpeechSeconds int
	ActiveSeconds     int
}

// Thresholds are the minimum evidence required to score a session.
type Thresholds struct {
	MinUserTurns, MinSpeechSeconds, MinActiveSeconds int
}

// Insufficient reports whether any statistic is below its minimum.
func (s Stats) Insufficient(t Thresholds) bool {
	return s.UserTurns < t.MinUserTurns || s.UserSpeechSeconds < t.MinSpeechSeconds || s.ActiveSeconds < t.MinActiveSeconds
}

// InsufficientSummary is the fixed summary_feedback of an evaluation without enough evidence.
const InsufficientSummary = "Percakapan ini terlalu singkat untuk dinilai secara akurat. Coba bicara minimal 3 kali dengan total sekitar 1 menit agar kami bisa memberikan umpan balik yang berguna."
