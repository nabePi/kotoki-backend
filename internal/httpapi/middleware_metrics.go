package httpapi

import (
	"net/http"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

var skipMetrics = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

// HTTPMetrics records request count and latency per chi route pattern. Paths
// without a matching route are recorded as "unmatched" so identifiers in raw
// paths never become label values.
func HTTPMetrics(m *metrics.HTTP) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skipMetrics[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			m.Observe(r.Method, routePattern(r), sw.code(), time.Since(start))
		})
	}
}
