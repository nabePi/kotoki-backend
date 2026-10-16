package learner

import (
	"math"
	"time"
)

// Prior is the onboarding starting level and confidence of one skill (spec 12.3).
type Prior struct{ Level, Confidence float64 }

type priorRow struct{ receptive, productive, confidence float64 }

// priorTable is keyed by the onboarding level; the empty key is "no level given". Receptive
// skills are GRAMMAR, VOCABULARY and LISTENING_RESPONSE; the rest are productive.
var priorTable = map[string]priorRow{
	"":         {1.00, 1.00, 0.050},
	"BEGINNER": {0.50, 0.50, 0.100},
	"N5":       {1.00, 0.50, 0.100},
	"N4":       {2.00, 1.50, 0.100},
	"N3":       {3.00, 2.50, 0.100},
	"N2":       {4.00, 3.50, 0.100},
	"N1":       {4.50, 4.00, 0.100},
}

func isReceptive(s Skill) bool {
	return s == SkillGrammar || s == SkillVocabulary || s == SkillListeningResponse
}

// PriorFor returns the starting state of skill for an onboarding level (nil or unknown means
// no level given).
func PriorFor(level *string, skill Skill) Prior {
	key := ""
	if level != nil {
		key = *level
	}
	row, ok := priorTable[key]
	if !ok {
		row = priorTable[""]
	}
	if isReceptive(skill) {
		return Prior{Level: row.receptive, Confidence: row.confidence}
	}
	return Prior{Level: row.productive, Confidence: row.confidence}
}

// EffectiveLevel is spec 12.6: a single bad session does not drop the adaptation below
// demonstrated ceiling - 0.5.
func EffectiveLevel(level float64, ceiling *float64) float64 {
	if ceiling != nil {
		return math.Max(level, *ceiling-0.5)
	}
	return level
}

// EffectiveConfidence is spec 12.6: conf x 0.5^(days/90) since the last evidence; nil last
// evidence leaves it unchanged.
func EffectiveConfidence(conf float64, lastEvidence *time.Time, now time.Time) float64 {
	if lastEvidence == nil {
		return conf
	}
	days := math.Max(0, now.Sub(*lastEvidence).Hours()/24)
	return conf * math.Pow(0.5, days/ConfHalfLifeDays)
}

// LevelLabel names a skill level as a JLPT-like band.
func LevelLabel(l float64) string {
	switch {
	case l < 0.50:
		return "BEGINNER"
	case l < 1.50:
		return "N5"
	case l < 2.50:
		return "N4"
	case l < 3.50:
		return "N3"
	case l < 4.50:
		return "N2"
	default:
		return "N1"
	}
}

// ConfidenceLabel buckets a confidence into LOW, MEDIUM or HIGH.
func ConfidenceLabel(c float64) string {
	switch {
	case c < 0.30:
		return "LOW"
	case c < 0.60:
		return "MEDIUM"
	default:
		return "HIGH"
	}
}
