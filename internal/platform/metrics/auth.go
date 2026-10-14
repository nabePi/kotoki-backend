package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	authEvents  = map[string]bool{"login": true, "refresh": true, "refresh_reuse": true, "password_reset": true, "register": true}
	authResults = map[string]bool{"success": true, "failure": true}
)

// Auth counts authentication events. Label values outside the fixed sets are
// ignored so a caller can never create unbounded series.
type Auth struct{ events *prometheus.CounterVec }

// NewAuth registers kotoki_auth_events_total on reg.
func NewAuth(reg prometheus.Registerer) *Auth {
	a := &Auth{events: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kotoki_auth_events_total",
		Help: "Authentication events by outcome.",
	}, []string{"event", "result"})}
	reg.MustRegister(a.events)
	return a
}

// Record counts one event; unknown event/result values are dropped.
func (a *Auth) Record(event, result string) {
	if !authEvents[event] || !authResults[result] {
		return
	}
	a.events.WithLabelValues(event, result).Inc()
}
