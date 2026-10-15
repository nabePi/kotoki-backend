package metrics

import "github.com/prometheus/client_golang/prometheus"

var contentSeverities = map[string]bool{"ERROR": true, "WARNING": true}

// Content holds the content metrics (spec 6.9, 6.10.3). It implements content.Observer.
type Content struct {
	resolveErrors   *prometheus.CounterVec
	voiceFallback   *prometheus.CounterVec
	validationIssue *prometheus.GaugeVec
}

// NewContent registers the content metrics on reg.
func NewContent(reg prometheus.Registerer) *Content {
	c := &Content{
		resolveErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_content_resolve_errors_total", Help: "Content resolution errors by error code."}, []string{"code"}),
		voiceFallback: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kotoki_content_voice_fallback_missing_total", Help: "Sessions created for a character that has no TTS fallback voice."}, []string{"character"}),
		validationIssue: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kotoki_content_validation_issues", Help: "Content validation issues found by the last catalog validation."}, []string{"severity"}),
	}
	reg.MustRegister(c.resolveErrors, c.voiceFallback, c.validationIssue)
	return c
}

// ResolveError counts a resolution error; an empty code is ignored.
func (c *Content) ResolveError(code string) {
	if code != "" {
		c.resolveErrors.WithLabelValues(code).Inc()
	}
}

// VoiceFallbackMissing counts a session of a character without a fallback voice.
func (c *Content) VoiceFallbackMissing(character string) {
	if character != "" {
		c.voiceFallback.WithLabelValues(character).Inc()
	}
}

// ValidationIssues sets the number of issues of a severity (ERROR or WARNING).
func (c *Content) ValidationIssues(severity string, n int) {
	if contentSeverities[severity] {
		c.validationIssue.WithLabelValues(severity).Set(float64(n))
	}
}
