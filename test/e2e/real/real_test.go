//go:build e2e && live

// Package real runs the small real-provider e2e session of M9-T17 (MANUAL GATE): it needs real
// vendor keys, real voice IDs in seed/env/local/0900_voice_ids.sql and a real-speaker recording
// at testdata/audio/ja_self_intro.wav. Without the keys the test is skipped.
package real

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// summary is the part of the CLI's summary.json this test reads (turn_latency_ms is a list, one
// entry per turn).
type summary struct {
	TurnLatencyMS []int64 `json:"turn_latency_ms"`
}

func TestRealProviderSingleSession(t *testing.T) {
	keys := []string{"OPENAI_API_KEY", "ELEVENLABS_API_KEY", "CARTESIA_API_KEY"}
	if os.Getenv("E2E_LLM") == "DEEPSEEK" {
		keys = append(keys, "DEEPSEEK_API_KEY")
	}
	// E2E_STT=ELEVENLABS|CARTESIA need no extra key: both vendor keys are required above for TTS.
	env := testutil.RequireEnv(t, keys...)

	root := testutil.ModuleRoot(t)
	for _, f := range []string{"testdata/audio/ja_self_intro.wav", "seed/env/local/0900_voice_ids.sql"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Skipf("MANUAL GATE: %s is missing (real recording / real voice IDs)", f)
		}
	}

	out := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "test/e2e/real/run.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "OUT_DIR="+out)
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))

	// One session per character (default HARUKA = ELEVENLABS, AOI = CARTESIA): a directory each.
	dirs, err := filepath.Glob(filepath.Join(out, "*", "summary.json"))
	require.NoError(t, err)
	require.NotEmpty(t, dirs, "run.sh produced no summary.json")
	for _, sumPath := range dirs {
		dir := filepath.Dir(sumPath)
		wavs, err := filepath.Glob(filepath.Join(dir, "turn-*-gen-*.wav"))
		require.NoError(t, err)
		require.NotEmpty(t, wavs, dir)
		fi, err := os.Stat(wavs[0])
		require.NoError(t, err)
		require.Greater(t, fi.Size(), int64(48000), "more than 0.5 s of PCM16 48 kHz: %s", wavs[0])

		raw, err := os.ReadFile(sumPath)
		require.NoError(t, err)
		var sum summary
		require.NoError(t, json.Unmarshal(raw, &sum))
		require.NotEmpty(t, sum.TurnLatencyMS, dir)
		require.Greater(t, sum.TurnLatencyMS[0], int64(0), dir)
	}
	for _, k := range keys {
		require.NotContains(t, string(b), env[k], "%s leaked into the output", k)
	}
}
