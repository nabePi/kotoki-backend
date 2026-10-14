package prompt_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func ptr[T any](v T) *T { return &v }

func TestParamsDBWins(t *testing.T) {
	p := prompt.Params{Temperature: ptr(0.3), MaxOutputTokens: ptr(150)}
	require.Equal(t, 0.3, p.TemperatureOr(0.8))
	require.Equal(t, 150, p.MaxOutputTokensOr(320))
	var empty prompt.Params
	require.Equal(t, 0.8, empty.TemperatureOr(0.8))
	require.Equal(t, 320, empty.MaxOutputTokensOr(320))
	zero := prompt.Params{Temperature: ptr(0.0)}
	require.Equal(t, 0.0, zero.TemperatureOr(0.8), "an explicit 0 is a value, not 'unset'")
}

func TestParamsJSONOmitsNil(t *testing.T) {
	b, err := json.Marshal(prompt.Params{Temperature: ptr(0.8)})
	require.NoError(t, err)
	require.JSONEq(t, `{"temperature":0.8}`, string(b))
	var p prompt.Params
	require.NoError(t, json.Unmarshal([]byte(`{"temperature":0.3,"max_output_tokens":320,"top_p":0.9,"seed":7}`), &p))
	require.Equal(t, 0.3, *p.Temperature)
	require.Equal(t, 320, *p.MaxOutputTokens)
	require.Equal(t, 0.9, *p.TopP)
	require.EqualValues(t, 7, *p.Seed)
}

func TestFixturesRegistry(t *testing.T) {
	prompt.ResetFixturesForTest()
	t.Cleanup(prompt.ResetFixturesForTest)
	require.Nil(t, prompt.Fixtures("EVALUATION"))
	calls := 0
	prompt.RegisterFixtures("EVALUATION", func() []any { calls++; return []any{1, "two"} })
	require.Equal(t, []any{1, "two"}, prompt.Fixtures("EVALUATION"))
	require.Equal(t, 1, calls)
	require.Nil(t, prompt.Fixtures("SUMMARY"))
	require.PanicsWithValue(t, `prompt: fixtures for key "EVALUATION" are already registered`, func() {
		prompt.RegisterFixtures("EVALUATION", func() []any { return nil })
	})
}
