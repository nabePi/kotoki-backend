//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func pgPartStore(pool *pgxpool.Pool) *jobs.PgPartitionStore {
	return jobs.NewPgPartitionStore(postgres.NewPartitionRepo(pool))
}

func partNames(t *testing.T, s jobs.PartitionStore, table string) []string {
	t.Helper()
	ps, err := s.List(context.Background(), table)
	require.NoError(t, err)
	return names(ps)
}

func TestPgPartitionStore_List_ParsesUTCBoundsInAnySessionTimeZone(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	require.NoError(t, pgPartStore(pool).Ensure(ctx, "session_turns", utc(2024, time.March, 20, 5, 0)))

	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Jakarta"
	jakarta, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(jakarta.Close)

	for name, p := range map[string]*pgxpool.Pool{"utc": pool, "jakarta": jakarta} {
		ps, err := pgPartStore(p).List(ctx, "session_turns")
		require.NoError(t, err, name)
		var got *jobs.Partition
		for i := range ps {
			if ps[i].Name == "session_turns_p202403" {
				got = &ps[i]
			}
		}
		require.NotNil(t, got, name)
		assert.True(t, got.Lower.Equal(utc(2024, time.March, 1, 0, 0)), "%s: lower %s", name, got.Lower)
		assert.True(t, got.Upper.Equal(utc(2024, time.April, 1, 0, 0)), "%s: upper %s", name, got.Upper)
	}
	// Ensure under a non-UTC session still creates the UTC month.
	require.NoError(t, pgPartStore(jakarta).Ensure(ctx, "session_events", utc(2024, time.May, 1, 0, 0)))
	assert.Contains(t, partNames(t, pgPartStore(pool), "session_events"), "session_events_p202405")
	ps, err := pgPartStore(jakarta).List(ctx, "session_events")
	require.NoError(t, err)
	for _, p := range ps {
		if p.Name == "session_events_p202405" {
			assert.True(t, p.Lower.Equal(utc(2024, time.May, 1, 0, 0)))
			assert.True(t, p.Upper.Equal(utc(2024, time.June, 1, 0, 0)))
		}
	}
}

// The job against a real database: expired old months go, the migration's current..+3 partitions
// stay, and a run later in time creates what is missing.
func TestPartitionMaintenanceJob_RealDatabase(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	store := pgPartStore(pool)
	now := time.Now().UTC()
	for _, table := range []string{"session_turns", "session_events"} {
		for _, m := range []time.Month{time.January, time.February, time.March} {
			require.NoError(t, store.Ensure(ctx, table, utc(2024, m, 1, 0, 0)))
		}
	}
	m := jobs.NewMetrics(prometheus.NewRegistry())
	clk := clock.NewFake(now)
	job := jobs.NewPartitionMaintenanceJob(store, jobs.PartitionConfig{RetentionTranscriptDays: 180, RetentionSessionEventsDays: 365}, m, clk, nil)
	require.NoError(t, job.Run(ctx))

	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, table := range []string{"session_turns", "session_events"} {
		var want []string
		for i := 0; i <= 3; i++ {
			want = append(want, jobs.PartitionName(table, first.AddDate(0, i, 0)))
		}
		assert.Equal(t, want, partNames(t, store, table), table+": only current..+3 remain")
		assert.Equal(t, 3.0, promtest.ToFloat64(m.PartitionMonthsAhead.WithLabelValues(table)), table)
		assert.Equal(t, 3.0, promtest.ToFloat64(m.PartitionsDropped.WithLabelValues(table)), table)
		assert.Zero(t, promtest.ToFloat64(m.PartitionsCreated.WithLabelValues(table)), table+": nothing was missing")
	}

	// Idempotent.
	require.NoError(t, job.Run(ctx))

	// Time moves on by 40 days: new partitions appear, the (now older) current ones are kept
	// until retention says otherwise.
	clk.Advance(40 * 24 * time.Hour)
	require.NoError(t, job.Run(ctx))
	later := clk.Now()
	for _, table := range []string{"session_turns", "session_events"} {
		got := partNames(t, store, table)
		assert.Contains(t, got, jobs.PartitionName(table, first), table+": still inside retention")
		for _, month := range jobs.PlanCreate(later) {
			assert.Contains(t, got, jobs.PartitionName(table, month), table)
		}
	}
}

// Even with a retention of 1 day, the partition of the running month and the one before it
// (upper == start of this month) survive, rows included.
func TestPartitionMaintenanceJob_RealDatabase_NeverDropsRunningMonth(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	store := pgPartStore(pool)
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.Ensure(ctx, "session_turns", first.AddDate(0, -1, 0)))

	job := jobs.NewPartitionMaintenanceJob(store, jobs.PartitionConfig{RetentionTranscriptDays: 1, RetentionSessionEventsDays: 1},
		jobs.NewMetrics(prometheus.NewRegistry()), clock.NewFake(now.Add(3*time.Hour)), nil)
	require.NoError(t, job.Run(ctx))

	got := partNames(t, store, "session_turns")
	assert.Contains(t, got, jobs.PartitionName("session_turns", first), "the running month")
	assert.Contains(t, got, jobs.PartitionName("session_turns", first.AddDate(0, -1, 0)), "upper == start of this month")
}

// A DEFAULT partition (never created by us) makes the bounds unreadable: nothing is dropped or
// reported for that table, but the months ahead are still created, and the run fails loudly.
func TestPartitionMaintenanceJob_RealDatabase_DefaultPartitionBlocksDropOnly(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	store := pgPartStore(pool)
	require.NoError(t, store.Ensure(ctx, "session_turns", utc(2024, time.January, 1, 0, 0)))
	_, err := pool.Exec(ctx, `CREATE TABLE session_turns_default PARTITION OF session_turns DEFAULT`)
	require.NoError(t, err)

	now := time.Now().UTC()
	job := jobs.NewPartitionMaintenanceJob(store, jobs.PartitionConfig{RetentionTranscriptDays: 180, RetentionSessionEventsDays: 365},
		jobs.NewMetrics(prometheus.NewRegistry()), clock.NewFake(now.AddDate(0, 6, 0)), nil)
	require.Error(t, job.Run(ctx))

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'session_turns_p202401'`).Scan(&n))
	assert.Equal(t, 1, n, "the old partition was not dropped without a readable listing")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = $1`,
		jobs.PartitionName("session_turns", time.Date(now.Year(), now.Month()+6, 1, 0, 0, 0, 0, time.UTC))).Scan(&n))
	assert.Equal(t, 1, n, "creation went on")
}
