package evaluation_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

func TestComputeOverall(t *testing.T) {
	p := func(v int) *int { return &v }
	s := evaluation.Scores{Grammar: p(72), Vocabulary: p(65), Fluency: p(58), Naturalness: p(70), TaskAchievement: p(80)}
	require.Equal(t, 69.00, *evaluation.ComputeOverall(s))
	require.Equal(t, 1.33, *evaluation.ComputeOverall(evaluation.Scores{Grammar: p(1), Vocabulary: p(1), Fluency: p(2)}))
	require.Equal(t, 0.0, *evaluation.ComputeOverall(evaluation.Scores{Grammar: p(0)}))
	require.Nil(t, evaluation.ComputeOverall(evaluation.Scores{}))
}

func TestInsufficientBoundaries(t *testing.T) {
	th := evaluation.Thresholds{MinUserTurns: 3, MinSpeechSeconds: 20, MinActiveSeconds: 60}
	for _, c := range []struct {
		s    evaluation.Stats
		want bool
	}{
		{evaluation.Stats{UserTurns: 2, UserSpeechSeconds: 100, ActiveSeconds: 100}, true},
		{evaluation.Stats{UserTurns: 3, UserSpeechSeconds: 19, ActiveSeconds: 100}, true},
		{evaluation.Stats{UserTurns: 3, UserSpeechSeconds: 20, ActiveSeconds: 59}, true},
		{evaluation.Stats{UserTurns: 3, UserSpeechSeconds: 20, ActiveSeconds: 60}, false},
	} {
		require.Equal(t, c.want, c.s.Insufficient(th), "%+v", c.s)
	}
}

func TestSpecOf(t *testing.T) {
	s, ok := evaluation.SpecOf(evaluation.FindingParticleError)
	require.True(t, ok)
	require.Equal(t, evaluation.FindingSpec{Dimension: evaluation.DimGrammar, TurnSeq: evaluation.ReqRequired, Original: evaluation.ReqRequired, Corrected: evaluation.ReqRequired}, s)

	s, _ = evaluation.SpecOf(evaluation.FindingExcessiveFiller)
	require.Equal(t, evaluation.DimFluency, s.Dimension)
	require.Equal(t, evaluation.ReqRequired, s.Original)
	require.Equal(t, evaluation.ReqOptional, s.Corrected)

	s, _ = evaluation.SpecOf(evaluation.FindingObjectiveNotMet)
	require.Equal(t, evaluation.DimTaskAchievement, s.Dimension)
	require.Equal(t, [3]evaluation.Req{evaluation.ReqNull, evaluation.ReqNull, evaluation.ReqNull}, [3]evaluation.Req{s.TurnSeq, s.Original, s.Corrected})

	s, ok = evaluation.SpecOf(evaluation.FindingStrength)
	require.True(t, ok)
	require.Empty(t, s.Dimension)
	require.Equal(t, [3]evaluation.Req{evaluation.ReqOptional, evaluation.ReqOptional, evaluation.ReqNull}, [3]evaluation.Req{s.TurnSeq, s.Original, s.Corrected})

	_, ok = evaluation.SpecOf("NOPE")
	require.False(t, ok)

	_, inMap := evaluation.FindingDimension[evaluation.FindingStrength]
	require.False(t, inMap)
	require.Len(t, evaluation.FindingDimension, 15)
	require.Len(t, evaluation.AllFindingTypes, 16)
	seen := map[evaluation.FindingType]bool{}
	for _, ft := range evaluation.AllFindingTypes {
		_, ok := evaluation.SpecOf(ft)
		require.True(t, ok, ft)
		seen[ft] = true
	}
	require.Len(t, seen, 16)
	for ft, d := range evaluation.FindingDimension {
		sp, _ := evaluation.SpecOf(ft)
		require.Equal(t, sp.Dimension, d, ft)
	}
}

func TestEnumValues(t *testing.T) {
	require.Equal(t, "RUNNING", string(evaluation.StatusRunning))
	require.Equal(t, "TASK_ACHIEVEMENT", string(evaluation.DimTaskAchievement))
	require.Equal(t, "MAJOR", string(evaluation.SeverityMajor))
	require.Equal(t, "INVALID_MODEL_OUTPUT", string(evaluation.FailInvalidModelOutput))
	require.Equal(t, []evaluation.Dimension{"GRAMMAR", "VOCABULARY", "FLUENCY", "NATURALNESS", "TASK_ACHIEVEMENT"}, evaluation.LLMDimensions)
}

func TestRubricVersionOf(t *testing.T) {
	require.Equal(t, "rubric-v1", evaluation.RubricVersionOf(content.PromptVersion{Version: 1}))
	require.Equal(t, "rubric-v12", evaluation.RubricVersionOf(content.PromptVersion{Version: 12}))
}

func TestDefaultConfig(t *testing.T) {
	c := evaluation.DefaultConfig()
	require.Equal(t, 3, c.MinUserTurns)
	require.Equal(t, 20, c.MinUserSpeechSeconds)
	require.Equal(t, 60, c.MinActiveSeconds)
	require.Equal(t, 0.6, c.MinFindingConfidence)
	require.Equal(t, 60000, c.TranscriptMaxChars)
	require.Equal(t, "3m20s", c.HandlerTimeout.String())
	require.Equal(t, evaluation.Thresholds{MinUserTurns: 3, MinSpeechSeconds: 20, MinActiveSeconds: 60}, c.Thresholds())
}

func TestInsufficientSummaryText(t *testing.T) {
	require.Equal(t, "Percakapan ini terlalu singkat untuk dinilai secara akurat. Coba bicara minimal 3 kali dengan total sekitar 1 menit agar kami bisa memberikan umpan balik yang berguna.", evaluation.InsufficientSummary)
}
