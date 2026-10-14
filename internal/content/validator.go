package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// Issue severities.
const (
	SeverityError   = "ERROR"
	SeverityWarning = "WARNING"
)

// Issue is one finding of the validator (spec 6.10): ERROR blocks create/publish/PATCH,
// WARNING is only reported.
type Issue struct {
	Severity string `json:"severity"`
	Entity   string `json:"entity,omitempty"` // e.g. "characters/HARUKA", "scenario_versions/0192…"
	Field    string `json:"field"`
	Rule     string `json:"rule"` // e.g. "max_length", "voice_placeholder"
	Message  string `json:"message"`
}

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("content: not found")

// Validator checks content (spec 6.10.1).
type Validator interface {
	ValidateDraft(ctx context.Context, family Family, v any) []Issue                 // content rules
	ValidatePublish(ctx context.Context, family Family, versionID uuid.UUID) []Issue // + cross references
	ValidateCharacter(ctx context.Context, c Character) []Issue                      // content + voice rules
	ValidateCatalog(ctx context.Context) []Issue                                     // everything ACTIVE/PUBLISHED
}

// ValidatorRepo is the read subset of the content repository the validator needs.
type ValidatorRepo interface {
	GetScenarioVersion(ctx context.Context, id uuid.UUID) (ScenarioVersion, error)
	GetScenario(ctx context.Context, id uuid.UUID) (Scenario, error)
	GetTopic(ctx context.Context, id uuid.UUID) (Topic, error)
	GetPromptVersion(ctx context.Context, id uuid.UUID) (PromptVersion, error)
	ListPublishedScenarioVersions(ctx context.Context) ([]ScenarioVersion, error)
	ListActiveCharacters(ctx context.Context) ([]Character, error)
	ListPublishedPromptVersions(ctx context.Context) ([]PromptVersion, error)
}

// ValidatorConfig is injected by the composition root.
type ValidatorConfig struct {
	// TTSEnabled reports whether a TTS vendor (ELEVENLABS, CARTESIA) is enabled by its
	// <VENDOR>_TTS_ENABLED flag (D-017).
	TTSEnabled func(provider string) bool
	// TTSModeReal is true when TTS runs against real vendors (PROVIDER_MODE(_TTS)=real).
	TTSModeReal bool
}

// NewValidator builds the validator.
func NewValidator(repo ValidatorRepo, cfg ValidatorConfig) Validator {
	if cfg.TTSEnabled == nil {
		cfg.TTSEnabled = func(string) bool { return true }
	}
	return &validator{repo: repo, cfg: cfg}
}

type validator struct {
	repo ValidatorRepo
	cfg  ValidatorConfig
}

// collector gathers the issues of one entity.
type collector struct {
	entity string
	issues []Issue
}

func (c *collector) add(sev, field, rule, format string, a ...any) {
	c.issues = append(c.issues, Issue{Severity: sev, Entity: c.entity, Field: field, Rule: rule, Message: fmt.Sprintf(format, a...)})
}

func (c *collector) err(field, rule, format string, a ...any) {
	c.add(SeverityError, field, rule, format, a...)
}
func (c *collector) warn(field, rule, format string, a ...any) {
	c.add(SeverityWarning, field, rule, format, a...)
}

// length checks min ≤ runes ≤ max of a trimmed value.
func (c *collector) length(field, value string, lo, hi int) {
	n := utf8.RuneCountInString(strings.TrimSpace(value))
	switch {
	case n < lo && lo == 1:
		c.err(field, "required", "%s is required", field)
	case n < lo:
		c.err(field, "min_length", "%s must have at least %d characters", field, lo)
	case n > hi:
		c.err(field, "max_length", "%s must have at most %d characters", field, hi)
	}
}

var (
	japaneseRE = regexp.MustCompile(`[\p{Hiragana}\p{Katakana}\p{Han}]`)
	readingRE  = regexp.MustCompile(`^[\p{Hiragana}\p{Katakana}ー・]+$`)
)

var validSkills = map[string]bool{
	"GRAMMAR": true, "VOCABULARY": true, "FLUENCY": true, "NATURALNESS": true, "LISTENING_RESPONSE": true, "INTERACTION_COMPLEXITY": true,
}

// defaultBand is the default recommended range per difficulty_level (spec 6.3.2).
var defaultBand = [...][2]float64{1: {0.5, 1.5}, 2: {1.0, 2.5}, 3: {2.0, 3.5}, 4: {3.0, 4.5}, 5: {4.0, 5.0}}

func (v *validator) scenarioIssues(sv ScenarioVersion) []Issue {
	c := &collector{entity: "scenario_versions/" + sv.ID.String()}
	if sv.DifficultyLevel < 1 || sv.DifficultyLevel > 5 {
		c.err("difficulty_level", "range", "difficulty_level must be 1–5")
	}
	if sv.RecommendedLevelMin < 0 || sv.RecommendedLevelMin > 5 || sv.RecommendedLevelMax < 0 || sv.RecommendedLevelMax > 5 || sv.RecommendedLevelMin > sv.RecommendedLevelMax {
		c.err("recommended_level_min", "range", "recommended levels must be within 0.0–5.0 with min ≤ max")
	} else if sv.DifficultyLevel >= 1 && sv.DifficultyLevel <= 5 {
		b := defaultBand[sv.DifficultyLevel]
		if sv.RecommendedLevelMax < b[0] || sv.RecommendedLevelMin > b[1] {
			c.warn("recommended_level_min", "band_mismatch", "the recommended range does not overlap the default band of difficulty %d", sv.DifficultyLevel)
		}
	}
	c.length("situation", sv.Situation, 1, 2000)
	c.length("ai_role", sv.AIRole, 1, 300)
	c.length("user_role", sv.UserRole, 1, 300)
	c.length("goal", sv.Goal, 1, 500)
	checkList := func(field string, items []string, maxItems, maxLen int) {
		if len(items) > maxItems {
			c.err(field, "max_items", "%s has at most %d items", field, maxItems)
		}
		for i, it := range items {
			c.length(fmt.Sprintf("%s[%d]", field, i), it, 1, maxLen)
		}
	}
	checkList("tasks", sv.Tasks, 6, 200)
	checkList("constraints", sv.Constraints, 8, 200)
	checkList("grammar_targets", sv.GrammarTargets, 8, 100)

	if len(sv.VocabHints) > 20 {
		c.err("vocab_hints", "max_items", "vocab_hints has at most 20 items")
	}
	seen := map[string]bool{}
	for i, h := range sv.VocabHints {
		f := fmt.Sprintf("vocab_hints[%d]", i)
		c.length(f+".term_ja", h.TermJA, 1, 30)
		if h.TermJA != "" && !japaneseRE.MatchString(h.TermJA) {
			c.err(f+".term_ja", "japanese_required", "term_ja must contain Japanese characters")
		}
		c.length(f+".reading", h.Reading, 1, 40)
		if h.Reading != "" && !readingRE.MatchString(h.Reading) {
			c.err(f+".reading", "reading_kana", "reading must be Hiragana or Katakana")
		}
		c.length(f+".meaning", h.Meaning, 1, 80)
		if seen[h.TermJA] {
			c.err(f+".term_ja", "duplicate", "term_ja %q appears twice", h.TermJA)
		}
		seen[h.TermJA] = true
	}
	if len(sv.EvaluationFocus) > 3 {
		c.err("evaluation_focus", "max_items", "evaluation_focus has at most 3 items")
	}
	skills := map[string]bool{}
	for i, s := range sv.EvaluationFocus {
		f := fmt.Sprintf("evaluation_focus[%d]", i)
		if !validSkills[s] {
			c.err(f, "invalid_skill", "%q is not a skill name", s)
		}
		if skills[s] {
			c.err(f, "duplicate", "%q appears twice", s)
		}
		skills[s] = true
	}
	switch sv.Formality {
	case FormalityCasual, FormalityPolite, FormalityKeigo:
	default:
		c.err("formality", "invalid_enum", "formality must be CASUAL, POLITE or KEIGO")
	}
	c.length("opening_line", sv.OpeningLine, 1, 200)
	if sv.OpeningLine != "" && !japaneseRE.MatchString(sv.OpeningLine) {
		c.err("opening_line", "japanese_required", "opening_line must contain Japanese characters")
	}
	if strings.Contains(sv.OpeningLine, "{{") || strings.Contains(sv.OpeningLine, "<") {
		c.err("opening_line", "markup_not_allowed", "opening_line must not contain placeholders or markup")
	}
	vp := sv.VisualPrompt
	c.length("visual_prompt.headline", vp.Headline, 0, 80)
	c.length("visual_prompt.body", vp.Body, 0, 400)
	if len(vp.StarterPhrasesJA) > 3 {
		c.err("visual_prompt.starter_phrases_ja", "max_items", "at most 3 starter phrases")
	}
	for i, p := range vp.StarterPhrasesJA {
		c.length(fmt.Sprintf("visual_prompt.starter_phrases_ja[%d]", i), p, 1, 60)
	}
	if vp.ImageKey != nil {
		c.length("visual_prompt.image_key", *vp.ImageKey, 1, 100)
	}
	return c.issues
}

// NormalizeCharacter upper-cases the vendor names (primary and fallback), as admin input and
// seeds may use lower case.
func NormalizeCharacter(ch Character) Character {
	ch.ProviderName = NormalizeProviderName(ch.ProviderName)
	if ch.Voice.Fallback != nil {
		fb := *ch.Voice.Fallback
		fb.ProviderName = NormalizeProviderName(fb.ProviderName)
		ch.Voice.Fallback = &fb
	}
	return ch
}

var ttsProviders = map[string]bool{"ELEVENLABS": true, "CARTESIA": true}

const placeholderPrefix = "PLACEHOLDER_"

func (v *validator) characterIssues(ch Character) []Issue {
	ch = NormalizeCharacter(ch)
	c := &collector{entity: "characters/" + ch.Code}
	c.length("name", ch.Name, 1, 50)
	if ch.AvatarKey != nil {
		c.length("avatar_key", *ch.AvatarKey, 1, 100)
	}
	c.length("persona", ch.Persona, 50, 3000)
	if ch.GreetingHint != nil {
		c.length("greeting_hint", *ch.GreetingHint, 1, 200)
	}
	// prefix "" names the primary columns (provider_name, …); "voice.fallback" the backup object
	binding := func(prefix string, b TTSBinding) {
		name := func(n string) string {
			if prefix == "" {
				return n
			}
			return prefix + "." + n
		}
		if !ttsProviders[b.ProviderName] {
			c.err(name("provider_name"), "invalid_enum", "provider_name must be ELEVENLABS or CARTESIA")
		}
		c.length(name("model_name"), b.ModelName, 1, 200)
		c.length(name("voice_id"), b.VoiceID, 1, 200)
		if strings.HasPrefix(b.VoiceID, placeholderPrefix) {
			if v.cfg.TTSModeReal {
				c.err(name("voice_id"), "voice_placeholder", "voice_id is still a placeholder")
			} else {
				c.warn(name("voice_id"), "voice_placeholder", "voice_id is still a placeholder")
			}
		}
	}
	binding("", ch.PrimaryTTSBinding())

	switch ch.Voice.Gender {
	case GenderFemale, GenderMale:
	default:
		c.err("voice.gender", "invalid_enum", "gender must be FEMALE or MALE")
	}
	switch ch.Voice.Accent {
	case AccentKanto, AccentKansai:
	default:
		c.err("voice.accent", "invalid_enum", "accent must be KANTO or KANSAI")
	}
	if ch.Voice.SpeakingRate < 0.80 || ch.Voice.SpeakingRate > 1.20 {
		c.err("voice.speaking_rate", "range", "speaking_rate must be within 0.80–1.20")
	}
	if fb := ch.Voice.Fallback; fb != nil {
		binding("voice.fallback", *fb)
		if fb.ProviderName == ch.ProviderName && fb.ProviderName != "" {
			c.err("voice.fallback.provider_name", "fallback_same_provider", "the fallback vendor must differ from the primary vendor")
		}
		if ttsProviders[fb.ProviderName] && !v.cfg.TTSEnabled(fb.ProviderName) {
			c.warn("voice.fallback.provider_name", "fallback_provider_not_configured", "the fallback vendor %s is not enabled", fb.ProviderName)
		}
	}
	if ch.Status == StatusActive && ttsProviders[ch.ProviderName] && !v.cfg.TTSEnabled(ch.ProviderName) {
		c.err("provider_name", "provider_not_configured", "the TTS vendor %s is not enabled", ch.ProviderName)
	}
	if ch.Voice.Accent == AccentKansai {
		low := strings.ToLower(ch.Persona)
		if !strings.Contains(low, "kansai") && !strings.Contains(ch.Persona, "関西") {
			c.warn("persona", "kansai_persona", "accent KANSAI but the persona does not mention Kansai")
		}
	}
	return c.issues
}

// PrimaryTTSBinding is the character's main TTS binding (columns provider_name/model_name/voice_id).
func (c Character) PrimaryTTSBinding() TTSBinding {
	return TTSBinding{ProviderName: c.ProviderName, ModelName: c.ModelName, VoiceID: c.VoiceID}
}

// ParseVoice reads characters.voice from JSON strictly: only gender, accent, speaking_rate and
// fallback (an object with provider_name, model_name, voice_id) are allowed; anything else is an
// unknown_key issue. Value rules (ranges, enums) are checked by ValidateCharacter.
func ParseVoice(raw []byte) (Voice, []Issue) {
	c := &collector{entity: "characters/voice"}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		c.err("voice", "invalid_type", "voice must be a JSON object")
		return Voice{}, c.issues
	}
	var v Voice
	for k, val := range top {
		switch k {
		case "gender":
			var s string
			if json.Unmarshal(val, &s) != nil {
				c.err("voice.gender", "invalid_type", "gender must be a string")
			}
			v.Gender = Gender(s)
		case "accent":
			var s string
			if json.Unmarshal(val, &s) != nil {
				c.err("voice.accent", "invalid_type", "accent must be a string")
			}
			v.Accent = Accent(s)
		case "speaking_rate":
			if json.Unmarshal(val, &v.SpeakingRate) != nil {
				c.err("voice.speaking_rate", "invalid_type", "speaking_rate must be a number")
			}
		case "fallback":
			if bytes.Equal(bytes.TrimSpace(val), []byte("null")) {
				continue
			}
			var fb map[string]json.RawMessage
			if json.Unmarshal(val, &fb) != nil {
				c.err("voice.fallback", "invalid_type", "fallback must be an object")
				continue
			}
			b := TTSBinding{}
			for fk, fv := range fb {
				var s string
				switch fk {
				case "provider_name", "model_name", "voice_id":
					if json.Unmarshal(fv, &s) != nil {
						c.err("voice.fallback."+fk, "invalid_type", "%s must be a string", fk)
					}
					switch fk {
					case "provider_name":
						b.ProviderName = s
					case "model_name":
						b.ModelName = s
					default:
						b.VoiceID = s
					}
				default:
					c.err("voice.fallback."+fk, "unknown_key", "unknown key %q in voice.fallback", fk)
				}
			}
			v.Fallback = &b
		default:
			c.err("voice."+k, "unknown_key", "unknown key %q in voice", k)
		}
	}
	return v, c.issues
}

// ParseLLMParams reads prompt_versions.llm_params strictly: only temperature (0–2),
// max_output_tokens (16–8192), top_p (0–1) and seed (0–2147483647); a model name is not allowed.
func ParseLLMParams(raw []byte) (LLMParams, []Issue) {
	c := &collector{entity: "prompt_versions/llm_params"}
	var p LLMParams
	var top map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		c.err("llm_params", "invalid_type", "llm_params must be a JSON object")
		return p, c.issues
	}
	for k, val := range top {
		switch k {
		case "temperature":
			var f float64
			if json.Unmarshal(val, &f) != nil {
				c.err(k, "invalid_type", "temperature must be a number")
			}
			p.Temperature = &f
		case "max_output_tokens":
			var n int
			if json.Unmarshal(val, &n) != nil {
				c.err(k, "invalid_type", "max_output_tokens must be an integer")
			}
			p.MaxOutputTokens = &n
		case "top_p":
			var f float64
			if json.Unmarshal(val, &f) != nil {
				c.err(k, "invalid_type", "top_p must be a number")
			}
			p.TopP = &f
		case "seed":
			var n int64
			if json.Unmarshal(val, &n) != nil {
				c.err(k, "invalid_type", "seed must be an integer")
			}
			p.Seed = &n
		default:
			c.err("llm_params."+k, "unknown_key", "unknown key %q: the model name does not belong here", k)
		}
	}
	return p, c.issues
}

var requiredUser = map[PromptKey]bool{PromptSummary: true, PromptEvaluation: true, PromptMemoryExtraction: true}
var requiredSchema = map[PromptKey]bool{PromptEvaluation: true, PromptMemoryExtraction: true}

const maxRenderedRunes = 12000

func countPlaceholder(tpl, ph string) int { return strings.Count(tpl, ph) }

func (v *validator) promptIssues(pv PromptVersion) []Issue {
	c := &collector{entity: fmt.Sprintf("prompt_versions/%s-v%d", pv.Key, pv.Version)}
	switch pv.Key {
	case PromptConversation, PromptOpening, PromptSummary, PromptEvaluation, PromptMemoryExtraction:
	default:
		c.err("key", "invalid_enum", "unknown prompt key %q", pv.Key)
		return c.issues
	}
	if n := len(pv.SystemTemplate); n < 1 {
		c.err("system_template", "required", "system_template is required")
	} else if n > 16384 {
		c.err("system_template", "max_length", "system_template has at most 16384 bytes")
	}
	conversational := pv.Key == PromptConversation || pv.Key == PromptOpening
	switch {
	case conversational && pv.UserTemplate != nil:
		c.err("user_template", "must_be_null", "%s has no user_template", pv.Key)
	case requiredUser[pv.Key] && (pv.UserTemplate == nil || *pv.UserTemplate == ""):
		c.err("user_template", "required", "%s requires a user_template", pv.Key)
	case pv.UserTemplate != nil && len(*pv.UserTemplate) > 16384:
		c.err("user_template", "max_length", "user_template has at most 16384 bytes")
	}
	hasSchema := len(bytes.TrimSpace(pv.OutputSchema)) > 0 && !bytes.Equal(bytes.TrimSpace(pv.OutputSchema), []byte("null"))
	switch {
	case conversational && hasSchema:
		c.err("output_schema", "must_be_null", "%s has no output_schema", pv.Key)
	case requiredSchema[pv.Key] && !hasSchema:
		c.err("output_schema", "required", "%s requires an output_schema", pv.Key)
	case hasSchema:
		if msg := compileSchema(pv.OutputSchema); msg != "" {
			c.err("output_schema", "invalid_json_schema", "%s", msg)
		}
	}
	{
		p := pv.LLMParams
		if p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2) {
			c.err("llm_params.temperature", "range", "temperature must be within 0–2")
		}
		if p.MaxOutputTokens != nil && (*p.MaxOutputTokens < 16 || *p.MaxOutputTokens > 8192) {
			c.err("llm_params.max_output_tokens", "range", "max_output_tokens must be within 16–8192")
		}
		if p.TopP != nil && (*p.TopP < 0 || *p.TopP > 1) {
			c.err("llm_params.top_p", "range", "top_p must be within 0–1")
		}
		if p.Seed != nil && (*p.Seed < 0 || *p.Seed > 2147483647) {
			c.err("llm_params.seed", "range", "seed must be within 0–2147483647")
		}
		if p.Temperature == nil {
			c.warn("llm_params.temperature", "missing_param", "temperature is absent: the code default applies")
		}
		if p.MaxOutputTokens == nil {
			c.warn("llm_params.max_output_tokens", "missing_param", "max_output_tokens is absent: the code default applies")
		}
	}
	if conversational {
		for _, ph := range []string{"{{.CoreRules}}", "{{.OutputContract}}"} {
			if n := countPlaceholder(pv.SystemTemplate, ph); n != 1 {
				c.err("system_template", "placeholder_count", "%s must appear exactly once, found %d", ph, n)
			}
		}
	}
	// dry-run against every fixture of the key
	fixtures := prompt.Fixtures(string(pv.Key))
	if len(fixtures) == 0 {
		c.err("system_template", "missing_fixture", "no fixture is registered for key %s", pv.Key)
		return c.issues
	}
	tpl := pv.Template()
	for i, fx := range fixtures {
		out, err := prompt.Render(tpl, fx)
		if err != nil {
			c.err("system_template", "template_render", "fixture %d: %v", i, err)
			break
		}
		if pv.Key == PromptConversation && i == len(fixtures)-1 && utf8.RuneCountInString(out.System) > maxRenderedRunes {
			c.err("system_template", "render_too_long", "the maximal fixture renders %d characters (limit %d)", utf8.RuneCountInString(out.System), maxRenderedRunes)
		}
	}
	return c.issues
}

// compileSchema returns "" when raw is a valid draft 2020-12 schema with an object root.
func compileSchema(raw json.RawMessage) string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "output_schema is not valid JSON"
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["type"] != "object" {
		return `the root of output_schema must be {"type":"object"}`
	}
	comp := jsonschema.NewCompiler()
	comp.DefaultDraft(jsonschema.Draft2020)
	if err := comp.AddResource("schema.json", doc); err != nil {
		return "output_schema cannot be loaded: " + err.Error()
	}
	if _, err := comp.Compile("schema.json"); err != nil {
		return "output_schema is not a valid JSON Schema: " + err.Error()
	}
	return ""
}

func deref[T any](v any) (T, bool) {
	switch x := v.(type) {
	case *T:
		if x != nil {
			return *x, true
		}
	case T:
		return x, true
	}
	var zero T
	return zero, false
}

// ValidateDraft checks the content rules of a scenario or prompt version value.
func (v *validator) ValidateDraft(_ context.Context, family Family, val any) []Issue {
	switch family {
	case FamilyScenarios:
		if sv, ok := deref[ScenarioVersion](val); ok {
			return v.scenarioIssues(sv)
		}
	case FamilyPrompts:
		if pv, ok := deref[PromptVersion](val); ok {
			return v.promptIssues(pv)
		}
	default:
		return []Issue{{Severity: SeverityError, Entity: string(family), Rule: "invalid_family", Message: "family must be scenarios or prompts"}}
	}
	return []Issue{{Severity: SeverityError, Entity: string(family), Rule: "invalid_type", Message: fmt.Sprintf("a %s value was expected, got %T", family, val)}}
}

// ValidatePublish repeats the content rules on the stored version and adds the cross
// references: a scenario version needs a scenario and topic that are not ARCHIVED.
func (v *validator) ValidatePublish(ctx context.Context, family Family, id uuid.UUID) []Issue {
	notFound := func(entity string) []Issue {
		return []Issue{{Severity: SeverityError, Entity: entity, Rule: "not_found", Message: "version not found"}}
	}
	switch family {
	case FamilyScenarios:
		sv, err := v.repo.GetScenarioVersion(ctx, id)
		if err != nil {
			return notFound("scenario_versions/" + id.String())
		}
		return append(v.scenarioIssues(sv), v.parentIssues(ctx, sv)...)
	case FamilyPrompts:
		pv, err := v.repo.GetPromptVersion(ctx, id)
		if err != nil {
			return notFound("prompt_versions/" + id.String())
		}
		return v.promptIssues(pv)
	}
	return []Issue{{Severity: SeverityError, Entity: string(family), Rule: "invalid_family", Message: "family must be scenarios or prompts"}}
}

func (v *validator) parentIssues(ctx context.Context, sv ScenarioVersion) []Issue {
	c := &collector{entity: "scenario_versions/" + sv.ID.String()}
	scn, err := v.repo.GetScenario(ctx, sv.ScenarioID)
	if err != nil {
		c.err("scenario_id", "parent_missing", "the scenario does not exist")
		return c.issues
	}
	if scn.Status == StatusArchived {
		c.err("scenario_id", "parent_archived", "the scenario is ARCHIVED")
	}
	topic, err := v.repo.GetTopic(ctx, scn.TopicID)
	if err != nil {
		c.err("scenario_id", "parent_missing", "the topic does not exist")
	} else if topic.Status == StatusArchived {
		c.err("scenario_id", "parent_archived", "the topic is ARCHIVED")
	}
	return c.issues
}

// ValidateCharacter checks the content and voice rules of a character.
func (v *validator) ValidateCharacter(_ context.Context, ch Character) []Issue {
	return v.characterIssues(ch)
}

// ValidateCatalog checks every PUBLISHED scenario and prompt version and every ACTIVE character.
// A prompt key without a PUBLISHED version is an ERROR for CONVERSATION and a WARNING for the
// others (their versions arrive with M7/M8 together with their fixtures).
func (v *validator) ValidateCatalog(ctx context.Context) []Issue {
	var out []Issue
	if svs, err := v.repo.ListPublishedScenarioVersions(ctx); err != nil {
		out = append(out, Issue{Severity: SeverityError, Entity: "scenario_versions", Rule: "repo_error", Message: err.Error()})
	} else {
		for _, sv := range svs {
			out = append(out, v.scenarioIssues(sv)...)
			out = append(out, v.parentIssues(ctx, sv)...)
		}
	}
	if chs, err := v.repo.ListActiveCharacters(ctx); err != nil {
		out = append(out, Issue{Severity: SeverityError, Entity: "characters", Rule: "repo_error", Message: err.Error()})
	} else {
		for _, ch := range chs {
			out = append(out, v.characterIssues(ch)...)
		}
	}
	pvs, err := v.repo.ListPublishedPromptVersions(ctx)
	if err != nil {
		return append(out, Issue{Severity: SeverityError, Entity: "prompt_versions", Rule: "repo_error", Message: err.Error()})
	}
	published := map[PromptKey]bool{}
	for _, pv := range pvs {
		published[pv.Key] = true
		out = append(out, v.promptIssues(pv)...)
	}
	for _, key := range []PromptKey{PromptConversation, PromptOpening, PromptSummary, PromptEvaluation, PromptMemoryExtraction} {
		if published[key] {
			continue
		}
		sev := SeverityWarning
		if key == PromptConversation {
			sev = SeverityError
		}
		out = append(out, Issue{Severity: sev, Entity: "prompt_configs/" + string(key), Field: "status", Rule: "missing_published_prompt",
			Message: fmt.Sprintf("no PUBLISHED version of %s", key)})
	}
	return out
}

// Observer receives content metrics; *metrics.Content implements it.
type Observer interface {
	ResolveError(code string)
	VoiceFallbackMissing(character string)
	ValidationIssues(severity string, n int)
}

// NopObserver ignores everything.
type NopObserver struct{}

// ResolveError implements Observer.
func (NopObserver) ResolveError(string) {}

// VoiceFallbackMissing implements Observer.
func (NopObserver) VoiceFallbackMissing(string) {}

// ValidationIssues implements Observer.
func (NopObserver) ValidationIssues(string, int) {}

// ReportCatalog validates the whole catalog once at startup: it logs every ERROR, sets the
// issue gauges and returns the issues. It never fails: broken content only hides the entity
// from the catalog (spec 6.10.3).
func ReportCatalog(ctx context.Context, v Validator, obs Observer, log *slog.Logger) []Issue {
	if obs == nil {
		obs = NopObserver{}
	}
	if log == nil {
		log = slog.Default()
	}
	issues := v.ValidateCatalog(ctx)
	var nErr, nWarn int
	for _, is := range issues {
		if is.Severity == SeverityError {
			nErr++
			log.ErrorContext(ctx, "content validation error", "entity", is.Entity, "field", is.Field, "rule", is.Rule, "message", is.Message)
		} else {
			nWarn++
		}
	}
	obs.ValidationIssues(SeverityError, nErr)
	obs.ValidationIssues(SeverityWarning, nWarn)
	return issues
}
