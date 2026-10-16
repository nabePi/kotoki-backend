//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var _ jobs.OutboxRetentionStore = (*postgres.RetentionRepo)(nil)

// The job against a real database: 5.001 old PUBLISHED rows take two batches, while younger rows,
// PENDING rows and rows of the other statuses stay.
func TestOutboxRetentionJob_RealDatabase(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const d = 24 * time.Hour
	_, err := pool.Exec(ctx, `
		INSERT INTO event_outbox (event_id, event_type, aggregate_type, aggregate_id, user_id, producer, payload, occurred_at, status, created_at, published_at)
		SELECT gen_random_uuid(), 'session.completed', 'SESSION', gen_random_uuid(), gen_random_uuid(), 'API', '{}', $1, 'PUBLISHED', $1, $1
		FROM generate_series(1, 5001)`, now.Add(-8*d))
	require.NoError(t, err)
	keepID := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, pool, "event_outbox", testutil.Row{
		"event_id": keepID, "event_type": "session.completed", "aggregate_type": "SESSION", "aggregate_id": uuid.Must(uuid.NewV7()),
		"user_id": uuid.Must(uuid.NewV7()), "producer": "API", "payload": `{}`, "occurred_at": now, "status": "PENDING", "created_at": now.Add(-90 * d),
	})

	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := jobs.NewOutboxRetentionJob(postgres.NewRetentionRepo(pool), jobs.OutboxRetentionConfig{PublishedDays: 7, ProcessedEventsDays: 30}, m, clock.NewFake(now), nil)
	require.NoError(t, job.Run(ctx))

	var left int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_outbox`).Scan(&left))
	assert.Equal(t, 1, left, "only the PENDING row is left")
	assert.Equal(t, 5001.0, promtest.ToFloat64(m.JobItems.WithLabelValues("outbox_retention", "outbox_published")))
}
