package bench_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func sttScored(prov provider.Name, model string, final, cerN, cerL float64) bench.ScoredCandidate {
	return bench.ScoredCandidate{
		Candidate: bench.Candidate{Capability: provider.CapSTT, Provider: prov, Model: model},
		Metrics:   bench.Metrics{P95: map[string]float64{"stt_final_ms": final}, CERNative: cerN, CERLearner: cerL},
	}
}

func llmScored(prov provider.Name, model string, cost, quality float64, json float64) bench.ScoredCandidate {
	return bench.ScoredCandidate{
		Candidate: bench.Candidate{Capability: provider.CapLLM, Provider: prov, Model: model},
		Metrics: bench.Metrics{P95: map[string]float64{"llm_first_sentence_ms": 500}, FormatOK: 0.99, QualityMean: quality,
			JSONAfterRepairOK: json, CostPerMinUSD: cost},
	}
}

func ttsScored(prov provider.Name, model, voice, role string, chars []string, mos, kanji, ttfa, cost float64) bench.ScoredCandidate {
	return bench.ScoredCandidate{
		Candidate: bench.Candidate{Capability: provider.CapTTS, Provider: prov, Model: model, Voice: voice, Role: role, Characters: chars},
		Metrics:   bench.Metrics{P95: map[string]float64{"tts_first_audio_ms": ttfa}, MOS: mos, KanjiErrRate: kanji, CostPerMinUSD: cost},
	}
}

func joined(notes []string) string { return strings.Join(notes, "\n") }

func TestRecommendSTT(t *testing.T) {
	openai := sttScored(provider.OpenAI, "a", 250, 0.06, 0.10)
	cartesia := sttScored(provider.Cartesia, "b", 280, 0.05, 0.09)
	eleven := sttScored(provider.ElevenLabs, "c", 350, 0.01, 0.01) // best CER but fails the latency gate
	rec, backups, notes := bench.RecommendSTT([]bench.ScoredCandidate{openai, eleven, cartesia})
	require.NotNil(t, rec)
	require.Equal(t, provider.Cartesia, rec.Provider)
	require.Len(t, backups, 1)
	require.Equal(t, provider.OpenAI, backups[0].Provider)
	n := joined(notes)
	require.Contains(t, n, "OPENAI_STT_ENABLED=true", "the backup is reported as a manual switch")
	require.Contains(t, n, "0.06", "with its numbers")
	require.Contains(t, n, "ELEVENLABS/c gagal gate")
	require.Contains(t, n, "stt_final_ms 350 (batas 300)", "the failing gate with actual and limit")
	require.NotContains(t, n, "ELEVENLABS_STT_ENABLED=true", "a failed candidate is not a backup")
}

func TestRecommendSTTTieBreaksOnLatency(t *testing.T) {
	a := sttScored(provider.OpenAI, "a", 250, 0.05, 0.05)
	b := sttScored(provider.Cartesia, "b", 200, 0.05, 0.05)
	rec, _, _ := bench.RecommendSTT([]bench.ScoredCandidate{a, b})
	require.Equal(t, provider.Cartesia, rec.Provider)
}

func TestRecommendSTTIgnoresOtherCapabilities(t *testing.T) {
	rec, backups, _ := bench.RecommendSTT([]bench.ScoredCandidate{llmScored(provider.OpenAI, "x", 1, 5, 1), sttScored(provider.OpenAI, "a", 100, 0.01, 0.01)})
	require.Equal(t, "a", rec.Model)
	require.Empty(t, backups)
}

func TestRecommendNoneQualifies(t *testing.T) {
	rec, backups, notes := bench.RecommendSTT([]bench.ScoredCandidate{sttScored(provider.OpenAI, "a", 400, 0.06, 0.1), sttScored(provider.Cartesia, "b", 280, 0.2, 0.3)})
	require.Nil(t, rec)
	require.Empty(t, backups)
	require.Contains(t, joined(notes), "gate tidak diturunkan")
	require.Contains(t, joined(notes), "stt_final_ms 400", "the actual numbers are reported")
	rec, _, notes = bench.RecommendSTT(nil)
	require.Nil(t, rec)
	require.Contains(t, joined(notes), "gate tidak diturunkan")

	rec, _, notes = bench.RecommendLLM([]bench.ScoredCandidate{llmScored(provider.OpenAI, "a", 1, 3.0, 1)}, 0)
	require.Nil(t, rec)
	require.Contains(t, joined(notes), "gate tidak diturunkan")
	require.Contains(t, joined(notes), "quality_mean 3 (batas 4)")
}

func TestRecommendLLM(t *testing.T) {
	a := llmScored(provider.OpenAI, "a", 1, 4.1, 0.99)
	b := llmScored(provider.OpenAI, "b", 2.5, 4.6, 0.99)
	c := llmScored(provider.DeepSeek, "c", 4, 4.9, 0.99) // above 3x the cheapest passing candidate
	d := llmScored(provider.DeepSeek, "d", 0.5, 5, 0.90) // fails the JSON gate: not even a candidate
	rec, backups, notes := bench.RecommendLLM([]bench.ScoredCandidate{a, b, c, d}, 0)
	require.NotNil(t, rec)
	require.Equal(t, "b", rec.Model)
	require.Len(t, backups, 1, "only passing candidates of the other vendor")
	require.Equal(t, "c", backups[0].Model)
	n := joined(notes)
	require.Contains(t, n, "DEEPSEEK_LLM_ENABLED=true")
	require.Contains(t, n, "DEEPSEEK/c lolos gate tetapi biaya 4 USD/menit melebihi batas 3")
	require.Contains(t, n, "DEEPSEEK/d gagal gate")
	require.Contains(t, n, "json_after_repair_ok 0.9")
}

func TestRecommendLLMExplicitCostLimit(t *testing.T) {
	a := llmScored(provider.OpenAI, "a", 1, 4.1, 0.99)
	b := llmScored(provider.OpenAI, "b", 2.5, 4.6, 0.99)
	c := llmScored(provider.DeepSeek, "c", 4, 4.9, 0.99)
	rec, _, _ := bench.RecommendLLM([]bench.ScoredCandidate{a, b, c}, 5)
	require.Equal(t, "c", rec.Model, "a 5 USD/minute limit admits the best quality")
	rec, _, _ = bench.RecommendLLM([]bench.ScoredCandidate{a, b, c}, 1)
	require.Equal(t, "a", rec.Model)
	rec, _, notes := bench.RecommendLLM([]bench.ScoredCandidate{a, b, c}, 0.5)
	require.Nil(t, rec)
	require.Contains(t, joined(notes), "gate tidak diturunkan")
}

func TestRecommendLLMTiesAndUnknownPrice(t *testing.T) {
	a := llmScored(provider.OpenAI, "a", 2, 4.5, 0.99)
	b := llmScored(provider.DeepSeek, "b", 1, 4.5, 0.99)
	rec, _, _ := bench.RecommendLLM([]bench.ScoredCandidate{a, b}, 0)
	require.Equal(t, "b", rec.Model, "equal quality: the cheaper one")

	cheapNoPrice := llmScored(provider.DeepSeek, "np", 0, 4.9, 0.99)
	cheapNoPrice.Metrics.CostMissing = true
	rec, backups, notes := bench.RecommendLLM([]bench.ScoredCandidate{a, cheapNoPrice}, 0)
	require.Equal(t, "a", rec.Model, "a candidate without a price cannot be checked against the limit")
	require.Contains(t, joined(notes), "tidak punya harga")
	require.Len(t, backups, 1)

	zero := llmScored(provider.OpenAI, "z", 0, 4.2, 0.99)
	_, _, notes = bench.RecommendLLM([]bench.ScoredCandidate{zero}, 0)
	require.Contains(t, joined(notes), "harga belum terisi")
}

func TestRecommendCharacterVoices(t *testing.T) {
	el := ttsScored(provider.ElevenLabs, "m1", "ev", "PRIMARY", []string{"HARUKA", "KENJI"}, 4.5, 0.02, 300, 1)
	ca := ttsScored(provider.Cartesia, "m2", "cv", "PRIMARY", []string{"HARUKA", "AOI"}, 4.3, 0.02, 200, 1)
	chars := []bench.CharacterSpec{{Code: "HARUKA"}, {Code: "AOI"}, {Code: "KENJI"}, {Code: "RIKU"}}
	got := bench.RecommendCharacterVoices([]bench.ScoredCandidate{el, ca}, chars)
	require.Len(t, got, 4)

	h := got[0]
	require.Equal(t, "HARUKA", h.Code)
	require.Equal(t, bench.VoiceChoice{ProviderName: "ELEVENLABS", ModelName: "m1", VoiceID: "ev"}, h.Primary)
	require.NotNil(t, h.Fallback)
	require.Equal(t, bench.VoiceChoice{ProviderName: "CARTESIA", ModelName: "m2", VoiceID: "cv"}, *h.Fallback)
	require.Empty(t, h.Notes)

	// AOI is nominated only for the Cartesia voice: no other vendor remains for the fallback
	aoi := got[1]
	require.Equal(t, "CARTESIA", aoi.Primary.ProviderName)
	require.Nil(t, aoi.Fallback)
	require.Contains(t, joined(aoi.Notes), "tanpa fallback TTS: kegagalan → APOLOGY (D-042)")

	// KENJI: only the ElevenLabs voice nominates him
	require.Equal(t, "ELEVENLABS", got[2].Primary.ProviderName)
	require.Nil(t, got[2].Fallback)

	// RIKU: nobody nominates him
	riku := got[3]
	require.Equal(t, bench.VoiceChoice{}, riku.Primary)
	require.Nil(t, riku.Fallback)
	require.Contains(t, joined(riku.Notes), "gate tidak diturunkan")
}

func TestRecommendCharacterVoicesEmptyListMeansAll(t *testing.T) {
	el := ttsScored(provider.ElevenLabs, "m1", "ev", "PRIMARY", nil, 4.5, 0.02, 300, 1)
	got := bench.RecommendCharacterVoices([]bench.ScoredCandidate{el}, []bench.CharacterSpec{{Code: "X"}, {Code: "Y"}})
	require.Equal(t, "ELEVENLABS", got[0].Primary.ProviderName)
	require.Equal(t, "ELEVENLABS", got[1].Primary.ProviderName)
}

func TestRecommendCharacterVoicesNeverSameVendorFallback(t *testing.T) {
	best := ttsScored(provider.ElevenLabs, "m1", "ev1", "PRIMARY", nil, 4.8, 0.01, 300, 1)
	sameVendor := ttsScored(provider.ElevenLabs, "m2", "ev2", "PRIMARY", nil, 4.6, 0.01, 300, 1)
	other := ttsScored(provider.Cartesia, "m3", "cv", "FALLBACK", nil, 3.6, 0.04, 550, 1) // passes the fallback gates only
	got := bench.RecommendCharacterVoices([]bench.ScoredCandidate{best, sameVendor, other}, []bench.CharacterSpec{{Code: "HARUKA"}})
	require.Equal(t, "ev1", got[0].Primary.VoiceID)
	require.NotNil(t, got[0].Fallback)
	require.Equal(t, "CARTESIA", got[0].Fallback.ProviderName, "the better ev2 is the same vendor as Primary")

	got = bench.RecommendCharacterVoices([]bench.ScoredCandidate{best, sameVendor}, []bench.CharacterSpec{{Code: "HARUKA"}})
	require.Nil(t, got[0].Fallback)
}

func TestRecommendCharacterVoicesRoleAndGates(t *testing.T) {
	// a FALLBACK-role candidate never becomes a main voice, even with the best MOS
	fb := ttsScored(provider.Cartesia, "m", "cv", "FALLBACK", nil, 4.9, 0.01, 200, 1)
	got := bench.RecommendCharacterVoices([]bench.ScoredCandidate{fb}, []bench.CharacterSpec{{Code: "A"}})
	require.Equal(t, bench.VoiceChoice{}, got[0].Primary)

	// main voice gates are stricter: MOS 3.8 passes only the fallback gates
	weak := ttsScored(provider.ElevenLabs, "m", "ev", "PRIMARY", nil, 3.8, 0.02, 300, 1)
	got = bench.RecommendCharacterVoices([]bench.ScoredCandidate{weak}, []bench.CharacterSpec{{Code: "A"}})
	require.Equal(t, bench.VoiceChoice{}, got[0].Primary)
	require.Contains(t, joined(got[0].Notes), "gate tidak diturunkan")

	// a candidate that is too slow for the main gate but fine as a fallback
	slow := ttsScored(provider.Cartesia, "m2", "cv", "PRIMARY", nil, 4.4, 0.02, 500, 1)
	main := ttsScored(provider.ElevenLabs, "m1", "ev", "PRIMARY", nil, 4.5, 0.02, 300, 1)
	got = bench.RecommendCharacterVoices([]bench.ScoredCandidate{slow, main}, []bench.CharacterSpec{{Code: "A"}})
	require.Equal(t, "ELEVENLABS", got[0].Primary.ProviderName)
	require.NotNil(t, got[0].Fallback)
	require.Equal(t, "CARTESIA", got[0].Fallback.ProviderName)

	// an unmeasured metric never qualifies
	un := bench.ScoredCandidate{Candidate: bench.Candidate{Capability: provider.CapTTS, Provider: provider.ElevenLabs, Model: "m", Voice: "v", Role: "PRIMARY"}}
	got = bench.RecommendCharacterVoices([]bench.ScoredCandidate{un}, []bench.CharacterSpec{{Code: "A"}})
	require.Equal(t, bench.VoiceChoice{}, got[0].Primary)
}

func TestRecommendCharacterVoicesTieBreaks(t *testing.T) {
	a := ttsScored(provider.ElevenLabs, "m1", "a", "PRIMARY", nil, 4.5, 0.01, 300, 2)
	b := ttsScored(provider.Cartesia, "m2", "b", "PRIMARY", nil, 4.5, 0.01, 300, 1)
	got := bench.RecommendCharacterVoices([]bench.ScoredCandidate{a, b}, []bench.CharacterSpec{{Code: "A"}})
	require.Equal(t, "CARTESIA", got[0].Primary.ProviderName, "equal MOS: the cheaper per minute")
	b.Metrics.CostPerMinUSD = 2
	b.Metrics.P95["tts_first_audio_ms"] = 250
	got = bench.RecommendCharacterVoices([]bench.ScoredCandidate{a, b}, []bench.CharacterSpec{{Code: "A"}})
	require.Equal(t, "CARTESIA", got[0].Primary.ProviderName, "equal MOS and cost: the lower first audio")
}

// TestRecommendNeverAutoSelects: the recommenders only return values; they cannot write a switch.
func TestRecommendNeverAutoSelects(t *testing.T) {
	in := []bench.ScoredCandidate{sttScored(provider.OpenAI, "a", 100, 0.01, 0.01), sttScored(provider.Cartesia, "b", 100, 0.02, 0.02)}
	snapshot := append([]bench.ScoredCandidate(nil), in...)
	rec, backups, _ := bench.RecommendSTT(in)
	require.NotNil(t, rec)
	require.Len(t, backups, 1, "the other qualifying vendor is a manual backup, not a second selection")
	require.Equal(t, snapshot[0].Model, in[0].Model, "the input is not reordered or mutated")
	require.Equal(t, snapshot[1].Model, in[1].Model)
}
