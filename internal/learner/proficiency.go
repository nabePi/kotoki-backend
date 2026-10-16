package learner

import (
	"math"
	"time"

	"github.com/google/uuid"
)

// SkillState is the mutable per-skill state (learner_skill_states).
type SkillState struct {
	Level, Confidence float64
	Count             int
	Ceiling           *float64
	LastEvidenceAt    *time.Time
}

// Estimate is one evaluation's evidence for one skill. LObs and CObs are the rounded values
// (2 and 3 decimals) that are stored in proficiency_estimates, so the normal path and a replay
// read identical inputs.
type Estimate struct {
	EvaluationID uuid.UUID
	AssessedAt   time.Time
	RawScore     int
	DEff         *int
	LObs, CObs   float64
}

// Applied is the derived result of applying one Estimate (the derived columns of
// proficiency_estimates).
type Applied struct {
	Previous, Level, Confidence float64
	Count                       int
	Ceiling                     float64
	Trend                       Trend
}

// Trend is the direction of the recent estimates of a skill.
type Trend string

// Trend values.
const (
	TrendInsufficient Trend = "INSUFFICIENT_DATA"
	TrendImproving    Trend = "IMPROVING"
	TrendStable       Trend = "STABLE"
	TrendDeclining    Trend = "DECLINING"
)

// TrendPoint is one historical estimated level.
type TrendPoint struct {
	At    time.Time
	Level float64
}

// computeTrend is a stub replaced by the OLS implementation of Task 3.
func computeTrend(_ []TrendPoint, _ TrendPoint, _ int) Trend { return TrendInsufficient }

// round2 rounds half away from zero to 2 decimals.
func round2(x float64) float64 { return math.Round(x*100) / 100 }

// round3 rounds half away from zero to 3 decimals.
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// EvidenceLevel is step 1 of spec 12.5.1: the level a score s proves at effective difficulty
// dEff, relative to dEff and clamped to [max(0, dEff-2), min(5, dEff+1)]. All skills share the
// formula; the skill parameter keeps the call sites explicit. The result has 2 decimals.
func EvidenceLevel(_ Skill, score, dEff int) float64 {
	s, d := float64(score), float64(dEff)
	var raw float64
	if s >= PivotScore {
		raw = d + (s-PivotScore)/UpperSpan
	} else {
		raw = d + (s-PivotScore)/LowerSpan
	}
	lo := math.Max(0, d-EvidenceBandBelow)
	hi := math.Min(5, d+EvidenceBandAbove)
	return round2(math.Min(math.Max(raw, lo), hi))
}

// EvidenceConfidence is step 2: c_obs = q_eval x q_sample, rounded to 3 decimals.
func EvidenceConfidence(qEval, qSample float64) float64 { return round3(qEval * qSample) }

// ShouldSkip reports that evidence this weak produces no estimate (c_obs < 0.05).
func ShouldSkip(cObs float64) bool { return cObs < MinEvidenceConfidence }

func clamp(x, lo, hi float64) float64 { return math.Min(math.Max(x, lo), hi) }

// Step applies one estimate to a skill state (spec 12.5.1 steps 3-7). hist holds the earlier
// estimated levels of the skill, oldest first, for the trend.
func Step(st SkillState, e Estimate, hist []TrendPoint) (SkillState, Applied) {
	cOld := st.Confidence
	if st.LastEvidenceAt != nil {
		days := math.Max(0, e.AssessedAt.Sub(*st.LastEvidenceAt).Hours()/24)
		cOld *= math.Pow(0.5, days/ConfHalfLifeDays)
	}

	alpha := math.Max(AlphaFloor, 1/float64(st.Count+2)) * e.CObs
	delta := clamp(alpha*(e.LObs-st.Level), -MaxDropPerEval, MaxRisePerEval)
	lNew := round2(clamp(st.Level+delta, 0, 5))

	gain := ConfGainRate * e.CObs * (1 - cOld)
	var penalty float64
	if math.Abs(e.LObs-st.Level) > ConfPenaltyGap {
		penalty = ConfPenaltyRate * e.CObs * cOld
	}
	cNew := round3(clamp(cOld+gain-penalty, 0, ConfMax))

	ceiling := lNew
	if st.Ceiling != nil && *st.Ceiling > ceiling {
		ceiling = *st.Ceiling
	}
	nNew := st.Count + 1
	at := e.AssessedAt
	next := SkillState{Level: lNew, Confidence: cNew, Count: nNew, Ceiling: &ceiling, LastEvidenceAt: &at}
	return next, Applied{
		Previous:   st.Level,
		Level:      lNew,
		Confidence: cNew,
		Count:      nNew,
		Ceiling:    ceiling,
		Trend:      computeTrend(hist, TrendPoint{At: e.AssessedAt, Level: lNew}, nNew),
	}
}
