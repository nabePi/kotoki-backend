package golden

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const exampleDir = "../../../testdata/benchmark/evaluation/rubric-v1"

func TestLoadDataset_Example(t *testing.T) {
	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	assert.Equal(t, StatusExample, ds.Manifest.Status)
	assert.Equal(t, "rubric-v1", ds.Manifest.RubricVersion)
	assert.Len(t, ds.Samples, 9)

	perLevel := map[string]int{}
	insufficient := 0
	for _, s := range ds.Samples {
		if s.Insufficient {
			insufficient++
			assert.Empty(t, s.Gold.Rater1, "an insufficient sample is not scored")
			continue
		}
		perLevel[s.Level]++
		assert.Equal(t, 6, s.EligibleUserTurns(), s.ID)
		assert.Len(t, s.Gold.Rater1, 5, s.ID)
	}
	assert.Equal(t, 1, insufficient)
	for _, l := range Levels {
		assert.Equal(t, 2, perLevel[l], l)
	}
	assert.Equal(t, 1, ds.Samples[0].Gold.MajorErrors[0].TurnSeq-3, "beg-cafe-1 has its MAJOR error on turn 4")
}

func TestGoldScore(t *testing.T) {
	dim := "GRAMMAR"
	g := Gold{Rater1: map[string]int{dim: 60}, Rater2: map[string]int{dim: 70}}
	v, err := g.Score(dim)
	require.NoError(t, err)
	assert.InDelta(t, 65, v, 1e-9)

	g = Gold{Rater1: map[string]int{dim: 40}, Rater2: map[string]int{dim: 70}}
	_, err = g.Score(dim)
	assert.ErrorIs(t, err, ErrAdjudicationMissing)

	g.Rater3 = map[string]int{dim: 65}
	v, err = g.Score(dim)
	require.NoError(t, err)
	assert.InDelta(t, 67.5, v, 1e-9, "mean of the two closest raters (70 and 65)")

	g.Rater3 = map[string]int{dim: 45}
	v, err = g.Score(dim)
	require.NoError(t, err)
	assert.InDelta(t, 42.5, v, 1e-9, "mean of 40 and 45")

	g = Gold{Rater1: map[string]int{dim: 40}, Rater2: map[string]int{dim: 60}}
	v, err = g.Score(dim)
	require.NoError(t, err, "a gap of exactly 20 needs no adjudication")
	assert.InDelta(t, 50, v, 1e-9)

	_, err = g.Score("FLUENCY")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAdjudicationMissing))
}

func TestCheckComplete_ExampleHasViolations(t *testing.T) {
	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	v := ds.CheckComplete()
	require.NotEmpty(t, v)
	joined := strings.Join(v, "\n")
	assert.Contains(t, joined, "8 scored samples (want 80)")
	assert.Contains(t, joined, "level BEGINNER has 2 samples (want 20)")
	assert.Contains(t, joined, "1 insufficient-evidence samples (want 8)")
}

func TestCheckComplete_CompleteDataset(t *testing.T) {
	ds := completeDataset()
	assert.Empty(t, ds.CheckComplete())

	// every rule on its own
	cases := map[string]func(*Dataset){
		"scored samples":        func(d *Dataset) { d.Samples = d.Samples[1:] },
		"scenario kinds":        func(d *Dataset) { setKind(d, "BEGINNER", "daily") },
		"start_mode AI_FIRST":   func(d *Dataset) { setMode(d, "USER_FIRST") },
		"insufficient-evidence": func(d *Dataset) { d.Samples = d.Samples[:len(d.Samples)-1] },
		"user turns":            func(d *Dataset) { d.Samples[0].Turns = d.Samples[0].Turns[:4] },
		"has 16 user turns":     func(d *Dataset) { d.Samples[0].Turns = turns(16) },
	}
	for want, mutate := range cases {
		d := completeDataset()
		mutate(&d)
		assert.Contains(t, strings.Join(d.CheckComplete(), "\n"), want, want)
	}
}

func writeDataset(t *testing.T, manifest, jsonl string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600))
	if jsonl != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(jsonl), 0o600))
	}
	return dir
}

const okManifest = `{"rubric_version":"rubric-v1","status":"EXAMPLE","expected":{}}`

func TestLoadDataset_Errors(t *testing.T) {
	ins := `{"id":"a","level":"BEGINNER","insufficient":true,"start_mode":"USER_FIRST","duration_seconds":10,"scenario":{},"turns":[],"gold":{}}`
	cases := map[string]struct{ manifest, jsonl, want string }{
		"no manifest":    {"", "", "read manifest"},
		"bad status":     {`{"status":"DONE"}`, ins, "must be EXAMPLE or COMPLETE"},
		"unknown field":  {okManifest, strings.Replace(ins, `"id":"a"`, `"id":"a","extra":1`, 1), "unknown field"},
		"bad json":       {okManifest, "{", "s.jsonl:1"},
		"bad level":      {okManifest, strings.Replace(ins, "BEGINNER", "EXPERT", 1), "level"},
		"bad start mode": {okManifest, strings.Replace(ins, "USER_FIRST", "NEITHER", 1), "start_mode"},
		"empty id":       {okManifest, strings.Replace(ins, `"id":"a"`, `"id":" "`, 1), "id is empty"},
		"duplicate id":   {okManifest, ins + "\n" + ins, "duplicate sample id"},
		"no samples":     {okManifest, "\n", "no samples"},
		"flag mismatch":  {okManifest, strings.Replace(ins, `"insufficient":true`, `"insufficient":false`, 1), "insufficient=false"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeDataset(t, c.manifest, c.jsonl)
			if c.manifest == "" {
				require.NoError(t, os.Remove(filepath.Join(dir, "manifest.json")))
			}
			_, err := LoadDataset(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

func TestLoadDataset_ScoredSampleNeedsGold(t *testing.T) {
	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	var line string
	{
		b, err := os.ReadFile(filepath.Join(exampleDir, "example.jsonl"))
		require.NoError(t, err)
		line = strings.SplitN(string(b), "\n", 2)[0]
	}
	require.NotEmpty(t, ds.Samples)
	cases := map[string]string{
		"missing rater2 dim": strings.Replace(line, `"FLUENCY": 35, `, ``, 1),
		"score above 100":    strings.Replace(line, `"GRAMMAR": 45`, `"GRAMMAR": 145`, 1),
		"unknown major type": strings.Replace(line, "PARTICLE_ERROR", "NOT_A_TYPE", 1),
	}
	for name, l := range cases {
		require.NotEqual(t, line, l, name)
		_, err := LoadDataset(writeDataset(t, okManifest, l))
		require.Error(t, err, name)
	}
	_, err = LoadDataset(writeDataset(t, okManifest, line))
	require.NoError(t, err)
}
