//go:build live

package cartesia_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// TestLiveCartesiaTTS talks to the real vendor (MANUAL GATE, needs CARTESIA_API_KEY,
// LIVE_CARTESIA_VOICE_ID and LIVE_CARTESIA_TTS_MODEL; skipped without them). The recorder mode
// that turns this run into fixtures with meta.verified=true belongs to M9-T16.
func TestLiveCartesiaTTS(t *testing.T) {
	env := testutil.RequireEnv(t, "CARTESIA_API_KEY", "LIVE_CARTESIA_VOICE_ID", "LIVE_CARTESIA_TTS_MODEL")
	cfg := cartesia.Config{APIKey: env["CARTESIA_API_KEY"], Version: os.Getenv("CARTESIA_VERSION")}
	if v := os.Getenv("CARTESIA_WS_BASE_URL"); v != "" {
		cfg.WSBaseURL = v
	}
	tts, err := cartesia.NewTTS(cfg, httpx.NewClient(), nil)
	require.NoError(t, err)
	defer func() { _ = tts.Close() }()

	text := "今日はいい天気ですね、散歩に行きましょう。" // 20 characters of Japanese
	start := time.Now()
	st, err := tts.Synthesize(ctx, provider.TTSRequest{
		Text: text, Language: "ja", Model: env["LIVE_CARTESIA_TTS_MODEL"], VoiceID: env["LIVE_CARTESIA_VOICE_ID"],
		SpeakingRate: 1.0, RequestID: "live:1:0",
	})
	require.NoError(t, err)
	var ttfa time.Duration
	var pcm int
	var last provider.TTSChunk
	for c := range st.Chunks() {
		require.Nil(t, c.Err)
		if ttfa == 0 && len(c.PCM) > 0 {
			ttfa = time.Since(start)
		}
		require.Zero(t, len(c.PCM)%2)
		pcm += len(c.PCM)
		last = c
	}
	t.Logf("TTFA=%s audio=%d ms", ttfa, pcm/48)
	require.True(t, last.Final)
	require.GreaterOrEqual(t, pcm, 24000, "at least 0.5 s of PCM16 24 kHz")
	require.NotNil(t, last.Usage)
}
