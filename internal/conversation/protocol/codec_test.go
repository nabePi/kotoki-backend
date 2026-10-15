package protocol

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDurableSet(t *testing.T) {
	durable := []string{"session.started", "session.ending", "session.completed", "session.failed", "turn.started", "stt.final",
		"ai.generation_started", "ai.sentence", "ai.audio_started", "ai.audio_completed", "ai.interrupted", "turn.completed"}
	transient := []string{"session.ready", "session.snapshot", "speech.started", "speech.ended", "stt.partial", "ai.text_delta", "balance.tick", "pong"}
	for _, s := range durable {
		require.True(t, IsDurable(s), s)
	}
	for _, s := range transient {
		require.False(t, IsDurable(s), s)
	}
	require.False(t, IsDurable("nope"))
}

func TestEnvelopeOmitsEmpty(t *testing.T) {
	ev := ServerEvent{Type: "pong", EventID: evID(1), ServerSeq: 1, SessionID: sid, TS: tsAt, Payload: Pong{}}
	raw, err := MarshalServer(ev)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	for _, k := range []string{"turn_id", "turn_seq", "generation", "replay"} {
		require.NotContains(t, m, k)
	}
	require.EqualValues(t, 1, m["version"])
	require.Equal(t, "2026-10-12T06:30:15.123Z", m["ts"])

	ev.Replay = true
	ev.Generation = ptr(uint32(0))
	raw, err = MarshalServer(ev)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, true, m["replay"])
	require.EqualValues(t, 0, m["generation"])
}

func clientJSON(typ string, version int, payload string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": typ, "version": version, "event_id": "8a4f0c1e-3c55-4b8e-9d4b-0f1d2e3a4b5c",
		"session_id": sid.String(), "payload": json.RawMessage(payload),
	})
	return b
}

func TestDecodeClient(t *testing.T) {
	_, err := DecodeClientEvent(clientJSON("bogus.type", 1, `{}`))
	require.ErrorIs(t, err, ErrUnknownType)

	_, err = DecodeClientEvent(clientJSON("ping", 2, `{"client_ts":1}`))
	require.ErrorIs(t, err, ErrUnsupportedVersion)

	_, err = DecodeClientEvent([]byte(`{not json`))
	require.ErrorIs(t, err, ErrInvalidPayload)
	_, err = DecodeClientEvent(clientJSON("ping", 1, `[]`))
	require.ErrorIs(t, err, ErrInvalidPayload)
	_, err = DecodeClientEvent(clientJSON("ping", 1, `null`))
	require.ErrorIs(t, err, ErrInvalidPayload)
	_, err = DecodeClientEvent(clientJSON("ping", 1, `{"client_ts":"x"}`))
	require.ErrorIs(t, err, ErrInvalidPayload)

	// field payload tak dikenal diabaikan (§7.4.2).
	ev, err := DecodeClientEvent(clientJSON("ping", 1, `{"client_ts":5,"future_field":true}`))
	require.NoError(t, err)
	require.Equal(t, Ping{ClientTS: 5}, ev.Payload)

	// audio tidak valid tetap ter-decode; pemanggil yang menolak.
	ev, err = DecodeClientEvent(clientJSON("session.start", 1,
		`{"audio":{"codec":"opus","sample_rate":16000,"channels":1,"frame_ms":20}}`))
	require.NoError(t, err)
	p := ev.Payload.(SessionStart)
	require.False(t, p.Audio.Valid())
	require.True(t, audio.Valid())
	require.Nil(t, p.Client)
}
