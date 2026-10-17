// Package hist is a small log-bucketed histogram for latency samples. Bucket i holds values in
// (gamma^(i-1), gamma^i]; a quantile is reported as the bucket midpoint, so the relative error is at
// most (gamma-1)/(gamma+1), just under 1% for gamma = 1.02. It is safe for concurrent use.
package hist

import (
	"math"
	"sort"
	"sync"
)

const gamma = 1.02

var logGamma = math.Log(gamma)

// Histogram counts samples in logarithmic buckets. Zero and negative samples share one bucket that
// reports 0 (latencies are never negative; clamping keeps a clock hiccup from breaking a run).
type Histogram struct {
	mu      sync.Mutex
	zero    int
	buckets map[int]int
	count   int
}

// New returns an empty histogram.
func New() *Histogram { return &Histogram{buckets: map[int]int{}} }

// Record adds one sample. NaN and infinite samples are ignored.
func (h *Histogram) Record(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	if v <= 0 {
		h.zero++
		return
	}
	h.buckets[int(math.Ceil(math.Log(v)/logGamma))]++
}

// Count is the number of recorded samples.
func (h *Histogram) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Quantile returns the q-quantile (q is clamped to [0,1]); NaN when the histogram is empty or q is NaN.
func (h *Histogram) Quantile(q float64) float64 {
	if math.IsNaN(q) {
		return math.NaN()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return math.NaN()
	}
	q = math.Min(1, math.Max(0, q))
	rank := int(math.Ceil(q * float64(h.count)))
	if rank < 1 {
		rank = 1
	}
	if rank <= h.zero {
		return 0
	}
	idx := make([]int, 0, len(h.buckets))
	for i := range h.buckets {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	seen := h.zero
	for _, i := range idx {
		seen += h.buckets[i]
		if seen >= rank {
			return 2 * math.Pow(gamma, float64(i)) / (gamma + 1)
		}
	}
	return math.NaN() // unreachable: counts add up to h.count
}

// Merge adds every sample of o into h. o is not modified; merging a histogram into itself doubles it.
func (h *Histogram) Merge(o *Histogram) {
	if o == nil {
		return
	}
	o.mu.Lock()
	zero, count := o.zero, o.count
	snap := make(map[int]int, len(o.buckets))
	for i, n := range o.buckets {
		snap[i] = n
	}
	o.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.zero += zero
	h.count += count
	for i, n := range snap {
		h.buckets[i] += n
	}
}
