package prompt_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func TestRenderMissingKeyAndSnakeCaseFail(t *testing.T) {
	_, err := prompt.Render(prompt.Template{System: "{{.Nope}}"}, prompt.PromptData{})
	require.Error(t, err)
	_, err = prompt.Render(prompt.Template{System: "{{.character_name}}"}, prompt.PromptData{})
	require.Error(t, err)
	_, err = prompt.Render(prompt.Template{System: "{{character_name}}"}, prompt.PromptData{})
	require.Error(t, err, "the bare snake_case form is not a function either")
	_, err = prompt.Render(prompt.Template{System: "{{.Character.Nope}}"}, prompt.PromptData{})
	require.Error(t, err, "nested misspellings fail too")
}

func TestRenderTypedFields(t *testing.T) {
	d := prompt.PromptData{CoreRules: "RULES", Character: prompt.CharacterData{Name: "ハナ"}, Session: prompt.SessionData{RemainingMinutes: 7}}
	r, err := prompt.Render(prompt.Template{Key: "CONVERSATION", Version: 1,
		System: "{{.CoreRules}} {{.Character.Name}} {{.Session.RemainingMinutes}}{{if .Learner.DisplayName}} Hi {{.Learner.DisplayName}}{{end}}"}, d)
	require.NoError(t, err)
	require.Equal(t, "RULES ハナ 7", r.System)
	d.Learner.DisplayName = "太郎"
	r, err = prompt.Render(prompt.Template{System: "{{if .Learner.DisplayName}}Hi {{.Learner.DisplayName}}{{end}}"}, d)
	require.NoError(t, err)
	require.Equal(t, "Hi 太郎", r.System)
}

func TestFuncMap(t *testing.T) {
	d := struct {
		L []string
		V []prompt.VocabHint
	}{[]string{"a", "b"}, []prompt.VocabHint{{TermJA: "経験", Reading: "けいけん", Meaning: "experience"}}}
	r, err := prompt.Render(prompt.Template{System: `{{join .L ", "}}|{{bullets .L}}|{{quote "私"}}|{{vocab .V}}`}, d)
	require.NoError(t, err)
	require.Equal(t, "a, b|- a\n- b|「私」|- 経験（けいけん）: experience", r.System)
}

func TestFuncMapEmptyAndMultiline(t *testing.T) {
	d := struct {
		L []string
		V []prompt.VocabHint
	}{nil, []prompt.VocabHint{{TermJA: "一", Reading: "いち", Meaning: "one"}, {TermJA: "二", Reading: "に", Meaning: "two"}}}
	r, err := prompt.Render(prompt.Template{System: `[{{bullets .L}}][{{join .L ","}}]{{vocab .V}}`}, d)
	require.NoError(t, err)
	require.Equal(t, "[][]- 一（いち）: one\n- 二（に）: two", r.System)
}

func TestRenderUserTemplateOptional(t *testing.T) {
	d := struct{ X string }{"v"}
	r, err := prompt.Render(prompt.Template{System: "s {{.X}}"}, d)
	require.NoError(t, err)
	require.Empty(t, r.User)
	r, err = prompt.Render(prompt.Template{System: "s", User: "u {{.X}}"}, d)
	require.NoError(t, err)
	require.Equal(t, "u v", r.User, "the user template renders against the same data")
	_, err = prompt.Render(prompt.Template{System: "s", User: "{{.Y}}"}, d)
	require.Error(t, err)
}

func TestRenderIsDeterministic(t *testing.T) {
	d := prompt.PromptData{Scenario: prompt.ScenarioData{Tasks: []string{"a", "b", "c"}, VocabHints: []prompt.VocabHint{{TermJA: "一", Reading: "いち", Meaning: "one"}}}}
	tpl := prompt.Template{System: `{{bullets .Scenario.Tasks}}{{range .Scenario.Tasks}}<{{.}}>{{end}}{{vocab .Scenario.VocabHints}}`}
	first, err := prompt.Render(tpl, d)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		r, err := prompt.Render(tpl, d)
		require.NoError(t, err)
		require.Equal(t, first, r)
	}
}

func TestRenderErrorDoesNotLeakData(t *testing.T) {
	const sentinel = "SENTINEL-秘密-123"
	d := prompt.PromptData{Character: prompt.CharacterData{Persona: sentinel}, Learner: prompt.LearnerContext{DisplayName: sentinel}}
	_, err := prompt.Render(prompt.Template{Key: "CONVERSATION", Version: 3, System: "{{.Character.Persona}} {{.Missing}}"}, d)
	require.Error(t, err)
	require.NotContains(t, err.Error(), sentinel)
	require.Contains(t, err.Error(), "CONVERSATION v3")
	require.Contains(t, err.Error(), "Missing")
	_, err = prompt.Render(prompt.Template{System: "{{index .Scenario.Tasks 9}}"}, d)
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), sentinel))
}

func TestRenderParseError(t *testing.T) {
	_, err := prompt.Render(prompt.Template{Key: "K", Version: 1, System: "{{.A"}, struct{ A string }{})
	require.ErrorContains(t, err, "K v1")
}
