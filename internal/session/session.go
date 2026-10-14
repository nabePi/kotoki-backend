// Package session owns the lifecycle of a practice session: the status machine, the single writer
// of sessions.status, the runtime facade and the read side for REST (spec Bab 5). It never imports
// pgx, chi or go-redis; repositories and the transaction port are defined here and implemented by
// internal/postgres.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status is sessions.status.
type Status string

// Statuses.
const (
	StatusReserved  Status = "RESERVED"
	StatusStarting  Status = "STARTING"
	StatusActive    Status = "ACTIVE"
	StatusEnding    Status = "ENDING"
	StatusCompleted Status = "COMPLETED"
	StatusCancelled Status = "CANCELLED"
	StatusAbandoned Status = "ABANDONED"
	StatusFailed    Status = "FAILED"
)

var allStatuses = []Status{StatusReserved, StatusStarting, StatusActive, StatusEnding, StatusCompleted, StatusCancelled, StatusAbandoned, StatusFailed}

// StatusFromString parses a status; an unknown value is an error.
func StatusFromString(s string) (Status, error) {
	for _, st := range allStatuses {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("session: unknown status %q", s)
}

// Open reports whether the status counts as an open session (one per user).
func (s Status) Open() bool {
	return s == StatusReserved || s == StatusStarting || s == StatusActive || s == StatusEnding
}

// Terminal reports whether no transition leaves the status.
func (s Status) Terminal() bool { return !s.Open() }

// EndReason is sessions.end_reason.
type EndReason string

// End reasons.
const (
	EndUserEnded        EndReason = "USER_ENDED"
	EndDurationElapsed  EndReason = "DURATION_ELAPSED"
	EndReconnectTimeout EndReason = "RECONNECT_TIMEOUT"
	EndProviderFailure  EndReason = "PROVIDER_FAILURE"
	EndServerError      EndReason = "SERVER_ERROR"
	EndUserCancelled    EndReason = "USER_CANCELLED"
	EndNotStarted       EndReason = "NOT_STARTED"
)

// Actor is who caused a transition (session_events.actor).
type Actor string

// Actors.
const (
	ActorUser     Actor = "USER"
	ActorRealtime Actor = "REALTIME"
	ActorSweeper  Actor = "SWEEPER"
	ActorCleanup  Actor = "CLEANUP"
	ActorAdmin    Actor = "ADMIN"
	ActorSystem   Actor = "SYSTEM"
)

// StartMode is sessions.conversation_start_mode.
type StartMode string

// Start modes.
const (
	StartUserFirst StartMode = "USER_FIRST"
	StartAIFirst   StartMode = "AI_FIRST"
)

// Fence identifies the caller of a transition: nil = a non-runtime caller (USER, SWEEPER, CLEANUP, ADMIN), not checked; non-nil = the
// realtime runtime, whose UPDATE requires sessions.runtime_fence = *fence (0 rows → ErrFenceLost,
// unless the session is already in the target status, which succeeds idempotently).
type Fence = *int64

// Timings of the start phase (spec 4.6, 5.5).
const (
	// StartDeadline is how long a RESERVED session may wait for session.start (start_deadline_at).
	StartDeadline = 5 * time.Minute
	// StartTimeout is how long STARTING may take before the sweeper fails it.
	StartTimeout = 10 * time.Second
)

// Errors of the session domain.
var (
	ErrNotFound            = errors.New("session not found")
	ErrInvalidTransition   = errors.New("invalid session transition") // wrapped with From/To
	ErrStartDeadlinePassed = errors.New("session start deadline passed")
	ErrStartTimeout        = errors.New("session start timeout")
	ErrAlreadyOpen         = errors.New("user already has an open session")
	// ErrDuplicateIdempotency is returned by Insert when the (user, Idempotency-Key) pair already
	// exists: the backstop of the replay check in Service.Create (spec 4.6.1).
	ErrDuplicateIdempotency = errors.New("duplicate create idempotency key")
	ErrFenceLost            = errors.New("runtime fence lost") // runtime_fence ≠ the caller's fence (spec 7.9.3)
)

// Session mirrors a sessions row (spec 5.3.1). The character, voice and formality of the session
// live in ConfigSnapshot, never in version columns (D-014).
type Session struct {
	ID                     uuid.UUID
	UserID                 uuid.UUID
	Status                 Status
	EndReason              *EndReason
	ConversationStartMode  StartMode
	DurationSeconds        int
	TopicID                uuid.UUID
	ScenarioID             uuid.UUID
	ScenarioVersionID      uuid.UUID
	CharacterID            uuid.UUID
	PromptVersionID        uuid.UUID
	OpeningPromptVersionID *uuid.UUID
	ConfigSnapshot         json.RawMessage
	AdaptiveDifficulty     json.RawMessage
	RuntimeFence           int64
	ReservedSeconds        int
	ChargedSeconds         int
	DisconnectedSeconds    int
	FailureDetail          *string
	SummaryText            *string
	TranscriptPurgedAt     *time.Time
	CreateIdempotencyKey   string
	CreateRequestHash      []byte
	CreatedAt              time.Time
	StartingAt             *time.Time
	StartedAt              *time.Time
	ExpiresAt              *time.Time
	EndingAt               *time.Time
	EndedAt                *time.Time
	HandoffAt              *time.Time
	UpdatedAt              time.Time
	Version                int64
}

// RuntimeRecord is the part of a session the realtime runtime reads (spec 7.13.1): enough to
// attach, resume and load the configuration without touching the live content rows.
type RuntimeRecord struct {
	ID                     uuid.UUID
	UserID                 uuid.UUID
	Status                 Status
	ConversationStartMode  StartMode
	DurationSeconds        int
	StartedAt              *time.Time
	ExpiresAt              *time.Time
	TopicID                uuid.UUID
	ScenarioID             uuid.UUID
	ScenarioVersionID      uuid.UUID
	CharacterID            uuid.UUID
	PromptVersionID        uuid.UUID
	OpeningPromptVersionID *uuid.UUID
	ConfigSnapshot         json.RawMessage
	AdaptiveDifficulty     json.RawMessage
	RuntimeFence           int64
}

// RuntimeRecordOf is the runtime's view of a session row.
func RuntimeRecordOf(s Session) RuntimeRecord {
	return RuntimeRecord{
		ID: s.ID, UserID: s.UserID, Status: s.Status, ConversationStartMode: s.ConversationStartMode, DurationSeconds: s.DurationSeconds,
		StartedAt: s.StartedAt, ExpiresAt: s.ExpiresAt, TopicID: s.TopicID, ScenarioID: s.ScenarioID, ScenarioVersionID: s.ScenarioVersionID,
		CharacterID: s.CharacterID, PromptVersionID: s.PromptVersionID, OpeningPromptVersionID: s.OpeningPromptVersionID,
		ConfigSnapshot: s.ConfigSnapshot, AdaptiveDifficulty: s.AdaptiveDifficulty, RuntimeFence: s.RuntimeFence,
	}
}

// Settlement is what the runtime learns when a session ends (spec 7.13.1).
type Settlement struct {
	ChargedSeconds, DisconnectedSeconds int
	StartedAt, EndedAt                  time.Time
	EndReason                           EndReason
}

// EventType is session_events.event_type; the lifecycle ones are written by Service, the runtime
// ones through EventRepository.AppendFenced.
type EventType string

// Lifecycle events (Service) and runtime events (spec 5.7.1, 7.13.1).
const (
	EventReserved   EventType = "SESSION_RESERVED"
	EventStarting   EventType = "SESSION_STARTING"
	EventStarted    EventType = "SESSION_STARTED"
	EventEnding     EventType = "SESSION_ENDING"
	EventCompleted  EventType = "SESSION_COMPLETED"
	EventCancelled  EventType = "SESSION_CANCELLED"
	EventAbandoned  EventType = "SESSION_ABANDONED"
	EventFailed     EventType = "SESSION_FAILED"
	EventOwner      EventType = "OWNER_CHANGED"
	EventDisconnect EventType = "SESSION_DISCONNECTED"
	EventReconnect  EventType = "SESSION_RECONNECTED"
	EventBargeIn    EventType = "BARGE_IN"
	EventTurnFailed EventType = "TURN_FAILED"
	EventFallback   EventType = "PROVIDER_FALLBACK"
	EventProtocol   EventType = "PROTOCOL_VIOLATION"
	EventDifficulty EventType = "DIFFICULTY_ADJUSTED"
)

// Event is a session_events row. The payload never carries transcript text.
type Event struct {
	ID         uuid.UUID
	SessionID  uuid.UUID
	Type       EventType
	Actor      Actor
	FromStatus *Status
	ToStatus   *Status
	TurnID     *uuid.UUID
	Payload    json.RawMessage
	CreatedAt  time.Time
}

// TurnStatus is session_turns.status; a nil status is a turn in progress.
type TurnStatus string

// Turn statuses (D-075).
const (
	TurnCompleted   TurnStatus = "COMPLETED"
	TurnInterrupted TurnStatus = "INTERRUPTED"
	TurnFailed      TurnStatus = "FAILED"
	TurnDiscarded   TurnStatus = "DISCARDED"
)

// Turn mirrors a session_turns row (spec 5.3.2). There are no expression fields (D-014) and no
// audio reference: user audio is never stored (D-099).
type Turn struct {
	ID                    uuid.UUID
	SessionID             uuid.UUID
	Sequence              int
	Status                *TurnStatus
	UserText              *string
	UserStartedAt         *time.Time
	UserEndedAt           *time.Time
	AssistantText         *string
	AssistantSpokenText   *string
	Generation            int
	STTProvider           *string
	LLMProvider           *string
	LLMModel              *string
	TTSProvider           *string
	TTSModel              *string
	TTSVoiceID            *string
	UsedFallback          bool
	TurnEndToFirstAudioMS *int
	STTFinalMS            *int
	LLMFirstTokenMS       *int
	TTSFirstAudioMS       *int
	InputTokens           *int
	OutputTokens          *int
	TTSCharacters         *int
	STTAudioMS            *int
	ErrorCode             *string
	CreatedAt             time.Time
	CompletedAt           *time.Time
}

// TurnStart opens a turn (runtime INSERT). CreatedAt is kept by the runtime for partition pruning
// of the later UPDATEs.
type TurnStart struct {
	ID         uuid.UUID
	SessionID  uuid.UUID
	Sequence   int
	Generation int
	CreatedAt  time.Time
}

// TurnUser is what the STT stage learned about the user's side of a turn.
type TurnUser struct {
	CreatedAt   time.Time // of the turn, for partition pruning
	Text        string
	StartedAt   *time.Time
	EndedAt     *time.Time
	STTProvider *string
	STTFinalMS  *int
	STTAudioMS  *int
}

// TurnFinal closes a turn.
type TurnFinal struct {
	CreatedAt             time.Time // of the turn, for partition pruning
	Status                TurnStatus
	AssistantText         *string
	AssistantSpokenText   *string
	LLMProvider           *string
	LLMModel              *string
	TTSProvider           *string
	TTSModel              *string
	TTSVoiceID            *string
	UsedFallback          bool
	TurnEndToFirstAudioMS *int
	LLMFirstTokenMS       *int
	TTSFirstAudioMS       *int
	InputTokens           *int
	OutputTokens          *int
	TTSCharacters         *int
	ErrorCode             *string
	CompletedAt           time.Time
}

// TurnRecord is a turn as the runtime and REST read it back.
type TurnRecord = Turn

// Patch is the set of columns a CAS transition changes (spec 5.4 transition table). A nil field is
// left alone; version is always incremented and updated_at set to At.
type Patch struct {
	To                     Status
	At                     time.Time
	EndReason              *EndReason
	FailureDetail          *string
	StartingAt             *time.Time
	StartedAt              *time.Time
	ExpiresAt              *time.Time
	EndingAt               *time.Time
	EndedAt                *time.Time
	ChargedSeconds         *int
	OpeningPromptVersionID *uuid.UUID
	// BumpFence raises runtime_fence in the same UPDATE: transitions by a non-runtime caller to
	// ENDING or a terminal status, and every terminal transition by the runtime (spec 5.2).
	BumpFence bool
}

// ListFilter narrows Repository.ListByUser.
type ListFilter struct {
	Statuses []Status
}

// Repository is the sessions port (spec 4.2, 5.2). Every write is one statement; the Service
// composes them inside a Tx.
type Repository interface {
	Insert(ctx context.Context, s Session) error
	GetByID(ctx context.Context, id uuid.UUID) (Session, error)
	// GetForUpdate locks the row (SELECT … FOR UPDATE) in the lock order of spec 4.6.0.
	GetForUpdate(ctx context.Context, id uuid.UUID) (Session, error)
	// OwnerOf returns the owner without locking; ErrNotFound when the session does not exist.
	OwnerOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error)
	FindByIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) (Session, error)
	FindOpenByUser(ctx context.Context, userID uuid.UUID) (Session, error)
	// CAS applies p when status = from (and runtime_fence = *fence when fence is non-nil); the bool
	// is false when no row matched, and the caller re-reads to tell idempotency from conflict.
	CAS(ctx context.Context, id uuid.UUID, from Status, fence Fence, p Patch) (Session, bool, error)
	// Claim raises runtime_fence, clears handoff_at and returns the row when status is in allowed
	// (spec 7.9.3); no match is ErrInvalidTransition.
	Claim(ctx context.Context, id uuid.UUID, allowed []Status) (Session, error)
	MarkHandoff(ctx context.Context, id uuid.UUID, fence int64, at time.Time) (bool, error)
	// AddDisconnected is the SQL of spec 5.6.5; the bool is false when status or fence did not match.
	AddDisconnected(ctx context.Context, id uuid.UUID, fence int64, n int, at time.Time) (Session, bool, error)
	ListByUser(ctx context.Context, userID uuid.UUID, f ListFilter, after *uuid.UUID, limit int) ([]Session, error)
	GetRuntimeRecord(ctx context.Context, id uuid.UUID) (RuntimeRecord, error)
	SetSummary(ctx context.Context, id uuid.UUID, text string, at time.Time) error

	// Candidates of the sweeper (spec 5.6.4); none of them locks.
	// The time arguments are cutoffs the caller computed from its clock (now − 15 s, now − 60 s, …).
	ListExpired(ctx context.Context, expiredBefore time.Time, limit int) ([]uuid.UUID, error)
	ListStuckEnding(ctx context.Context, endingBefore time.Time, limit int) ([]uuid.UUID, error)
	ListStuckStarting(ctx context.Context, startingBefore time.Time, limit int) ([]uuid.UUID, error)
	ListHandoffStale(ctx context.Context, handoffBefore time.Time, limit int) ([]uuid.UUID, error)
	ListActiveForHeartbeat(ctx context.Context, startedBefore time.Time, limit int) ([]ActiveRef, error)
	CountActiveNonHandoff(ctx context.Context) (int, error)
	// ListAbandonCandidates is the candidate query of spec 4.6.2 without FOR UPDATE, ordered by
	// start_deadline_at; reservation_cleanup (M6) feeds the ids to Service.Abandon.
	ListAbandonCandidates(ctx context.Context, deadlineBefore time.Time, limit int) ([]uuid.UUID, error)
}

// ActiveRef is an ACTIVE session the sweeper checks for a heartbeat.
type ActiveRef struct {
	ID        uuid.UUID
	StartedAt time.Time
	HandoffAt *time.Time
}

// TurnRepository is the turns port (spec 7.13.1 signatures). Writes with a fence only succeed
// while sessions.runtime_fence = fence and the status is STARTING, ACTIVE or ENDING; otherwise
// ErrFenceLost. There is no SetUserAudioKey: nothing about user audio is persisted (D-099).
type TurnRepository interface {
	Insert(ctx context.Context, fence int64, t TurnStart) error
	UpdateUser(ctx context.Context, fence int64, turnID uuid.UUID, u TurnUser) error
	Finalize(ctx context.Context, fence int64, turnID uuid.UUID, f TurnFinal) error
	FailInProgress(ctx context.Context, fence int64, sessionID uuid.UUID, partial *TurnFinal) (int, error)
	ListRecent(ctx context.Context, sessionID uuid.UUID, limit int) ([]TurnRecord, error)
	// MaxSeqAndGeneration is (-1, 0) when the session has no turn yet (AI_FIRST starts at sequence 0,
	// USER_FIRST at 1).
	MaxSeqAndGeneration(ctx context.Context, sessionID uuid.UUID) (maxSeq int, maxGen int, err error)

	// FailDangling marks every turn still in progress FAILED when the session is settled (spec 4.6.3
	// step 7) and returns how many it changed.
	FailDangling(ctx context.Context, sessionID uuid.UUID, startingAt time.Time, at time.Time) (int, error)
	// Stats counts the non-DISCARDED turns of a session for the session.completed payload; hasAudio is
	// true when any turn recorded STT audio or a first TTS audio latency.
	Stats(ctx context.Context, sessionID uuid.UUID, startingAt time.Time) (turnCount, userTurnCount int, hasAudio bool, err error)
	// ListForUser pages the transcript; startingAt/endedAt bound created_at for partition pruning.
	ListForUser(ctx context.Context, sessionID uuid.UUID, startingAt time.Time, endedAt *time.Time, afterSeq *int, limit int) ([]TurnRecord, error)
}

// EventRepository is the session_events port.
type EventRepository interface {
	// Append writes a lifecycle event inside the transition's transaction.
	Append(ctx context.Context, e Event) error
	// AppendFenced writes a runtime event only while runtime_fence = fence and the status is
	// STARTING, ACTIVE or ENDING; otherwise ErrFenceLost.
	AppendFenced(ctx context.Context, fence int64, e Event) error
}
