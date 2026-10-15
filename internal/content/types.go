// Package content owns scenarios, prompts and characters: versioned scenario and prompt
// content, the unversioned characters with their inline voice, the resolver that turns a
// session request into a snapshot, the validator and the catalog (spec Bab 6). It imports only
// prompt among the feature packages (spec 2.2).
package content

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// VersionStatus is the lifecycle of a scenario or prompt version.
type VersionStatus string

// Version statuses.
const (
	VersionDraft     VersionStatus = "DRAFT"
	VersionPublished VersionStatus = "PUBLISHED"
	VersionRetired   VersionStatus = "RETIRED"
)

// EntityStatus is the status of a topic, scenario or character.
type EntityStatus string

// Entity statuses.
const (
	StatusActive   EntityStatus = "ACTIVE"
	StatusDisabled EntityStatus = "DISABLED"
	StatusArchived EntityStatus = "ARCHIVED"
)

// Formality is the base register of a scenario (D-014).
type Formality string

// Formalities.
const (
	FormalityCasual Formality = "CASUAL"
	FormalityPolite Formality = "POLITE"
	FormalityKeigo  Formality = "KEIGO"
)

// PromptKey names a prompt config.
type PromptKey string

// Prompt keys.
const (
	PromptConversation     PromptKey = "CONVERSATION"
	PromptOpening          PromptKey = "OPENING"
	PromptSummary          PromptKey = "SUMMARY"
	PromptEvaluation       PromptKey = "EVALUATION"
	PromptMemoryExtraction PromptKey = "MEMORY_EXTRACTION"
)

// Family is the path family of the versioned admin resources.
type Family string

// Families.
const (
	FamilyScenarios Family = "scenarios"
	FamilyPrompts   Family = "prompts"
)

// VocabHint is defined in package prompt (template FuncMap and PromptData use it) so prompt never
// imports content.
type VocabHint = prompt.VocabHint

// LLMParams is prompt_versions.llm_params; a nil field means "key absent": the caller uses its
// own default (the database value wins, S-1).
type LLMParams = prompt.Params

// VisualPrompt is what the client shows when the user speaks first; it never reaches the LLM.
type VisualPrompt struct {
	Headline         string   `json:"headline"`
	Body             string   `json:"body"`
	StarterPhrasesJA []string `json:"starter_phrases_ja"`
	ImageKey         *string  `json:"image_key"`
}

// Topic groups scenarios.
type Topic struct {
	ID                   uuid.UUID
	Code, Name, NameJA   string
	Description          string
	IconKey              *string
	SortOrder            int
	Status               EntityStatus
	CreatedAt, UpdatedAt time.Time
}

// Scenario is a concrete situation inside a topic; its content lives in versions.
type Scenario struct {
	ID, TopicID          uuid.UUID
	Code, Name, NameJA   string
	Description          string
	SortOrder            int
	Status               EntityStatus
	CreatedAt, UpdatedAt time.Time
}

// ScenarioVersion is one immutable-once-published version of a scenario (spec 6.3.2).
type ScenarioVersion struct {
	ID, ScenarioID                           uuid.UUID
	Version                                  int
	Status                                   VersionStatus
	DifficultyLevel                          int
	RecommendedLevelMin, RecommendedLevelMax float64
	Situation, AIRole, UserRole, Goal        string
	Tasks, Constraints, GrammarTargets       []string
	VocabHints                               []VocabHint
	EvaluationFocus                          []string // D-097 skill names, 0–3
	Formality                                Formality
	OpeningLine                              string
	VisualPrompt                             VisualPrompt
	Notes                                    *string
	CreatedAt                                time.Time
	PublishedAt, RetiredAt                   *time.Time
}

// ScenarioFormality is the base formality of the content (D-014). The effective formality of a
// session (learner preference, internal KEIGO_LIGHT) is sessions.adaptive_difficulty.formality.
func ScenarioFormality(sv ScenarioVersion) Formality { return sv.Formality }

// Difficulty bands (spec 6.3.2), derived in code from difficulty_level.
var difficultyBands = [...]struct{ band, jlpt string }{
	1: {"BEGINNER", "N5"}, 2: {"ELEMENTARY", "N4"}, 3: {"INTERMEDIATE", "N3"}, 4: {"UPPER_INTERMEDIATE", "N2"}, 5: {"ADVANCED", "N1"},
}

// DifficultyBand returns the band and the JLPT label of a difficulty level 1–5; out of range
// values give empty strings.
func DifficultyBand(level int) (band, jlpt string) {
	if level < 1 || level >= len(difficultyBands) {
		return "", ""
	}
	return difficultyBands[level].band, difficultyBands[level].jlpt
}

// Style is the code-owned general speaking style (spec 6.3.6), not database data.
type Style struct {
	MaxSentencesPerTurn     int // used when AdaptiveDifficulty.MaxAISentencesPerTurn is absent
	MaxConsecutiveQuestions int
}

// DefaultStyle is the style every session starts from.
var DefaultStyle = Style{MaxSentencesPerTurn: 3, MaxConsecutiveQuestions: 2}

const (
	minSpeakingRate = 0.80
	maxSpeakingRate = 1.05
)

// SpeakingRate is the canonical tempo formula (spec 6.6.2): clamp(voice × adaptive, 0.80, 1.05);
// adaptiveRate ≤ 0 means 1.0. The result is TTSRequest.SpeakingRate.
func SpeakingRate(voiceSpeakingRate, adaptiveRate float64) float64 {
	if adaptiveRate <= 0 {
		adaptiveRate = 1.0
	}
	return min(max(voiceSpeakingRate*adaptiveRate, minSpeakingRate), maxSpeakingRate)
}

// PromptVersion is one version of a prompt config (spec 6.3.8).
type PromptVersion struct {
	ID, PromptConfigID     uuid.UUID
	Key                    PromptKey
	Version                int
	Status                 VersionStatus
	SystemTemplate         string
	UserTemplate           *string         // nil for CONVERSATION and OPENING
	OutputSchema           json.RawMessage // nil when NULL
	LLMParams              LLMParams
	Notes                  *string
	CreatedAt              time.Time
	PublishedAt, RetiredAt *time.Time
}

// Template is the shape prompt.Render takes; prompt does not know PromptVersion (import
// direction content → prompt).
func (pv PromptVersion) Template() prompt.Template {
	t := prompt.Template{Key: string(pv.Key), Version: pv.Version, System: pv.SystemTemplate, Params: pv.LLMParams, OutputSchema: pv.OutputSchema}
	if pv.UserTemplate != nil {
		t.User = *pv.UserTemplate
	}
	return t
}
