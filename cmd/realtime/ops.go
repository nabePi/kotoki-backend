package main

import (
	"encoding/json"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// podState is what /readyz asks of the Manager (§7.12.2): a pod that is draining or holds
// REALTIME_READY_MAX_SESSIONS sessions is not ready, so the load balancer stops sending it new
// connections; the connections it has are not affected.
type podState interface {
	Ready() bool
	Draining() bool
}

// opsWrap is health.OpsOptions.Wrap of the realtime pod: /readyz answers 503 while the Manager is
// not Ready (on top of the startup and draining states of health.Ops), and every ops request is
// traced.
func opsWrap(pod podState) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" && !pod.Ready() {
				status := "at_capacity"
				if pod.Draining() {
					status = "draining"
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
				return
			}
			next.ServeHTTP(w, r)
		})
		return otelhttp.NewHandler(gated, "ops")
	}
}
