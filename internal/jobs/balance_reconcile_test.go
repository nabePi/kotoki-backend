package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/jobs"
)

type fakeReconciler struct {
	v   []balance.Violation
	err error
}

func (f fakeReconciler) Reconcile(context.Context) ([]balance.Violation, error) { return f.v, f.err }

func newReconcileJob(r jobs.Reconciler) (*jobs.BalanceReconcileJob, *jobs.Metrics, *bytes.Buffer) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	logs := &bytes.Buffer{}
	return jobs.NewBalanceReconcileJob(r, m, slog.New(slog.NewJSONHandler(logs, nil))), m, logs
}

func violationGauge(m *jobs.Metrics, check string) float64 {
	return promtest.ToFloat64(m.BalanceInvariantViolations.WithLabelValues(check))
}

func TestBalanceReconcile_SetsGaugePerCheck(t *testing.T) {
	job, m, logs := newReconcileJob(fakeReconciler{v: []balance.Violation{{Check: "R3", Rows: []string{"a", "b"}}}})
	require.NoError(t, job.Run(context.Background()))

	assert.Equal(t, 2.0, violationGauge(m, "R3"))
	for _, c := range []string{"R1", "R2", "R4", "R5", "R6"} {
		assert.Equal(t, 0.0, violationGauge(m, c), c)
	}
	out := logs.String()
	assert.Contains(t, out, `"level":"ERROR"`)
	assert.Contains(t, out, "balance.reconcile.mismatch")
	assert.Contains(t, out, `"check":"R3"`)
}

func TestBalanceReconcile_ClearsGaugeOnceFixed(t *testing.T) {
	r := &mutableReconciler{v: []balance.Violation{{Check: "R1", Rows: []string{"x"}}}}
	job, m, _ := newReconcileJob(r)
	require.NoError(t, job.Run(context.Background()))
	require.Equal(t, 1.0, violationGauge(m, "R1"))
	r.v = nil
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, 0.0, violationGauge(m, "R1"), "the gauge is set on every run, also back to 0")
}

type mutableReconciler struct{ v []balance.Violation }

func (m *mutableReconciler) Reconcile(context.Context) ([]balance.Violation, error) { return m.v, nil }

func TestBalanceReconcile_LogsAtMost20Ids(t *testing.T) {
	var rows []string
	for i := range 35 {
		rows = append(rows, fmt.Sprintf("id-%02d", i))
	}
	job, m, logs := newReconcileJob(fakeReconciler{v: []balance.Violation{{Check: "R1", Rows: rows}}})
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, 35.0, violationGauge(m, "R1"), "the gauge counts every row, the log only shows 20")
	out := logs.String()
	assert.Contains(t, out, "id-19")
	assert.NotContains(t, out, "id-20")
	assert.Equal(t, 1, strings.Count(out, "balance.reconcile.mismatch"), "one log line per violated check")
}

func TestBalanceReconcile_ErrorReturnedAfterGaugesSet(t *testing.T) {
	boom := errors.New("R2 failed")
	job, m, _ := newReconcileJob(fakeReconciler{v: []balance.Violation{{Check: "R4", Rows: []string{"a"}}}, err: boom})
	err := job.Run(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1.0, violationGauge(m, "R4"), "what the other checks found is still published")
	assert.Equal(t, 0.0, violationGauge(m, "R1"))
}

func TestBalanceReconcile_CleanRun_NoErrorLog(t *testing.T) {
	job, m, logs := newReconcileJob(fakeReconciler{})
	require.NoError(t, job.Run(context.Background()))
	assert.Empty(t, logs.String())
	for _, c := range []string{"R1", "R2", "R3", "R4", "R5", "R6"} {
		assert.Equal(t, 0.0, violationGauge(m, c), c)
	}
}

func TestBalanceReconcile_ErrorKeepsGaugeOfCheckThatDidNotReport(t *testing.T) {
	r := &mutableReconciler{v: []balance.Violation{{Check: "R2", Rows: []string{"a", "b", "c"}}}}
	job, m, _ := newReconcileJob(r)
	require.NoError(t, job.Run(context.Background()))
	require.Equal(t, 3.0, violationGauge(m, "R2"))

	failing := jobs.NewBalanceReconcileJob(fakeReconciler{err: errors.New("R2 timed out")}, m, nil)
	require.Error(t, failing.Run(context.Background()))
	assert.Equal(t, 3.0, violationGauge(m, "R2"), "a check that could not run must not clear its last finding")
}

func TestBindReconciler_CallsServiceWithRepository(t *testing.T) {
	repo := &recordingChecks{}
	rec := jobs.BindReconciler(balance.NewService(nil, nil, nil), repo)
	v, err := rec.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Empty(t, v)
	assert.Equal(t, []string{"R1", "R2", "R3", "R4", "R5", "R6"}, repo.calls)
}

type recordingChecks struct{ calls []string }

func (r *recordingChecks) RunCheck(_ context.Context, c string) ([]string, error) {
	r.calls = append(r.calls, c)
	return nil, nil
}
