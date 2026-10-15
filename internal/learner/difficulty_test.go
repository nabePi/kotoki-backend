package learner_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func TestSpeedTables(t *testing.T) {
	rates := map[int]float64{1: 0.80, 2: 0.88, 3: 0.95, 4: 1.00, 5: 1.05}
	sentences := map[int]int{1: 2, 2: 2, 3: 3, 4: 3, 5: 4}
	for speed := 1; speed <= 5; speed++ {
		require.InDelta(t, rates[speed], learner.TTSSpeakingRate(speed), 1e-9, "rate %d", speed)
		require.Equal(t, sentences[speed], learner.MaxAISentences(speed), "sentences %d", speed)
	}
	// Out-of-range speeds clamp to the nearest valid one.
	require.InDelta(t, 0.80, learner.TTSSpeakingRate(0), 1e-9)
	require.InDelta(t, 1.05, learner.TTSSpeakingRate(9), 1e-9)
	require.Equal(t, 2, learner.MaxAISentences(-1))
	require.Equal(t, 4, learner.MaxAISentences(6))
}

func TestAdaptiveDifficultyJSONTags(t *testing.T) {
	raw, err := json.Marshal(learner.AdaptiveDifficulty{FocusSkills: []learner.Skill{"GRAMMAR"}})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	for _, k := range []string{"algorithm_version", "scenario_difficulty", "vocabulary_complexity", "grammar_complexity", "speech_speed",
		"tts_speaking_rate", "max_ai_sentences_per_turn", "topic_novelty", "topic_abstraction", "question_complexity", "follow_up_depth",
		"max_follow_ups", "expected_response_length", "spontaneity", "scaffolding", "formality", "dialect_exposure",
		"turn_taking_complexity", "focus_skills", "in_session_adjustments"} {
		require.Contains(t, m, k)
	}
	require.Len(t, m, 20)
}
