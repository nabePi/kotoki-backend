//go:build live

package deepseek_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider/deepseek"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/livekit"
)

// The tests below talk to the real vendor (MANUAL GATE, M9-T16): each skips without its key and
// LIVE_* model env (never literals); DEEPSEEK_BASE_URL may override the endpoint.

func liveDeepSeekConfig(key string) deepseek.Config {
	return deepseek.Config{APIKey: key, BaseURL: livekit.Getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com")}
}

func TestLiveDeepSeekLLM(t *testing.T) {
	env := testutil.RequireEnv(t, "DEEPSEEK_API_KEY", "LIVE_LLM_MODEL_DEEPSEEK")
	rec := livekit.NewRecorder(t, "deepseek", "llm", "golden")
	llm, err := deepseek.NewLLM(liveDeepSeekConfig(env["DEEPSEEK_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)
	livekit.RunLLM(t, llm, env["LIVE_LLM_MODEL_DEEPSEEK"])
}

// TestLiveDeepSeekEvaluation uses JSON mode, which does not enforce the schema: the strict
// decoder in RunEval validates it, and an empty or invalid answer must surface as a retryable
// INVALID_RESPONSE error (never a panic) while at least one of three calls succeeds.
func TestLiveDeepSeekEvaluation(t *testing.T) {
	env := testutil.RequireEnv(t, "DEEPSEEK_API_KEY", "LIVE_EVAL_MODEL_DEEPSEEK")
	rec := livekit.NewRecorder(t, "deepseek", "evaluation", "basic")
	ev, err := deepseek.NewEvaluation(liveDeepSeekConfig(env["DEEPSEEK_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)
	livekit.RunEval(t, ev, env["LIVE_EVAL_MODEL_DEEPSEEK"], 3, true)
}
