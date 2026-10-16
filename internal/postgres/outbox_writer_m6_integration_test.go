//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func outboxCount(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	require.NoError(t, store.Pool().QueryRow(context.Background(), `SELECT count(*) FROM event_outbox`).Scan(&n))
	return n
}

func TestOutboxWriter_StoresTraceparentFromActiveSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_ = rec
	store := postgres.NewStore(testutil.NewPostgres(t))
	ev := sampleEvent(uuid.New())

	ctx, span := tracerForTest().Start(context.Background(), "outbox-test")
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
	span.End()

	var tp, ts *string
	require.NoError(t, store.Pool().QueryRow(context.Background(),
		`SELECT traceparent, tracestate FROM event_outbox WHERE event_id = $1`, ev.EventID).Scan(&tp, &ts))
	require.NotNil(t, tp)
	sc := span.SpanContext()
	// The active span at Enqueue is the db.tx span (child of outbox-test) in the same trace.
	require.Regexp(t, regexp.MustCompile(`^00-`+sc.TraceID().String()+`-[0-9a-f]{16}-01$`), *tp)
	require.Nil(t, ts, "tracestate stays NULL when the span carries none")
}

func TestOutboxWriter_TracestateStoredWhenPresent(t *testing.T) {
	testutil.NewTraceRecorder(t)
	store := postgres.NewStore(testutil.NewPostgres(t))
	ev := sampleEvent(uuid.New())

	ctx := contextWithRemoteTracestate(t, "vendor=abc")
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
	var ts *string
	require.NoError(t, store.Pool().QueryRow(context.Background(),
		`SELECT tracestate FROM event_outbox WHERE event_id = $1`, ev.EventID).Scan(&ts))
	require.NotNil(t, ts)
	require.Equal(t, "vendor=abc", *ts)
}

func TestOutboxWriter_NoSpan_NullTrace(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	ev := sampleEvent(uuid.New())
	// No tracer provider installed and no span in the context: the trace columns stay NULL.
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
	var tp, ts *string
	require.NoError(t, store.Pool().QueryRow(context.Background(),
		`SELECT traceparent, tracestate FROM event_outbox WHERE event_id = $1`, ev.EventID).Scan(&tp, &ts))
	require.Nil(t, tp)
	require.Nil(t, ts)
}

func TestOutboxWriter_AllFourEventTypes(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	user := uuid.New()
	now := t0.Add(time.Hour)
	evals := []events.Event{
		sampleEvent(user),
		events.NewEvaluationCompleted(uuid.Must(uuid.NewV7()), now, events.ProducerWorker, user,
			events.EvaluationCompletedV1{EvaluationID: uuid.New(), SessionID: uuid.New(), RubricVersion: "rubric-v1", CompletedAt: now, SessionEndedAt: now}),
		events.NewEvaluationFailed(uuid.Must(uuid.NewV7()), now, events.ProducerWorker, user,
			events.EvaluationFailedV1{EvaluationID: uuid.New(), SessionID: uuid.New(), FailureCode: "INTERNAL_ERROR", AttemptCount: 1, FailedAt: now}),
		events.NewMemoryExtracted(uuid.Must(uuid.NewV7()), now, events.ProducerWorker, user,
			events.MemoryExtractedV1{SessionID: uuid.New(), PromptVersionID: uuid.New(), Provider: "FAKE", Model: "m", ExtractedAt: now}),
	}
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		for _, ev := range evals {
			if err := tx.Outbox().Enqueue(ctx, ev); err != nil {
				return err
			}
		}
		return nil
	}))
	rows, err := store.Pool().Query(context.Background(), `SELECT event_type, aggregate_type FROM event_outbox ORDER BY event_type`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var typ, agg string
		require.NoError(t, rows.Scan(&typ, &agg))
		got[typ] = agg
	}
	require.Equal(t, map[string]string{
		"session.completed": "SESSION", "evaluation.completed": "EVALUATION", "evaluation.failed": "EVALUATION", "memory.extracted": "SESSION",
	}, got)

	// Any other type violates the CHECK and rolls the whole transaction back.
	bad := sampleEvent(user)
	bad.EventType = "session.exploded"
	err = store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, bad)
	})
	require.Error(t, err)
	require.Equal(t, 4, outboxCount(t, store))
}

func TestOutboxWriter_PayloadOver64KiB_Rejected(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	ev := sampleEvent(uuid.New())
	// jsonb re-renders the document with ": " and ", " spacing, so size the raw value, then check
	// both sides of the 65536-byte limit on the stored text form.
	ev.Payload = map[string]string{"x": strings.Repeat("a", 65536-len(`{"x": ""}`)+1)}
	err := store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	})
	require.Error(t, err)
	require.Zero(t, outboxCount(t, store))

	ev.Payload = map[string]string{"x": strings.Repeat("a", 65536-len(`{"x": ""}`))}
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}), "exactly 65536 bytes is allowed")
	require.Equal(t, 1, outboxCount(t, store))
}

func TestOutboxWriter_RollbackLeavesNoEvent(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	boom := errors.New("business failure after enqueue")
	err := store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		if err := tx.Outbox().Enqueue(ctx, sampleEvent(uuid.New())); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, outboxCount(t, store))
}
