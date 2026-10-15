package conversation_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// Two sentences of one second each: 50 frames per sentence, so a pacer that stops after a few
// waits leaves the first sentence written, the second one parsed and partly queued.
var longScript = fakes.Script{Deltas: []string{"こんにちは。", "今日はいい天気ですね。"}, PCMPerSentenceMS: 1000}

// gatedSleep is the pacer's wait with a budget: it moves the fake clock like the harness does until
// the budget is used, then holds the pacer (held) until the budget is raised or its generation is
// cancelled (a barge-in).
type gatedSleep struct {
	clk   *clock.Fake
	limit atomic.Int64
	used  atomic.Int64
	held  atomic.Bool
}

func (g *gatedSleep) sleep(ctx context.Context, d time.Duration) error {
	for g.used.Load() >= g.limit.Load() {
		g.held.Store(true)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	g.held.Store(false)
	g.used.Add(1)
	g.clk.Advance(d)
	return ctx.Err()
}

// writtenOf is how many frames of generation gen were written to the socket.
func writtenOf(h *rtharness.H, gen uint32) int {
	n := 0
	for _, f := range h.Frames() {
		if f.Gen == gen {
			n++
		}
	}
	return n
}

// bargeIn speaks over the AI: 200 ms of ja_barge_in make the candidate, the STT hears words, and the
// rest of the 15 voiced frames of 20 confirm it. The words come as a partial the test chooses the
// moment of, so they are unmistakably "since the onset".
func bargeIn(h *rtharness.H, words string) {
	h.T.Helper()
	h.SpeakSlice("ja_barge_in", 200, 400)
	h.Sync()
	h.Router.STT().EmitPartial(words)
	h.Sync()
	h.SpeakSlice("ja_barge_in", 400, 800)
}

func TestBargeInConfirmed(t *testing.T) {
	gs := &gatedSleep{}
	h := rtharness.New(t, rtharness.WithScript(longScript), rtharness.WithTimeline(timelineOf(t, "ja_question")),
		rtharness.WithSleep(gs.sleep))
	gs.clk = h.Clock
	gs.limit.Store(40) // the first sentence (50 frames) is written, the second is parsed and half queued
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_started", 1)
	old := lastGeneration(t, h)
	h.WaitFor("the pacer is held", gs.held.Load)
	h.Sync()
	require.Greater(t, writtenOf(h, old), 50, "sentence 0 (50 frames) is written")
	require.Less(t, writtenOf(h, old), 100, "sentence 1 is not")

	// A hint of the client, then the server's own confirmation within a second.
	h.Send(h.Cmd("client.barge_in", protocol.ClientBargeIn{Generation: old}))
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState, "a hint changes no state")
	bargeIn(h, "あす")
	it := h.WaitEvents("ai.interrupted", 1)[0]
	gs.limit.Store(1 << 40) // the reply to the barge-in plays freely
	h.Sync()

	p := it.Payload.(protocol.AIInterrupted)
	require.Equal(t, "BARGE_IN", p.Reason)
	require.Equal(t, old+1, p.ActiveGeneration)
	require.Equal(t, 1, p.SpokenSentenceCount, "the first sentence was written, the second was not")
	require.Equal(t, old, *it.Generation)
	require.Equal(t, old+1, h.View().Generation)

	types := h.Types()
	requireOrder(t, types, "ai.interrupted", "turn.completed", "speech.started", "turn.started")
	ss := h.Events("speech.started")
	require.True(t, ss[len(ss)-1].Payload.(protocol.SpeechStarted).DuringAISpeech)
	ts := h.Events("turn.started")
	require.Equal(t, "USER", ts[len(ts)-1].Payload.(protocol.TurnStarted).Initiator)
	require.Equal(t, 2, *ts[len(ts)-1].TurnSeq, "turn 1 was the question; the barge-in begins turn 2")
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)

	tc := completed(h)[0]
	require.Equal(t, "INTERRUPTED", tc.Status)
	require.Equal(t, questionJA, *tc.UserText)
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnInterrupted, *row.Status)
	require.Equal(t, "こんにちは。", *row.AssistantSpokenText, "exactly the sentences whose last frame was written")
	require.Equal(t, scriptText, *row.AssistantText, "the whole text the LLM produced")
	newRow := turnRow(t, h, 2)
	require.Nil(t, newRow.Status, "the turn of the barge-in is running")

	require.Equal(t, 1.0, h.Metrics.BargeInTotal("server"))
	require.Equal(t, 1.0, h.Metrics.BargeInTotal("hint_confirmed"))
	require.Equal(t, 0.0, h.Metrics.BargeInTotal("hint_unconfirmed"))
	require.Equal(t, 1.0, h.Metrics.TurnsTotal("INTERRUPTED"))
	h.Tick()
	h.Sync()
	require.Equal(t, uint64(1), h.Metrics.BargeInStopSamples(), "bargein_stop_seconds is observed once per barge-in")
	h.WaitFor("BARGE_IN row", func() bool { return len(eventPayloads(h, "BARGE_IN")) == 1 })
	bi := eventPayloads(h, "BARGE_IN")[0]
	require.EqualValues(t, old, bi["generation"])
	require.EqualValues(t, 1, bi["spoken_sentence_count"])
	require.GreaterOrEqual(t, bi["stop_ms"], float64(0))
	for _, ev := range h.Written.Events() {
		if ev.Type == "BARGE_IN" {
			require.Equal(t, row.ID, *ev.TurnID, "the event belongs to the turn that was cut")
		}
	}

	// The words since the onset are the start of the new turn; the old turn's text is not.
	h.Router.STT().SetFinalizeText("ちょっと待ってください。")
	h.Silence(1200)
	h.Sync()
	final := h.WaitEvents("stt.final", 2)[1]
	require.Equal(t, "ちょっと待ってください。", final.Payload.(protocol.STTFinal).Text)
	require.Equal(t, 2, *final.TurnSeq)
	h.WaitEvents("ai.generation_started", 2)
	require.Equal(t, old+2, lastGeneration(t, h), "the cancelled generation's number is never reused")
}

// speaking brings a USER_FIRST session to AI_SPEAKING: the question is answered and the first audio
// is out (the turn stays open until playback_finished).
func speaking(t *testing.T, h *rtharness.H) {
	t.Helper()
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)
}

func TestNoStaleFramesAfterInterrupt(t *testing.T) {
	// The client reads slowly: the writer holds one frame and 25 more wait in the queue. After the
	// barge-in no frame of the old generation may be written; the one in flight was written before
	// ai.interrupted could be (Review Focus 2, D-101).
	gs := &gatedSleep{}
	h := rtharness.New(t, rtharness.WithScript(longScript), rtharness.WithTimeline(timelineOf(t, "ja_question")),
		rtharness.WithSleep(gs.sleep))
	gs.clk = h.Clock
	gs.limit.Store(5)
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_started", 1)
	old := lastGeneration(t, h)
	h.WaitFor("the pacer is held", gs.held.Load)
	h.Sync()
	before := writtenOf(h, old)
	h.Conn.StallWrites()
	target := gs.used.Load() + 30
	gs.limit.Store(target) // the pacer goes on for 30 waits and queues behind the stalled writer
	h.WaitFor("the pacer used its waits", func() bool { return gs.used.Load() >= target })
	require.False(t, h.View().OutboundDrained, "frames of the old generation wait in the queue")

	h.SpeakSlice("ja_barge_in", 200, 400)
	barrier(h)
	h.Router.STT().EmitPartial("あす")
	barrier(h)
	h.SpeakSlice("ja_barge_in", 400, 800)
	h.WaitFor("the barge-in", func() bool { return h.View().Generation == old+1 })
	h.Conn.ReleaseWrites()
	h.WaitEvents("ai.interrupted", 1)
	h.Sync()

	var seenInterrupt bool
	late := 0
	for _, w := range h.Conn.Written() {
		if w.Kind == conversation.MessageText {
			if ev, err := protocol.DecodeServerEvent(w.Data); err == nil && ev.Type == "ai.interrupted" {
				seenInterrupt = true
			}
			continue
		}
		gen, _, _, err := protocol.DecodeDownstream(w.Data)
		require.NoError(t, err)
		if seenInterrupt && gen <= old {
			late++
		}
	}
	require.True(t, seenInterrupt)
	require.Zero(t, late, "no frame of the old generation after ai.interrupted")
	require.LessOrEqual(t, writtenOf(h, old), before+1, "only the frame that was already in flight is written after the stall began")
	require.Zero(t, writtenOf(h, old+1), "the interrupted number carries no audio")

	// The goroutines of the old generation left (the harness checks the leak at the end) and the
	// loop saw their exit time.
	h.WaitFor("the exit of the old generation was observed", func() bool { return h.Metrics.GenerationExitSamples() >= 1 })
}

// barrier waits for the loop to handle what was sent, like Sync but without needing the outbound
// queue to be empty (a stalled writer keeps it full).
func barrier(h *rtharness.H) {
	h.T.Helper()
	r, ok := h.Runtime()
	require.True(h.T, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.WaitFor("pipe idle", h.Conn.Idle)
	require.NoError(h.T, r.Barrier(ctx))
}

func TestEchoDoesNotBargeIn(t *testing.T) {
	t.Run("no words since the onset", func(t *testing.T) {
		// Energy while the AI speaks, but the STT hears nothing new: a speaker echo the client's
		// AEC let through must not interrupt the AI.
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
		speaking(t, h)
		h.SpeakSlice("ja_barge_in", 200, 1400) // 60 voiced frames
		h.Sync()
		require.Empty(t, h.Events("ai.interrupted"))
		require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)
		require.Zero(t, h.Metrics.BargeInTotal("server"))

		// Control: the same sound with words since its onset does interrupt.
		h.Silence(400)
		bargeIn(h, "あす")
		h.WaitEvents("ai.interrupted", 1)
	})

	t.Run("a partial from before the commit", func(t *testing.T) {
		// The STT hears something just before the AI's first audio. The commit entering AI_SPEAKING
		// (S-8 (b)) closes it: those words are from before, whatever the sound that follows.
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
		h.Router.SetOnMarkAudioSent(func() { h.Router.STT().EmitPartial("あー") })
		speaking(t, h)
		h.Sync()
		require.Equal(t, 1.0, h.Metrics.STTDiscardedTotal("ai_speaking"), "the commit's FINAL was answered")
		h.SpeakSlice("ja_barge_in", 200, 1400)
		h.Sync()
		require.Empty(t, h.Events("ai.interrupted"))
		require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)

		h.Silence(400)
		bargeIn(h, "あす")
		h.WaitEvents("ai.interrupted", 1)
	})

	t.Run("a pending commit hides the old words", func(t *testing.T) {
		// While the FINAL of the commit is still on its way, the running partial is audio from
		// before it: nothing counts as a word since the onset.
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
		speaking(t, h)
		h.Sync()
		stt := h.Router.STT()
		stt.HoldFinalize(true)
		h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h))) // the turn ends and commit (c) is held
		h.WaitEvents("turn.completed", 1)
		h.Sync()
		h.SpeakSlice("ja_barge_in", 200, 400)
		h.Sync()
		stt.EmitPartial("あす")
		h.Sync()
		h.SpeakSlice("ja_barge_in", 400, 1400) // user speech opens the next turn, not a barge-in
		h.Sync()
		require.Empty(t, h.Events("ai.interrupted"))
	})
}

func TestClientBargeInHintOnly(t *testing.T) {
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")),
		rtharness.WithTuning(func(c *config.RealtimeTuning) { c.PlaybackFinishGraceMS = 60000 })) // the turn must not close on its deadline
	speaking(t, h)
	gen := lastGeneration(t, h)
	h.Send(h.Cmd("client.barge_in", protocol.ClientBargeIn{Generation: gen}))
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState, "a hint changes no state")
	require.Empty(t, h.Events("ai.interrupted"))
	require.Zero(t, h.Metrics.BargeInTotal("hint_unconfirmed"), "still within the second the server may confirm it")

	h.AdvanceClock(1100 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.Equal(t, 1.0, h.Metrics.BargeInTotal("hint_unconfirmed"))
	require.Zero(t, h.Metrics.BargeInTotal("hint_confirmed"))
	require.Zero(t, h.Metrics.BargeInTotal("server"))
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)
}

func TestBargeInDisabledAfterExpiry(t *testing.T) {
	// Row 13 guard: with acceptingTurns false (the grace after expiry) nothing interrupts the AI.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	speaking(t, h)
	r, _ := h.Runtime()
	require.NoError(t, r.SetAccepting(context.Background(), false))
	bargeIn(h, "あす")
	h.Sync()
	require.Empty(t, h.Events("ai.interrupted"))
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)

	require.NoError(t, r.SetAccepting(context.Background(), true)) // control: the same sound interrupts
	h.Silence(400)
	bargeIn(h, "あすす")
	h.WaitEvents("ai.interrupted", 1)
}

func TestBargeInDuringApology(t *testing.T) {
	// §7.6.5: when the generation that is cut is the APOLOGY, the turn is already FAILED and stays
	// FAILED (not INTERRUPTED), and it still counts in the streak.
	h := rtharness.New(t, rtharness.WithScript(llmFails))
	started(t, h)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_started", 1) // the apology
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)
	apology := *generationsOf(h, "APOLOGY")[0].Generation

	bargeIn(h, "あす")
	it := h.WaitEvents("ai.interrupted", 1)[0]
	h.Sync()
	require.Equal(t, "BARGE_IN", it.Payload.(protocol.AIInterrupted).Reason)
	require.Equal(t, apology, *it.Generation)

	require.Equal(t, "FAILED", completed(h)[0].Status)
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "AI_GENERATION_FAILED", *row.ErrorCode)
	require.Equal(t, 1, h.View().FailedTurns, "an interrupted apology is still a failed turn")
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
	require.Equal(t, 0.0, h.Metrics.TurnsTotal("INTERRUPTED"))
	ts := h.Events("turn.started")
	require.Equal(t, 2, *ts[len(ts)-1].TurnSeq)
}

func TestBargeInKeepsTheFinalWordsSinceTheOnset(t *testing.T) {
	// The STT closed a segment between the onset and the confirmation: those words open the user's
	// turn, whole (the turn text starts at the onset, §7.6.5, not at the running partial).
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	speaking(t, h)
	h.SpeakSlice("ja_barge_in", 200, 400)
	h.Sync()
	h.Router.STT().EmitFinal("あ、すみません、")
	h.Sync()
	h.SpeakSlice("ja_barge_in", 400, 800)
	h.WaitEvents("ai.interrupted", 1)
	h.Sync()
	h.Router.STT().EmitPartial("ちょっと")
	h.SpeakSlice("ja_barge_in", 800, 1200)
	h.Silence(1200)
	h.Sync()
	final := h.WaitEvents("stt.final", 2)[1]
	require.Equal(t, "あ、すみません、ちょっと", final.Payload.(protocol.STTFinal).Text)
	require.Equal(t, 2, *final.TurnSeq)
}

func TestSpokenTextDoesNotWaitForTheLoop(t *testing.T) {
	// The writer has written the sentences while the loop was busy and had read none of the events
	// that carry them (here it sits in the barge-in itself): what the user heard and what the LLM said
	// still come out whole (D-078), because both are read from the Generation, not from the loop's
	// copy of the events.
	gate := make(chan struct{})
	const spokenAll = scriptText + "また会いましょう。"
	script := fakes.Script{Deltas: []string{"こんにちは。", "今日はいい天気ですね。", "また会いましょう。"}, PCMPerSentenceMS: 100, Gate: gate} // the last delta waits for the gate
	h := rtharness.New(t, rtharness.WithScript(script), rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_started", 1)
	gen := lastGeneration(t, h)
	h.WaitFor("sentence 0 written", func() bool { return writtenOf(h, gen) == 5 })
	r, _ := h.Runtime()

	entered, proceed := make(chan struct{}), make(chan struct{})
	type result struct {
		unread int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		n, err := r.BargeInBehindTheLoop(context.Background(), entered, proceed)
		done <- result{n, err}
	}()
	<-entered
	close(gate) // the rest is produced, spoken and written; the loop reads nothing of it
	h.WaitFor("every sentence written", func() bool { return writtenOf(h, gen) == 15 })
	close(proceed)
	res := <-done
	require.NoError(t, res.err)
	require.Positive(t, res.unread, "the loop was behind the generation")

	h.WaitEvents("turn.completed", 1)
	require.Equal(t, 3, h.Events("ai.interrupted")[0].Payload.(protocol.AIInterrupted).SpokenSentenceCount)
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnInterrupted, *row.Status)
	require.Equal(t, spokenAll, *row.AssistantSpokenText)
	require.Equal(t, spokenAll, *row.AssistantText)
}
