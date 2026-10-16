package golden

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

func dims() []string {
	out := make([]string, 0, 5)
	for _, d := range evaluation.LLMDimensions {
		out = append(out, string(d))
	}
	return out
}

func TestRun_WithPerfectEvaluator_ExampleDataset(t *testing.T) {
	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	rep, err := Run(context.Background(), ds, perfect(nil), 3)
	require.NoError(t, err)

	assert.Equal(t, VerdictNotEvaluable, rep.Verdict)
	assert.Equal(t, StatusExample, rep.DatasetStatus)
	assert.Equal(t, 8, rep.Samples)
	assert.Equal(t, 1, rep.InsufficientCount)
	assert.NotEmpty(t, rep.Violations)
	for _, d := range dims() {
		assert.LessOrEqual(t, rep.Metrics["MAE."+d].Value, 0.5, d)
		assert.InDelta(t, 1, rep.Metrics["QWK."+d].Value, 1e-9, d)
	}
	assert.InDelta(t, 0, rep.Metrics[MetricStability].Value, 1e-9)
	assert.Equal(t, MetricPass, rep.Metrics[MetricMonotonic].Status)
	assert.Equal(t, MetricPendingManual, rep.Metrics[MetricPrecision].Status)
	assert.Equal(t, MetricReported, rep.Metrics[MetricInterRaterQWK].Status)
}

func runComplete(t *testing.T, ev Evaluator) Report {
	t.Helper()
	rep, err := Run(context.Background(), completeDataset(), ev, 3)
	require.NoError(t, err)
	return rep
}

func TestRun_CompleteDataset_Verdicts(t *testing.T) {
	rep := runComplete(t, perfect(nil))
	assert.Empty(t, rep.Violations)
	assert.Equal(t, 80, rep.Samples)
	assert.Equal(t, 8, rep.InsufficientCount)
	for k, m := range rep.Metrics {
		assert.NotEqual(t, MetricFail, m.Status, k)
	}
	assert.Equal(t, VerdictPendingManual, rep.Verdict, "precision still needs the raters")
	assert.Len(t, rep.LevelOverallMeans, 4)

	ratings := func(valid, total int) []bool {
		out := make([]bool, total)
		for i := range valid {
			out[i] = true
		}
		return out
	}
	assert.Equal(t, VerdictPass, rep.WithPrecision(ratings(80, 100)).Verdict)
	assert.Equal(t, VerdictFail, rep.WithPrecision(ratings(79, 100)).Verdict)
	assert.Equal(t, VerdictPendingManual, rep.Verdict, "WithPrecision does not modify the original")
	assert.Equal(t, VerdictPendingManual, rep.WithPrecision(nil).Verdict)
}

func TestRun_FailingCriteria(t *testing.T) {
	cases := map[string]struct {
		ev  Evaluator
		key string
	}{
		"MAE": {perfect(func(_ Sample, _ int, r *RunResult) {
			r.Scores["GRAMMAR"] -= 11
		}), "MAE.GRAMMAR"},
		"QWK": {perfect(func(_ Sample, _ int, r *RunResult) {
			r.Scores["FLUENCY"] = 100 - r.Scores["FLUENCY"] // reversed ranking
		}), "QWK.FLUENCY"},
		"Spearman": {perfect(func(s Sample, _ int, r *RunResult) {
			r.Scores["VOCABULARY"] = 50 + len(s.ID)%2 // no relation to gold
		}), "SPEARMAN.VOCABULARY"},
		"monotonic level": {perfect(func(s Sample, _ int, r *RunResult) {
			if s.Level == "ADVANCED" {
				for d := range r.Scores {
					r.Scores[d] = 10
				}
			}
		}), MetricMonotonic},
		"stability": {perfect(func(_ Sample, run int, r *RunResult) {
			if run == 1 {
				for d := range r.Scores {
					r.Scores[d] -= 30
				}
			}
		}), MetricStability},
		"recall": {perfect(func(_ Sample, _ int, r *RunResult) { r.Findings = nil }), MetricRecallMajor},
		"validity": {perfect(func(s Sample, _ int, r *RunResult) {
			r.FirstCallValid = s.ID[len(s.ID)-1] != '1'
		}), MetricFirstCallValid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rep := runComplete(t, c.ev)
			assert.Equal(t, MetricFail, rep.Metrics[c.key].Status, "%s = %v", c.key, rep.Metrics[c.key].Value)
			assert.Equal(t, VerdictFail, rep.Verdict)
		})
	}
}

func TestRun_MetricBoundaries(t *testing.T) {
	// MAE of exactly 10 still passes: the evaluator is 10 below floor(gold) on one dimension
	rep := runComplete(t, perfect(func(s Sample, _ int, r *RunResult) {
		g, _ := s.Gold.Score("GRAMMAR")
		r.Scores["GRAMMAR"] = int(g) - 10
	}))
	assert.InDelta(t, 10, rep.Metrics["MAE.GRAMMAR"].Value, 1e-9)
	assert.Equal(t, MetricPass, rep.Metrics["MAE.GRAMMAR"].Status)
	assert.NotNil(t, rep.Metrics["MAE.GRAMMAR"].Threshold)
	assert.Equal(t, "<=", rep.Metrics["MAE.GRAMMAR"].Op)
}

func TestRun_MonotonicNeedsEveryLevel(t *testing.T) {
	ds := completeDataset()
	ds.Manifest.Status = StatusExample
	var kept []Sample
	for _, s := range ds.Samples {
		if s.Level != "ADVANCED" {
			kept = append(kept, s)
		}
	}
	ds.Samples = kept
	rep, err := Run(context.Background(), ds, perfect(nil), 1)
	require.NoError(t, err)
	assert.Equal(t, MetricFail, rep.Metrics[MetricMonotonic].Status, "three rising levels are not enough")
	assert.NotContains(t, rep.LevelOverallMeans, "ADVANCED")
}

func TestRun_Errors(t *testing.T) {
	ctx := context.Background()
	t.Run("complete manifest with an incomplete dataset", func(t *testing.T) {
		ds, err := LoadDataset(exampleDir)
		require.NoError(t, err)
		ds.Manifest.Status = StatusComplete
		_, err = Run(ctx, ds, perfect(nil), 1)
		require.ErrorContains(t, err, "COMPLETE")
	})
	t.Run("evaluator error", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := Run(ctx, completeDataset(), func(context.Context, Sample, int) (RunResult, error) { return RunResult{}, boom }, 1)
		require.ErrorIs(t, err, boom)
	})
	t.Run("missing dimension", func(t *testing.T) {
		_, err := Run(ctx, completeDataset(), perfect(func(_ Sample, _ int, r *RunResult) { delete(r.Scores, "GRAMMAR") }), 1)
		require.ErrorContains(t, err, "no score for GRAMMAR")
	})
	t.Run("missing adjudication", func(t *testing.T) {
		ds := completeDataset()
		ds.Samples[0].Gold.Rater2["GRAMMAR"] = ds.Samples[0].Gold.Rater1["GRAMMAR"] + 30
		_, err := Run(ctx, ds, perfect(nil), 1)
		require.ErrorIs(t, err, ErrAdjudicationMissing)
	})
	t.Run("no scored samples", func(t *testing.T) {
		ds := completeDataset()
		ds.Manifest.Status = StatusExample
		ds.Samples = ds.Samples[80:]
		_, err := Run(ctx, ds, perfect(nil), 1)
		require.ErrorContains(t, err, "no scored samples")
	})
	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		_, err := Run(c, completeDataset(), perfect(nil), 1)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("runs below one means one", func(t *testing.T) {
		rep, err := Run(ctx, completeDataset(), perfect(nil), 0)
		require.NoError(t, err)
		assert.Equal(t, 1, rep.Runs)
	})
}

func TestRecallMajor(t *testing.T) {
	me := MajorError{TurnSeq: 2, FindingType: "PARTICLE_ERROR"}
	cases := map[string]struct {
		f    FindingKey
		want bool
	}{
		"same turn, same type":                 {FindingKey{TurnSeq: 2, Type: "PARTICLE_ERROR", Dimension: "GRAMMAR"}, true},
		"same turn, same dimension other type": {FindingKey{TurnSeq: 2, Type: "CONJUGATION_ERROR", Dimension: "GRAMMAR"}, true},
		"same turn, other dimension":           {FindingKey{TurnSeq: 2, Type: "WORD_CHOICE_ERROR", Dimension: "VOCABULARY"}, false},
		"other turn, same type":                {FindingKey{TurnSeq: 3, Type: "PARTICLE_ERROR", Dimension: "GRAMMAR"}, false},
	}
	for name, c := range cases {
		assert.Equal(t, c.want, majorFound(me, []FindingKey{c.f}), name)
	}
	assert.False(t, majorFound(me, nil))
	assert.False(t, majorFound(MajorError{TurnSeq: 2, FindingType: "NOT_A_TYPE"}, []FindingKey{{TurnSeq: 2, Type: "X"}}), "unknown gold type matches by type only")
}
