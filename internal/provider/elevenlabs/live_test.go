//go:build live

package elevenlabs_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/livekit"
)

// The tests below talk to the real vendor (MANUAL GATE, M9-T16): each skips without its key and
// LIVE_* env (model and voice never come from literals or a profile, D-015); ELEVENLABS_BASE_URL
// and ELEVENLABS_WS_BASE_URL may override the endpoints.

func liveElevenLabsConfig(key string) elevenlabs.Config {
	return elevenlabs.Config{
		APIKey:    key,
		BaseURL:   livekit.Getenv("ELEVENLABS_BASE_URL", "https://api.elevenlabs.io"),
		WSBaseURL: livekit.Getenv("ELEVENLABS_WS_BASE_URL", "wss://api.elevenlabs.io"),
	}
}

func TestLiveElevenLabsSTT(t *testing.T) {
	env := testutil.RequireEnv(t, "ELEVENLABS_API_KEY", "LIVE_STT_MODEL_ELEVENLABS")
	for _, name := range livekit.STTFixtures {
		t.Run(name, func(t *testing.T) {
			pcm, ref := livekit.LoadPCM16k(t, name), livekit.LoadRef(t, name)
			rec := livekit.NewRecorder(t, "elevenlabs", "stt", name)
			cfg := liveElevenLabsConfig(env["ELEVENLABS_API_KEY"])
			cfg.STTModel, cfg.Tap = env["LIVE_STT_MODEL_ELEVENLABS"], livekit.Tap(rec)
			stt, err := elevenlabs.NewSTT(cfg, nil, nil)
			require.NoError(t, err)
			got := livekit.Transcribe(t, stt, pcm)
			cer := livekit.CER(got, ref)
			t.Logf("CER=%.3f", cer)
			require.LessOrEqual(t, cer, 0.15, name)
		})
	}
}

func TestLiveElevenLabsTTS(t *testing.T) {
	env := testutil.RequireEnv(t, "ELEVENLABS_API_KEY", "LIVE_ELEVENLABS_VOICE_ID", "LIVE_TTS_MODEL_ELEVENLABS")
	rec := livekit.NewRecorder(t, "elevenlabs", "tts", "basic")
	tts, err := elevenlabs.NewTTS(liveElevenLabsConfig(env["ELEVENLABS_API_KEY"]), livekit.HTTPClient(rec), nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	st, err := tts.Synthesize(ctx, provider.TTSRequest{
		Text: "今日はいい天気ですね、散歩に行きましょう。", Language: "ja", Model: env["LIVE_TTS_MODEL_ELEVENLABS"],
		VoiceID: env["LIVE_ELEVENLABS_VOICE_ID"], SpeakingRate: 1.0, RequestID: "live:1:0",
	})
	require.NoError(t, err)
	pcm, ttfa, last := livekit.DrainTTS(t, st, start)
	t.Logf("TTFA=%s audio=%d ms", ttfa, pcm/48)
	require.True(t, last.Final)
	require.GreaterOrEqual(t, pcm, 24000, "at least 0.5 s of PCM16 24 kHz")
}
