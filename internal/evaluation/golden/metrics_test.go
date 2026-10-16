package golden

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBand(t *testing.T) {
	cases := map[float64]int{
		0: 0, 19: 0, 19.9: 0, 20: 1, 39.9: 1, 40: 2, 59: 2, 60: 3, 79: 3, 79.99: 3, 80: 4, 100: 4,
		-5: 0, 140: 4, // out of range clamps
	}
	for score, want := range cases {
		assert.Equal(t, want, Band(score), "score %v", score)
	}
}

func TestMAE(t *testing.T) {
	assert.InDelta(t, 10, MAE([]float64{50, 60}, []float64{40, 70}), 1e-9)
	assert.InDelta(t, 0, MAE([]float64{50, 60}, []float64{50, 60}), 1e-9)
	assert.Zero(t, MAE(nil, nil))
	assert.Zero(t, MAE([]float64{1}, []float64{1, 2}), "length mismatch")
}

func TestQWK(t *testing.T) {
	assert.InDelta(t, 1.0, QWK([]int{0, 1, 2, 3, 4}, []int{0, 1, 2, 3, 4}, 5), 1e-9)
	assert.InDelta(t, -1.0, QWK([]int{0, 1, 2, 3, 4}, []int{4, 3, 2, 1, 0}, 5), 1e-9)

	// Hand computed, 3 bands, a=[0,0,1,1,2,2] b=[0,1,1,2,2,2]:
	//   observed pairs (0,0) (0,1) (1,1) (1,2) (2,2) (2,2); N=6
	//   weights w=(i-j)^2/4: only (0,1) and (1,2) differ, each 1/4 -> sum w*O = 2/4 = 0.5
	//   hist a = [2,2,2], hist b = [1,2,3]; expected E[i][j]=ha[i]*hb[j]/6
	//   sum w*E = (1/4)*(E01+E10+E12+E21) + 1*(E02+E20)
	//     E01=2*2/6, E10=2*1/6, E12=2*3/6, E21=2*2/6 -> (4+2+6+4)/6=16/6 -> /4 = 2/3
	//     E02=2*3/6, E20=2*1/6 -> (6+2)/6 = 8/6 -> weight (2-0)^2/4 = 1 -> 4/3
	//   sum w*E = 2/3 + 4/3 = 2 ; kappa = 1 - 0.5/2 = 0.75
	assert.InDelta(t, 0.75, QWK([]int{0, 0, 1, 1, 2, 2}, []int{0, 1, 1, 2, 2, 2}, 3), 1e-9)

	assert.Zero(t, QWK(nil, nil, 5))
	assert.Zero(t, QWK([]int{1, 1}, []int{1, 1}, 5), "no variance divides by zero")
	assert.Zero(t, QWK([]int{1}, []int{1, 2}, 5))
	assert.Zero(t, QWK([]int{0}, []int{0}, 1))
}

func TestSpearman(t *testing.T) {
	assert.InDelta(t, 1, Spearman([]float64{1, 2, 3}, []float64{1, 2, 3}), 1e-9)
	assert.InDelta(t, -1, Spearman([]float64{1, 2, 3}, []float64{9, 5, 1}), 1e-9)
	// ties get average ranks: x ranks [1.5 1.5 3], y ranks [1 2 3] -> 1.5/sqrt(1.5*2)
	assert.InDelta(t, math.Sqrt(3)/2, Spearman([]float64{1, 1, 2}, []float64{1, 2, 3}), 1e-9)
	// ties in the middle: avg ranks [1 2.5 2.5 4] -> 4.5/sqrt(4.5*5)
	assert.InDelta(t, 4.5/math.Sqrt(22.5), Spearman([]float64{1, 2, 2, 3}, []float64{1, 2, 3, 4}), 1e-9)
	assert.Zero(t, Spearman([]float64{2, 2, 2}, []float64{1, 2, 3}), "constant side")
	assert.Zero(t, Spearman([]float64{1}, []float64{1}))
	assert.Zero(t, Spearman([]float64{1, 2}, []float64{1}))
}

func TestStdDev(t *testing.T) {
	assert.InDelta(t, 2, StdDev([]float64{2, 4, 4, 4, 5, 5, 7, 9}), 1e-9)
	assert.Zero(t, StdDev(nil))
	assert.Zero(t, StdDev([]float64{5}))
}

func TestStrictlyIncreasing(t *testing.T) {
	assert.True(t, StrictlyIncreasing([]float64{1, 2, 3}))
	assert.False(t, StrictlyIncreasing([]float64{1, 2, 2}))
	assert.False(t, StrictlyIncreasing([]float64{3, 2, 1}))
	assert.True(t, StrictlyIncreasing([]float64{1}))
	assert.False(t, StrictlyIncreasing(nil))
}

func TestRecall(t *testing.T) {
	assert.Zero(t, Recall(0, 0))
	assert.InDelta(t, 0.75, Recall(3, 4), 1e-9)
	require.InDelta(t, 1, Recall(2, 2), 1e-9)
}
