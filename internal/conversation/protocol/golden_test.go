package protocol

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var update = flag.Bool("update", false, "regenerate testdata/api/ws/{client,server}/*.json")

var (
	sid  = uuid.MustParse("01929a40-1111-7abc-8def-000000000001")
	tid  = uuid.MustParse("01929a4e-5a00-7d00-8000-0000000000aa")
	tsAt = time.Date(2026, 10, 12, 6, 30, 15, 123_000_000, time.UTC)
)

func tm(s string) Timestamp {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return Timestamp{t}
}

func ptr[T any](v T) *T { return &v }

func evID(n int) uuid.UUID {
	return uuid.MustParse("01929a4e-6f2c-7c3a-9a51-3f1f0b7c2d" + string(rune('0'+n/10)) + string(rune('0'+n%10)))
}

var audio = AudioParams{Codec: "opus", SampleRate: 48000, Channels: 1, FrameMS: 20}

func srv(typ string, seq int64, turn, gen bool, payload any) ServerEvent {
	ev := ServerEvent{Type: typ, Version: Version, EventID: evID(int(seq) % 100), ServerSeq: seq, SessionID: sid, TS: tsAt, Payload: payload}
	if turn {
		ev.TurnID = ptr(tid)
		ev.TurnSeq = ptr(3)
	}
	if gen {
		ev.Generation = ptr(uint32(7))
	}
	return ev
}

func serverFixtures() map[string]ServerEvent {
	started := "2026-10-12T06:30:01.200Z"
	expires := "2026-10-12T06:40:01.200Z"
	fx := []ServerEvent{
		srv("session.ready", 1, false, false, SessionReady{
			Status: "RESERVED", ExpectedCommand: "session.start", ConversationStartMode: "AI_FIRST", ProtocolVersion: 1,
			Audio: ReadyAudio{
				Upstream:   AudioStream{AudioParams: audio, FrameHeader: "0x01|u32 seq"},
				Downstream: AudioStream{AudioParams: audio, FrameHeader: "0x02|u32 generation|u32 seq"},
			},
			Limits:             Limits{MaxJSONBytes: 16384, MaxBinaryBytes: 4096, MaxBinaryPerSecond: 100},
			HeartbeatIntervalS: 15, ServerTime: tm("2026-10-12T06:30:00.000Z"),
		}),
		srv("session.started", 2, false, true, SessionStarted{
			Status: "ACTIVE", ConversationStartMode: "AI_FIRST", StartedAt: tm(started), ExpiresAt: tm(expires),
			DurationSeconds: 600, RemainingSeconds: 600, TurnState: "LISTENING",
		}),
		srv("session.snapshot", 155, false, true, SessionSnapshot{
			Replay: "COMPLETE", ReplayedCount: 12, ServerSeqBaseline: 154,
			Session:   SnapshotSession{Status: "ACTIVE", ConversationStartMode: "USER_FIRST", StartedAt: tm(started), ExpiresAt: tm(expires), RemainingSeconds: 312},
			TurnState: "LISTENING", ActiveGeneration: 9, NextTurnSeq: 6,
			RecentTurns: []RecentTurn{{
				TurnID: tid, TurnSeq: 5, Status: "INTERRUPTED",
				UserText: ptr("昨日、会社に行きました。"), AssistantSpokenText: ptr("へえ、そうなんですね。"),
			}},
		}),
		srv("session.ending", 3, false, false, SessionEnding{Stage: "WARNING", Reason: "DURATION_ELAPSED", RemainingSeconds: 60, GraceSeconds: 0}),
		srv("session.completed", 4, false, false, SessionCompleted{
			EndReason: "DURATION_ELAPSED", StartedAt: tm(started), EndedAt: tm(expires), ChargedSeconds: 598, DisconnectedSeconds: 4, TurnCount: 14,
		}),
		srv("session.failed", 5, false, false, SessionFailed{EndReason: "PROVIDER_FAILURE", ErrorCode: "PROVIDER_FAILURE", ChargedSeconds: 0}),
		srv("speech.started", 6, true, false, SpeechStarted{AudioOffsetMS: 182340, DuringAISpeech: false}),
		srv("speech.ended", 7, true, false, SpeechEnded{AudioOffsetMS: 185120, SpeechMS: 2780}),
		srv("turn.started", 8, true, false, TurnStarted{Initiator: "USER", StartedAt: tm("2026-10-12T06:33:02.410Z")}),
		srv("stt.partial", 9, true, false, STTPartial{Text: "昨日会社に"}),
		srv("stt.final", 10, true, false, STTFinal{Text: "昨日会社に行きました。", EndReason: "SILENCE", STTFinalMS: 240, Provider: "ELEVENLABS", UsedFallback: false}),
		srv("ai.generation_started", 11, true, true, AIGenerationStarted{Kind: "REPLY"}),
		srv("ai.text_delta", 12, true, true, AITextDelta{Text: "へえ、そうなんですね。"}),
		srv("ai.sentence", 142, true, true, AISentence{SentenceIndex: 0, Text: "へえ、そうなんですね。", FirstFrameSeq: 0, StartMS: 0}),
		srv("ai.audio_started", 13, true, true, AIAudioStarted{AudioParams: audio}),
		srv("ai.audio_completed", 14, true, true, AIAudioCompleted{FrameCount: 192, TotalAudioMS: 3840, SentenceCount: 2, LLMFinished: true}),
		srv("ai.interrupted", 15, true, true, AIInterrupted{Reason: "BARGE_IN", ActiveGeneration: 8, SpokenSentenceCount: 1}),
		srv("turn.completed", 16, true, false, TurnCompleted{
			Status: "COMPLETED", UserText: ptr("昨日会社に行きました。"),
			AssistantText:       ptr("へえ、そうなんですね。何か特別なことがありましたか？"),
			AssistantSpokenText: ptr("へえ、そうなんですね。何か特別なことがありましたか？"),
			Latency:             &Latency{TurnEndToFirstAudioMS: 1180, STTFinalMS: 240, LLMFirstTokenMS: 410, TTSFirstAudioMS: 290},
		}),
		srv("balance.tick", 17, false, false, BalanceTick{RemainingSeconds: 412, ElapsedSeconds: 188, ExpiresAt: tm(expires)}),
		srv("pong", 18, false, false, Pong{ClientTS: 1760250615123, ServerTS: 1760250615131}),
		srv("error", 19, false, false, ErrorPayload{
			Error: ErrorBody{Code: "AUDIO_FRAME_INVALID", Message: "Audio dari mikrofon tidak terbaca dengan baik.", RequestID: "0192f1a0-2222-7000-8000-000000000009"},
		}),
	}
	m := map[string]ServerEvent{}
	for _, e := range fx {
		m[e.Type] = e
	}
	return m
}

func clientFixtures() map[string]ClientEvent {
	cid := uuid.MustParse("8a4f0c1e-3c55-4b8e-9d4b-0f1d2e3a4b5c")
	ts := time.Date(2026, 10, 12, 6, 30, 16, 1_000_000, time.UTC)
	mk := func(typ string, p any) ClientEvent {
		return ClientEvent{Type: typ, Version: Version, EventID: cid, SessionID: sid, TS: ts, Payload: p}
	}
	fx := []ClientEvent{
		mk("session.start", SessionStart{Client: &ClientInfo{AppVersion: "web-1.4.0", Platform: "web"}, Audio: audio}),
		mk("session.resume", SessionResume{LastServerSeq: 141, Audio: audio}),
		mk("session.end", SessionEnd{Reason: "USER_ENDED"}),
		mk("client.barge_in", ClientBargeIn{Generation: 7, VadMS: ptr(180)}),
		mk("client.playback_finished", ClientPlaybackFinished{Generation: 7, PlayedMS: 3820}),
		mk("ping", Ping{ClientTS: 1760250615123}),
	}
	m := map[string]ClientEvent{}
	for _, e := range fx {
		m[e.Type] = e
	}
	return m
}

func goldenDir(t *testing.T, side string) string {
	t.Helper()
	return filepath.Join(testutil.ModuleRoot(t), "testdata", "api", "ws", side)
}

func indent(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, raw, "", "  "))
	buf.WriteByte('\n')
	return buf.Bytes()
}

// goldenCheck membandingkan body dengan golden; dengan -update menulis ulang.
func goldenCheck(t *testing.T, dir, typ string, body []byte) []byte {
	t.Helper()
	path := filepath.Join(dir, typ+".json")
	if *update {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(path, indent(t, body), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run: go test ./internal/conversation/protocol -run Golden -update")
	require.JSONEq(t, string(want), string(body))
	return want
}

func goldenFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

func TestGoldenServer(t *testing.T) {
	fx := serverFixtures()
	_, types := Registered()
	require.Len(t, types, len(fx), "fixture per tipe terdaftar")
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			ev, ok := fx[typ]
			require.True(t, ok, "fixture hilang untuk %s", typ)
			body, err := MarshalServer(ev)
			require.NoError(t, err)
			want := goldenCheck(t, goldenDir(t, "server"), typ, body)

			dec, err := DecodeServerEvent(want)
			require.NoError(t, err)
			again, err := MarshalServer(dec)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(again))
			require.Equal(t, ev.Payload, dec.Payload)
		})
	}
	require.Equal(t, types, goldenFiles(t, goldenDir(t, "server")), "kelengkapan: golden <-> registry")
}

func TestGoldenClient(t *testing.T) {
	fx := clientFixtures()
	types, _ := Registered()
	require.Len(t, types, len(fx))
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			ev, ok := fx[typ]
			require.True(t, ok, "fixture hilang untuk %s", typ)
			body, err := MarshalClient(ev)
			require.NoError(t, err)
			want := goldenCheck(t, goldenDir(t, "client"), typ, body)

			dec, err := DecodeClientEvent(want)
			require.NoError(t, err)
			again, err := MarshalClient(dec)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(again))
			require.Equal(t, ev.Payload, dec.Payload)
		})
	}
	require.Equal(t, types, goldenFiles(t, goldenDir(t, "client")), "kelengkapan: golden <-> registry")
}
