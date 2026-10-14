package content

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Gender of a character voice.
type Gender string

// Genders.
const (
	GenderFemale Gender = "FEMALE"
	GenderMale   Gender = "MALE"
)

// Accent of a character voice.
type Accent string

// Accents.
const (
	AccentKanto  Accent = "KANTO"
	AccentKansai Accent = "KANSAI"
)

// TTSBinding is one vendor/model/voice triple: the primary one in the characters columns, the
// backup in voice.fallback (D-015, D-017).
type TTSBinding struct {
	ProviderName string `json:"provider_name"` // ELEVENLABS | CARTESIA, UPPERCASE
	ModelName    string `json:"model_name"`    // free text 1–200
	VoiceID      string `json:"voice_id"`      // 1–200; "PLACEHOLDER_…" is rejected in real mode
}

// Voice is characters.voice.
type Voice struct {
	Gender       Gender      `json:"gender"`
	Accent       Accent      `json:"accent"`
	SpeakingRate float64     `json:"speaking_rate"`      // 0.80–1.20
	Fallback     *TTSBinding `json:"fallback,omitempty"` // nil: no TTS fallback
}

// Character is an AI persona; one row, no versions (D-014).
type Character struct {
	ID                   uuid.UUID
	Code                 string
	Status               EntityStatus
	SortOrder            int
	Name                 string
	AvatarKey            *string
	Persona              string
	GreetingHint         *string
	ProviderName         string // primary TTS (D-015)
	ModelName            string
	VoiceID              string
	Voice                Voice
	CreatedAt, UpdatedAt time.Time
}

// CharacterSnapshot is the character part of sessions.config_snapshot (spec 6.3.5); immutable
// once the session row exists.
type CharacterSnapshot struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	Name         string    `json:"name"`
	AvatarKey    *string   `json:"avatar_key,omitempty"`
	Persona      string    `json:"persona"`
	GreetingHint *string   `json:"greeting_hint,omitempty"`
	ProviderName string    `json:"provider_name"`
	ModelName    string    `json:"model_name"`
	VoiceID      string    `json:"voice_id"`
	Voice        Voice     `json:"voice"`
}

// SessionSnapshot is the whole sessions.config_snapshot document.
type SessionSnapshot struct {
	Character CharacterSnapshot `json:"character"`
	Formality Formality         `json:"formality"` // scenario_versions.formality at resolve time
}

// PrimaryTTS is the character's main TTS binding.
func (c CharacterSnapshot) PrimaryTTS() TTSBinding {
	return TTSBinding{ProviderName: c.ProviderName, ModelName: c.ModelName, VoiceID: c.VoiceID}
}

// FallbackTTS is the backup binding from voice.fallback; ok is false when there is none.
func (c CharacterSnapshot) FallbackTTS() (TTSBinding, bool) {
	if c.Voice.Fallback == nil {
		return TTSBinding{}, false
	}
	return *c.Voice.Fallback, true
}

// NormalizeProviderName trims and upper-cases a vendor name.
func NormalizeProviderName(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// MarshalJSONB renders the snapshot for the config_snapshot column.
func (s SessionSnapshot) MarshalJSONB() ([]byte, error) { return json.Marshal(s) }

// ParseSnapshot reads a config_snapshot document; unknown keys at any level are an error.
func ParseSnapshot(raw []byte) (SessionSnapshot, error) {
	var s SessionSnapshot
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return SessionSnapshot{}, fmt.Errorf("content: invalid config_snapshot: %w", err)
	}
	return s, nil
}
