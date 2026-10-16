//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/sessionenv"
)

func reconcileJob(e *sessionenv.Env) (*jobs.BalanceReconcileJob, *jobs.Metrics) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	return jobs.NewBalanceReconcileJob(jobs.BindReconciler(e.Balance, postgres.NewReconcileRepo(e.Pool)), m, nil), m
}

// finishSession runs one real, billed lifecycle: 60 s of active time on the fake clock, then the
// runtime ends and finalizes it.
func finishSession(t *testing.T, e *sessionenv.Env) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := e.SeedActive(t, sessionenv.ActiveOpts{})
	fence := e.Fence(t, id)
	e.Clock.Advance(time.Minute)
	_, err := e.Sessions.BeginEnding(ctx, id, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.Sessions.Finalize(ctx, id, &fence, session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, id))
	return id
}

func TestBalanceReconcile_CleanDB_AllZero(t *testing.T) {
	e := sessionenv.New(t)
	finishSession(t, e)
	e.SeedActive(t, sessionenv.ActiveOpts{}) // a HELD reservation on an ACTIVE session is consistent too
	job, m := reconcileJob(e)
	require.NoError(t, job.Run(context.Background()))
	for _, c := range []string{"R1", "R2", "R3", "R4", "R5", "R6"} {
		require.Equal(t, 0.0, violationGauge(m, c), c)
	}
}

func TestBalanceReconcile_DetectsTamper(t *testing.T) {
	e := sessionenv.New(t, sessionenv.SkipFinalReconcile()) // the books are broken on purpose
	ctx := context.Background()
	id := finishSession(t, e)
	var uid uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, id).Scan(&uid))
	available := func() int64 {
		var n int64
		require.NoError(t, e.Pool.QueryRow(ctx, `SELECT available_seconds FROM balance_accounts WHERE user_id = $1`, uid).Scan(&n))
		return n
	}
	before := available()
	_, err := e.Pool.Exec(ctx, `UPDATE balance_accounts SET available_seconds = available_seconds + 1 WHERE user_id = $1`, uid)
	require.NoError(t, err)

	job, m := reconcileJob(e)
	require.NoError(t, job.Run(ctx))
	require.Greater(t, violationGauge(m, "R1"), 0.0)
	require.Greater(t, violationGauge(m, "R3"), 0.0)
	require.Equal(t, 0.0, violationGauge(m, "R2"))
	require.Equal(t, before+1, available(), "the job only reports, it never corrects")
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE user_id = $1 AND entry_type = 'CONSUME'`, uid))
}

func TestBalanceReconcile_R6_StuckReservation(t *testing.T) {
	e := sessionenv.New(t, sessionenv.SkipFinalReconcile())
	ctx := context.Background()
	id := e.SeedReserved(t, sessionenv.ActiveOpts{})
	// R6 compares with the database clock, the harness runs on a fake one: pin the deadline to now()
	_, err := e.Pool.Exec(ctx, `UPDATE balance_reservations SET start_deadline_at = now() + interval '5 minutes' WHERE session_id = $1`, id)
	require.NoError(t, err)
	job, m := reconcileJob(e)
	require.NoError(t, job.Run(ctx))
	require.Equal(t, 0.0, violationGauge(m, "R6"), "a fresh reservation is not stuck")

	_, err = e.Pool.Exec(ctx, `UPDATE balance_reservations SET start_deadline_at = now() - interval '11 minutes' WHERE session_id = $1`, id)
	require.NoError(t, err)
	require.NoError(t, job.Run(ctx))
	require.Equal(t, 1.0, violationGauge(m, "R6"))
	for _, c := range []string{"R1", "R2", "R3", "R4", "R5"} {
		require.Equal(t, 0.0, violationGauge(m, c), c)
	}

	// once reservation_cleanup abandons it the finding goes away (the deadline is in real time)
	e.Clock.Set(time.Now().UTC())
	_, err = e.Sessions.Abandon(ctx, id, session.ActorCleanup)
	require.NoError(t, err)
	require.NoError(t, job.Run(ctx))
	require.Equal(t, 0.0, violationGauge(m, "R6"))
}
