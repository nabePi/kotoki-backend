//go:build integration

package jobs_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/testutil/sessionenv"
)

func sweeperJob(e *sessionenv.Env, m *jobs.Metrics, interval time.Duration) *jobs.SessionSweeperJob {
	return jobs.NewSessionSweeperJob(func() jobs.SessionTicker { return e.Sweeper }, m, slog.New(slog.DiscardHandler), e.Clock, interval)
}

// Two workers whose advisory lock failed over run the sweeper at once: the session is still closed
// once, billed once and announced once. The clock is fake and moved by 5 minutes so the charge is
// not 0 (a sub-millisecond active period would write no CONSUME row at all).
func TestSessionSweeperJob_ExpiredSession_SettledExactlyOnceByConcurrentRuns(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	id := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 300})
	e.Clock.Advance(300*time.Second + 16*time.Second)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, sweeperJob(e, m, 10*time.Second).Run(ctx))
		}()
	}
	wg.Wait()
	require.NoError(t, sweeperJob(e, m, 10*time.Second).Run(ctx))

	var status string
	var charged int
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT status, charged_seconds FROM sessions WHERE id = $1`, id).Scan(&status, &charged))
	require.Equal(t, "COMPLETED", status)
	require.Equal(t, 300, charged)
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, id))
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, id))
	require.GreaterOrEqual(t, promtest.ToFloat64(m.SweeperActions.WithLabelValues("a", "ok")), 1.0)
}

// A Redis flush wipes every heartbeat at once. Healthy sessions must stay ACTIVE, the skip must
// show in the metric and the heartbeat rule must stay quiet (review focus 2).
func TestSessionSweeperJob_RedisFlush_DoesNotFailHealthySessions(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := sweeperJob(e, m, time.Minute)

	var all []uuid.UUID
	for range 25 {
		all = append(all, e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900, Beat: true}))
	}
	e.Clock.Advance(time.Minute)
	for _, id := range all {
		e.Beat(t, id)
	}
	require.NoError(t, job.Run(ctx)) // the guard sees Redis for the first time
	require.Equal(t, 1.0, promtest.ToFloat64(m.HeartbeatChecksSuspended))

	e.Clock.Advance(50 * time.Second)
	require.NoError(t, e.Redis.FlushAll(ctx).Err())
	require.NoError(t, job.Run(ctx))

	require.Equal(t, 0.0, promtest.ToFloat64(m.HeartbeatChecksSuspended))
	require.Equal(t, 1.0, promtest.ToFloat64(m.MassHeartbeatLoss))
	require.Equal(t, 0.0, promtest.ToFloat64(m.SweeperActions.WithLabelValues("b", "ok")))
	require.Equal(t, 25, e.Count(t, `SELECT count(*) FROM sessions WHERE status = 'ACTIVE'`))
	require.Equal(t, 0, e.Count(t, `SELECT count(*) FROM event_outbox`))
}

// A real crash: one session stops beating while another keeps going. Only the dead one is failed,
// billed up to its last heartbeat, once.
func TestSessionSweeperJob_DeadOwner_FailedOnceAtLastHeartbeat(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := sweeperJob(e, m, time.Minute)

	dead := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900, Beat: true})
	alive := e.SeedActive(t, sessionenv.ActiveOpts{DurationSeconds: 900, Beat: true})
	e.Clock.Advance(time.Minute)
	e.Beat(t, dead) // the last sign of life: 60 s into the session
	e.Beat(t, alive)
	e.Clock.Advance(2 * time.Minute)
	e.Beat(t, alive)
	require.NoError(t, job.Run(ctx)) // the guard starts
	e.Clock.Advance(50 * time.Second)
	e.Beat(t, alive)
	require.NoError(t, job.Run(ctx))
	require.NoError(t, job.Run(ctx))

	var status string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, dead).Scan(&status))
	require.Equal(t, "FAILED", status)
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, alive).Scan(&status))
	require.Equal(t, "ACTIVE", status)
	var charged int
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT charged_seconds FROM sessions WHERE id = $1`, dead).Scan(&charged))
	require.Equal(t, 30, charged, "60 s until the last heartbeat, minus 30 s for a service fault")
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM balance_ledger WHERE session_id = $1 AND entry_type = 'CONSUME'`, dead))
	require.Equal(t, 1, e.Count(t, `SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'session.completed'`, dead))
	require.Equal(t, 1.0, promtest.ToFloat64(m.SweeperActions.WithLabelValues("b", "ok")))
}
