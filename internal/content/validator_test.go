package content_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

type fakeRepo struct {
	scenarioVersions map[uuid.UUID]content.ScenarioVersion
	scenarios        map[uuid.UUID]content.Scenario
	topics           map[uuid.UUID]content.Topic
	promptVersions   map[uuid.UUID]content.PromptVersion
	publishedSV      []content.ScenarioVersion
	characters       []content.Character
	publishedPV      []content.PromptVersion
}

func (f *fakeRepo) GetScenarioVersion(_ context.Context, id uuid.UUID) (content.ScenarioVersion, error) {
	if v, ok := f.scenarioVersions[id]; ok {
		return v, nil
	}
	return content.ScenarioVersion{}, content.ErrNotFound
}

func (f *fakeRepo) GetScenario(_ context.Context, id uuid.UUID) (content.Scenario, error) {
	if v, ok := f.scenarios[id]; ok {
		return v, nil
	}
	return content.Scenario{}, content.ErrNotFound
}

func (f *fakeRepo) GetTopic(_ context.Context, id uuid.UUID) (content.Topic, error) {
	if v, ok := f.topics[id]; ok {
		return v, nil
	}
	return content.Topic{}, content.ErrNotFound
}

func (f *fakeRepo) GetPromptVersion(_ context.Context, id uuid.UUID) (content.PromptVersion, error) {
	if v, ok := f.promptVersions[id]; ok {
		return v, nil
	}
	return content.PromptVersion{}, content.ErrNotFound
}

func (f *fakeRepo) ListPublishedScenarioVersions(context.Context) ([]content.ScenarioVersion, error) {
	return f.publishedSV, nil
}
func (f *fakeRepo) ListActiveCharacters(context.Context) ([]content.Character, error) {
	return f.characters, nil
}
func (f *fakeRepo) ListPublishedPromptVersions(context.Context) ([]content.PromptVersion, error) {
	return f.publishedPV, nil
}

func newValidator(repo content.ValidatorRepo, mod ...func(*content.ValidatorConfig)) content.Validator {
	cfg := content.ValidatorConfig{TTSEnabled: func(string) bool { return true }}
	for _, m := range mod {
		m(&cfg)
	}
	return content.NewValidator(repo, cfg)
}

func has(issues []content.Issue, severity, field, rule string) bool {
	for _, i := range issues {
		if i.Severity == severity && i.Rule == rule && (field == "" || i.Field == field) {
			return true
		}
	}
	return false
}

func errorsOf(issues []content.Issue) []content.Issue {
	var out []content.Issue
	for _, i := range issues {
		if i.Severity == content.SeverityError {
			out = append(out, i)
		}
	}
	return out
}

func validScenario() content.ScenarioVersion {
	return content.ScenarioVersion{
		ID: uuid.New(), ScenarioID: uuid.New(), Version: 1, Status: content.VersionDraft, DifficultyLevel: 2,
		RecommendedLevelMin: 1.0, RecommendedLevelMax: 2.5, Situation: "You order lunch at a small restaurant.", AIRole: "A waiter", UserRole: "A customer",
		Goal: "Order a meal", Tasks: []string{"order food"}, Constraints: []string{"ask one question at a time"}, GrammarTargets: []string{"〜をください"},
		VocabHints: []content.VocabHint{{TermJA: "注文", Reading: "ちゅうもん", Meaning: "order"}}, EvaluationFocus: []string{"GRAMMAR"},
		Formality: content.FormalityPolite, OpeningLine: "いらっしゃいませ。", VisualPrompt: content.VisualPrompt{Headline: "Lunch", Body: "Order something", StarterPhrasesJA: []string{"すみません"}},
	}
}

func TestValidatorScenarioRules(t *testing.T) {
	v := newValidator(&fakeRepo{})
	require.Empty(t, errorsOf(v.ValidateDraft(context.Background(), content.FamilyScenarios, validScenario())), "the baseline is valid")

	cases := []struct {
		name   string
		mutate func(*content.ScenarioVersion)
		field  string
		rule   string
	}{
		{"four focus skills", func(s *content.ScenarioVersion) {
			s.EvaluationFocus = []string{"GRAMMAR", "VOCABULARY", "FLUENCY", "NATURALNESS"}
		}, "evaluation_focus", "max_items"},
		{"unknown skill", func(s *content.ScenarioVersion) { s.EvaluationFocus = []string{"PRONUNCIATION"} }, "evaluation_focus[0]", "invalid_skill"},
		{"duplicate skill", func(s *content.ScenarioVersion) { s.EvaluationFocus = []string{"GRAMMAR", "GRAMMAR"} }, "evaluation_focus[1]", "duplicate"},
		{"bad formality", func(s *content.ScenarioVersion) { s.Formality = "LOUD" }, "formality", "invalid_enum"},
		{"opening line without Japanese", func(s *content.ScenarioVersion) { s.OpeningLine = "Hello there" }, "opening_line", "japanese_required"},
		{"opening line with a placeholder", func(s *content.ScenarioVersion) { s.OpeningLine = "こんにちは {{.Name}}" }, "opening_line", "markup_not_allowed"},
		{"opening line with markup", func(s *content.ScenarioVersion) { s.OpeningLine = "こんにちは<b>" }, "opening_line", "markup_not_allowed"},
		{"min above max", func(s *content.ScenarioVersion) { s.RecommendedLevelMin, s.RecommendedLevelMax = 3, 2 }, "recommended_level_min", "range"},
		{"duplicate vocab", func(s *content.ScenarioVersion) {
			s.VocabHints = append(s.VocabHints, content.VocabHint{TermJA: "注文", Reading: "ちゅうもん", Meaning: "again"})
		}, "vocab_hints[1].term_ja", "duplicate"},
		{"vocab term without Japanese", func(s *content.ScenarioVersion) {
			s.VocabHints = []content.VocabHint{{TermJA: "order", Reading: "おーだー", Meaning: "x"}}
		}, "vocab_hints[0].term_ja", "japanese_required"},
		{"vocab reading with latin", func(s *content.ScenarioVersion) {
			s.VocabHints = []content.VocabHint{{TermJA: "注文", Reading: "chuumon", Meaning: "x"}}
		}, "vocab_hints[0].reading", "reading_kana"},
		{"21 vocab hints", func(s *content.ScenarioVersion) {
			s.VocabHints = nil
			for i := 0; i < 21; i++ {
				s.VocabHints = append(s.VocabHints, content.VocabHint{TermJA: strings.Repeat("あ", i+1), Reading: "あ", Meaning: "x"})
			}
		}, "vocab_hints", "max_items"},
		{"7 tasks", func(s *content.ScenarioVersion) { s.Tasks = []string{"a", "b", "c", "d", "e", "f", "g"} }, "tasks", "max_items"},
		{"blank task", func(s *content.ScenarioVersion) { s.Tasks = []string{"  "} }, "tasks[0]", "required"},
		{"long situation", func(s *content.ScenarioVersion) { s.Situation = strings.Repeat("a", 2001) }, "situation", "max_length"},
		{"empty goal", func(s *content.ScenarioVersion) { s.Goal = "" }, "goal", "required"},
		{"difficulty 6", func(s *content.ScenarioVersion) { s.DifficultyLevel = 6 }, "difficulty_level", "range"},
		{"four starter phrases", func(s *content.ScenarioVersion) { s.VisualPrompt.StarterPhrasesJA = []string{"a", "b", "c", "d"} }, "visual_prompt.starter_phrases_ja", "max_items"},
		{"long headline", func(s *content.ScenarioVersion) { s.VisualPrompt.Headline = strings.Repeat("a", 81) }, "visual_prompt.headline", "max_length"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sv := validScenario()
			c.mutate(&sv)
			issues := v.ValidateDraft(context.Background(), content.FamilyScenarios, sv)
			require.True(t, has(issues, content.SeverityError, c.field, c.rule), "want ERROR %s on %s, got %+v", c.rule, c.field, issues)
		})
	}
	issues := v.ValidateDraft(context.Background(), content.FamilyScenarios, func() content.ScenarioVersion {
		sv := validScenario()
		sv.DifficultyLevel, sv.RecommendedLevelMin, sv.RecommendedLevelMax = 5, 0.5, 1.0
		return sv
	}())
	require.True(t, has(issues, content.SeverityWarning, "", "band_mismatch"), "a range outside the default band is only a warning")
	require.Empty(t, errorsOf(issues))
}

func TestValidatorAcceptsPointerAndRejectsWrongType(t *testing.T) {
	v := newValidator(&fakeRepo{})
	sv := validScenario()
	require.Empty(t, errorsOf(v.ValidateDraft(context.Background(), content.FamilyScenarios, &sv)))
	require.True(t, has(v.ValidateDraft(context.Background(), content.FamilyScenarios, "nope"), content.SeverityError, "", "invalid_type"))
	require.True(t, has(v.ValidateDraft(context.Background(), "styles", sv), content.SeverityError, "", "invalid_family"))
}

func validCharacter() content.Character {
	return content.Character{
		ID: uuid.New(), Code: "HARUKA", Status: content.StatusActive, Name: "Haruka", Persona: strings.Repeat("A calm and friendly guide. ", 3),
		ProviderName: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "voice-1",
		Voice: content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1.0,
			Fallback: &content.TTSBinding{ProviderName: "CARTESIA", ModelName: "sonic3.6", VoiceID: "voice-2"}},
	}
}

func TestValidateCharacterRules(t *testing.T) {
	v := newValidator(&fakeRepo{})
	require.Empty(t, v.ValidateCharacter(context.Background(), validCharacter()), "the baseline has no issue at all")

	cases := []struct {
		name   string
		mutate func(*content.Character)
		field  string
		rule   string
	}{
		{"empty name", func(c *content.Character) { c.Name = "" }, "name", "required"},
		{"51 character name", func(c *content.Character) { c.Name = strings.Repeat("a", 51) }, "name", "max_length"},
		{"short persona", func(c *content.Character) { c.Persona = strings.Repeat("a", 49) }, "persona", "min_length"},
		{"long persona", func(c *content.Character) { c.Persona = strings.Repeat("a", 3001) }, "persona", "max_length"},
		{"long greeting", func(c *content.Character) { s := strings.Repeat("あ", 201); c.GreetingHint = &s }, "greeting_hint", "max_length"},
		{"AZURE", func(c *content.Character) { c.ProviderName = "AZURE" }, "provider_name", "invalid_enum"},
		{"DEEPGRAM", func(c *content.Character) { c.ProviderName = "DEEPGRAM" }, "provider_name", "invalid_enum"},
		{"OPENAI", func(c *content.Character) { c.ProviderName = "OPENAI" }, "provider_name", "invalid_enum"},
		{"blank model", func(c *content.Character) { c.ModelName = "  " }, "model_name", "required"},
		{"long model", func(c *content.Character) { c.ModelName = strings.Repeat("m", 201) }, "model_name", "max_length"},
		{"empty voice id", func(c *content.Character) { c.VoiceID = "" }, "voice_id", "required"},
		{"long voice id", func(c *content.Character) { c.VoiceID = strings.Repeat("v", 201) }, "voice_id", "max_length"},
		{"bad gender", func(c *content.Character) { c.Voice.Gender = "X" }, "voice.gender", "invalid_enum"},
		{"bad accent", func(c *content.Character) { c.Voice.Accent = "OSAKA" }, "voice.accent", "invalid_enum"},
		{"rate 0.79", func(c *content.Character) { c.Voice.SpeakingRate = 0.79 }, "voice.speaking_rate", "range"},
		{"rate 1.21", func(c *content.Character) { c.Voice.SpeakingRate = 1.21 }, "voice.speaking_rate", "range"},
		{"fallback with the same vendor", func(c *content.Character) { c.Voice.Fallback.ProviderName = "ELEVENLABS" }, "voice.fallback.provider_name", "fallback_same_provider"},
		{"fallback without voice id", func(c *content.Character) { c.Voice.Fallback.VoiceID = "" }, "voice.fallback.voice_id", "required"},
		{"fallback with a bad vendor", func(c *content.Character) { c.Voice.Fallback.ProviderName = "OPENAI" }, "voice.fallback.provider_name", "invalid_enum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := validCharacter()
			c.mutate(&ch)
			issues := v.ValidateCharacter(context.Background(), ch)
			require.True(t, has(issues, content.SeverityError, c.field, c.rule), "want ERROR %s on %s, got %+v", c.rule, c.field, issues)
		})
	}

	for _, rate := range []float64{0.80, 1.20} {
		ch := validCharacter()
		ch.Voice.SpeakingRate = rate
		require.Empty(t, v.ValidateCharacter(context.Background(), ch), rate)
	}
}

func TestValidateCharacterNormalisesVendorCase(t *testing.T) {
	ch := validCharacter()
	ch.ProviderName = "cartesia"
	ch.Voice.Fallback.ProviderName = " elevenlabs "
	require.Empty(t, newValidator(&fakeRepo{}).ValidateCharacter(context.Background(), ch), "lower case passes")
	n := content.NormalizeCharacter(ch)
	require.Equal(t, "CARTESIA", n.ProviderName)
	require.Equal(t, "ELEVENLABS", n.Voice.Fallback.ProviderName)
	require.Equal(t, "cartesia", ch.ProviderName, "the input is not modified")
}

func TestValidateCharacterModelNameWithSpacesPasses(t *testing.T) {
	ch := validCharacter()
	ch.ModelName = "Eleven v3 Conversational"
	require.Empty(t, newValidator(&fakeRepo{}).ValidateCharacter(context.Background(), ch))
}

func TestParseVoiceRejectsUnknownKeys(t *testing.T) {
	good := `{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.0,"fallback":{"provider_name":"CARTESIA","model_name":"m","voice_id":"v"}}`
	v, issues := content.ParseVoice([]byte(good))
	require.Empty(t, issues)
	require.Equal(t, content.GenderFemale, v.Gender)
	require.Equal(t, "CARTESIA", v.Fallback.ProviderName)

	for name, raw := range map[string]string{
		"providers at top level": `{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.0,"providers":{}}`,
		"extra key in fallback":  `{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.0,"fallback":{"provider_name":"CARTESIA","model_name":"m","voice_id":"v","style":"x"}}`,
		"expression key":         `{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.0,"expressions":[]}`,
	} {
		_, issues := content.ParseVoice([]byte(raw))
		require.True(t, has(issues, content.SeverityError, "", "unknown_key"), name)
	}
	_, issues = content.ParseVoice([]byte(`{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.0,"fallback":"cartesia"}`))
	require.True(t, has(issues, content.SeverityError, "voice.fallback", "invalid_type"), "a fallback that is not an object")
	_, issues = content.ParseVoice([]byte(`[]`))
	require.True(t, has(issues, content.SeverityError, "voice", "invalid_type"))
	v, issues = content.ParseVoice([]byte(`{"gender":"FEMALE","accent":"KANTO","speaking_rate":1.0,"fallback":null}`))
	require.Empty(t, issues)
	require.Nil(t, v.Fallback, "a null fallback is no fallback")
	_, issues = content.ParseVoice([]byte(`{"speaking_rate":"fast"}`))
	require.True(t, has(issues, content.SeverityError, "voice.speaking_rate", "invalid_type"))
}

func TestValidateCharacterProviderNotConfigured(t *testing.T) {
	onlyEleven := func(c *content.ValidatorConfig) { c.TTSEnabled = func(p string) bool { return p == "ELEVENLABS" } }
	v := newValidator(&fakeRepo{}, onlyEleven)

	ch := validCharacter()
	ch.ProviderName = "CARTESIA"
	ch.Voice.Fallback.ProviderName = "ELEVENLABS"
	issues := v.ValidateCharacter(context.Background(), ch)
	require.True(t, has(issues, content.SeverityError, "provider_name", "provider_not_configured"), "an ACTIVE character on a disabled vendor")

	draft := ch
	draft.Status = content.StatusDisabled
	require.False(t, has(v.ValidateCharacter(context.Background(), draft), content.SeverityError, "provider_name", "provider_not_configured"), "only ACTIVE characters are blocked")

	fb := validCharacter() // primary ELEVENLABS ok, fallback CARTESIA disabled
	issues = v.ValidateCharacter(context.Background(), fb)
	require.True(t, has(issues, content.SeverityWarning, "voice.fallback.provider_name", "fallback_provider_not_configured"))
	require.Empty(t, errorsOf(issues))
}

func TestValidateCharacterPlaceholderVoiceID(t *testing.T) {
	ch := validCharacter()
	ch.VoiceID = "PLACEHOLDER_VOICE_HARUKA"
	ch.Voice.Fallback.VoiceID = "PLACEHOLDER_VOICE_HARUKA_FB"
	fake := newValidator(&fakeRepo{}).ValidateCharacter(context.Background(), ch)
	require.True(t, has(fake, content.SeverityWarning, "voice_id", "voice_placeholder"))
	require.True(t, has(fake, content.SeverityWarning, "voice.fallback.voice_id", "voice_placeholder"))
	require.Empty(t, errorsOf(fake), "a placeholder is only a warning in fake mode")

	realMode := newValidator(&fakeRepo{}, func(c *content.ValidatorConfig) { c.TTSModeReal = true }).ValidateCharacter(context.Background(), ch)
	require.True(t, has(realMode, content.SeverityError, "voice_id", "voice_placeholder"))
	require.True(t, has(realMode, content.SeverityError, "voice.fallback.voice_id", "voice_placeholder"))
}

func TestValidateCharacterKansaiPersona(t *testing.T) {
	v := newValidator(&fakeRepo{})
	ch := validCharacter()
	ch.Voice.Accent = content.AccentKansai
	require.True(t, has(v.ValidateCharacter(context.Background(), ch), content.SeverityWarning, "persona", "kansai_persona"))
	ch.Persona = "Speaks Kansai-ben with 〜やん and ほんま. " + strings.Repeat("Warm. ", 10)
	require.Empty(t, v.ValidateCharacter(context.Background(), ch))
	ch.Persona = "関西弁で話す、明るい人です。" + strings.Repeat("A friendly guide. ", 5)
	require.Empty(t, v.ValidateCharacter(context.Background(), ch))
}

func TestValidatorHasNoStyleOrExpressionRules(t *testing.T) {
	ch := validCharacter()
	for _, is := range newValidator(&fakeRepo{}).ValidateCharacter(context.Background(), ch) {
		for _, banned := range []string{"style", "expression", "voice_profile", "tts_mapping", "providers", "<e:"} {
			require.NotContains(t, strings.ToLower(is.Rule+is.Field+is.Message), banned)
		}
	}
}

// ---- prompts -------------------------------------------------------------------------------

type summaryData struct{ Transcript string }

func registerFixtures(t *testing.T) {
	t.Helper()
	prompt.ResetFixturesForTest()
	t.Cleanup(prompt.ResetFixturesForTest)
	prompt.RegisterFixtures("SUMMARY", func() []any { return []any{summaryData{}, summaryData{Transcript: "x"}} })
	prompt.RegisterFixtures("EVALUATION", func() []any { return []any{summaryData{}} })
	prompt.RegisterFixtures("MEMORY_EXTRACTION", func() []any { return []any{summaryData{}} })
}

const objectSchema = `{"type":"object","properties":{"score":{"type":"integer"}},"required":["score"]}`

func ptrStr(s string) *string { return &s }

func validPrompt(key content.PromptKey) content.PromptVersion {
	temp, tokens := 0.8, 320
	pv := content.PromptVersion{ID: uuid.New(), Key: key, Version: 1, Status: content.VersionDraft, LLMParams: content.LLMParams{Temperature: &temp, MaxOutputTokens: &tokens}}
	switch key {
	case content.PromptConversation:
		pv.SystemTemplate = prompt.ConversationV1.System
	case content.PromptOpening:
		pv.SystemTemplate = prompt.OpeningV1.System
	case content.PromptSummary:
		pv.SystemTemplate, pv.UserTemplate = "Summarise.", ptrStr("{{.Transcript}}")
	default:
		pv.SystemTemplate, pv.UserTemplate, pv.OutputSchema = "Evaluate.", ptrStr("{{.Transcript}}"), json.RawMessage(objectSchema)
	}
	return pv
}

func TestValidatorPromptRules(t *testing.T) {
	registerFixtures(t)
	v := newValidator(&fakeRepo{})
	for _, key := range []content.PromptKey{content.PromptConversation, content.PromptOpening, content.PromptSummary, content.PromptEvaluation, content.PromptMemoryExtraction} {
		require.Empty(t, errorsOf(v.ValidateDraft(context.Background(), content.FamilyPrompts, validPrompt(key))), key)
	}

	cases := []struct {
		name   string
		key    content.PromptKey
		mutate func(*content.PromptVersion)
		field  string
		rule   string
	}{
		{"summary without user template", content.PromptSummary, func(p *content.PromptVersion) { p.UserTemplate = nil }, "user_template", "required"},
		{"evaluation without user template", content.PromptEvaluation, func(p *content.PromptVersion) { p.UserTemplate = nil }, "user_template", "required"},
		{"conversation with a user template", content.PromptConversation, func(p *content.PromptVersion) { p.UserTemplate = ptrStr("x") }, "user_template", "must_be_null"},
		{"opening with a user template", content.PromptOpening, func(p *content.PromptVersion) { p.UserTemplate = ptrStr("x") }, "user_template", "must_be_null"},
		{"conversation with an output schema", content.PromptConversation, func(p *content.PromptVersion) { p.OutputSchema = json.RawMessage(objectSchema) }, "output_schema", "must_be_null"},
		{"evaluation without a schema", content.PromptEvaluation, func(p *content.PromptVersion) { p.OutputSchema = nil }, "output_schema", "required"},
		{"memory extraction without a schema", content.PromptMemoryExtraction, func(p *content.PromptVersion) { p.OutputSchema = nil }, "output_schema", "required"},
		{"schema with an array root", content.PromptEvaluation, func(p *content.PromptVersion) { p.OutputSchema = json.RawMessage(`{"type":"array"}`) }, "output_schema", "invalid_json_schema"},
		{"schema that does not compile", content.PromptEvaluation, func(p *content.PromptVersion) {
			p.OutputSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"nonsense"}}}`)
		}, "output_schema", "invalid_json_schema"},
		{"schema that is not JSON", content.PromptEvaluation, func(p *content.PromptVersion) { p.OutputSchema = json.RawMessage(`{`) }, "output_schema", "invalid_json_schema"},
		{"snake case placeholder", content.PromptSummary, func(p *content.PromptVersion) { p.SystemTemplate = "{{.character_name}}" }, "system_template", "template_render"},
		{"unknown field", content.PromptSummary, func(p *content.PromptVersion) { p.UserTemplate = ptrStr("{{.Nope}}") }, "system_template", "template_render"},
		{"core rules twice", content.PromptConversation, func(p *content.PromptVersion) { p.SystemTemplate += "\n{{.CoreRules}}" }, "system_template", "placeholder_count"},
		{"output contract missing", content.PromptOpening, func(p *content.PromptVersion) {
			p.SystemTemplate = strings.ReplaceAll(p.SystemTemplate, "{{.OutputContract}}", "")
		}, "system_template", "placeholder_count"},
		{"empty system template", content.PromptSummary, func(p *content.PromptVersion) { p.SystemTemplate = "" }, "system_template", "required"},
		{"temperature 3", content.PromptSummary, func(p *content.PromptVersion) { x := 3.0; p.LLMParams.Temperature = &x }, "llm_params.temperature", "range"},
		{"max tokens 8", content.PromptSummary, func(p *content.PromptVersion) { x := 8; p.LLMParams.MaxOutputTokens = &x }, "llm_params.max_output_tokens", "range"},
		{"render too long", content.PromptConversation, func(p *content.PromptVersion) { p.SystemTemplate += strings.Repeat("あ", 3000) }, "system_template", "render_too_long"},
		{"unknown key", content.PromptKey("STYLE"), func(*content.PromptVersion) {}, "key", "invalid_enum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pv := validPrompt(firstKnown(c.key))
			pv.Key = c.key
			c.mutate(&pv)
			issues := v.ValidateDraft(context.Background(), content.FamilyPrompts, pv)
			require.True(t, has(issues, content.SeverityError, c.field, c.rule), "want ERROR %s on %s, got %+v", c.rule, c.field, issues)
		})
	}
}

func firstKnown(k content.PromptKey) content.PromptKey {
	if k == "STYLE" {
		return content.PromptSummary
	}
	return k
}

func TestValidatorPromptMissingFixture(t *testing.T) {
	prompt.ResetFixturesForTest()
	t.Cleanup(prompt.ResetFixturesForTest) // only CONVERSATION/OPENING fixtures exist
	issues := newValidator(&fakeRepo{}).ValidateDraft(context.Background(), content.FamilyPrompts, validPrompt(content.PromptSummary))
	require.True(t, has(issues, content.SeverityError, "", "missing_fixture"))
}

func TestValidatorLLMParamsWarningsAndModelKey(t *testing.T) {
	registerFixtures(t)
	v := newValidator(&fakeRepo{})
	pv := validPrompt(content.PromptSummary)
	pv.LLMParams = content.LLMParams{}
	issues := v.ValidateDraft(context.Background(), content.FamilyPrompts, pv)
	require.True(t, has(issues, content.SeverityWarning, "llm_params.temperature", "missing_param"))
	require.True(t, has(issues, content.SeverityWarning, "llm_params.max_output_tokens", "missing_param"))
	require.Empty(t, errorsOf(issues))

	p, issues := content.ParseLLMParams([]byte(`{"temperature":0.5,"model":"gpt-4.1"}`))
	require.True(t, has(issues, content.SeverityError, "llm_params.model", "unknown_key"), "the model name does not belong in llm_params")
	require.Equal(t, 0.5, *p.Temperature)
	p, issues = content.ParseLLMParams([]byte(`{"temperature":0.3,"max_output_tokens":120,"top_p":0.9,"seed":7}`))
	require.Empty(t, issues)
	require.Equal(t, 120, *p.MaxOutputTokens)
	_, issues = content.ParseLLMParams([]byte(`[]`))
	require.True(t, has(issues, content.SeverityError, "llm_params", "invalid_type"))
	_, issues = content.ParseLLMParams([]byte(`{"temperature":"hot"}`))
	require.True(t, has(issues, content.SeverityError, "temperature", "invalid_type"))
	_, issues = content.ParseLLMParams(nil)
	require.Empty(t, issues)
}

func TestValidatePublishCrossReferences(t *testing.T) {
	registerFixtures(t)
	sv := validScenario()
	scn := content.Scenario{ID: sv.ScenarioID, TopicID: uuid.New(), Status: content.StatusActive}
	topic := content.Topic{ID: scn.TopicID, Status: content.StatusActive}
	repo := &fakeRepo{
		scenarioVersions: map[uuid.UUID]content.ScenarioVersion{sv.ID: sv},
		scenarios:        map[uuid.UUID]content.Scenario{scn.ID: scn},
		topics:           map[uuid.UUID]content.Topic{topic.ID: topic},
		promptVersions:   map[uuid.UUID]content.PromptVersion{},
	}
	pv := validPrompt(content.PromptSummary)
	repo.promptVersions[pv.ID] = pv
	v := newValidator(repo)
	ctx := context.Background()

	require.Empty(t, errorsOf(v.ValidatePublish(ctx, content.FamilyScenarios, sv.ID)))
	require.Empty(t, errorsOf(v.ValidatePublish(ctx, content.FamilyPrompts, pv.ID)))

	scn.Status = content.StatusArchived
	repo.scenarios[scn.ID] = scn
	require.True(t, has(v.ValidatePublish(ctx, content.FamilyScenarios, sv.ID), content.SeverityError, "", "parent_archived"))
	scn.Status = content.StatusActive
	repo.scenarios[scn.ID] = scn
	topic.Status = content.StatusArchived
	repo.topics[topic.ID] = topic
	require.True(t, has(v.ValidatePublish(ctx, content.FamilyScenarios, sv.ID), content.SeverityError, "", "parent_archived"))
	delete(repo.topics, topic.ID)
	require.True(t, has(v.ValidatePublish(ctx, content.FamilyScenarios, sv.ID), content.SeverityError, "", "parent_missing"))

	require.True(t, has(v.ValidatePublish(ctx, content.FamilyScenarios, uuid.New()), content.SeverityError, "", "not_found"))
	require.True(t, has(v.ValidatePublish(ctx, content.FamilyPrompts, uuid.New()), content.SeverityError, "", "not_found"))
	require.True(t, has(v.ValidatePublish(ctx, "styles", uuid.New()), content.SeverityError, "", "invalid_family"))
}

func TestValidateCatalog(t *testing.T) {
	registerFixtures(t)
	sv := validScenario()
	sv.Status = content.VersionPublished
	scn := content.Scenario{ID: sv.ScenarioID, TopicID: uuid.New(), Status: content.StatusActive}
	conv := validPrompt(content.PromptConversation)
	repo := &fakeRepo{
		scenarios:   map[uuid.UUID]content.Scenario{scn.ID: scn},
		topics:      map[uuid.UUID]content.Topic{scn.TopicID: {ID: scn.TopicID, Status: content.StatusActive}},
		publishedSV: []content.ScenarioVersion{sv}, characters: []content.Character{validCharacter()}, publishedPV: []content.PromptVersion{conv},
	}
	issues := newValidator(repo).ValidateCatalog(context.Background())
	require.Empty(t, errorsOf(issues), "a healthy catalog has no ERROR")
	for _, k := range []string{"OPENING", "SUMMARY", "EVALUATION", "MEMORY_EXTRACTION"} {
		found := false
		for _, is := range issues {
			found = found || (is.Rule == "missing_published_prompt" && is.Severity == content.SeverityWarning && strings.HasSuffix(is.Entity, k))
		}
		require.True(t, found, "a missing %s prompt is only a warning (M7/M8 add them)", k)
	}

	repo.publishedPV = nil
	issues = newValidator(repo).ValidateCatalog(context.Background())
	require.True(t, has(issues, content.SeverityError, "status", "missing_published_prompt"), "no PUBLISHED CONVERSATION is an ERROR")

	bad := validCharacter()
	bad.Name = ""
	repo.characters = []content.Character{bad}
	require.True(t, has(newValidator(repo).ValidateCatalog(context.Background()), content.SeverityError, "name", "required"))
}

// ---- ReportCatalog ------------------------------------------------------------------------

type stubValidator struct{ issues []content.Issue }

func (s stubValidator) ValidateDraft(context.Context, content.Family, any) []content.Issue {
	return nil
}
func (s stubValidator) ValidatePublish(context.Context, content.Family, uuid.UUID) []content.Issue {
	return nil
}
func (s stubValidator) ValidateCharacter(context.Context, content.Character) []content.Issue {
	return nil
}
func (s stubValidator) ValidateCatalog(context.Context) []content.Issue { return s.issues }

type recObserver struct{ gauges map[string]int }

func (recObserver) ResolveError(string)         {}
func (recObserver) VoiceFallbackMissing(string) {}
func (r recObserver) ValidationIssues(sev string, n int) {
	r.gauges[sev] = n
}

func TestReportCatalog_SetsGaugesAndLogs(t *testing.T) {
	v := stubValidator{issues: []content.Issue{
		{Severity: content.SeverityError, Entity: "characters/A", Field: "name", Rule: "required", Message: "name is required"},
		{Severity: content.SeverityError, Entity: "characters/B", Field: "persona", Rule: "min_length", Message: "short"},
		{Severity: content.SeverityWarning, Entity: "characters/C", Field: "voice_id", Rule: "voice_placeholder", Message: "placeholder"},
	}}
	obs := recObserver{gauges: map[string]int{}}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	got := content.ReportCatalog(context.Background(), v, obs, log)
	require.Len(t, got, 3)
	require.Equal(t, 2, obs.gauges["ERROR"])
	require.Equal(t, 1, obs.gauges["WARNING"])
	require.Equal(t, 2, strings.Count(buf.String(), `"level":"ERROR"`), "one ERROR log per ERROR issue")
	require.NotContains(t, buf.String(), `"level":"WARN"`)

	require.NotPanics(t, func() { content.ReportCatalog(context.Background(), v, nil, nil) }, "nil observer and logger are tolerated")
	clean := content.ReportCatalog(context.Background(), stubValidator{}, obs, log)
	require.Empty(t, clean)
	require.Zero(t, obs.gauges["ERROR"], "a clean run resets the gauge")
}
