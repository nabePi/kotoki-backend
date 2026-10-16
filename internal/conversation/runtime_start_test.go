package conversation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// first returns the only event of typ; the test fails when there is none or more than one.
func first(t *testing.T, h *rtharness.H, typ string) protocol.ServerEvent {
	t.Helper()
	evs := h.Events(typ)
	require.Len(t, evs, 1, "events of type %s in %v", typ, h.Types())
	return evs[0]
}

func errorOf(t *testing.T, h *rtharness.H) protocol.ErrorPayload {
	t.Helper()
	return first(t, h, "error").Payload.(protocol.ErrorPayload)
}

func sessionEvents(h *rtharness.H, typ string) []fakes.WrittenEvent {
	var out []fakes.WrittenEvent
	for _, e := range h.Written.Events() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestStartUserFirst(t *testing.T) {
	h := rtharness.New(t)
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	h.Sync()

	ready := first(t, h, "session.ready").Payload.(protocol.SessionReady)
	require.Equal(t, "RESERVED", ready.Status)
	require.Equal(t, "session.start", ready.ExpectedCommand)
	require.Equal(t, "USER_FIRST", ready.ConversationStartMode)
	require.Equal(t, 1, ready.ProtocolVersion)
	require.Equal(t, "0x01|u32 seq", ready.Audio.Upstream.FrameHeader)
	require.Equal(t, 15, ready.HeartbeatIntervalS)

	ev := first(t, h, "session.started")
	started := ev.Payload.(protocol.SessionStarted)
	require.Equal(t, "ACTIVE", started.Status)
	require.Equal(t, "LISTENING", started.TurnState)
	require.Equal(t, 600, started.RemainingSeconds)
	require.Equal(t, 600, started.DurationSeconds)
	require.Equal(t, rtharness.T0.Add(600*time.Second), started.ExpiresAt.Time)
	require.NotNil(t, ev.Generation, "session.started carries the generation (0)")
	require.Equal(t, uint32(0), *ev.Generation)
	require.Equal(t, []string{"session.ready", "session.started"}, h.Types())
	require.Less(t, h.Events("session.ready")[0].ServerSeq, ev.ServerSeq)

	// §7.6.3: ClaimRuntime([RESERVED]) → MarkStarting → Activate, in this order.
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate"}, h.Calls.Calls())
	require.Equal(t, [][]session.Status{{session.StatusReserved}}, h.Session.Claims())

	pod, fence, ok := h.Store.Owner(h.SID)
	require.True(t, ok, "AcquireOwner ran")
	require.Equal(t, "pod-a", pod)
	require.Equal(t, int64(1), fence)

	h.WaitFor("OWNER_CHANGED", func() bool { return len(sessionEvents(h, "OWNER_CHANGED")) == 1 })
	owner := sessionEvents(h, "OWNER_CHANGED")[0]
	require.JSONEq(t, `{"reason":"START","from_pod":"","to_pod":"pod-a","fence":1}`, string(owner.Payload))
	require.Equal(t, int64(1), owner.Fence)

	v := h.View()
	require.Equal(t, session.StatusActive, v.Status)
	require.Equal(t, conversation.StateListening, v.TurnState)
	require.True(t, v.Accepting)
	require.True(t, v.Connected)
	require.Equal(t, uint32(0), v.Generation)
	require.Equal(t, 1, v.NextTurnSeq, "the first user turn is turn_seq 1")
	require.Equal(t, 1, h.Router.STTOpens())
	cfg := h.Router.STTConfigs()[0]
	require.Equal(t, "ja", cfg.Language)
	require.Equal(t, provider.STTSampleRate, cfg.SampleRate)
	require.Equal(t, h.SID, cfg.SessionID)
	require.Equal(t, 1.0, h.Metrics.ConnectionsTotal(conversation.ConnAccepted))

	// The redisSync publishes the rt hash on the next ticks.
	h.AdvanceClock(200 * time.Millisecond)
	h.Tick()
	h.Sync()
	h.WaitFor("rt hash", func() bool {
		rt, found, err := h.Store.ReadRT(t.Context(), h.SID)
		return err == nil && found && rt.Status == "ACTIVE"
	})
	rt, _, _ := h.Store.ReadRT(t.Context(), h.SID)
	require.True(t, rt.AcceptingTurns)
	require.True(t, rt.Connected)
	require.Equal(t, 1, rt.NextTurnSeq)
	require.Equal(t, int64(1), rt.Fence)
}

func TestStartLoadsCharacterFromSnapshot(t *testing.T) {
	// The characters table is never read: the live row may be DISABLED (or gone) and the session
	// still starts, with the voices of config_snapshot (D-014, D-015).
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst), rtharness.WithVoiceFallback())
	h.Connect(h.StartCmd())
	h.WaitEvents("ai.audio_started", 1)
	h.Sync()

	require.Len(t, h.Events("session.started"), 1)
	gens := h.Router.Generations()
	require.Len(t, gens, 1)
	want := provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.Name(rtharness.PrimaryVoice.ProviderName), ModelName: rtharness.PrimaryVoice.ModelName, VoiceID: rtharness.PrimaryVoice.VoiceID},
		Fallback: &provider.VoiceRef{Provider: provider.Name(rtharness.FallbackVoice.ProviderName), ModelName: rtharness.FallbackVoice.ModelName, VoiceID: rtharness.FallbackVoice.VoiceID},
	}
	require.Equal(t, want, gens[0].Voice, "NewGeneration gets the snapshot voices (and no tier: D-016)")
	calls := gens[0].TTSCalls()
	require.NotEmpty(t, calls)
	for _, c := range calls {
		require.Equal(t, want, c.Req.Voice, "TTSRequest.Voice")
	}
	require.Equal(t, 1, h.Content.Loads())
}

func TestStartAIFirst(t *testing.T) {
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
	h.Connect(h.StartCmd())
	h.WaitEvents("ai.audio_completed", 1)
	h.Sync()

	types := h.Types()
	idx := func(typ string) int {
		for i, x := range types {
			if x == typ {
				return i
			}
		}
		return -1
	}
	require.Equal(t, []string{"session.ready", "session.started", "turn.started", "ai.generation_started"}, types[:4])
	require.Less(t, idx("ai.generation_started"), idx("ai.audio_started"))
	require.Less(t, idx("ai.audio_started"), idx("ai.audio_completed"))

	ts := first(t, h, "turn.started")
	require.Equal(t, "AI", ts.Payload.(protocol.TurnStarted).Initiator)
	require.NotNil(t, ts.TurnID)
	require.Equal(t, 0, *ts.TurnSeq, "the opening is turn 0")
	gs := first(t, h, "ai.generation_started")
	require.Equal(t, "OPENING", gs.Payload.(protocol.AIGenerationStarted).Kind)
	require.NotNil(t, gs.Generation)
	require.Equal(t, uint32(1), *gs.Generation)
	require.Equal(t, *ts.TurnID, *gs.TurnID)

	v := h.View()
	require.Equal(t, uint32(1), v.Generation)
	require.Equal(t, 1, v.NextTurnSeq)
	require.Equal(t, conversation.StateAISpeaking, v.TurnState)
	require.Equal(t, *ts.TurnID, v.CurrentTurn)

	// Turn 0 is INSERTed with no status and no user text.
	h.WaitFor("turn 0 row", func() bool { return len(h.Turns.Turns(h.SID)) == 1 })
	row := h.Turns.Turns(h.SID)[0]
	require.Equal(t, 0, row.Sequence)
	require.Nil(t, row.Status)
	require.Nil(t, row.UserText)

	// The opening used the OPENING version of the session record, loaded by its id (S-2).
	require.Equal(t, 1, len(h.Content.OpeningLoads()))
	llm := h.Router.Generations()[0].LLMRequests()
	require.Len(t, llm, 1)
	require.Equal(t, provider.RoleSystem, llm[0].Messages[0].Role)
	require.Len(t, llm[0].Messages, 1, "an opening prompt is the system message only")

	frames := h.Frames()
	require.NotEmpty(t, frames)
	for i, f := range frames {
		require.Equal(t, uint32(1), f.Gen)
		require.Equal(t, uint32(i), f.Seq)
	}
	require.Len(t, h.Events("ai.sentence"), 2)
	done := first(t, h, "ai.audio_completed").Payload.(protocol.AIAudioCompleted)
	require.Equal(t, len(frames), done.FrameCount)
	require.Equal(t, 2, done.SentenceCount)
	require.True(t, done.LLMFinished)
}

func TestOpeningFallbackStatic(t *testing.T) {
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst), rtharness.WithoutOpeningPrompt())
	h.Connect(h.StartCmd())
	done := h.WaitEvents("ai.audio_completed", 1)[0]
	h.Sync()

	gens := h.Router.Generations()
	require.Len(t, gens, 1)
	require.Empty(t, gens[0].LLMRequests(), "StreamLLM is never called for the static opening")
	var spoken []string
	for _, c := range gens[0].TTSCalls() {
		spoken = append(spoken, c.Req.Text)
	}
	require.Equal(t, []string{"いらっしゃいませ。", "ご注文はお決まりですか。"}, spoken)
	require.Empty(t, h.Content.OpeningLoads(), "no OPENING version is loaded when the record has none")
	require.Equal(t, 1.0, h.Metrics.OpeningFallbackTotal())

	// The turn closes when the client reports the end of playback.
	h.Send(h.PlaybackFinishedCmd(1))
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	_ = done
	tc := first(t, h, "turn.completed").Payload.(protocol.TurnCompleted)
	require.Equal(t, "COMPLETED", tc.Status)
	require.Nil(t, tc.UserText)
	require.Equal(t, rtharness.OpeningLine, *tc.AssistantText)
	require.Nil(t, tc.Latency)

	h.WaitFor("turn 0 final", func() bool {
		ts := h.Turns.Turns(h.SID)
		return len(ts) == 1 && ts[0].Status != nil
	})
	row := h.Turns.Turns(h.SID)[0]
	require.Equal(t, session.TurnCompleted, *row.Status)
	require.Equal(t, rtharness.OpeningLine, *row.AssistantText)
	require.Equal(t, rtharness.OpeningLine, *row.AssistantSpokenText)
	require.Nil(t, row.LLMProvider, "llm_provider is NULL for the static opening")
	require.Nil(t, row.LLMModel)
	require.Nil(t, row.InputTokens)
	require.Nil(t, row.OutputTokens)
	require.Equal(t, rtharness.PrimaryVoice.VoiceID, *row.TTSVoiceID)
	require.NotNil(t, row.TurnEndToFirstAudioMS)
	require.GreaterOrEqual(t, *row.TurnEndToFirstAudioMS, 0)
	require.Equal(t, 1.0, h.Metrics.TurnsTotal("COMPLETED"))
	require.Equal(t, conversation.StateListening, h.View().TurnState)
	require.Equal(t, 2, h.Router.STT().FinalizeCalls(), "S-8: a boundary commit entering AI_SPEAKING and one back to LISTENING")
	h.WaitFor("both DISCARD finals dropped", func() bool {
		h.Sync()
		return h.Metrics.STTDiscardedTotal("ai_speaking") == 1 && h.Metrics.STTDiscardedTotal("listening") == 1
	})
}

func TestOpeningPlaybackDeadline(t *testing.T) {
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
	h.Connect(h.StartCmd())
	h.WaitEvents("ai.audio_completed", 1)
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState)

	// No client.playback_finished: the turn closes at the estimated end of playback + 1.5 s.
	h.AdvanceClock(200 * time.Millisecond) // the audio lasts 200 ms (two sentences of 100 ms)
	h.Tick()
	h.Sync()
	require.Equal(t, conversation.StateAISpeaking, h.View().TurnState, "the grace period has not passed")
	h.AdvanceClock(1500 * time.Millisecond)
	h.Tick()
	h.WaitEvents("turn.completed", 1)
	h.Sync()
	require.Equal(t, conversation.StateListening, h.View().TurnState)
	require.Equal(t, 1.0, h.Metrics.PlaybackFinishedMissingTotal())
	require.Equal(t, "COMPLETED", first(t, h, "turn.completed").Payload.(protocol.TurnCompleted).Status)
}

func TestOpeningFailureFailsTurn(t *testing.T) {
	// Row 12: an opening whose LLM fails is not retried; turn 0 is FAILED and the session listens.
	llmErr := provider.NewError(provider.FakePrimary, provider.CapLLM, provider.KindUnavailable, "scripted")
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst),
		rtharness.WithScript(fakes.Script{Deltas: []string{"こんにちは。"}, LLMErr: llmErr}))
	h.Connect(h.StartCmd())
	h.WaitEvents("turn.completed", 1)
	h.Sync()

	e := errorOf(t, h)
	require.Equal(t, string(errs.CodeAiGenerationFailed), e.Error.Code)
	require.False(t, e.Fatal)
	tc := first(t, h, "turn.completed").Payload.(protocol.TurnCompleted)
	require.Equal(t, "FAILED", tc.Status)
	require.Equal(t, conversation.StateListening, h.View().TurnState)
	require.Len(t, h.Events("ai.generation_started"), 1, "the opening is not played again")
	h.WaitFor("turn 0 failed", func() bool {
		ts := h.Turns.Turns(h.SID)
		return len(ts) == 1 && ts[0].Status != nil
	})
	require.Equal(t, session.TurnFailed, *h.Turns.Turns(h.SID)[0].Status)
	require.Equal(t, 1.0, h.Metrics.TurnsTotal("FAILED"))
	require.Equal(t, uint32(2), h.View().Generation, "a cancelled generation consumes its number")
}

func TestStartDeadlinePassed(t *testing.T) {
	h := rtharness.New(t)
	h.Session.FailNext("MarkStarting", session.ErrStartDeadlinePassed)
	h.Connect(h.StartCmd())
	code, reason := h.WaitClosed()
	require.Equal(t, 4010, code)
	require.Equal(t, string(errs.CodeSessionStartDeadlinePassed), reason)
	e := errorOf(t, h)
	require.Equal(t, string(errs.CodeSessionStartDeadlinePassed), e.Error.Code)
	require.True(t, e.Fatal)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting"}, h.Calls.Calls(), "no Activate, no FailStart: the session is already ABANDONED")
	var ae *conversation.AttachError
	require.ErrorAs(t, h.AttachResult(), &ae)
	require.Equal(t, errs.CodeSessionStartDeadlinePassed, ae.Code)
	require.Equal(t, 0, h.Mgr.ActiveSessions())
}

func TestStartFailsWhenSTTUnavailable(t *testing.T) {
	h := rtharness.New(t, rtharness.WithSTTOpenError(errors.New("vendor down")))
	h.Connect(h.StartCmd())
	code, reason := h.WaitClosed()
	require.Equal(t, 4500, code)
	require.Equal(t, "PROVIDER_FAILURE", reason)

	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "FailStart"}, h.Calls.Calls(), "FailStart, never Activate")
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "PROVIDER_FAILURE", failed.EndReason)
	require.Equal(t, "PROVIDER_FAILURE", failed.ErrorCode)
	require.Equal(t, 0, failed.ChargedSeconds, "a failed start charges nothing")
	e := errorOf(t, h)
	require.Equal(t, "PROVIDER_FAILURE", e.Error.Code)
	require.True(t, e.Fatal)
	require.Empty(t, h.Events("session.started"))
	types := h.Types()
	require.Equal(t, []string{"session.ready", "session.failed", "error"}, types)
	require.Equal(t, 1.0, h.Metrics.WSCloseTotal("4500"))

	// Nothing stays behind: the owner key is released, the runtime is gone.
	h.WaitFor("runtime gone", func() bool { return h.Mgr.ActiveSessions() == 0 })
	h.WaitFor("owner released", func() bool { _, _, ok := h.Store.Owner(h.SID); return !ok })
}

func TestStartDegrades(t *testing.T) {
	t.Run("learner fails", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
		h.Learner.FailNext("PromptContext", errors.New("learner down"))
		h.Connect(h.StartCmd())
		h.WaitEvents("ai.generation_started", 1)
		h.Sync()
		require.Len(t, h.Events("session.started"), 1, "the session starts without the learner block")
		sys := h.Router.Generations()[0].LLMRequests()[0].Messages[0].Content
		require.NotContains(t, sys, "タロウ", "no learner context in the prompt")
	})
	t.Run("learner hangs for 1 s", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
		h.Gates.Learner = make(chan struct{}) // never opened
		h.Connect(h.StartCmd())
		h.WaitFor("learner blocked", func() bool { return h.Blocked() == 1 })
		h.Sync()
		require.Empty(t, h.Events("session.started"))
		h.AdvanceClock(999 * time.Millisecond)
		h.Tick()
		h.Sync()
		require.Empty(t, h.Events("session.started"), "1 s has not passed yet")
		h.AdvanceClock(time.Millisecond)
		h.Tick()
		h.WaitEvents("session.started", 1)
		h.WaitEvents("ai.generation_started", 1)
	})
	t.Run("memory degraded", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
		h.Memory.Context = memory.MemoryContext{
			Degraded: true, Core: []memory.ContextItem{{Category: memory.CategoryPreference, Content: "好きな食べ物はラーメン", Score: 1}},
		}
		h.Connect(h.StartCmd())
		h.WaitEvents("ai.generation_started", 1)
		h.Sync()
		require.Len(t, h.Events("session.started"), 1)
		sys := h.Router.Generations()[0].LLMRequests()[0].Messages[0].Content
		require.NotContains(t, sys, "ラーメン", "a Degraded memory context is dropped")
	})
	t.Run("memory ok", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst))
		h.Memory.Context = memory.MemoryContext{
			Core: []memory.ContextItem{{Category: memory.CategoryPreference, Content: "好きな食べ物はラーメン", Score: 1}},
		}
		h.Connect(h.StartCmd())
		h.WaitEvents("ai.generation_started", 1)
		h.Sync()
		sys := h.Router.Generations()[0].LLMRequests()[0].Messages[0].Content
		require.Contains(t, sys, "ラーメン")
		builds := h.Memory.Builds()
		require.Len(t, builds, 1)
		require.Equal(t, rtharness.UserID, builds[0].UserID)
		require.Equal(t, h.SID, builds[0].SessionID)
		require.Contains(t, builds[0].ScenarioText, "旅行")
	})
	t.Run("memory fails", func(t *testing.T) {
		h := rtharness.New(t)
		h.Memory.FailNext("BuildSessionContext", errors.New("memory down"))
		h.Connect(h.StartCmd())
		h.WaitEvents("session.started", 1)
	})
}

func TestStartTimeout10s(t *testing.T) {
	cases := []struct {
		name   string
		gate   func(h *rtharness.H)
		almost time.Duration // just before the deadline of the step that hangs
		code   int
		reason string // end_reason of session.failed
		sample string // close reason = error code
	}{
		{"config load hangs", func(h *rtharness.H) { h.Gates.Config = make(chan struct{}) }, 9*time.Second + 900*time.Millisecond, 1011, "SERVER_ERROR", "INTERNAL_ERROR"},
		{"stt connect hangs (3 s connect limit)", func(h *rtharness.H) { h.Gates.STT = make(chan struct{}) }, 2*time.Second + 900*time.Millisecond, 4500, "PROVIDER_FAILURE", "PROVIDER_FAILURE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := rtharness.New(t)
			tc.gate(h)
			h.Connect(h.StartCmd())
			h.WaitFor("load blocked", func() bool { return h.Blocked() == 1 })
			h.Sync()
			h.AdvanceClock(tc.almost)
			h.Tick()
			h.Sync()
			select {
			case <-h.Conn.Closed():
				t.Fatal("the start failed too early")
			default:
			}
			h.AdvanceClock(100 * time.Millisecond)
			h.Tick()
			code, reason := h.WaitClosed()
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.sample, reason)
			require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "FailStart"}, h.Calls.Calls())
			failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
			require.Equal(t, tc.reason, failed.EndReason)
			require.Equal(t, 0, failed.ChargedSeconds)
			require.Empty(t, h.Events("session.started"))
			h.WaitFor("runtime gone", func() bool { return h.Mgr.ActiveSessions() == 0 })
		})
	}
}

func TestSessionEndBeforeActive(t *testing.T) {
	h := rtharness.New(t)
	h.Gates.Config = make(chan struct{})
	h.Connect(h.StartCmd())
	h.WaitFor("load blocked", func() bool { return h.Blocked() == 1 })
	h.Sync()
	h.Send(h.EndCmd())
	h.Sync()
	v := h.View()
	require.True(t, v.PendingEnd, "session.end during STARTING is remembered")
	require.Equal(t, session.StatusStarting, v.Status)
	require.NotContains(t, h.Calls.Calls(), "BeginEnding")

	close(h.Gates.Config)
	h.WaitEvents("session.completed", 1)
	code, _ := h.WaitClosed()
	require.Equal(t, 1000, code)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Complete"}, h.Calls.Calls())
	require.Equal(t, []string{"session.ready", "session.started", "session.ending", "session.completed"}, h.Types())
	ending := first(t, h, "session.ending").Payload.(protocol.SessionEnding)
	require.Equal(t, "ENDING", ending.Stage)
	require.Equal(t, "USER_ENDED", ending.Reason)
	require.Equal(t, 2, ending.GraceSeconds)
}

func TestHandshakeFirstCommand(t *testing.T) {
	t.Run("not start or resume", func(t *testing.T) {
		h := rtharness.New(t)
		h.Connect(h.EndCmd())
		code, reason := h.WaitClosed()
		require.Equal(t, 1008, code)
		require.Equal(t, "PROTOCOL_VIOLATION", reason)
		require.Equal(t, "PROTOCOL_VIOLATION", errorOf(t, h).Error.Code)
		require.Equal(t, 1.0, h.Metrics.ProtocolViolationsTotal(conversation.ViolationHeader))
		require.Equal(t, []string{"GetForAttach"}, h.Calls.Calls(), "the session is not touched")
	})
	t.Run("unsupported audio format", func(t *testing.T) {
		h := rtharness.New(t)
		bad := h.Cmd("session.start", protocol.SessionStart{Audio: protocol.AudioParams{Codec: "opus", SampleRate: 16000, Channels: 1, FrameMS: 20}})
		h.Connect(bad)
		code, _ := h.WaitClosed()
		require.Equal(t, 1008, code)
		require.Equal(t, "UNSUPPORTED_AUDIO_FORMAT", errorOf(t, h).Error.Code)
		require.Equal(t, []string{"GetForAttach"}, h.Calls.Calls())
	})
	t.Run("resume on RESERVED", func(t *testing.T) {
		h := rtharness.New(t)
		h.Connect(h.ResumeCmd(0))
		code, _ := h.WaitClosed()
		require.Equal(t, 4009, code)
		e := errorOf(t, h)
		require.Equal(t, "SESSION_STATE_CONFLICT", e.Error.Code)
		require.False(t, e.Retryable)
		require.Equal(t, []string{"GetForAttach"}, h.Calls.Calls(), "the client must start, not resume")
	})
	t.Run("no command in 10 s", func(t *testing.T) {
		h := rtharness.New(t)
		h.Attach()
		h.WaitEvents("session.ready", 1)
		h.AdvanceClock(9*time.Second + 900*time.Millisecond)
		h.Tick()
		time.Sleep(20 * time.Millisecond) // a real pause so a wrongly early close would show
		select {
		case <-h.Conn.Closed():
			t.Fatal("closed before the 10 s of WS_HANDSHAKE_TIMEOUT")
		default:
		}
		h.AdvanceClock(100 * time.Millisecond)
		h.Tick()
		code, reason := h.WaitClosed()
		require.Equal(t, 4008, code)
		require.Equal(t, "HANDSHAKE_TIMEOUT", reason)
		e := errorOf(t, h)
		require.Equal(t, "HANDSHAKE_TIMEOUT", e.Error.Code)
		require.True(t, e.Retryable)
		require.Equal(t, []string{"GetForAttach"}, h.Calls.Calls())
	})
	t.Run("invalid message does not end the handshake", func(t *testing.T) {
		h := rtharness.New(t)
		h.Attach()
		h.WaitEvents("session.ready", 1)
		h.Conn.Send(conversation.MessageText, []byte(`{"type":"nope"`))
		h.WaitEvents("error", 1)
		require.Equal(t, "INVALID_MESSAGE", errorOf(t, h).Error.Code)
		h.Send(h.StartCmd())
		h.WaitEvents("session.started", 1)
	})
}

func TestStartWhileActive(t *testing.T) {
	// §7.3.2: session.start on an ACTIVE session is a resume from 0; session.resume carries its
	// own last_server_seq. The resume flow itself is Task 21; here the Manager's route is checked.
	for _, tc := range []struct {
		name string
		cmd  func(h *rtharness.H) protocol.ClientEvent
		want int64
	}{
		{"start", func(h *rtharness.H) protocol.ClientEvent { return h.StartCmd() }, 0},
		{"resume", func(h *rtharness.H) protocol.ClientEvent { return h.ResumeCmd(41) }, 41},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := rtharness.New(t, rtharness.WithStatus(session.StatusActive))
			got := make(chan int64, 1)
			conversation.SetResumeHook(h.Mgr, func(last int64) { got <- last })
			h.Connect(tc.cmd(h))
			require.Equal(t, tc.want, <-got)
			h.WaitClosed()
			ready := first(t, h, "session.ready").Payload.(protocol.SessionReady)
			require.Equal(t, "session.resume", ready.ExpectedCommand)
			require.Equal(t, "ACTIVE", ready.Status)
			require.NotContains(t, h.Calls.Calls(), "ClaimRuntime", "no new start")
		})
	}
}

func TestAttachRefusals(t *testing.T) {
	t.Run("unknown session", func(t *testing.T) {
		h := rtharness.New(t)
		h.Ticket.SessionID = uuid.New()
		h.Attach()
		code, _ := h.WaitClosed()
		require.Equal(t, 4004, code)
		require.Equal(t, "SESSION_NOT_FOUND", errorOf(t, h).Error.Code)
	})
	t.Run("another user", func(t *testing.T) {
		h := rtharness.New(t)
		h.Ticket.UserID = uuid.New()
		h.Attach()
		code, _ := h.WaitClosed()
		require.Equal(t, 4003, code)
		require.Equal(t, "FORBIDDEN", errorOf(t, h).Error.Code)
	})
	t.Run("STARTING is retryable", func(t *testing.T) {
		h := rtharness.New(t, rtharness.WithStatus(session.StatusStarting))
		h.Attach()
		code, _ := h.WaitClosed()
		require.Equal(t, 4009, code)
		e := errorOf(t, h)
		require.Equal(t, "SESSION_STATE_CONFLICT", e.Error.Code)
		require.True(t, e.Retryable)
	})
	for _, st := range []session.Status{session.StatusEnding, session.StatusCompleted, session.StatusFailed, session.StatusCancelled, session.StatusAbandoned} {
		t.Run(string(st), func(t *testing.T) {
			h := rtharness.New(t, rtharness.WithStatus(st))
			h.Attach()
			code, _ := h.WaitClosed()
			require.Equal(t, 4010, code)
			require.Equal(t, "SESSION_NOT_RESUMABLE", errorOf(t, h).Error.Code)
		})
	}
	t.Run("store down", func(t *testing.T) {
		h := rtharness.New(t)
		h.Session.FailNext("GetForAttach", errors.New("postgres down"))
		h.Attach()
		code, _ := h.WaitClosed()
		require.Equal(t, 4503, code)
		require.Equal(t, "SERVICE_UNAVAILABLE", errorOf(t, h).Error.Code)
	})
}

func TestStartRefusedAtCapacity(t *testing.T) {
	h := rtharness.New(t, rtharness.WithLimits(conversation.Limits{ReadyMax: 0, Max: 0, HardMax: 0}))
	h.Connect(h.StartCmd())
	code, _ := h.WaitClosed()
	require.Equal(t, 4503, code)
	e := errorOf(t, h)
	require.Equal(t, "SERVER_AT_CAPACITY", e.Error.Code)
	require.True(t, e.Retryable)
	require.Equal(t, []string{"GetForAttach"}, h.Calls.Calls(), "the session stays RESERVED: nothing was claimed")
	require.Equal(t, 1.0, h.Metrics.ConnectionsTotal(conversation.ConnRejectedCapacity))
	require.Equal(t, 0, h.Mgr.ActiveSessions())
}

func TestStartErrorsMapToRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		err    error
		code   int
		errc   string
	}{
		{"claim: not found", "ClaimRuntime", session.ErrNotFound, 4004, "SESSION_NOT_FOUND"},
		{"claim: status moved", "ClaimRuntime", session.ErrInvalidTransition, 4009, "SESSION_STATE_CONFLICT"},
		{"claim: db down", "ClaimRuntime", errors.New("down"), 4503, "SERVICE_UNAVAILABLE"},
		{"mark starting: db down", "MarkStarting", errors.New("down"), 4503, "SERVICE_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := rtharness.New(t)
			h.Session.FailNext(tc.method, tc.err)
			h.Connect(h.StartCmd())
			code, _ := h.WaitClosed()
			require.Equal(t, tc.code, code)
			require.Equal(t, tc.errc, errorOf(t, h).Error.Code)
			require.Equal(t, 0, h.Mgr.ActiveSessions(), "the reserved slot is given back")
		})
	}
}

func TestFenceLostWhileStarting(t *testing.T) {
	h := rtharness.New(t)
	h.Session.FailNext("Activate", session.ErrFenceLost)
	h.Connect(h.StartCmd())
	code, reason := h.WaitClosed()
	require.Equal(t, 4011, code, "another owner took the session: SESSION_TAKEN_OVER")
	require.Equal(t, "SESSION_TAKEN_OVER", reason)
	require.Empty(t, h.Events("session.started"))
	require.NotContains(t, h.Calls.Calls(), "FailStart", "a lost fence writes nothing more")
	h.WaitFor("runtime gone", func() bool { return h.Mgr.ActiveSessions() == 0 })
}

func TestProtocolViolationAfterStart(t *testing.T) {
	h := rtharness.New(t)
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	// a command for another session is a violation (§7.4.2): error, audit row, close 1008
	bad := h.Cmd("ping", protocol.Ping{ClientTS: 1})
	bad.SessionID = uuid.New()
	h.Send(bad)
	code, reason := h.WaitClosed()
	require.Equal(t, 1008, code)
	require.Equal(t, "PROTOCOL_VIOLATION", reason)
	require.Equal(t, "PROTOCOL_VIOLATION", errorOf(t, h).Error.Code)
	require.Equal(t, 1.0, h.Metrics.ProtocolViolationsTotal(conversation.ViolationSessionID))
	h.WaitFor("audit row", func() bool { return len(sessionEvents(h, "PROTOCOL_VIOLATION")) == 1 })
	row := sessionEvents(h, "PROTOCOL_VIOLATION")[0]
	require.JSONEq(t, `{"code":"PROTOCOL_VIOLATION","detail":"session_id mismatch"}`, string(row.Payload))
	require.False(t, strings.Contains(string(row.Payload), bad.SessionID.String()), "the audit row carries no frame content")
	// the connection is dropped but the session lives on for a reconnect (Task 21)
	h.WaitFor("detached", func() bool { return !h.View().Connected })
	require.Equal(t, session.StatusActive, h.View().Status)
}

func TestInvalidMessageIsNonFatal(t *testing.T) {
	h := rtharness.New(t)
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	h.Conn.Send(conversation.MessageText, []byte(`not json`))
	h.WaitEvents("error", 1)
	e := errorOf(t, h)
	require.Equal(t, "INVALID_MESSAGE", e.Error.Code)
	require.False(t, e.Fatal)
	select {
	case <-h.Conn.Closed():
		t.Fatal("INVALID_MESSAGE must not close the connection")
	default:
	}
	// a second start is COMMAND_REJECTED
	second := h.StartCmd()
	h.Send(second)
	h.WaitEvents("error", 2)
	require.Equal(t, "COMMAND_REJECTED", h.Events("error")[1].Payload.(protocol.ErrorPayload).Error.Code)
	// ping → pong
	h.Send(h.Cmd("ping", protocol.Ping{ClientTS: 77}))
	pong := h.WaitEvents("pong", 1)[0].Payload.(protocol.Pong)
	require.Equal(t, int64(77), pong.ClientTS)
}

func TestConnectionLossDetaches(t *testing.T) {
	h := rtharness.New(t)
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	h.Conn.EOF()
	h.WaitFor("detached", func() bool { return !h.View().Connected })
	require.Equal(t, session.StatusActive, h.View().Status, "the session outlives the connection")
	require.NoError(t, h.AttachResult())
	h.AdvanceClock(200 * time.Millisecond)
	h.Tick()
	h.WaitFor("rt connected=0", func() bool {
		rt, ok, _ := h.Store.ReadRT(t.Context(), h.SID)
		h.Tick()
		return ok && !rt.Connected && rt.DisconnectedAtMS > 0
	})
}

func TestTimersWarningsBalanceExpiry(t *testing.T) {
	h := rtharness.New(t, rtharness.WithDuration(120))
	h.Connect(h.StartCmd())
	h.WaitEvents("session.started", 1)
	h.Sync()
	// keep the connection alive past the 45 s idle limit: the heartbeat is not under test here
	step := func(d time.Duration) {
		h.AdvanceClock(d)
		h.Tick()
		h.Sync()
	}

	step(10 * time.Second)
	ticks := h.Events("balance.tick")
	require.Len(t, ticks, 1)
	bt := ticks[0].Payload.(protocol.BalanceTick)
	require.Equal(t, 10, bt.ElapsedSeconds)
	require.Equal(t, 110, bt.RemainingSeconds)
	step(10 * time.Second)
	require.Len(t, h.Events("balance.tick"), 2)

	// T-60 s and T-10 s
	step(40 * time.Second) // 60 s elapsed: 60 s remaining
	w := h.Events("session.ending")
	require.Len(t, w, 1)
	require.Equal(t, protocol.SessionEnding{Stage: "WARNING", Reason: "DURATION_ELAPSED", RemainingSeconds: 60}, w[0].Payload)
	step(50 * time.Second) // 110 s elapsed
	w = h.Events("session.ending")
	require.Len(t, w, 2)
	require.Equal(t, 10, w[1].Payload.(protocol.SessionEnding).RemainingSeconds)

	// expiry
	step(10 * time.Second)
	h.WaitEvents("session.completed", 1)
	code, _ := h.WaitClosed()
	require.Equal(t, 1000, code)
	ending := h.Events("session.ending")
	require.Equal(t, protocol.SessionEnding{Stage: "ENDING", Reason: "DURATION_ELAPSED", RemainingSeconds: 0, GraceSeconds: 0}, ending[2].Payload)
	completed := first(t, h, "session.completed").Payload.(protocol.SessionCompleted)
	require.Equal(t, "DURATION_ELAPSED", completed.EndReason)
	require.LessOrEqual(t, completed.ChargedSeconds, 120)
	require.Equal(t, 0, completed.TurnCount)
}

// panicGen is a provider.Generation whose LLM call panics.
type panicGen struct{ provider.Generation }

func (panicGen) StreamLLM(context.Context, provider.LLMRequest) (provider.LLMStream, error) {
	panic("scripted panic in the LLM goroutine")
}

func TestPanicInStartupFailsTheStart(t *testing.T) {
	h := rtharness.New(t)
	h.OnLearner = func() { panic("scripted panic in a start step") }
	h.Connect(h.StartCmd())
	code, reason := h.WaitClosed()
	require.Equal(t, 1011, code)
	require.Equal(t, "INTERNAL_ERROR", reason)
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "SERVER_ERROR", failed.EndReason)
	require.Equal(t, "INTERNAL_ERROR", failed.ErrorCode)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "FailStart"}, h.Calls.Calls())
	h.WaitFor("runtime gone", func() bool { return h.Mgr.ActiveSessions() == 0 })
}

func TestPanicInGenerationEndsTheSessionServerError(t *testing.T) {
	// §7.7.2: a panic in any goroutine is recovered and becomes FATAL{SERVER_ERROR} (§7.11.4):
	// BeginEnding → Fail, session.failed, error{INTERNAL_ERROR}, close 1011.
	h := rtharness.New(t, rtharness.WithStartMode(session.StartAIFirst),
		rtharness.WithGenerationWrapper(func(g provider.Generation) provider.Generation { return panicGen{g} }))
	h.Connect(h.StartCmd())
	code, reason := h.WaitClosed()
	require.Equal(t, 1011, code)
	require.Equal(t, "INTERNAL_ERROR", reason)
	failed := first(t, h, "session.failed").Payload.(protocol.SessionFailed)
	require.Equal(t, "SERVER_ERROR", failed.EndReason)
	var fatal []protocol.ErrorPayload // the pipeline may report its own failure just before the panic reaches the loop
	for _, ev := range h.Events("error") {
		if p := ev.Payload.(protocol.ErrorPayload); p.Fatal {
			fatal = append(fatal, p)
		}
	}
	require.Len(t, fatal, 1)
	require.Equal(t, "INTERNAL_ERROR", fatal[0].Error.Code)
	require.Equal(t, []string{"GetForAttach", "ClaimRuntime", "MarkStarting", "Activate", "BeginEnding", "Fail"}, h.Calls.Calls())
	h.WaitFor("turn 0 failed", func() bool {
		ts := h.Turns.Turns(h.SID)
		return len(ts) == 1 && ts[0].Status != nil
	})
	require.Equal(t, session.TurnFailed, *h.Turns.Turns(h.SID)[0].Status, "the turn in progress is FAILED")
	h.WaitFor("runtime gone", func() bool { return h.Mgr.ActiveSessions() == 0 })
}
