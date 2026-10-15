package conversation

import (
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Exports for the external test package conversation_test, which exists because
// internal/testutil/fakes imports conversation (an internal test importing fakes would cycle).

// GenParams is genParams for external tests.
type GenParams = genParams

// NewGenerationForTest is newGeneration.
func NewGenerationForTest(p GenParams) *Generation { return newGeneration(p) }

// StartLLM starts runLLM + runTTS + the pacer.
func (g *Generation) StartLLM() { g.startLLM() }

// StartStatic starts runStatic(line) + runTTS + the pacer (S-2).
func (g *Generation) StartStatic(line string) { g.startStatic(line) }

// StartAsset starts playAsset + the pacer (APOLOGY).
func (g *Generation) StartAsset() { g.startAsset() }

// SpeakingRateForTest is the rate computed once for the generation.
func (g *Generation) SpeakingRateForTest() float64 { return g.speakingRate }

// VoiceSelectionFor is voiceSelection.
func VoiceSelectionFor(c content.CharacterSnapshot) provider.VoiceSelection { return voiceSelection(c) }

// SleepCtx is sleepCtx.
var SleepCtx = sleepCtx

// SentenceLimitTotal reads ai_sentence_limit_total{kind}.
func (m *Metrics) SentenceLimitTotal(kind string) float64 {
	return promtest.ToFloat64(m.aiSentenceLimit.WithLabelValues(kind))
}

// LLMTruncatedTotal reads llm_truncated_total.
func (m *Metrics) LLMTruncatedTotal() float64 { return promtest.ToFloat64(m.llmTruncated) }

// AudioFramesOutTotal reads audio_frames_out_total{result}.
func (m *Metrics) AudioFramesOutTotal(result string) float64 {
	return promtest.ToFloat64(m.audioFramesOut.WithLabelValues(result))
}

// FenceLostTotal reads fence_lost_total.
func (m *Metrics) FenceLostTotal() float64 { return promtest.ToFloat64(m.fenceLost) }

// RedisSyncPassHook runs f after every pass of the sync loop (the tests wait on it).
func RedisSyncPassHook(f func()) RedisSyncOption { return func(s *RedisSync) { s.passHook = f } }
