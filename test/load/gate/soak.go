package gate

import "math"

// SoakDrift is |mean(last) - mean(first)| / mean(first): the relative change of a per-pod series
// (RSS, goroutines) between the first and the last window of a run. It is NaN when either window has
// no samples or the first mean is 0, so a missing measurement can never read as "stable".
func SoakDrift(first, last []float64) float64 {
	if len(first) == 0 || len(last) == 0 {
		return math.NaN()
	}
	f := mean(first)
	if f == 0 {
		return math.NaN()
	}
	return math.Abs(mean(last)-f) / f
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
