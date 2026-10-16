package golden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_MAERegressionVsBaseline(t *testing.T) {
	mk := func(grammar, vocab float64) Report {
		return Report{Metrics: map[string]MetricResult{"MAE.GRAMMAR": {Value: grammar}, "MAE.VOCABULARY": {Value: vocab}}}
	}
	got := Compare(mk(12.5, 8), mk(10, 8))
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "GRAMMAR")
	assert.Empty(t, Compare(mk(12, 9), mk(10, 8)), "a loss of exactly 2 (and 1) is accepted")
	assert.Empty(t, Compare(mk(5, 5), mk(10, 10)), "improvements are fine")
	assert.Empty(t, Compare(Report{}, mk(1, 1)), "a missing metric is not a regression")
	assert.Len(t, Compare(mk(20, 20), mk(10, 10)), 2)
}

func TestExportFindingsForRating(t *testing.T) {
	var rep Report
	for i := range 150 {
		rep.Findings = append(rep.Findings, RatedFinding{SampleID: fmt.Sprintf("s%03d", i%30), Run: i/30 + 1,
			Key: FindingKey{TurnSeq: i, Type: "PARTICLE_ERROR", Dimension: "GRAMMAR", Severity: "MINOR", Original: fmt.Sprintf("o%d", i), Corrected: "c"}})
	}
	a := ExportFindingsForRating(rep, 100, 7)
	b := ExportFindingsForRating(rep, 100, 7)
	assert.Len(t, a, 100)
	assert.Equal(t, a, b, "deterministic for a seed")
	assert.NotEqual(t, a, ExportFindingsForRating(rep, 100, 8))

	seen := map[string]bool{}
	for i, it := range a {
		assert.Equal(t, i+1, it.ID)
		assert.False(t, seen[it.Original], "no finding is drawn twice")
		seen[it.Original] = true
	}
	assert.Len(t, ExportFindingsForRating(rep, 500, 1), 150, "all findings when fewer than n")
	assert.Empty(t, ExportFindingsForRating(rep, 0, 1))
	assert.Empty(t, ExportFindingsForRating(Report{}, 10, 1))

	raw, err := json.Marshal(a[0])
	require.NoError(t, err)
	var keys map[string]any
	require.NoError(t, json.Unmarshal(raw, &keys))
	assert.ElementsMatch(t, []string{"id", "sample_id", "turn_seq", "finding_type", "dimension", "severity", "original", "corrected"}, mapKeys(keys),
		"only the quoted text of the finding, never the transcript")
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPrecisionFromRatings(t *testing.T) {
	assert.Zero(t, PrecisionFromRatings(nil))
	assert.InDelta(t, 0.75, PrecisionFromRatings([]bool{true, true, true, false}), 1e-9)
	assert.InDelta(t, 1, PrecisionFromRatings([]bool{true}), 1e-9)
}

func mainDeps(ev Evaluator, built *bool, cleaned *bool) MainDeps {
	return MainDeps{Build: func(context.Context) (Evaluator, func(), error) {
		if built != nil {
			*built = true
		}
		return ev, func() {
			if cleaned != nil {
				*cleaned = true
			}
		}, nil
	}}
}

func TestMain_WritesReport(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.json")
	exportPath := filepath.Join(t.TempDir(), "findings.json")
	var stdout, stderr bytes.Buffer
	var cleaned bool
	code := Main([]string{"--dataset", exampleDir, "--runs", "3", "--out", out, "--export-findings", exportPath, "--export-count", "5"},
		&stdout, &stderr, mainDeps(perfect(nil), nil, &cleaned))
	require.Equal(t, 0, code, stderr.String())
	assert.True(t, cleaned, "the evaluator is released")
	assert.Contains(t, stdout.String(), "verdict: NOT_EVALUABLE_EXAMPLE_DATASET")

	var rep Report
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &rep))
	assert.Equal(t, "EXAMPLE", rep.DatasetStatus)
	assert.Equal(t, VerdictNotEvaluable, rep.Verdict)
	assert.Equal(t, 3, rep.Runs)
	assert.NotContains(t, string(b), "テスト", "findings never land in report.json")

	var items []RatingItem
	b, err = os.ReadFile(exportPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &items))
	assert.Len(t, items, 5)
}

func TestMain_BaselineRegressionAndRatings(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "r.json")
	base := filepath.Join(dir, "base.json")
	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, Main([]string{"--dataset", exampleDir, "--out", base}, &stdout, &stderr, mainDeps(perfect(nil), nil, nil)), stderr.String())

	worse := perfect(func(_ Sample, _ int, r *RunResult) { r.Scores["GRAMMAR"] = min(100, r.Scores["GRAMMAR"]+5) })
	stdout.Reset()
	code := Main([]string{"--dataset", exampleDir, "--out", out, "--baseline", base}, &stdout, &stderr, mainDeps(worse, nil, nil))
	assert.Equal(t, 1, code)
	assert.Contains(t, stdout.String(), "regression: GRAMMAR")

	ratings := filepath.Join(dir, "ratings.json")
	require.NoError(t, os.WriteFile(ratings, []byte(`[true,true,false]`), 0o600))
	stdout.Reset()
	require.Equal(t, 0, Main([]string{"--dataset", exampleDir, "--out", out, "--ratings", ratings}, &stdout, &stderr, mainDeps(perfect(nil), nil, nil)),
		"the example dataset is never a FAIL verdict, however the raters judged")
	var rep Report
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &rep))
	assert.Equal(t, MetricFail, rep.Metrics[MetricPrecision].Status, "2 of 3 valid is below 0.80")
	assert.InDelta(t, 0.67, rep.Metrics[MetricPrecision].Value, 1e-9)
}

func TestMain_UsageAndFailures(t *testing.T) {
	ev := perfect(nil)
	cases := map[string]struct {
		args []string
		code int
	}{
		"no dataset":       {[]string{}, 2},
		"bad flag":         {[]string{"--nope"}, 2},
		"runs zero":        {[]string{"--dataset", exampleDir, "--runs", "0"}, 2},
		"positional":       {[]string{"--dataset", exampleDir, "extra"}, 2},
		"missing dataset":  {[]string{"--dataset", "/nonexistent"}, 1},
		"missing baseline": {[]string{"--dataset", exampleDir, "--out", filepath.Join(t.TempDir(), "r.json"), "--baseline", "/nonexistent.json"}, 1},
		"missing ratings":  {[]string{"--dataset", exampleDir, "--out", filepath.Join(t.TempDir(), "r.json"), "--ratings", "/nonexistent.json"}, 1},
		"unwritable out":   {[]string{"--dataset", exampleDir, "--out", "/nonexistent-dir/r.json"}, 1},
	}
	for name, c := range cases {
		var stdout, stderr bytes.Buffer
		assert.Equal(t, c.code, Main(c.args, &stdout, &stderr, mainDeps(ev, nil, nil)), "%s: %s", name, stderr.String())
	}
	t.Run("build error", func(t *testing.T) {
		var stderr bytes.Buffer
		code := Main([]string{"--dataset", exampleDir}, &bytes.Buffer{}, &stderr, MainDeps{Build: func(context.Context) (Evaluator, func(), error) {
			return nil, nil, fmt.Errorf("no database")
		}})
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr.String(), "no database")
	})
	t.Run("evaluator error", func(t *testing.T) {
		var stderr bytes.Buffer
		code := Main([]string{"--dataset", exampleDir, "--out", filepath.Join(t.TempDir(), "r.json")}, &bytes.Buffer{}, &stderr,
			mainDeps(func(context.Context, Sample, int) (RunResult, error) { return RunResult{}, fmt.Errorf("provider down") }, nil, nil))
		assert.Equal(t, 1, code)
		assert.Contains(t, stderr.String(), "provider down")
	})
}
