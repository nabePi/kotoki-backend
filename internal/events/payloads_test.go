package events_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func ptr[T any](v T) *T { return &v }

var (
	evalID    = uuid.MustParse("0192f1b0-0000-7000-8000-000000000001")
	sessionID = uuid.MustParse("0192f1a0-1111-7aaa-8bbb-cccccccccccc")
)

func sampleEvaluationCompleted() events.EvaluationCompletedV1 {
	return events.EvaluationCompletedV1{
		EvaluationID: evalID, SessionID: sessionID, RubricVersion: "rubric-v1",
		Scores:                  events.EvalScores{Grammar: ptr(72), Vocabulary: ptr(65), Fluency: ptr(80), TaskAchievement: ptr(90)},
		DimensionConfidence:     map[string]float64{"GRAMMAR": 0.8, "VOCABULARY": 0.7, "FLUENCY": 0.9, "TASK_ACHIEVEMENT": 0.6},
		Signals:                 events.EvalSignals{ListeningResponse: &events.EvalSignal{Score: 70, Confidence: 0.5}},
		OverallScore:            ptr(76.75),
		SessionEndedAt:          time.Date(2026, 10, 12, 6, 30, 14, 500_000_000, time.UTC),
		ScenarioDifficultyLevel: 3, UserTurnCount: 21, UserSpeechSeconds: 240,
		FindingCounts: map[string]events.SeverityCounts{"GRAMMAR": {Major: 1, Minor: 3}},
		CompletedAt:   time.Date(2026, 10, 12, 6, 31, 2, 7_000_000, time.UTC),
	}
}

func keysOf(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func TestPayloadRequiredKeys(t *testing.T) {
	required := map[string][]string{
		"session.completed": {"session_id", "final_status", "end_reason", "conversation_start_mode", "topic_id", "scenario_id",
			"scenario_version_id", "character_id", "prompt_version_id", "duration_seconds", "started_at", "ended_at",
			"active_seconds", "charged_seconds", "turn_count", "user_turn_count"},
		"evaluation.completed": {"evaluation_id", "session_id", "rubric_version", "insufficient_evidence", "scores",
			"dimension_confidence", "signals", "overall_score", "session_ended_at", "scenario_difficulty_level",
			"user_turn_count", "user_speech_seconds", "finding_counts", "completed_at"},
		"evaluation.failed": {"evaluation_id", "session_id", "failure_code", "attempt_count", "failed_at"},
		"memory.extracted": {"session_id", "created_memory_ids", "updated_memory_ids", "superseded_memory_ids",
			"prompt_version_id", "provider", "model", "extracted_at"},
	}
	payloads := map[string]any{
		"session.completed":    samplePayload(),
		"evaluation.completed": events.EvaluationCompletedV1{}, // zero value: every key must still be present
		"evaluation.failed":    events.EvaluationFailedV1{},
		"memory.extracted":     events.MemoryExtractedV1{},
	}
	for typ, keys := range required {
		got := keysOf(t, payloads[typ])
		for _, k := range keys {
			require.Contains(t, got, k, "%s must carry %s", typ, k)
		}
	}

	// Zero-value evaluation.completed: nullable parts are null, objects/arrays are never null.
	ev := keysOf(t, events.EvaluationCompletedV1{})
	require.Equal(t, "null", string(ev["overall_score"]))
	require.Equal(t, "{}", string(ev["dimension_confidence"]))
	require.Equal(t, "{}", string(ev["finding_counts"]))
	require.JSONEq(t, `{"LISTENING_RESPONSE":null,"INTERACTION_COMPLEXITY":null}`, string(ev["signals"]))
	mem := keysOf(t, events.MemoryExtractedV1{})
	for _, k := range []string{"created_memory_ids", "updated_memory_ids", "superseded_memory_ids"} {
		require.Equal(t, "[]", string(mem[k]), k)
	}
}

func TestEvaluationCompletedV1_ScoresHaveFiveKeysNullForNil(t *testing.T) {
	var scores map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(keysOf(t, events.EvaluationCompletedV1{Scores: events.EvalScores{Fluency: ptr(0)}})["scores"], &scores))
	require.Len(t, scores, 5)
	for _, k := range []string{"GRAMMAR", "VOCABULARY", "NATURALNESS", "TASK_ACHIEVEMENT"} {
		require.Equal(t, "null", string(scores[k]), k)
	}
	require.Equal(t, "0", string(scores["FLUENCY"]), "a zero score is not null")
}

func TestEvaluationCompletedV1_Golden(t *testing.T) {
	p := sampleEvaluationCompleted()
	p.CompletedAt = time.Date(2026, 10, 12, 6, 31, 2, 7_000_000, time.UTC)
	got, err := json.Marshal(p)
	require.NoError(t, err)
	require.JSONEq(t, golden(t, "evaluation_completed_v1.golden.json"), string(got))

	back, err := events.DecodePayload[events.EvaluationCompletedV1](events.Envelope{Payload: got})
	require.NoError(t, err)
	require.Equal(t, p, back)
}

func TestEvaluationFailedV1_Golden(t *testing.T) {
	p := events.EvaluationFailedV1{
		EvaluationID: evalID, SessionID: sessionID, FailureCode: "PROVIDER_UNAVAILABLE", AttemptCount: 3,
		FailedAt: time.Date(2026, 10, 12, 6, 40, 0, 0, time.UTC),
	}
	got, err := json.Marshal(p)
	require.NoError(t, err)
	require.JSONEq(t, golden(t, "evaluation_failed_v1.golden.json"), string(got))
}

func TestMemoryExtractedV1_Golden(t *testing.T) {
	p := events.MemoryExtractedV1{
		SessionID: sessionID, CreatedMemoryIDs: []uuid.UUID{uuid.MustParse("0192f1c0-0000-7000-8000-000000000001")},
		CategoryCounts:  map[string]int{"PREFERENCE": 1},
		PromptVersionID: uuid.MustParse("01920000-000e-7000-8000-000000000001"), Provider: "FAKE", Model: "fake-llm",
		ExtractedAt: time.Date(2026, 10, 12, 6, 32, 0, 250_000_000, time.UTC),
	}
	got, err := json.Marshal(p)
	require.NoError(t, err)
	require.JSONEq(t, golden(t, "memory_extracted_v1.golden.json"), string(got))
}

func TestBuilders_AggregateAndVersion(t *testing.T) {
	user := uuid.New()
	now := time.Date(2026, 10, 12, 6, 30, 15, 0, time.FixedZone("WIB", 7*3600))
	cases := []struct {
		ev       events.Event
		typ, agg string
		aggID    uuid.UUID
		wantProd string
	}{
		{events.NewEvaluationCompleted(uuid.New(), now, events.ProducerWorker, user, sampleEvaluationCompleted()),
			"evaluation.completed", "EVALUATION", evalID, "WORKER"},
		{events.NewEvaluationFailed(uuid.New(), now, events.ProducerWorker, user, events.EvaluationFailedV1{EvaluationID: evalID, SessionID: sessionID}),
			"evaluation.failed", "EVALUATION", evalID, "WORKER"},
		{events.NewMemoryExtracted(uuid.New(), now, events.ProducerWorker, user, events.MemoryExtractedV1{SessionID: sessionID}),
			"memory.extracted", "SESSION", sessionID, "WORKER"},
		{events.NewSessionCompleted(uuid.New(), now, events.ProducerRealtime, user, samplePayload()),
			"session.completed", "SESSION", samplePayload().SessionID, "REALTIME"},
	}
	for _, c := range cases {
		require.Equal(t, 1, c.ev.EventVersion, c.typ)
		require.Equal(t, c.typ, c.ev.EventType)
		require.Equal(t, c.agg, c.ev.AggregateType, c.typ)
		require.Equal(t, c.aggID, c.ev.AggregateID, c.typ)
		require.Equal(t, c.wantProd, c.ev.Producer)
		require.Equal(t, user, c.ev.UserID)
		require.Equal(t, time.UTC, c.ev.OccurredAt.Location())
		require.NotEqual(t, uuid.Nil, c.ev.EventID)
		require.Equal(t, events.SupportedVersions[c.typ], c.ev.EventVersion)
	}
}
