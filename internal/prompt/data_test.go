package prompt_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// fieldPaths lists every exported field path of a struct type, recursively.
func fieldPaths(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := prefix + f.Name
		out = append(out, path)
		if f.Type.Kind() == reflect.Struct {
			out = append(out, fieldPaths(f.Type, path+".")...)
		}
	}
	return out
}

func TestPromptDataShape(t *testing.T) {
	paths := fieldPaths(reflect.TypeOf(prompt.PromptData{}), "")
	require.ElementsMatch(t, []string{
		"CoreRules", "OutputContract",
		"Character", "Character.Name", "Character.Persona", "Character.GreetingHint",
		"Topic", "Topic.Code", "Topic.Name",
		"Scenario", "Scenario.Situation", "Scenario.AIRole", "Scenario.UserRole", "Scenario.Goal", "Scenario.Tasks",
		"Scenario.Constraints", "Scenario.GrammarTargets", "Scenario.VocabHints", "Scenario.DifficultyLevel",
		"Scenario.DifficultyBand", "Scenario.OpeningLine",
		"Speech", "Speech.Formality", "Speech.FormalityInstruction", "Speech.MaxSentencesPerTurn", "Speech.MaxConsecutiveQuestions",
		"Learner", "Learner.DisplayName", "Learner.Adaptation", "Learner.GoalLine", "Learner.FocusSkills",
		"Memory", "Memory.Items", "Memory.Episodes",
		"Session", "Session.Mode", "Session.DurationMinutes", "Session.RemainingMinutes", "Session.Summary",
	}, paths)
}

func TestPromptDataHasNoStyleExpressionOrIdentity(t *testing.T) { // D-014, S-4
	for _, p := range fieldPaths(reflect.TypeOf(prompt.PromptData{}), "") {
		lp := strings.ToLower(p)
		for _, banned := range []string{"style", "expression", "allowedexpressions", "fullname", "full_name", "email", "voice", "userid"} {
			require.NotContains(t, lp, banned, "PromptData must not carry %q (%s)", banned, p)
		}
	}
}

func TestOpeningPromptDataIsAlias(t *testing.T) {
	require.Equal(t, prompt.PromptData{}, prompt.OpeningPromptData{})
	require.Equal(t, reflect.TypeOf(prompt.PromptData{}), reflect.TypeOf(prompt.OpeningPromptData{}))
}

func TestVocabHintJSONTags(t *testing.T) {
	typ := reflect.TypeOf(prompt.VocabHint{})
	require.Equal(t, "term_ja", typ.Field(0).Tag.Get("json"))
	require.Equal(t, "reading", typ.Field(1).Tag.Get("json"))
	require.Equal(t, "meaning", typ.Field(2).Tag.Get("json"))
}

func TestNormalizeSpeakable(t *testing.T) {
	require.Equal(t, "こんにちは世界", prompt.NormalizeSpeakable("こんにちは、 世界！"))
	require.Equal(t, "abc123", prompt.NormalizeSpeakable("a b-c (1) 2 \n3"))
	require.Equal(t, "コーヒー", prompt.NormalizeSpeakable("コーヒー"), "the long vowel mark is a letter, not punctuation")
	require.Equal(t, "", prompt.NormalizeSpeakable("、。！？… ♪ ¥$"))
	require.Equal(t, "二十円", prompt.NormalizeSpeakable("二十　円"))
	require.Equal(t, "", prompt.NormalizeSpeakable(""))
}
