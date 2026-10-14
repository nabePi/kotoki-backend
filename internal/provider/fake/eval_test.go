package fake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
)

func newTestEval(cfg CapConfig, conf Config, d Deps) *Evaluation {
	if d.Clock == nil {
		d.Clock = clock.NewFake(t0)
	}
	d.Seed = 42
	return NewEvaluation(provider.FakePrimary, cfg, conf, d)
}

func evalReq() provider.StructuredRequest {
	return provider.StructuredRequest{
		Task: "EVALUATION", SchemaName: "kotoki_fake_example", RequestID: "EVALUATION:s1", MaxOutputTokens: 500,
		Messages: []provider.Message{{Role: provider.RoleSystem, Content: "評価してください"}, {Role: provider.RoleUser, Content: "こんにちは"}},
	}
}

func TestFakeEvalConformance(t *testing.T) {
	providertest.EvaluationSuite(t, func(_ *testing.T) provider.EvaluationProvider { return newTestEval(CapConfig{}, Config{}, Deps{}) }, evalReq())
}

func TestFakeEvalScorePlaceholder(t *testing.T) {
	e := newTestEval(CapConfig{}, Config{}, Deps{})
	r1, err := e.GenerateJSON(context.Background(), evalReq())
	require.NoError(t, err)
	r2, err := e.GenerateJSON(context.Background(), evalReq())
	require.NoError(t, err)
	require.JSONEq(t, string(r1.JSON), string(r2.JSON), "the same RequestID gives the same scores")
	require.NotContains(t, string(r1.JSON), "{{score}}")

	var doc struct {
		Overall int `json:"overall_score"`
		Dims    struct {
			Fluency  int `json:"fluency"`
			Accuracy int `json:"accuracy"`
		} `json:"dimensions"`
	}
	require.NoError(t, json.Unmarshal(r1.JSON, &doc))
	for _, s := range []int{doc.Overall, doc.Dims.Fluency, doc.Dims.Accuracy} {
		require.GreaterOrEqual(t, s, 50)
		require.LessOrEqual(t, s, 90)
	}
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		req := evalReq()
		req.RequestID = "EVALUATION:" + strconv.Itoa(i)
		r, err := e.GenerateJSON(context.Background(), req)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(r.JSON, &doc))
		require.GreaterOrEqual(t, doc.Overall, 50)
		require.LessOrEqual(t, doc.Overall, 90)
		seen[doc.Overall] = true
	}
	require.Greater(t, len(seen), 10, "scores vary across request ids")
}

func TestFakeEvalUnknownSchema(t *testing.T) {
	e := newTestEval(CapConfig{}, Config{}, Deps{})
	req := evalReq()
	req.SchemaName = "no_such_schema"
	_, err := e.GenerateJSON(context.Background(), req)
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindInvalidRequest, pe.Kind)
}

func TestFakeEvalSchemaNameCannotEscapeFixtureDir(t *testing.T) {
	e := newTestEval(CapConfig{}, Config{EvalFixtureDir: t.TempDir()}, Deps{})
	req := evalReq()
	req.SchemaName = "../../etc/passwd"
	_, err := e.GenerateJSON(context.Background(), req)
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(err))
}

func TestFakeEvalFixtureDirOverrides(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kotoki_fake_example.json"), []byte(`{"from":"dir"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom_schema.json"), []byte(`{"custom":"{{score}}"}`), 0o600))
	e := newTestEval(CapConfig{}, Config{EvalFixtureDir: dir}, Deps{})
	r, err := e.GenerateJSON(context.Background(), evalReq())
	require.NoError(t, err)
	require.JSONEq(t, `{"from":"dir"}`, string(r.JSON))

	req := evalReq()
	req.SchemaName = "custom_schema"
	r, err = e.GenerateJSON(context.Background(), req)
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^\{"custom":\d+\}$`), string(r.JSON))

	e2 := newTestEval(CapConfig{}, Config{EvalFixtureDir: dir}, Deps{})
	req.SchemaName = "kotoki_fake_example"
	_, err = e2.GenerateJSON(context.Background(), req)
	require.NoError(t, err, "a schema missing from the dir still falls back to the embedded fixtures")
}

func TestFakeEvalBrokenFixtureIsInvalidResponse(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{not json`), 0o600))
	e := newTestEval(CapConfig{}, Config{EvalFixtureDir: dir}, Deps{})
	req := evalReq()
	req.SchemaName = "broken"
	_, err := e.GenerateJSON(context.Background(), req)
	require.Equal(t, provider.KindInvalidResponse, provider.KindOf(err))
}

func TestFakeEvalUsage(t *testing.T) {
	e := newTestEval(CapConfig{}, Config{}, Deps{})
	r, err := e.GenerateJSON(context.Background(), evalReq())
	require.NoError(t, err)
	require.Equal(t, len("評価してください")/3+len("こんにちは")/3, r.Usage.InputTokens)
	require.Equal(t, provider.FakePrimary, r.Provider)
}

func TestFakeEvalFaults(t *testing.T) {
	e := newTestEval(CapConfig{FailEveryN: 1, FailureKind: provider.KindRateLimited}, Config{}, Deps{})
	_, err := e.GenerateJSON(context.Background(), evalReq())
	require.Equal(t, provider.KindRateLimited, provider.KindOf(err))

	hang := newTestEval(CapConfig{FailEveryN: 1, FailurePhase: PhaseHang}, Config{}, Deps{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = hang.GenerateJSON(ctx, evalReq())
	require.Equal(t, provider.KindTimeout, provider.KindOf(err), "a hang ends with the caller's deadline")

	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"EVALUATION.FAKE_PRIMARY":{"mode":"slow","delay_ms":60}}`), t0, nil)
	})
	slow := newTestEval(CapConfig{}, Config{}, Deps{Faults: faults})
	start := time.Now()
	r, err := slow.GenerateJSON(context.Background(), evalReq())
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(start), 55*time.Millisecond)
	require.GreaterOrEqual(t, r.Latency, 60*time.Millisecond)
}

func TestNoFallbackInstanceForEvaluation(t *testing.T) {
	_, err := LoadCapConfig(func(string) (string, bool) { return "", false }, provider.FakeFallback, provider.CapEvaluation)
	require.Error(t, err, "D-017: FAKE_FALLBACK exists only for TTS")
}
