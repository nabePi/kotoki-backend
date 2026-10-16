package learner

import (
	"math"
	"slices"
)

// Focus selection constants (spec 12.7).
const (
	MaxFocusSkills     = 2
	FocusThreshold     = 0.30
	focusFindingWeight = 0.5
	focusDeclining     = 0.30
	focusImproving     = -0.10
	focusScenarioBonus = 0.2
	focusEpsilon       = 1e-9
)

// FocusInput is the input of FocusSkills. Missing map entries count as 0 / no finding / no trend.
type FocusInput struct {
	Levels, Confidences map[Skill]float64
	Trends              map[Skill]Trend
	Findings            map[Skill]FindingStat
	Evaluations         int
	ScenarioFocus       []Skill
}

// priority is P_k of spec 12.7 for every skill.
func priority(in FocusInput) map[Skill]float64 {
	var sum float64
	for _, k := range allSkills {
		sum += in.Levels[k]
	}
	avg := sum / float64(len(allSkills))

	out := make(map[Skill]float64, len(allSkills))
	for _, k := range allSkills {
		relgap := math.Max(0, avg-in.Levels[k])
		var f float64
		if in.Evaluations > 0 {
			fs := in.Findings[k]
			f = math.Min(1, float64(2*fs.Major+fs.Minor)/float64(3*in.Evaluations))
		}
		var trend float64
		switch in.Trends[k] {
		case TrendDeclining:
			trend = focusDeclining
		case TrendImproving:
			trend = focusImproving
		}
		var focus float64
		if slices.Contains(in.ScenarioFocus, k) {
			focus = 1
		}
		out[k] = in.Confidences[k]*relgap + focusFindingWeight*f + trend + focusScenarioBonus*focus
	}
	return out
}

// FocusSkills returns the (at most 2) skills with the highest priority P_k >= 0.30; ties go to
// the lower level, then to the D-097 skill order. The result is never nil.
func FocusSkills(in FocusInput) []Skill {
	p := priority(in)
	var cands []Skill
	for _, k := range allSkills {
		if p[k] >= FocusThreshold-focusEpsilon {
			cands = append(cands, k)
		}
	}
	slices.SortStableFunc(cands, func(a, b Skill) int {
		if d := p[b] - p[a]; math.Abs(d) > focusEpsilon {
			if d > 0 {
				return 1
			}
			return -1
		}
		switch la, lb := in.Levels[a], in.Levels[b]; {
		case la < lb:
			return -1
		case la > lb:
			return 1
		}
		return 0
	})
	if len(cands) > MaxFocusSkills {
		cands = cands[:MaxFocusSkills]
	}
	if cands == nil {
		return []Skill{}
	}
	return cands
}
