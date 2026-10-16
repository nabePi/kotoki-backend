package summary

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Metrics are the summary collectors (spec 13.7). Labels never carry user text or ids. All methods
// are safe on a nil *Metrics.
type Metrics struct {
	failures *prometheus.CounterVec
}

// NewMetrics registers kotoki_summary_failures_total on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	return &Metrics{failures: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "kotoki_summary_failures_total", Help: "Failed summary deliveries by reason."}, []string{"reason"})}
}

// Failure counts one failed delivery.
func (m *Metrics) Failure(reason string) {
	if m != nil {
		m.failures.WithLabelValues(reason).Inc()
	}
}

// Failure reasons (the closed label set of kotoki_summary_failures_total).
const (
	ReasonConfiguration = "configuration_error"
	ReasonNotFound      = "entity_not_found"
	ReasonInvariant     = "invariant_violation"
	ReasonTranscript    = "transcript_unavailable"
	ReasonDecode        = "decode_error"
	ReasonPermanent     = "permanent_other"
	ReasonProvider      = "provider_error"
	ReasonEmptyOutput   = "empty_output"
	ReasonInternal      = "internal_error"
)

// FailureReason maps the error that failed a delivery to a metric label.
func FailureReason(err error) string {
	var perm *events.PermanentError
	if errors.As(err, &perm) {
		switch perm.Code {
		case codeConfigurationError:
			return ReasonConfiguration
		case codeEntityNotFound:
			return ReasonNotFound
		case codeInvariantViolation:
			return ReasonInvariant
		case codeTranscriptUnavailable:
			return ReasonTranscript
		case "DECODE_ERROR":
			return ReasonDecode
		}
		return ReasonPermanent
	}
	var pe *provider.Error
	switch {
	case errors.As(err, &pe):
		return ReasonProvider
	case errors.Is(err, ErrEmptySummary):
		return ReasonEmptyOutput
	}
	return ReasonInternal
}
