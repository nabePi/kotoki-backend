package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type fakeTicker struct {
	mu    sync.Mutex
	rep   session.SweepReport
	err   error
	ticks int
}

func (f *fakeTicker) Tick(context.Context) (session.SweepReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ticks++
	return f.rep, f.err
}

type sweeperRig struct {
	m    *jobs.Metrics
	logs *bytes.Buffer
	clk  *clock.Fake
	tk   *fakeTicker
	job  *jobs.SessionSweeperJob
	made int
}

func newSweeperRig(t *testing.T, rep session.SweepReport, interval time.Duration) *sweeperRig {
	t.Helper()
	r := &sweeperRig{
		m: jobs.NewMetrics(prometheus.NewRegistry()), logs: &bytes.Buffer{},
		clk: clock.NewFake(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)), tk: &fakeTicker{rep: rep},
	}
	log := slog.New(slog.NewJSONHandler(r.logs, nil))
	r.job = jobs.NewSessionSweeperJob(func() jobs.SessionTicker { r.made++; return r.tk }, r.m, log, r.clk, interval)
	return r
}

func TestSessionSweeperJob_MapsReportToMetrics(t *testing.T) {
	r := newSweeperRig(t, session.SweepReport{Rule: map[string]session.RuleResult{
		"b": {OK: 2, Noop: 1, Error: 1},
		"a": {OK: 3},
	}}, 10*time.Second)
	require.NoError(t, r.job.Run(context.Background()))

	for rule, want := range map[string]map[string]float64{"b": {"ok": 2, "noop": 1, "error": 1}, "a": {"ok": 3, "noop": 0, "error": 0}} {
		for result, n := range want {
			assert.Equal(t, n, promtest.ToFloat64(r.m.SweeperActions.WithLabelValues(rule, result)), "rule %s %s", rule, result)
		}
	}
	assert.Equal(t, 2.0, promtest.ToFloat64(r.m.JobItems.WithLabelValues("session_sweeper", "rule_b_ok")))
	assert.Equal(t, 1.0, promtest.ToFloat64(r.m.JobItems.WithLabelValues("session_sweeper", "rule_b_noop")))
	assert.Equal(t, 1.0, promtest.ToFloat64(r.m.JobItems.WithLabelValues("session_sweeper", "rule_b_error")))
	assert.Equal(t, 3.0, promtest.ToFloat64(r.m.JobItems.WithLabelValues("session_sweeper", "rule_a_ok")))

	// counters accumulate across runs
	require.NoError(t, r.job.Run(context.Background()))
	assert.Equal(t, 4.0, promtest.ToFloat64(r.m.SweeperActions.WithLabelValues("b", "ok")))
}

func TestSessionSweeperJob_HeartbeatSuspendedGauge(t *testing.T) {
	r := newSweeperRig(t, session.SweepReport{HeartbeatSuspended: true}, 10*time.Second)
	require.NoError(t, r.job.Run(context.Background()))
	assert.Equal(t, 1.0, promtest.ToFloat64(r.m.HeartbeatChecksSuspended))

	r.tk.mu.Lock()
	r.tk.rep = session.SweepReport{}
	r.tk.mu.Unlock()
	require.NoError(t, r.job.Run(context.Background()))
	assert.Equal(t, 0.0, promtest.ToFloat64(r.m.HeartbeatChecksSuspended))
}

func TestSessionSweeperJob_MassLoss_LogsErrorAndCounts(t *testing.T) {
	r := newSweeperRig(t, session.SweepReport{MassHeartbeatLoss: true, HeartbeatCandidates: 30, ActiveNonHandoff: 40}, 10*time.Second)
	require.NoError(t, r.job.Run(context.Background()))
	assert.Equal(t, 1.0, promtest.ToFloat64(r.m.MassHeartbeatLoss))
	out := r.logs.String()
	assert.Contains(t, out, `"level":"ERROR"`)
	assert.Contains(t, out, "sweeper.mass_heartbeat_loss")
	assert.Contains(t, out, `"candidates":30`)
	assert.Contains(t, out, `"active":40`)

	// a tick without mass loss neither counts nor logs again
	r.tk.mu.Lock()
	r.tk.rep = session.SweepReport{}
	r.tk.mu.Unlock()
	r.logs.Reset()
	require.NoError(t, r.job.Run(context.Background()))
	assert.Equal(t, 1.0, promtest.ToFloat64(r.m.MassHeartbeatLoss))
	assert.NotContains(t, r.logs.String(), "sweeper.mass_heartbeat_loss")
}

func TestSessionSweeperJob_TickErrorReturned(t *testing.T) {
	boom := errors.New("boom")
	r := newSweeperRig(t, session.SweepReport{Rule: map[string]session.RuleResult{"e": {OK: 1}}}, 10*time.Second)
	r.tk.err = boom
	err := r.job.Run(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1.0, promtest.ToFloat64(r.m.SweeperActions.WithLabelValues("e", "ok")), "what the tick did before failing is still counted")
}

func TestSessionSweeperJob_NewSweeperAfterLockGap(t *testing.T) {
	r := newSweeperRig(t, session.SweepReport{}, 10*time.Second)
	ctx := context.Background()
	require.NoError(t, r.job.Run(ctx))
	require.Equal(t, 1, r.made)

	r.clk.Advance(10 * time.Second)
	require.NoError(t, r.job.Run(ctx))
	r.clk.Advance(20 * time.Second) // exactly 2x interval: still the same sweeper
	require.NoError(t, r.job.Run(ctx))
	require.Equal(t, 1, r.made, "a gap of at most 2x the interval keeps the instance and its heartbeat guard")

	r.clk.Advance(20*time.Second + time.Millisecond)
	require.NoError(t, r.job.Run(ctx))
	require.Equal(t, 2, r.made, "another instance may have held the lock: start the guard from zero")

	r.clk.Advance(time.Second)
	require.NoError(t, r.job.Run(ctx))
	require.Equal(t, 2, r.made)
}
