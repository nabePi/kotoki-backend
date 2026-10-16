package memory_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

func ptr[T any](v T) *T { return &v }

// uuidFor is a deterministic id per index.
func uuidFor(i int) uuid.UUID {
	return uuid.MustParse("01920000-0000-7000-8000-" + padHex(i))
}

func padHex(i int) string {
	const digits = "0123456789abcdef"
	b := []byte("000000000000")
	for p := len(b) - 1; i > 0 && p >= 0; p-- {
		b[p] = digits[i%16]
		i /= 16
	}
	return string(b)
}

// newMetrics returns Metrics on a fresh registry.
func newMetrics(t *testing.T) (*memory.Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return memory.NewMetrics(reg), reg
}

func counterValues(t *testing.T, reg *prometheus.Registry, name, label string) map[string]float64 {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label {
					out[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

func rejectedCounts(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	return counterValues(t, reg, "kotoki_memory_candidates_rejected_total", "reason")
}

func acceptedCounts(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	return counterValues(t, reg, "kotoki_memory_candidates_total", "category")
}
