//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/evaluation/golden"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// TestEvalGoldenExampleDataset runs the calibration tool over the synthetic example set with the
// seeded EVALUATION prompt version from Postgres and the fake evaluation provider: the report is
// written, has the five MAE metrics and is never evaluable (status EXAMPLE).
func TestEvalGoldenExampleDataset(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	ctx := context.Background()
	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptEvaluation)
	require.NoError(t, err)

	fakeEval := fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{})
	ev := golden.NewLLMEvaluator(pv, fakeEval, evaluation.NewSchemaCache(), evaluation.DefaultConfig().MinFindingConfidence)

	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := golden.Main([]string{
		"--dataset", filepath.Join(testutil.ModuleRoot(t), "testdata", "benchmark", "evaluation", "rubric-v1"),
		"--runs", "3", "--out", out,
	}, &stdout, &stderr, golden.MainDeps{Build: func(context.Context) (golden.Evaluator, func(), error) { return ev, nil, nil }})
	require.Equal(t, 0, code, stderr.String())

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	var rep struct {
		DatasetStatus string                    `json:"dataset_status"`
		RubricVersion string                    `json:"rubric_version"`
		Verdict       string                    `json:"verdict"`
		Runs          int                       `json:"runs"`
		Metrics       map[string]map[string]any `json:"metrics"`
	}
	require.NoError(t, json.Unmarshal(raw, &rep))
	assert.Equal(t, "EXAMPLE", rep.DatasetStatus)
	assert.Equal(t, "rubric-v1", rep.RubricVersion)
	assert.Equal(t, "NOT_EVALUABLE_EXAMPLE_DATASET", rep.Verdict)
	assert.Equal(t, 3, rep.Runs)
	for _, d := range evaluation.LLMDimensions {
		assert.Contains(t, rep.Metrics, "MAE."+string(d))
	}
	assert.Equal(t, 1.0, rep.Metrics["FIRST_CALL_VALIDITY"]["value"], "the fake output passes the validator on the first call")
	assert.Contains(t, stdout.String(), "verdict: NOT_EVALUABLE_EXAMPLE_DATASET")
}
