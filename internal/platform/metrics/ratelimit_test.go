package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

func TestRateLimitMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewRateLimit(reg)
	m.Rejected("auth_login_ip")
	m.Rejected("auth_login_ip")
	m.Rejected("admin_user")
	m.BackendError()

	want := `
# HELP kotoki_ratelimit_backend_errors_total Rate limiter backend (Redis) errors; requests fall back to the local limiter.
# TYPE kotoki_ratelimit_backend_errors_total counter
kotoki_ratelimit_backend_errors_total 1
# HELP kotoki_ratelimit_rejected_total Requests rejected by the rate limiter, by policy.
# TYPE kotoki_ratelimit_rejected_total counter
kotoki_ratelimit_rejected_total{policy="admin_user"} 1
kotoki_ratelimit_rejected_total{policy="auth_login_ip"} 2
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(want)))
}
