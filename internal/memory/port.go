// Package memory holds the retrieval port the realtime runtime consumes (spec 11.7.1, S-15). The
// implementation arrives with M8.
package memory

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Category is the kind of a memory item.
type Category string

// Memory categories.
const (
	CategoryProfileFact     Category = "PROFILE_FACT"
	CategoryPreference      Category = "PREFERENCE"
	CategoryLearningPattern Category = "LEARNING_PATTERN"
	CategoryEpisodic        Category = "EPISODIC"
)

// ContextItem is one memory line destined for the prompt.
type ContextItem struct {
	ID         uuid.UUID `json:"id"`
	Category   Category  `json:"category"`
	Content    string    `json:"content"`     // Japanese, sanitised
	ObservedAt time.Time `json:"observed_at"` // last_observed_at (EPISODIC: date of the source session)
	Score      float64   `json:"score"`       // priority (core) or similarity (episodic)
	Tokens     int       `json:"tokens"`
}

// MemoryContext is the memory block of a session. The name is the one of spec 11.7.1.
type MemoryContext struct { //nolint:revive // name fixed by spec 11.7.1
	SessionID      uuid.UUID     `json:"session_id"`
	UserID         uuid.UUID     `json:"user_id"`      // owner; lets ReloadCoreIfChanged work from the context alone (M8-T16)
	UserMemVer     int64         `json:"user_mem_ver"` // kotoki:user:{id}:memver when built
	Core           []ContextItem `json:"core"`         // PROFILE_FACT, PREFERENCE, LEARNING_PATTERN
	CoreTokens     int           `json:"core_tokens"`
	Episodic       []ContextItem `json:"episodic"`
	EpisodicTokens int           `json:"episodic_tokens"`
	BuiltAt        time.Time     `json:"built_at"`
	Degraded       bool          `json:"degraded"` // embedding or DB failed during the build
}

// SessionContextInput is the input of BuildSessionContext.
type SessionContextInput struct {
	UserID       uuid.UUID
	SessionID    uuid.UUID
	ScenarioText string // topic name + scenario title + description (Japanese), ≤ 500 runes
}

// TurnText is the text of one finished turn.
type TurnText struct{ UserText, AssistantSpokenText string }

// RetrievalService is the only memory port of the realtime runtime. BuildSessionContext and
// RefreshEpisodic never fail the session: on error they return an empty or previous result with
// Degraded set.
type RetrievalService interface {
	BuildSessionContext(ctx context.Context, in SessionContextInput) (MemoryContext, error)
	RefreshEpisodic(ctx context.Context, userID, sessionID uuid.UUID, recent []TurnText) ([]ContextItem, error)
	ReloadCoreIfChanged(ctx context.Context, mc *MemoryContext) (changed bool, err error)
}
