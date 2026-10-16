//go:build live

package openai_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/livekit"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// The tests below talk to the real vendor (MANUAL GATE, M9-T16): each skips without its keys and
// model env. Model names come only from LIVE_* env (never literals); endpoints may be overridden
// by OPENAI_BASE_URL / OPENAI_WS_BASE_URL. `-record` writes fixtures (see livekit.Record).

func liveOpenAIConfig(key string) openai.Config {
	return openai.Config{
		APIKey:    key,
		BaseURL:   livekit.Getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		WSBaseURL: livekit.Getenv("OPENAI_WS_BASE_URL", "wss://api.openai.com/v1/realtime"),
	}
}

func TestLiveOpenAISTT(t *testing.T) {
	env := testutil.RequireEnv(t, "OPENAI_API_KEY", "LIVE_STT_MODEL_OPENAI")
	for _, name := range livekit.STTFixtures {
		t.Run(name, func(t *testing.T) {
			pcm, ref := livekit.LoadPCM16k(t, name), livekit.LoadRef(t, name)
			rec := livekit.NewRecorder(t, "openai", "stt", name)
			cfg := liveOpenAIConfig(env["OPENAI_API_KEY"])
			cfg.STTModel, cfg.Tap = env["LIVE_STT_MODEL_OPENAI"], livekit.Tap(rec)
			stt, err := openai.NewSTT(cfg, nil, nil)
			require.NoError(t, err)
			got := livekit.Transcribe(t, stt, pcm)
			cer := livekit.CER(got, ref)
			t.Logf("CER=%.3f", cer)
			require.LessOrEqual(t, cer, 0.15, name)
		})
	}
}

func TestLiveOpenAILLM(t *testing.T) {
	env := testutil.RequireEnv(t, "OPENAI_API_KEY", "LIVE_LLM_MODEL_OPENAI")
	rec := livekit.NewRecorder(t, "openai", "llm", "golden")
	llm, err := openai.NewLLM(liveOpenAIConfig(env["OPENAI_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)
	livekit.RunLLM(t, llm, env["LIVE_LLM_MODEL_OPENAI"])
}

func TestLiveOpenAIEmbedding(t *testing.T) {
	env := testutil.RequireEnv(t, "OPENAI_API_KEY", "LIVE_EMBEDDING_MODEL_OPENAI")
	rec := livekit.NewRecorder(t, "openai", "embedding", "basic")
	emb, err := openai.NewEmbedding(liveOpenAIConfig(env["OPENAI_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)
	res, err := emb.Embed(ctx, provider.EmbeddingRequest{
		Texts: []string{"今日はいい天気ですね。", "週末は友達と映画を見ました。"}, Model: env["LIVE_EMBEDDING_MODEL_OPENAI"],
		Dimensions: 1536, RequestID: "live:embedding",
	})
	require.NoError(t, err)
	require.Len(t, res.Vectors, 2)
	for _, v := range res.Vectors {
		require.Len(t, v, 1536)
	}
}

func TestLiveOpenAIEvaluation(t *testing.T) {
	env := testutil.RequireEnv(t, "OPENAI_API_KEY", "LIVE_EVAL_MODEL_OPENAI")
	rec := livekit.NewRecorder(t, "openai", "evaluation", "basic")
	ev, err := openai.NewEvaluation(liveOpenAIConfig(env["OPENAI_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)
	livekit.RunEval(t, ev, env["LIVE_EVAL_MODEL_OPENAI"], 1, false)
}

// TestLiveRecordMode runs only with -record: it records one embedding call to a temporary file
// and checks the recorder's own guarantees on real traffic (verified=true, no credential).
func TestLiveRecordMode(t *testing.T) {
	if !*livekit.Record {
		t.Skip("only runs with -record")
	}
	env := testutil.RequireEnv(t, "OPENAI_API_KEY", "LIVE_EMBEDDING_MODEL_OPENAI")
	path := filepath.Join(t.TempDir(), "embedding_record_mode.jsonl")
	rec := replay.NewRecorder(t, "openai", "embedding", path)
	emb, err := openai.NewEmbedding(liveOpenAIConfig(env["OPENAI_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)
	_, err = emb.Embed(ctx, provider.EmbeddingRequest{
		Texts: []string{"録音モードの確認です。"}, Model: env["LIVE_EMBEDDING_MODEL_OPENAI"], Dimensions: 1536, RequestID: "live:record",
	})
	require.NoError(t, err)
	rec.Flush()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"verified":true`)
	require.NotContains(t, string(raw), env["OPENAI_API_KEY"], "the recording must not hold the API key")
	require.False(t, strings.Contains(strings.ToLower(string(raw)), "bearer "), "no Authorization value in a recording")
}
