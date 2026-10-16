//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func outboxRow(status string, created time.Time, published *time.Time) testutil.Row {
	r := testutil.Row{
		"event_id": uuid.Must(uuid.NewV7()), "event_type": "session.completed", "aggregate_type": "SESSION",
		"aggregate_id": uuid.Must(uuid.NewV7()), "user_id": uuid.Must(uuid.NewV7()), "producer": "API",
		"payload": `{}`, "occurred_at": created, "status": status, "created_at": created,
	}
	if published != nil {
		r["published_at"] = *published
	}
	return r
}

func seedOutbox(t *testing.T, pool *pgxpool.Pool, status string, created time.Time, published *time.Time) uuid.UUID {
	t.Helper()
	r := outboxRow(status, created, published)
	testutil.MustInsert(context.Background(), t, pool, "event_outbox", r)
	return r["event_id"].(uuid.UUID)
}

func seedProcessed(t *testing.T, pool *pgxpool.Pool, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	testutil.MustInsert(context.Background(), t, pool, "processed_events", testutil.Row{
		"consumer": "memory.session_completed", "event_id": id, "event_type": "session.completed", "processed_at": at,
	})
	return id
}

func outboxExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox WHERE event_id = $1`, id).Scan(&n))
	return n == 1
}

func processedExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE event_id = $1`, id).Scan(&n))
	return n == 1
}

func TestOutboxRetention_DeletesOnlyEligible(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewRetentionRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	pub8 := seedOutbox(t, pool, "PUBLISHED", now.Add(-9*day), ago(8*day))
	pub6 := seedOutbox(t, pool, "PUBLISHED", now.Add(-9*day), ago(6*day))
	pendingOld := seedOutbox(t, pool, "PENDING", now.Add(-60*day), nil)
	failed31 := seedOutbox(t, pool, "FAILED", now.Add(-31*day), nil)
	failed29 := seedOutbox(t, pool, "FAILED", now.Add(-29*day), nil)
	proc31 := seedProcessed(t, pool, now.Add(-31*day))
	proc29 := seedProcessed(t, pool, now.Add(-29*day))

	n, err := repo.DeleteOutboxPublished(ctx, now.Add(-7*day), 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = repo.DeleteOutboxFailed(ctx, now.Add(-30*day), 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = repo.DeleteProcessedEvents(ctx, now.Add(-30*day), 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	assert.False(t, outboxExists(t, pool, pub8), "published 8d: deleted")
	assert.True(t, outboxExists(t, pool, pub6), "published 6d: kept")
	assert.True(t, outboxExists(t, pool, pendingOld), "PENDING is never touched, however old")
	assert.False(t, outboxExists(t, pool, failed31), "failed 31d: deleted")
	assert.True(t, outboxExists(t, pool, failed29), "failed 29d: kept")
	assert.False(t, processedExists(t, pool, proc31))
	assert.True(t, processedExists(t, pool, proc29))

	// Each step only touches its own status: a FAILED row is not removed by the published step.
	failedOld := seedOutbox(t, pool, "FAILED", now.Add(-90*day), nil)
	n, err = repo.DeleteOutboxPublished(ctx, now, 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only pub6 remains PUBLISHED")
	assert.True(t, outboxExists(t, pool, failedOld))
}

func TestOutboxRetention_Batches(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewRetentionRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO event_outbox (event_id, event_type, aggregate_type, aggregate_id, user_id, producer, payload, occurred_at, status, created_at, published_at)
		SELECT gen_random_uuid(), 'session.completed', 'SESSION', gen_random_uuid(), gen_random_uuid(), 'API', '{}', $1, 'PUBLISHED', $1, $1
		FROM generate_series(1, 5001)`, now.Add(-10*day))
	require.NoError(t, err)

	before := now.Add(-7 * day)
	n, err := repo.DeleteOutboxPublished(ctx, before, 5000)
	require.NoError(t, err)
	assert.Equal(t, 5000, n, "first batch is capped")
	n, err = repo.DeleteOutboxPublished(ctx, before, 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "second batch takes the remainder")
	n, err = repo.DeleteOutboxPublished(ctx, before, 5000)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestOutboxRetention_ProcessedEventsBatchCap(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewRetentionRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := pool.Exec(ctx, `
		INSERT INTO processed_events (consumer, event_id, event_type, processed_at)
		SELECT 'memory.session_completed', gen_random_uuid(), 'session.completed', $1 FROM generate_series(1, 5001)`, now.Add(-40*day))
	require.NoError(t, err)
	n, err := repo.DeleteProcessedEvents(ctx, now.Add(-30*day), 5000)
	require.NoError(t, err)
	assert.Equal(t, 5000, n)
	n, err = repo.DeleteProcessedEvents(ctx, now.Add(-30*day), 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}
