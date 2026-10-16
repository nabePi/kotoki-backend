package evaluation_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

type scriptedEval struct {
	steps []func(provider.StructuredRequest) (provider.StructuredResult, error)
	reqs  []provider.StructuredRequest
}

func (s *scriptedEval) Name() provider.Name { return provider.OpenAI }

func (s *scriptedEval) GenerateJSON(_ context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	s.reqs = append(s.reqs, req)
	if len(s.reqs) > len(s.steps) {
		return provider.StructuredResult{}, errors.New("scriptedEval: unexpected extra call")
	}
	return s.steps[len(s.reqs)-1](req)
}

func okJSON(raw string) func(provider.StructuredRequest) (provider.StructuredResult, error) {
	return func(provider.StructuredRequest) (provider.StructuredResult, error) {
		return provider.StructuredResult{JSON: json.RawMessage(raw), Provider: provider.OpenAI, Model: "gpt-x",
			Usage: provider.TokenUsage{InputTokens: 100, OutputTokens: 40}}, nil
	}
}

func failWith(err error) func(provider.StructuredRequest) (provider.StructuredResult, error) {
	return func(provider.StructuredRequest) (provider.StructuredResult, error) {
		return provider.StructuredResult{}, err
	}
}

func llmFixture(t *testing.T) (evaluation.LLMInput, *evaluation.SchemaCache) {
	t.Helper()
	schema, err := os.ReadFile("testdata/output_schema.json")
	require.NoError(t, err)
	sid := uuid.New()
	return evaluation.LLMInput{
		SessionID: sid,
		PV: content.PromptVersion{ID: uuid.New(), Key: content.PromptEvaluation, Version: 1,
			OutputSchema: schema},
		Rendered: prompt.Rendered{System: "SYS", User: "USR"},
		Turns: []evaluation.TurnRef{
			{ID: uuid.New(), Seq: 1, UserText: "昨日、会社に働いていました。"},
			{ID: uuid.New(), Seq: 2, UserText: "とても忙しいでした。"},
		},
	}, evaluation.NewSchemaCache()
}

func invalidSchemaJSON(t *testing.T) string {
	t.Helper()
	return string(mutateOutput(t, func(m map[string]any) { delete(m, "summary") }))
}

func TestEvaluateValidFirstCall(t *testing.T) {
	in, cache := llmFixture(t)
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(evaluation.ExampleOutput)}}
	e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

	res, err := e.Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 1, res.CallCount)
	require.Equal(t, provider.OpenAI, res.Provider)
	require.Equal(t, "gpt-x", res.Model)
	require.Equal(t, 100, res.InputTokens)
	require.Equal(t, 40, res.OutputTokens)
	require.Len(t, res.Findings, 2)
	require.Equal(t, 52, res.Output.Scores.Grammar.Score)

	req := sc.reqs[0]
	require.Equal(t, "EVALUATION", req.Task)
	require.Equal(t, "kotoki_evaluation_rubric_v1", req.SchemaName)
	require.Equal(t, "EVALUATION:"+in.SessionID.String(), req.RequestID)
	require.Equal(t, 4000, req.MaxOutputTokens)
	require.Zero(t, req.Temperature)
	require.NotNil(t, req.Seed)
	require.EqualValues(t, 42, *req.Seed)
	require.JSONEq(t, string(in.PV.OutputSchema), string(req.JSONSchema))
	require.Equal(t, []provider.Message{
		{Role: provider.RoleSystem, Content: "SYS"}, {Role: provider.RoleUser, Content: "USR"}}, req.Messages)
}

func TestBuildStructuredRequestDBParamsWin(t *testing.T) {
	in, _ := llmFixture(t)
	temp, tokens, seed := 0.3, 3000, int64(7)
	in.PV.LLMParams = prompt.Params{Temperature: &temp, MaxOutputTokens: &tokens, Seed: &seed}
	in.PV.Version = 2
	req := evaluation.BuildStructuredRequest(in.SessionID, in.PV, nil)
	require.Equal(t, 3000, req.MaxOutputTokens)
	require.Equal(t, 0.3, req.Temperature)
	require.EqualValues(t, 7, *req.Seed)
	require.Equal(t, "kotoki_evaluation_rubric_v2", req.SchemaName)

	zero := 0.0
	in.PV.LLMParams = prompt.Params{Temperature: &zero}
	req = evaluation.BuildStructuredRequest(in.SessionID, in.PV, nil)
	require.Zero(t, req.Temperature)
	require.Equal(t, 4000, req.MaxOutputTokens)
}

func TestEvaluateRepairAfterSchemaFailure(t *testing.T) {
	in, cache := llmFixture(t)
	bad := invalidSchemaJSON(t)
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		okJSON(bad), okJSON(evaluation.ExampleOutput)}}
	e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

	res, err := e.Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 2, res.CallCount)
	require.Equal(t, 200, res.InputTokens, "tokens of both calls are summed")
	require.Equal(t, 80, res.OutputTokens)

	msgs := sc.reqs[1].Messages
	require.Len(t, msgs, 4)
	require.Equal(t, sc.reqs[0].Messages, msgs[:2])
	require.Equal(t, provider.RoleAssistant, msgs[2].Role)
	require.Equal(t, bad, msgs[2].Content)
	require.Equal(t, provider.RoleUser, msgs[3].Role)
	require.True(t, strings.HasPrefix(msgs[3].Content, "Your previous output was rejected by the validator. Errors:\n- "))
	require.Contains(t, msgs[3].Content, "summary")
	require.True(t, strings.HasSuffix(msgs[3].Content, "Return the complete corrected JSON object only. Follow the schema and all rules exactly."))
}

func TestRepairMessagesTruncatesPrevious(t *testing.T) {
	base := []provider.Message{{Role: provider.RoleSystem, Content: "s"}}
	prev := strings.Repeat("あ", 9000)
	msgs := evaluation.RepairMessages(base, prev, []string{"a", "b"})
	require.Len(t, msgs, 3)
	require.Len(t, []rune(msgs[1].Content), 8000)
	require.Equal(t, "Your previous output was rejected by the validator. Errors:\n- a\n- b\nReturn the complete corrected JSON object only. Follow the schema and all rules exactly.", msgs[2].Content)

	exact := evaluation.RepairMessages(base, strings.Repeat("あ", 8000), nil)
	require.Len(t, []rune(exact[1].Content), 8000)

	none := evaluation.RepairMessages(base, "", []string{"x"})
	require.Len(t, none, 2, "no assistant message without a previous output")
}

func TestEvaluateTwoInvalidOutputsStopAtTwoCalls(t *testing.T) {
	in, cache := llmFixture(t)
	bad := invalidSchemaJSON(t)
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(bad), okJSON(bad)}}
	e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

	_, err := e.Evaluate(t.Context(), in)
	require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
	require.Len(t, sc.reqs, 2, "no third call")
}

func TestEvaluateProviderErrorsReturnedAsIs(t *testing.T) {
	for _, kind := range []provider.ErrorKind{provider.KindUnavailable, provider.KindContentFiltered, provider.KindTimeout, provider.KindAuth} {
		t.Run(string(kind), func(t *testing.T) {
			in, cache := llmFixture(t)
			perr := provider.NewError(provider.OpenAI, provider.CapEvaluation, kind, "boom")
			sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){failWith(perr)}}
			e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

			_, err := e.Evaluate(t.Context(), in)
			require.Same(t, perr, err)
			require.Len(t, sc.reqs, 1, "no repair and no fallback")
		})
	}
}

func TestEvaluateErrorOnRepairCallReturned(t *testing.T) {
	in, cache := llmFixture(t)
	perr := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "down")
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		okJSON(invalidSchemaJSON(t)), failWith(perr)}}
	e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

	_, err := e.Evaluate(t.Context(), in)
	require.Same(t, perr, err)
	require.NotErrorIs(t, err, evaluation.ErrInvalidModelOutput)
}

func TestEvaluateInvalidResponseTriggersRepair(t *testing.T) {
	in, cache := llmFixture(t)
	invalid := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindInvalidResponse, "not json")
	invalid.Retryable = true
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		failWith(invalid), okJSON(evaluation.ExampleOutput)}}
	e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

	res, err := e.Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 2, res.CallCount)
	require.Equal(t, 100, res.InputTokens, "only the successful call reports usage")
	require.Len(t, sc.reqs[1].Messages, 3, "base + repair user message; no assistant echo without output")
	require.Equal(t, provider.RoleUser, sc.reqs[1].Messages[2].Role)
}

func TestEvaluateInvalidResponseTwiceIsInvalidModelOutput(t *testing.T) {
	in, cache := llmFixture(t)
	invalid := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindInvalidResponse, "not json")
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){failWith(invalid), failWith(invalid)}}
	e := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6)

	_, err := e.Evaluate(t.Context(), in)
	require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
	require.Len(t, sc.reqs, 2)
}

func TestEvaluateSchemaEnforcedInCode(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"unknown property": func(m map[string]any) { m["extra"] = 1 },
		"score out of range": func(m map[string]any) {
			m["scores"].(map[string]any)["GRAMMAR"].(map[string]any)["score"] = 101
		},
		"enum violated": func(m map[string]any) {
			m["findings"].([]any)[0].(map[string]any)["finding_type"] = "UNKNOWN_TYPE"
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			in, cache := llmFixture(t)
			raw := string(mutateOutput(t, mut))
			require.True(t, json.Valid([]byte(raw)))
			sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
				okJSON(raw), okJSON(evaluation.ExampleOutput)}}
			res, err := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6).Evaluate(t.Context(), in)
			require.NoError(t, err)
			require.Equal(t, 2, res.CallCount, "json.Valid output still rejected by the schema")
		})
	}
}

func TestEvaluateLanguageViolationTriggersRepair(t *testing.T) {
	in, cache := llmFixture(t)
	jp := string(mutateOutput(t, func(m map[string]any) {
		m["summary"] = "会社で働いていました。今日はとても忙しい日でした。"
	}))
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		okJSON(jp), okJSON(evaluation.ExampleOutput)}}
	res, err := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6).Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 2, res.CallCount)
	require.Contains(t, sc.reqs[1].Messages[len(sc.reqs[1].Messages)-1].Content, "summary")
}

func TestEvaluateTooManyRejectedFindingsTriggersRepair(t *testing.T) {
	in, cache := llmFixture(t)
	// Both findings quote text that is not in the transcript: 2 of 2 rejected.
	hallucinated := string(mutateOutput(t, func(m map[string]any) {
		for _, f := range m["findings"].([]any) {
			f.(map[string]any)["original"] = "存在しない文章です"
		}
	}))
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		okJSON(hallucinated), okJSON(evaluation.ExampleOutput)}}
	res, err := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6).Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 2, res.CallCount)
	require.Len(t, res.Findings, 2)
	last := sc.reqs[1].Messages[len(sc.reqs[1].Messages)-1].Content
	require.Contains(t, last, "original_not_found=2")
	require.NotContains(t, last, "存在しない", "finding text is never echoed in the repair errors")

	// Still too many rejected on the repair call: invalid output, no third call.
	sc = &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		okJSON(hallucinated), okJSON(hallucinated)}}
	_, err = evaluation.NewLLMEvaluator(sc, cache, nil, 0.6).Evaluate(t.Context(), in)
	require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
	require.Len(t, sc.reqs, 2)
}

func TestEvaluateOneRejectedFindingOfTwoIsKept(t *testing.T) {
	in, cache := llmFixture(t)
	partial := string(mutateOutput(t, func(m map[string]any) {
		m["findings"].([]any)[0].(map[string]any)["original"] = "存在しない文章です"
	}))
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(partial)}}
	res, err := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6).Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 1, res.CallCount, "exactly half rejected is not more than half")
	require.Len(t, res.Findings, 1)
	require.Equal(t, 1, res.Dropped["original_not_found"])
}

func TestEvaluateMinConfidenceIsPassedToFilter(t *testing.T) {
	in, cache := llmFixture(t)
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(evaluation.ExampleOutput)}}
	res, err := evaluation.NewLLMEvaluator(sc, cache, nil, 0.96).Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, res.Findings, 1, "the 0.95 finding is below 0.96")
	require.Equal(t, 1, res.Dropped["low_confidence"])
}

func TestEvaluateMetrics(t *testing.T) {
	in, cache := llmFixture(t)
	reg := prometheus.NewRegistry()
	m := evaluation.NewMetrics(reg)
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		okJSON(invalidSchemaJSON(t)), okJSON(evaluation.ExampleOutput)}}
	_, err := evaluation.NewLLMEvaluator(sc, cache, m, 0.6).Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 1.0, counter(t, reg, "kotoki_evaluation_llm_calls_total", map[string]string{"provider": "OPENAI", "result": "invalid"}))
	require.Equal(t, 1.0, counter(t, reg, "kotoki_evaluation_llm_calls_total", map[string]string{"provider": "OPENAI", "result": "valid"}))

	reg = prometheus.NewRegistry()
	m = evaluation.NewMetrics(reg)
	sc = &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		failWith(provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "x"))}}
	_, err = evaluation.NewLLMEvaluator(sc, cache, m, 0.6).Evaluate(t.Context(), in)
	require.Error(t, err)
	require.Equal(t, 1.0, counter(t, reg, "kotoki_evaluation_llm_calls_total", map[string]string{"provider": "OPENAI", "result": "error"}))
}

func TestEvaluateBrokenSchemaIsReturned(t *testing.T) {
	in, cache := llmFixture(t)
	in.PV.OutputSchema = json.RawMessage(`{"type": 5}`)
	sc := &scriptedEval{}
	_, err := evaluation.NewLLMEvaluator(sc, cache, nil, 0.6).Evaluate(t.Context(), in)
	require.Error(t, err)
	require.NotErrorIs(t, err, evaluation.ErrInvalidModelOutput)
	require.Empty(t, sc.reqs)
}

func TestEvaluateCountsDroppedFindings(t *testing.T) {
	in, cache := llmFixture(t)
	reg := prometheus.NewRegistry()
	partial := string(mutateOutput(t, func(m map[string]any) {
		m["findings"].([]any)[0].(map[string]any)["original"] = "存在しない文章です"
	}))
	sc := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(partial)}}
	_, err := evaluation.NewLLMEvaluator(sc, cache, evaluation.NewMetrics(reg), 0.6).Evaluate(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, 1.0, counter(t, reg, "kotoki_evaluation_findings_dropped_total", map[string]string{"reason": "original_not_found"}))
}
