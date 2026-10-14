package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	rtestutil "github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestHTTPMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewHTTP(reg, "kotoki-api")
	r := chi.NewRouter()
	r.Use(httpapi.HTTPMetrics(m))
	r.Post("/api/v1/auth/login", ok200)
	r.Get("/api/v1/sessions/{session_id}", ok200)
	r.Get("/healthz", ok200)

	serve(r, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))
	serve(r, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/0192f1a0-1111-7000-8000-000000000001", nil))
	serve(r, httptest.NewRequest(http.MethodGet, "/no/such/path/0192f1a0-1111-7000-8000-000000000001", nil))
	serve(r, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	want := `
# HELP kotoki_http_requests_total HTTP requests by service, method, route pattern and status.
# TYPE kotoki_http_requests_total counter
kotoki_http_requests_total{method="GET",route="/api/v1/sessions/{session_id}",service="kotoki-api",status="200"} 1
kotoki_http_requests_total{method="GET",route="unmatched",service="kotoki-api",status="404"} 1
kotoki_http_requests_total{method="POST",route="/api/v1/auth/login",service="kotoki-api",status="200"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(want), "kotoki_http_requests_total"))
	require.Equal(t, 3, testutil.CollectAndCount(reg, "kotoki_http_request_duration_seconds"))

	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		for _, mt := range f.GetMetric() {
			for _, l := range mt.GetLabel() {
				require.NotContains(t, []string{"user_id", "email", "path"}, l.GetName())
				require.NotContains(t, l.GetValue(), "0192f1a0")
			}
		}
	}
}

func TestTelemetrySpanName(t *testing.T) {
	rec := rtestutil.NewTraceRecorder(t)
	r := chi.NewRouter()
	r.Use(httpapi.Telemetry)
	r.Post("/api/v1/auth/login", ok200)
	r.Get("/boom", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	serve(r, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil))
	serve(r, httptest.NewRequest(http.MethodGet, "/boom", nil))

	span := rec.Find("HTTP POST /api/v1/auth/login")
	require.NotEqual(t, codes.Error, span.Status.Code)
	require.Equal(t, codes.Error, rec.Find("HTTP GET /boom").Status.Code, "5xx marks the span as failed")
}
