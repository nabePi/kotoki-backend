package kotokiclient

import (
	"encoding/json"

	"github.com/google/uuid"
)

// The DTOs below mirror the JSON of the REST API (spec Bab 5/6; httpapi M2/M3). They are copies on
// purpose: the client never imports httpapi. Unknown response fields are ignored.

// RegisterRequest is POST /auth/register.
type RegisterRequest struct {
	Email        string  `json:"email"`
	Password     string  `json:"password"`
	FullName     string  `json:"full_name"`
	PhoneNumber  *string `json:"phone_number"`
	TermsVersion string  `json:"terms_version"`
}

// User is the Me object (spec 5.9.2).
type User struct {
	ID                  string  `json:"id"`
	Email               string  `json:"email"`
	FullName            string  `json:"full_name"`
	PhoneNumber         *string `json:"phone_number"`
	Role                string  `json:"role"`
	Status              string  `json:"status"`
	TermsVersion        string  `json:"terms_version"`
	TermsCurrentVersion string  `json:"terms_current_version"`
	TermsAcceptedAt     string  `json:"terms_accepted_at"`
	CreatedAt           string  `json:"created_at"`
	LastLoginAt         *string `json:"last_login_at"`
}

// Topic is one entry of GET /topics.
type Topic struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	NameJA        string    `json:"name_ja"`
	Description   string    `json:"description"`
	IconKey       *string   `json:"icon_key"`
	ScenarioCount int       `json:"scenario_count"`
}

// VocabHint is a scenario vocabulary hint.
type VocabHint struct {
	TermJA  string `json:"term_ja"`
	Reading string `json:"reading"`
	Meaning string `json:"meaning"`
}

// VisualPrompt is the scenario visual prompt.
type VisualPrompt struct {
	Headline         string   `json:"headline"`
	Body             string   `json:"body"`
	StarterPhrasesJA []string `json:"starter_phrases_ja"`
	ImageKey         *string  `json:"image_key"`
}

// Scenario is one entry of GET /scenarios.
type Scenario struct {
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
	Formality           string       `json:"formality"`
	VisualPrompt        VisualPrompt `json:"visual_prompt"`
}

// Character is one entry of GET /characters (no vendor or persona data).
type Character struct {
	ID        uuid.UUID `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	AvatarKey *string   `json:"avatar_key"`
	Gender    string    `json:"gender"`
	Accent    string    `json:"accent"`
}

// Catalog is the active content a user can start a session from: topics, scenarios and
// characters (spec 6.12).
type Catalog struct {
	Topics     []Topic
	Scenarios  []Scenario
	Characters []Character
}

// CreateSessionRequest is POST /sessions (spec 5.9.4). IdempotencyKey is sent as the
// Idempotency-Key header (not in the body); empty means the client generates a fresh UUID.
type CreateSessionRequest struct {
	TopicID               uuid.UUID `json:"topic_id"`
	ScenarioID            uuid.UUID `json:"scenario_id"`
	CharacterID           uuid.UUID `json:"character_id"`
	DurationSeconds       int       `json:"duration_seconds"`
	ConversationStartMode string    `json:"conversation_start_mode"`
	IdempotencyKey        string    `json:"-"`
}

// Ref is an id/name pair.
type Ref struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// CharacterRef is the character of a session.
type CharacterRef struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	AvatarKey *string   `json:"avatar_key"`
}

// Session is the user view of a session.
type Session struct {
	ID                    uuid.UUID    `json:"id"`
	Status                string       `json:"status"`
	EndReason             *string      `json:"end_reason"`
	ConversationStartMode string       `json:"conversation_start_mode"`
	DurationSeconds       int          `json:"duration_seconds"`
	ReservedSeconds       int          `json:"reserved_seconds"`
	ChargedSeconds        int          `json:"charged_seconds"`
	DisconnectedSeconds   int          `json:"disconnected_seconds"`
	Topic                 Ref          `json:"topic"`
	Scenario              Ref          `json:"scenario"`
	Character             CharacterRef `json:"character"`
	StartDeadlineAt       *string      `json:"start_deadline_at"`
	CreatedAt             string       `json:"created_at"`
	StartingAt            *string      `json:"starting_at"`
	StartedAt             *string      `json:"started_at"`
	ExpiresAt             *string      `json:"expires_at"`
	EndingAt              *string      `json:"ending_at"`
	EndedAt               *string      `json:"ended_at"`
	RemainingSeconds      *int         `json:"remaining_seconds"`
	Summary               *string      `json:"summary"`
	ServerTime            string       `json:"server_time"`
}

// Turn is one transcript turn of GET /sessions/{id}/turns.
type Turn struct {
	ID                  uuid.UUID `json:"id"`
	SequenceNumber      int       `json:"sequence_number"`
	Status              string    `json:"status"`
	UserText            *string   `json:"user_text"`
	UserStartedAt       *string   `json:"user_started_at"`
	UserEndedAt         *string   `json:"user_ended_at"`
	AssistantText       *string   `json:"assistant_text"`
	AssistantSpokenText *string   `json:"assistant_spoken_text"`
	CreatedAt           string    `json:"created_at"`
	CompletedAt         *string   `json:"completed_at"`
}

// Balance is GET /balance.
type Balance struct {
	AvailableSeconds int64   `json:"available_seconds"`
	ReservedSeconds  int64   `json:"reserved_seconds"`
	AllowedDurations []int64 `json:"allowed_durations"`
	UpdatedAt        string  `json:"updated_at"`
}

// Evaluation is GET /sessions/{id}/evaluation. The endpoint arrives with M7, so only Status is
// typed; Raw keeps the whole body for printing.
type Evaluation struct {
	Status string
	Raw    json.RawMessage
}

// UnmarshalJSON keeps the raw body and extracts Status.
func (e *Evaluation) UnmarshalJSON(b []byte) error {
	var head struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	e.Status = head.Status
	e.Raw = append(e.Raw[:0], b...)
	return nil
}
