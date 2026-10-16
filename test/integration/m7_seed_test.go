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
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/summary"
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

func TestFakeRubricFixtureMatchesSeedSchema(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	ctx := context.Background()
	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptEvaluation)
	require.NoError(t, err)
	schema := compileSchema(t, pv.OutputSchema)

	texts := []string{"今日は会社で働きます", "昨日は友達と映画を見ました", "私は学生です", "毎日電車で通います", "週末はよく散歩します", "来週は旅行に行きたいです"}
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var turns []evaluation.TurnRow
	for i, text := range texts {
		status, user, ai := "COMPLETED", text, "そうですか。"
		start := base.Add(time.Duration(i) * 10 * time.Second)
		end := start.Add(3 * time.Second)
		turns = append(turns, evaluation.TurnRow{
			Seq: i + 1, Status: &status, UserText: &user, AssistantText: &ai, UserStartedAt: &start, UserEndedAt: &end,
		})
	}
	data := evaluation.PromptFixtures()[0].(evaluation.EvaluationPromptData)
	data.Transcript = evaluation.BuildTranscript(turns, 0)
	out, err := prompt.Render(pv.Template(), data)
	require.NoError(t, err)

	ev := fake.NewEvaluation(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{Seed: 42})
	res, err := ev.GenerateJSON(ctx, provider.StructuredRequest{
		Task: "EVALUATION", SchemaName: "kotoki_evaluation_rubric_v1", RequestID: "EVALUATION:s1",
		Messages: []provider.Message{{Role: provider.RoleSystem, Content: out.System}, {Role: provider.RoleUser, Content: out.User}},
	})
	require.NoError(t, err)
	require.NoError(t, validate(t, schema, string(res.JSON)))

	var doc struct {
		Findings []struct {
			TurnSeq  int    `json:"turn_seq"`
			Original string `json:"original"`
		} `json:"findings"`
	}
	require.NoError(t, json.Unmarshal(res.JSON, &doc))
	require.Len(t, doc.Findings, 2)
	require.Equal(t, 3, doc.Findings[0].TurnSeq)
	require.Equal(t, 6, doc.Findings[1].TurnSeq)
	require.Contains(t, data.Transcript, doc.Findings[0].Original)
}

func TestSummarySeedPrompt(t *testing.T) {
	pool := testutil.NewPostgres(t)
	testutil.ApplySeed(t, pool)
	ctx := context.Background()

	pv, err := postgres.NewContentRepo(pool).GetPublishedPromptVersion(ctx, content.PromptSummary)
	require.NoError(t, err)
	require.Equal(t, 1, pv.Version)
	require.Equal(t, content.PromptSummary, pv.Key)
	require.Equal(t, "01920000-000d-7000-8000-000000000003", pv.ID.String())
	require.Equal(t, content.VersionPublished, pv.Status)
	require.NotNil(t, pv.UserTemplate)
	require.Empty(t, pv.OutputSchema)
	require.NotNil(t, pv.LLMParams.Temperature)
	require.InDelta(t, 0.3, *pv.LLMParams.Temperature, 1e-9)
	require.NotNil(t, pv.LLMParams.MaxOutputTokens)
	require.Equal(t, 400, *pv.LLMParams.MaxOutputTokens)
	require.Nil(t, pv.LLMParams.TopP)
	require.Nil(t, pv.LLMParams.Seed)

	fixtures := summary.PromptFixtures()
	require.Len(t, fixtures, 2)
	for i, fx := range fixtures {
		out, err := prompt.Render(pv.Template(), fx)
		require.NoError(t, err, "fixture %d", i)
		data := fx.(summary.SummaryPromptData)
		require.Contains(t, out.System, "300文字以内", "fixture %d", i)
		require.NotContains(t, out.System, "{{")
		require.Contains(t, out.User, "話題: "+data.TopicName+"\n場面: "+data.ScenarioName+"\n")
		require.Contains(t, out.User, "ユーザーの発話数: "+strconv.Itoa(data.UserTurnCount)+"\n")
		require.True(t, strings.HasSuffix(out.User, "</transcript>"), "fixture %d", i)
		require.Contains(t, out.User, "<transcript>\n"+data.Transcript+"\n</transcript>", "fixture %d", i)
	}
	custom := fixtures[0].(summary.SummaryPromptData)
	custom.MaxChars = 120
	out, err := prompt.Render(pv.Template(), custom)
	require.NoError(t, err)
	require.Contains(t, out.System, "120文字以内")

	errs, _ := errorsAndWarnings(validatorFor(pool, false).ValidateCatalog(ctx))
	require.Empty(t, errs)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id WHERE pc.key = 'SUMMARY'`).Scan(&n))
	require.Equal(t, 1, n)
}
