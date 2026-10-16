//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func compileSchema(t *testing.T, raw []byte) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	require.NoError(t, c.AddResource("schema.json", doc))
	s, err := c.Compile("schema.json")
	require.NoError(t, err)
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, raw string) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	require.NoError(t, err)
	return s.Validate(inst)
}

func TestEvaluationSeedPrompt(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	ctx := context.Background()

	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptEvaluation)
	require.NoError(t, err)
	require.Equal(t, 1, pv.Version)
	require.Equal(t, content.PromptEvaluation, pv.Key)
	require.NotNil(t, pv.UserTemplate)
	require.NotEmpty(t, pv.SystemTemplate)
	require.NotNil(t, pv.LLMParams.Temperature)
	require.InDelta(t, 0, *pv.LLMParams.Temperature, 1e-9)
	require.NotNil(t, pv.LLMParams.MaxOutputTokens)
	require.Equal(t, 4000, *pv.LLMParams.MaxOutputTokens)
	require.NotNil(t, pv.LLMParams.Seed)
	require.EqualValues(t, 42, *pv.LLMParams.Seed)
	require.Nil(t, pv.LLMParams.TopP)
	require.Equal(t, "rubric-v1", evaluation.RubricVersionOf(pv))

	schema := compileSchema(t, pv.OutputSchema)
	require.NoError(t, validate(t, schema, evaluation.ExampleOutput))
	// the schema is not vacuous: out-of-range score and unknown finding type are rejected
	bad := strings.Replace(evaluation.ExampleOutput, `"score": 52`, `"score": 101`, 1)
	require.NotEqual(t, evaluation.ExampleOutput, bad)
	require.Error(t, validate(t, schema, bad))
	bad = strings.Replace(evaluation.ExampleOutput, `"PARTICLE_ERROR"`, `"NOPE"`, 1)
	require.Error(t, validate(t, schema, bad))

	fixtures := evaluation.PromptFixtures()
	require.Len(t, fixtures, 2)
	for i, fx := range fixtures {
		out, err := prompt.Render(pv.Template(), fx)
		require.NoError(t, err, "fixture %d", i)
		require.Contains(t, out.System, "RUBRIC (rubric-v1)", "fixture %d", i)
		require.NotContains(t, out.System, "{{", "fixture %d", i)
		require.NotContains(t, out.User, "{{", "fixture %d", i)
		require.Contains(t, out.User, "## Transcript", "fixture %d", i)
		data := fx.(evaluation.EvaluationPromptData)
		require.Contains(t, out.System, "PARTICLE_ERROR -> GRAMMAR; turn_seq, original, corrected required")
		require.Contains(t, out.User, "Expected politeness: "+data.ExpectedFormality)
		require.Contains(t, out.User, data.Transcript)
		require.Contains(t, out.User, "Topic: "+data.TopicTitle)
		require.Contains(t, out.User, "Scenario: "+data.ScenarioTitle+" (difficulty level "+strconv.Itoa(data.DifficultyLevel)+" of 5)")
		require.Contains(t, out.User, "Learner role: "+data.LearnerRole)
		require.Contains(t, out.User, "AI role: "+data.AIRole+" (character: "+data.CharacterName+")")
		require.Contains(t, out.User, "Context: "+data.ScenarioContext)
		require.Contains(t, out.User, "USER turns: "+strconv.Itoa(data.UserTurnCount)+" | total USER speech: "+
			strconv.Itoa(data.UserSpeechSeconds)+" s | session length: "+strconv.Itoa(data.ActiveSeconds)+" s")
		for _, o := range append(append([]string{}, data.Objectives...), data.EvaluationFocus...) {
			require.Contains(t, out.User, "- "+o+"\n")
		}
	}

	errs, _ := errorsAndWarnings(validatorFor(pool, false).ValidateCatalog(ctx))
	require.Empty(t, errs)

	testutil.ApplySeed(t, pool)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id WHERE pc.key = 'EVALUATION'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestSchemaCopyMatchesSeed(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(context.Background(), content.PromptEvaluation)
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), "internal", "evaluation", "testdata", "output_schema.json"))
	require.NoError(t, err)
	require.True(t, json.Valid(b))
	require.JSONEq(t, string(b), string(pv.OutputSchema))
}
