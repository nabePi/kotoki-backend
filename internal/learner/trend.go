package learner

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

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

// slopeEpsilon absorbs float noise so a slope of exactly +-0.05 lands on the threshold side.
const slopeEpsilon = 1e-9

// ComputeTrend is spec 12.5.2. hist holds the earlier estimated levels, oldest first; only those
// with At > newPoint.At - 120 days count, at most the TrendWindow-1 most recent, followed by
// newPoint. nNew is the evidence count after the update.
func ComputeTrend(hist []TrendPoint, newPoint TrendPoint, nNew int) Trend {
	cutoff := newPoint.At.AddDate(0, 0, -TrendMaxAgeDays)
	var kept []TrendPoint
	for _, p := range hist {
		if p.At.After(cutoff) {
			kept = append(kept, p)
		}
	}
	if len(kept) > TrendWindow-1 {
		kept = kept[len(kept)-(TrendWindow-1):]
	}
	ys := make([]float64, 0, len(kept)+1)
	for _, p := range kept {
		ys = append(ys, p.Level)
	}
	ys = append(ys, newPoint.Level)
	if nNew < TrendMinPoints || len(ys) < TrendMinPoints {
		return TrendInsufficient
	}
	b := olsSlope(ys)
	switch {
	case b >= TrendSlope-slopeEpsilon:
		return TrendImproving
	case b <= -TrendSlope+slopeEpsilon:
		return TrendDeclining
	default:
		return TrendStable
	}
}

// olsSlope is the least-squares slope of ys against x = 0..k-1.
func olsSlope(ys []float64) float64 {
	k := float64(len(ys))
	xMean := (k - 1) / 2
	var yMean float64
	for _, y := range ys {
		yMean += y
	}
	yMean /= k
	var num, den float64
	for i, y := range ys {
		dx := float64(i) - xMean
		num += dx * (y - yMean)
		den += dx * dx
	}
	return num / den
}

// Replay recomputes a skill's state from prior and every estimate (spec 12.5.5), ordered by
// (AssessedAt, EvaluationID) whatever the order of ests, which is not modified. Count, ceiling
// and last evidence restart from the prior. rows holds the derived columns per estimate in the
// replay order; final.LastEvidenceAt is the maximum AssessedAt.
func Replay(prior SkillState, ests []Estimate) (final SkillState, rows []Applied) {
	sorted := append([]Estimate(nil), ests...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].AssessedAt.Equal(sorted[j].AssessedAt) {
			return sorted[i].AssessedAt.Before(sorted[j].AssessedAt)
		}
		return uuidLess(sorted[i].EvaluationID, sorted[j].EvaluationID)
	})
	st := SkillState{Level: prior.Level, Confidence: prior.Confidence}
	if len(sorted) == 0 {
		return st, nil
	}
	rows = make([]Applied, 0, len(sorted))
	var hist []TrendPoint
	for _, e := range sorted {
		var a Applied
		st, a = Step(st, e, hist)
		rows = append(rows, a)
		hist = append(hist, TrendPoint{At: e.AssessedAt, Level: a.Level})
	}
	return st, rows
}

func uuidLess(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
