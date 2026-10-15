package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.Metrics = (*metrics.Session)(nil)

func TestSessionMetricsAdapter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSession(reg)
	m.Created("ok")
	m.Created("ok")
	m.Created("insufficient_balance")
	m.Created("error")
	m.Ended("realtime", "USER_ENDED", "COMPLETED")
	m.Ended("worker", "SERVER_ERROR", "FAILED")

	count := func(name string, labels map[string]string) float64 {
		t.Helper()
		mfs, err := reg.Gather()
		require.NoError(t, err)
		for _, mf := range mfs {
			if mf.GetName() != name {
				continue
			}
		next:
			for _, mm := range mf.GetMetric() {
				got := map[string]string{}
				for _, l := range mm.GetLabel() {
					got[l.GetName()] = l.GetValue()
				}
				for k, v := range labels {
					if got[k] != v {
						continue next
					}
				}
				return mm.GetCounter().GetValue()
			}
		}
		return 0
	}
	require.Equal(t, 2.0, count("kotoki_sessions_created_total", map[string]string{"result": "ok"}))
	require.Equal(t, 1.0, count("kotoki_sessions_created_total", map[string]string{"result": "insufficient_balance"}))
	require.Equal(t, 1.0, count("kotoki_sessions_created_total", map[string]string{"result": "error"}))
	require.Equal(t, 1.0, count("kotoki_sessions_ended_total", map[string]string{"service": "realtime", "end_reason": "USER_ENDED", "status": "COMPLETED"}))
	require.Equal(t, 1.0, count("kotoki_sessions_ended_total", map[string]string{"service": "worker", "end_reason": "SERVER_ERROR", "status": "FAILED"}))
	require.Equal(t, 0.0, count("kotoki_sessions_ended_total", map[string]string{"service": "api"}))
}
