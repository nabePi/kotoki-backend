package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// ErrSessionNotFound is returned by Source.LoadSession when the session does not exist.
var ErrSessionNotFound = errors.New("evaluation: session not found")

// Evaluation is one evaluations row (spec 10.2).
type Evaluation struct {
	ID, SessionID, UserID uuid.UUID
	Status                Status
	InsufficientEvidence  bool

	Scores                      Scores
	OverallScore                *float64
	ListeningResponseSignal     *int
	InteractionComplexitySignal *int
	DimensionConfidence         map[string]float64
	DimensionComments           map[string]string

	SummaryFeedback *string
	Strengths       []string
	FocusAreas      []string
	Recommendations []Recommendation

	UserTurnCount, UserSpeechSeconds, ActiveSeconds, ScenarioDifficultyLevel *int

	RubricVersion   string
	PromptVersionID *uuid.UUID
	Provider, Model *string
	LLMCallCount    int
	InputTokens     *int
	OutputTokens    *int

	AttemptCount  int
	FailureCode   *FailureCode
	LastError     *string
	SourceEventID uuid.UUID

	DetailsPurgedAt        *time.Time
	CreatedAt              time.Time
	StartedAt, CompletedAt *time.Time
	UpdatedAt              time.Time
}

// FindingRow is a stored finding: the validated Finding plus its identity and display order.
type FindingRow struct {
	ID      uuid.UUID
	Ordinal int
	Finding
	CreatedAt time.Time
}

// ClaimInput is the claim of spec 10.5 step 4.
type ClaimInput struct {
	ID, SessionID, UserID, PromptVersionID, SourceEventID uuid.UUID
	RubricVersion                                         string
}

// ClaimResult is the outcome of Claim; Claimed is false when the evaluation is already COMPLETED
// (ID is then the zero UUID).
type ClaimResult struct {
	ID           uuid.UUID
	Claimed      bool
	AttemptCount int
}

// CompletedUpdate holds every result column of the final transaction (spec 10.5 step 10). On the
// insufficient-evidence path all scores, signals, provider, model and token counts are nil/zero,
// DimensionConfidence is empty and LLMCallCount is 0.
type CompletedUpdate struct {
	InsufficientEvidence bool

	Scores                      Scores
	OverallScore                *float64
	ListeningResponseSignal     *int
	InteractionComplexitySignal *int
	DimensionConfidence         map[string]float64
	DimensionComments           map[string]string

	SummaryFeedback string
	Strengths       []string
	FocusAreas      []string
	Recommendations []Recommendation

	UserTurnCount, UserSpeechSeconds, ActiveSeconds, ScenarioDifficultyLevel int

	Provider, Model           *string
	LLMCallCount              int
	InputTokens, OutputTokens *int

	CompletedAt time.Time
}

// Repository is the evaluation write port; it runs inside Tx.
type Repository interface {
	// Claim upserts the row as RUNNING (attempt_count + 1) unless it is COMPLETED.
	Claim(ctx context.Context, in ClaimInput) (ClaimResult, error)
	// Complete stores the result; false when the row is already COMPLETED (or missing).
	Complete(ctx context.Context, id uuid.UUID, u CompletedUpdate) (bool, error)
	// ReplaceFindings deletes the findings of the evaluation and inserts fs.
	ReplaceFindings(ctx context.Context, evaluationID, userID, sessionID uuid.UUID, fs []FindingRow) error
	// MarkPending moves RUNNING to PENDING with last_error (cut to 2,000 characters).
	MarkPending(ctx context.Context, sessionID uuid.UUID, lastError string) error
	// MarkFailed moves RUNNING or PENDING to FAILED and returns the row; changed is false when the
	// row was in another state (COMPLETED, FAILED already, or missing).
	MarkFailed(ctx context.Context, sessionID uuid.UUID, code FailureCode, lastError string) (Evaluation, bool, error)
}

// Tx is the transaction handle: the repository plus the idempotency and outbox ports that commit
// or roll back with it.
type Tx interface {
	Evaluations() Repository
	Processed() events.ProcessedEvents
	Outbox() events.OutboxWriter
}

// Store runs transactions.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// SessionInfo is the session data the evaluation reads (spec 10.5 step 2) joined with the owner's
// status.
type SessionInfo struct {
	ID, UserID, ScenarioID, ScenarioVersionID, TopicID uuid.UUID
	Status                                             string
	StartingAt, StartedAt, EndedAt                     *time.Time
	DisconnectedSeconds                                int
	AdaptiveDifficulty, ConfigSnapshot                 json.RawMessage
	TranscriptPurgedAt                                 *time.Time
	UserStatus                                         string
}

// Source reads the session and its turns without a transaction.
type Source interface {
	// LoadSession returns ErrSessionNotFound for an unknown id.
	LoadSession(ctx context.Context, id uuid.UUID) (SessionInfo, error)
	// LoadTurns returns the non-DISCARDED turns ordered by sequence_number; from/to bound
	// created_at for partition pruning.
	LoadTurns(ctx context.Context, sessionID uuid.UUID, from, to time.Time) ([]TurnRow, error)
}

// Reader reads stored evaluations for the API.
type Reader interface {
	GetBySession(ctx context.Context, sessionID uuid.UUID) (Evaluation, bool, error)
	// ListFindings returns the findings of an evaluation ordered by ordinal.
	ListFindings(ctx context.Context, evaluationID uuid.UUID) ([]FindingRow, error)
}
