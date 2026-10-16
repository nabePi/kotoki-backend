package bench_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func writeModels(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadModelsValid(t *testing.T) {
	cs, err := bench.LoadModels("testdata/models_ok.yaml")
	require.NoError(t, err)
	require.Len(t, cs, 6)
	require.Equal(t, bench.Candidate{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "stt-model-a"}, cs[0])
	require.Equal(t, bench.Candidate{Capability: provider.CapTTS, Provider: provider.ElevenLabs, Model: "tts-model-a", Voice: "voice-1",
		Role: "PRIMARY", Characters: []string{"HARUKA", "KENJI"}}, cs[3])
	require.Equal(t, "ELEVENLABS/tts-model-a/voice-1", cs[3].Key())
	require.Equal(t, "OPENAI/stt-model-a", cs[0].Key())
}

func TestLoadModelsRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown vendor":       {"candidates:\n  - {capability: STT, provider: DEEPGRAM, model: m}\n", "DEEPGRAM"},
		"gemini":               {"candidates:\n  - {capability: LLM, provider: GEMINI, model: m}\n", "GEMINI"},
		"lowercase provider":   {"candidates:\n  - {capability: STT, provider: openai, model: m}\n", "openai"},
		"lowercase capability": {"candidates:\n  - {capability: stt, provider: OPENAI, model: m}\n", "capability"},
		"vendor not for cap":   {"candidates:\n  - {capability: LLM, provider: CARTESIA, model: m}\n", "not a vendor of LLM"},
		"missing model":        {"candidates:\n  - {capability: STT, provider: OPENAI}\n", "model is required"},
		"tts no voice":         {"candidates:\n  - {capability: TTS, provider: CARTESIA, model: m, role: PRIMARY}\n", "voice is required"},
		"tts bad role":         {"candidates:\n  - {capability: TTS, provider: CARTESIA, model: m, voice: v, role: MAIN}\n", "role"},
		"tts lowercase char":   {"candidates:\n  - {capability: TTS, provider: CARTESIA, model: m, voice: v, role: PRIMARY, characters: [haruka]}\n", "UPPERCASE"},
		"stt with role":        {"candidates:\n  - {capability: STT, provider: OPENAI, model: m, role: PRIMARY}\n", "TTS only"},
		"unknown field":        {"candidates:\n  - {capability: STT, provider: OPENAI, model: m, tier: HIGH}\n", "tier"},
		"duplicate":            {"candidates:\n  - {capability: STT, provider: OPENAI, model: m}\n  - {capability: STT, provider: OPENAI, model: m}\n", "duplicate"},
	} {
		_, err := bench.LoadModels(writeModels(t, tc.body))
		require.ErrorContains(t, err, tc.want, name)
	}
	_, err := bench.LoadModels(filepath.Join(t.TempDir(), "none.yaml"))
	require.Error(t, err)
}
