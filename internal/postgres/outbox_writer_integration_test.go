//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func sampleEvent(userID uuid.UUID) events.Event {
	p := events.SessionCompletedPayload{
		SessionID: uuid.New(), FinalStatus: "COMPLETED", EndReason: "USER_ENDED", ConversationStartMode: "USER_FIRST",
		TopicID: uuid.New(), ScenarioID: uuid.New(), ScenarioVersionID: uuid.New(), CharacterID: uuid.New(), PromptVersionID: uuid.New(),
		DurationSeconds: 300, StartedAt: t0, EndedAt: t0.Add(5 * time.Minute), ActiveSeconds: 295, ChargedSeconds: 295, TurnCount: 6, UserTurnCount: 3,
	}
	return events.NewSessionCompleted(uuid.Must(uuid.NewV7()), t0.Add(5*time.Minute), events.ProducerRealtime, userID, p)
}

func TestOutboxWriterInsertsPendingRow(t *testing.T) {
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	ev := sampleEvent(uuid.New())
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))

	var typ, status, aggType, producer string
	var attempts, version int
	var payload string
	var traceparent *string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT event_type, status, attempts, event_version, aggregate_type, producer, payload::text, traceparent FROM event_outbox WHERE event_id = $1`, ev.EventID).
		Scan(&typ, &status, &attempts, &version, &aggType, &producer, &payload, &traceparent))
	require.Equal(t, "session.completed", typ)
	require.Equal(t, "PENDING", status)
	require.Zero(t, attempts)
	require.Equal(t, 1, version)
	require.Equal(t, "SESSION", aggType)
	require.Equal(t, "REALTIME", producer)
	require.Nil(t, traceparent, "no active span in the test context")
	require.Contains(t, payload, `"final_status": "COMPLETED"`)
	require.Contains(t, payload, `"started_at": "2026-10-14T09:00:00.000Z"`)
}

func TestOutboxWriterRollsBackWithTx(t *testing.T) {
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	ev := sampleEvent(uuid.New())
	boom := errors.New("boom")
	err := store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		if err := tx.Outbox().Enqueue(ctx, ev); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox`).Scan(&n))
	require.Zero(t, n)
}
