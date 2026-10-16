//go:build integration

package jobs_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil/sessionenv"
)

func TestReservationCleanup_ReleasesOnlyStaleReserved(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	job := jobs.NewReservationCleanupJob(postgres.NewSessionRepo(e.Pool), e.Sessions, jobs.NewMetrics(prometheus.NewRegistry()), e.Clock, slog.New(slog.DiscardHandler))

	stale := e.SeedReserved(t, sessionenv.ActiveOpts{})
	active := e.SeedActive(t, sessionenv.ActiveOpts{})
	e.Clock.Advance(10 * time.Minute) // well past the start deadline of `stale`
	fresh := e.SeedReserved(t, sessionenv.ActiveOpts{})

	require.NoError(t, job.Run(ctx))

	var status, reason string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT status, end_reason FROM sessions WHERE id = $1`, stale).Scan(&status, &reason))
	require.Equal(t, "ABANDONED", status)
	require.Equal(t, "NOT_STARTED", reason)
	var resStatus string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT status FROM balance_reservations WHERE session_id = $1`, stale).Scan(&resStatus))
	require.Equal(t, "RELEASED", resStatus)
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger l JOIN balance_reservations r ON r.id = l.reservation_id
		WHERE r.session_id = $1 AND l.entry_type = 'RELEASE' AND l.idempotency_key = 'release:' || r.id::text`, stale))

	for name, id := range map[string]any{"active": active, "fresh reserved": fresh} {
		var st, rs string
		require.NoError(t, e.Pool.QueryRow(ctx, `SELECT s.status, r.status FROM sessions s JOIN balance_reservations r ON r.session_id = s.id WHERE s.id = $1`, id).Scan(&st, &rs))
		require.Equal(t, "HELD", rs, name)
		require.NotEqual(t, "ABANDONED", st, name)
	}
	require.Equal(t, 0, e.Count(t, `SELECT count(*) FROM event_outbox`), "abandoning writes no event")

	// a second run changes nothing
	ledgerBefore := e.Count(t, `SELECT count(*) FROM balance_ledger`)
	require.NoError(t, job.Run(ctx))
	require.Equal(t, ledgerBefore, e.Count(t, `SELECT count(*) FROM balance_ledger`))
}
