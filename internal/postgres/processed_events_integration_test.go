//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func envelopeOf(id uuid.UUID, typ string) events.Envelope {
	return events.Envelope{EventID: id, EventType: typ, EventVersion: 1}
}

func markIn(t *testing.T, store *postgres.Store, consumer string, env events.Envelope) error {
	t.Helper()
	return store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Processed().MarkProcessed(ctx, consumer, env)
	})
}

func processedCount(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	require.NoError(t, store.Pool().QueryRow(context.Background(), `SELECT count(*) FROM processed_events`).Scan(&n))
	return n
}

func TestMarkProcessed_FirstInsertThenDuplicate(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	env := envelopeOf(uuid.Must(uuid.NewV7()), events.SessionCompleted)

	require.NoError(t, markIn(t, store, events.QueueEvaluation, env))
	err := markIn(t, store, events.QueueEvaluation, env)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	require.Equal(t, 1, processedCount(t, store))

	var consumer, typ string
	require.NoError(t, store.Pool().QueryRow(context.Background(),
		`SELECT consumer, event_type FROM processed_events WHERE event_id = $1`, env.EventID).Scan(&consumer, &typ))
	require.Equal(t, events.QueueEvaluation, consumer)
	require.Equal(t, events.SessionCompleted, typ)
}

func TestMarkProcessed_RollbackLeavesNoRow(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	env := envelopeOf(uuid.Must(uuid.NewV7()), events.SessionCompleted)
	boom := errors.New("boom")

	err := store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		if err := tx.Processed().MarkProcessed(ctx, events.QueueMemory, env); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, processedCount(t, store))
	// Because the first attempt rolled back, the event can still be processed.
	require.NoError(t, markIn(t, store, events.QueueMemory, env))
}

func TestMarkProcessed_UnknownConsumer_NotErrAlreadyProcessed(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	err := markIn(t, store, "no.such_consumer", envelopeOf(uuid.Must(uuid.NewV7()), events.SessionCompleted))
	require.Error(t, err)
	require.NotErrorIs(t, err, events.ErrAlreadyProcessed)
	require.Zero(t, processedCount(t, store))
}

func TestMarkProcessed_SameEventDifferentConsumers_BothOK(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	env := envelopeOf(uuid.Must(uuid.NewV7()), events.SessionCompleted)
	for _, c := range []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary} {
		require.NoError(t, markIn(t, store, c, env), c)
	}
	require.Equal(t, 3, processedCount(t, store))
}

func TestMarkProcessed_ConcurrentDuplicates_ExactlyOneWins(t *testing.T) {
	store := postgres.NewStore(testutil.NewPostgres(t))
	env := envelopeOf(uuid.Must(uuid.NewV7()), events.SessionCompleted)
	const n = 8
	results := make(chan error, n)
	for range n {
		go func() { results <- markIn(t, store, events.QueueSummary, env) }()
	}
	var ok, dup int
	for range n {
		switch err := <-results; {
		case err == nil:
			ok++
		case errors.Is(err, events.ErrAlreadyProcessed):
			dup++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, n-1, dup)
}

func TestIsProcessed(t *testing.T) {
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	reader := postgres.NewProcessedEventsReader(pool)
	ctx := context.Background()
	env := envelopeOf(uuid.Must(uuid.NewV7()), events.EvaluationCompleted)

	got, err := reader.IsProcessed(ctx, events.QueueLearner, env.EventID)
	require.NoError(t, err)
	require.False(t, got)

	require.NoError(t, markIn(t, store, events.QueueLearner, env))
	got, err = reader.IsProcessed(ctx, events.QueueLearner, env.EventID)
	require.NoError(t, err)
	require.True(t, got)

	other, err := reader.IsProcessed(ctx, events.QueueEvaluation, env.EventID)
	require.NoError(t, err)
	require.False(t, other, "a different consumer has not processed it")
}
