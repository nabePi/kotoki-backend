package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

func TestContentMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewContent(reg)

	m.ResolveError("CONTENT_MISCONFIGURED")
	m.ResolveError("CONTENT_MISCONFIGURED")
	m.ResolveError("CHARACTER_NOT_AVAILABLE")
	require.Equal(t, 2.0, testutil.ToFloat64(contentCounter(t, reg, "kotoki_content_resolve_errors_total", "code", "CONTENT_MISCONFIGURED")))

	m.VoiceFallbackMissing("HARUKA")
	require.Equal(t, 1.0, testutil.ToFloat64(contentCounter(t, reg, "kotoki_content_voice_fallback_missing_total", "character", "HARUKA")))

	m.ValidationIssues("ERROR", 3)
	m.ValidationIssues("WARNING", 1)
	require.Equal(t, 3.0, gaugeValue(t, reg, "ERROR"))
	m.ValidationIssues("ERROR", 0)
	require.Equal(t, 0.0, gaugeValue(t, reg, "ERROR"), "the gauge is set, not accumulated")
	require.Equal(t, 1.0, gaugeValue(t, reg, "WARNING"))

	m.ValidationIssues("INFO", 9) // unknown severities are ignored
	m.ResolveError("")            // so are empty codes
	n, err := testutil.GatherAndCount(reg, "kotoki_content_validation_issues")
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

func contentCounter(t *testing.T, reg *prometheus.Registry, name, label, value string) prometheus.Collector {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, mm := range f.GetMetric() {
			for _, l := range mm.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return constMetric(mm.GetCounter().GetValue())
				}
			}
		}
	}
	t.Fatalf("series %s{%s=%q} missing", name, label, value)
	return nil
}

type constMetric float64

func (c constMetric) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("const", "const", nil, nil)
}

func (c constMetric) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("const", "const", nil, nil), prometheus.GaugeValue, float64(c))
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, severity string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() != "kotoki_content_validation_issues" {
			continue
		}
		for _, mm := range f.GetMetric() {
			for _, l := range mm.GetLabel() {
				if l.GetValue() == severity {
					return mm.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("gauge for %s missing", severity)
	return 0
}
