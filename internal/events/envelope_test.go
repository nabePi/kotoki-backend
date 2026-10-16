package events_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
)

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func TestEnvelope_Golden_SessionCompleted(t *testing.T) {
	sid := uuid.MustParse("0192f1a0-1111-7aaa-8bbb-cccccccccccc")
	p := samplePayload()
	p.SessionID = sid
	p.StartedAt = time.Date(2026, 10, 12, 6, 15, 0, 0, time.UTC)
	p.EndedAt = time.Date(2026, 10, 12, 6, 30, 14, 500_000_000, time.UTC)
	p.ActiveSeconds, p.ChargedSeconds = 888, 888
	ev := events.NewSessionCompleted(uuid.MustParse("0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c"),
		time.Date(2026, 10, 12, 6, 30, 15, 123_456_789, time.UTC), events.ProducerRealtime,
		uuid.MustParse("0192e000-2222-7ddd-8eee-ffffffffffff"), p)

	env, err := ev.Envelope("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", nil)
	require.NoError(t, err)
	got, err := json.Marshal(env)
	require.NoError(t, err)
	require.JSONEq(t, golden(t, "envelope_session_completed.golden.json"), string(got))
	require.Contains(t, string(got), `"occurred_at":"2026-10-12T06:30:15.123Z"`, "truncated, not rounded")
	require.Contains(t, string(got), `"tracestate":null`)
}

func TestEnvelope_OccurredAtAlwaysThreeFractionDigits(t *testing.T) {
	for in, want := range map[time.Time]string{
		time.Date(2026, 10, 12, 6, 30, 15, 0, time.UTC):                                 `"occurred_at":"2026-10-12T06:30:15.000Z"`,
		time.Date(2026, 10, 12, 6, 30, 15, 120_000_000, time.UTC):                       `"occurred_at":"2026-10-12T06:30:15.120Z"`,
		time.Date(2026, 10, 12, 13, 30, 15, 999_999_999, time.FixedZone("WIB", 7*3600)): `"occurred_at":"2026-10-12T06:30:15.999Z"`,
	} {
		ev := events.NewSessionCompleted(uuid.New(), in, events.ProducerAPI, uuid.New(), samplePayload())
		env, err := ev.Envelope("tp", nil)
		require.NoError(t, err)
		b, err := json.Marshal(env)
		require.NoError(t, err)
		require.Contains(t, string(b), want)
	}
}

func TestEnvelope_TracestateAndTruncation(t *testing.T) {
	ts := "vendor=1"
	ev := events.NewSessionCompleted(uuid.New(), time.Date(2026, 10, 12, 6, 30, 15, 123_999_999, time.UTC), events.ProducerAPI, uuid.New(), samplePayload())
	env, err := ev.Envelope("tp", &ts)
	require.NoError(t, err)
	require.Equal(t, &ts, env.Tracestate)
	require.Equal(t, 123_000_000, env.OccurredAt.Nanosecond())
	require.Equal(t, time.UTC, env.OccurredAt.Location())
	b, err := json.Marshal(env)
	require.NoError(t, err)
	require.Contains(t, string(b), `"tracestate":"vendor=1"`)
}

func TestEnvelope_UnmarshalablePayloadFails(t *testing.T) {
	ev := events.Event{EventID: uuid.New(), EventType: "x", EventVersion: 1, Payload: make(chan int)}
	_, err := ev.Envelope("tp", nil)
	require.Error(t, err)
}

func TestEnvelope_RoundTrip(t *testing.T) {
	ev := events.NewSessionCompleted(uuid.New(), time.Date(2026, 10, 12, 6, 30, 15, 123_000_000, time.UTC), events.ProducerWorker, uuid.New(), samplePayload())
	env, err := ev.Envelope("tp", nil)
	require.NoError(t, err)
	b, err := json.Marshal(env)
	require.NoError(t, err)
	back, err := events.DecodeEnvelope(b)
	require.NoError(t, err)
	require.Equal(t, env.EventID, back.EventID)
	require.Equal(t, env.OccurredAt, back.OccurredAt)
	require.Equal(t, env.UserID, back.UserID)
	require.Equal(t, "WORKER", back.Producer)
	require.JSONEq(t, string(env.Payload), string(back.Payload))
	p, err := events.DecodePayload[events.SessionCompletedV1](back)
	require.NoError(t, err)
	require.Equal(t, samplePayload().SessionID, p.SessionID)
	require.Equal(t, 23, p.TurnCount)
}

const validEnvelope = `{"event_id":"0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c","event_type":"session.completed","event_version":1,
"occurred_at":"2026-10-12T06:30:15.123Z","producer":"REALTIME","aggregate_type":"SESSION",
"aggregate_id":"0192f1a0-1111-7aaa-8bbb-cccccccccccc","user_id":"0192e000-2222-7ddd-8eee-ffffffffffff",
"traceparent":"tp","tracestate":null,"future_field":{"a":1},"payload":%s}`

func TestDecodeEnvelope(t *testing.T) {
	body := func(payload string) []byte { return []byte(sprintf(validEnvelope, payload)) }

	t.Run("unknown fields ignored, payload kept verbatim", func(t *testing.T) {
		env, err := events.DecodeEnvelope(body(`{"x": 1,  "y":[2]}`))
		require.NoError(t, err)
		require.Equal(t, `{"x": 1,  "y":[2]}`, string(env.Payload))
		require.Equal(t, "session.completed", env.EventType)
		require.Equal(t, 1, env.EventVersion)
		require.Nil(t, env.Tracestate)
	})
	t.Run("missing event_id", func(t *testing.T) {
		b := []byte(`{"event_type":"x","event_version":1,"payload":{}}`)
		_, err := events.DecodeEnvelope(b)
		require.ErrorContains(t, err, "event_id")
	})
	t.Run("missing event_type", func(t *testing.T) {
		b := []byte(`{"event_id":"0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c","event_version":1,"payload":{}}`)
		_, err := events.DecodeEnvelope(b)
		require.ErrorContains(t, err, "event_type")
	})
	t.Run("version zero", func(t *testing.T) {
		b := []byte(`{"event_id":"0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c","event_type":"x","event_version":0,"payload":{}}`)
		_, err := events.DecodeEnvelope(b)
		require.ErrorContains(t, err, "event_version")
	})
	t.Run("version absent", func(t *testing.T) {
		b := []byte(`{"event_id":"0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c","event_type":"x","payload":{}}`)
		_, err := events.DecodeEnvelope(b)
		require.ErrorContains(t, err, "event_version")
	})
	t.Run("payload missing or null", func(t *testing.T) {
		b := []byte(`{"event_id":"0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c","event_type":"x","event_version":1}`)
		_, err := events.DecodeEnvelope(b)
		require.ErrorContains(t, err, "payload")
		_, err = events.DecodeEnvelope(body(`null`))
		require.ErrorContains(t, err, "payload")
	})
	t.Run("not json", func(t *testing.T) {
		_, err := events.DecodeEnvelope([]byte(`{nope`))
		require.Error(t, err)
	})
	t.Run("bad event_id", func(t *testing.T) {
		_, err := events.DecodeEnvelope([]byte(`{"event_id":"zzz","event_type":"x","event_version":1,"payload":{}}`))
		require.Error(t, err)
	})
}

func TestDecodePayload_Invalid_IsPermanentDecodeError(t *testing.T) {
	_, err := events.DecodePayload[events.SessionCompletedV1](events.Envelope{Payload: json.RawMessage(`{"session_id": 5}`)})
	var pe *events.PermanentError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "DECODE_ERROR", pe.Code)

	_, err = events.DecodePayload[events.SessionCompletedV1](events.Envelope{Payload: json.RawMessage(`[1]`)})
	require.ErrorAs(t, err, &pe)
}

func TestQueueConstants(t *testing.T) {
	require.Equal(t, "evaluation.session_completed", events.QueueEvaluation)
	require.Equal(t, "memory.session_completed", events.QueueMemory)
	require.Equal(t, "summary.session_completed", events.QueueSummary)
	require.Equal(t, "learner.evaluation_completed", events.QueueLearner)
}

func TestSupportedVersions(t *testing.T) {
	require.Equal(t, map[string]int{
		"session.completed": 1, "evaluation.completed": 1, "evaluation.failed": 1, "memory.extracted": 1,
	}, events.SupportedVersions)
	require.Equal(t, 1, events.CurrentVersion)
}
