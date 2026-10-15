package conversation_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/audiofx"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// The default script of the harness: two sentences of 100 ms.
const (
	scriptText = "こんにちは。今日はいい天気ですね。"
	questionJA = "趣味は何ですか？"
)

// timelineOf loads the STT timeline of a fixture (<name>.partials.json).
func timelineOf(t *testing.T, fixture string) replay.Timeline {
	t.Helper()
	tl, err := replay.LoadTimeline(audiofx.PartialsPath(t, fixture))
	require.NoError(t, err)
	return tl
}

// started starts the session and waits until it is ACTIVE.
func started(t *testing.T, h *rtharness.H) {
	t.Helper()
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	h.Sync()
}

// utter plays a fixture and enough silence for the detector to end the turn.
func utter(h *rtharness.H, fixture string) {
	h.T.Helper()
	h.Speak(fixture)
	h.Silence(1200)
	h.Sync()
}

// lastGeneration is the generation of the latest ai.generation_started.
func lastGeneration(t *testing.T, h *rtharness.H) uint32 {
	t.Helper()
	evs := h.Events("ai.generation_started")
	require.NotEmpty(t, evs)
	require.NotNil(t, evs[len(evs)-1].Generation)
	return *evs[len(evs)-1].Generation
}

// answerTurn plays one complete turn whose user text is text (the FINAL of the TURN_END commit
// carries it), lets the AI answer and reports the end of playback. n is how many turns completed
// before it.
func answerTurn(t *testing.T, h *rtharness.H, text string, n int) {
	t.Helper()
	h.Router.STT().SetFinalizeText(text)
	h.Speak("ja_greeting_short")
	h.Silence(1200)
	h.WaitEvents("ai.audio_completed", n+1)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", n+1)
	h.Sync()
}

func requireOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	at := -1
	for _, w := range want {
		found := -1
		for i := at + 1; i < len(got); i++ {
			if got[i] == w {
				found = i
				break
			}
		}
		require.Greater(t, found, at, "%s must come after the previous one in %v", w, got)
		at = found
	}
}

// ofTurn returns the types of the events that belong to turn seq.
func ofTurn(h *rtharness.H, seq int) []string {
	var out []string
	for _, ev := range h.Events() {
		if ev.TurnSeq != nil && *ev.TurnSeq == seq {
			out = append(out, ev.Type)
		}
	}
	return out
}

// turnRow waits for the row of turn seq and returns it.
func turnRow(t *testing.T, h *rtharness.H, seq int) session.Turn {
	t.Helper()
	var row session.Turn
	h.WaitFor("turn row", func() bool {
		for _, r := range h.Turns.Turns(h.SID) {
			if r.Sequence == seq {
				row = r
				return true
			}
		}
		return false
	})
	return row
}

// finalRow waits until turn seq has a status.
func finalRow(t *testing.T, h *rtharness.H, seq int) session.Turn {
	t.Helper()
	var row session.Turn
	h.WaitFor("final turn row", func() bool {
		row = turnRow(t, h, seq)
		return row.Status != nil
	})
	return row
}

func completed(h *rtharness.H) []protocol.TurnCompleted {
	var out []protocol.TurnCompleted
	for _, ev := range h.Events("turn.completed") {
		out = append(out, ev.Payload.(protocol.TurnCompleted))
	}
	return out
}

func TestUserTurnHappyPath(t *testing.T) {
	// AI_FIRST, so the reply is generation 2 (the opening is generation 1).
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst), rtharness.WithTimeline(timelineOf(t, "ja_question")))
	h.Connect(h.StartCmd())
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	h.Sync()

	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 2)
	h.Sync()

	// Rows 1, 3, 7: the events of the user turn (turn_seq 1) in the order of the spec.
	types := ofTurn(h, 1)
	requireOrder(t, types, "speech.started", "turn.started", "stt.partial", "speech.ended", "stt.final",
		"ai.generation_started", "ai.text_delta", "ai.audio_started", "ai.sentence", "ai.audio_completed")
	require.NotContains(t, types, "turn.completed", "the turn closes on client.playback_finished")

	ts := h.Events("turn.started")[1]
	require.Equal(t, "USER", ts.Payload.(protocol.TurnStarted).Initiator)
	ss := h.Events("speech.started")
	require.Len(t, ss, 1)
	require.False(t, ss[0].Payload.(protocol.SpeechStarted).DuringAISpeech)
	require.Greater(t, ss[0].Payload.(protocol.SpeechStarted).AudioOffsetMS, int64(0))
	for _, ev := range h.Events("stt.partial") {
		require.True(t, strings.HasPrefix(questionJA, ev.Payload.(protocol.STTPartial).Text), "a partial is the cumulative text of the turn")
	}
	se := h.Events("speech.ended")
	require.Len(t, se, 1)
	require.Greater(t, se[0].Payload.(protocol.SpeechEnded).SpeechMS, 0)

	sf := h.Events("stt.final")
	require.Len(t, sf, 1)
	final := sf[0].Payload.(protocol.STTFinal)
	require.Equal(t, questionJA, final.Text)
	require.Equal(t, "SILENCE", final.EndReason)
	require.Equal(t, "FAKE_PRIMARY", final.Provider)
	require.GreaterOrEqual(t, final.STTFinalMS, 0)
	require.False(t, final.UsedFallback)
	require.Equal(t, 1.0, h.Metrics.TurnEndReasonTotal("silence_500"), "a question ending in ？ with its FINAL in: 500 ms")

	gs := h.Events("ai.generation_started")
	require.Len(t, gs, 2)
	require.Equal(t, "REPLY", gs[1].Payload.(protocol.AIGenerationStarted).Kind)
	require.Equal(t, uint32(2), *gs[1].Generation)
	require.Equal(t, *ts.TurnID, *gs[1].TurnID)
	done := h.Events("ai.audio_completed")[1].Payload.(protocol.AIAudioCompleted)
	var frames []rtharness.Frame
	for _, f := range h.Frames() {
		if f.Gen == 2 {
			frames = append(frames, f)
		}
	}
	require.Len(t, frames, done.FrameCount)
	for i, f := range frames {
		require.Equal(t, uint32(i), f.Seq, "downstream seq restarts at 0 per generation")
	}
	require.Len(t, h.Events("ai.sentence"), 4, "2 sentences of the opening and 2 of the reply")

	// The row is INSERTed without a status and carries the user side once the STT has final text.
	h.WaitFor("user side of the row", func() bool { r := turnRow(t, h, 1); return r.UserText != nil })
	row := turnRow(t, h, 1)
	require.Nil(t, row.Status, "the turn is running: status NULL")
	require.Equal(t, questionJA, *row.UserText)
	require.Equal(t, "FAKE_PRIMARY", *row.STTProvider)
	require.NotNil(t, row.STTFinalMS)
	require.GreaterOrEqual(t, *row.STTFinalMS, 0)
	require.NotNil(t, row.STTAudioMS)
	require.NotNil(t, row.UserStartedAt)
	require.NotNil(t, row.UserEndedAt)
	require.True(t, row.UserStartedAt.Before(*row.UserEndedAt))

	h.Send(h.PlaybackFinishedCmd(2))
	h.WaitEvents("turn.completed", 2)
	h.Sync()
	tc := completed(h)[1]
	require.Equal(t, "COMPLETED", tc.Status)
	require.Equal(t, questionJA, *tc.UserText)
	require.Equal(t, scriptText, *tc.AssistantText)
	require.Equal(t, scriptText, *tc.AssistantSpokenText)
	require.NotNil(t, tc.Latency)
	require.Equal(t, conversation.StateListening, h.View().TurnState)

	row = finalRow(t, h, 1)
	require.Equal(t, session.TurnCompleted, *row.Status)
	require.Equal(t, scriptText, *row.AssistantText)
	require.Equal(t, scriptText, *row.AssistantSpokenText)
	require.NotNil(t, row.LLMProvider)
	require.Equal(t, rtharness.PrimaryVoice.VoiceID, *row.TTSVoiceID, "the voice that was actually used (D-015)")
	require.NotNil(t, row.TurnEndToFirstAudioMS)
	require.GreaterOrEqual(t, *row.TurnEndToFirstAudioMS, 0)
	require.Equal(t, 2.0, h.Metrics.TurnsTotal("COMPLETED"), "the opening and this turn completed")
}

func TestFinalizeCountIsThree(t *testing.T) {
	// S-8: a normal REPLY turn calls stt.Finalize three times: TURN_END, the commit entering
	// AI_SPEAKING and the commit back to LISTENING.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	require.Equal(t, 3, h.Router.STT().FinalizeCalls())
	h.WaitFor("both DISCARD finals dropped", func() bool {
		h.Sync()
		return h.Metrics.STTDiscardedTotal("ai_speaking") == 1 && h.Metrics.STTDiscardedTotal("listening") == 1
	})
}

func TestDiscardEchoCommit(t *testing.T) {
	// The FINAL of a DISCARD commit (here an echo of the AI, "gema") arrives after the user has
	// already started the next turn: it must not become part of that turn (S-8).
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	stt := h.Router.STT()
	stt.SetFinalizeText("gema")
	stt.HoldFinalize(true) // the commit back to LISTENING stays pending
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	require.Equal(t, 3, stt.FinalizeCalls())

	h.Speak("ja_greeting_short") // the next turn starts while the DISCARD commit is pending
	h.Sync()
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
	stt.HoldFinalize(false) // "gema" arrives now
	h.Sync()
	require.Equal(t, 1.0, h.Metrics.STTDiscardedTotal("listening"))

	stt.SetFinalizeText("次の発話です。")
	h.Silence(1200)
	h.Sync()
	finals := h.WaitEvents("stt.final", 2)
	text := finals[1].Payload.(protocol.STTFinal).Text
	require.Equal(t, "次の発話です。", text)
	require.NotContains(t, text, "gema")
	row := turnRow(t, h, 2)
	h.WaitFor("user_text of turn 2", func() bool { row = turnRow(t, h, 2); return row.UserText != nil })
	require.Equal(t, "次の発話です。", *row.UserText)
}

func TestCommitFinalsAreFIFO(t *testing.T) {
	// Two commits are pending at once: the DISCARD of the boundary (an echo, "gema") and the
	// TURN_END of the next turn. Their FINALs answer them in the order they were made: the first
	// is the echo and is dropped, the second is the text of the turn.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	stt := h.Router.STT()
	stt.SetFinalizeText("gema")
	stt.HoldFinalize(true)
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	stt.SetFinalizeText("次の発話です。")
	h.Speak("ja_greeting_short")
	waitTurnEnd(t, h)
	require.Equal(t, 4, stt.FinalizeCalls(), "TURN_END of the second turn is the fourth commit")
	require.Empty(t, h.Events("stt.final")[1:])

	stt.HoldFinalize(false) // gema, then the text of the turn
	finals := h.WaitEvents("stt.final", 2)
	require.Equal(t, "次の発話です。", finals[1].Payload.(protocol.STTFinal).Text)
	h.Sync()
	require.Equal(t, 1.0, h.Metrics.STTDiscardedTotal("listening"))
}

func TestNoiseOnlyDiscarded(t *testing.T) {
	// Steady noise never makes a turn (the detector); speech-like sound whose STT text is empty
	// makes one that is DISCARDED (row 8): no generation, no APOLOGY, no failure counted.
	h := rtharness.New(t, rtharness.WithTimeline(replay.Timeline{}))
	started(t, h)
	h.Speak("noise_only")
	h.Sync()
	require.Equal(t, []string{"session.ready", "session.started"}, h.Types(), "no turn from noise alone")
	require.Equal(t, conversation.StateListening, h.View().TurnState)

	for i := 1; i <= 3; i++ { // three in a row: a streak of failures would end the session
		utter(h, "ja_greeting_short")
		h.WaitEvents("turn.completed", i)
		h.Sync()
		tc := completed(h)[i-1]
		require.Equal(t, "DISCARDED", tc.Status)
		require.Nil(t, tc.UserText)
		require.Nil(t, tc.AssistantText)
		require.Equal(t, conversation.StateListening, h.View().TurnState)
	}
	require.Empty(t, h.Events("ai.generation_started"), "a discarded turn has no generation")
	require.Empty(t, h.Events("error"), "and no APOLOGY or error (§7.6.2 beats the word FAILED of the master plan)")
	require.Empty(t, h.Events("stt.final"))
	require.Empty(t, h.Router.Generations())
	require.Equal(t, 3.0, h.Metrics.TurnsTotal("DISCARDED"))
	require.Equal(t, 6, h.Router.STT().FinalizeCalls(), "per discarded turn: TURN_END and the commit back to LISTENING (S-8 (c))")
	require.Equal(t, session.StatusActive, h.View().Status)
	for seq := 1; seq <= 3; seq++ {
		row := finalRow(t, h, seq)
		require.Equal(t, session.TurnDiscarded, *row.Status)
		require.Nil(t, row.UserText, "the noise is not stored")
		require.Nil(t, row.AssistantText)
	}

	// turn_count of session.completed excludes DISCARDED turns
	h.Send(h.EndCmd())
	sc := h.WaitEvents("session.completed", 1)[0].Payload.(protocol.SessionCompleted)
	require.Equal(t, 0, sc.TurnCount)
}

func TestShortNoiseDiscarded(t *testing.T) {
	// "ん" from a short burst of voice is noise; the same text from a long one is an answer (row 8).
	// (The VAD holds voiced for ~120 ms after speech, so 200 ms of speech is ~320 ms voiced.)
	short := replay.Timeline{{AtMs: 200, Text: "ん"}}
	h := rtharness.New(t, rtharness.WithTimeline(short))
	started(t, h)
	h.SpeakSlice("ja_greeting_short", 250, 450) // 200 ms of speech
	h.Silence(1500)
	h.Sync()
	h.WaitEvents("turn.completed", 1)
	require.Equal(t, "DISCARDED", completed(h)[0].Status)
	require.Empty(t, h.Events("ai.generation_started"))

	h = rtharness.New(t, rtharness.WithTimeline(short))
	started(t, h)
	h.SpeakSlice("ja_greeting_short", 250, 850) // 600 ms of speech: over 400 ms voiced
	h.Silence(1500)
	h.Sync()
	f := h.WaitEvents("stt.final", 1)[0].Payload.(protocol.STTFinal)
	require.Equal(t, "ん", f.Text, "one rune of a long utterance is kept")
	h.WaitEvents("ai.generation_started", 1)
}

func TestUserResumesInProcessing(t *testing.T) {
	// Row 9: the user goes on speaking before the AI has produced a sound. The generation is
	// cancelled, the same turn continues and no turn row is added.
	gate := make(chan struct{})
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")),
		rtharness.WithScript(fakes.Script{Deltas: []string{"はい。"}, Gate: gate}))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.generation_started", 1)
	h.Sync()
	require.Equal(t, conversation.StateProcessing, h.View().TurnState)
	require.Equal(t, uint32(1), h.View().Generation)
	turnID := h.View().CurrentTurn

	h.Speak("ja_greeting_short")
	h.Sync()
	ai := h.WaitEvents("ai.interrupted", 1)[0]
	pl := ai.Payload.(protocol.AIInterrupted)
	require.Equal(t, "USER_RESUMED", pl.Reason)
	require.Equal(t, uint32(2), pl.ActiveGeneration, "the cancelled generation consumes its number")
	require.Equal(t, 0, pl.SpokenSentenceCount)
	require.Equal(t, uint32(1), *ai.Generation)
	v := h.View()
	require.Equal(t, conversation.StateUserSpeaking, v.TurnState)
	require.Equal(t, turnID, v.CurrentTurn, "the same turn")
	require.Equal(t, uint32(2), v.Generation)
	require.Len(t, h.Events("speech.started"), 2)
	require.Len(t, h.Events("turn.started"), 1)
	require.Equal(t, 1.0, h.Metrics.TurnResumedTotal())
	require.Len(t, h.Turns.Turns(h.SID), 1, "no INSERT for a resumed turn")

	h.Silence(1200)
	h.Sync()
	finals := h.WaitEvents("stt.final", 2)
	require.Equal(t, questionJA, finals[1].Payload.(protocol.STTFinal).Text, "the earlier text of the turn is kept")
	gs := h.WaitEvents("ai.generation_started", 2)
	require.Equal(t, uint32(3), *gs[1].Generation)
	require.Len(t, h.Turns.Turns(h.SID), 1)
	require.Len(t, h.Events("turn.started"), 1)
}

func TestResumeAfterFinalTimeoutKeepsText(t *testing.T) {
	// Row 9 after a STT_FINAL_TIMEOUT: the turn continues, and the FINAL that comes late for the
	// first commit does not take the earlier text of the turn with it (it is superseded, not late).
	gate := make(chan struct{})
	h := rtharness.New(t, rtharness.WithTimeline(replay.Timeline{{AtMs: 600, Text: "こんにちは"}}),
		rtharness.WithScript(fakes.Script{Deltas: []string{"はい。"}, Gate: gate}))
	started(t, h)
	stt := h.Router.STT()
	stt.HoldFinalize(true)
	h.Speak("ja_greeting_short")
	waitTurnEnd(t, h)
	h.AdvanceClock(1500 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.Equal(t, "こんにちは", h.WaitEvents("stt.final", 1)[0].Payload.(protocol.STTFinal).Text)
	h.WaitEvents("ai.generation_started", 1)

	h.Speak("ja_greeting_short") // the user goes on: row 9
	h.Sync()
	h.WaitEvents("ai.interrupted", 1)
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
	stt.HoldFinalize(false) // the late FINAL of the first commit
	h.Sync()
	stt.SetFinalizeText("です。")
	h.Silence(1200)
	h.Sync()
	finals := h.WaitEvents("stt.final", 2)
	require.Equal(t, "こんにちはです。", finals[1].Payload.(protocol.STTFinal).Text, "the first part of the turn survived")
	require.Len(t, h.Events("turn.started"), 1)
}

func TestNoMergeAfterMaxUtterance(t *testing.T) {
	// Row 4/9: a turn cut at 45 s is not merged with what the user says next, even while the AI
	// has not answered yet.
	gate := make(chan struct{})
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_max_utterance")),
		rtharness.WithScript(fakes.Script{Deltas: []string{"はい。"}, Gate: gate}))
	started(t, h)
	h.Speak("ja_max_utterance") // 50 s: the turn ends at 45 s and the user keeps talking
	h.Sync()
	final := h.WaitEvents("stt.final", 1)[0].Payload.(protocol.STTFinal)
	require.Equal(t, "MAX_UTTERANCE", final.EndReason)
	require.Equal(t, 1.0, h.Metrics.TurnEndReasonTotal("max_utterance"))
	h.WaitEvents("ai.generation_started", 1)
	v := h.View()
	require.Equal(t, conversation.StateProcessing, v.TurnState, "the speech after the cut did not resume the turn")
	require.Equal(t, uint32(1), v.Generation, "the generation was not cancelled")
	require.Empty(t, h.Events("ai.interrupted"))
	require.Equal(t, 0.0, h.Metrics.TurnResumedTotal())

	close(gate)
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	h.Sync()

	h.Router.STT().SetFinalizeText("新しい発話です。")
	utter(h, "ja_greeting_short")
	ts := h.WaitEvents("turn.started", 2)
	require.Equal(t, 2, *ts[1].TurnSeq, "a new turn, not a continuation")
	finals := h.WaitEvents("stt.final", 2)
	require.Equal(t, "新しい発話です。", finals[1].Payload.(protocol.STTFinal).Text)
	require.NotEqual(t, *ts[0].TurnID, *ts[1].TurnID)
}

// waitTurnEnd sends silence one frame at a time until the detector has ended the user turn, so the
// fake clock stands (within one frame) where the turn end was detected.
func waitTurnEnd(t *testing.T, h *rtharness.H) {
	t.Helper()
	for range 200 {
		if h.View().TurnState != conversation.StateUserSpeaking {
			return
		}
		h.Silence(20)
		h.Sync()
	}
	t.Fatal("the turn did not end")
}

func TestSTTFinalTimeout(t *testing.T) {
	// No FINAL within 1.5 s of the commit: the turn goes on with the text STT has (row 7).
	h := rtharness.New(t, rtharness.WithTimeline(replay.Timeline{{AtMs: 600, Text: "こんにちは"}}))
	started(t, h)
	stt := h.Router.STT()
	stt.HoldFinalize(true)
	h.Speak("ja_greeting_short")
	waitTurnEnd(t, h)
	require.Equal(t, 1, stt.FinalizeCalls())

	h.AdvanceClock(1400 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.Empty(t, h.Events("stt.final"), "STT_FINAL_TIMEOUT is 1.5 s")
	require.Equal(t, 0, stt.FinalTimeoutReports())
	h.AdvanceClock(100 * time.Millisecond)
	h.Tick()
	h.Sync()
	f := h.WaitEvents("stt.final", 1)[0].Payload.(protocol.STTFinal)
	require.Equal(t, "こんにちは", f.Text, "TurnText() as it was: the final segments and the running partial")
	require.Equal(t, -1, f.STTFinalMS)
	require.Equal(t, 1, stt.FinalTimeoutReports(), "ReportFinalTimeout")
	h.WaitEvents("ai.generation_started", 1)
	row := turnRow(t, h, 1)
	h.WaitFor("user side", func() bool { row = turnRow(t, h, 1); return row.UserText != nil })
	require.Equal(t, "こんにちは", *row.UserText)
	require.Nil(t, row.STTFinalMS, "NULL: the column has CHECK ≥ 0")

	// The late FINAL is dropped: it neither changes the turn nor counts as a DISCARD.
	stt.HoldFinalize(false)
	h.Sync()
	require.Len(t, h.Events("stt.final"), 1)
	require.Len(t, h.Events("ai.generation_started"), 1)
}

func TestPlaybackDeadline(t *testing.T) {
	// Row 15 by timeout: no client.playback_finished → the turn closes at the estimated end of
	// playback + PLAYBACK_FINISH_GRACE_MS.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	h.Speak("ja_question")
	waitTurnEnd(t, h) // the clock stands where the reply starts: the pacer's estimate is relative to it
	h.WaitEvents("ai.audio_completed", 1)
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)

	h.AdvanceClock(200 * time.Millisecond) // the audio lasts 200 ms (two sentences of 100 ms)
	h.Tick()
	h.AdvanceClock(1400 * time.Millisecond)
	h.Tick()
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState, "1.6 s: the grace period has not passed")
	require.Empty(t, h.Events("turn.completed"))
	h.AdvanceClock(200 * time.Millisecond)
	h.Tick()
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	require.Equal(t, 1.0, h.Metrics.PlaybackFinishedMissingTotal())
	tc := completed(h)[0]
	require.Equal(t, "COMPLETED", tc.Status)
	require.Equal(t, conversation.StateListening, h.View().TurnState)
	require.Equal(t, session.TurnCompleted, *finalRow(t, h, 1).Status)
}

func TestPlaybackFinishedOutsideAISpeaking(t *testing.T) {
	// Row 19: client.playback_finished while the AI is not speaking, or for another generation,
	// changes nothing.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	h.Send(h.PlaybackFinishedCmd(1))
	h.Sync()
	require.Equal(t, []string{"session.ready", "session.started"}, h.Types())

	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	h.Send(h.PlaybackFinishedCmd(7)) // not the active generation
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)
	require.Empty(t, h.Events("turn.completed"))
}

func TestOpeningUserResumes9b(t *testing.T) {
	// Row 9b: AI_FIRST and the user speaks before the opening is heard: turn 0 is INTERRUPTED with
	// nothing spoken and the user's turn begins.
	gate := make(chan struct{})
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst), rtharness.WithTimeline(replay.Timeline{}),
		rtharness.WithScript(fakes.Script{Deltas: []string{"いらっしゃいませ。"}, Gate: gate}))
	h.Connect(h.StartCmd())
	h.WaitEvents("ai.generation_started", 1)
	h.Sync()
	require.Equal(t, conversation.StateProcessing, h.View().TurnState)

	h.Speak("ja_greeting_short")
	h.Sync()
	h.WaitEvents("turn.started", 2)
	types := h.Types()
	requireOrder(t, types, "ai.generation_started", "ai.interrupted", "turn.completed", "speech.started", "turn.started")
	ai := h.Events("ai.interrupted")[0]
	require.Equal(t, "USER_RESUMED", ai.Payload.(protocol.AIInterrupted).Reason)
	require.Equal(t, uint32(2), ai.Payload.(protocol.AIInterrupted).ActiveGeneration)
	tc := completed(h)[0]
	require.Equal(t, "INTERRUPTED", tc.Status)
	require.Equal(t, "", *tc.AssistantSpokenText)
	require.Equal(t, 0, *h.Events("turn.completed")[0].TurnSeq)
	ts := h.Events("turn.started")[1]
	require.Equal(t, "USER", ts.Payload.(protocol.TurnStarted).Initiator)
	require.Equal(t, 1, *ts.TurnSeq)
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)

	row := finalRow(t, h, 0)
	require.Equal(t, session.TurnInterrupted, *row.Status)
	require.NotNil(t, row.AssistantSpokenText)
	require.Equal(t, "", *row.AssistantSpokenText)
	require.Nil(t, row.TTSVoiceID, "no audio was synthesised")
	require.Equal(t, 1.0, h.Metrics.TurnsTotal("INTERRUPTED"))
	close(gate)
}

func TestPostTurnTasks(t *testing.T) {
	h := rtharness.New(t)
	h.Memory.Episodic = []memory.ContextItem{{Category: memory.CategoryEpisodic, Content: "先月は京都に行きました", Tokens: 12}}
	h.Memory.CoreChanged = true
	h.Memory.NewCore = []memory.ContextItem{{Category: memory.CategoryProfileFact, Content: "名前はタロウです", Tokens: 8}}
	started(t, h)

	// Turn 1: RefreshEpisodic gets the turn, ReloadCoreIfChanged runs; the results are used by the
	// next generation (not by the one that is running).
	answerTurn(t, h, "最近の旅行について話します", 0)
	h.WaitFor("refresh", func() bool { return h.Memory.Refreshes() == 1 && h.Memory.Reloads() == 1 })
	require.Equal(t, []memory.TurnText{{UserText: "最近の旅行について話します", AssistantSpokenText: scriptText}}, h.Memory.Refreshed()[0])
	first := h.Router.Generations()[0].LLMRequests()[0].Messages[0].Content
	require.NotContains(t, first, "先月は京都に行きました")
	h.Sync()

	answerTurn(t, h, "二番目の発話です", 1)
	second := h.Router.Generations()[1].LLMRequests()[0].Messages[0].Content
	require.Contains(t, second, "先月は京都に行きました", "RefreshEpisodic result, from the next generation on")
	require.Contains(t, second, "名前はタロウです", "ReloadCoreIfChanged result")
	answerTurn(t, h, "三番目の発話です", 2)
	answerTurn(t, h, "四番目の発話です", 3)
	h.WaitFor("4 refreshes", func() bool { return h.Memory.Refreshes() == 4 })
	last := h.Memory.Refreshed()[3]
	require.Len(t, last, 3, "at most the 3 latest turns")
	require.Equal(t, "二番目の発話です", last[0].UserText)
	require.Equal(t, "四番目の発話です", last[2].UserText)

	// A user_text of less than 6 runes skips the refresh (the core reload still runs).
	reloads := h.Memory.Reloads()
	answerTurn(t, h, "はい", 4)
	h.WaitFor("core reload", func() bool { return h.Memory.Reloads() == reloads+1 })
	require.Equal(t, 4, h.Memory.Refreshes())

	// At most one job in flight: while one waits, the next turn does not start another.
	h.Gates.Refresh = make(chan struct{})
	answerTurn(t, h, "長い発話その一です", 5)
	h.WaitFor("job blocked", func() bool { return h.Blocked() == 1 })
	reloads = h.Memory.Reloads()
	answerTurn(t, h, "長い発話その二です", 6)
	h.Sync()
	require.Equal(t, 1, h.Blocked(), "the second job was not started")
	require.Equal(t, reloads, h.Memory.Reloads())
	close(h.Gates.Refresh)
	h.WaitFor("the job ran", func() bool { return h.Memory.Refreshes() == 5 && h.Memory.Reloads() == reloads+1 })
	h.Sync()
	answerTurn(t, h, "長い発話その三です", 7)
	h.WaitFor("a new job after the first finished", func() bool { return h.Memory.Refreshes() == 6 })
}

func TestRollingSummaryAfterTurns(t *testing.T) {
	// §7.8.5: once 4 turns have left the window of 8 and no summary covers them, a summary job runs;
	// its text is used from the next generation on and kept in Redis.
	h := rtharness.New(t)
	h.Router.SetSummary("京都旅行の話をした。")
	started(t, h)
	for i := range 11 {
		answerTurn(t, h, "発話その"+string(rune('一'+i))+"です", i)
	}
	require.Empty(t, h.Router.TextRequests(), "11 turns: only 3 fell out of the window")
	answerTurn(t, h, "発話その十二です", 11)
	h.WaitFor("summary request", func() bool { return len(h.Router.TextRequests()) == 1 })
	req := h.Router.TextRequests()[0]
	require.Equal(t, "ROLLING_SUMMARY", req.Task)
	require.Contains(t, req.Messages[len(req.Messages)-1].Content, "発話その一です")
	require.NotContains(t, req.Messages[len(req.Messages)-1].Content, "発話その十二です", "only the turns outside the window")
	h.Sync()
	h.AdvanceClock(200 * time.Millisecond)
	h.Tick()
	h.Sync()
	h.WaitFor("summary in redis", func() bool {
		sum, ok, err := h.Store.ReadSummary(t.Context(), h.SID)
		return err == nil && ok && sum.Text == "京都旅行の話をした。" && sum.CoveredUntilSeq == 4
	})

	answerTurn(t, h, "発話その十三です", 12)
	gens := h.Router.Generations()
	var all strings.Builder
	for _, m := range gens[12].LLMRequests()[0].Messages {
		all.WriteString(m.Content)
	}
	require.Contains(t, all.String(), "京都旅行の話をした。")
	require.Equal(t, 1, len(h.Router.TextRequests()), "the 4 covered turns do not trigger another job")
}

func TestAdaptiveAdjustsAfterClarifications(t *testing.T) {
	// §7.7.5 step 1b / §7.8.6: after 4 COMPLETED turns with 2 requests for clarification the
	// difficulty drops, from the next generation on.
	h := rtharness.New(t)
	started(t, h)
	for i, text := range []string{"もう一度お願いします。", "ゆっくり話してください。", "普通の文章です。", "もう少し聞かせてください。"} {
		answerTurn(t, h, text, i)
	}
	h.WaitFor("adjustment", func() bool { return h.Metrics.DifficultyAdjustmentsTotal() == 1 })
	h.WaitFor("DIFFICULTY_ADJUSTED row", func() bool { return len(sessionEvents(h, "DIFFICULTY_ADJUSTED")) == 1 })
	require.JSONEq(t, `{"turn_seq":4,"in_session_adjustments":1,"speech_speed":2,"question_complexity":2,"scaffolding":2}`,
		string(sessionEvents(h, "DIFFICULTY_ADJUSTED")[0].Payload))
	require.Equal(t, 1, h.Learner.Rerenders())
	gens := h.Router.Generations()
	require.Contains(t, gens[3].LLMRequests()[0].Messages[0].Content, "adjustments=0", "the running generation keeps the old difficulty")

	answerTurn(t, h, "次の文章です。", 4)
	gens = h.Router.Generations()
	require.Contains(t, gens[4].LLMRequests()[0].Messages[0].Content, "speed=2 adjustments=1")
}

func TestSpokenTextAfterAudioOnly(t *testing.T) {
	// assistant_spoken_text is what was synthesised and written: a fragment of text the parser
	// never cut (DONE{TRUNCATED}, S-7) is in assistant_text but not in assistant_spoken_text.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")),
		rtharness.WithScript(fakes.Script{Deltas: []string{"こんにちは。", "今日は"}, Finish: provider.FinishTruncated}))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	h.Sync()
	done := h.Events("ai.audio_completed")[0].Payload.(protocol.AIAudioCompleted)
	require.False(t, done.LLMFinished)
	require.Equal(t, 1, done.SentenceCount)
	h.WaitFor("generation text", func() bool { return strings.Contains(joinDeltas(h), "今日は") })

	row := turnRow(t, h, 1)
	require.Nil(t, row.AssistantSpokenText, "assistant_spoken_text is written with the final status")
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	tc := completed(h)[0]
	require.Equal(t, "COMPLETED", tc.Status, "a truncated generation is a COMPLETED turn")
	require.Equal(t, "こんにちは。今日は", *tc.AssistantText)
	require.Equal(t, "こんにちは。", *tc.AssistantSpokenText)
	row = finalRow(t, h, 1)
	require.Equal(t, "こんにちは。今日は", *row.AssistantText)
	require.Equal(t, "こんにちは。", *row.AssistantSpokenText)
}

func joinDeltas(h *rtharness.H) string {
	var b strings.Builder
	for _, ev := range h.Events("ai.text_delta") {
		b.WriteString(ev.Payload.(protocol.AITextDelta).Text)
	}
	return b.String()
}

func TestTextDeltasAreCoalesced(t *testing.T) {
	// ai.text_delta is coalesced to 100 ms and never reordered after the events that follow it.
	deltas := []string{"こ", "ん", "に", "ち", "は", "。", "今日", "は", "いい", "天気", "です", "ね。"}
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")),
		rtharness.WithScript(fakes.Script{Deltas: deltas, PCMPerSentenceMS: 100}))
	started(t, h)
	utter(h, "ja_question")
	h.WaitEvents("ai.audio_completed", 1)
	h.Sync()
	require.Equal(t, strings.Join(deltas, ""), joinDeltas(h), "all the text arrives, in order")
	require.Less(t, len(h.Events("ai.text_delta")), len(deltas), "deltas inside 100 ms are merged")
	types := ofTurn(h, 1)
	last := -1
	for i, typ := range types {
		if typ == "ai.text_delta" {
			last = i
		}
	}
	require.Less(t, last, indexOf(types, "ai.audio_completed"), "text before audio_completed")
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}
	return -1
}

func TestStarvationEndsTurn(t *testing.T) {
	// §7.5.2: a client that stops sending frames (a stalled tab) does not leave the user turn
	// open: the loop injects the silence that passed.
	h := rtharness.New(t, rtharness.WithTimeline(timelineOf(t, "ja_question")))
	started(t, h)
	h.Speak("ja_question")
	h.Sync()
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
	h.AdvanceClock(100 * time.Millisecond) // within 200 ms: nothing yet
	h.Tick()
	h.Sync()
	require.Equal(t, conversation.StateUserSpeaking, h.View().TurnState)
	h.AdvanceClock(2 * time.Second)
	h.Tick()
	h.Sync()
	f := h.WaitEvents("stt.final", 1)[0].Payload.(protocol.STTFinal)
	require.Equal(t, questionJA, f.Text)
	require.Equal(t, "SILENCE", f.EndReason)
}

func TestInvalidFrameBurstViaRuntime(t *testing.T) {
	// Undecodable packets reach the client as one AUDIO_FRAME_INVALID per 5 s and, more than 50 in
	// 10 s, end the connection with PROTOCOL_VIOLATION 1008 (§7.5.1).
	h := rtharness.New(t, rtharness.WithTimeline(replay.Timeline{}))
	started(t, h)
	for range 20 {
		h.SendRawUpstream([]byte{0x00})
	}
	h.Sync()
	require.Len(t, h.Events("error"), 1)
	e := errorOf(t, h)
	require.Equal(t, string(errs.CodeAudioFrameInvalid), e.Error.Code)
	require.False(t, e.Fatal)
	require.Equal(t, 20.0, h.Metrics.AudioFramesInTotal("invalid"))
	require.Equal(t, conversation.StateListening, h.View().TurnState, "silence frames are not speech")

	for range 31 {
		h.SendRawUpstream([]byte{0x00})
	}
	code, reason := h.WaitClosed()
	require.Equal(t, 1008, code)
	require.Equal(t, "PROTOCOL_VIOLATION", reason)
	require.Equal(t, 1.0, h.Metrics.ProtocolViolationsTotal(conversation.ViolationInvalidBurst))
}
