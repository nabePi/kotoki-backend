package golden

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

// spyEval wraps the fake provider and keeps the requests it saw.
type spyEval struct {
	provider.EvaluationProvider
	reqs []provider.StructuredRequest
}

func (s *spyEval) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	s.reqs = append(s.reqs, req)
	return s.EvaluationProvider.GenerateJSON(ctx, req)
}

func testPV(t *testing.T) content.PromptVersion {
	t.Helper()
	schema, err := os.ReadFile("../testdata/output_schema.json")
	require.NoError(t, err)
	user := "{{.RubricVersion}} {{.ScenarioTitle}} {{.ExpectedFormality}}\n{{.Transcript}}"
	return content.PromptVersion{ID: uuid.MustParse("00000000-0000-0000-0000-0000000000a1"), Key: content.PromptEvaluation, Version: 1,
		SystemTemplate: "SYS", UserTemplate: &user, OutputSchema: schema}
}

func TestNewLLMEvaluator_BuildsThePromptLikeTheWorker(t *testing.T) {
	spy := &spyEval{EvaluationProvider: fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{})}
	ev := NewLLMEvaluator(testPV(t), spy, evaluation.NewSchemaCache(), 0.6)

	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	s := ds.Samples[4] // int-job-1, KEIGO_LIGHT
	res, err := ev(context.Background(), s, 0)
	require.NoError(t, err)

	require.Len(t, spy.reqs, 1)
	req := spy.reqs[0]
	assert.Equal(t, "EVALUATION:"+sampleSessionID(s.ID).String(), req.RequestID)
	assert.Equal(t, "kotoki_evaluation_rubric_v1", req.SchemaName)
	user := req.Messages[len(req.Messages)-1].Content
	assert.Contains(t, user, "rubric-v1 Part-time job interview")
	assert.Contains(t, user, "fixed keigo phrases", "formality of the scenario is rendered")
	assert.Contains(t, user, "[T2] USER (7.0 s,", "the transcript has the worker's format")
	assert.Contains(t, user, "もともと本が好きで")

	assert.True(t, res.FirstCallValid)
	require.Len(t, res.Scores, 5)
	for _, d := range dims() {
		assert.Contains(t, res.Scores, d)
	}
	assert.NotEmpty(t, res.Findings, "the fake derives findings from the transcript")
	for _, f := range res.Findings {
		assert.NotEmpty(t, f.Type)
		assert.NotEmpty(t, f.Dimension)
	}
}

func TestNewLLMEvaluator_RunOnExampleDatasetWithFake(t *testing.T) {
	ev := NewLLMEvaluator(testPV(t), fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{}), evaluation.NewSchemaCache(), 0.6)
	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	rep, err := Run(context.Background(), ds, ev, 3)
	require.NoError(t, err)
	assert.Equal(t, VerdictNotEvaluable, rep.Verdict)
	assert.InDelta(t, 1, rep.Metrics[MetricFirstCallValid].Value, 1e-9)
	assert.InDelta(t, 0, rep.Metrics[MetricStability].Value, 1e-9, "the fake is deterministic per request")
	assert.NotEmpty(t, rep.Findings)
}

func TestNewLLMEvaluator_TemplateAndProviderErrors(t *testing.T) {
	ds, err := LoadDataset(exampleDir)
	require.NoError(t, err)
	s := ds.Samples[0]

	bad := testPV(t)
	broken := "{{.NoSuchField}}"
	bad.UserTemplate = &broken
	_, err = NewLLMEvaluator(bad, fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{}), evaluation.NewSchemaCache(), 0.6)(context.Background(), s, 0)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "NoSuchField"))

	noSchema := testPV(t)
	noSchema.OutputSchema = []byte(`{"type":`)
	_, err = NewLLMEvaluator(noSchema, fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{}), evaluation.NewSchemaCache(), 0.6)(context.Background(), s, 0)
	require.Error(t, err)
}
