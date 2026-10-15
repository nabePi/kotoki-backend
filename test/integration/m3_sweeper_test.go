//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// TestSweeperRuleBWithRealPostgresAndRedis: the runtime "dies" (its heartbeat stops), and rule (b)
// settles the session with the last heartbeat as the end; the books balance and exactly one
// session.completed is queued.
func TestSweeperRuleBWithRealPostgresAndRedis(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	e := newSessEnvClock(t, 1800, clk, nil)
	rdb := testutil.NewRedis(t)
	ctx := context.Background()
	s, fence := e.activate(t, "sweeper-key-0001", 900)

	// the owner beat until 10:05:00 and then died
	last := time.Date(2026, 10, 14, 10, 5, 0, 0, time.UTC)
	require.NoError(t, rdb.Set(ctx, redisstore.HeartbeatKey(s.ID), strconv.FormatInt(last.UnixMilli(), 10), 0).Err())

	repo := postgres.NewSessionRepo(e.pool)
	w := session.NewSweeper(e.svc, repo, redisstore.NewHeartbeatReader(rdb), clk, slog.Default())
	clk.Set(last.Add(10 * time.Second))
	rep, err := w.Tick(ctx)
	require.NoError(t, err)
	require.True(t, rep.Rule["b"].Skipped, "Redis has only just been seen")
	clk.Set(last.Add(time.Minute))
	rep, err = w.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Rule["b"].OK)

	got, err := repo.GetByID(ctx, s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Equal(t, session.EndServerError, *got.EndReason)
	require.Equal(t, last, got.EndedAt.UTC())
	require.Equal(t, 270, got.ChargedSeconds, "300 s active, minus 30 s for a service fault")
	require.Equal(t, 1, e.countFor(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, s.ID))
	require.Equal(t, 1, e.countFor(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, s.ID))
	var final, producer string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT payload->>'final_status', producer FROM event_outbox WHERE aggregate_id = $1`, s.ID).Scan(&final, &producer))
	require.Equal(t, "FAILED", final)
	require.Equal(t, "WORKER", producer)

	// the dead runtime, if it ever wakes up, is locked out
	err = e.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		return tx.Turns().Insert(ctx, fence, session.TurnStart{ID: uuid.Must(uuid.NewV7()), SessionID: s.ID, Sequence: 1, Generation: 1, CreatedAt: clk.Now()})
	})
	require.ErrorIs(t, err, session.ErrFenceLost)
}

// TestSweeperRulesWithRedisDown: the Postgres-only rules keep working when Redis is unreachable.
func TestSweeperRulesWithRedisDown(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	e := newSessEnvClock(t, 1800, clk, nil)
	rdb := testutil.NewRedis(t)
	ctx := context.Background()
	s, _ := e.activate(t, "sweeper-key-0002", 300)
	require.NoError(t, rdb.Close()) // Redis is gone

	w := session.NewSweeper(e.svc, postgres.NewSessionRepo(e.pool), redisstore.NewHeartbeatReader(rdb), clk, slog.Default())
	clk.Set(time.Date(2026, 10, 14, 10, 5, 16, 0, time.UTC))
	rep, err := w.Tick(ctx)
	require.NoError(t, err)
	require.True(t, rep.Rule["b"].Skipped)
	require.Equal(t, 1, rep.Rule["a"].OK)
	var status string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, s.ID).Scan(&status))
	require.Equal(t, "COMPLETED", status)
}
