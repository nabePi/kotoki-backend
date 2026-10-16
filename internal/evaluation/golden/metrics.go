// Package golden holds the calibration tooling of the evaluation rubric (spec 10.12): pure
// metrics, the golden-set dataset and its loader, the runner that scores an evaluator against the
// gold labels, and the report with the pass criteria. Nothing here talks to infrastructure; the
// evaluator under test is injected.
package golden

import (
	"math"
	"sort"
)

// Band maps a 0–100 score to one of the five calibration bands (0–19, 20–39, 40–59, 60–79,
// 80–100); values outside the range clamp to the first or last band.
func Band(score float64) int {
	b := int(math.Floor(score / 20))
	return min(max(b, 0), 4)
}

// MAE is the mean absolute error of model against gold; 0 for empty or unequal inputs.
func MAE(model, gold []float64) float64 {
	if len(model) == 0 || len(model) != len(gold) {
		return 0
	}
	var sum float64
	for i := range model {
		sum += math.Abs(model[i] - gold[i])
	}
	return sum / float64(len(model))
}

// QWK is the quadratic weighted kappa of two ratings in [0, bands) (out-of-range values clamp).
// It is 1 for identical non-constant ratings and 0 when the expected disagreement is zero.
func QWK(a, b []int, bands int) float64 {
	n := len(a)
	if n == 0 || n != len(b) || bands < 2 {
		return 0
	}
	clamp := func(v int) int { return min(max(v, 0), bands-1) }
	obs := make([][]float64, bands)
	for i := range obs {
		obs[i] = make([]float64, bands)
	}
	ha, hb := make([]float64, bands), make([]float64, bands)
	for i := range a {
		x, y := clamp(a[i]), clamp(b[i])
		obs[x][y]++
		ha[x]++
		hb[y]++
	}
	var num, den float64
	denom := float64((bands - 1) * (bands - 1))
	for i := range bands {
		for j := range bands {
			w := float64((i-j)*(i-j)) / denom
			num += w * obs[i][j]
			den += w * ha[i] * hb[j] / float64(n)
		}
	}
	if den == 0 {
		return 0
	}
	return 1 - num/den
}

// Spearman is the rank correlation of x and y (average ranks for ties); 0 for fewer than two
// points, unequal lengths or a constant side.
func Spearman(x, y []float64) float64 {
	if len(x) < 2 || len(x) != len(y) {
		return 0
	}
	rx, ry := ranks(x), ranks(y)
	mx, my := mean(rx), mean(ry)
	var cov, vx, vy float64
	for i := range rx {
		dx, dy := rx[i]-mx, ry[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx == 0 || vy == 0 {
		return 0
	}
	return cov / math.Sqrt(vx*vy)
}

// ranks returns the 1-based ranks of xs, ties sharing their average rank.
func ranks(xs []float64) []float64 {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(p, q int) bool { return xs[idx[p]] < xs[idx[q]] })
	out := make([]float64, len(xs))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, v := range xs {
		s += v
	}
	return s / float64(len(xs))
}

// StdDev is the population standard deviation; 0 for fewer than two values.
func StdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var s float64
	for _, v := range xs {
		s += (v - m) * (v - m)
	}
	return math.Sqrt(s / float64(len(xs)))
}

// StrictlyIncreasing reports whether means rise at every step; empty input is false.
func StrictlyIncreasing(means []float64) bool {
	if len(means) == 0 {
		return false
	}
	for i := 1; i < len(means); i++ {
		if means[i] <= means[i-1] {
			return false
		}
	}
	return true
}

// Recall is found/total; 0 when total is 0.
func Recall(found, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(found) / float64(total)
}
