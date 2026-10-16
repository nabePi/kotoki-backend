//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func monthUTC(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }

// ensureMonth creates the monthly partition of table for the UTC month containing t.
func ensureMonth(t *testing.T, repo *postgres.PartitionRepo, table string, month time.Time) {
	t.Helper()
	lo := monthUTC(month.Year(), month.Month())
	require.NoError(t, repo.Ensure(context.Background(), table, table+"_p"+lo.Format("200601"), lo, lo.AddDate(0, 1, 0)))
}

// seedSessionRow returns a session id owned by a fresh user, for rows of partitioned child tables.
func seedSessionRow(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	uid := testutil.SeedUser(t, pool, nil)
	return testutil.SeedSession(t, pool, uid, testutil.SeedContent(t, pool), nil)
}

func insertTurnAt(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID, seq int, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	testutil.MustInsert(context.Background(), t, pool, "session_turns", testutil.Row{
		"id": id, "session_id": sessionID, "sequence_number": seq, "generation": 1, "created_at": createdAt,
		"user_text": "こんにちは",
	})
	return id
}

func insertEventAt(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID, createdAt time.Time) {
	t.Helper()
	testutil.MustInsert(context.Background(), t, pool, "session_events", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "session_id": sessionID, "event_type": "BARGE_IN", "actor": "REALTIME", "created_at": createdAt,
	})
}

func regclass(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok))
	return ok
}

func partitionNames(t *testing.T, repo *postgres.PartitionRepo, table string) []string {
	t.Helper()
	ps, err := repo.List(context.Background(), table)
	require.NoError(t, err)
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestPartitionRepo_Ensure_Idempotent(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	for _, table := range []string{"session_turns", "session_events"} {
		ensureMonth(t, repo, table, monthUTC(2024, time.March))
		ensureMonth(t, repo, table, monthUTC(2024, time.March)) // second call: no error, no duplicate
		names := partitionNames(t, repo, table)
		n := 0
		for _, name := range names {
			if name == table+"_p202403" {
				n++
			}
		}
		assert.Equal(t, 1, n, table)
	}
}

func TestPartitionRepo_Ensure_RejectsUnsafeOrInconsistentInput(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	lo := monthUTC(2024, time.March)
	up := lo.AddDate(0, 1, 0)
	before := len(partitionNames(t, repo, "session_turns"))

	cases := []struct {
		name         string
		table, pname string
		lo, up       time.Time
	}{
		{"table outside the allow list", "users", "users_p202403", lo, up},
		{"injection through the table", `session_turns; DROP TABLE users; --`, "session_turns_p202403", lo, up},
		{"injection through the name", "session_turns", `session_turns_p202403"; DROP TABLE users; --`, lo, up},
		{"name of the other table", "session_turns", "session_events_p202403", lo, up},
		{"name month differs from the bounds", "session_turns", "session_turns_p202404", lo, up},
		{"lower not a month start", "session_turns", "session_turns_p202403", lo.Add(time.Hour), up},
		{"upper not the next month", "session_turns", "session_turns_p202403", lo, lo.AddDate(0, 2, 0)},
		{"short name", "session_turns", "session_turns_p2024", lo, up},
	}
	for _, c := range cases {
		require.Error(t, repo.Ensure(ctx, c.table, c.pname, c.lo, c.up), c.name)
	}
	assert.Equal(t, before, len(partitionNames(t, repo, "session_turns")))
	assert.True(t, regclass(t, pool, "users"), "users must still exist")
}

func TestPartitionRepo_Drop_RejectsUnsafeInput(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	require.Error(t, repo.Drop(ctx, "users", "users", false))
	require.Error(t, repo.Drop(ctx, "session_turns", "sessions", false))
	require.Error(t, repo.Drop(ctx, "session_turns", `session_turns_p202403"; DROP TABLE users; --`, false))
	require.Error(t, repo.Drop(ctx, "session_turns", "session_events_p202403", false))
	require.Error(t, repo.Drop(ctx, "session_turns", "session_turns_pxxxxxx", false))

	// A real partition whose name is not `{table}_pYYYYMM` is not ours to drop, however valid it is.
	_, err := pool.Exec(ctx, `CREATE TABLE session_turns_legacy PARTITION OF session_turns
FOR VALUES FROM ('2001-01-01 00:00:00+00') TO ('2001-02-01 00:00:00+00')`)
	require.NoError(t, err)
	require.Error(t, repo.Drop(ctx, "session_turns", "session_turns_legacy", false))
	assert.True(t, regclass(t, pool, "session_turns_legacy"))
	_, err = pool.Exec(ctx, `ALTER TABLE session_turns DETACH PARTITION session_turns_legacy`)
	require.NoError(t, err)
	assert.True(t, regclass(t, pool, "users"))
	assert.True(t, regclass(t, pool, "sessions"))
	_, err = repo.List(ctx, "sessions")
	require.Error(t, err, "List is limited to the managed tables as well")
}

func TestPartitionRepo_List_ReturnsBoundsFromPgInherits(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	ensureMonth(t, repo, "session_turns", monthUTC(2024, time.March))

	ps, err := repo.List(ctx, "session_turns")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ps), 5, "4 from the migration + the 2024-03 partition")
	var found *postgres.PartitionInfo
	for i := 1; i < len(ps); i++ {
		assert.Less(t, ps[i-1].Name, ps[i].Name, "ordered by name, which is chronological")
	}
	for i, p := range ps {
		assert.True(t, strings.HasPrefix(p.Bound, "FOR VALUES FROM ("), p.Bound)
		assert.False(t, p.DetachPending, p.Name)
		if p.Name == "session_turns_p202403" {
			found = &ps[i]
		}
	}
	require.NotNil(t, found)
	assert.Contains(t, found.Bound, "2024-03-01 00:00:00")
	assert.Contains(t, found.Bound, "2024-04-01 00:00:00")

	// Only the asked table's partitions come back.
	for _, p := range ps {
		assert.True(t, strings.HasPrefix(p.Name, "session_turns_p"), p.Name)
	}
}

func TestPartitionRepo_NoDefaultPartition(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ensureMonth(t, repo, "session_turns", monthUTC(2024, time.March))
	ensureMonth(t, repo, "session_events", monthUTC(2024, time.March))
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_class c
WHERE c.relispartition AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&n))
	assert.Zero(t, n, "no DEFAULT partition: a row in a month without a partition must fail")
	sid := seedSessionRow(t, pool)
	err := testutil.TryInsert(context.Background(), pool, "session_turns", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": 1, "generation": 1,
		"created_at": monthUTC(2001, time.January),
	})
	require.Error(t, err)
	testutil.RequirePgErr(t, err, "23514", "") // no partition of relation found for row
}

func TestPartitionRepo_InsertAcrossTwoPartitions(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	ensureMonth(t, repo, "session_turns", monthUTC(2024, time.January))
	ensureMonth(t, repo, "session_turns", monthUTC(2024, time.February))
	sid := seedSessionRow(t, pool)
	a := insertTurnAt(t, pool, sid, 1, time.Date(2024, 1, 31, 23, 59, 59, 0, time.UTC))
	b := insertTurnAt(t, pool, sid, 2, time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC))

	var pa, pb string
	require.NoError(t, pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM session_turns WHERE id = $1`, a).Scan(&pa))
	require.NoError(t, pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM session_turns WHERE id = $1`, b).Scan(&pb))
	assert.Equal(t, "session_turns_p202401", pa)
	assert.Equal(t, "session_turns_p202402", pb, "lower bound is inclusive, in UTC")
}

func TestPartitionRepo_Drop_RemovesPartitionAndItsRows(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	ensureMonth(t, repo, "session_turns", monthUTC(2024, time.January))
	ensureMonth(t, repo, "session_turns", monthUTC(2024, time.February))
	sid := seedSessionRow(t, pool)
	insertTurnAt(t, pool, sid, 1, time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC))
	keep := insertTurnAt(t, pool, sid, 2, time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC))

	require.NoError(t, repo.Drop(ctx, "session_turns", "session_turns_p202401", false))
	assert.False(t, regclass(t, pool, "session_turns_p202401"))
	assert.NotContains(t, partitionNames(t, repo, "session_turns"), "session_turns_p202401")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM session_turns WHERE id = $1`, keep).Scan(&n))
	assert.Equal(t, 1, n, "the neighbouring partition is untouched")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM session_turns WHERE created_at < '2024-02-01'`).Scan(&n))
	assert.Zero(t, n)
	// Dropping a partition that is already gone is an error, not a silent success.
	require.Error(t, repo.Drop(ctx, "session_turns", "session_turns_p202401", false))
}

// An interrupted DETACH ... CONCURRENTLY leaves the partition "detach pending"; List reports it and
// Drop finishes it with FINALIZE before dropping.
func TestPartitionRepo_Drop_DetachPendingIsFinalized(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	ensureMonth(t, repo, "session_events", monthUTC(2024, time.January))
	sid := seedSessionRow(t, pool)
	insertEventAt(t, pool, sid, time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC))

	// A transaction that has read the parent keeps DETACH CONCURRENTLY waiting in its 2nd phase.
	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(ctx, `SELECT count(*) FROM session_events`).Scan(&n))

	done := make(chan error, 1)
	go func() { done <- repo.Drop(ctx, "session_events", "session_events_p202401", false) }()
	// Wait until the detach is parked behind the blocker.
	require.Eventually(t, func() bool {
		var waiting int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE query ILIKE 'ALTER TABLE %DETACH PARTITION%' AND wait_event_type IS NOT NULL AND wait_event_type <> 'Client'`).Scan(&waiting))
		return waiting > 0
	}, 10*time.Second, 50*time.Millisecond)
	// Cancel it on the server (as an operator or a timeout would): the first transaction of the
	// detach has committed, so the partition stays "detach pending".
	var cancelled int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(pg_cancel_backend(pid)) FROM pg_stat_activity
WHERE query ILIKE 'ALTER TABLE %DETACH PARTITION%' AND pid <> pg_backend_pid()`).Scan(&cancelled))
	require.Equal(t, 1, cancelled)
	require.Error(t, <-done, "the cancelled detach reports an error")
	require.NoError(t, blocker.Rollback(ctx))

	ps, err := repo.List(ctx, "session_events")
	require.NoError(t, err)
	var pending bool
	for _, p := range ps {
		if p.Name == "session_events_p202401" {
			pending = p.DetachPending
		}
	}
	require.True(t, pending, "the cancelled detach left the partition detach-pending")

	require.NoError(t, repo.Drop(ctx, "session_events", "session_events_p202401", true))
	assert.False(t, regclass(t, pool, "session_events_p202401"))
	assert.NotContains(t, partitionNames(t, repo, "session_events"), "session_events_p202401")
}

// A busy parent table must fail the creation (lock_timeout 5 s) instead of queueing an
// ACCESS EXCLUSIVE request that blocks every insert behind it.
func TestPartitionRepo_Ensure_LockTimeout(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewPartitionRepo(pool)
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = holder.Exec(ctx, `LOCK TABLE session_turns IN SHARE MODE`)
	require.NoError(t, err)

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	lo := monthUTC(2024, time.March)
	err = repo.Ensure(cctx, "session_turns", "session_turns_p202403", lo, lo.AddDate(0, 1, 0))
	elapsed := time.Since(start)
	require.NoError(t, holder.Rollback(ctx))

	require.Error(t, err)
	assert.NoError(t, cctx.Err(), "failed by lock_timeout, not by the caller's deadline")
	assert.Contains(t, err.Error(), "partition create failed")
	assert.GreaterOrEqual(t, elapsed, 4*time.Second)
	assert.Less(t, elapsed, 10*time.Second)
	assert.NotContains(t, partitionNames(t, repo, "session_turns"), "session_turns_p202403")
}
