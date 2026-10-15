package content

import (
	"time"

	"github.com/google/uuid"
)

// TopicView is one item of GET /topics (spec 6.12.1).
type TopicView struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	NameJA        string    `json:"name_ja"`
	Description   string    `json:"description"`
	IconKey       *string   `json:"icon_key"`
	ScenarioCount int       `json:"scenario_count"`
}

// ScenarioView is a scenario of GET /scenarios (spec 6.12.2); it never carries opening_line or
// constraints.
type ScenarioView struct {
	ID                  uuid.UUID    `json:"id"`
	TopicID             uuid.UUID    `json:"topic_id"`
	Code                string       `json:"code"`
	Name                string       `json:"name"`
	NameJA              string       `json:"name_ja"`
	Description         string       `json:"description"`
	VersionID           uuid.UUID    `json:"version_id"`
	Version             int          `json:"version"`
	DifficultyLevel     int          `json:"difficulty_level"`
	DifficultyBand      string       `json:"difficulty_band"`
	JLPTLabel           string       `json:"jlpt_label"`
	RecommendedLevelMin float64      `json:"recommended_level_min"`
	RecommendedLevelMax float64      `json:"recommended_level_max"`
	Situation           string       `json:"situation"`
	AIRole              string       `json:"ai_role"`
	UserRole            string       `json:"user_role"`
	Goal                string       `json:"goal"`
	Tasks               []string     `json:"tasks"`
	VocabHints          []VocabHint  `json:"vocab_hints"`
	GrammarTargets      []string     `json:"grammar_targets"`
	Formality           Formality    `json:"formality"`
	VisualPrompt        VisualPrompt `json:"visual_prompt"`
}

// CharacterView is a character of GET /characters (spec 6.12.3): no persona, greeting hint,
// vendor, model, voice id, fallback or speaking rate.
type CharacterView struct {
	ID        uuid.UUID `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	AvatarKey *string   `json:"avatar_key"`
	Gender    Gender    `json:"gender"`
	Accent    Accent    `json:"accent"`
}

// VersionView is a scenario or prompt version as the admin endpoints return it; exactly one of
// Scenario and Prompt is set.
type VersionView struct {
	ID          uuid.UUID        `json:"id"`
	ParentID    uuid.UUID        `json:"parent_id"`
	Version     int              `json:"version"`
	Status      VersionStatus    `json:"status"`
	CreatedAt   time.Time        `json:"created_at"`
	PublishedAt *time.Time       `json:"published_at"`
	RetiredAt   *time.Time       `json:"retired_at"`
	Scenario    *ScenarioVersion `json:"-"`
	Prompt      *PromptVersion   `json:"-"`
}

// PublishResult is the outcome of Publish.
type PublishResult struct {
	Version          VersionView
	RetiredVersionID *uuid.UUID
	Warnings         []Issue
}

// RetireResult is the outcome of Retire.
type RetireResult struct {
	Version  VersionView
	Warnings []Issue
}

// EntityView is a topic or scenario after a status change.
type EntityView struct {
	ID     uuid.UUID    `json:"id"`
	Code   string       `json:"code"`
	Name   string       `json:"name"`
	Status EntityStatus `json:"status"`
}

// CharacterPatch is a partial edit of a character (spec 6.13.1). A nil field is untouched; Voice
// replaces the whole voice object; AvatarKey and GreetingHint use a double pointer so null can
// clear them. code and id are not editable, so the type does not have them.
type CharacterPatch struct {
	Status       *EntityStatus
	Name         *string
	AvatarKey    **string
	Persona      *string
	GreetingHint **string
	SortOrder    *int
	ProviderName *string
	ModelName    *string
	VoiceID      *string
	Voice        *Voice
}

// CharacterUpdateResult is the outcome of UpdateCharacter.
type CharacterUpdateResult struct {
	Character Character
	Warnings  []Issue
}
