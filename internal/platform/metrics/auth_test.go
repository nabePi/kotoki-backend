package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

func TestMetricsAuthCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	a := metrics.NewAuth(reg)
	a.Record("login", "success")
	a.Record("login", "success")
	a.Record("refresh_reuse", "failure")

	want := `
# HELP kotoki_auth_events_total Authentication events by outcome.
# TYPE kotoki_auth_events_total counter
kotoki_auth_events_total{event="login",result="success"} 2
kotoki_auth_events_total{event="refresh_reuse",result="failure"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(want), "kotoki_auth_events_total"))
}

func TestMetricsAuthIgnoresUnknownLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	a := metrics.NewAuth(reg)
	require.NotPanics(t, func() {
		a.Record("bogus", "success")
		a.Record("login", "maybe")
		a.Record("user@example.com", "failure")
	})
	require.Zero(t, testutil.CollectAndCount(reg, "kotoki_auth_events_total"))
}
