package learner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func intp(v int) *int { return &v }

func TestProficiencyGolden_Grammar(t *testing.T) { // spec 12.5.4, N4: L=2.00 c=0.100 n=0
	st := learner.SkillState{Level: 2.00, Confidence: 0.100}
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	e1 := learner.Estimate{AssessedAt: t0, RawScore: 82, DEff: intp(2)}
	e1.LObs, e1.CObs = learner.EvidenceLevel(learner.SkillGrammar, 82, 2), learner.EvidenceConfidence(0.80, 1.0)
	require.InDelta(t, 2.40, e1.LObs, 1e-9)
	require.InDelta(t, 0.800, e1.CObs, 1e-9)
	st, a := learner.Step(st, e1, nil)
	require.Equal(t, 2.16, st.Level)
	require.Equal(t, 0.280, a.Confidence)
	require.Equal(t, 0.280, st.Confidence)
	require.Equal(t, 1, st.Count)
	require.Equal(t, learner.TrendInsufficient, a.Trend)
	require.Equal(t, 2.00, a.Previous)
	require.Equal(t, t0, *st.LastEvidenceAt)

	e2 := learner.Estimate{AssessedAt: t0.AddDate(0, 0, 3), RawScore: 91, DEff: intp(2)}
	e2.LObs, e2.CObs = learner.EvidenceLevel(learner.SkillGrammar, 91, 2), learner.EvidenceConfidence(0.85, 0.75)
	require.InDelta(t, 2.70, e2.LObs, 1e-9)
	require.InDelta(t, 0.638, e2.CObs, 1e-9)
	st, a = learner.Step(st, e2, []learner.TrendPoint{{At: t0, Level: 2.16}})
	require.Equal(t, 2.27, st.Level)
	require.Equal(t, 0.389, a.Confidence)
	require.Equal(t, 2, a.Count)
	require.Equal(t, learner.TrendInsufficient, a.Trend)

	e3 := learner.Estimate{AssessedAt: t0.AddDate(0, 0, 7), RawScore: 58, DEff: intp(3)}
	e3.LObs, e3.CObs = learner.EvidenceLevel(learner.SkillGrammar, 58, 3), learner.EvidenceConfidence(0.80, 1.0)
	require.InDelta(t, 2.66, e3.LObs, 1e-9)
	require.InDelta(t, 0.80, e3.CObs, 1e-9)
	st, a = learner.Step(st, e3, []learner.TrendPoint{{At: t0, Level: 2.16}, {At: e2.AssessedAt, Level: 2.27}})
	require.Equal(t, 2.35, st.Level)
	require.Equal(t, 0.502, a.Confidence)
	require.Equal(t, 3, st.Count)
	require.Equal(t, 2.27, a.Previous)
	require.NotNil(t, st.Ceiling)
	require.Equal(t, 2.35, *st.Ceiling)
	require.Equal(t, 2.35, a.Ceiling)
	require.Equal(t, e3.AssessedAt, *st.LastEvidenceAt)
}

// The IMPROVING assertion of the golden sequence needs the OLS trend of Task 3.
func TestProficiencyGolden_GrammarTrendImproving(t *testing.T) {
	t.Skip("task 3")
}

func TestStepRiseCap(t *testing.T) { // alpha 0.50, raw delta 0.50 -> +0.40
	st, a := learner.Step(learner.SkillState{Level: 2.00, Confidence: 0.1}, learner.Estimate{AssessedAt: time.Now(), LObs: 3.00, CObs: 1.0}, nil)
	require.Equal(t, 2.40, st.Level)
	require.Equal(t, 2.40, a.Level)
}

func TestStepDropCap(t *testing.T) { // alpha 0.50, raw delta -2.00 -> -0.30
	st, _ := learner.Step(learner.SkillState{Level: 4.00, Confidence: 0.1}, learner.Estimate{AssessedAt: time.Now(), LObs: 0.0, CObs: 1.0}, nil)
	require.Equal(t, 3.70, st.Level)
}

func TestStepConfidencePenalty(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	ceil := 3.6
	st := learner.SkillState{Level: 3.50, Confidence: 0.70, Count: 6, Ceiling: &ceil, LastEvidenceAt: &now}
	next, a := learner.Step(st, learner.Estimate{AssessedAt: now, LObs: 2.20, CObs: 0.80}, nil)
	require.Equal(t, 3.34, next.Level)
	require.Equal(t, 0.676, a.Confidence)
	require.Equal(t, 7, next.Count)
	require.Equal(t, 3.6, *next.Ceiling, "ceiling never drops")
	require.Equal(t, 3.5, a.Previous)

	// A gap of exactly 1.0 is not "very contradictory": no penalty, only gain.
	next, a = learner.Step(st, learner.Estimate{AssessedAt: now, LObs: 2.50, CObs: 0.80}, nil)
	require.Equal(t, 0.760, a.Confidence) // 0.70 + 0.25*0.80*0.30
	require.Equal(t, 3.38, next.Level)    // alpha 0.15*0.80 = 0.12; delta = -0.12
}

func TestEvidenceLevelBand(t *testing.T) {
	require.InDelta(t, 3.00, learner.EvidenceLevel(learner.SkillGrammar, 100, 2), 1e-9) // upper band D+1
	require.InDelta(t, 0.00, learner.EvidenceLevel(learner.SkillGrammar, 0, 2), 1e-9)   // lower band max(0, D-2)
	require.InDelta(t, 2.00, learner.EvidenceLevel(learner.SkillGrammar, 70, 2), 1e-9)
	require.InDelta(t, 5.00, learner.EvidenceLevel(learner.SkillGrammar, 100, 5), 1e-9)
	require.InDelta(t, 3.00, learner.EvidenceLevel(learner.SkillGrammar, 130, 2), 1e-9) // out-of-range score still clamps
	require.InDelta(t, 1.00, learner.EvidenceLevel(learner.SkillFluency, 0, 3), 1e-9)   // D-2
	require.InDelta(t, 2.66, learner.EvidenceLevel(learner.SkillFluency, 58, 3), 1e-9)
	require.InDelta(t, 2.97, learner.EvidenceLevel(learner.SkillFluency, 99, 2), 1e-9)
	// Both skills share the formula.
	for _, k := range learner.AllSkills() {
		require.InDelta(t, 2.40, learner.EvidenceLevel(k, 82, 2), 1e-9)
	}
}

func TestEvidenceConfidenceRounding(t *testing.T) {
	require.Equal(t, 0.638, learner.EvidenceConfidence(0.85, 0.75)) // 0.6375 rounds half up
	require.Equal(t, 0.560, learner.EvidenceConfidence(0.80, 0.70))
	require.Equal(t, 0.050, learner.EvidenceConfidence(0.50, 0.10))
}

func TestStepSkipsWhenCObsBelow005(t *testing.T) {
	require.True(t, learner.ShouldSkip(0.049))
	require.False(t, learner.ShouldSkip(0.05))
	require.False(t, learner.ShouldSkip(0.8))
}

func TestStepDecaysConfidenceByHalfLife(t *testing.T) {
	last := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := last.AddDate(0, 0, 90)
	st := learner.SkillState{Level: 3.0, Confidence: 0.80, Count: 2, LastEvidenceAt: &last}
	next, a := learner.Step(st, learner.Estimate{AssessedAt: now, LObs: 3.0, CObs: 0.40}, nil)
	// c_old 0.80 x 0.5 = 0.40; gain 0.25*0.40*0.60 = 0.06
	require.Equal(t, 0.460, a.Confidence)
	require.Equal(t, 3.0, next.Level)

	// Without a previous evidence date nothing decays.
	_, a = learner.Step(learner.SkillState{Level: 3.0, Confidence: 0.80}, learner.Estimate{AssessedAt: now, LObs: 3.0, CObs: 0.40}, nil)
	require.Equal(t, 0.820, a.Confidence) // 0.80 + 0.25*0.40*0.20

	// An estimate older than the last evidence (replay tie) is not decayed by negative days.
	later := now.AddDate(0, 0, 10)
	st.LastEvidenceAt = &later
	_, a = learner.Step(st, learner.Estimate{AssessedAt: now, LObs: 3.0, CObs: 0.40}, nil)
	require.Equal(t, 0.820, a.Confidence) // days clamp to 0: no decay; 0.80 + 0.25*0.40*0.20
}

func TestStepBoundsAndFloors(t *testing.T) {
	now := time.Now()
	// Level never leaves 0..5.
	st, _ := learner.Step(learner.SkillState{Level: 0.40, Confidence: 0.5}, learner.Estimate{AssessedAt: now, LObs: 0, CObs: 1}, nil)
	require.Equal(t, 0.20, st.Level)
	st, _ = learner.Step(learner.SkillState{Level: 4.90, Confidence: 0.5}, learner.Estimate{AssessedAt: now, LObs: 5, CObs: 1}, nil)
	require.Equal(t, 4.95, st.Level)
	// Low confidence is not floored: gain 0.25*0.10*0.95 = 0.02375.
	_, a := learner.Step(learner.SkillState{Level: 3, Confidence: 0.05}, learner.Estimate{AssessedAt: now, LObs: 3, CObs: 0.10}, nil)
	require.Equal(t, 0.074, a.Confidence)
	// Alpha floor: n=20 would give 1/22 = 0.045 but the floor is 0.15 -> 0.15*1.0*(3.0-2.0) = 0.15.
	st, _ = learner.Step(learner.SkillState{Level: 2.0, Confidence: 0.5, Count: 20}, learner.Estimate{AssessedAt: now, LObs: 3.0, CObs: 1}, nil)
	require.Equal(t, 2.15, st.Level)
	// Alpha uses 1/(n+2): n=2 -> 0.25.
	st, _ = learner.Step(learner.SkillState{Level: 2.0, Confidence: 0.5, Count: 2}, learner.Estimate{AssessedAt: now, LObs: 3.0, CObs: 1}, nil)
	require.Equal(t, 2.25, st.Level)
	// c_obs scales alpha.
	st, _ = learner.Step(learner.SkillState{Level: 2.0, Confidence: 0.5, Count: 2}, learner.Estimate{AssessedAt: now, LObs: 3.0, CObs: 0.5}, nil)
	require.Equal(t, 2.13, st.Level) // 0.125 -> 2.125 -> 2.13 (half away from zero)
	// Confidence is capped at 0.95.
	_, a = learner.Step(learner.SkillState{Level: 3, Confidence: 0.94}, learner.Estimate{AssessedAt: now, LObs: 3, CObs: 1}, nil)
	require.Equal(t, 0.95, a.Confidence)
	// Confidence is never negative: a huge penalty cannot push it below zero.
	_, a = learner.Step(learner.SkillState{Level: 5, Confidence: 0.95}, learner.Estimate{AssessedAt: now, LObs: 0, CObs: 1}, nil)
	require.GreaterOrEqual(t, a.Confidence, 0.0)
	// A previous ceiling above the new level is kept; below it is replaced.
	hi, lo := 4.0, 1.0
	st, a2 := learner.Step(learner.SkillState{Level: 2, Confidence: 0.5, Ceiling: &hi}, learner.Estimate{AssessedAt: now, LObs: 2, CObs: 1}, nil)
	require.Equal(t, 4.0, *st.Ceiling)
	require.Equal(t, 4.0, a2.Ceiling)
	st, _ = learner.Step(learner.SkillState{Level: 2, Confidence: 0.5, Ceiling: &lo}, learner.Estimate{AssessedAt: now, LObs: 2, CObs: 1}, nil)
	require.Equal(t, 2.0, *st.Ceiling)
	// The input state is not modified.
	in := learner.SkillState{Level: 2, Confidence: 0.5, Ceiling: &hi}
	_, _ = learner.Step(in, learner.Estimate{AssessedAt: now, LObs: 3, CObs: 1}, nil)
	require.Equal(t, 2.0, in.Level)
	require.Equal(t, 4.0, *in.Ceiling)
}
