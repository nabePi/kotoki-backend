package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SessionCompletedV1 is the v1 payload of session.completed (spec 9.5.1); defined in types.go as
// SessionCompletedPayload.
type SessionCompletedV1 = SessionCompletedPayload

// EvalSignal is a non-dimension signal of an evaluation (spec 9.5.2 $defs/signal).
type EvalSignal struct {
	Score      int     `json:"score"`
	Confidence float64 `json:"confidence"`
}

// EvalScores holds the five dimension scores; nil marshals as null (dimension without evidence).
type EvalScores struct {
	Grammar         *int `json:"GRAMMAR"`
	Vocabulary      *int `json:"VOCABULARY"`
	Fluency         *int `json:"FLUENCY"`
	Naturalness     *int `json:"NATURALNESS"`
	TaskAchievement *int `json:"TASK_ACHIEVEMENT"`
}

// EvalSignals holds the two signals; nil marshals as null.
type EvalSignals struct {
	ListeningResponse     *EvalSignal `json:"LISTENING_RESPONSE"`
	InteractionComplexity *EvalSignal `json:"INTERACTION_COMPLEXITY"`
}

// SeverityCounts counts the non-STRENGTH findings of one dimension per severity.
type SeverityCounts struct {
	Major int `json:"MAJOR"`
	Minor int `json:"MINOR"`
	Info  int `json:"INFO"`
}

// EvaluationCompletedV1 is the v1 payload of evaluation.completed (spec 9.5.2).
type EvaluationCompletedV1 struct {
	EvaluationID            uuid.UUID                 `json:"evaluation_id"`
	SessionID               uuid.UUID                 `json:"session_id"`
	RubricVersion           string                    `json:"rubric_version"`
	InsufficientEvidence    bool                      `json:"insufficient_evidence"`
	Scores                  EvalScores                `json:"scores"`
	DimensionConfidence     map[string]float64        `json:"dimension_confidence"`
	Signals                 EvalSignals               `json:"signals"`
	OverallScore            *float64                  `json:"overall_score"`
	SessionEndedAt          time.Time                 `json:"session_ended_at"`
	ScenarioDifficultyLevel int                       `json:"scenario_difficulty_level"`
	UserTurnCount           int                       `json:"user_turn_count"`
	UserSpeechSeconds       int                       `json:"user_speech_seconds"`
	FindingCounts           map[string]SeverityCounts `json:"finding_counts"`
	CompletedAt             time.Time                 `json:"completed_at"`
}

// MarshalJSON writes the timestamps with millisecond precision and never writes null for the
// required objects dimension_confidence and finding_counts.
func (p EvaluationCompletedV1) MarshalJSON() ([]byte, error) {
	type plain EvaluationCompletedV1
	if p.DimensionConfidence == nil {
		p.DimensionConfidence = map[string]float64{}
	}
	if p.FindingCounts == nil {
		p.FindingCounts = map[string]SeverityCounts{}
	}
	return json.Marshal(struct {
		plain
		SessionEndedAt string `json:"session_ended_at"`
		CompletedAt    string `json:"completed_at"`
	}{plain: plain(p), SessionEndedAt: millis(p.SessionEndedAt), CompletedAt: millis(p.CompletedAt)})
}

// EvaluationFailedV1 is the v1 payload of evaluation.failed (spec 9.5.3). FailureCode is one of
// TRANSCRIPT_UNAVAILABLE, PROVIDER_UNAVAILABLE, INVALID_MODEL_OUTPUT, CONFIGURATION_ERROR,
// INTERNAL_ERROR.
type EvaluationFailedV1 struct {
	EvaluationID uuid.UUID `json:"evaluation_id"`
	SessionID    uuid.UUID `json:"session_id"`
	FailureCode  string    `json:"failure_code"`
	AttemptCount int       `json:"attempt_count"`
	FailedAt     time.Time `json:"failed_at"`
}

// MarshalJSON writes failed_at with millisecond precision.
func (p EvaluationFailedV1) MarshalJSON() ([]byte, error) {
	type plain EvaluationFailedV1
	return json.Marshal(struct {
		plain
		FailedAt string `json:"failed_at"`
	}{plain: plain(p), FailedAt: millis(p.FailedAt)})
}

// MemoryExtractedV1 is the v1 payload of memory.extracted (spec 9.5.4). The id arrays are written
// as [] (never null) when empty.
type MemoryExtractedV1 struct {
	SessionID           uuid.UUID      `json:"session_id"`
	CreatedMemoryIDs    []uuid.UUID    `json:"created_memory_ids"`
	UpdatedMemoryIDs    []uuid.UUID    `json:"updated_memory_ids"`
	SupersededMemoryIDs []uuid.UUID    `json:"superseded_memory_ids"`
	CategoryCounts      map[string]int `json:"category_counts,omitempty"`
	PromptVersionID     uuid.UUID      `json:"prompt_version_id"`
	Provider            string         `json:"provider"`
	Model               string         `json:"model"`
	ExtractedAt         time.Time      `json:"extracted_at"`
}

// MarshalJSON writes extracted_at with millisecond precision and empty arrays instead of null.
func (p MemoryExtractedV1) MarshalJSON() ([]byte, error) {
	type plain MemoryExtractedV1
	nonNil := func(s []uuid.UUID) []uuid.UUID {
		if s == nil {
			return []uuid.UUID{}
		}
		return s
	}
	p.CreatedMemoryIDs, p.UpdatedMemoryIDs, p.SupersededMemoryIDs =
		nonNil(p.CreatedMemoryIDs), nonNil(p.UpdatedMemoryIDs), nonNil(p.SupersededMemoryIDs)
	return json.Marshal(struct {
		plain
		ExtractedAt string `json:"extracted_at"`
	}{plain: plain(p), ExtractedAt: millis(p.ExtractedAt)})
}

func newEvent(id uuid.UUID, typ, aggregate string, aggregateID uuid.UUID, now time.Time, producer string, userID uuid.UUID, payload any) Event {
	return Event{
		EventID: id, EventType: typ, EventVersion: CurrentVersion, AggregateType: aggregate, AggregateID: aggregateID,
		UserID: userID, Producer: producer, OccurredAt: now.UTC(), Payload: payload,
	}
}

// NewEvaluationCompleted builds evaluation.completed v1 (aggregate EVALUATION).
func NewEvaluationCompleted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p EvaluationCompletedV1) Event {
	return newEvent(id, EvaluationCompleted, AggregateEvaluation, p.EvaluationID, now, producer, userID, p)
}

// NewEvaluationFailed builds evaluation.failed v1 (aggregate EVALUATION).
func NewEvaluationFailed(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p EvaluationFailedV1) Event {
	return newEvent(id, EvaluationFailed, AggregateEvaluation, p.EvaluationID, now, producer, userID, p)
}

// NewMemoryExtracted builds memory.extracted v1 (aggregate SESSION).
func NewMemoryExtracted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p MemoryExtractedV1) Event {
	return newEvent(id, MemoryExtracted, AggregateSession, p.SessionID, now, producer, userID, p)
}
