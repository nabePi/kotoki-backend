package bench_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

const synthDir = "../../../../testdata/audio/synth"

func capCfg(t *testing.T, c provider.Capability, mut func(*fake.CapConfig)) fake.CapConfig {
	t.Helper()
	cc, err := fake.LoadCapConfig(func(string) (string, bool) { return "", false }, provider.FakePrimary, c)
	require.NoError(t, err)
	cc.LatencyMS = 5
	if c == provider.CapSTT {
		cc.ConnectMS = 2
	}
	if mut != nil {
		mut(&cc)
	}
	return cc
}

func fakeDeps() fake.Deps { return fake.Deps{Clock: clock.System{}} }

func fakeLLM(t *testing.T, mut func(*fake.CapConfig)) *fake.LLM {
	return fake.NewLLM(provider.FakePrimary, capCfg(t, provider.CapLLM, mut), fake.Config{LLMTokenIntervalMS: 1}, fakeDeps())
}

// fixtures16k converts the M5 synth utterances (48 kHz, the client rate) to the 16 kHz copies the fake
// STT reads, like `make fixtures16k`; the benchmark items still point at the 48 kHz originals, which
// DirAudio resamples.
func fixtures16k(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"ja_greeting_short", "ja_question", "ja_self_intro"} {
		b, err := os.ReadFile(synthDir + "/" + n + ".wav")
		require.NoError(t, err)
		pcm, rate, err := provider.DecodeWAV(b)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dir+"/"+n+".wav", provider.EncodeWAV(provider.ResamplePCM16(pcm, rate, provider.STTSampleRate), provider.STTSampleRate), 0o600))
		txt, err := os.ReadFile(synthDir + "/" + n + ".txt")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dir+"/"+n+".txt", txt, 0o600))
	}
	return dir
}

func fakeSTT(t *testing.T, mut func(*fake.CapConfig)) *fake.STT {
	s, err := fake.NewSTT(provider.FakePrimary, capCfg(t, provider.CapSTT, mut), fake.Config{STTMode: "fixture", STTFixtureDir: fixtures16k(t)}, fakeDeps())
	require.NoError(t, err)
	return s
}

func fakeTTS(t *testing.T, mut func(*fake.CapConfig)) *fake.TTS {
	return fake.NewTTS(provider.FakePrimary, capCfg(t, provider.CapTTS, mut), fake.Config{TTSMsPerChar: 20, TTSChunkIntervalMS: 1}, fakeDeps())
}

func llmCand() bench.Candidate {
	return bench.Candidate{Capability: provider.CapLLM, Provider: provider.FakePrimary, Model: "fake-llm"}
}

func sttCand() bench.Candidate {
	return bench.Candidate{Capability: provider.CapSTT, Provider: provider.FakePrimary, Model: "fake-stt"}
}

func ttsCand() bench.Candidate {
	return bench.Candidate{Capability: provider.CapTTS, Provider: provider.FakePrimary, Model: "fake-tts", Voice: "fake-voice", Role: "PRIMARY"}
}

func userMsgs(text string) []provider.Message {
	return []provider.Message{{Role: provider.RoleSystem, Content: "あなたは友達です。"}, {Role: provider.RoleUser, Content: text}}
}

func TestRunLLMMeasuresFirstSentence(t *testing.T) {
	items := []bench.Item{{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("週末は何をしましたか？")}}
	rs := bench.RunLLM(context.Background(), fakeLLM(t, nil), llmCand(), items, 2, clock.System{})
	require.Len(t, rs, 2)
	for i, r := range rs {
		require.Equal(t, bench.SuiteLLM, r.Suite)
		require.Equal(t, "LLM", r.Capability)
		require.Equal(t, "FAKE_PRIMARY", r.Provider)
		require.Equal(t, "c1", r.ItemID)
		require.Equal(t, i+1, r.Attempt)
		require.Empty(t, r.ErrorKind)
		require.Greater(t, r.Values["first_sentence_ms"], 0.0)
		require.LessOrEqual(t, r.Values["ttft_ms"], r.Values["first_sentence_ms"])
		require.LessOrEqual(t, r.Values["first_sentence_ms"], r.Values["total_ms"])
		require.Greater(t, r.Values["runes"], 0.0)
		require.True(t, r.Flags["format_ok"])
		require.False(t, r.Flags["no_sentence_end"])
	}
}

func TestRunLLMRecordsNoAnswerText(t *testing.T) {
	items := []bench.Item{{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("ひみつの質問です")}}
	rs := bench.RunLLM(context.Background(), fakeLLM(t, nil), llmCand(), items, 1, clock.System{})
	b, err := json.Marshal(rs)
	require.NoError(t, err)
	require.NotContains(t, string(b), "ひみつ")
	require.NotContains(t, string(b), "そうなんですね", "no answer text either")
}

func TestRunLLMErrorIsARowNotAnAbort(t *testing.T) {
	l := fakeLLM(t, func(c *fake.CapConfig) { c.FailEveryN = 2; c.FailureKind = provider.KindRateLimited })
	items := []bench.Item{{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("こんにちは")}}
	rs := bench.RunLLM(context.Background(), l, llmCand(), items, 4, clock.System{})
	require.Len(t, rs, 4)
	var failed, ok int
	for _, r := range rs {
		if r.ErrorKind != "" {
			failed++
			require.Equal(t, "RATE_LIMITED", r.ErrorKind)
			require.Nil(t, r.Values, "an error row has no measurements")
		} else {
			ok++
		}
	}
	require.Equal(t, 2, failed)
	require.Equal(t, 2, ok)
}

func TestRunLLMStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := []bench.Item{{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("こんにちは")}}
	require.Empty(t, bench.RunLLM(ctx, fakeLLM(t, nil), llmCand(), items, 3, clock.System{}))
}

func TestRunLLMTimeoutIsAnErrorRow(t *testing.T) {
	l := fakeLLM(t, func(c *fake.CapConfig) { c.FailEveryN = 1; c.FailurePhase = fake.PhaseHang })
	items := []bench.Item{{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("こんにちは")}}
	rs := bench.RunLLM(context.Background(), l, llmCand(), items, 1, clock.System{}, bench.WithTimeout(80*time.Millisecond))
	require.Len(t, rs, 1)
	require.Equal(t, "TIMEOUT", rs[0].ErrorKind)
}

func synthItem(id, set, speaker, wav, ref string) bench.Item {
	return bench.Item{ID: id, Set: set, Speaker: speaker, AudioURI: wav, ReferenceText: ref}
}

func TestRunSTTComputesCERAndFinalLatency(t *testing.T) {
	audio := bench.DirAudio{Root: synthDir}
	items := []bench.Item{
		synthItem("ok", bench.SetSTTNative, "native", "ja_greeting_short.wav", "こんにちは。"),
		synthItem("bad", bench.SetSTTLearner, "learner", "ja_greeting_short.wav", "いいえ、ちがいます。"),
	}
	rs := bench.RunSTT(context.Background(), fakeSTT(t, nil), sttCand(), items, audio, 2, clock.System{}, bench.WithTimeout(20*time.Second))
	require.Len(t, rs, 4)
	for _, r := range rs {
		require.Empty(t, r.ErrorKind, r.ItemID)
		require.Greater(t, r.Values["final_after_finalize_ms"], 0.0)
		require.Greater(t, r.Values["connect_ms"], 0.0)
		require.Greater(t, r.Values["audio_ms"], 1000.0)
		require.Contains(t, r.Flags, "has_partial")
	}
	require.NotContains(t, readSynthText(t, "ja_greeting_short"), "いいえ")
	for _, r := range rs {
		if r.ItemID == "ok" {
			require.InDelta(t, 0.0, r.Values["cer"], 1e-9, "the fixture text matches the reference")
			require.InDelta(t, 0.0, r.Values["cer_edits"], 0)
			require.Greater(t, r.Values["cer_ref_len"], 0.0)
		} else {
			require.Greater(t, r.Values["cer"], 0.5, "a wrong reference gives a high CER")
		}
	}
}

func readSynthText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(synthDir + "/" + name + ".txt")
	require.NoError(t, err)
	return string(b)
}

func TestRunSTTAudioProblemIsAnErrorRow(t *testing.T) {
	items := []bench.Item{synthItem("x", bench.SetSTTNative, "native", "missing.wav", "はい")}
	rs := bench.RunSTT(context.Background(), fakeSTT(t, nil), sttCand(), items, bench.DirAudio{Root: synthDir}, 2, clock.System{})
	require.Len(t, rs, 2)
	require.Equal(t, "AUDIO", rs[0].ErrorKind)
}

func TestRunSTTVendorErrorIsARow(t *testing.T) {
	s := fakeSTT(t, func(c *fake.CapConfig) { c.FailEveryN = 1; c.FailureKind = provider.KindAuth })
	items := []bench.Item{synthItem("x", bench.SetSTTNative, "native", "ja_greeting_short.wav", "はい")}
	rs := bench.RunSTT(context.Background(), s, sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{})
	require.Len(t, rs, 1)
	require.Equal(t, "AUTH", rs[0].ErrorKind)
}

func TestRunSTTHangIsATimeout(t *testing.T) {
	s := fakeSTT(t, func(c *fake.CapConfig) { c.FailEveryN = 1; c.FailurePhase = fake.PhaseHang })
	items := []bench.Item{synthItem("x", bench.SetSTTNative, "native", "ja_greeting_short.wav", "はい")}
	rs := bench.RunSTT(context.Background(), s, sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(300*time.Millisecond))
	require.Len(t, rs, 1)
	require.Equal(t, "TIMEOUT", rs[0].ErrorKind)
}

func TestRunSTTPacingTakesRealTime(t *testing.T) {
	items := []bench.Item{synthItem("x", bench.SetSTTNative, "native", "ja_greeting_short.wav", "こんにちは。")}
	t0 := time.Now()
	rs := bench.RunSTT(context.Background(), fakeSTT(t, nil), sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithPacing(true), bench.WithTimeout(20*time.Second))
	require.Len(t, rs, 1)
	require.Empty(t, rs[0].ErrorKind)
	require.GreaterOrEqual(t, time.Since(t0), 1400*time.Millisecond, "1.46 s of audio sent one 20 ms frame per 20 ms")
}

// --- LLM-json

type scriptedEval struct {
	steps []func(req provider.StructuredRequest) (provider.StructuredResult, error)
	reqs  []provider.StructuredRequest
}

func (s *scriptedEval) Name() provider.Name { return provider.FakePrimary }

func (s *scriptedEval) GenerateJSON(_ context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	s.reqs = append(s.reqs, req)
	if len(s.reqs) > len(s.steps) {
		return provider.StructuredResult{}, errors.New("unscripted call")
	}
	return s.steps[len(s.reqs)-1](req)
}

func jsonOK(raw string) func(provider.StructuredRequest) (provider.StructuredResult, error) {
	return func(provider.StructuredRequest) (provider.StructuredResult, error) {
		return provider.StructuredResult{JSON: json.RawMessage(raw)}, nil
	}
}

func jsonErr(k provider.ErrorKind) func(provider.StructuredRequest) (provider.StructuredResult, error) {
	return func(provider.StructuredRequest) (provider.StructuredResult, error) {
		return provider.StructuredResult{}, &provider.Error{Kind: k, Retryable: true}
	}
}

func jsonItems() []bench.Item {
	mk := func(id string) bench.Item {
		return bench.Item{ID: id, Set: bench.SetLLMJSON, Messages: []provider.Message{{Role: provider.RoleUser, Content: "transkrip " + id}},
			Tags: []string{"task:EVALUATION", "schema:s1"}}
	}
	return []bench.Item{mk("j1"), mk("j2"), mk("j3")}
}

func resolveS1(bench.Item) (bench.JSONSpec, error) {
	return bench.JSONSpec{Task: "EVALUATION", SchemaName: "s1", Schema: []byte(`{"type":"object"}`)}, nil
}

func validateGood(_ string, raw []byte) error {
	if !strings.Contains(string(raw), `"ok":true`) {
		return errors.New("/ok: violates the schema")
	}
	return nil
}

func TestRunLLMJSONFirstPassAndRepair(t *testing.T) {
	ev := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		jsonOK(`{"ok":true}`),                         // j1: first pass
		jsonOK(`{"ok":false}`), jsonOK(`{"ok":true}`), // j2: repaired
		jsonOK(`{"ok":false}`), jsonOK(`{"nope":1}`), // j3: invalid twice
	}}
	c := bench.Candidate{Capability: provider.CapLLM, Provider: provider.FakePrimary, Model: "fake-llm"}
	rs := bench.RunLLMJSON(context.Background(), ev, c, jsonItems(), resolveS1, validateGood, 1, clock.System{})
	require.Len(t, rs, 3)
	require.Len(t, ev.reqs, 5, "at most 2 calls per item and attempt")
	require.True(t, rs[0].Flags["first_pass_ok"])
	require.True(t, rs[0].Flags["after_repair_ok"])
	require.EqualValues(t, 1, rs[0].Values["calls"])
	require.False(t, rs[1].Flags["first_pass_ok"])
	require.True(t, rs[1].Flags["after_repair_ok"])
	require.EqualValues(t, 2, rs[1].Values["calls"])
	require.False(t, rs[2].Flags["first_pass_ok"])
	require.False(t, rs[2].Flags["after_repair_ok"])
	require.EqualValues(t, 2, rs[2].Values["calls"])
	require.Empty(t, rs[2].ErrorKind, "an invalid answer is a quality result, not a vendor error")

	m := bench.Aggregate(c, rs)
	require.InDelta(t, 1.0/3, m.JSONFirstPassOK, 1e-9)
	require.InDelta(t, 2.0/3, m.JSONAfterRepairOK, 1e-9)

	// the request carries the task, schema name and schema; the repair call carries the rejected
	// output and the validator problems
	require.Equal(t, "EVALUATION", ev.reqs[0].Task)
	require.Equal(t, "s1", ev.reqs[0].SchemaName)
	require.JSONEq(t, `{"type":"object"}`, string(ev.reqs[0].JSONSchema))
	repair := ev.reqs[2].Messages
	require.Len(t, repair, len(ev.reqs[0].Messages)+2)
	require.Equal(t, provider.RoleAssistant, repair[len(repair)-2].Role)
	require.Equal(t, `{"ok":false}`, repair[len(repair)-2].Content)
	require.Contains(t, repair[len(repair)-1].Content, "/ok: violates the schema")
}

func TestRunLLMJSONInvalidResponseThenRepair(t *testing.T) {
	ev := &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){
		jsonErr(provider.KindInvalidResponse), jsonOK(`{"ok":true}`), // empty content, repaired
		jsonErr(provider.KindInvalidResponse), jsonErr(provider.KindInvalidResponse), // never valid
		jsonErr(provider.KindRateLimited), // a vendor error ends the attempt
	}}
	c := bench.Candidate{Capability: provider.CapLLM, Provider: provider.DeepSeek, Model: "m"}
	rs := bench.RunLLMJSON(context.Background(), ev, c, jsonItems(), resolveS1, validateGood, 1, clock.System{})
	require.Len(t, rs, 3)
	require.True(t, rs[0].Flags["invalid_response_first"])
	require.True(t, rs[0].Flags["after_repair_ok"])
	require.False(t, rs[0].Flags["first_pass_ok"])
	require.Len(t, ev.reqs[1].Messages, len(ev.reqs[0].Messages)+1, "no rejected output to quote after an empty answer")
	require.False(t, rs[1].Flags["after_repair_ok"])
	require.Empty(t, rs[1].ErrorKind)
	require.Equal(t, "RATE_LIMITED", rs[2].ErrorKind)
	require.Nil(t, rs[2].Values)
	require.Len(t, ev.reqs, 5)
}

func TestRunLLMJSONResolveErrorIsADatasetRow(t *testing.T) {
	ev := &scriptedEval{}
	bad := func(bench.Item) (bench.JSONSpec, error) { return bench.JSONSpec{}, errors.New("no schema") }
	rs := bench.RunLLMJSON(context.Background(), ev, bench.Candidate{Capability: provider.CapLLM}, jsonItems()[:1], bad, validateGood, 2, clock.System{})
	require.Len(t, rs, 2)
	require.Equal(t, "DATASET", rs[0].ErrorKind)
	require.Empty(t, ev.reqs)
}

func TestRunLLMJSONWithFakeEvaluation(t *testing.T) {
	ev := fake.NewEvaluation(provider.FakePrimary, capCfg(t, provider.CapEvaluation, nil), fake.Config{}, fakeDeps())
	c := bench.Candidate{Capability: provider.CapLLM, Provider: provider.FakePrimary, Model: "fake-evaluation"}
	items := []bench.Item{{ID: "j1", Set: bench.SetLLMJSON, Messages: userMsgs("transkrip"), Tags: []string{"task:EVALUATION", "schema:kotoki_evaluation_rubric_v1"}}}
	res := func(bench.Item) (bench.JSONSpec, error) {
		return bench.JSONSpec{Task: "EVALUATION", SchemaName: "kotoki_evaluation_rubric_v1", Schema: []byte(`{"type":"object"}`)}, nil
	}
	rs := bench.RunLLMJSON(context.Background(), ev, c, items, res, func(string, []byte) error { return nil }, 2, clock.System{})
	require.Len(t, rs, 2)
	require.True(t, rs[0].Flags["first_pass_ok"])
}

// --- TTS

type recordingTTS struct {
	provider.TTSProvider
	reqs []provider.TTSRequest
}

func (r *recordingTTS) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	r.reqs = append(r.reqs, req)
	return r.TTSProvider.Synthesize(ctx, req)
}

func ttsItems() []bench.Item {
	return []bench.Item{{ID: "t1", Set: bench.SetTTSSentences, ReferenceText: "今日はいい天気ですね。"}, {ID: "t2", Set: bench.SetTTSSentences, ReferenceText: "駅はどこですか？"}}
}

func TestRunTTSMeasuresFirstAudioAndRTF(t *testing.T) {
	rec := &recordingTTS{TTSProvider: fakeTTS(t, nil)}
	rs := bench.RunTTS(context.Background(), rec, ttsCand(), ttsItems(), 2, clock.System{})
	require.Len(t, rs, 4)
	for _, r := range rs {
		require.Empty(t, r.ErrorKind)
		require.Equal(t, "TTS", r.Capability)
		require.Equal(t, "fake-voice", r.Voice)
		require.Equal(t, "PRIMARY", r.Role)
		require.Greater(t, r.Values["first_audio_ms"], 0.0)
		require.LessOrEqual(t, r.Values["first_audio_ms"], r.Values["total_ms"])
		require.Greater(t, r.Values["bytes"], 0.0)
		require.InDelta(t, r.Values["bytes"]/48, r.Values["audio_ms"], 1e-9, "24 kHz PCM16 mono = 48 bytes per ms")
		require.InDelta(t, r.Values["total_ms"]/r.Values["audio_ms"], r.Values["rtf"], 1e-9)
	}
	require.Len(t, rec.reqs, 4)
	for _, q := range rec.reqs {
		require.Equal(t, "fake-voice", q.VoiceID, "the voice of the candidate, as written")
		require.Equal(t, "fake-tts", q.Model)
		require.Equal(t, "ja", q.Language)
		require.InDelta(t, 1.0, q.SpeakingRate, 0)
	}
}

func TestRunTTSErrorIsARow(t *testing.T) {
	tts := fakeTTS(t, func(c *fake.CapConfig) { c.FailEveryN = 1; c.FailureKind = provider.KindInvalidRequest })
	rs := bench.RunTTS(context.Background(), tts, ttsCand(), ttsItems()[:1], 1, clock.System{})
	require.Len(t, rs, 1)
	require.Equal(t, "INVALID_REQUEST", rs[0].ErrorKind)
}

func TestRunTTSMidStreamError(t *testing.T) {
	tts := fakeTTS(t, func(c *fake.CapConfig) {
		c.FailEveryN = 1
		c.FailurePhase = fake.PhaseBeforeFirst
		c.FailureKind = provider.KindUnavailable
	})
	rs := bench.RunTTS(context.Background(), tts, ttsCand(), ttsItems()[:1], 1, clock.System{})
	require.Equal(t, "UNAVAILABLE", rs[0].ErrorKind)
}

// --- e2e

func TestRunE2ESumsTheStages(t *testing.T) {
	st := bench.E2EStack{STT: fakeSTT(t, nil), STTCand: sttCand(), LLM: fakeLLM(t, nil), LLMCand: llmCand(), TTS: fakeTTS(t, nil), TTSCand: ttsCand()}
	items := []bench.Item{synthItem("e1", bench.SetSTTNative, "native", "ja_greeting_short.wav", "こんにちは。")}
	rs := bench.RunE2E(context.Background(), st, items, bench.DirAudio{Root: synthDir}, 2, clock.System{},
		bench.WithTurnEndMs(700), bench.WithTimeout(20*time.Second), bench.WithSystemPrompt("あなたは友達です。"))
	require.Len(t, rs, 2)
	for _, r := range rs {
		require.Empty(t, r.ErrorKind)
		require.Equal(t, "E2E", r.Capability)
		require.Equal(t, "FAKE_PRIMARY+FAKE_PRIMARY+FAKE_PRIMARY", r.Provider)
		require.Equal(t, "fake-stt+fake-llm+fake-tts", r.Model)
		require.Equal(t, "fake-voice", r.Voice)
		v := r.Values
		require.InDelta(t, v["stt_final_ms"]+v["llm_first_sentence_ms"]+v["tts_first_audio_ms"]+700, v["e2e_turn_end_to_first_audio_ms"], 1e-9)
		require.InDelta(t, 700, v["turn_end_ms"], 0)
		require.Greater(t, v["stt_final_ms"], 0.0)
		require.Greater(t, v["llm_first_sentence_ms"], 0.0)
		require.Greater(t, v["tts_first_audio_ms"], 0.0)
	}
}

func TestRunE2EFailingStage(t *testing.T) {
	failing := func(c *fake.CapConfig) { c.FailEveryN = 1; c.FailureKind = provider.KindRateLimited }
	items := []bench.Item{synthItem("e1", bench.SetSTTNative, "native", "ja_greeting_short.wav", "こんにちは。")}
	audio := bench.DirAudio{Root: synthDir}
	base := bench.E2EStack{STT: fakeSTT(t, nil), STTCand: sttCand(), LLM: fakeLLM(t, nil), LLMCand: llmCand(), TTS: fakeTTS(t, nil), TTSCand: ttsCand()}

	s := base
	s.STT = fakeSTT(t, failing)
	rs := bench.RunE2E(context.Background(), s, items, audio, 1, clock.System{}, bench.WithTimeout(20*time.Second))
	require.Equal(t, "STT", rs[0].Stage)
	require.Equal(t, "RATE_LIMITED", rs[0].ErrorKind)

	s = base
	s.LLM = fakeLLM(t, failing)
	rs = bench.RunE2E(context.Background(), s, items, audio, 1, clock.System{}, bench.WithTimeout(20*time.Second))
	require.Equal(t, "LLM", rs[0].Stage)

	s = base
	s.TTS = fakeTTS(t, failing)
	rs = bench.RunE2E(context.Background(), s, items, audio, 1, clock.System{}, bench.WithTimeout(20*time.Second))
	require.Equal(t, "TTS", rs[0].Stage)
	require.Nil(t, rs[0].Values)

	rs = bench.RunE2E(context.Background(), base, []bench.Item{synthItem("x", bench.SetSTTNative, "native", "missing.wav", "はい")}, audio, 1, clock.System{})
	require.Equal(t, "AUDIO", rs[0].ErrorKind)
}

// --- concurrency

func TestConcurrencyRamp(t *testing.T) {
	var n atomic.Int64
	call := func(context.Context) error {
		if n.Add(1)%10 < 3 { // 3 of every 10 calls: 30%
			return &provider.Error{Kind: provider.KindRateLimited}
		}
		return nil
	}
	c := llmCand()
	rs := bench.RunConcurrency(context.Background(), c, []int{10, 50, 100}, call, clock.System{})
	require.Len(t, rs, 3)
	require.EqualValues(t, 10, rs[0].Values["step"])
	require.EqualValues(t, 100, rs[2].Values["step"])
	require.InDelta(t, 30, rs[2].Values["rate_limited_pct"], 1)
	require.InDelta(t, 30, rs[2].Values["error_pct"], 1, "429 counts as an error")
	require.EqualValues(t, 160, n.Load())
	require.Equal(t, "concurrency", rs[2].Suite)
	require.Equal(t, "LLM", rs[2].Capability)
}

func TestConcurrencyOtherErrorsAndLatency(t *testing.T) {
	var n atomic.Int64
	call := func(context.Context) error {
		time.Sleep(20 * time.Millisecond)
		if n.Add(1)%4 == 0 {
			return &provider.Error{Kind: provider.KindUnavailable}
		}
		return nil
	}
	rs := bench.RunConcurrency(context.Background(), llmCand(), []int{8}, call, clock.System{})
	require.Len(t, rs, 1)
	require.InDelta(t, 0, rs[0].Values["rate_limited_pct"], 0)
	require.InDelta(t, 25, rs[0].Values["error_pct"], 0)
	require.GreaterOrEqual(t, rs[0].Values["p50_ms"], 20.0)
	require.GreaterOrEqual(t, rs[0].Values["p95_ms"], rs[0].Values["p50_ms"])
}

func TestConcurrencyStartsEveryCallTogether(t *testing.T) {
	var inflight, peak atomic.Int64
	call := func(context.Context) error {
		cur := inflight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inflight.Add(-1)
		return nil
	}
	bench.RunConcurrency(context.Background(), llmCand(), []int{20}, call, clock.System{})
	require.EqualValues(t, 20, peak.Load())
}

func TestConcurrencyStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	call := func(context.Context) error { return nil }
	rs := bench.RunConcurrency(ctx, llmCand(), []int{2, 4}, func(context.Context) error {
		cancel()
		return nil
	}, clock.System{})
	require.Len(t, rs, 1, "the second step does not start after the context ended")
	require.Empty(t, bench.RunConcurrency(context.Background(), llmCand(), []int{0, 5}, call, clock.System{}))
}

func TestRunSTTLongAudioWaitsOutAFullVendorBuffer(t *testing.T) {
	// 3.9 s sent without pacing overruns the 2 s vendor buffer: SendAudio answers UNAVAILABLE and the
	// runner backs off and retries instead of failing the attempt
	b, err := os.ReadFile(synthDir + "/ja_self_intro.txt")
	require.NoError(t, err)
	items := []bench.Item{synthItem("long", bench.SetSTTNative, "native", "ja_self_intro.wav", strings.TrimSpace(string(b)))}
	rs := bench.RunSTT(context.Background(), fakeSTT(t, nil), sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(30*time.Second))
	require.Len(t, rs, 1)
	require.Empty(t, rs[0].ErrorKind)
	require.InDelta(t, 0.0, rs[0].Values["cer"], 1e-9)
	require.Greater(t, rs[0].Values["audio_ms"], 3000.0)
}

func TestConcurrencyCalls(t *testing.T) {
	ctx := context.Background()
	audio := bench.DirAudio{Root: synthDir}
	sttItem := synthItem("n1", bench.SetSTTNative, "native", "ja_greeting_short.wav", "こんにちは。")

	call, err := bench.STTCall(fakeSTT(t, nil), sttItem, audio, clock.System{}, bench.WithTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, call(ctx))
	failing := func(c *fake.CapConfig) { c.FailEveryN = 1; c.FailureKind = provider.KindRateLimited }
	call, err = bench.STTCall(fakeSTT(t, failing), sttItem, audio, clock.System{})
	require.NoError(t, err)
	require.Equal(t, provider.KindRateLimited, provider.KindOf(call(ctx)))
	_, err = bench.STTCall(fakeSTT(t, nil), synthItem("x", bench.SetSTTNative, "native", "missing.wav", "x"), audio, clock.System{})
	require.Error(t, err)

	llmItem := bench.Item{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("こんにちは")}
	require.NoError(t, bench.LLMCall(fakeLLM(t, nil), llmCand(), llmItem, clock.System{})(ctx))
	require.Equal(t, provider.KindRateLimited, provider.KindOf(bench.LLMCall(fakeLLM(t, failing), llmCand(), llmItem, clock.System{})(ctx)))

	ttsItem := ttsItems()[0]
	require.NoError(t, bench.TTSCall(fakeTTS(t, nil), ttsCand(), ttsItem, clock.System{})(ctx))
	require.Equal(t, provider.KindRateLimited, provider.KindOf(bench.TTSCall(fakeTTS(t, failing), ttsCand(), ttsItem, clock.System{})(ctx)))
}

// scriptedLLM answers every request with fixed deltas and records the requests.
type scriptedLLM struct {
	deltas []string
	reqs   []provider.LLMRequest
}

func (s *scriptedLLM) Name() provider.Name { return provider.FakePrimary }

func (s *scriptedLLM) Stream(_ context.Context, req provider.LLMRequest) (provider.LLMStream, error) {
	s.reqs = append(s.reqs, req)
	ch := make(chan provider.LLMEvent, len(s.deltas)+1)
	for _, d := range s.deltas {
		ch <- provider.LLMEvent{Type: provider.LLMDelta, Text: d}
	}
	ch <- provider.LLMEvent{Type: provider.LLMDone, FinishReason: provider.FinishStop, Usage: &provider.TokenUsage{}}
	close(ch)
	return scriptedStream{ch}, nil
}

type scriptedStream struct{ ch chan provider.LLMEvent }

func (s scriptedStream) Events() <-chan provider.LLMEvent { return s.ch }
func (scriptedStream) Close() error                       { return nil }

func TestRunLLMFormatAndAnswersWithoutSentenceEnd(t *testing.T) {
	items := []bench.Item{{ID: "c1", Set: bench.SetLLMContext, Messages: userMsgs("こんにちは")}}
	bad := &scriptedLLM{deltas: []string{"Sure, ", "here is an English answer"}}
	rs := bench.RunLLM(context.Background(), bad, llmCand(), items, 1, clock.System{})
	require.False(t, rs[0].Flags["format_ok"], "English text is not plain Japanese")
	require.True(t, rs[0].Flags["no_sentence_end"])
	require.InDelta(t, rs[0].Values["total_ms"], rs[0].Values["first_sentence_ms"], 0, "no 。！？: the whole answer is the first sentence")
	require.Greater(t, rs[0].Values["first_sentence_ms"], 0.0)

	good := &scriptedLLM{deltas: []string{"こんにちは。", "元気ですか？"}}
	rs = bench.RunLLM(context.Background(), good, llmCand(), items, 1, clock.System{}, bench.WithLLMParams(77, 0.3))
	require.True(t, rs[0].Flags["format_ok"])
	require.False(t, rs[0].Flags["no_sentence_end"])
	require.Equal(t, 77, good.reqs[0].MaxTokens)
	require.InDelta(t, 0.3, good.reqs[0].Temperature, 0)
	require.Equal(t, "fake-llm", good.reqs[0].Model)
}

func TestRunE2EPromptIsTheRecognisedText(t *testing.T) {
	llm := &scriptedLLM{deltas: []string{"はい。"}}
	st := bench.E2EStack{STT: fakeSTT(t, nil), STTCand: sttCand(), LLM: llm, LLMCand: llmCand(), TTS: fakeTTS(t, nil), TTSCand: ttsCand()}
	items := []bench.Item{synthItem("e1", bench.SetSTTNative, "native", "ja_greeting_short.wav", "こんにちは。")}
	rs := bench.RunE2E(context.Background(), st, items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(20*time.Second), bench.WithSystemPrompt("system!"))
	require.Empty(t, rs[0].ErrorKind)
	msgs := llm.reqs[0].Messages
	require.Len(t, msgs, 2)
	require.Equal(t, provider.Message{Role: provider.RoleSystem, Content: "system!"}, msgs[0])
	require.Equal(t, provider.Message{Role: provider.RoleUser, Content: "こんにちは。"}, msgs[1], "the text the STT recognised")
}

// scriptedSTT replays a fixed event script when Finalize is called.
type scriptedSTT struct{ script []provider.STTEvent }

func (scriptedSTT) Name() provider.Name { return provider.FakePrimary }
func (scriptedSTT) Model() string       { return "scripted" }

func (s scriptedSTT) Open(context.Context, provider.STTSessionConfig) (provider.STTSession, error) {
	return &scriptedSession{script: s.script, ev: make(chan provider.STTEvent, 64)}, nil
}

type scriptedSession struct {
	script []provider.STTEvent
	ev     chan provider.STTEvent
	once   sync.Once
}

func (*scriptedSession) SendAudio([]byte) error { return nil }
func (s *scriptedSession) Finalize() error {
	for _, e := range s.script {
		s.ev <- e
	}
	return nil
}
func (s *scriptedSession) Events() <-chan provider.STTEvent { return s.ev }
func (s *scriptedSession) Close() error {
	s.once.Do(func() { s.ev <- provider.STTEvent{Type: provider.STTClosed}; close(s.ev) })
	return nil
}
func (*scriptedSession) Stats() provider.STTStats { return provider.STTStats{} }

func TestRunSTTJoinsFinalsAndCountsPartialRevisions(t *testing.T) {
	p := func(text string) provider.STTEvent { return provider.STTEvent{Type: provider.STTPartial, Text: text} }
	stt := scriptedSTT{script: []provider.STTEvent{
		p("こん"), p("今日"), p("今日は"), // the second partial rewrites the first: one revision
		{Type: provider.STTFinal, Text: "今日は"},
		p("あ"), p("雨"), // after a FINAL the running segment starts over; "雨" rewrites "あ"
		{Type: provider.STTFinal, Text: "雨", FromFinalize: true},
	}}
	items := []bench.Item{synthItem("s", bench.SetSTTNative, "native", "ja_greeting_short.wav", "今日は雨")}
	rs := bench.RunSTT(context.Background(), stt, sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(5*time.Second))
	require.Len(t, rs, 1)
	require.Empty(t, rs[0].ErrorKind)
	require.InDelta(t, 0.0, rs[0].Values["cer"], 1e-9, "the FINAL texts of the whole utterance are joined")
	require.InDelta(t, 5, rs[0].Values["partials"], 0)
	require.InDelta(t, 2, rs[0].Values["partial_revisions"], 0)
	require.True(t, rs[0].Flags["has_partial"])
	require.Contains(t, rs[0].Values, "first_partial_ms")
}

func TestRunSTTClosedBeforeFinalIsAnError(t *testing.T) {
	items := []bench.Item{synthItem("s", bench.SetSTTNative, "native", "ja_greeting_short.wav", "はい")}
	rs := bench.RunSTT(context.Background(), scriptedSTT{}, sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(2*time.Second))
	require.Len(t, rs, 1)
	require.Equal(t, "TIMEOUT", rs[0].ErrorKind, "a session that never sends the FINAL times out")
	rs = bench.RunSTT(context.Background(), scriptedSTT{script: []provider.STTEvent{{Type: provider.STTClosed}}}, sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(2*time.Second))
	require.Equal(t, "UNAVAILABLE", rs[0].ErrorKind)
	rs = bench.RunSTT(context.Background(), scriptedSTT{script: []provider.STTEvent{{Type: provider.STTError, Err: &provider.Error{Kind: provider.KindAuth}}}}, sttCand(), items, bench.DirAudio{Root: synthDir}, 1, clock.System{}, bench.WithTimeout(2*time.Second))
	require.Equal(t, "AUTH", rs[0].ErrorKind)
}
