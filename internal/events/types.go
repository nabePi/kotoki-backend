package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event types (routing keys) and aggregate types.
const (
	SessionCompleted    = "session.completed"
	EvaluationCompleted = "evaluation.completed"
	EvaluationFailed    = "evaluation.failed"
	MemoryExtracted     = "memory.extracted"

	AggregateSession    = "SESSION"
	AggregateEvaluation = "EVALUATION"
)

// CurrentVersion is the event_version every event type is published with today (spec 9.4).
const CurrentVersion = 1

// SupportedVersions is the highest event_version this build understands per event type; a consumer
// treats a higher version as UNSUPPORTED_EVENT_VERSION (spec 9.4, 9.7.3).
var SupportedVersions = map[string]int{
	SessionCompleted: 1, EvaluationCompleted: 1, EvaluationFailed: 1, MemoryExtracted: 1,
}

// Producers.
const (
	ProducerAPI      = "API"
	ProducerRealtime = "REALTIME"
	ProducerWorker   = "WORKER"
)

// SessionCompletedPayload is the v1 payload of session.completed (spec 5.7.2, 9.5.1). It is
// published for every session that was ACTIVE and ended COMPLETED or FAILED.
type SessionCompletedPayload struct {
	SessionID             uuid.UUID `json:"session_id"`
	FinalStatus           string    `json:"final_status"` // COMPLETED | FAILED
	EndReason             string    `json:"end_reason"`
	ConversationStartMode string    `json:"conversation_start_mode"`
	TopicID               uuid.UUID `json:"topic_id"`
	ScenarioID            uuid.UUID `json:"scenario_id"`
	ScenarioVersionID     uuid.UUID `json:"scenario_version_id"`
	CharacterID           uuid.UUID `json:"character_id"`
	PromptVersionID       uuid.UUID `json:"prompt_version_id"`
	DurationSeconds       int       `json:"duration_seconds"`
	StartedAt             time.Time `json:"started_at"`
	EndedAt               time.Time `json:"ended_at"`
	ActiveSeconds         int       `json:"active_seconds"`
	ChargedSeconds        int       `json:"charged_seconds"`
	TurnCount             int       `json:"turn_count"`
	UserTurnCount         int       `json:"user_turn_count"`
}

// MarshalJSON writes the timestamps as RFC 3339 UTC with millisecond precision (spec 5.8.1).
func (p SessionCompletedPayload) MarshalJSON() ([]byte, error) {
	type plain SessionCompletedPayload
	return json.Marshal(struct {
		plain
		StartedAt string `json:"started_at"`
		EndedAt   string `json:"ended_at"`
	}{plain: plain(p), StartedAt: millis(p.StartedAt), EndedAt: millis(p.EndedAt)})
}

func millis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ActiveSeconds is floor(activeMS / 1000), the active_seconds of the payload (spec 4.5).
func ActiveSeconds(activeMS int64) int {
	if activeMS < 0 {
		return 0
	}
	return int(activeMS / 1000)
}

// NewSessionCompleted builds the session.completed event v1; id is the event id (UUIDv7), now the
// time of the terminal transition, producer REALTIME when the runtime closed the session and
// WORKER when the sweeper did.
func NewSessionCompleted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p SessionCompletedPayload) Event {
	return Event{
		EventID: id, EventType: SessionCompleted, EventVersion: 1, AggregateType: AggregateSession, AggregateID: p.SessionID,
		UserID: userID, Producer: producer, OccurredAt: now.UTC(), Payload: p,
	}
}
