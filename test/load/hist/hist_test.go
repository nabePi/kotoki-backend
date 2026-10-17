package hist

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHistogram_Quantile(t *testing.T) {
	h := New()
	for i := 0; i < 10000; i++ {
		h.Record(float64(i) / 10000)
	}
	require.Equal(t, 10000, h.Count())
	p95 := h.Quantile(0.95)
	require.GreaterOrEqual(t, p95, 0.94)
	require.LessOrEqual(t, p95, 0.96)
	p50 := h.Quantile(0.5)
	require.InDelta(t, 0.5, p50, 0.01)
}

func TestHistogram_EmptyIsNaN(t *testing.T) {
	h := New()
	require.True(t, math.IsNaN(h.Quantile(0.95)))
	require.Equal(t, 0, h.Count())
}

func TestHistogram_RelativeErrorWithinOnePercent(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	h := New()
	var xs []float64
	for i := 0; i < 20000; i++ {
		v := math.Exp(rng.Float64()*math.Log(1e6)) * 1e-3 // 1e-3 .. 1e3, log-uniform
		xs = append(xs, v)
		h.Record(v)
	}
	sort.Float64s(xs)
	var qs []float64
	for i := 1; i <= 999; i++ {
		qs = append(qs, float64(i)/1000)
	}
	for _, q := range qs {
		rank := int(math.Ceil(q*float64(len(xs)))) - 1
		want := xs[rank]
		got := h.Quantile(q)
		require.LessOrEqualf(t, math.Abs(got-want)/want, 0.01, "q=%v got=%v want=%v", q, got, want)
	}
}

func TestHistogram_Merge(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	a, b, all := New(), New(), New()
	for i := 0; i < 5000; i++ {
		v := rng.Float64() * 3
		a.Record(v)
		all.Record(v)
		w := rng.Float64() * 0.2
		b.Record(w)
		all.Record(w)
	}
	a.Merge(b)
	require.Equal(t, all.Count(), a.Count())
	for _, q := range []float64{0.1, 0.5, 0.95, 0.99} {
		require.Equal(t, all.Quantile(q), a.Quantile(q), "q=%v", q)
	}
	// b is untouched by the merge
	require.Equal(t, 5000, b.Count())
	a.Merge(nil)
	require.Equal(t, 10000, a.Count())
	a.Merge(a) // self merge doubles counts without deadlock
	require.Equal(t, 20000, a.Count())
}

func TestHistogram_IgnoresNonFinite(t *testing.T) {
	h := New()
	h.Record(math.NaN())
	h.Record(math.Inf(1))
	h.Record(math.Inf(-1))
	require.Equal(t, 0, h.Count())
	require.True(t, math.IsNaN(h.Quantile(0.5)))
}

func TestHistogram_ZeroAndNegative(t *testing.T) {
	h := New()
	h.Record(0)
	h.Record(-5) // clamped to zero: latencies are never negative
	h.Record(2)
	require.Equal(t, 3, h.Count())
	require.Equal(t, 0.0, h.Quantile(0.5))
	require.InEpsilon(t, 2, h.Quantile(1), 0.01)
}

func TestHistogram_QuantileBounds(t *testing.T) {
	h := New()
	for _, v := range []float64{1, 2, 3, 4} {
		h.Record(v)
	}
	require.InEpsilon(t, 1, h.Quantile(0), 0.01)
	require.InEpsilon(t, 1, h.Quantile(-1), 0.01)
	require.InEpsilon(t, 4, h.Quantile(1), 0.01)
	require.InEpsilon(t, 4, h.Quantile(2), 0.01)
	require.True(t, math.IsNaN(h.Quantile(math.NaN())))
}

func TestHistogram_ConcurrentRecord(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				h.Record(float64(i%100) / 100)
				_ = h.Quantile(0.5)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 8000, h.Count())
}
