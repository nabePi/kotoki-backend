package httpapi

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// versionBase is what every version response carries.
type versionBase struct {
	ID          uuid.UUID             `json:"id"`
	Version     int                   `json:"version"`
	Status      content.VersionStatus `json:"status"`
	CreatedAt   time.Time             `json:"created_at"`
	PublishedAt *time.Time            `json:"published_at"`
	RetiredAt   *time.Time            `json:"retired_at"`
}

type scenarioVersionResponse struct {
	versionBase
	ScenarioID          uuid.UUID            `json:"scenario_id"`
	DifficultyLevel     int                  `json:"difficulty_level"`
	RecommendedLevelMin float64              `json:"recommended_level_min"`
	RecommendedLevelMax float64              `json:"recommended_level_max"`
	Situation           string               `json:"situation"`
	AIRole              string               `json:"ai_role"`
	UserRole            string               `json:"user_role"`
	Goal                string               `json:"goal"`
	Tasks               []string             `json:"tasks"`
	Constraints         []string             `json:"constraints"`
	GrammarTargets      []string             `json:"grammar_targets"`
	VocabHints          []vocabHintResponse  `json:"vocab_hints"`
	EvaluationFocus     []string             `json:"evaluation_focus"`
	Formality           content.Formality    `json:"formality"`
	OpeningLine         string               `json:"opening_line"`
	VisualPrompt        visualPromptResponse `json:"visual_prompt"`
	Notes               *string              `json:"notes"`
}

type promptVersionResponse struct {
	versionBase
	PromptConfigID uuid.UUID         `json:"prompt_config_id"`
	Key            content.PromptKey `json:"key"`
	SystemTemplate string            `json:"system_template"`
	UserTemplate   *string           `json:"user_template"`
	OutputSchema   json.RawMessage   `json:"output_schema"`
	LLMParams      content.LLMParams `json:"llm_params"`
	Notes          *string           `json:"notes"`
}

func newVersionResponse(v content.VersionView) any {
	base := versionBase{ID: v.ID, Version: v.Version, Status: v.Status, CreatedAt: v.CreatedAt, PublishedAt: v.PublishedAt, RetiredAt: v.RetiredAt}
	if sv := v.Scenario; sv != nil {
		hints := make([]vocabHintResponse, len(sv.VocabHints))
		for i, h := range sv.VocabHints {
			hints[i] = vocabHintResponse{TermJA: h.TermJA, Reading: h.Reading, Meaning: h.Meaning}
		}
		return scenarioVersionResponse{versionBase: base, ScenarioID: sv.ScenarioID, DifficultyLevel: sv.DifficultyLevel,
			RecommendedLevelMin: sv.RecommendedLevelMin, RecommendedLevelMax: sv.RecommendedLevelMax,
			Situation: sv.Situation, AIRole: sv.AIRole, UserRole: sv.UserRole, Goal: sv.Goal,
			Tasks: orEmptyStrings(sv.Tasks), Constraints: orEmptyStrings(sv.Constraints), GrammarTargets: orEmptyStrings(sv.GrammarTargets),
			VocabHints: hints, EvaluationFocus: orEmptyStrings(sv.EvaluationFocus), Formality: sv.Formality, OpeningLine: sv.OpeningLine,
			VisualPrompt: newVisualPromptResponse(sv.VisualPrompt), Notes: sv.Notes}
	}
	pv := v.Prompt
	schema := pv.OutputSchema
	if len(schema) == 0 {
		schema = json.RawMessage("null")
	}
	return promptVersionResponse{versionBase: base, PromptConfigID: pv.PromptConfigID, Key: pv.Key, SystemTemplate: pv.SystemTemplate,
		UserTemplate: pv.UserTemplate, OutputSchema: schema, LLMParams: pv.LLMParams, Notes: pv.Notes}
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyIssues(i []content.Issue) []content.Issue {
	if i == nil {
		return []content.Issue{}
	}
	return i
}

type publishResponse struct {
	Version          any             `json:"version"`
	RetiredVersionID *uuid.UUID      `json:"retired_version_id"`
	Warnings         []content.Issue `json:"warnings"`
}

type retireResponse struct {
	Version  any             `json:"version"`
	Warnings []content.Issue `json:"warnings"`
}

type entityStatusResponse struct {
	ID     uuid.UUID            `json:"id"`
	Code   string               `json:"code"`
	Name   string               `json:"name"`
	Status content.EntityStatus `json:"status"`
}

type statusRequest struct {
	Status string `json:"status"`
}

// Validate implements Validator.
func (r statusRequest) Validate() error {
	switch content.EntityStatus(r.Status) {
	case content.StatusActive, content.StatusDisabled, content.StatusArchived:
		return nil
	}
	return FieldError("status", "not_allowed")
}

type ttsBindingResponse struct {
	ProviderName string `json:"provider_name"`
	ModelName    string `json:"model_name"`
	VoiceID      string `json:"voice_id"`
}

type voiceResponse struct {
	Gender       content.Gender      `json:"gender"`
	Accent       content.Accent      `json:"accent"`
	SpeakingRate float64             `json:"speaking_rate"`
	Fallback     *ttsBindingResponse `json:"fallback,omitempty"`
}

// characterAdminResponse is the full character of the admin view: it does carry the persona and
// the vendor ids that the public view never shows.
type characterAdminResponse struct {
	ID           uuid.UUID            `json:"id"`
	Code         string               `json:"code"`
	Status       content.EntityStatus `json:"status"`
	SortOrder    int                  `json:"sort_order"`
	Name         string               `json:"name"`
	AvatarKey    *string              `json:"avatar_key"`
	Persona      string               `json:"persona"`
	GreetingHint *string              `json:"greeting_hint"`
	ProviderName string               `json:"provider_name"`
	ModelName    string               `json:"model_name"`
	VoiceID      string               `json:"voice_id"`
	Voice        voiceResponse        `json:"voice"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

func newCharacterAdminResponse(c content.Character) characterAdminResponse {
	v := voiceResponse{Gender: c.Voice.Gender, Accent: c.Voice.Accent, SpeakingRate: c.Voice.SpeakingRate}
	if fb := c.Voice.Fallback; fb != nil {
		b := ttsBindingResponse(*fb)
		v.Fallback = &b
	}
	return characterAdminResponse{ID: c.ID, Code: c.Code, Status: c.Status, SortOrder: c.SortOrder, Name: c.Name, AvatarKey: c.AvatarKey,
		Persona: c.Persona, GreetingHint: c.GreetingHint, ProviderName: c.ProviderName, ModelName: c.ModelName, VoiceID: c.VoiceID,
		Voice: v, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

type characterUpdateResponse struct {
	Character characterAdminResponse `json:"character"`
	Warnings  []content.Issue        `json:"warnings"`
}

type validationReportResponse struct {
	Errors   []content.Issue `json:"errors"`
	Warnings []content.Issue `json:"warnings"`
}

// optionalString tells an absent key (Set false) from an explicit null (Set true, Val nil).
type optionalString struct {
	Set bool
	Val *string
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *optionalString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	o.Val = &s
	return nil
}

// patchCharacterRequest is PATCH /admin/content/characters/{id}: partial; code and id do not exist
// here, so sending them is an unknown field. voice replaces the whole voice object.
type patchCharacterRequest struct {
	Status       *string         `json:"status"`
	Name         *string         `json:"name"`
	AvatarKey    optionalString  `json:"avatar_key"`
	Persona      *string         `json:"persona"`
	GreetingHint optionalString  `json:"greeting_hint"`
	SortOrder    *int            `json:"sort_order"`
	ProviderName *string         `json:"provider_name"`
	ModelName    *string         `json:"model_name"`
	VoiceID      *string         `json:"voice_id"`
	Voice        json.RawMessage `json:"voice"`
}

// patch converts the request; a malformed voice object is a 422 with the issues as details.
func (r patchCharacterRequest) patch() (content.CharacterPatch, error) {
	p := content.CharacterPatch{Name: r.Name, Persona: r.Persona, SortOrder: r.SortOrder, ProviderName: r.ProviderName, ModelName: r.ModelName, VoiceID: r.VoiceID}
	if r.Status != nil {
		st := content.EntityStatus(*r.Status)
		p.Status = &st
	}
	if r.AvatarKey.Set {
		p.AvatarKey = &r.AvatarKey.Val
	}
	if r.GreetingHint.Set {
		p.GreetingHint = &r.GreetingHint.Val
	}
	if len(r.Voice) > 0 {
		v, issues := content.ParseVoice(r.Voice)
		if len(issues) > 0 {
			return content.CharacterPatch{}, errs.WithDetails(errs.New(errs.CodeContentValidationFailed, "content validation failed"), issues)
		}
		p.Voice = &v
	}
	return p, nil
}
