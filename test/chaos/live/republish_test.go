package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/live"
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

func row() live.OutboxEvent {
	return live.OutboxEvent{
		EventID: "0192a3f0-0000-7000-8000-000000000001", EventType: "session.completed", EventVersion: 1,
		AggregateType: "SESSION", AggregateID: "0192a3f0-0000-7000-8000-000000000002", UserID: "0192a3f0-0000-7000-8000-000000000003",
		Producer: "realtime", Payload: json.RawMessage(`{"session_id":"x","charged_seconds":12}`),
		OccurredAt: time.Date(2026, 10, 17, 12, 0, 0, 123456789, time.FixedZone("WIB", 7*3600)),
	}
}

// TestPublishBody_IsTheSameEventAgain: the message id and the envelope carry the id of the original
// event, which is what the consumers deduplicate on (D-093).
func TestPublishBody_IsTheSameEventAgain(t *testing.T) {
	body, err := live.PublishBody(row())
	require.NoError(t, err)
	var m struct {
		Properties map[string]any `json:"properties"`
		RoutingKey string         `json:"routing_key"`
		Payload    string         `json:"payload"`
		Encoding   string         `json:"payload_encoding"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &m))
	require.Equal(t, "session.completed", m.RoutingKey)
	require.Equal(t, "string", m.Encoding)
	require.Equal(t, "0192a3f0-0000-7000-8000-000000000001", m.Properties["message_id"])
	require.Equal(t, "session.completed", m.Properties["type"])
	require.Equal(t, "application/json", m.Properties["content_type"])
	require.EqualValues(t, 2, m.Properties["delivery_mode"])
	require.Equal(t, map[string]any{"x-event-version": 1.0}, m.Properties["headers"])

	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(m.Payload), &env))
	require.Equal(t, "0192a3f0-0000-7000-8000-000000000001", env["event_id"])
	require.Equal(t, "session.completed", env["event_type"])
	require.Equal(t, "2026-10-17T05:00:00.123Z", env["occurred_at"], "UTC, millisecond precision like the publisher")
	require.Equal(t, "SESSION", env["aggregate_type"])
	require.Equal(t, map[string]any{"session_id": "x", "charged_seconds": 12.0}, env["payload"])
	require.Contains(t, env, "traceparent")
	require.Contains(t, env, "tracestate")
}

func TestRepublisher_ResolvesOnlyItsToken(t *testing.T) {
	r := live.Republisher{Find: func(context.Context, time.Time) (live.OutboxEvent, error) { return row(), nil }, Since: time.Now()}
	v, err := r.Resolve(t.Context(), scenario.F13Token)
	require.NoError(t, err)
	require.Contains(t, v, `"routing_key":"session.completed"`)
	_, err = r.Resolve(t.Context(), "@republish:evaluation.completed")
	require.Error(t, err)
	_, err = r.Resolve(t.Context(), "@pod:x")
	require.Error(t, err)
}

func TestRepublisher_NothingToRepublishIsAnError(t *testing.T) {
	r := live.Republisher{Find: func(context.Context, time.Time) (live.OutboxEvent, error) {
		return live.OutboxEvent{}, errors.New("no published session.completed since the run started")
	}}
	_, err := r.Resolve(t.Context(), scenario.F13Token)
	require.ErrorContains(t, err, "no published")
}
