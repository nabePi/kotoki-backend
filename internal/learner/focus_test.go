package learner_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

// spec 12.7 example: avg = 14.95/6 = 2.492, three evaluations.
func specFocusInput() learner.FocusInput {
	return learner.FocusInput{
		Levels: map[learner.Skill]float64{
			learner.SkillGrammar: 2.35, learner.SkillVocabulary: 3.10, learner.SkillFluency: 2.40,
			learner.SkillNaturalness: 2.20, learner.SkillListeningResponse: 2.90, learner.SkillInteractionComplexity: 2.00,
		},
		Confidences: map[learner.Skill]float64{
			learner.SkillGrammar: 0.50, learner.SkillVocabulary: 0.30, learner.SkillFluency: 0.45,
			learner.SkillNaturalness: 0.50, learner.SkillListeningResponse: 0.30, learner.SkillInteractionComplexity: 0.10,
		},
		Trends:      map[learner.Skill]learner.Trend{learner.SkillGrammar: learner.TrendImproving},
		Findings:    map[learner.Skill]learner.FindingStat{learner.SkillGrammar: {Major: 2, Minor: 4}, learner.SkillNaturalness: {Major: 0, Minor: 3}},
		Evaluations: 3,
	}
}

func TestFocusSkills(t *testing.T) {
	in := specFocusInput()
	p := learner.Priority(in)
	require.InDelta(t, 0.415, p[learner.SkillGrammar], 0.001)
	require.InDelta(t, 0.313, p[learner.SkillNaturalness], 0.001)
	require.InDelta(t, 0.041, p[learner.SkillFluency], 0.001)
	require.InDelta(t, 0.049, p[learner.SkillInteractionComplexity], 0.001)
	require.Equal(t, []learner.Skill{learner.SkillGrammar, learner.SkillNaturalness}, learner.FocusSkills(in))
}

func TestFocusSkillsNoneBelowThreshold(t *testing.T) {
	in := specFocusInput()
	in.Findings, in.Trends = nil, nil
	got := learner.FocusSkills(in)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestFocusSkillsDecliningTrendAddsPoint3(t *testing.T) {
	in := learner.FocusInput{Levels: map[learner.Skill]float64{}, Confidences: map[learner.Skill]float64{},
		Trends: map[learner.Skill]learner.Trend{learner.SkillFluency: learner.TrendDeclining}}
	require.InDelta(t, 0.30, learner.Priority(in)[learner.SkillFluency], 1e-9)
	require.Equal(t, []learner.Skill{learner.SkillFluency}, learner.FocusSkills(in))
}

func TestFocusSkillsScenarioFocusAddsPoint2(t *testing.T) {
	in := specFocusInput()
	base := learner.Priority(in)[learner.SkillFluency]
	in.ScenarioFocus = []learner.Skill{learner.SkillFluency}
	require.InDelta(t, base+0.2, learner.Priority(in)[learner.SkillFluency], 1e-9)
}

func TestFocusSkillsFindingsIgnoredWithoutEvaluations(t *testing.T) {
	in := specFocusInput()
	in.Evaluations = 0
	in.Trends = nil
	require.InDelta(t, 0.5*0.142, learner.Priority(in)[learner.SkillGrammar], 0.001)
}

func TestFocusSkillsCapsFindingFrequencyAtOne(t *testing.T) {
	in := specFocusInput()
	in.Findings = map[learner.Skill]learner.FindingStat{learner.SkillGrammar: {Major: 50}}
	in.Trends = nil
	in.Confidences[learner.SkillGrammar] = 0
	require.InDelta(t, 0.5, learner.Priority(in)[learner.SkillGrammar], 1e-9)
}

func TestFocusSkillsTieLowerLevelWins(t *testing.T) {
	// Three skills with the same P = 0.5 (finding-only); the two lowest levels win.
	in := learner.FocusInput{
		Levels: map[learner.Skill]float64{
			learner.SkillGrammar: 3, learner.SkillVocabulary: 2, learner.SkillFluency: 1, learner.SkillNaturalness: 5,
			learner.SkillListeningResponse: 5, learner.SkillInteractionComplexity: 5,
		},
		Confidences: map[learner.Skill]float64{},
		Findings: map[learner.Skill]learner.FindingStat{
			learner.SkillGrammar: {Major: 3}, learner.SkillVocabulary: {Major: 3}, learner.SkillFluency: {Major: 3},
		},
		Evaluations: 2,
	}
	p := learner.Priority(in)
	require.InDelta(t, p[learner.SkillGrammar], p[learner.SkillFluency], 1e-9)
	require.Equal(t, []learner.Skill{learner.SkillFluency, learner.SkillVocabulary}, learner.FocusSkills(in))
}

func TestFocusSkillsHigherPriorityBeatsLowerLevel(t *testing.T) {
	in := specFocusInput()
	got := learner.FocusSkills(in)
	require.Equal(t, learner.SkillGrammar, got[0]) // GRAMMAR (2.35) outranks NATURALNESS (2.20) by P
}

func TestFocusSkillsAboveAverageHasNoRelativeGap(t *testing.T) {
	in := specFocusInput() // VOCABULARY 3.10 is above avg 2.492: relgap must clamp at 0, not go negative
	require.InDelta(t, 0.0, learner.Priority(in)[learner.SkillVocabulary], 1e-9)
}
