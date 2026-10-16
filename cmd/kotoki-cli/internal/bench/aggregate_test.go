package bench_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func sttRes(c bench.Candidate, set string, final, edits, refLen float64) bench.Result {
	return bench.Result{Suite: bench.SuiteSTT, Capability: "STT", Provider: string(c.Provider), Model: c.Model, Set: set, Attempt: 1,
		Values: map[string]float64{"final_after_finalize_ms": final, "cer_edits": edits, "cer_ref_len": refLen}}
}

func TestAggregateSTT(t *testing.T) {
	c := bench.Candidate{Capability: provider.CapSTT, Provider: provider.Cartesia, Model: "m"}
	other := bench.Candidate{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "m"}
	rs := []bench.Result{
		sttRes(c, bench.SetSTTNative, 100, 1, 10),
		sttRes(c, bench.SetSTTNative, 200, 0, 30), // corpus CER: 1/40, not the mean of 0.1 and 0
		sttRes(c, bench.SetSTTLearner, 300, 3, 20),
		sttRes(c, bench.SetSTTNoise, 250, 9, 10), // latency counts, the CER of the noise set is not gated
		sttRes(other, bench.SetSTTNative, 999, 9, 10),
		{Suite: bench.SuiteSTT, Capability: "STT", Provider: "CARTESIA", Model: "m", Set: bench.SetSTTNative, ErrorKind: "TIMEOUT"},
	}
	m := bench.Aggregate(c, rs)
	require.InDelta(t, 1.0/40, m.CERNative, 1e-12)
	require.InDelta(t, 0.15, m.CERLearner, 1e-12)
	require.InDelta(t, 300, m.P95["stt_final_ms"], 0, "nearest rank over 100, 200, 250, 300")
	require.InDelta(t, 200, m.P50["stt_final_ms"], 0)
	require.True(t, math.IsNaN(m.ErrorRate), "no concurrency step 100: not measured")
	require.True(t, m.CostMissing)
	require.True(t, math.IsNaN(m.QualityMean))
	require.False(t, bench.AllPass(bench.EvalGates(provider.CapSTT, "", m)), "unmeasured error rate fails its gate")
}

func TestAggregateNothingMeasured(t *testing.T) {
	m := bench.Aggregate(bench.Candidate{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "m"}, nil)
	require.Empty(t, m.P95)
	require.True(t, math.IsNaN(m.CERNative))
	require.True(t, math.IsNaN(m.FormatOK))
	require.True(t, math.IsNaN(m.JSONAfterRepairOK))
	require.False(t, bench.AllPass(bench.EvalGates(provider.CapSTT, "", m)))
	require.False(t, bench.AllPass(bench.EvalGates(provider.CapLLM, "", m)))
	require.False(t, bench.AllPass(bench.EvalGates(provider.CapTTS, "", m)))
}

func TestAggregateLLMTTSConcurrencyHumanCost(t *testing.T) {
	llm := bench.Candidate{Capability: provider.CapLLM, Provider: provider.DeepSeek, Model: "m"}
	tts := bench.Candidate{Capability: provider.CapTTS, Provider: provider.Cartesia, Model: "t", Voice: "v", Role: "FALLBACK"}
	mk := func(suite, capab string, c bench.Candidate, vals map[string]float64, flags map[string]bool) bench.Result {
		return bench.Result{Suite: suite, Capability: capab, Provider: string(c.Provider), Model: c.Model, Voice: c.Voice, Role: c.Role, Values: vals, Flags: flags}
	}
	rs := []bench.Result{
		mk(bench.SuiteLLM, "LLM", llm, map[string]float64{"first_sentence_ms": 400}, map[string]bool{"format_ok": true}),
		mk(bench.SuiteLLM, "LLM", llm, map[string]float64{"first_sentence_ms": 900}, map[string]bool{"format_ok": false}),
		mk(bench.SuiteLLM, "LLM", llm, map[string]float64{"first_sentence_ms": 500}, map[string]bool{"format_ok": true}),
		mk(bench.SuiteLLM, "LLM", llm, map[string]float64{"first_sentence_ms": 500}, map[string]bool{"format_ok": true}),
		mk(bench.SuiteLLMJSON, "LLM", llm, map[string]float64{"calls": 1}, map[string]bool{"first_pass_ok": true, "after_repair_ok": true}),
		mk(bench.SuiteLLMJSON, "LLM", llm, map[string]float64{"calls": 2}, map[string]bool{"first_pass_ok": false, "after_repair_ok": true}),
		mk(bench.SuiteHuman, "LLM", llm, map[string]float64{"quality_mean": 4.2}, nil),
		mk(bench.SuiteCost, "LLM", llm, map[string]float64{"cost_per_min_usd": 0.75}, nil),
		mk(bench.SuiteConcurrency, "LLM", llm, map[string]float64{"step": 50, "error_pct": 50}, nil),
		mk(bench.SuiteConcurrency, "LLM", llm, map[string]float64{"step": 100, "error_pct": 0.5}, nil),
		mk(bench.SuiteTTS, "TTS", tts, map[string]float64{"first_audio_ms": 550}, nil),
		mk(bench.SuiteHuman, "TTS", tts, map[string]float64{"mos": 3.6, "kanji_err_rate": 0.04}, nil),
		mk(bench.SuiteCost, "TTS", tts, nil, map[string]bool{"cost_missing": true}),
	}
	m := bench.Aggregate(llm, rs)
	require.InDelta(t, 0.75, m.FormatOK, 1e-12)
	require.InDelta(t, 0.5, m.JSONFirstPassOK, 1e-12)
	require.InDelta(t, 1.0, m.JSONAfterRepairOK, 1e-12)
	require.InDelta(t, 4.2, m.QualityMean, 1e-12)
	require.InDelta(t, 0.75, m.CostPerMinUSD, 1e-12)
	require.False(t, m.CostMissing)
	require.InDelta(t, 0.005, m.ErrorRate, 1e-12, "only step 100 counts")
	require.InDelta(t, 900, m.P95["llm_first_sentence_ms"], 0)

	mt := bench.Aggregate(tts, rs)
	require.InDelta(t, 550, mt.P95["tts_first_audio_ms"], 0)
	require.InDelta(t, 3.6, mt.MOS, 1e-12)
	require.InDelta(t, 0.04, mt.KanjiErrRate, 1e-12)
	require.True(t, mt.CostMissing)
	require.True(t, math.IsNaN(mt.ErrorRate))
	primary := tts
	primary.Role = "PRIMARY"
	require.Empty(t, bench.Aggregate(primary, rs).P95, "role is part of the candidate identity")
}

func TestCandidatesOfAndScoreAll(t *testing.T) {
	a := bench.Candidate{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "a"}
	b := bench.Candidate{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "b"}
	rs := []bench.Result{sttRes(a, bench.SetSTTNative, 1, 0, 1), sttRes(a, bench.SetSTTLearner, 1, 0, 1),
		{Suite: bench.SuiteLLM, Capability: "LLM", Provider: "OPENAI", Model: "b"},
		{Suite: bench.SuiteE2E, Capability: "E2E", Provider: "A+B+C", Model: "a+b+c"}}
	cs := bench.CandidatesOf(rs)
	require.Equal(t, []bench.Candidate{b, a}, cs, "sorted by capability, E2E left out")
	require.Len(t, bench.ScoreAll(rs), 2)
}

func TestCostResult(t *testing.T) {
	c := bench.Candidate{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "m"}
	r := bench.CostResult(c, stubPrices{llmIn: 1, llmOut: 2}, bench.DefaultUsage())
	require.Equal(t, bench.SuiteCost, r.Suite)
	require.InDelta(t, 6*(400.0/1e6+80.0*2/1e6), r.Values["cost_per_min_usd"], 1e-12)
	r = bench.CostResult(c, stubPrices{missing: true}, bench.DefaultUsage())
	require.True(t, r.Flags["cost_missing"])
	require.Empty(t, r.Values)
}

// --- DirAudio

func TestDirAudioResolve(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, provider.EncodeWAV(make([]byte, 640), provider.STTSampleRate), 0o600))
	}
	write("native/n1.wav")
	write("flat.wav")
	a := bench.DirAudio{Root: root}
	for _, uri := range []string{"native/n1.wav", "s3://kotoki-benchmark/v1/native/n1.wav", "v1/native/n1.wav", "file://" + filepath.Join(root, "native/n1.wav"), filepath.Join(root, "native/n1.wav")} {
		pcm, err := a.PCM16k(bench.Item{ID: "x", AudioURI: uri})
		require.NoError(t, err, uri)
		require.Len(t, pcm, 640)
	}
	pcm, err := a.PCM16k(bench.Item{ID: "x", AudioURI: "s3://b/v1/deep/dir/flat.wav"})
	require.NoError(t, err, "falls back to the base name")
	require.Len(t, pcm, 640)
	for _, uri := range []string{"", "missing.wav", "../../etc/passwd", "s3://bucket", "native"} {
		_, err := a.PCM16k(bench.Item{ID: "x", AudioURI: uri})
		require.Error(t, err, uri)
	}
	_, err = bench.DirAudio{}.PCM16k(bench.Item{ID: "x", AudioURI: "rel.wav"})
	require.ErrorContains(t, err, "--audio-root")
}

func TestDirAudioDoesNotLeaveTheRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "secret.wav"), provider.EncodeWAV(make([]byte, 64), provider.STTSampleRate), 0o600))
	_, err := bench.DirAudio{Root: root}.PCM16k(bench.Item{ID: "x", AudioURI: "../secret.wav"})
	require.Error(t, err)
}

func TestDirAudioResamplesAndRejectsOtherFormats(t *testing.T) {
	root := t.TempDir()
	tone48 := make([]byte, 48000*2/10) // 100 ms at 48 kHz
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.wav"), provider.EncodeWAV(tone48, 48000), 0o600))
	pcm, err := bench.DirAudio{Root: root}.PCM16k(bench.Item{ID: "x", AudioURI: "a.wav"})
	require.NoError(t, err)
	require.Len(t, pcm, 16000*2/10)
	require.NoError(t, os.WriteFile(filepath.Join(root, "bad.wav"), []byte("not a wav"), 0o600))
	_, err = bench.DirAudio{Root: root}.PCM16k(bench.Item{ID: "x", AudioURI: "bad.wav"})
	require.Error(t, err)
}

// --- SchemaDir

func TestSchemaDir(t *testing.T) {
	d := bench.NewSchemaDir("testdata/schemas")
	it := bench.Item{ID: "j", Tags: []string{"x", "task:EVALUATION", "schema:s_eval"}}
	spec, err := d.Resolve(it)
	require.NoError(t, err)
	require.Equal(t, "EVALUATION", spec.Task)
	require.Equal(t, "s_eval", spec.SchemaName)
	require.Contains(t, string(spec.Schema), `"required"`)

	require.NoError(t, d.Validate("s_eval", []byte(`{"score":80,"comment":"ok"}`)))
	for _, bad := range []string{`not json`, `{"score":80}`, `{"score":101,"comment":"x"}`, `{"score":1,"comment":"x","extra":1}`, `[]`} {
		require.Error(t, d.Validate("s_eval", []byte(bad)), bad)
	}
	err = d.Validate("s_eval", []byte(`{"score":101,"comment":"秘密のコメント"}`))
	require.ErrorContains(t, err, "/score")
	require.NotContains(t, err.Error(), "秘密", "problems name paths, never values")

	for _, tags := range [][]string{nil, {"task:EVALUATION"}, {"schema:s_eval"}, {"task:OTHER", "schema:s_eval"}, {"task:EVALUATION", "schema:nope"}, {"task:EVALUATION", "schema:../x"}} {
		_, err := d.Resolve(bench.Item{ID: "j", Tags: tags})
		require.Error(t, err, tags)
	}
	require.Error(t, d.Validate("nope", []byte(`{}`)))
	// a name cannot climb out of the schema directory, even to a file that exists
	require.FileExists(t, "testdata/outside_schema.json")
	_, err = bench.NewSchemaDir("testdata/schemas").Resolve(bench.Item{ID: "j", Tags: []string{"task:EVALUATION", "schema:../outside_schema"}})
	require.ErrorContains(t, err, "[a-z0-9_]")
}

// --- LoadHuman

func TestLoadHuman(t *testing.T) {
	cands := []bench.Candidate{
		{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "llm-a"},
		{Capability: provider.CapTTS, Provider: provider.ElevenLabs, Model: "tts-a", Voice: "v1", Role: "PRIMARY"},
		{Capability: provider.CapTTS, Provider: provider.ElevenLabs, Model: "tts-a", Voice: "v1", Role: "FALLBACK"},
		{Capability: provider.CapLLM, Provider: provider.DeepSeek, Model: "no-ratings"},
	}
	rs, err := bench.LoadHuman("testdata/human_ok.csv", cands)
	require.NoError(t, err)
	require.Len(t, rs, 3, "LLM-a, and tts-a once per role; the candidate without ratings has none")
	require.Equal(t, bench.SuiteHuman, rs[0].Suite)
	require.InDelta(t, (5+4+4+4+4+3)/6.0, rs[0].Values["quality_mean"], 1e-12, "mean of every given score; blank row skipped")
	require.InDelta(t, 4.25, rs[1].Values["mos"], 1e-12)
	require.InDelta(t, 1.0/3, rs[1].Values["kanji_err_rate"], 1e-12, "one misread in three rated sentences")
	require.Equal(t, "PRIMARY", rs[1].Role)
	require.Equal(t, "FALLBACK", rs[2].Role)
	m := bench.Aggregate(cands[0], rs)
	require.InDelta(t, 4.0, m.QualityMean, 1e-12)
}

func TestLoadHumanRejects(t *testing.T) {
	cands := []bench.Candidate{
		{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "llm-a"},
		{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "stt"},
	}
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "h.csv")
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	hdr := "item_id,reviewer,candidate,score_naturalness,score_level,score_character,mos,kanji_misread\n"
	for name, tc := range map[string]struct{ body, want string }{
		"unknown candidate": {hdr + "c1,r1,OPENAI/other,5,5,5,,\n", "not in the models file"},
		"score too high":    {hdr + "c1,r1,OPENAI/llm-a,6,5,5,,\n", "score_naturalness"},
		"score not number":  {hdr + "c1,r1,OPENAI/llm-a,x,5,5,,\n", "score_naturalness"},
		"misread not 0/1":   {hdr + "c1,r1,OPENAI/llm-a,,,,,2\n", "kanji_misread"},
		"stt rated":         {hdr + "c1,r1,OPENAI/stt,5,5,5,,\n", "STT"},
		"missing column":    {"item_id,candidate\nc1,OPENAI/llm-a\n", "column"},
	} {
		_, err := bench.LoadHuman(write(tc.body), cands)
		require.ErrorContains(t, err, tc.want, name)
	}
	_, err := bench.LoadHuman(filepath.Join(t.TempDir(), "none.csv"), cands)
	require.Error(t, err)
	_, err = bench.LoadHuman(write(""), cands)
	require.Error(t, err)
}
