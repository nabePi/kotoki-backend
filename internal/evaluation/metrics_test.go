package evaluation_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

func TestMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := evaluation.NewMetrics(reg)
	m.LLMCall("OPENAI", "valid")
	m.LLMCall("OPENAI", "valid")
	m.LLMCall("OPENAI", "invalid")
	m.FindingDropped("original_not_found")
	m.ObserveLatency(1.5)

	n, err := testutil.GatherAndCount(reg, "kotoki_evaluation_llm_calls_total", "kotoki_evaluation_findings_dropped_total", "kotoki_evaluation_latency_seconds")
	require.NoError(t, err)
	require.Equal(t, 4, n) // 2 llm series + 1 dropped series + 1 histogram

	require.Equal(t, 2.0, counter(t, reg, "kotoki_evaluation_llm_calls_total", map[string]string{"provider": "OPENAI", "result": "valid"}))
	require.Equal(t, 1.0, counter(t, reg, "kotoki_evaluation_findings_dropped_total", map[string]string{"reason": "original_not_found"}))

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "kotoki_evaluation_latency_seconds" {
			h := mf.GetMetric()[0].GetHistogram()
			require.EqualValues(t, 1, h.GetSampleCount())
			require.Equal(t, 1.5, h.GetSampleSum())
			require.Len(t, h.GetBucket(), 12)
		}
	}
}

func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if labels[lp.GetName()] != lp.GetValue() {
					continue next
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("series %s%v not found", name, labels)
	return 0
}

func TestMetricsNilSafe(t *testing.T) {
	var m *evaluation.Metrics
	require.NotPanics(t, func() {
		m.LLMCall("OPENAI", "valid")
		m.FindingDropped("x")
		m.ObserveLatency(1)
	})
}
