// Package conversation is the realtime runtime. ports.go lists what it consumes (spec 7.13.1);
// interfaces live on the consumer side (D-005). The package stays free of cgo.
package conversation

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// Turn types the ports speak, owned by session.
type (
	// TurnStart opens a turn.
	TurnStart = session.TurnStart
	// TurnUser is what STT learned about the user's side of a turn.
	TurnUser = session.TurnUser
	// TurnFinal closes a turn.
	TurnFinal = session.TurnFinal
	// TurnRecord is a turn as read back.
	TurnRecord = session.TurnRecord
)

// SessionRuntime is satisfied by *session.RuntimeService; every method is idempotent and fenced.
type SessionRuntime interface {
	GetForAttach(ctx context.Context, sessionID uuid.UUID) (session.RuntimeRecord, error)
	ClaimRuntime(ctx context.Context, sessionID uuid.UUID, allowed []session.Status) (fence int64, rec session.RuntimeRecord, err error)
	// MarkStarting is RESERVED → STARTING (BeginStart, S-2): the record carries
	// opening_prompt_version_id, which the runtime uses as it is.
	MarkStarting(ctx context.Context, sessionID uuid.UUID, fence int64) (session.RuntimeRecord, error)
	Activate(ctx context.Context, sessionID uuid.UUID, fence int64) (startedAt, expiresAt time.Time, err error)
	FailStart(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error)
	AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) error
	MarkHandoff(ctx context.Context, sessionID uuid.UUID, fence int64) error
	BeginEnding(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) error
	Complete(ctx context.Context, sessionID uuid.UUID, fence int64) (session.Settlement, error)
	Fail(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error)
}

// TurnRepository is the fenced turn store (guard of spec 7.9.3). It is the part of
// session.TurnRepository the runtime uses; there is no SetUserAudioKey because user audio is never
// stored (D-099).
type TurnRepository interface {
	Insert(ctx context.Context, fence int64, t TurnStart) error
	UpdateUser(ctx context.Context, fence int64, turnID uuid.UUID, u TurnUser) error
	Finalize(ctx context.Context, fence int64, turnID uuid.UUID, f TurnFinal) error
	FailInProgress(ctx context.Context, fence int64, sessionID uuid.UUID, partial *TurnFinal) (int, error)
	ListRecent(ctx context.Context, sessionID uuid.UUID, limit int) ([]TurnRecord, error)
	MaxSeqAndGeneration(ctx context.Context, sessionID uuid.UUID) (maxSeq int, maxGen int, err error)
}

// SessionEventWriter appends a runtime event to session_events (actor REALTIME) while the fence
// holds; turnID is set for BARGE_IN and TURN_FAILED. The payload never carries transcript text.
type SessionEventWriter interface {
	Append(ctx context.Context, fence int64, sid uuid.UUID, eventType string, turnID *uuid.UUID, payload any) error
}

// RuntimeStore is the realtime Redis state (spec 7.9).
type RuntimeStore = rtstate.Store

// ConfigSnapshot is the immutable configuration of a session: character snapshot, scenario
// version and CONVERSATION prompt, plus the OPENING prompt when the session has one.
type ConfigSnapshot struct {
	content.SessionConfig
	Opening *content.PromptVersion
}

// ConfigLoader loads the snapshot of a session; there is no lookup of a PUBLISHED version here (S-2).
type ConfigLoader interface {
	LoadSnapshot(ctx context.Context, s session.RuntimeRecord) (ConfigSnapshot, error)
}

// LearnerContextProvider builds the learner block of the prompt. Rerender re-derives the
// adaptation text of prev after an in-session adjustment without touching storage; M8 backs it
// with learner.NewPromptContext.
type LearnerContextProvider interface {
	PromptContext(ctx context.Context, userID uuid.UUID, ad learner.AdaptiveDifficulty) (prompt.LearnerContext, error)
	Rerender(prev prompt.LearnerContext, ad learner.AdaptiveDifficulty) prompt.LearnerContext
}
