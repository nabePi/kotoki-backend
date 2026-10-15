package conversation

import (
	"context"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/learner"
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

// AudioFramesInTotal reads audio_frames_in_total{result}.
func (m *Metrics) AudioFramesInTotal(result string) float64 {
	return promtest.ToFloat64(m.audioFramesIn.WithLabelValues(result))
}

// TurnResumedTotal reads turn_resumed_total.
func (m *Metrics) TurnResumedTotal() float64 { return promtest.ToFloat64(m.turnResumed) }

// TurnEndReasonTotal reads turn_end_reason_total{reason}.
func (m *Metrics) TurnEndReasonTotal(reason string) float64 {
	return promtest.ToFloat64(m.turnEndReason.WithLabelValues(reason))
}

// DifficultyAdjustmentsTotal reads difficulty_adjustments_total.
func (m *Metrics) DifficultyAdjustmentsTotal() float64 {
	return promtest.ToFloat64(m.difficultyAdjusted)
}

// BargeInTotal reads bargein_total{source}.
func (m *Metrics) BargeInTotal(source string) float64 {
	return promtest.ToFloat64(m.bargeins.WithLabelValues(source))
}

// BargeInStopSamples is how many bargein_stop_seconds observations were made.
func (m *Metrics) BargeInStopSamples() uint64 {
	var out dto.Metric
	if err := m.bargeinStop.Write(&out); err != nil {
		panic(err)
	}
	return out.GetHistogram().GetSampleCount()
}

// LLMContentFilteredTotal reads kotoki_llm_content_filtered_total{provider}.
func (m *Metrics) LLMContentFilteredTotal(provider string) float64 {
	return promtest.ToFloat64(m.llmContentFilter.WithLabelValues(provider))
}

// GenerationExitSamples is how many generation_goroutine_exit_seconds observations were made.
func (m *Metrics) GenerationExitSamples() uint64 {
	var out dto.Metric
	if err := m.genExit.Write(&out); err != nil {
		panic(err)
	}
	return out.GetHistogram().GetSampleCount()
}

// BargeInBehindTheLoop runs a barge-in on the loop at a moment the loop is known to be behind the
// generation: it closes entered, waits for proceed (the test makes the generation produce events
// meanwhile, which the loop cannot read: it is in here) and then confirms the barge-in. It returns
// how many generation events were still unread.
func (r *Runtime) BargeInBehindTheLoop(ctx context.Context, entered chan<- struct{}, proceed <-chan struct{}) (int, error) {
	var unread int
	err := r.call(ctx, func(r *Runtime) {
		close(entered)
		<-proceed
		unread = len(r.genEvents)
		r.bargeIn(r.det.NowMs())
	})
	return unread, err
}

// FlushPersister waits until every job the runtime submitted is written.
func (r *Runtime) FlushPersister(ctx context.Context) error { return r.persister.Flush(ctx) }

// AdaptiveForTest is the adaptive difficulty the runtime currently uses.
func (r *Runtime) AdaptiveForTest(ctx context.Context) (learner.AdaptiveDifficulty, error) {
	var ad learner.AdaptiveDifficulty
	err := r.call(ctx, func(r *Runtime) { ad = r.adaptive.Current })
	return ad, err
}

// HandleTakeover is what the TAKEOVER subscriber does with a message, called synchronously.
func HandleTakeover(m *Manager, t rtstate.Takeover) { m.onTakeover(t) }

// PersisterPending is how many jobs wait behind the one the persister is writing.
func (r *Runtime) PersisterPending() int { return r.persister.Pending() }

// PanicOnLoop makes the loop panic: the panic of Runtime.loop itself (§7.7.2) that recoverLoop ends.
func (r *Runtime) PanicOnLoop(ctx context.Context) error {
	return r.call(ctx, func(*Runtime) { panic("scripted panic in the runtime loop") })
}
