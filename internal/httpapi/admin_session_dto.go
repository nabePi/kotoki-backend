package httpapi

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type adminReservationResponse struct {
	ID              uuid.UUID `json:"id"`
	Status          string    `json:"status"`
	ReservedSeconds int64     `json:"reserved_seconds"`
	ConsumedSeconds *int64    `json:"consumed_seconds"`
	ReleasedSeconds *int64    `json:"released_seconds"`
	StartDeadlineAt string    `json:"start_deadline_at"`
	SettledAt       *string   `json:"settled_at"`
	ReleasedAt      *string   `json:"released_at"`
}

func newAdminReservationResponse(r *balance.Reservation) *adminReservationResponse {
	if r == nil {
		return nil
	}
	return &adminReservationResponse{
		ID: r.ID, Status: string(r.Status), ReservedSeconds: r.ReservedSeconds, ConsumedSeconds: r.ConsumedSeconds,
		ReleasedSeconds: r.ReleasedSeconds, StartDeadlineAt: formatTime(r.StartDeadlineAt),
		SettledAt: formatTimePtr(r.SettledAt), ReleasedAt: formatTimePtr(r.ReleasedAt),
	}
}

// adminSessionResponse is AdminSession (spec 5.9.8): the user's Session plus the internal fields.
// config_snapshot arrives already redacted from the service.
type adminSessionResponse struct {
	sessionResponse
	UserID                 uuid.UUID                 `json:"user_id"`
	ScenarioVersionID      uuid.UUID                 `json:"scenario_version_id"`
	ConfigSnapshot         json.RawMessage           `json:"config_snapshot"`
	PromptVersionID        uuid.UUID                 `json:"prompt_version_id"`
	OpeningPromptVersionID *uuid.UUID                `json:"opening_prompt_version_id"`
	AdaptiveDifficulty     json.RawMessage           `json:"adaptive_difficulty"`
	RuntimeFence           int64                     `json:"runtime_fence"`
	HandoffAt              *string                   `json:"handoff_at"`
	FailureDetail          *string                   `json:"failure_detail"`
	TranscriptPurgedAt     *string                   `json:"transcript_purged_at"`
	Version                int64                     `json:"version"`
	Reservation            *adminReservationResponse `json:"reservation"`
}

func rawOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func newAdminSessionResponse(v session.AdminView, now time.Time) adminSessionResponse {
	return adminSessionResponse{
		sessionResponse: newSessionResponse(v.View, now), UserID: v.UserID, ScenarioVersionID: v.ScenarioVersionID,
		ConfigSnapshot: rawOrNull(v.ConfigSnapshot), PromptVersionID: v.PromptVersionID, OpeningPromptVersionID: v.OpeningPromptVersionID,
		AdaptiveDifficulty: rawOrNull(v.AdaptiveDifficulty), RuntimeFence: v.RuntimeFence, HandoffAt: formatTimePtr(v.HandoffAt),
		FailureDetail: v.FailureDetail, TranscriptPurgedAt: formatTimePtr(v.TranscriptPurgedAt), Version: v.Version,
		Reservation: newAdminReservationResponse(v.Reservation),
	}
}

// adminTurnResponse carries every session_turns column.
type adminTurnResponse struct {
	turnResponse
	Status                *string `json:"status"` // null while the turn is still running; shadows the embedded field
	Generation            int     `json:"generation"`
	STTProvider           *string `json:"stt_provider"`
	LLMProvider           *string `json:"llm_provider"`
	LLMModel              *string `json:"llm_model"`
	TTSProvider           *string `json:"tts_provider"`
	TTSModel              *string `json:"tts_model"`
	TTSVoiceID            *string `json:"tts_voice_id"`
	UsedFallback          bool    `json:"used_fallback"`
	TurnEndToFirstAudioMS *int    `json:"turn_end_to_first_audio_ms"`
	STTFinalMS            *int    `json:"stt_final_ms"`
	LLMFirstTokenMS       *int    `json:"llm_first_token_ms"`
	TTSFirstAudioMS       *int    `json:"tts_first_audio_ms"`
	InputTokens           *int    `json:"input_tokens"`
	OutputTokens          *int    `json:"output_tokens"`
	TTSCharacters         *int    `json:"tts_characters"`
	STTAudioMS            *int    `json:"stt_audio_ms"`
	ErrorCode             *string `json:"error_code"`
}

func newAdminTurnResponse(t session.TurnRecord) adminTurnResponse {
	var status *string
	if t.Status != nil {
		s := string(*t.Status)
		status = &s
	}
	return adminTurnResponse{
		turnResponse: newTurnResponse(t), Status: status, Generation: t.Generation, STTProvider: t.STTProvider, LLMProvider: t.LLMProvider,
		LLMModel: t.LLMModel, TTSProvider: t.TTSProvider, TTSModel: t.TTSModel, TTSVoiceID: t.TTSVoiceID, UsedFallback: t.UsedFallback,
		TurnEndToFirstAudioMS: t.TurnEndToFirstAudioMS, STTFinalMS: t.STTFinalMS, LLMFirstTokenMS: t.LLMFirstTokenMS,
		TTSFirstAudioMS: t.TTSFirstAudioMS, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
		TTSCharacters: t.TTSCharacters, STTAudioMS: t.STTAudioMS, ErrorCode: t.ErrorCode,
	}
}

type adminEventResponse struct {
	ID         uuid.UUID       `json:"id"`
	EventType  string          `json:"event_type"`
	Actor      string          `json:"actor"`
	FromStatus *string         `json:"from_status"`
	ToStatus   *string         `json:"to_status"`
	TurnID     *uuid.UUID      `json:"turn_id"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  string          `json:"created_at"`
}

func newAdminEventResponse(e session.Event) adminEventResponse {
	r := adminEventResponse{ID: e.ID, EventType: string(e.Type), Actor: string(e.Actor), TurnID: e.TurnID, Payload: rawOrNull(e.Payload), CreatedAt: formatTime(e.CreatedAt)}
	if e.FromStatus != nil {
		s := string(*e.FromStatus)
		r.FromStatus = &s
	}
	if e.ToStatus != nil {
		s := string(*e.ToStatus)
		r.ToStatus = &s
	}
	return r
}

// eventCursorKey is the cursor key of the events timeline: [created_at (RFC 3339, nanoseconds), id].
// Milliseconds would lose the microseconds of timestamptz and repeat or skip rows.
func eventCursorKey(e session.Event) []string {
	return []string{e.CreatedAt.UTC().Format(time.RFC3339Nano), e.ID.String()}
}

func decodeEventCursor(raw string) (*session.EventCursor, error) {
	if raw == "" {
		return nil, nil
	}
	var key []string
	if err := DecodeCursor(raw, &key); err != nil {
		return nil, err
	}
	invalid := errs.New(errs.CodeInvalidCursor, "invalid cursor")
	if len(key) != 2 {
		return nil, invalid
	}
	at, err := time.Parse(time.RFC3339Nano, key[0])
	if err != nil {
		return nil, invalid
	}
	id, err := uuid.Parse(key[1])
	if err != nil {
		return nil, invalid
	}
	return &session.EventCursor{CreatedAt: at, ID: id}, nil
}
