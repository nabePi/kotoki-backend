package learner_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

func skillNames(ss []learner.Skill) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

func TestAllSkillsOrderD097(t *testing.T) {
	require.Equal(t, []string{"GRAMMAR", "VOCABULARY", "FLUENCY", "NATURALNESS", "LISTENING_RESPONSE", "INTERACTION_COMPLEXITY"}, skillNames(learner.AllSkills()))
	_, ok := learner.ParseSkill("TASK_ACHIEVEMENT")
	require.False(t, ok)
	for _, s := range learner.AllSkills() {
		got, ok := learner.ParseSkill(string(s))
		require.True(t, ok)
		require.Equal(t, s, got)
	}
	_, ok = learner.ParseSkill("grammar")
	require.False(t, ok)
	// The returned slice is a copy: mutating it must not change the canonical order.
	learner.AllSkills()[0] = learner.SkillFluency
	require.Equal(t, learner.SkillGrammar, learner.AllSkills()[0])
}

func TestAdaptiveDifficultyJSONKeys(t *testing.T) {
	want := []string{"algorithm_version", "scenario_difficulty", "vocabulary_complexity", "grammar_complexity", "speech_speed",
		"tts_speaking_rate", "max_ai_sentences_per_turn", "topic_novelty", "topic_abstraction", "question_complexity", "follow_up_depth",
		"max_follow_ups", "expected_response_length", "spontaneity", "scaffolding", "formality", "dialect_exposure",
		"turn_taking_complexity", "focus_skills", "in_session_adjustments"}
	for name, ad := range map[string]learner.AdaptiveDifficulty{
		"nil focus":   {},
		"empty focus": {FocusSkills: []learner.Skill{}},
	} {
		raw, err := json.Marshal(ad)
		require.NoError(t, err, name)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m), name)
		require.Len(t, m, 20, name)
		for _, k := range want {
			require.Contains(t, m, k, name)
		}
		require.Equal(t, []any{}, m["focus_skills"], name)
	}
	// Pointer and round-trip behave the same.
	raw, err := json.Marshal(&learner.AdaptiveDifficulty{FocusSkills: []learner.Skill{"GRAMMAR"}, Scaffolding: 2})
	require.NoError(t, err)
	var back learner.AdaptiveDifficulty
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, []learner.Skill{"GRAMMAR"}, back.FocusSkills)
	require.Equal(t, 2, back.Scaffolding)
}
