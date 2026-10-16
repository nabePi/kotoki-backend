package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	benchSynthDir   = "../../testdata/audio/synth"
	secretReference = "ひみつの参照テキスト" // must never reach stdout, stderr or any result file
	secretPrompt    = "ひみつのプロンプト"
	secretSentence  = "ひみつの文章です。"
)

type benchFixtureDirs struct{ dataset, out, models, schemas, human string }

// benchFixture writes a tiny dataset, a models file of FAKE_PRIMARY candidates and the fake
// environment; nothing here can reach a vendor.
func benchFixture(t *testing.T) benchFixtureDirs {
	t.Helper()
	root := t.TempDir()
	d := benchFixtureDirs{dataset: filepath.Join(root, "dataset"), out: filepath.Join(root, "out"), models: filepath.Join(root, "models.yaml"),
		schemas: filepath.Join(root, "schemas"), human: filepath.Join(root, "human.csv")}
	require.NoError(t, os.MkdirAll(d.dataset, 0o750))
	require.NoError(t, os.MkdirAll(d.schemas, 0o750))
	manifest := strings.Join([]string{
		`{"id":"n1","set":"STT-native","audio_uri":"ja_greeting_short.wav","reference_text":"こんにちは。","speaker":"native"}`,
		`{"id":"l1","set":"STT-learner","audio_uri":"ja_question.wav","reference_text":"趣味は何ですか？","speaker":"learner"}`,
		`{"id":"c1","set":"LLM-context","messages":[{"Role":"user","Content":"` + secretPrompt + `"}]}`,
		`{"id":"j1","set":"LLM-json","tags":["task:EVALUATION","schema:kotoki_evaluation_rubric_v1"],"messages":[{"Role":"user","Content":"` + secretPrompt + `"}]}`,
		`{"id":"t1","set":"TTS-sentences","reference_text":"` + secretSentence + `"}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(d.dataset, "manifest.jsonl"), []byte(manifest), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(d.schemas, "kotoki_evaluation_rubric_v1.json"), []byte(`{"type":"object"}`), 0o600))
	require.NoError(t, os.WriteFile(d.models, []byte(`candidates:
  - {capability: STT, provider: FAKE_PRIMARY, model: fake-stt}
  - {capability: LLM, provider: FAKE_PRIMARY, model: fake-llm}
  - {capability: TTS, provider: FAKE_PRIMARY, model: fake-tts, voice: fake-voice, role: PRIMARY, characters: [HARUKA, AOI]}
`), 0o600))
	require.NoError(t, os.WriteFile(d.human, []byte("item_id,reviewer,candidate,score_naturalness,score_level,score_character,mos,kanji_misread\n"+
		"c1,r1,FAKE_PRIMARY/fake-llm,5,5,4,,\n"+
		"t1,r1,FAKE_PRIMARY/fake-tts/fake-voice,,,,4.5,0\n"), 0o600))

	// the fake STT reads 16 kHz copies of the synth utterances (like `make fixtures16k`)
	fx := filepath.Join(root, "fixtures16k")
	require.NoError(t, os.MkdirAll(fx, 0o750))
	for _, n := range []string{"ja_greeting_short", "ja_question"} {
		b, err := os.ReadFile(filepath.Join(benchSynthDir, n+".wav"))
		require.NoError(t, err)
		pcm, rate, err := provider.DecodeWAV(b)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(fx, n+".wav"), provider.EncodeWAV(provider.ResamplePCM16(pcm, rate, provider.STTSampleRate), provider.STTSampleRate), 0o600))
		txt, err := os.ReadFile(filepath.Join(benchSynthDir, n+".txt"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(fx, n+".txt"), txt, 0o600))
	}
	t.Setenv("PROVIDER_MODE", "fake")
	t.Setenv("FAKE_STT_MODE", "fixture")
	t.Setenv("FAKE_STT_FIXTURE_DIR", fx)
	t.Setenv("FAKE_LLM_TOKEN_INTERVAL_MS", "1")
	t.Setenv("FAKE_TTS_MS_PER_CHAR", "20")
	t.Setenv("FAKE_TTS_CHUNK_INTERVAL_MS", "1")
	for _, k := range []string{"FAKE_PRIMARY_STT_LATENCY_MS", "FAKE_PRIMARY_LLM_LATENCY_MS", "FAKE_PRIMARY_TTS_LATENCY_MS", "FAKE_PRIMARY_EVALUATION_LATENCY_MS"} {
		t.Setenv(k, "5")
	}
	t.Setenv("FAKE_PRIMARY_STT_CONNECT_MS", "2")
	return d
}

func (d benchFixtureDirs) args(extra ...string) []string {
	return append([]string{"provider-bench", "--dataset", d.dataset, "--models-file", d.models, "--out", d.out, "--schemas-dir", d.schemas,
		"--audio-root", benchSynthDir, "--pacing=false", "--repeat", "2", "--concurrency-steps", "4,10", "--attempt-timeout", "20s"}, extra...)
}

func runBench(t *testing.T, args ...string) (code int, out, errw string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(context.Background(), args, &o, &e, lookupEnv)
	return code, o.String(), e.String()
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		out = append(out, m)
	}
	return out
}

func TestProviderBenchFlagValidation(t *testing.T) {
	d := benchFixture(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"bad suite":      {d.args("--suite", "x"), "--suite"},
		"empty out":      {[]string{"provider-bench", "--dataset", d.dataset, "--models-file", d.models}, "--out"},
		"blank out":      {d.args("--out", " "), "--out"},
		"repeat zero":    {d.args("--repeat", "0"), "--repeat"},
		"bad steps":      {d.args("--concurrency-steps", "10,x"), "--concurrency-steps"},
		"zero step":      {d.args("--concurrency-steps", "0"), "--concurrency-steps"},
		"turn end":       {d.args("--turn-end-ms", "-1"), "--turn-end-ms"},
		"e2e items":      {d.args("--e2e-items", "0"), "--e2e-items"},
		"attempt":        {d.args("--attempt-timeout", "0s"), "--attempt-timeout"},
		"max cost":       {d.args("--max-cost-per-min", "-1"), "--max-cost-per-min"},
		"stray argument": {d.args("oops"), "oops"},
	} {
		code, _, errw := runBench(t, tc.args...)
		assert.Equal(t, 2, code, name)
		assert.Contains(t, errw, tc.want, name)
	}
	assert.NoDirExists(t, d.out, "nothing is written when the flags are invalid")
}

func TestProviderBenchInputErrors(t *testing.T) {
	d := benchFixture(t)
	code, _, errw := runBench(t, d.args("--models-file", filepath.Join(t.TempDir(), "none.yaml"))...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "models")
	code, _, errw = runBench(t, d.args("--dataset", filepath.Join(t.TempDir(), "none"))...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "manifest")
	for _, vendor := range []string{"DEEPGRAM", "GEMINI"} {
		require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: STT, provider: "+vendor+", model: m}\n"), 0o600))
		code, _, errw = runBench(t, d.args()...)
		assert.Equal(t, 1, code, vendor)
		assert.Contains(t, errw, vendor, "a vendor outside the D-017 set is rejected by name")
	}
	assert.NoDirExists(t, d.out)
}

func TestProviderBenchVendorCandidateNeedsItsKey(t *testing.T) {
	d := benchFixture(t)
	t.Setenv("PROVIDER_MODE", "real")
	for _, k := range []string{"OPENAI_API_KEY", "DEEPSEEK_API_KEY", "ELEVENLABS_API_KEY", "CARTESIA_API_KEY"} {
		t.Setenv(k, "") // never the real key of the developer's shell: the run must stop before any dial
	}
	require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: STT, provider: OPENAI, model: some-stt-model}\n"), 0o600))
	code, out, errw := runBench(t, d.args("--suite", "stt")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "OPENAI_API_KEY", "the message names the missing variable")
	assert.Contains(t, errw, "OPENAI/some-stt-model")
	assert.Empty(t, out, "nothing ran")
	assert.NoDirExists(t, d.out)

	require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: LLM, provider: DEEPSEEK, model: m}\n"), 0o600))
	code, _, errw = runBench(t, d.args("--suite", "llm")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "DEEPSEEK_API_KEY")

	require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: TTS, provider: CARTESIA, model: m, voice: v, role: PRIMARY}\n"), 0o600))
	code, _, errw = runBench(t, d.args("--suite", "tts")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "CARTESIA_API_KEY")

	// a vendor that no selected suite touches needs no key
	require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: STT, provider: OPENAI, model: m}\n  - {capability: LLM, provider: FAKE_PRIMARY, model: fake-llm}\n"), 0o600))
	code, _, errw = runBench(t, d.args("--suite", "llm")...)
	assert.Equal(t, 0, code, errw)
}

func TestProviderBenchVendorCandidateNeverRunsAgainstFakes(t *testing.T) {
	d := benchFixture(t) // PROVIDER_MODE=fake
	require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: LLM, provider: OPENAI, model: m}\n"), 0o600))
	code, _, errw := runBench(t, d.args("--suite", "llm")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "PROVIDER_MODE=real")
	assert.NoDirExists(t, d.out)
}

func TestProviderBenchDryRunWithFake(t *testing.T) {
	d := benchFixture(t)
	code, out, errw := runBench(t, d.args("--human", d.human)...)
	require.Equal(t, 0, code, errw)

	rows := readLines(t, filepath.Join(d.out, "results.jsonl"))
	count := map[string]int{}
	for _, r := range rows {
		count[r["suite"].(string)]++
		_, hasErr := r["error_kind"]
		if r["suite"] != "concurrency" {
			assert.False(t, hasErr, "fake providers do not fail: %v", r)
		}
	}
	assert.Equal(t, map[string]int{"stt": 4, "llm": 2, "llm-json": 2, "tts": 2, "e2e": 2, "concurrency": 6, "human": 2, "cost": 3}, count)
	for _, r := range rows {
		if r["suite"] == "llm-json" {
			assert.Equal(t, true, r["flags"].(map[string]any)["first_pass_ok"])
		}
		if r["suite"] == "e2e" {
			assert.Equal(t, "FAKE_PRIMARY+FAKE_PRIMARY+FAKE_PRIMARY", r["provider"])
		}
	}

	f, err := os.Open(filepath.Join(d.out, "summary.csv"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	recs, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"suite", "capability", "provider", "model", "voice", "metric", "p50", "p95", "gate", "pass"}, recs[0])
	assert.Greater(t, len(recs), 30)

	// stdout: per-suite progress with ids and numbers, the gates and the recommendations
	assert.Contains(t, out, "stt FAKE_PRIMARY/fake-stt: 4 attempts, 0 errors")
	assert.Contains(t, out, "llm FAKE_PRIMARY/fake-llm: 2 attempts, 0 errors")
	assert.Contains(t, out, "concurrency FAKE_PRIMARY/fake-stt:")
	assert.Contains(t, out, "results written to "+d.out)
	assert.Contains(t, out, "gates ")
	// the ramp has no step 100: the error-rate gate is unmeasured and fails, so nothing qualifies
	assert.Contains(t, out, "FAIL: ")
	assert.Contains(t, out, "gate tidak diturunkan")

	env, err := os.ReadFile(filepath.Join(d.out, "provider.env"))
	require.NoError(t, err)
	assert.Contains(t, string(env), "keputusan akhir vendor aktif manual pemilik produk")
	for _, l := range strings.Split(string(env), "\n") {
		if strings.HasSuffix(l, "_STT_ENABLED=true") || strings.HasSuffix(l, "_LLM_ENABLED=true") {
			t.Fatalf("no candidate qualified, yet the env file switches one on: %s", l)
		}
	}

	var voices struct {
		Characters []map[string]any `json:"characters"`
	}
	b, err := os.ReadFile(filepath.Join(d.out, "character-voices.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &voices))
	require.Len(t, voices.Characters, 2)
	assert.Equal(t, "AOI", voices.Characters[0]["code"])
	assert.Equal(t, "HARUKA", voices.Characters[1]["code"])
	assert.Equal(t, "", voices.Characters[0]["provider_name"], "step 100 was not run: no voice qualifies")
}

func TestProviderBenchGatesPassAtStep100(t *testing.T) {
	d := benchFixture(t)
	code, out, errw := runBench(t, d.args("--human", d.human, "--concurrency-steps", "100", "--repeat", "1", "--suite", "all")...)
	require.Equal(t, 0, code, errw)
	// every fake candidate passes: concurrency 100 without errors, latencies far under the gates, human ratings given
	assert.Contains(t, out, "gates LLM|FAKE_PRIMARY/fake-llm|: PASS")
	assert.Contains(t, out, "gates TTS|FAKE_PRIMARY/fake-tts/fake-voice|PRIMARY: PASS")
	assert.Contains(t, out, "voice HARUKA: primary FAKE_PRIMARY/fake-tts/fake-voice, no fallback")

	var voices struct {
		Characters []map[string]any `json:"characters"`
	}
	b, err := os.ReadFile(filepath.Join(d.out, "character-voices.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &voices))
	assert.Equal(t, "FAKE_PRIMARY", voices.Characters[1]["provider_name"])
	assert.Equal(t, "fake-voice", voices.Characters[1]["voice_id"])
	assert.NotContains(t, voices.Characters[1], "fallback")
}

func TestProviderBenchSingleSuiteAndEmptySuites(t *testing.T) {
	d := benchFixture(t)
	code, out, errw := runBench(t, d.args("--suite", "tts")...)
	require.Equal(t, 0, code, errw)
	assert.Contains(t, out, "tts FAKE_PRIMARY/fake-tts/fake-voice: 2 attempts, 0 errors")
	assert.NotContains(t, out, "stt FAKE_PRIMARY")
	for _, r := range readLines(t, filepath.Join(d.out, "results.jsonl")) {
		assert.Contains(t, []any{"tts", "cost"}, r["suite"])
	}

	// a suite that was asked for by name and has nothing to run is an error; under `all` it is skipped
	require.NoError(t, os.WriteFile(d.models, []byte("candidates:\n  - {capability: LLM, provider: FAKE_PRIMARY, model: fake-llm}\n"), 0o600))
	d.out = filepath.Join(t.TempDir(), "out2")
	code, _, errw = runBench(t, d.args("--suite", "tts")...)
	assert.Equal(t, 1, code)
	assert.Contains(t, errw, "nothing to run")
	code, out, errw = runBench(t, d.args("--suite", "all", "--concurrency-steps", "4")...)
	require.Equal(t, 0, code, errw)
	assert.Contains(t, out, "stt: skipped")
	assert.Contains(t, out, "tts: skipped")
	assert.Contains(t, out, "e2e: skipped")
	assert.Contains(t, out, "character-voices.json not written")
}

func TestBenchDoesNotLogTextOrKeys(t *testing.T) {
	d := benchFixture(t)
	for k, v := range map[string]string{"OPENAI_API_KEY": "sk-bench-dummy-key", "DEEPSEEK_API_KEY": "ds-bench-dummy-key", "ELEVENLABS_API_KEY": "xi-bench-dummy-key", "CARTESIA_API_KEY": "ca-bench-dummy-key"} {
		t.Setenv(k, v)
	}
	code, out, errw := runBench(t, d.args("--human", d.human)...)
	require.Equal(t, 0, code, errw)
	all := out + errw
	for _, n := range []string{"results.jsonl", "summary.csv", "provider.env", "character-voices.json"} {
		b, err := os.ReadFile(filepath.Join(d.out, n))
		require.NoError(t, err)
		all += string(b)
	}
	for _, banned := range []string{secretReference, secretPrompt, secretSentence, "こんにちは。", "趣味は何ですか", "dummy-key", "Bearer", "API_KEY"} {
		assert.NotContains(t, all, banned)
	}
}

func TestProviderBenchHelpListsEveryFlag(t *testing.T) {
	code, _, errw := runBench(t, "provider-bench", "--help")
	assert.Equal(t, 0, code)
	for _, f := range []string{"-suite", "-dataset", "-models-file", "-out", "-repeat", "-audio-root", "-turn-end-ms", "-concurrency-steps", "-human",
		"-schemas-dir", "-pacing", "-e2e-items", "-attempt-timeout", "-max-cost-per-min"} {
		assert.Contains(t, errw, f)
	}
	_, _, usage := runBench(t, "help")
	assert.Contains(t, usage, "provider-bench")
}
