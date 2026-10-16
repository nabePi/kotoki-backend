package learner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

var trendT0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

func pts(levels ...float64) []learner.TrendPoint {
	out := make([]learner.TrendPoint, len(levels))
	for i, l := range levels {
		out[i] = learner.TrendPoint{At: trendT0.AddDate(0, 0, i), Level: l}
	}
	return out
}

func newPt(levels []learner.TrendPoint, l float64) learner.TrendPoint {
	return learner.TrendPoint{At: trendT0.AddDate(0, 0, len(levels)), Level: l}
}

func TestTrend(t *testing.T) {
	cases := []struct {
		name string
		hist []float64
		new  float64
		n    int
		want learner.Trend
	}{
		{"stable", []float64{2.0, 2.0}, 2.0, 3, learner.TrendStable},
		{"declining", []float64{2.4, 2.2}, 2.0, 3, learner.TrendDeclining},
		{"improving", []float64{2.16, 2.27}, 2.35, 3, learner.TrendImproving},
		{"two points insufficient", []float64{2.0}, 3.0, 3, learner.TrendInsufficient},
		{"count below three insufficient", []float64{2.0, 2.5}, 3.0, 2, learner.TrendInsufficient},
		{"slope exactly +0.05 improving", []float64{2.00, 2.05}, 2.10, 3, learner.TrendImproving},
		{"slope exactly -0.05 declining", []float64{2.10, 2.05}, 2.00, 3, learner.TrendDeclining},
		{"slope just under threshold stable", []float64{2.00, 2.04}, 2.08, 3, learner.TrendStable},
		// Only the last 4 history points count: the old 1.0 and 1.0 are dropped.
		{"window keeps four most recent", []float64{1.0, 1.0, 3.0, 3.0, 3.0, 3.0}, 3.0, 7, learner.TrendStable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := pts(c.hist...)
			require.Equal(t, c.want, learner.ComputeTrend(h, newPt(h, c.new), c.n))
		})
	}
}

func TestTrendIgnoresPointsOlderThan120Days(t *testing.T) {
	old := []learner.TrendPoint{
		{At: trendT0, Level: 1.0}, {At: trendT0.AddDate(0, 0, 1), Level: 2.0}, {At: trendT0.AddDate(0, 0, 2), Level: 3.0},
	}
	now := learner.TrendPoint{At: trendT0.AddDate(0, 0, 130), Level: 3.5}
	require.Equal(t, learner.TrendInsufficient, learner.ComputeTrend(old, now, 4))

	// Exactly 120 days old is excluded (assessed_at > t - 120d), 119 days is included.
	edge := []learner.TrendPoint{
		{At: trendT0, Level: 1.0}, {At: trendT0.AddDate(0, 0, 1), Level: 2.0},
	}
	require.Equal(t, learner.TrendInsufficient, learner.ComputeTrend(edge, learner.TrendPoint{At: trendT0.AddDate(0, 0, 120), Level: 3}, 3))
	require.Equal(t, learner.TrendImproving, learner.ComputeTrend(edge, learner.TrendPoint{At: trendT0.AddDate(0, 0, 119), Level: 3}, 3))
}
