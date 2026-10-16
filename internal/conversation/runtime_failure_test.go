package conversation_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// Scripts of the generations the failure tests need.
var (
	okScript = fakes.Script{Deltas: []string{"こんにちは。", "今日はいい天気ですね。"}, PCMPerSentenceMS: 100}
	// llmFails: the LLM errors before its first token.
	llmFails = fakes.Script{
		Deltas: []string{"こんにちは。"}, PCMPerSentenceMS: 100,
		LLMErr: provider.NewError(provider.FakePrimary, provider.CapLLM, provider.KindUnavailable, "scripted LLM failure"),
	}
	// ttsFails: TTS errors on the first sentence, before any audio (the character has no voice.fallback).
	ttsFails = fakes.Script{
		Deltas: okScript.Deltas, PCMPerSentenceMS: 100,
		TTSErr: provider.NewError(provider.FakePrimary, provider.CapTTS, provider.KindUnavailable, "scripted TTS failure"),
	}
	// filteredDone and filteredError: the content filter stops the answer (S-9), as DONE and as ERROR.
	filteredDone  = fakes.Script{PCMPerSentenceMS: 100, Finish: provider.FinishContentFiltered}
	filteredError = fakes.Script{
		Deltas: []string{"こんにちは。"}, PCMPerSentenceMS: 100,
		LLMErr: provider.NewError(provider.FakePrimary, provider.CapLLM, provider.KindContentFiltered, "blocked"),
	}
	truncated = fakes.Script{Deltas: okScript.Deltas, PCMPerSentenceMS: 100, Finish: provider.FinishTruncated}
)

// generationsOf returns the ai.generation_started events of the given kind.
func generationsOf(h *rtharness.H, kind string) []protocol.ServerEvent {
	var out []protocol.ServerEvent
	for _, ev := range h.Events("ai.generation_started") {
		if ev.Payload.(protocol.AIGenerationStarted).Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// userSays plays one user utterance whose STT text is text (the FINAL of the commit carries it).
func userSays(h *rtharness.H, text string) {
	h.T.Helper()
	h.Router.STT().SetFinalizeText(text)
	h.Speak("ja_greeting_short")
	h.Silence(1200)
}

// apologized finishes a turn that failed and is answered by the APOLOGY: it waits for the apology to
// be sent, reports its playback and waits for turn.completed number n.
func apologized(t *testing.T, h *rtharness.H, n int) {
	t.Helper()
	h.WaitFor("the apology", func() bool { return len(generationsOf(h, "APOLOGY")) >= n })
	h.WaitEvents("ai.audio_completed", n)
	apologies := generationsOf(h, "APOLOGY")
	h.Send(h.PlaybackFinishedCmd(*apologies[len(apologies)-1].Generation))
	h.WaitEvents("turn.completed", n)
	h.Sync()
}

// repliedTo finishes a turn answered normally: audio completed, playback finished, turn.completed n.
func repliedTo(t *testing.T, h *rtharness.H, n int) {
	t.Helper()
	h.WaitEvents("ai.audio_completed", n)
	h.Send(h.PlaybackFinishedCmd(lastGeneration(t, h)))
	h.WaitEvents("turn.completed", n)
	h.Sync()
}

// eventPayloads returns the payloads of the session_events rows of a type.
func eventPayloads(h *rtharness.H, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range h.Written.Events() {
		if ev.Type != typ {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			h.T.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestReplyFailureApology(t *testing.T) {
	for _, tc := range []struct {
		name, stage, class string
		script             fakes.Script
	}{
		{"the LLM errors", "LLM", "UNAVAILABLE", llmFails},
		{"the TTS errors before any audio", "TTS", "UNAVAILABLE", ttsFails},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Row 11: nothing was heard, so there is nothing to interrupt; the turn is FAILED and
			// persisted only when the APOLOGY has played.
			h := rtharness.New(t, rtharness.WithScript(tc.script))
			started(t, h)
			userSays(h, questionJA)
			apologized(t, h, 1)

			types := ofTurn(h, 1)
			requireOrder(t, types, "stt.final", "ai.generation_started", "error", "ai.generation_started",
				"ai.audio_started", "ai.sentence", "ai.audio_completed", "turn.completed")
			require.NotContains(t, types, "ai.interrupted", "the reply never got to speak")
			gens := h.Events("ai.generation_started")
			require.Len(t, gens, 2)
			require.Equal(t, "REPLY", gens[0].Payload.(protocol.AIGenerationStarted).Kind)
			require.Equal(t, "APOLOGY", gens[1].Payload.(protocol.AIGenerationStarted).Kind)
			require.Equal(t, uint32(3), *gens[1].Generation, "the failed reply's number 1 is cancelled (2), the apology is 3")

			ev := h.Events("error")
			require.Len(t, ev, 1)
			p := ev[0].Payload.(protocol.ErrorPayload)
			require.Equal(t, "AI_GENERATION_FAILED", p.Error.Code)
			require.False(t, p.Fatal)
			require.True(t, conversation.EventDurable(ev[0]), "a non-fatal error is durable (replayed on resume)")
			require.Equal(t, audio.ApologyText, h.Events("ai.sentence")[0].Payload.(protocol.AISentence).Text)
			var frames int
			for _, f := range h.Frames() {
				if f.Gen == 3 {
					frames++
				}
			}
			require.Positive(t, frames, "the apology is audio")

			tcm := completed(h)[0]
			require.Equal(t, "FAILED", tcm.Status)
			require.Equal(t, questionJA, *tcm.UserText)
			row := finalRow(t, h, 1)
			require.Equal(t, session.TurnFailed, *row.Status)
			require.Equal(t, "AI_GENERATION_FAILED", *row.ErrorCode)
			require.Equal(t, questionJA, *row.UserText)
			if row.AssistantText != nil {
				require.NotContains(t, *row.AssistantText, audio.ApologyText, "the apology is never the turn's text")
			}
			require.Equal(t, 1, h.View().FailedTurns)
			require.Equal(t, conversation.StateListening, h.View().TurnState)
			require.Equal(t, session.StatusActive, h.View().Status)
			require.Equal(t, 1.0, h.Metrics.TurnsTotal("FAILED"))

			h.WaitFor("TURN_FAILED row", func() bool { return len(eventPayloads(h, "TURN_FAILED")) == 1 })
			ev0 := eventPayloads(h, "TURN_FAILED")[0]
			require.Equal(t, tc.stage, ev0["stage"])
			require.Equal(t, tc.class, ev0["error_class"])
			require.EqualValues(t, 1, ev0["turn_seq"])
		})
	}
}

// holdSecondSentence is a provider.Generation whose TTS speaks the first sentence and then, once gate
// is closed, fails: the failure comes after the first audio.
type holdSecondSentence struct {
	provider.Generation
	gate <-chan struct{}
}

func (g holdSecondSentence) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	if strings.HasSuffix(req.RequestID, ":0") {
		return g.Generation.Synthesize(ctx, req)
	}
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, provider.NewError(provider.FakePrimary, provider.CapTTS, provider.KindUnavailable, "scripted failure after the first sentence")
}

func TestFailureAfterFirstAudio16(t *testing.T) {
	gate := make(chan struct{})
	h := rtharness.New(t, rtharness.WithScript(okScript),
		rtharness.WithGenerationWrapper(func(g provider.Generation) provider.Generation { return holdSecondSentence{g, gate} }))
	started(t, h)
	userSays(h, questionJA)
	h.WaitEvents("ai.audio_started", 1)
	h.WaitFor("sentence 0 written", func() bool { return writtenOf(h, 1) == 5 })
	close(gate)
	h.WaitEvents("ai.interrupted", 1)
	apologized(t, h, 1)

	requireOrder(t, ofTurn(h, 1), "ai.audio_started", "ai.interrupted", "error", "ai.generation_started", "turn.completed")
	it := h.Events("ai.interrupted")[0]
	p := it.Payload.(protocol.AIInterrupted)
	require.Equal(t, "GENERATION_FAILED", p.Reason)
	require.Equal(t, uint32(2), p.ActiveGeneration)
	require.Equal(t, 1, p.SpokenSentenceCount)
	require.Equal(t, uint32(1), *it.Generation)
	require.Equal(t, uint32(3), *generationsOf(h, "APOLOGY")[0].Generation)

	tcm := completed(h)[0]
	require.Equal(t, "FAILED", tcm.Status)
	require.Equal(t, "こんにちは。", *tcm.AssistantSpokenText, "the sentence the writer finished; sentence 1 was parsed but never heard")
	row := finalRow(t, h, 1)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "こんにちは。", *row.AssistantSpokenText)
	require.Contains(t, *row.AssistantText, "こんにちは。")
	require.NotContains(t, *row.AssistantText, audio.ApologyText)
	require.Equal(t, "AI_GENERATION_FAILED", *row.ErrorCode)
	require.Equal(t, 1, h.View().FailedTurns)
	h.WaitFor("TURN_FAILED row", func() bool { return len(eventPayloads(h, "TURN_FAILED")) == 1 })
	require.Equal(t, "TTS", eventPayloads(h, "TURN_FAILED")[0]["stage"])
}

func TestOpeningFailure12(t *testing.T) {
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst), rtharness.WithScript(llmFails))
	h.Connect(h.StartCmd())
	h.WaitEvents("turn.completed", 1)
	h.Sync()

	requireOrder(t, ofTurn(h, 0), "turn.started", "ai.generation_started", "error", "turn.completed")
	require.Equal(t, "FAILED", completed(h)[0].Status)
	require.Equal(t, "AI_GENERATION_FAILED", h.Events("error")[0].Payload.(protocol.ErrorPayload).Error.Code)
	require.Empty(t, generationsOf(h, "APOLOGY"), "an opening that failed is not apologized for")
	require.Len(t, h.Events("ai.generation_started"), 1, "the opening is not tried again")
	require.Equal(t, conversation.StateListening, h.View().TurnState)
	row := finalRow(t, h, 0)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "AI_GENERATION_FAILED", *row.ErrorCode)
	require.Equal(t, 1, h.View().FailedTurns, "a FAILED turn, the opening included, is counted")
}

func TestThreeFailedTurnsEndSession(t *testing.T) {
	t.Run("three in a row", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithScript(llmFails))
		started(t, h)
		for i := 1; i <= 2; i++ {
			userSays(h, questionJA)
			apologized(t, h, i)
			require.Equal(t, i, h.View().FailedTurns)
			require.Equal(t, session.StatusActive, h.View().Status)
		}
		userSays(h, questionJA)
		code, reason := h.WaitClosed()
		require.Equal(t, 4500, code)
		require.Equal(t, "PROVIDER_FAILURE", reason)

		failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
		require.Equal(t, "PROVIDER_FAILURE", failed.EndReason)
		require.Equal(t, "PROVIDER_FAILURE", failed.ErrorCode)
		require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Fail"}, h.Calls.Calls())
		require.Len(t, generationsOf(h, "APOLOGY"), 2, "the third failure ends the session instead of apologizing")
		require.Equal(t, 3.0, h.Metrics.TurnsTotal("FAILED"))
		h.WaitFor("turn 3 FAILED", func() bool { r := turnRow(t, h, 3); return r.Status != nil })
		require.Equal(t, session.TurnFailed, *turnRow(t, h, 3).Status)
	})

	t.Run("a completed turn resets the streak", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithScript(llmFails))
		started(t, h)
		for i := 1; i <= 2; i++ {
			userSays(h, questionJA)
			apologized(t, h, i)
		}
		h.Router.SetScript(okScript)
		userSays(h, questionJA)
		repliedTo(t, h, 3)
		require.Equal(t, 0, h.View().FailedTurns)

		h.Router.SetScript(llmFails)
		for i := 1; i <= 2; i++ {
			userSays(h, questionJA)
			apologized(t, h, 2+i)
			require.Equal(t, i, h.View().FailedTurns)
		}
		require.Equal(t, session.StatusActive, h.View().Status, "two failures after the reset are not three in a row")
		require.NotContains(t, h.Calls.Calls(), "Fail")
	})
}

func TestContentFilteredNotCounted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script fakes.Script
	}{{"DONE", filteredDone}, {"ERROR", filteredError}} {
		t.Run(tc.name, func(t *testing.T) {
			h := rtharness.New(t, rtharness.WithScript(tc.script))
			started(t, h)
			for i := 1; i <= 3; i++ {
				userSays(h, questionJA)
				apologized(t, h, i)
				require.Equal(t, 0, h.View().FailedTurns, "CONTENT_FILTERED neither raises nor resets the streak")
				require.Equal(t, i, h.View().ContentFilteredTurns)
			}
			require.Len(t, generationsOf(h, "APOLOGY"), 3)
			require.Equal(t, session.StatusActive, h.View().Status)
			require.Equal(t, 3.0, h.Metrics.LLMContentFilteredTotal("FAKE_PRIMARY"))
			require.NotContains(t, h.Calls.Calls(), "Fail")
			require.NotContains(t, h.Calls.Calls(), "BeginEnding")
			require.Equal(t, 3.0, h.Metrics.TurnsTotal("FAILED"))
			require.Equal(t, "AI_GENERATION_FAILED", *finalRow(t, h, 3).ErrorCode)
		})
	}

	t.Run("it does not reset a streak either", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithScript(llmFails))
		started(t, h)
		userSays(h, questionJA)
		apologized(t, h, 1)
		h.Router.SetScript(filteredDone)
		userSays(h, questionJA)
		apologized(t, h, 2)
		require.Equal(t, 1, h.View().FailedTurns, "the filtered turn left the streak at 1")
		h.Router.SetScript(llmFails)
		userSays(h, questionJA)
		apologized(t, h, 3)
		userSays(h, questionJA) // the third real failure
		code, _ := h.WaitClosed()
		require.Equal(t, 4500, code)
	})
}

func TestNonJapaneseSTTFailsTurn(t *testing.T) {
	h := rtharness.New(t, rtharness.WithScript(llmFails))
	started(t, h)
	userSays(h, questionJA) // one real failure first: the streak is 1
	apologized(t, h, 1)
	require.Equal(t, 1, h.View().FailedTurns)

	for i := 2; i <= 4; i++ { // three turns of English: FAILED + APOLOGY, none counted (a streak of 4 would have ended the session)
		userSays(h, "OK thanks")
		apologized(t, h, i)
		require.Equal(t, 1, h.View().FailedTurns, "neither raised nor reset")
		require.Equal(t, session.StatusActive, h.View().Status)
	}
	require.Len(t, h.Router.Generations(), 1, "an English turn has no LLM generation")
	require.Len(t, generationsOf(h, "REPLY"), 1)
	require.Equal(t, 4.0, h.Metrics.TurnsTotal("FAILED"))

	sf := h.Events("stt.final")
	require.Equal(t, "OK thanks", sf[1].Payload.(protocol.STTFinal).Text)
	row := finalRow(t, h, 2)
	require.Equal(t, session.TurnFailed, *row.Status)
	require.Equal(t, "STT_NON_JAPANESE", *row.ErrorCode)
	require.Equal(t, "OK thanks", *row.UserText)
	require.Nil(t, row.AssistantText)
	h.WaitFor("TURN_FAILED row", func() bool { return len(eventPayloads(h, "TURN_FAILED")) == 4 })
	require.Equal(t, "STT", eventPayloads(h, "TURN_FAILED")[1]["stage"])
	require.NotContains(t, h.Calls.Calls(), "BeginEnding")
}

func TestTruncatedIsCompleted(t *testing.T) {
	// S-7: the answer is cut after audio was sent: what was parsed is spoken, the turn is COMPLETED
	// (and resets the streak), not a failure.
	h := rtharness.New(t, rtharness.WithScript(llmFails))
	started(t, h)
	for i := 1; i <= 2; i++ {
		userSays(h, questionJA)
		apologized(t, h, i)
	}
	require.Equal(t, 2, h.View().FailedTurns)

	h.Router.SetScript(truncated)
	userSays(h, questionJA)
	repliedTo(t, h, 3)
	done := h.Events("ai.audio_completed")[2].Payload.(protocol.AIAudioCompleted)
	require.False(t, done.LLMFinished)
	require.Equal(t, "COMPLETED", completed(h)[2].Status)
	require.Equal(t, session.TurnCompleted, *finalRow(t, h, 3).Status)
	require.Equal(t, 0, h.View().FailedTurns, "a turn that was cut is a completed turn: the streak resets")
	require.Equal(t, 1.0, h.Metrics.LLMTruncatedTotal())
	require.Equal(t, session.StatusActive, h.View().Status)
	require.Len(t, generationsOf(h, "APOLOGY"), 2, "no apology for a truncated answer")
}

func TestFailedTurnDoesNotChangeCharge(t *testing.T) {
	// A FAILED turn changes nothing about the charge: a session with one ends and settles exactly like
	// one with a good turn of the same length (§7.6.1).
	run := func(script fakes.Script, apology bool) (session.Settlement, []string) {
		h := rtharness.New(t, rtharness.WithScript(script))
		started(t, h)
		userSays(h, questionJA)
		if apology {
			apologized(t, h, 1)
		} else {
			repliedTo(t, h, 1)
		}
		h.AdvanceClock(rtharness.T0.Add(90 * time.Second).Sub(h.Clock.Now())) // both sessions are 90 s old at the end
		h.Send(h.EndCmd())
		h.WaitEvents("session.completed", 1)
		c := first(t, h, "session.completed").Payload.(protocol.SessionCompleted)
		return session.Settlement{ChargedSeconds: c.ChargedSeconds, DisconnectedSeconds: c.DisconnectedSeconds}, h.Calls.Calls()
	}
	good, goodCalls := run(okScript, false)
	bad, badCalls := run(llmFails, true)
	require.Equal(t, 90, good.ChargedSeconds)
	require.Equal(t, good, bad)
	require.Equal(t, goodCalls, badCalls, "the same transitions, nothing extra for the failed turn")
}

func TestSTTErrorEndsSessionProviderFailure(t *testing.T) {
	h := rtharness.New(t)
	started(t, h)
	h.Router.STT().Fail(provider.NewError(provider.FakePrimary, provider.CapSTT, provider.KindUnavailable, "the vendor stream is gone"))
	code, reason := h.WaitClosed()
	require.Equal(t, 4500, code)
	require.Equal(t, "PROVIDER_FAILURE", reason)
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "PROVIDER_FAILURE", failed.EndReason)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Fail"}, h.Calls.Calls())
	require.Equal(t, 1, h.Router.STTOpens(), "no second vendor, no reopening")
	require.NotContains(t, h.Written.Types(), "PROVIDER_FALLBACK")
	var fatal []protocol.ErrorPayload
	for _, ev := range h.Events("error") {
		if p := ev.Payload.(protocol.ErrorPayload); p.Fatal {
			fatal = append(fatal, p)
		}
	}
	require.Len(t, fatal, 1)
	require.Equal(t, "PROVIDER_FAILURE", fatal[0].Error.Code)
}

func TestFailedTurnEntersRecentTurns(t *testing.T) {
	// The prompt of the next reply shows what the user said in the turn that failed (§7.8.1: a FAILED
	// turn carries only the user's side).
	h := rtharness.New(t, rtharness.WithScript(llmFails))
	started(t, h)
	userSays(h, "趣味は映画です。")
	apologized(t, h, 1)
	h.Router.SetScript(okScript)
	userSays(h, "二番目の発話です。")
	repliedTo(t, h, 2)

	gens := h.Router.Generations()
	reqs := gens[len(gens)-1].LLMRequests()
	require.Len(t, reqs, 1)
	var users []string
	for _, m := range reqs[0].Messages {
		if m.Role == provider.RoleUser {
			users = append(users, m.Content)
		}
	}
	require.Equal(t, []string{"趣味は映画です。", "二番目の発話です。"}, users)
}

func TestTTSFallbackWritesProviderFallback(t *testing.T) {
	// TTS moved to the character's voice.fallback before any audio: the turn goes on with that voice
	// and the switch is a session event (D-017: the only vendor fallback).
	h := rtharness.New(t, rtharness.WithVoiceFallback(), rtharness.WithScript(ttsFails))
	started(t, h)
	userSays(h, questionJA)
	repliedTo(t, h, 1)
	require.Equal(t, "COMPLETED", completed(h)[0].Status)
	row := finalRow(t, h, 1)
	require.True(t, row.UsedFallback)
	require.Equal(t, rtharness.FallbackVoice.VoiceID, *row.TTSVoiceID)
	h.WaitFor("PROVIDER_FALLBACK row", func() bool { return len(eventPayloads(h, "PROVIDER_FALLBACK")) == 1 })
	ev := eventPayloads(h, "PROVIDER_FALLBACK")[0]
	require.Equal(t, "TTS", ev["capability"])
	require.Equal(t, rtharness.PrimaryVoice.ProviderName, ev["from"])
	require.Equal(t, rtharness.FallbackVoice.ProviderName, ev["to"])
	require.EqualValues(t, 1, ev["turn_seq"])
	require.Equal(t, 0, h.View().FailedTurns)
}
