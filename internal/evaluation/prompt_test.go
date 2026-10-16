package evaluation_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

const (
	phraseCasual     = "casual (plain form, タメ口)"
	phrasePolite     = "polite (です・ます)"
	phraseKeigoLight = "polite (です・ます); fixed keigo phrases of the role are expected, and any correct keigo is also acceptable"
	phraseKeigo      = "polite/keigo (です・ます, 尊敬語・謙譲語 where appropriate)"
)

func TestExpectedFormality(t *testing.T) {
	tests := []struct {
		name     string
		adaptive json.RawMessage
		scenario content.Formality
		want     string
	}{
		{"adaptive casual", json.RawMessage(`{"formality":"CASUAL"}`), content.FormalityKeigo, phraseCasual},
		{"adaptive polite", json.RawMessage(`{"formality":"POLITE"}`), content.FormalityCasual, phrasePolite},
		{"adaptive keigo light", json.RawMessage(`{"formality":"KEIGO_LIGHT"}`), content.FormalityCasual, phraseKeigoLight},
		{"adaptive keigo", json.RawMessage(`{"formality":"KEIGO"}`), content.FormalityCasual, phraseKeigo},
		{"other keys ignored", json.RawMessage(`{"level":3,"formality":"CASUAL","x":{"formality":"KEIGO"}}`), content.FormalityPolite, phraseCasual},
		{"missing key falls back (casual)", json.RawMessage(`{"level":3}`), content.FormalityCasual, phraseCasual},
		{"missing key falls back (polite)", json.RawMessage(`{}`), content.FormalityPolite, phrasePolite},
		{"nil falls back (keigo)", nil, content.FormalityKeigo, phraseKeigo},
		{"unknown value falls back", json.RawMessage(`{"formality":"SLANG"}`), content.FormalityKeigo, phraseKeigo},
		{"non-string value falls back", json.RawMessage(`{"formality":7}`), content.FormalityCasual, phraseCasual},
		{"invalid json falls back", json.RawMessage(`{`), content.FormalityPolite, phrasePolite},
		{"unknown scenario formality defaults to polite", json.RawMessage(`{}`), content.Formality("WEIRD"), phrasePolite},
		{"json null falls back", json.RawMessage(`null`), content.FormalityCasual, phraseCasual},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, evaluation.ExpectedFormality(tt.adaptive, content.ScenarioVersion{Formality: tt.scenario}))
		})
	}
}

func TestBuildPromptDataObjectives(t *testing.T) {
	sv := content.ScenarioVersion{
		Goal: "Order lunch", Tasks: []string{"Ask for the menu", "Order a drink"},
		GrammarTargets: []string{"〜をください", "〜たいです"},
		Situation:      "A small cafe.", UserRole: "Customer", AIRole: "Waiter", DifficultyLevel: 2,
		EvaluationFocus: []string{"GRAMMAR"},
	}
	d := evaluation.BuildPromptData(evaluation.PromptInput{
		TopicName: "Food", ScenarioName: "Cafe", CharacterName: "葵", SV: sv,
		SessionAdaptive: json.RawMessage(`{"formality":"POLITE"}`), RubricVersion: "rubric-v1",
		Stats: evaluation.Stats{UserTurns: 4, UserSpeechSeconds: 25, ActiveSeconds: 90}, Transcript: "[T0] AI: こんにちは",
	})
	require.Equal(t, []string{"Order lunch", "Ask for the menu", "Order a drink", "Use: 〜をください, 〜たいです"}, d.Objectives)
	require.Equal(t, "Food", d.TopicTitle)
	require.Equal(t, "Cafe", d.ScenarioTitle)
	require.Equal(t, 2, d.DifficultyLevel)
	require.Equal(t, "Customer", d.LearnerRole)
	require.Equal(t, "Waiter", d.AIRole)
	require.Equal(t, "葵", d.CharacterName)
	require.Equal(t, "A small cafe.", d.ScenarioContext)
	require.Equal(t, phrasePolite, d.ExpectedFormality)
	require.Equal(t, []string{"GRAMMAR"}, d.EvaluationFocus)
	require.Equal(t, 4, d.UserTurnCount)
	require.Equal(t, 25, d.UserSpeechSeconds)
	require.Equal(t, 90, d.ActiveSeconds)
	require.Equal(t, "rubric-v1", d.RubricVersion)
	require.Equal(t, "[T0] AI: こんにちは", d.Transcript)
	require.Equal(t, evaluation.FindingTypesText(), d.FindingTypes)
	require.Equal(t, evaluation.PatternKeyExamplesText(), d.PatternKeyExamples)

	sv.GrammarTargets = nil
	d = evaluation.BuildPromptData(evaluation.PromptInput{SV: sv})
	require.Equal(t, []string{"Order lunch", "Ask for the menu", "Order a drink"}, d.Objectives)

	sv.Tasks = nil
	d = evaluation.BuildPromptData(evaluation.PromptInput{SV: sv})
	require.Equal(t, []string{"Order lunch"}, d.Objectives)
}

func TestFindingTypesText(t *testing.T) {
	text := evaluation.FindingTypesText()
	lines := strings.Split(text, "\n")
	require.Len(t, lines, 16)
	require.Contains(t, lines, "PARTICLE_ERROR -> GRAMMAR; turn_seq, original, corrected required")
	require.Contains(t, lines, "EXCESSIVE_FILLER -> FLUENCY; turn_seq, original required; corrected optional")
	require.Contains(t, lines, "OBJECTIVE_NOT_MET -> TASK_ACHIEVEMENT; turn_seq, original, corrected must be null")
	require.Contains(t, lines, "STRENGTH -> any of GRAMMAR, VOCABULARY, FLUENCY, NATURALNESS, TASK_ACHIEVEMENT; severity must be INFO; turn_seq and original optional; corrected must be null")
	for i, ft := range evaluation.AllFindingTypes {
		require.True(t, strings.HasPrefix(lines[i], string(ft)+" -> "), "line %d: %s", i, lines[i])
	}
}

func TestPatternKeyExamples(t *testing.T) {
	require.Equal(t, "particle.de_ni_location, particle.wa_ga, particle.wo_omission, conjugation.i_adjective_past, "+
		"conjugation.te_form, aspect.teiru, politeness.plain_in_formal, politeness.keigo_misuse, "+
		"vocab.english_substitution, vocab.wrong_counter, structure.missing_predicate, fluency.one_word_answers, "+
		"task.no_follow_up_question", evaluation.PatternKeyExamplesText())
	require.Len(t, strings.Split(evaluation.PatternKeyExamplesText(), ", "), 13)
}

func TestPromptFixtures(t *testing.T) {
	fx := evaluation.PromptFixtures()
	require.Len(t, fx, 2)
	small, ok := fx[0].(evaluation.EvaluationPromptData)
	require.True(t, ok)
	large, ok := fx[1].(evaluation.EvaluationPromptData)
	require.True(t, ok)

	require.Empty(t, small.Objectives)
	require.Empty(t, small.EvaluationFocus)
	require.Equal(t, 3, strings.Count(small.Transcript, "USER ("))

	require.Equal(t, 60000, utf8.RuneCountInString(large.Transcript))
	require.Len(t, large.EvaluationFocus, 3)
	require.NotEmpty(t, large.Objectives)
	for _, d := range []evaluation.EvaluationPromptData{small, large} {
		require.NotEmpty(t, d.RubricVersion)
		require.NotEmpty(t, d.FindingTypes)
		require.NotEmpty(t, d.PatternKeyExamples)
		require.NotEmpty(t, d.ExpectedFormality)
	}
}

func TestExampleOutputIsJSON(t *testing.T) {
	var v struct {
		Findings []json.RawMessage `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(evaluation.ExampleOutput), &v))
	require.Len(t, v.Findings, 2)
}
