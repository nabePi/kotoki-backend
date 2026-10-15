package conversation

import (
	"context"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
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

// OpeningFallbackTotal reads kotoki_content_opening_fallback_total.
func (m *Metrics) OpeningFallbackTotal() float64 { return promtest.ToFloat64(m.openingFallback) }

// ConnectionsTotal reads connections_total{result}.
func (m *Metrics) ConnectionsTotal(result string) float64 {
	return promtest.ToFloat64(m.connections.WithLabelValues(result))
}

// TurnsTotal reads turns_total{status}.
func (m *Metrics) TurnsTotal(status string) float64 {
	return promtest.ToFloat64(m.turns.WithLabelValues(status))
}

// STTDiscardedTotal reads stt_discarded_commits_total{boundary}.
func (m *Metrics) STTDiscardedTotal(boundary string) float64 {
	return promtest.ToFloat64(m.sttDiscarded.WithLabelValues(boundary))
}

// PlaybackFinishedMissingTotal reads playback_finished_missing_total.
func (m *Metrics) PlaybackFinishedMissingTotal() float64 {
	return promtest.ToFloat64(m.playbackMissing)
}

// WSCloseTotal reads ws_close_total{code}.
func (m *Metrics) WSCloseTotal(code string) float64 {
	return promtest.ToFloat64(m.wsClose.WithLabelValues(code))
}

// ProtocolViolationsTotal reads protocol_violations_total{kind}.
func (m *Metrics) ProtocolViolationsTotal(kind string) float64 {
	return promtest.ToFloat64(m.protocolViolations.WithLabelValues(kind))
}

// SetResumeHook replaces the resume flow of the Manager (Task 21's seam) with one that reports
// the last_server_seq it was asked to resume from and refuses the connection.
func SetResumeHook(m *Manager, f func(lastServerSeq int64)) {
	m.resume = func(_ context.Context, h *handshake, _ session.RuntimeRecord, last int64) error {
		f(last)
		return h.fail(errs.CodeServiceUnavailable)
	}
}
