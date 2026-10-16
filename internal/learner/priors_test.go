package learner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func strp(s string) *string { return &s }

func TestPriorTable(t *testing.T) { // 7 levels x 6 skills = 42 cells
	rows := []struct {
		level           *string
		receptive, prod float64
		confidence      float64
	}{
		{nil, 1.00, 1.00, 0.050},
		{strp("BEGINNER"), 0.50, 0.50, 0.100},
		{strp("N5"), 1.00, 0.50, 0.100},
		{strp("N4"), 2.00, 1.50, 0.100},
		{strp("N3"), 3.00, 2.50, 0.100},
		{strp("N2"), 4.00, 3.50, 0.100},
		{strp("N1"), 4.50, 4.00, 0.100},
	}
	receptive := map[learner.Skill]bool{
		learner.SkillGrammar: true, learner.SkillVocabulary: true, learner.SkillListeningResponse: true,
	}
	cells := 0
	for _, r := range rows {
		for _, k := range learner.AllSkills() {
			want := r.prod
			if receptive[k] {
				want = r.receptive
			}
			got := learner.PriorFor(r.level, k)
			require.Equal(t, want, got.Level, "%v %s", r.level, k)
			require.Equal(t, r.confidence, got.Confidence, "%v %s", r.level, k)
			cells++
		}
	}
	require.Equal(t, 42, cells)
	require.Equal(t, learner.Prior{Level: 2.00, Confidence: 0.100}, learner.PriorFor(strp("N4"), learner.SkillGrammar))
	require.Equal(t, learner.Prior{Level: 1.50, Confidence: 0.100}, learner.PriorFor(strp("N4"), learner.SkillFluency))
	require.Equal(t, learner.Prior{Level: 4.00, Confidence: 0.100}, learner.PriorFor(strp("N1"), learner.SkillInteractionComplexity))
	// An unknown level behaves like no level.
	require.Equal(t, learner.PriorFor(nil, learner.SkillGrammar), learner.PriorFor(strp("N9"), learner.SkillGrammar))
}

func TestEffectiveLevelUsesCeiling(t *testing.T) {
	c := 3.0
	require.InDelta(t, 2.5, learner.EffectiveLevel(2.0, &c), 1e-9)
	require.InDelta(t, 2.8, learner.EffectiveLevel(2.8, &c), 1e-9)
	require.InDelta(t, 2.0, learner.EffectiveLevel(2.0, nil), 1e-9)
}

func TestEffectiveConfidence(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	last := now.AddDate(0, 0, -90)
	require.InDelta(t, 0.4, learner.EffectiveConfidence(0.8, &last, now), 1e-9)
	require.InDelta(t, 0.8, learner.EffectiveConfidence(0.8, nil, now), 1e-9)
	future := now.AddDate(0, 0, 10)
	require.InDelta(t, 0.8, learner.EffectiveConfidence(0.8, &future, now), 1e-9)
}

func TestLabels(t *testing.T) {
	for l, want := range map[float64]string{0: "BEGINNER", 0.49: "BEGINNER", 0.50: "N5", 1.49: "N5", 1.50: "N4", 2.49: "N4",
		2.50: "N3", 3.49: "N3", 3.50: "N2", 4.49: "N2", 4.50: "N1", 5: "N1"} {
		require.Equal(t, want, learner.LevelLabel(l), "%v", l)
	}
	for c, want := range map[float64]string{0: "LOW", 0.29: "LOW", 0.30: "MEDIUM", 0.59: "MEDIUM", 0.60: "HIGH", 0.95: "HIGH"} {
		require.Equal(t, want, learner.ConfidenceLabel(c), "%v", c)
	}
}
