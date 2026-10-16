package bench_test

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// sampleResults is a small but complete run: one STT (passes the latency/CER gates, no concurrency),
// one LLM with everything measured, one TTS, an e2e stack, with an error row in each.
func sampleResults() []bench.Result {
	stt := bench.Candidate{Capability: provider.CapSTT, Provider: provider.Cartesia, Model: "ink"}
	rs := []bench.Result{
		sttRes(stt, bench.SetSTTNative, 100, 0, 10), sttRes(stt, bench.SetSTTNative, 200, 0, 10), sttRes(stt, bench.SetSTTLearner, 150, 1, 20),
		{Suite: bench.SuiteSTT, Capability: "STT", Provider: "CARTESIA", Model: "ink", Set: bench.SetSTTNative, ItemID: "e", Attempt: 1, ErrorKind: "TIMEOUT"},
	}
	for _, v := range []float64{300, 500, 700, 750} {
		rs = append(rs, bench.Result{Suite: bench.SuiteLLM, Capability: "LLM", Provider: "OPENAI", Model: "gpt", Values: map[string]float64{"first_sentence_ms": v}, Flags: map[string]bool{"format_ok": true}})
	}
	rs = append(rs,
		bench.Result{Suite: bench.SuiteLLMJSON, Capability: "LLM", Provider: "OPENAI", Model: "gpt", Values: map[string]float64{"calls": 1}, Flags: map[string]bool{"first_pass_ok": true, "after_repair_ok": true}},
		bench.Result{Suite: bench.SuiteHuman, Capability: "LLM", Provider: "OPENAI", Model: "gpt", Values: map[string]float64{"quality_mean": 4.3}},
		bench.Result{Suite: bench.SuiteConcurrency, Capability: "LLM", Provider: "OPENAI", Model: "gpt", Values: map[string]float64{"step": 100, "error_pct": 0.5, "p50_ms": 10, "p95_ms": 20, "rate_limited_pct": 0.5}},
		bench.Result{Suite: bench.SuiteConcurrency, Capability: "LLM", Provider: "OPENAI", Model: "gpt", Values: map[string]float64{"step": 10, "error_pct": 0, "p50_ms": 5, "p95_ms": 8, "rate_limited_pct": 0}},
		bench.Result{Suite: bench.SuiteTTS, Capability: "TTS", Provider: "ELEVENLABS", Model: "eleven", Voice: "v1", Role: "PRIMARY", Values: map[string]float64{"first_audio_ms": 350}},
		bench.Result{Suite: bench.SuiteTTS, Capability: "TTS", Provider: "ELEVENLABS", Model: "eleven", Voice: "v1", Role: "PRIMARY", ErrorKind: "UNAVAILABLE"},
		bench.Result{Suite: bench.SuiteE2E, Capability: "E2E", Provider: "CARTESIA+OPENAI+ELEVENLABS", Model: "ink+gpt+eleven", Voice: "v1", Values: map[string]float64{"e2e_turn_end_to_first_audio_ms": 1500}},
	)
	return rs
}

func TestWriteResultsJSONLAndCSV(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "2026-10-17")
	rs := sampleResults()
	require.NoError(t, bench.WriteResults(dir, rs))

	f, err := os.Open(filepath.Join(dir, "results.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var back []bench.Result
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r bench.Result
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		back = append(back, r)
	}
	require.Len(t, back, len(rs), "one line per attempt")
	require.Equal(t, rs[0].Values, back[0].Values)
	require.Equal(t, "TIMEOUT", back[3].ErrorKind)

	b, err := os.ReadFile(filepath.Join(dir, "summary.csv"))
	require.NoError(t, err)
	recs, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"suite", "capability", "provider", "model", "voice", "metric", "p50", "p95", "gate", "pass"}, recs[0])
	for _, r := range recs {
		require.Len(t, r, 10)
	}
	find := func(capab, suite, metric string) []string {
		for _, r := range recs[1:] {
			if r[1] == capab && r[0] == suite && r[5] == metric {
				return r
			}
		}
		t.Fatalf("no row %s/%s/%s", capab, suite, metric)
		return nil
	}
	r := find("STT", "stt", "final_after_finalize_ms")
	require.Equal(t, []string{"150", "200"}, []string{r[6], r[7]})
	r = find("STT", "stt", "error_pct")
	require.Equal(t, "25", r[6], "one of four attempts failed")

	r = find("STT", "stt", "stt_final_ms")
	require.Equal(t, []string{"200", "stt_final_ms<=300", "true"}, []string{r[7], r[8], r[9]})
	r = find("STT", "stt", "cer_native")
	require.Equal(t, []string{"0", "cer_native<=0.08", "true"}, []string{r[7], r[8], r[9]})
	r = find("STT", "stt", "cer_learner")
	require.Equal(t, []string{"0.05", "cer_learner<=0.15", "true"}, []string{r[7], r[8], r[9]})
	r = find("STT", "concurrency", "error_rate_conc100")
	require.Equal(t, []string{"", "false"}, []string{r[7], r[9]}, "step 100 was not run: no value, the gate fails")
	require.Equal(t, "false", find("STT", "gates", "all_gates")[9])

	r = find("LLM", "llm", "llm_first_sentence_ms")
	require.Equal(t, []string{"750", "llm_first_sentence_ms<=800", "true"}, []string{r[7], r[8], r[9]})
	r = find("LLM", "llm", "format_ok")
	require.Equal(t, []string{"1", "format_ok>=0.97", "true"}, []string{r[7], r[8], r[9]})
	r = find("LLM", "llm-json", "json_after_repair_ok")
	require.Equal(t, []string{"1", "json_after_repair_ok>=0.98", "true"}, []string{r[7], r[8], r[9]})
	r = find("LLM", "human", "quality_mean")
	require.Equal(t, []string{"4.3", "quality_mean>=4", "true"}, []string{r[7], r[8], r[9]})
	r = find("LLM", "concurrency", "error_rate_conc100")
	require.Equal(t, []string{"0.005", "error_rate_conc100<=0.01", "true"}, []string{r[7], r[8], r[9]})
	r = find("LLM", "concurrency", "p95_ms@100")
	require.Equal(t, "20", r[7])
	r = find("LLM", "concurrency", "p95_ms@10")
	require.Equal(t, "8", r[7])
	require.Equal(t, "true", find("LLM", "gates", "all_gates")[9])

	r = find("TTS", "tts", "tts_first_audio_ms")
	require.Equal(t, []string{"350", "tts_first_audio_ms<=400", "true"}, []string{r[7], r[8], r[9]})
	r = find("TTS", "human", "mos")
	require.Equal(t, []string{"", "mos>=4", "false"}, []string{r[7], r[8], r[9]}, "no human rating yet")
	require.Equal(t, "v1", r[4])
	require.Equal(t, "50", find("TTS", "tts", "error_pct")[6])

	r = find("E2E", "e2e", "e2e_turn_end_to_first_audio_ms")
	require.Equal(t, "1500", r[7])
	for _, r := range recs[1:] {
		if r[1] == "E2E" {
			require.Empty(t, r[8], "an e2e stack has no gate")
		}
	}
}

func TestWriteResultsReplacesAndQuotes(t *testing.T) {
	dir := t.TempDir()
	rs := []bench.Result{{Suite: bench.SuiteTTS, Capability: "TTS", Provider: "CARTESIA", Model: `m,"x"`, Voice: "v", Role: "PRIMARY", Values: map[string]float64{"first_audio_ms": 1}}}
	require.NoError(t, bench.WriteResults(dir, sampleResults()))
	require.NoError(t, bench.WriteResults(dir, rs))
	b, err := os.ReadFile(filepath.Join(dir, "results.jsonl"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(b), "\n"))
	s, err := os.ReadFile(filepath.Join(dir, "summary.csv"))
	require.NoError(t, err)
	recs, err := csv.NewReader(bytes.NewReader(s)).ReadAll()
	require.NoError(t, err, "valid CSV even with commas and quotes in a model name")
	require.Equal(t, `m,"x"`, recs[1][3])
	require.Error(t, bench.WriteResults(filepath.Join(dir, "results.jsonl", "sub"), rs), "the directory cannot be created under a file")
}

func TestWriteResultsNoSecretOrText(t *testing.T) {
	rs := sampleResults()
	dir := t.TempDir()
	require.NoError(t, bench.WriteResults(dir, rs))
	for _, n := range []string{"results.jsonl", "summary.csv"} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		require.NoError(t, err)
		require.NotContains(t, strings.ToLower(string(b)), "api_key")
		require.NotContains(t, string(b), "reference")
	}
}

func scored(c bench.Candidate, m bench.Metrics) *bench.ScoredCandidate {
	return &bench.ScoredCandidate{Candidate: c, Metrics: m}
}

func recEnvInputs() (stt, llm *bench.ScoredCandidate, backups []bench.ScoredCandidate) {
	stt = scored(bench.Candidate{Capability: provider.CapSTT, Provider: provider.Cartesia, Model: "ink-x"},
		bench.Metrics{P95: map[string]float64{"stt_final_ms": 250}, CERNative: 0.05, CERLearner: 0.09})
	llm = scored(bench.Candidate{Capability: provider.CapLLM, Provider: provider.DeepSeek, Model: "ds-model"},
		bench.Metrics{P95: map[string]float64{"llm_first_sentence_ms": 600}, QualityMean: 4.4, JSONAfterRepairOK: 0.99, CostPerMinUSD: 0.12})
	backups = []bench.ScoredCandidate{*scored(bench.Candidate{Capability: provider.CapSTT, Provider: provider.OpenAI, Model: "oa-stt"},
		bench.Metrics{P95: map[string]float64{"stt_final_ms": 280}, CERNative: 0.06, CERLearner: 0.1})}
	return stt, llm, backups
}

func TestWriteRecommendationEnvFormat(t *testing.T) {
	stt, llm, backups := recEnvInputs()
	llmBackups := []bench.ScoredCandidate{*scored(bench.Candidate{Capability: provider.CapLLM, Provider: provider.OpenAI, Model: "oa-llm"},
		bench.Metrics{P95: map[string]float64{"llm_first_sentence_ms": 700}, QualityMean: 4.1, JSONAfterRepairOK: 0.98, CostPerMinUSD: 0.5})}
	path := filepath.Join(t.TempDir(), "deploy", "provider.env")
	err := bench.WriteRecommendationEnv(path, stt, llm,
		map[provider.Name]string{provider.OpenAI: "oa-stt", provider.ElevenLabs: "scribe-x", provider.Cartesia: "stale-ink"},
		map[provider.Name]string{provider.OpenAI: "oa-llm", provider.DeepSeek: "stale"},
		map[provider.Name]string{provider.OpenAI: "oa-eval"},
		map[string]string{"CARTESIA_TTS": "plan Pro: 20 concurrent\nrequests", "OPENAI_STT": "tier 2"},
		bench.WithBackups(backups, llmBackups),
		bench.WithTTSVendors([]provider.Name{provider.Cartesia}),
		bench.WithConcurrency(map[string]int{"CARTESIA_TTS": 20, "OPENAI_LLM": 60}),
		bench.WithLLMLimits(300, 0.8))
	require.NoError(t, err)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(b)
	lines := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		lines[l] = true
	}

	require.Contains(t, got, "REKOMENDASI")
	require.Contains(t, got, "keputusan akhir vendor aktif manual pemilik produk")
	// exactly one STT flag and one LLM flag are true
	require.True(t, lines["CARTESIA_STT_ENABLED=true"])
	require.True(t, lines["OPENAI_STT_ENABLED=false"])
	require.True(t, lines["ELEVENLABS_STT_ENABLED=false"])
	require.True(t, lines["DEEPSEEK_LLM_ENABLED=true"])
	require.True(t, lines["OPENAI_LLM_ENABLED=false"])
	require.Equal(t, 1, strings.Count(got, "\n"+"CARTESIA_STT_ENABLED=true")+strings.Count(got, "\n"+"OPENAI_STT_ENABLED=true")+strings.Count(got, "\nELEVENLABS_STT_ENABLED=true"))
	// models: the recommended model of its vendor wins over the map, other vendors come from the map
	require.True(t, lines["CARTESIA_STT_MODEL=ink-x"])
	require.True(t, lines["OPENAI_STT_MODEL=oa-stt"])
	require.True(t, lines["ELEVENLABS_STT_MODEL=scribe-x"])
	require.True(t, lines["DEEPSEEK_LLM_MODEL=ds-model"])
	require.True(t, lines["OPENAI_LLM_MODEL=oa-llm"])
	require.True(t, lines["OPENAI_EVALUATION_MODEL=oa-eval"])
	require.True(t, lines["DEEPSEEK_EVALUATION_MODEL=ds-model"], "evaluation defaults to the LLM model of the recommended vendor")
	require.True(t, lines["ROLLING_SUMMARY_LLM_MODEL=ds-model"])
	require.True(t, lines["LLM_MAX_OUTPUT_TOKENS=300"])
	require.True(t, lines["LLM_TEMPERATURE=0.8"])
	// TTS follows character-voices.json
	require.True(t, lines["CARTESIA_TTS_ENABLED=true"])
	require.True(t, lines["ELEVENLABS_TTS_ENABLED=false"])
	// concurrency with the plan-limit comments, on one line
	require.True(t, lines["PROVIDER_CONCURRENCY_CARTESIA_TTS=20"])
	require.True(t, lines["PROVIDER_CONCURRENCY_OPENAI_LLM=60"])
	require.Contains(t, got, "# limit plan akun: plan Pro: 20 concurrent requests\nPROVIDER_CONCURRENCY_CARTESIA_TTS=20")
	require.Contains(t, got, "# PROVIDER_CONCURRENCY_OPENAI_STT (belum diisi) - limit plan akun: tier 2")
	// backups: commented switches with their numbers, never active
	require.Contains(t, got, "# backup manual: OPENAI_STT_ENABLED=true (OPENAI/oa-stt; CER native 0.06, learner 0.1, final P95 280 ms)")
	require.Contains(t, got, "# backup manual: OPENAI_LLM_ENABLED=true (OPENAI/oa-llm; kualitas 4.1, 0.5 USD/menit")
	require.False(t, lines["OPENAI_STT_ENABLED=true"])
	// no secrets, no removed variables
	for _, banned := range []string{"_API_KEY", "PROVIDER_TTS_", "PROVIDER_PROFILE_", "PROVIDER_LLM_", "PROVIDER_STT_", "DEEPGRAM", "GEMINI", "_TIER", "FALLBACK_MODEL"} {
		require.NotContains(t, got, banned)
	}
}

func TestWriteRecommendationEnvNoRecommendation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.env")
	require.NoError(t, bench.WriteRecommendationEnv(path, nil, nil, nil, nil, nil, nil))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(b)
	require.NotContains(t, got, "OPENAI_STT_ENABLED=true")
	require.NotContains(t, got, "CARTESIA_STT_ENABLED=true")
	require.NotContains(t, got, "OPENAI_LLM_ENABLED=true")
	require.NotContains(t, got, "DEEPSEEK_LLM_ENABLED=true")
	require.NotContains(t, got, "ROLLING_SUMMARY_LLM_MODEL")
	require.Contains(t, got, "gate tidak diturunkan")
	require.Contains(t, got, "# OPENAI_STT_ENABLED=  (belum ditentukan)")
	require.Contains(t, got, "ELEVENLABS_TTS_ENABLED=true", "TTS keeps its defaults without character voices")
	require.Contains(t, got, "CARTESIA_TTS_ENABLED=true")
}

func TestWriteRecommendationEnvRejectsBadValues(t *testing.T) {
	stt, llm, _ := recEnvInputs()
	p := filepath.Join(t.TempDir(), "p.env")
	bad := *stt
	bad.Model = "x\nOPENAI_API_KEY=leak"
	require.Error(t, bench.WriteRecommendationEnv(p, &bad, llm, nil, nil, nil, nil), "a model with a newline cannot inject a variable")
	require.Error(t, bench.WriteRecommendationEnv(p, stt, llm, map[provider.Name]string{provider.OpenAI: "a b"}, nil, nil, nil))
	require.Error(t, bench.WriteRecommendationEnv(p, stt, llm, nil, nil, nil, map[string]string{"bad key": "x"}))
	require.Error(t, bench.WriteRecommendationEnv(p, stt, llm, nil, nil, nil, nil, bench.WithConcurrency(map[string]int{"OPENAI_LLM": 0})))
	require.Error(t, bench.WriteRecommendationEnv(p, stt, llm, nil, nil, nil, nil, bench.WithConcurrency(map[string]int{"NOPE_LLM": 5})))
	require.Error(t, bench.WriteRecommendationEnv(p, stt, llm, nil, nil, nil, nil, bench.WithLLMLimits(0, 0.8)))
	// a comment cannot start a second line either
	require.NoError(t, bench.WriteRecommendationEnv(p, stt, llm, nil, nil, nil, map[string]string{"OPENAI_LLM": "tier\nOPENAI_LLM_ENABLED=true"}, bench.WithConcurrency(map[string]int{"OPENAI_LLM": 5})))
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NotContains(t, string(b), "\nOPENAI_LLM_ENABLED=true")
}

func TestWriteCharacterVoices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "res", "character-voices.json")
	err := bench.WriteCharacterVoices(path, []bench.CharacterVoice{
		{Code: "HARUKA", Primary: bench.VoiceChoice{ProviderName: "ELEVENLABS", ModelName: "m1", VoiceID: "v1"},
			Fallback: &bench.VoiceChoice{ProviderName: "CARTESIA", ModelName: "sonic-x", VoiceID: "cv"}},
		{Code: "AOI", Primary: bench.VoiceChoice{ProviderName: "CARTESIA", ModelName: "m2", VoiceID: "v2"}, Notes: []string{"tanpa fallback TTS: kegagalan → APOLOGY (D-042)"}},
		{Code: "KENJI", Notes: []string{bench.NoQualifierNote}},
	})
	require.NoError(t, err)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var got struct {
		Characters []map[string]any `json:"characters"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got.Characters, 3)
	require.Equal(t, map[string]any{
		"code": "HARUKA", "provider_name": "ELEVENLABS", "model_name": "m1", "voice_id": "v1",
		"fallback": map[string]any{"provider_name": "CARTESIA", "model_name": "sonic-x", "voice_id": "cv"},
	}, got.Characters[0])
	require.NotContains(t, got.Characters[1], "fallback")
	require.Equal(t, "CARTESIA", got.Characters[1]["provider_name"])
	require.Equal(t, []any{"tanpa fallback TTS: kegagalan → APOLOGY (D-042)"}, got.Characters[1]["notes"])
	require.Equal(t, "", got.Characters[2]["provider_name"], "no qualified voice: empty primary")
	require.Equal(t, "", got.Characters[2]["voice_id"])
	require.True(t, strings.HasSuffix(string(b), "}\n"))
}
