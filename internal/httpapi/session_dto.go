package httpapi

import (
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// createSessionRequest is POST /sessions (spec 5.9.4). Anything else a client might send (a version
// id, voice, provider, style or expression profile) is an unknown field and fails to decode. The ids
// are strings so that a malformed one is a field error rather than a JSON error.
type createSessionRequest struct {
	TopicID               string `json:"topic_id"`
	ScenarioID            string `json:"scenario_id"`
	CharacterID           string `json:"character_id"`
	DurationSeconds       int    `json:"duration_seconds"`
	ConversationStartMode string `json:"conversation_start_mode"`
}

func parseID(field, raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, FieldError(field, "required")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, FieldError(field, "invalid_format")
	}
	return id, nil
}

// input validates the request and turns it into the service input; the allowed durations are
// checked by the service against the balance rules.
func (r createSessionRequest) input(userID uuid.UUID, key string) (session.CreateSessionInput, error) {
	in := session.CreateSessionInput{UserID: userID, IdempotencyKey: key, DurationSeconds: r.DurationSeconds, ConversationStartMode: session.StartMode(r.ConversationStartMode)}
	var err error
	if in.TopicID, err = parseID("topic_id", r.TopicID); err != nil {
		return in, err
	}
	if in.ScenarioID, err = parseID("scenario_id", r.ScenarioID); err != nil {
		return in, err
	}
	if in.CharacterID, err = parseID("character_id", r.CharacterID); err != nil {
		return in, err
	}
	switch {
	case r.DurationSeconds == 0:
		return in, FieldError("duration_seconds", "required")
	case r.ConversationStartMode == "":
		return in, FieldError("conversation_start_mode", "required")
	case in.ConversationStartMode != session.StartUserFirst && in.ConversationStartMode != session.StartAIFirst:
		return in, FieldError("conversation_start_mode", "not_allowed")
	}
	return in, nil
}

// Validate implements Validator.
func (r createSessionRequest) Validate() error {
	_, err := r.input(uuid.Nil, "")
	return err
}

type refResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type characterRefResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	AvatarKey *string   `json:"avatar_key"`
}

// sessionResponse is the Session object of the user view: no adaptive_difficulty, runtime_fence,
// config_snapshot or opening_prompt_version_id, and no vendor voice id.
type sessionResponse struct {
	ID                    uuid.UUID            `json:"id"`
	Status                string               `json:"status"`
	EndReason             *string              `json:"end_reason"`
	ConversationStartMode string               `json:"conversation_start_mode"`
	DurationSeconds       int                  `json:"duration_seconds"`
	ReservedSeconds       int                  `json:"reserved_seconds"`
	ChargedSeconds        int                  `json:"charged_seconds"`
	DisconnectedSeconds   int                  `json:"disconnected_seconds"`
	Topic                 refResponse          `json:"topic"`
	Scenario              refResponse          `json:"scenario"`
	Character             characterRefResponse `json:"character"`
	StartDeadlineAt       *string              `json:"start_deadline_at"`
	CreatedAt             string               `json:"created_at"`
	StartingAt            *string              `json:"starting_at"`
	StartedAt             *string              `json:"started_at"`
	ExpiresAt             *string              `json:"expires_at"`
	EndingAt              *string              `json:"ending_at"`
	EndedAt               *string              `json:"ended_at"`
	RemainingSeconds      *int                 `json:"remaining_seconds"`
	Summary               *string              `json:"summary"`
	ServerTime            string               `json:"server_time"`
}

func newSessionResponse(v session.View, now time.Time) sessionResponse {
	r := sessionResponse{
		ID: v.ID, Status: string(v.Status), ConversationStartMode: string(v.ConversationStartMode), DurationSeconds: v.DurationSeconds,
		ReservedSeconds: v.ReservedSeconds, ChargedSeconds: v.ChargedSeconds, DisconnectedSeconds: v.DisconnectedSeconds,
		Topic: refResponse{ID: v.TopicID, Name: v.TopicName}, Scenario: refResponse{ID: v.ScenarioID, Name: v.ScenarioName},
		Character: characterRefResponse{ID: v.CharacterID, Name: v.CharacterName, AvatarKey: v.AvatarKey},
		CreatedAt: formatTime(v.CreatedAt), StartingAt: formatTimePtr(v.StartingAt), StartedAt: formatTimePtr(v.StartedAt),
		ExpiresAt: formatTimePtr(v.ExpiresAt), EndingAt: formatTimePtr(v.EndingAt), EndedAt: formatTimePtr(v.EndedAt),
		Summary: v.SummaryText, ServerTime: formatTime(now),
	}
	if v.EndReason != nil {
		s := string(*v.EndReason)
		r.EndReason = &s
	}
	if !v.StartDeadlineAt.IsZero() {
		s := formatTime(v.StartDeadlineAt)
		r.StartDeadlineAt = &s
	}
	if v.Status == session.StatusActive && v.ExpiresAt != nil {
		n := max(0, int(math.Ceil(v.ExpiresAt.Sub(now).Seconds())))
		r.RemainingSeconds = &n
	}
	return r
}

// sessionListItem is one entry of GET /sessions.
type sessionListItem struct {
	ID              uuid.UUID            `json:"id"`
	Status          string               `json:"status"`
	EndReason       *string              `json:"end_reason"`
	DurationSeconds int                  `json:"duration_seconds"`
	ChargedSeconds  int                  `json:"charged_seconds"`
	Topic           refResponse          `json:"topic"`
	Scenario        refResponse          `json:"scenario"`
	Character       characterRefResponse `json:"character"`
	CreatedAt       string               `json:"created_at"`
	StartedAt       *string              `json:"started_at"`
	EndedAt         *string              `json:"ended_at"`
}

func newSessionListItem(v session.View) sessionListItem {
	it := sessionListItem{
		ID: v.ID, Status: string(v.Status), DurationSeconds: v.DurationSeconds, ChargedSeconds: v.ChargedSeconds,
		Topic: refResponse{ID: v.TopicID, Name: v.TopicName}, Scenario: refResponse{ID: v.ScenarioID, Name: v.ScenarioName},
		Character: characterRefResponse{ID: v.CharacterID, Name: v.CharacterName, AvatarKey: v.AvatarKey},
		CreatedAt: formatTime(v.CreatedAt), StartedAt: formatTimePtr(v.StartedAt), EndedAt: formatTimePtr(v.EndedAt),
	}
	if v.EndReason != nil {
		s := string(*v.EndReason)
		it.EndReason = &s
	}
	return it
}

// turnResponse is one turn of the transcript.
type turnResponse struct {
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

func newTurnResponse(t session.TurnRecord) turnResponse {
	status := ""
	if t.Status != nil {
		status = string(*t.Status)
	}
	return turnResponse{
		ID: t.ID, SequenceNumber: t.Sequence, Status: status, UserText: t.UserText, UserStartedAt: formatTimePtr(t.UserStartedAt),
		UserEndedAt: formatTimePtr(t.UserEndedAt), AssistantText: t.AssistantText, AssistantSpokenText: t.AssistantSpokenText,
		CreatedAt: formatTime(t.CreatedAt), CompletedAt: formatTimePtr(t.CompletedAt),
	}
}

// ticketResponse is POST /sessions/{id}/realtime-ticket.
type ticketResponse struct {
	SessionID   uuid.UUID `json:"session_id"`
	Ticket      string    `json:"ticket"`
	ExpiresIn   int       `json:"expires_in"`
	RealtimeURL string    `json:"realtime_url"`
}

func newTicketResponse(id uuid.UUID, r auth.TicketResult) ticketResponse {
	return ticketResponse{SessionID: id, Ticket: r.Ticket, ExpiresIn: r.ExpiresIn, RealtimeURL: r.RealtimeURL}
}

// parseStatuses reads the comma-separated `status` filter.
func parseStatuses(raw string) ([]session.Status, error) {
	if raw == "" {
		return nil, nil
	}
	var out []session.Status
	for _, part := range strings.Split(raw, ",") {
		st, err := session.StatusFromString(strings.TrimSpace(part))
		if err != nil {
			return nil, FieldError("status", "not_allowed")
		}
		out = append(out, st)
	}
	return out, nil
}
