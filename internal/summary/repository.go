package summary

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

var (
	// ErrSessionNotFound is returned by Source.LoadSession for an unknown session id.
	ErrSessionNotFound = errors.New("summary: session not found")
	// ErrPromptNotFound is returned by PromptSource when no SUMMARY version is PUBLISHED.
	ErrPromptNotFound = errors.New("summary: no published SUMMARY prompt")
)

// SessionInfo is the session data the summary reads (spec 9.9.6 steps 1 and 3): the owner's
// status plus the topic and scenario names.
type SessionInfo struct {
	ID, UserID                     uuid.UUID
	UserStatus                     string
	TopicName, ScenarioName        string
	StartingAt, StartedAt, EndedAt *time.Time
}

// Source reads the session and its turns without a transaction.
type Source interface {
	// LoadSession returns ErrSessionNotFound for an unknown id.
	LoadSession(ctx context.Context, id uuid.UUID) (SessionInfo, error)
	// LoadTurns returns the non-DISCARDED turns ordered by sequence_number; from/to bound
	// created_at for partition pruning.
	LoadTurns(ctx context.Context, id uuid.UUID, from, to time.Time) ([]TurnRow, error)
}

// Prompt is the PUBLISHED SUMMARY version: its id (logged; there is no column for it) and the
// template.
type Prompt struct {
	ID       uuid.UUID
	Template prompt.Template
}

// PromptSource reads the published SUMMARY prompt (summary does not import content).
type PromptSource interface {
	// PublishedPrompt returns ErrPromptNotFound when no version is PUBLISHED.
	PublishedPrompt(ctx context.Context) (Prompt, error)
}

// SessionWriter writes sessions.summary_text.
type SessionWriter interface {
	// SetSummary sets summary_text only while it is NULL and does not touch sessions.version; false
	// when nothing changed (a summary already exists, or the session is gone).
	SetSummary(ctx context.Context, id uuid.UUID, text string) (bool, error)
}

// Tx is the transaction handle: the summary write plus the idempotency port that commits or rolls
// back with it.
type Tx interface {
	Processed() events.ProcessedEvents
	Sessions() SessionWriter
}

// Store runs transactions.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// TextRouter completes text with the active LLM vendor; provider.AsyncRouter satisfies it.
type TextRouter interface {
	CompleteText(ctx context.Context, req provider.TextRequest) (provider.TextResult, error)
}
