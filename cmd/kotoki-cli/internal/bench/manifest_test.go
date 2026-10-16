package bench_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func writeManifest(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "manifest.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return p
}

func TestLoadManifestValid(t *testing.T) {
	items, err := bench.LoadManifest("testdata/manifest_ok.jsonl")
	require.NoError(t, err)
	require.Len(t, items, 6)
	require.Equal(t, "n1", items[0].ID)
	require.Equal(t, bench.SpeakerNative, items[0].Speaker)
	require.EqualValues(t, 900, items[0].DurationMs)
	require.Equal(t, []string{"short"}, items[0].Tags)
	require.Equal(t, provider.Message{Role: provider.RoleSystem, Content: "あなたは友達です。"}, items[3].Messages[0])
	require.Len(t, bench.ItemsOf(items, bench.SetSTTNative, bench.SetSTTLearner), 2)
	require.True(t, items[2].IsSTT())
	require.False(t, items[3].IsSTT())
}

func TestLoadManifestRejectsBadSet(t *testing.T) {
	_, err := bench.LoadManifest(writeManifest(t, `{"id":"a","set":"STT-other","audio_uri":"x","reference_text":"y","speaker":"native"}`))
	require.ErrorContains(t, err, `set "STT-other"`)
	require.ErrorContains(t, err, "line 1")
}

func TestLoadManifestRejectsBadSpeaker(t *testing.T) {
	_, err := bench.LoadManifest(writeManifest(t, `{"id":"a","set":"STT-native","audio_uri":"x","reference_text":"y","speaker":"child"}`))
	require.ErrorContains(t, err, `speaker "child"`)
	// an STT item must name its speaker, and it must match STT-native / STT-learner
	_, err = bench.LoadManifest(writeManifest(t, `{"id":"a","set":"STT-native","audio_uri":"x","reference_text":"y"}`))
	require.ErrorContains(t, err, "needs a speaker")
	_, err = bench.LoadManifest(writeManifest(t, `{"id":"a","set":"STT-native","audio_uri":"x","reference_text":"y","speaker":"learner"}`))
	require.ErrorContains(t, err, "does not take speaker")
	_, err = bench.LoadManifest(writeManifest(t, `{"id":"a","set":"STT-learner","audio_uri":"x","reference_text":"y","speaker":"native"}`))
	require.ErrorContains(t, err, "does not take speaker")
}

func TestLoadManifestRejectsDuplicateID(t *testing.T) {
	_, err := bench.LoadManifest(writeManifest(t,
		`{"id":"a","set":"TTS-sentences","reference_text":"x"}`,
		`{"id":"a","set":"TTS-sentences","reference_text":"y"}`))
	require.ErrorContains(t, err, `duplicate id "a"`)
	require.ErrorContains(t, err, "line 2")
}

func TestLoadManifestRejectsIncompleteItems(t *testing.T) {
	for name, line := range map[string]string{
		"empty id":         `{"id":"","set":"TTS-sentences","reference_text":"x"}`,
		"stt no audio":     `{"id":"a","set":"STT-noise","reference_text":"y","speaker":"native"}`,
		"stt no reference": `{"id":"a","set":"STT-noise","audio_uri":"x","speaker":"native"}`,
		"llm no messages":  `{"id":"a","set":"LLM-context"}`,
		"json no messages": `{"id":"a","set":"LLM-json"}`,
		"tts no sentence":  `{"id":"a","set":"TTS-sentences"}`,
		"unknown field":    `{"id":"a","set":"TTS-sentences","reference_text":"x","extra":1}`,
		"not json":         `not json`,
	} {
		_, err := bench.LoadManifest(writeManifest(t, line))
		require.Error(t, err, name)
	}
	_, err := bench.LoadManifest(filepath.Join(t.TempDir(), "missing.jsonl"))
	require.Error(t, err)
}
