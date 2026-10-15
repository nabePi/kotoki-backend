package conversation_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// ---- helpers ----

// expiryOf is expires_at of the started session.
func expiryOf(t *testing.T, h *rtharness.H) time.Time {
	t.Helper()
	return first(t, h, "session.started").Payload.(protocol.SessionStarted).ExpiresAt.Time
}

// advanceTo moves the fake clock to at, ticks the runtime and waits for it to settle.
func advanceTo(h *rtharness.H, at time.Time) {
	h.T.Helper()
	h.AdvanceClock(at.Sub(h.Clock.Now()))
	h.Tick()
	h.Sync()
}

// callCount is how many times the session facade was called with method.
func callCount(h *rtharness.H, method string) int {
	n := 0
	for _, c := range h.Calls.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

// sessionEnding returns the payloads of the session.ending events of a stage.
func sessionEnding(h *rtharness.H, stage string) []protocol.SessionEnding {
	var out []protocol.SessionEnding
	for _, ev := range h.Events("session.ending") {
		if p := ev.Payload.(protocol.SessionEnding); p.Stage == stage {
			out = append(out, p)
		}
	}
	return out
}

// gone waits until the runtime has released everything and left the registry.
func gone(h *rtharness.H) {
	h.T.Helper()
	h.WaitFor("runtime gone", func() bool { _, ok := h.Runtime(); return !ok && h.Mgr.Active() == 0 })
}

// gatedLLM is the longScript with its last delta held: the LLM has produced the first sentence (and
// the start of a second) and is still going.
func gatedLLM(gate <-chan struct{}) fakes.Script {
	// The parser cuts a sentence when the next one begins: "今日は" is what makes "こんにちは。" a sentence.
	return fakes.Script{Deltas: []string{"こんにちは。", "今日は", "いい天気ですね。"}, PCMPerSentenceMS: 1000, Gate: gate}
}

// pausedDuringGeneration brings a USER_FIRST session to PROCESSING with a reply whose LLM has produced
// nothing yet (its only delta is held), so the reply has no audio and none is parsed.
func pausedDuringGeneration(t *testing.T, gate <-chan struct{}, opts ...rtharness.Option) *rtharness.H {
	t.Helper()
	opts = append([]rtharness.Option{rtharness.WithScript(fakes.Script{Deltas: []string{"こんにちは。"}, PCMPerSentenceMS: 100, Gate: gate})}, opts...)
	h := rtharness.New(t, opts...)
	started(t, h)
	userSays(h, questionJA)
	h.WaitEvents("ai.generation_started", 1)
	h.Sync()
	require.Equal(t, conversation.StateProcessing, h.View().TurnState)
	return h
}

// ---- warnings and ticks ----

func TestWarnings(t *testing.T) {
	h := rtharness.New(t, rtharness.WithDuration(300))
	started(t, h)
	exp := expiryOf(t, h)
	closing := strings.TrimPrefix(prompt.OutputContract(true), prompt.OutputContract(false))
	require.NotEmpty(t, closing)
	hasClosing := func(gen int) bool {
		for _, m := range h.Router.Generations()[gen].LLMRequests()[0].Messages {
			if strings.Contains(m.Content, closing) {
				return true
			}
		}
		return false
	}

	// A turn well before the warning: no closing instruction.
	userSays(h, questionJA)
	repliedTo(t, h, 1)
	require.False(t, hasClosing(0))

	advanceTo(h, exp.Add(-60*time.Second-time.Millisecond))
	require.Empty(t, sessionEnding(h, "WARNING"), "not before expires_at − 60 s")
	advanceTo(h, exp.Add(-60*time.Second))
	w := sessionEnding(h, "WARNING")
	require.Equal(t, []protocol.SessionEnding{{Stage: "WARNING", Reason: "DURATION_ELAPSED", RemainingSeconds: 60}}, w)
	advanceTo(h, exp.Add(-10*time.Second-time.Millisecond))
	require.Len(t, sessionEnding(h, "WARNING"), 1, "the second warning is at expires_at − 10 s")
	advanceTo(h, exp.Add(-10*time.Second))
	w = sessionEnding(h, "WARNING")
	require.Len(t, w, 2)
	require.Equal(t, protocol.SessionEnding{Stage: "WARNING", Reason: "DURATION_ELAPSED", RemainingSeconds: 10}, w[1])
	require.Equal(t, session.StatusActive, h.View().Status, "a warning is not the end")
	require.Empty(t, sessionEnding(h, "ENDING"))

	// The next generation carries the closing instruction.
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_completed", 2)
	require.True(t, hasClosing(1), "the prompt after the T-60 s warning asks for a natural close")
}

func TestBalanceTick10s(t *testing.T) {
	h := rtharness.New(t, rtharness.WithDuration(300))
	started(t, h)
	start := expiryOf(t, h).Add(-300 * time.Second)
	step := func(at time.Duration) { advanceTo(h, start.Add(at)) }
	ticks := func() []protocol.BalanceTick {
		var out []protocol.BalanceTick
		for _, ev := range h.Events("balance.tick") {
			out = append(out, ev.Payload.(protocol.BalanceTick))
		}
		return out
	}

	step(9900 * time.Millisecond)
	require.Empty(t, ticks())
	step(10 * time.Second)
	require.Len(t, ticks(), 1)
	require.Equal(t, 10, ticks()[0].ElapsedSeconds)
	require.Equal(t, 290, ticks()[0].RemainingSeconds)
	step(20 * time.Second)
	require.Len(t, ticks(), 2)
	require.Equal(t, 20, ticks()[1].ElapsedSeconds)

	// No client, no tick: the schedule goes on, aligned to multiples of 10 s, and the next
	// connection hears the tick of its own time.
	lastSeq := lastSeen(h)
	detach(t, h)
	step(40 * time.Second)
	require.Len(t, ticks(), 2, "nothing is sent to a client that is away")
	h.Reconnect()
	h.Connect(h.ResumeCmd(lastSeq))
	h.WaitEvents("session.snapshot", 1)
	step(50 * time.Second)
	got := ticks()
	require.Len(t, got, 1)
	require.Equal(t, 50, got[0].ElapsedSeconds)
}

// ---- expiry ----

func TestExpiryListening(t *testing.T) {
	h := rtharness.New(t, rtharness.WithDuration(120))
	started(t, h)
	exp := expiryOf(t, h)
	advanceTo(h, exp.Add(-time.Millisecond))
	require.Equal(t, session.StatusActive, h.View().Status)
	advanceTo(h, exp)
	h.WaitEvents("session.completed", 1)
	code, _ := h.WaitClosed()
	require.Equal(t, 1000, code)

	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Complete"}, h.Calls.Calls())
	ending := sessionEnding(h, "ENDING")
	require.Equal(t, []protocol.SessionEnding{{Stage: "ENDING", Reason: "DURATION_ELAPSED", RemainingSeconds: 0, GraceSeconds: 0}}, ending)
	requireOrder(t, h.Types(), "session.started", "session.ending", "session.completed")
	done := first(t, h, "session.completed").Payload.(protocol.SessionCompleted)
	require.Equal(t, "DURATION_ELAPSED", done.EndReason)
	require.LessOrEqual(t, done.ChargedSeconds, 120)
	require.Equal(t, 120, done.ChargedSeconds)

	gone(h)
	_, _, owned := h.Store.Owner(h.SID)
	require.False(t, owned, "the owner key is released")
	rt, ok, _ := h.Store.ReadRT(t.Context(), h.SID)
	require.True(t, ok)
	require.Equal(t, "COMPLETED", rt.Status)
}

func TestExpiryAISpeaking(t *testing.T) {
	t.Run("the LLM is cut and what was parsed is played", func(t *testing.T) {
		gate := make(chan struct{})
		h, gs := speakingSessionHeldAt(t, 10, rtharness.WithDuration(120), rtharness.WithScript(gatedLLM(gate)))
		gen := lastGeneration(t, h)
		exp := expiryOf(t, h)
		require.Len(t, h.Events("turn.started"), 1)

		advanceTo(h, exp)
		require.Equal(t, session.StatusEnding, h.View().Status)
		require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)
		require.Equal(t, []protocol.SessionEnding{{Stage: "ENDING", Reason: "DURATION_ELAPSED", RemainingSeconds: 0, GraceSeconds: 10}},
			sessionEnding(h, "ENDING"), "grace_seconds = ceil(graceDeadline − now)")
		gens := h.Router.Generations()
		h.WaitFor("the LLM to be stopped", gens[len(gens)-1].LLMCancelled)
		require.NotContains(t, h.Calls.Calls(), "Complete", "the sentences that were parsed are still being played")
		require.Empty(t, h.Events("ai.interrupted"))

		gs.limit.Store(1 << 40) // the pacer goes on: the rest of the first sentence is written
		h.WaitEvents("ai.audio_completed", 1)
		ac := first(t, h, "ai.audio_completed").Payload.(protocol.AIAudioCompleted)
		require.False(t, ac.LLMFinished)
		require.Equal(t, 1, ac.SentenceCount, "no sentence beyond the ones already parsed")
		require.Equal(t, conversation.StateAISpeaking, h.View().TurnState, "the turn closes when the playback is over")
		h.Send(h.PlaybackFinishedCmd(gen))
		h.WaitEvents("session.completed", 1)

		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "INTERRUPTED", tc[0].Status, "an LLM that did not finish: INTERRUPTED")
		require.Equal(t, "こんにちは。", *tc[0].AssistantSpokenText)
		require.Equal(t, "こんにちは。今日は", *tc[0].AssistantText, "what the LLM had produced when it was cut")
		row := finalRow(t, h, 1)
		require.Equal(t, session.TurnInterrupted, *row.Status)
		require.Equal(t, "こんにちは。", *row.AssistantSpokenText)
		require.Len(t, h.Events("turn.started"), 1, "no new turn after the expiry")
		require.False(t, h.Clock.Now().After(exp.Add(10*time.Second)), "settled within the grace")
		require.Equal(t, 1, callCount(h, "Complete"))
		require.Equal(t, 1, callCount(h, "BeginEnding"))
		gone(h)
	})

	t.Run("an LLM that finished normally: COMPLETED", func(t *testing.T) {
		h, gs := speakingSession(t, rtharness.WithDuration(120))
		gen := lastGeneration(t, h)
		advanceTo(h, expiryOf(t, h))
		require.Equal(t, session.StatusEnding, h.View().Status)
		gs.limit.Store(1 << 40)
		h.WaitEvents("ai.audio_completed", 1)
		require.True(t, first(t, h, "ai.audio_completed").Payload.(protocol.AIAudioCompleted).LLMFinished)
		h.Send(h.PlaybackFinishedCmd(gen))
		h.WaitEvents("session.completed", 1)
		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "COMPLETED", tc[0].Status)
		require.Equal(t, scriptText, *tc[0].AssistantSpokenText)
		require.Equal(t, session.TurnCompleted, *finalRow(t, h, 1).Status)
	})

	t.Run("the estimated end of the playback closes the turn without client.playback_finished", func(t *testing.T) {
		h, gs := speakingSession(t, rtharness.WithDuration(120))
		exp := expiryOf(t, h)
		advanceTo(h, exp)
		gs.limit.Store(1 << 40)
		h.WaitEvents("ai.audio_completed", 1)
		require.Equal(t, session.StatusEnding, h.View().Status)
		advanceTo(h, exp.Add(9*time.Second)) // the estimated end of the playback + 1.5 s is before the grace
		h.WaitEvents("session.completed", 1)
		require.Equal(t, 1.0, h.Metrics.PlaybackFinishedMissingTotal())
		require.Equal(t, "COMPLETED", completed(h)[0].Status)
	})
}

func TestExpiryProcessing(t *testing.T) {
	// The reply has not been heard: it is cancelled (ai.interrupted{SESSION_ENDING}) and the turn
	// is the user's words alone (§7.11.2 PROCESSING).
	gate := make(chan struct{})
	h := pausedDuringGeneration(t, gate, rtharness.WithDuration(120))
	exp := expiryOf(t, h)
	advanceTo(h, exp)
	h.WaitEvents("session.completed", 1)

	it := first(t, h, "ai.interrupted").Payload.(protocol.AIInterrupted)
	require.Equal(t, "SESSION_ENDING", it.Reason)
	require.Equal(t, 0, it.SpokenSentenceCount)
	requireOrder(t, h.Types(), "ai.generation_started", "ai.interrupted", "turn.completed", "session.completed")
	tc := completed(h)
	require.Len(t, tc, 1)
	require.Equal(t, "COMPLETED", tc[0].Status)
	require.Equal(t, questionJA, *tc[0].UserText)
	require.Nil(t, tc[0].AssistantText)
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnCompleted, *row.Status)
	require.Nil(t, row.AssistantText, "no assistant side")
	require.Equal(t, questionJA, *row.UserText)
	require.Equal(t, 1, callCount(h, "Complete"))
}

func TestExpiryWaitingForFinal(t *testing.T) {
	// PROCESSING, still waiting for the FINAL of the turn end: it is waited for within the grace, and
	// the turn is the user's words alone.
	h := rtharness.New(t, rtharness.WithDuration(120), rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	exp := expiryOf(t, h)
	h.Router.STT().HoldFinalize(true)
	h.Speak("ja_question")
	h.Silence(1200)
	h.Sync()
	require.Equal(t, conversation.StateProcessing, h.View().TurnState)
	advanceTo(h, exp)
	require.Equal(t, session.StatusEnding, h.View().Status)
	require.Empty(t, h.Events("session.completed"), "the final is still waited for")

	h.Router.STT().HoldFinalize(false) // the FINAL arrives
	h.WaitEvents("session.completed", 1)
	tc := completed(h)
	require.Len(t, tc, 1)
	require.Equal(t, "COMPLETED", tc[0].Status)
	require.Equal(t, questionJA, *tc[0].UserText)
	require.Empty(t, h.Events("ai.generation_started"), "nobody answers after the expiry")
	require.Equal(t, 0, h.Router.STT().FinalTimeoutReports())
}

func TestExpiryUserSpeaking(t *testing.T) {
	// The user is speaking when expires_at passes: the detector goes on, the turn ends as it would,
	// and it is saved with the user's words and no answer.
	speakAcrossExpiry := func(t *testing.T) (*rtharness.H, time.Time) {
		h := rtharness.New(t, rtharness.WithDuration(120), rtharness.WithTimeline(timelineOf(t, "ja_long_utterance")))
		started(t, h)
		exp := expiryOf(t, h)
		advanceTo(h, exp.Add(-5*time.Second))
		h.SpeakSlice("ja_long_utterance", 0, 5000)
		h.Sync()
		require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
		h.SpeakSlice("ja_long_utterance", 5000, 6000) // the clock passes expires_at while the user talks
		h.Tick()
		h.Sync()
		v := h.View()
		require.Equal(t, session.StatusEnding, v.Status)
		require.Equal(t, conversation.StateUserSpeaking, v.TurnState, "the turn is not cut at the expiry")
		require.Equal(t, []protocol.SessionEnding{{Stage: "ENDING", Reason: "DURATION_ELAPSED", RemainingSeconds: 0, GraceSeconds: 9}},
			sessionEnding(h, "ENDING"))
		return h, exp
	}

	t.Run("the detector ends the turn in the grace", func(t *testing.T) {
		h, _ := speakAcrossExpiry(t)
		h.Router.STT().SetFinalizeText("毎朝六時に起きます。")
		h.Silence(2200) // the words so far end in a filler suffix: the silence that ends the turn is 1400 ms
		h.WaitEvents("session.completed", 1)
		sf := first(t, h, "stt.final").Payload.(protocol.STTFinal)
		require.Equal(t, "SILENCE", sf.EndReason)
		require.Equal(t, "毎朝六時に起きます。", sf.Text)
		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "COMPLETED", tc[0].Status)
		require.Nil(t, tc[0].AssistantText)
		require.Empty(t, h.Events("ai.generation_started"))
		row := finalRow(t, h, 1)
		require.Equal(t, session.TurnCompleted, *row.Status)
		require.Equal(t, "毎朝六時に起きます。", *row.UserText)
		require.Nil(t, row.AssistantText)
	})

	t.Run("a muted client does not hold the grace", func(t *testing.T) {
		h, _ := speakAcrossExpiry(t)
		h.AdvanceClock(3 * time.Second) // no frames: the starvation watchdog supplies the silence
		h.Tick()
		h.WaitEvents("session.completed", 1)
		require.Equal(t, "SILENCE", first(t, h, "stt.final").Payload.(protocol.STTFinal).EndReason)
		require.Equal(t, "COMPLETED", completed(h)[0].Status)
	})

	t.Run("the grace cuts the wait for the final", func(t *testing.T) {
		h, exp := speakAcrossExpiry(t)
		h.SpeakSlice("ja_long_utterance", 6000, 11500) // voiced until exp+7.5 s
		h.Sync()
		h.Router.STT().HoldFinalize(true)
		h.Silence(1500) // the turn ends at about exp+8.9 s: STT_FINAL_TIMEOUT alone would wait to exp+10.4 s
		h.Sync()
		require.Equal(t, conversation.StateProcessing, h.View().TurnState)
		require.Less(t, h.Clock.Now().Sub(exp), 9500*time.Millisecond)
		require.Empty(t, h.Events("session.completed"))
		advanceTo(h, exp.Add(10*time.Second))
		h.WaitEvents("session.completed", 1)
		require.Equal(t, "COMPLETED", completed(h)[0].Status)
	})

	t.Run("a turn that never ends is cut at the deadline", func(t *testing.T) {
		h, exp := speakAcrossExpiry(t)
		h.SpeakSlice("ja_long_utterance", 6000, 12000)
		h.Sync()
		require.Equal(t, session.StatusEnding, h.View().Status, "still speaking, still in the grace")
		require.Less(t, h.Clock.Now().Sub(exp), 10*time.Second)
		advanceTo(h, exp.Add(10*time.Second))
		h.WaitEvents("session.completed", 1)
		sf := first(t, h, "stt.final").Payload.(protocol.STTFinal)
		require.Equal(t, "SESSION_ENDING", sf.EndReason)
		require.NotEmpty(t, sf.Text, "the words heard so far")
		require.Equal(t, -1, sf.STTFinalMS, "no final was waited for")
		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "COMPLETED", tc[0].Status)
		require.Equal(t, 0, h.Router.STT().FinalTimeoutReports(), "a final the end cut short is not counted against the vendor")
		require.Equal(t, session.TurnCompleted, *finalRow(t, h, 1).Status)
	})
}

func TestGraceDeadlineHard(t *testing.T) {
	// The pacer never gets going again: at expires_at + 10 s whatever is left is cut, and not a
	// moment before.
	h, _ := speakingSession(t, rtharness.WithDuration(120))
	exp := expiryOf(t, h)
	advanceTo(h, exp)
	require.Equal(t, session.StatusEnding, h.View().Status)
	advanceTo(h, exp.Add(10*time.Second-time.Millisecond))
	require.Equal(t, session.StatusEnding, h.View().Status, "the grace is 10 s")
	require.Empty(t, h.Events("ai.interrupted"))

	advanceTo(h, exp.Add(10*time.Second))
	h.WaitEvents("session.completed", 1)
	it := first(t, h, "ai.interrupted").Payload.(protocol.AIInterrupted)
	require.Equal(t, "SESSION_ENDING", it.Reason)
	require.Equal(t, 1, it.SpokenSentenceCount)
	tc := completed(h)
	require.Len(t, tc, 1)
	require.Equal(t, "INTERRUPTED", tc[0].Status)
	require.Equal(t, "こんにちは。", *tc[0].AssistantSpokenText)
	requireOrder(t, h.Types(), "session.ending", "ai.interrupted", "turn.completed", "session.completed")
	require.Equal(t, 1, callCount(h, "Complete"))
}

// ---- user end ----

func TestUserEnd(t *testing.T) {
	t.Run("the AI is cut", func(t *testing.T) {
		h, _ := speakingSession(t, rtharness.WithDuration(120))
		gen := lastGeneration(t, h)
		h.Send(h.EndCmd())
		h.Send(h.EndCmd()) // a second end changes nothing
		h.WaitEvents("session.completed", 1)
		code, _ := h.WaitClosed()
		require.Equal(t, 1000, code)

		ending := sessionEnding(h, "ENDING")
		require.Len(t, ending, 1)
		require.Equal(t, "USER_ENDED", ending[0].Reason)
		require.Equal(t, 2, ending[0].GraceSeconds)
		require.Equal(t, 120-int(h.Clock.Now().Sub(rtharness.T0)/time.Second), ending[0].RemainingSeconds)
		it := first(t, h, "ai.interrupted").Payload.(protocol.AIInterrupted)
		require.Equal(t, "SESSION_ENDING", it.Reason)
		require.Equal(t, gen+1, it.ActiveGeneration)
		require.Equal(t, 1, it.SpokenSentenceCount)
		requireOrder(t, h.Types(), "session.ending", "ai.interrupted", "turn.completed", "session.completed")
		require.Equal(t, "INTERRUPTED", completed(h)[0].Status)
		require.Equal(t, session.TurnInterrupted, *finalRow(t, h, 1).Status)
		require.Equal(t, 1, callCount(h, "BeginEnding"))
		require.Equal(t, 1, callCount(h, "Complete"), "Complete exactly once")
		gone(h)
	})

	t.Run("a second end during the grace is ignored", func(t *testing.T) {
		h, gs := speakingSession(t, rtharness.WithDuration(120))
		gen := lastGeneration(t, h)
		advanceTo(h, expiryOf(t, h))
		require.Equal(t, session.StatusEnding, h.View().Status)
		h.Send(h.EndCmd())
		h.Sync()
		require.Equal(t, session.StatusEnding, h.View().Status)
		require.Len(t, sessionEnding(h, "ENDING"), 1, "no second session.ending")
		require.Empty(t, h.Events("ai.interrupted"), "the user end does not cut what the expiry lets play")
		gs.limit.Store(1 << 40)
		h.WaitEvents("ai.audio_completed", 1)
		h.Send(h.PlaybackFinishedCmd(gen))
		h.WaitEvents("session.completed", 1)
		require.Equal(t, 1, callCount(h, "BeginEnding"))
		require.Equal(t, 1, callCount(h, "Complete"))
		require.Equal(t, "DURATION_ELAPSED", first(t, h, "session.completed").Payload.(protocol.SessionCompleted).EndReason)
	})

	t.Run("while the reply has not been heard", func(t *testing.T) {
		gate := make(chan struct{})
		h := pausedDuringGeneration(t, gate)
		h.Send(h.EndCmd())
		h.WaitEvents("session.completed", 1)
		require.Equal(t, "SESSION_ENDING", first(t, h, "ai.interrupted").Payload.(protocol.AIInterrupted).Reason)
		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "COMPLETED", tc[0].Status, "the user's words alone")
		require.Equal(t, questionJA, *tc[0].UserText)
		row := finalRow(t, h, 1)
		require.Equal(t, session.TurnCompleted, *row.Status)
		require.Nil(t, row.AssistantText)
	})

	t.Run("while the user is speaking the final is waited for", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_long_utterance")))
		started(t, h)
		h.SpeakSlice("ja_long_utterance", 0, 5000)
		h.Sync()
		require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
		h.Router.STT().SetFinalizeText("毎朝六時に起きます。")
		h.Send(h.EndCmd())
		h.WaitEvents("session.completed", 1)
		sf := first(t, h, "stt.final").Payload.(protocol.STTFinal)
		require.Equal(t, "SESSION_ENDING", sf.EndReason)
		require.Equal(t, "毎朝六時に起きます。", sf.Text)
		require.GreaterOrEqual(t, sf.STTFinalMS, 0, "the final arrived")
		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "COMPLETED", tc[0].Status)
		require.Nil(t, tc[0].AssistantText)
		require.Equal(t, "毎朝六時に起きます。", *finalRow(t, h, 1).UserText)
		require.Empty(t, h.Events("ai.generation_started"))
	})

	t.Run("a final that does not come is waited for 1.5 s", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_long_utterance")))
		started(t, h)
		h.SpeakSlice("ja_long_utterance", 0, 5000)
		h.Sync()
		h.Router.STT().HoldFinalize(true)
		h.Send(h.EndCmd())
		h.Sync()
		require.Equal(t, session.StatusEnding, h.View().Status)
		advanceTo(h, h.Clock.Now().Add(1400*time.Millisecond))
		require.Empty(t, h.Events("session.completed"), "STT_FINAL_TIMEOUT is 1.5 s")
		advanceTo(h, h.Clock.Now().Add(100*time.Millisecond))
		h.WaitEvents("session.completed", 1)
		sf := first(t, h, "stt.final").Payload.(protocol.STTFinal)
		require.Equal(t, "SESSION_ENDING", sf.EndReason)
		require.NotEmpty(t, sf.Text, "the running text")
		require.Equal(t, "COMPLETED", completed(h)[0].Status)
		require.Equal(t, 0, h.Router.STT().FinalTimeoutReports())
	})

	t.Run("noise is DISCARDED, not saved", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_greeting_short")))
		started(t, h)
		h.SpeakSlice("ja_greeting_short", 250, 450) // 200 ms of sound the STT does not turn into words
		h.Sync()
		require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
		h.Router.STT().SetFinalizeText("")
		h.Send(h.EndCmd())
		h.WaitEvents("session.completed", 1)
		tc := completed(h)
		require.Len(t, tc, 1)
		require.Equal(t, "DISCARDED", tc[0].Status)
		require.Equal(t, 0, first(t, h, "session.completed").Payload.(protocol.SessionCompleted).TurnCount, "DISCARDED turns are not counted")
	})
}

// ---- billing ----

func TestBillingStopsAtEnding(t *testing.T) {
	// The seconds after BeginEnding are not charged, however long the persister and the grace take
	// (Review Focus 3): the session ends at elapsed 30 s and is settled 9 s later.
	h, _ := speakingSession(t, rtharness.WithDuration(120))
	start := expiryOf(t, h).Add(-120 * time.Second)
	advanceTo(h, start.Add(30*time.Second))
	release := h.Turns.Hold("Finalize") // the turn of the AI cannot be written yet
	t.Cleanup(release)
	ticksBefore := len(h.Events("balance.tick"))

	h.Send(h.EndCmd())
	h.WaitFor("ENDING", func() bool { return h.View().Status == session.StatusEnding })
	require.Equal(t, "ENDING", string(h.Session.Status(h.SID)))
	h.Sync()
	require.Empty(t, h.Events("session.completed"), "the persister is flushed first")
	require.NotContains(t, h.Calls.Calls(), "Complete")

	advanceTo(h, start.Add(34*time.Second))
	require.Empty(t, h.Events("session.completed"), "the flush is waited for 5 s")
	h.AdvanceClock(start.Add(45 * time.Second).Sub(h.Clock.Now())) // the flush deadline has passed, and the tick of 40 s is due
	h.Tick()
	h.WaitEvents("session.completed", 1) // (no Sync: the runtime cannot finish exiting while the write is held)
	done := first(t, h, "session.completed").Payload.(protocol.SessionCompleted)
	require.Equal(t, 30, done.ChargedSeconds, "billing stopped at ENDING")
	require.Equal(t, ticksBefore, len(h.Events("balance.tick")), "no balance.tick after ENDING")
	release()
	gone(h)
}

// ---- fatal failures ----

func TestFatalSTTFailure(t *testing.T) {
	h, _ := speakingSession(t, rtharness.WithDuration(120))
	start := expiryOf(t, h).Add(-120 * time.Second)
	advanceTo(h, start.Add(20*time.Second))
	release := h.Turns.Hold("Finalize") // the failed turn is not on disk yet
	t.Cleanup(release)

	h.Router.STT().Fail(provider.NewError(provider.FakePrimary, provider.CapSTT, provider.KindUnavailable, "the vendor stream is gone"))
	h.WaitFor("BeginEnding", func() bool { return slices.Contains(h.Calls.Calls(), "BeginEnding") })
	h.Sync()
	// accepting off, everything cancelled, the turn failed, ENDING: and the persister is waited for.
	v := h.View()
	require.False(t, v.Accepting)
	require.Equal(t, session.StatusEnding, v.Status)
	require.Equal(t, uuid.Nil, v.CurrentTurn, "the turn in progress is over")
	require.NotContains(t, h.Calls.Calls(), "Fail", "Fail comes after the persister is flushed")
	require.Empty(t, h.Events("session.failed"))
	require.Equal(t, []protocol.SessionEnding{{Stage: "ENDING", Reason: "PROVIDER_FAILURE", RemainingSeconds: 100}}, sessionEnding(h, "ENDING"))

	release()
	h.WaitEvents("session.failed", 1)
	code, reason := h.WaitClosed()
	require.Equal(t, 4500, code)
	require.Equal(t, "PROVIDER_FAILURE", reason)
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "PROVIDER_FAILURE", failed.EndReason)
	require.Equal(t, "PROVIDER_FAILURE", failed.ErrorCode)
	require.Equal(t, 20, failed.ChargedSeconds, "charged up to BeginEnding")
	requireOrder(t, h.Types(), "session.ending", "session.failed", "error")
	require.Equal(t, "PROVIDER_FAILURE", errorOf(t, h).Error.Code)
	require.True(t, errorOf(t, h).Fatal)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Fail"}, h.Calls.Calls())
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status, "the turn that was running is FAILED")
	require.Empty(t, h.Events("ai.interrupted"), "nothing is told to a client that is being closed")
	gone(h)
}

func TestPanicInGoroutine(t *testing.T) {
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst),
		rtharness.WithGenerationWrapper(func(g provider.Generation) provider.Generation { return panicGen{g} }))
	h.Connect(h.StartCmd())
	code, reason := h.WaitClosed()
	require.Equal(t, 1011, code)
	require.Equal(t, "INTERNAL_ERROR", reason)
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "SERVER_ERROR", failed.EndReason)
	require.Equal(t, "INTERNAL_ERROR", failed.ErrorCode)
	require.Equal(t, []protocol.SessionEnding{{Stage: "ENDING", Reason: "SERVER_ERROR", RemainingSeconds: 600}}, sessionEnding(h, "ENDING"))
	var fatal []protocol.ErrorPayload
	for _, ev := range h.Events("error") {
		if p := ev.Payload.(protocol.ErrorPayload); p.Fatal {
			fatal = append(fatal, p)
		}
	}
	require.Len(t, fatal, 1)
	require.Equal(t, "INTERNAL_ERROR", fatal[0].Error.Code)
	requireOrder(t, h.Types(), "session.ending", "session.failed", "error")
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Fail"}, h.Calls.Calls())
	require.Equal(t, session.TurnFailed, *finalRow(t, h, 0).Status)
	gone(h)
}

func TestPanicInTheLoop(t *testing.T) {
	// A panic of the loop itself: the end is still attempted, on the dying goroutine, and the
	// session is settled.
	h := rtharness.New(t)
	started(t, h)
	r, _ := h.Runtime()
	require.Error(t, r.PanicOnLoop(t.Context()))
	code, reason := h.WaitClosed()
	require.Equal(t, 1011, code)
	require.Equal(t, "INTERNAL_ERROR", reason)
	require.Equal(t, "SERVER_ERROR", first(t, h, "session.failed").Payload.(protocol.SessionFailed).EndReason)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Fail"}, h.Calls.Calls())
	gone(h)
}

func TestPersisterStuckFatal(t *testing.T) {
	// A database that does not answer for more than 30 s ends the session SERVER_ERROR (§7.11.4);
	// the end itself waits for the persister no longer than 5 s.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	h.Turns.FailAlways(errors.New("database unreachable"))
	t.Cleanup(func() { h.Turns.FailAlways(nil) })
	h.Speak("ja_question") // the turn row cannot be written
	h.WaitFor("the first failed write", func() bool { return len(h.Turns.Attempts()) >= 1 })

	h.AdvanceClock(31 * time.Second)
	h.WaitFor("the end of the session", func() bool {
		h.Tick()
		return h.View().Status == session.StatusEnding
	})
	require.Contains(t, h.Calls.Calls(), "BeginEnding")
	require.NotContains(t, h.Calls.Calls(), "Fail", "the persister is waited for first")
	h.AdvanceClock(4900 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.NotContains(t, h.Calls.Calls(), "Fail", "the wait is 5 s")
	h.AdvanceClock(200 * time.Millisecond)
	h.Tick()
	h.WaitEvents("session.failed", 1)
	code, reason := h.WaitClosed()
	require.Equal(t, 1011, code)
	require.Equal(t, "INTERNAL_ERROR", reason)
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "SERVER_ERROR", failed.EndReason)
	require.Equal(t, "Fail", h.Calls.Calls()[len(h.Calls.Calls())-1])
	h.Turns.FailAlways(nil) // the database is back: the persister can drain
	gone(h)
}

// ---- no leftovers ----

func TestNoGoroutineLeakAfterSession(t *testing.T) {
	// §7.16 #10: when the session is over every goroutine of the runtime is gone, with no Close of the
	// Manager to help (the leak check runs when the test ends, before the harness is closed).
	h := rtharness.New(t, rtharness.WithDuration(120), rtharness.WithTimeline(timelineOf(t, "ja_question")))
	testutil.NoGoroutineLeak(t, "internal/conversation")
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", 1)
	userSays(h, questionJA) // a turn in progress when the session ends
	h.WaitEvents("ai.audio_started", 2)
	h.Send(h.EndCmd())
	h.WaitEvents("session.completed", 1)
	_, _ = h.WaitClosed()
	gone(h)
	require.NoError(t, h.AttachResult())
}

// ---- the open items of the earlier tasks ----

func TestBargeInRowSurvivesTheEnd(t *testing.T) {
	// The BARGE_IN row and bargein_stop_seconds of a barge-in are written on the tick after it; a
	// session that ends before that tick must still write them.
	gs := &gatedSleep{}
	h := rtharness.New(t, rtharness.WithScript(longScript), rtharness.WithTimeline(timelineOf(t, "ja_question")), rtharness.WithSleep(gs.sleep))
	gs.clk = h.Clock
	gs.limit.Store(40)
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_started", 1)
	h.WaitFor("the pacer is held", gs.held.Load)
	h.Sync()
	bargeIn(h, "あす")
	h.WaitEvents("ai.interrupted", 1)
	h.Send(h.EndCmd()) // no tick between the barge-in and the end
	h.WaitEvents("session.completed", 1)

	h.WaitFor("BARGE_IN row", func() bool { return len(eventPayloads(h, "BARGE_IN")) == 1 })
	require.Equal(t, uint64(1), h.Metrics.BargeInStopSamples())
	require.Equal(t, 1.0, h.Metrics.BargeInTotal("server"))
}

func TestFenceLostTellsTheTerminalStatus(t *testing.T) {
	// §7.9.3: the client of a runtime that lost its fence hears 4011, or 4010 when the session is
	// already in a terminal status (the sweeper settled it).
	for _, tc := range []struct {
		name   string
		status session.Status
		code   int
		err    errs.Code
	}{
		{"another owner", session.StatusActive, 4011, errs.CodeSessionTakenOver},
		{"settled by the sweeper", session.StatusCompleted, 4010, errs.CodeSessionNotResumable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
			started(t, h)
			rec, err := h.Session.GetForAttach(t.Context(), h.SID)
			require.NoError(t, err)
			rec.RuntimeFence++ // somebody else holds the fence now
			rec.Status = tc.status
			h.Session.Seed(rec)

			h.Speak("ja_question") // the next write of the runtime is refused
			code, reason := h.WaitClosed()
			require.Equal(t, tc.code, code)
			require.Equal(t, string(tc.err), reason)
			require.Equal(t, string(tc.err), errorOf(t, h).Error.Code)
			require.Equal(t, 1.0, h.Metrics.FenceLostTotal())
			gone(h)
		})
	}
}

func TestDetachedInTheGraceSettlesAtTheDeadline(t *testing.T) {
	// A client that left during the grace is not waited for, and not given up on early either:
	// the AI turn is cut at the deadline, and session.completed goes to the replay stream only.
	h, _ := speakingSession(t, rtharness.WithDuration(120))
	exp := expiryOf(t, h)
	advanceTo(h, exp)
	detach(t, h)
	require.Equal(t, session.StatusEnding, h.View().Status)
	advanceTo(h, exp.Add(10*time.Second))
	gone(h)
	require.Equal(t, 1, callCount(h, "Complete"))
	require.Equal(t, session.StatusCompleted, h.Session.Status(h.SID))
	var types []string
	entries, _ := h.Store.ReadEvents(t.Context(), h.SID)
	for _, e := range entries {
		ev, err := protocol.DecodeServerEvent(e.Env)
		require.NoError(t, err)
		types = append(types, ev.Type)
	}
	require.Contains(t, types, "session.completed")
	require.Equal(t, session.TurnInterrupted, *finalRow(t, h, 1).Status)
}

func TestNoBalanceTickInTheGrace(t *testing.T) {
	// 125 s: the tick of 130 s falls inside the grace, where nothing is billed any more.
	h, gs := speakingSession(t, rtharness.WithDuration(125))
	exp := expiryOf(t, h)
	advanceTo(h, exp)
	ticks := len(h.Events("balance.tick"))
	advanceTo(h, exp.Add(6*time.Second))
	require.Equal(t, session.StatusEnding, h.View().Status)
	require.Equal(t, ticks, len(h.Events("balance.tick")), "no balance.tick in ENDING")
	gs.limit.Store(1 << 40)
}
