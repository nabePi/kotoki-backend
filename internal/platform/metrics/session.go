package metrics

import "github.com/prometheus/client_golang/prometheus"

// Session holds the session lifecycle counters (spec 13.7.3). It implements session.Metrics.
type Session struct {
	created *prometheus.CounterVec
	ended   *prometheus.CounterVec
}

// NewSession registers the session metrics on reg.
func NewSession(reg prometheus.Registerer) *Session {
	s := &Session{
		created: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_sessions_created_total", Help: "Session creation attempts by result (ok, insufficient_balance, error)."}, []string{"result"}),
		ended: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_sessions_ended_total", Help: "Sessions that reached a terminal status."}, []string{"service", "end_reason", "status"}),
	}
	reg.MustRegister(s.created, s.ended)
	return s
}

// Created counts one creation attempt.
func (s *Session) Created(result string) { s.created.WithLabelValues(result).Inc() }

// Ended counts one session that reached a terminal status.
func (s *Session) Ended(service, endReason, status string) {
	s.ended.WithLabelValues(service, endReason, status).Inc()
}
