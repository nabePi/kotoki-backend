//go:build e2e

package e2e

import (
	"math"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

const (
	bargeAttempts = 200
	// bargeStopP95 is D-101: barge-in confirmed to the last AI audio frame written.
	bargeStopP95 = 0.15
	stopMetric   = "kotoki_realtime_bargein_stop_seconds"
)

// stopHistogram returns the count and sum of bargein_stop_seconds in the harness registry.
func stopHistogram(t *testing.T, h *rtharness.H) (count uint64, sum float64) {
	t.Helper()
	mfs, err := h.Reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == stopMetric {
			hist := mf.GetMetric()[0].GetHistogram()
			return hist.GetSampleCount(), hist.GetSampleSum()
		}
	}
	t.Fatalf("metric %s is not registered", stopMetric)
	return 0, 0
}

// TestBargeInP95 interrupts the AI 200 times on a real clock (D-101, spec 7.16 #3): a runtime with
// the scripted providers answers in real time, the test speaks over it once the answer's audio has
// begun, and every barge-in's stop time (confirmation to the last old-generation frame written,
// bargein_stop_seconds) is taken from the runtime's own histogram. It runs in-process because the
// fake STT of the real binary cannot be told to hear words on demand.
func TestBargeInP95(t *testing.T) {
	// Three sentences of 1.5 s: the answer is still being written when the barge-in confirms.
	// The test sends audio faster than real time, so the per-connection upstream rate limit (D-082)
	// is lifted: it guards the socket, not the barge-in path that is measured here.
	lift := func(c *config.RealtimeTuning) { c.WSBinaryRate, c.WSBinaryBurst = 1_000_000, 1_000_000 }
	h := rtharness.New(t, rtharness.WithRealClock(), rtharness.WithTuning(lift), rtharness.WithDuration(3000), rtharness.WithScript(fakes.Script{
		Deltas: []string{"こんにちは。", "今日はいい天気ですね。", "どこへ行きますか。"}, PCMPerSentenceMS: 1500,
	}))
	h.Router.SetSummary("要約")
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	h.Sync()

	// The first answer is to an ordinary question; every later one is to the previous barge-in.
	h.Router.STT().SetFinalizeText("趣味は何ですか。")
	h.Speak("ja_question")
	h.Silence(1200)
	h.Sync()

	stops := make([]float64, 0, bargeAttempts)
	for i := 0; i < bargeAttempts; i++ {
		h.WaitEvents("ai.audio_started", i+1)
		h.Router.STT().SetFinalizeText("趣味は何ですか。")
		before, sumBefore := stopHistogram(t, h)

		// 200 ms of voice make the candidate; the STT hears words since its onset; the rest of the
		// fixture completes the 15 voiced frames of 20 that confirm it.
		h.SpeakSlice("ja_barge_in", 200, 400)
		h.Sync()
		h.Router.STT().EmitPartial("あす")
		h.Sync()
		h.SpeakSlice("ja_barge_in", 400, 800)
		h.WaitEvents("ai.interrupted", i+1)
		// The runtime observes the stop time on its next tick, once the writer's stamp is final.
		h.WaitFor("bargein_stop_seconds observed", func() bool { n, _ := stopHistogram(t, h); return n == before+1 })
		_, sum := stopHistogram(t, h)
		stops = append(stops, sum-sumBefore)

		// The rest of the utterance ends the user's turn, and the AI answers it.
		h.Silence(1200)
		h.Sync()
	}
	h.WaitEvents("ai.audio_started", bargeAttempts+1)
	require.Len(t, stops, bargeAttempts)

	// Not one frame of an old generation after the ai.interrupted that replaced it.
	floor, stale := uint32(0), 0
	for _, w := range h.Conn.Written() {
		switch w.Kind {
		case conversation.MessageText:
			ev, err := protocol.DecodeServerEvent(w.Data)
			require.NoError(t, err)
			if p, ok := ev.Payload.(protocol.AIInterrupted); ok && p.ActiveGeneration > floor {
				floor = p.ActiveGeneration
			}
		case conversation.MessageBinary:
			gen, _, _, err := protocol.DecodeDownstream(w.Data)
			require.NoError(t, err)
			if gen < floor {
				stale++
			}
		}
	}
	assert.Zero(t, stale, "frames of an old generation written after ai.interrupted")

	sorted := append([]float64(nil), stops...)
	sort.Float64s(sorted)
	rank := func(q float64) float64 { return sorted[int(math.Ceil(q*float64(len(sorted))))-1] }
	p50, p95, worst := rank(0.5), rank(0.95), sorted[len(sorted)-1]
	t.Logf("bargein_stop_seconds over %d barge-ins: P50=%.4fs P95=%.4fs max=%.4fs", len(sorted), p50, p95, worst)
	assert.LessOrEqual(t, p95, bargeStopP95, "D-101: P95 bargein_stop_seconds <= 0.15 s")
}
