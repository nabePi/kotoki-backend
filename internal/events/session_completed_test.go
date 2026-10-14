package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
)

func samplePayload() events.SessionCompletedPayload {
	return events.SessionCompletedPayload{
		SessionID: uuid.MustParse("0192f1a0-0000-7000-8000-000000000001"), FinalStatus: "COMPLETED", EndReason: "USER_ENDED",
		ConversationStartMode: "AI_FIRST",
		TopicID:               uuid.MustParse("01920000-0001-7000-8000-000000000001"), ScenarioID: uuid.MustParse("01920000-0002-7000-8000-000000000003"),
		ScenarioVersionID: uuid.MustParse("01920000-0003-7000-8000-000000000003"), CharacterID: uuid.MustParse("01920000-000a-7000-8000-000000000001"),
		PromptVersionID: uuid.MustParse("01920000-000d-7000-8000-000000000001"), DurationSeconds: 900,
		StartedAt: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), EndedAt: time.Date(2026, 10, 12, 10, 10, 0, 123_456_789, time.UTC),
		ActiveSeconds: events.ActiveSeconds(588_999), ChargedSeconds: 588, TurnCount: 23, UserTurnCount: 21,
	}
}

func TestNewSessionCompletedFields(t *testing.T) {
	id := uuid.MustParse("0192f1a0-ffff-7000-8000-000000000009")
	user := uuid.MustParse("0192f1a0-eeee-7000-8000-000000000002")
	now := time.Date(2026, 10, 12, 10, 10, 1, 0, time.FixedZone("WIB", 7*3600))
	ev := events.NewSessionCompleted(id, now, events.ProducerRealtime, user, samplePayload())

	require.Equal(t, id, ev.EventID)
	require.Equal(t, "session.completed", ev.EventType)
	require.Equal(t, 1, ev.EventVersion)
	require.Equal(t, "SESSION", ev.AggregateType)
	require.Equal(t, samplePayload().SessionID, ev.AggregateID)
	require.Equal(t, user, ev.UserID)
	require.Equal(t, "REALTIME", ev.Producer)
	require.Equal(t, time.UTC, ev.OccurredAt.Location())

	got, err := json.Marshal(ev.Payload)
	require.NoError(t, err)
	require.JSONEq(t, `{
  "session_id": "0192f1a0-0000-7000-8000-000000000001", "final_status": "COMPLETED", "end_reason": "USER_ENDED",
  "conversation_start_mode": "AI_FIRST", "topic_id": "01920000-0001-7000-8000-000000000001",
  "scenario_id": "01920000-0002-7000-8000-000000000003", "scenario_version_id": "01920000-0003-7000-8000-000000000003",
  "character_id": "01920000-000a-7000-8000-000000000001", "prompt_version_id": "01920000-000d-7000-8000-000000000001",
  "duration_seconds": 900, "started_at": "2026-10-12T10:00:00.000Z", "ended_at": "2026-10-12T10:10:00.123Z",
  "active_seconds": 588, "charged_seconds": 588, "turn_count": 23, "user_turn_count": 21}`, string(got))
}

func TestActiveSecondsFloors(t *testing.T) {
	for ms, want := range map[int64]int{0: 0, 999: 0, 1000: 1, 588_999: 588, -5: 0} {
		require.Equal(t, want, events.ActiveSeconds(ms), ms)
	}
}

func TestPayloadKeysAreExactlyTheV1Set(t *testing.T) {
	raw, err := json.Marshal(samplePayload())
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Len(t, m, 16)
}
