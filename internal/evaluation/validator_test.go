package evaluation_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

func loadSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	b, err := os.ReadFile("testdata/output_schema.json")
	require.NoError(t, err)
	s, err := evaluation.NewSchemaCache().Get(uuid.New(), b)
	require.NoError(t, err)
	return s
}

func mutateOutput(t *testing.T, f func(m map[string]any)) json.RawMessage {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(evaluation.ExampleOutput), &m))
	f(m)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

func TestParseOutputRules(t *testing.T) {
	schema := loadSchema(t)
	scoreOf := func(m map[string]any, dim string) map[string]any {
		return m["scores"].(map[string]any)[dim].(map[string]any)
	}
	tests := []struct {
		name    string
		mutate  func(m map[string]any)
		problem string // substring of a problem; "" = valid
		check   func(t *testing.T, out evaluation.EvalOutput)
	}{
		{name: "example output is valid", mutate: func(map[string]any) {}, check: func(t *testing.T, out evaluation.EvalOutput) {
			require.Equal(t, 52, out.Scores.Grammar.Score)
			require.Equal(t, 62, out.Signals.ListeningResponse.Score)
			require.Len(t, out.Findings, 2)
			require.Equal(t, "PARTICLE_ERROR", out.Findings[0].FindingType)
			require.NotNil(t, out.Findings[0].TurnSeq)
			require.Equal(t, 1, *out.Findings[0].TurnSeq)
			require.NotNil(t, out.Recommendations[0].ExampleJA)
		}},
		{name: "missing summary", mutate: func(m map[string]any) { delete(m, "summary") }, problem: "summary"},
		{name: "score above 100", mutate: func(m map[string]any) { scoreOf(m, "GRAMMAR")["score"] = 101 }, problem: "/scores/GRAMMAR/score"},
		{name: "negative score", mutate: func(m map[string]any) { scoreOf(m, "FLUENCY")["score"] = -1 }, problem: "/scores/FLUENCY/score"},
		{name: "unknown finding type", mutate: func(m map[string]any) {
			m["findings"].([]any)[0].(map[string]any)["finding_type"] = "UNKNOWN_TYPE"
		}, problem: "/findings/0/finding_type"},
		{name: "unknown top-level property", mutate: func(m map[string]any) { m["extra"] = 1 }, problem: "extra"},
		{name: "unknown nested property", mutate: func(m map[string]any) { scoreOf(m, "GRAMMAR")["extra"] = 1 }, problem: "extra"},
		{name: "non-integer score is rounded", mutate: func(m map[string]any) { scoreOf(m, "GRAMMAR")["score"] = 85.4 }, check: func(t *testing.T, out evaluation.EvalOutput) {
			require.Equal(t, 85, out.Scores.Grammar.Score)
		}},
		{name: "half rounds up", mutate: func(m map[string]any) { scoreOf(m, "VOCABULARY")["score"] = 85.5 }, check: func(t *testing.T, out evaluation.EvalOutput) {
			require.Equal(t, 86, out.Scores.Vocabulary.Score)
		}},
		{name: "signal score is rounded", mutate: func(m map[string]any) {
			m["signals"].(map[string]any)["LISTENING_RESPONSE"].(map[string]any)["score"] = 61.6
		}, check: func(t *testing.T, out evaluation.EvalOutput) {
			require.Equal(t, 62, out.Signals.ListeningResponse.Score)
		}},
		{name: "rounding up past 100 is invalid", mutate: func(m map[string]any) { scoreOf(m, "GRAMMAR")["score"] = 100.6 }, problem: "/scores/GRAMMAR/score"},
		{name: "integral turn_seq float", mutate: func(m map[string]any) {
			m["findings"].([]any)[0].(map[string]any)["turn_seq"] = 3.0
		}, check: func(t *testing.T, out evaluation.EvalOutput) { require.Equal(t, 3, *out.Findings[0].TurnSeq) }},
		{name: "fractional turn_seq", mutate: func(m map[string]any) {
			m["findings"].([]any)[0].(map[string]any)["turn_seq"] = 1.5
		}, problem: "/findings/0/turn_seq"},
		{name: "empty strengths", mutate: func(m map[string]any) { m["strengths"] = []any{} }, problem: "/strengths"},
		{name: "too many findings", mutate: func(m map[string]any) {
			f := m["findings"].([]any)[0]
			var many []any
			for range 19 {
				many = append(many, f)
			}
			m["findings"] = many
		}, problem: "/findings"},
		{name: "bad pattern_key", mutate: func(m map[string]any) {
			m["findings"].([]any)[0].(map[string]any)["pattern_key"] = "Bad Key"
		}, problem: "pattern_key"},
		{name: "null pattern_key and turn_seq allowed", mutate: func(m map[string]any) {
			f := m["findings"].([]any)[0].(map[string]any)
			f["pattern_key"], f["turn_seq"] = nil, nil
		}, check: func(t *testing.T, out evaluation.EvalOutput) {
			require.Nil(t, out.Findings[0].PatternKey)
			require.Nil(t, out.Findings[0].TurnSeq)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evaluation.ParseOutput(schema, mutateOutput(t, tc.mutate))
			if tc.problem == "" {
				require.NoError(t, err)
				if tc.check != nil {
					tc.check(t, out)
				}
				return
			}
			require.Error(t, err)
			require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
			var oe *evaluation.OutputError
			require.True(t, errors.As(err, &oe))
			require.NotEmpty(t, oe.Problems)
			require.Contains(t, strings.Join(oe.Problems, "\n"), tc.problem)
		})
	}
}

func TestParseOutputNotJSON(t *testing.T) {
	schema := loadSchema(t)
	for _, raw := range []string{"", "not json", `{"scores":`} {
		_, err := evaluation.ParseOutput(schema, json.RawMessage(raw))
		require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput, raw)
	}
	_, err := evaluation.ParseOutput(schema, json.RawMessage(`[]`))
	require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
}

func validOutput(t *testing.T) evaluation.EvalOutput {
	t.Helper()
	out, err := evaluation.ParseOutput(loadSchema(t), json.RawMessage(evaluation.ExampleOutput))
	require.NoError(t, err)
	return out
}

func TestCheckLanguage(t *testing.T) {
	require.Empty(t, evaluation.CheckLanguage(validOutput(t)), "example output is Indonesian with Japanese quotations")

	tests := []struct {
		name    string
		mutate  func(o *evaluation.EvalOutput)
		problem string
	}{
		{"summary Japanese only", func(o *evaluation.EvalOutput) {
			o.Summary = "今日は会社で働きましたので、とても疲れました。"
		}, "summary"},
		{"summary Indonesian with quoted Japanese", func(o *evaluation.EvalOutput) {
			o.Summary = "Kamu menulis 「会社で働く」 dengan benar dan alami sekali."
		}, ""},
		{"comment empty after removing CJK", func(o *evaluation.EvalOutput) { o.Scores.Fluency.Comment = "「会社で働く」" }, "scores.FLUENCY.comment"},
		{"comment Japanese only", func(o *evaluation.EvalOutput) { o.Scores.Grammar.Comment = "文法はとても良いです。" }, "scores.GRAMMAR.comment"},
		{"vocabulary comment Japanese only", func(o *evaluation.EvalOutput) { o.Scores.Vocabulary.Comment = "語彙が少ないです。" }, "scores.VOCABULARY.comment"},
		{"naturalness comment Japanese only", func(o *evaluation.EvalOutput) { o.Scores.Naturalness.Comment = "自然ではありません。" }, "scores.NATURALNESS.comment"},
		{"task comment Japanese only", func(o *evaluation.EvalOutput) { o.Scores.TaskAchievement.Comment = "目標は達成しました。" }, "scores.TASK_ACHIEVEMENT.comment"},
		{"finding explanation Japanese only", func(o *evaluation.EvalOutput) {
			o.Findings[1].Explanation = "過去形は「かったです」になります。"
		}, "findings[1].explanation"},
		{"mostly Japanese with a single Latin letter", func(o *evaluation.EvalOutput) {
			o.Summary = "A 今日は会社で働きましたので、とても疲れました。"
		}, ""},
		{"Latin letters below half of the remainder", func(o *evaluation.EvalOutput) { o.Summary = "ok ★★★★★★★★★★★★" }, "summary"},
		{"Latin share 30 percent", func(o *evaluation.EvalOutput) { o.Summary = "abc★★★★★★★" }, "summary"},
		{"Latin share exactly 50 percent", func(o *evaluation.EvalOutput) { o.Summary = "abcd★★★★" }, ""},
		{"Latin share just below 50 percent", func(o *evaluation.EvalOutput) { o.Summary = "abcd★★★★★" }, "summary"},
		{"digits count as acceptable", func(o *evaluation.EvalOutput) { o.Summary = "ok 1234567" }, ""},
		{"prolonged sound mark is Japanese", func(o *evaluation.EvalOutput) { o.Summary = "ok ーーーーー" }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := validOutput(t)
			tc.mutate(&out)
			problems := evaluation.CheckLanguage(out)
			if tc.problem == "" {
				require.Empty(t, problems)
				return
			}
			require.Len(t, problems, 1)
			require.Contains(t, problems[0], tc.problem)
		})
	}
}

func TestSchemaCacheCompilesOnce(t *testing.T) {
	b, err := os.ReadFile("testdata/output_schema.json")
	require.NoError(t, err)
	c := evaluation.NewSchemaCache()
	id := uuid.New()
	s1, err := c.Get(id, b)
	require.NoError(t, err)
	s2, err := c.Get(id, json.RawMessage(`{"type":"string"}`))
	require.NoError(t, err)
	require.Same(t, s1, s2)
	s3, err := c.Get(uuid.New(), b)
	require.NoError(t, err)
	require.NotSame(t, s1, s3)
	_, err = c.Get(uuid.New(), json.RawMessage(`{"type": 5}`))
	require.Error(t, err)
	_, err = c.Get(uuid.New(), json.RawMessage(`not json`))
	require.Error(t, err)
}
