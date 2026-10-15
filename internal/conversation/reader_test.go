package conversation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

var testSID = uuid.MustParse("01929a40-1111-7abc-8def-000000000001")

var eventSeq uint32

// eid returns a fresh deterministic event id.
func eid() uuid.UUID {
	eventSeq++
	var u uuid.UUID
	u[0], u[6], u[8] = 0x0c, 0x40, 0x80
	u[12], u[13], u[14], u[15] = byte(eventSeq>>24), byte(eventSeq>>16), byte(eventSeq>>8), byte(eventSeq)
	return u
}

func pingMsg(t *testing.T, id uuid.UUID) []byte {
	t.Helper()
	b, err := protocol.MarshalClient(protocol.ClientEvent{Type: "ping", EventID: id, SessionID: testSID, Payload: protocol.Ping{ClientTS: 1}})
	require.NoError(t, err)
	return b
}

func upstream(seq uint32) []byte { return protocol.EncodeUpstream(seq, []byte{0xF8, 0xFF, 0xFE}) }

type readerRig struct {
	conn    *pipeConn
	clk     *clock.Fake
	m       *Metrics
	r       *Reader
	act     *Activity
	cmdCh   chan protocol.ClientEvent
	audioIn chan AudioIn
	notices chan ReaderNotice
}

func newReaderRig(t *testing.T) *readerRig {
	t.Helper()
	noLeak(t)
	clk := clock.NewFake(t0)
	rig := &readerRig{conn: newPipeConn(), clk: clk, m: newTestMetrics(), act: NewActivity(t0.Add(-time.Hour)),
		cmdCh: make(chan protocol.ClientEvent, 256), audioIn: make(chan AudioIn, 50), notices: make(chan ReaderNotice, 64)}
	rig.r = NewReader(ReaderConfig{SessionID: testSID, Clock: clk, Activity: rig.act, Metrics: rig.m})
	return rig
}

// run reads until the reader returns; the conn reports EOF after the queued messages.
func (g *readerRig) run(t *testing.T) error {
	t.Helper()
	g.conn.eof()
	done := make(chan error, 1)
	go func() { done <- g.r.Run(context.Background(), g.conn, g.cmdCh, g.audioIn, g.notices) }()
	return waitErr(t, done, "reader")
}

func requireViolation(t *testing.T, g *readerRig, err error, code errs.Code, kind string) {
	t.Helper()
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, code, pe.Code)
	require.Equal(t, kind, pe.Kind)
	require.Equal(t, 1008, pe.CloseCode())
	require.Equal(t, 1.0, promtest.ToFloat64(g.m.protocolViolations.WithLabelValues(kind)))
}

func TestReaderLimits(t *testing.T) {
	wrongSession, err := protocol.MarshalClient(protocol.ClientEvent{Type: "ping", EventID: uuid.MustParse("0c2f0000-0000-4000-8000-000000000001"),
		SessionID: uuid.MustParse("01929a40-1111-7abc-8def-000000000002"), Payload: protocol.Ping{ClientTS: 1}})
	require.NoError(t, err)
	version2, err := protocol.MarshalClient(protocol.ClientEvent{Type: "ping", Version: 2, EventID: uuid.MustParse("0c2f0000-0000-4000-8000-000000000002"),
		SessionID: testSID, Payload: protocol.Ping{ClientTS: 1}})
	require.NoError(t, err)
	header02 := protocol.EncodeUpstream(1, []byte{1, 2, 3})
	header02[0] = 0x02
	bigJSON := append([]byte(`{"type":"ping","pad":"`), bytes.Repeat([]byte("x"), 16384)...)
	bigJSON = append(bigJSON, `"}`...)

	type msg struct {
		kind MessageKind
		data []byte
	}
	cases := []struct {
		name string
		msgs func(t *testing.T) []msg
		code errs.Code
		kind string // "" = no violation
	}{
		{"binary 4097 bytes", func(*testing.T) []msg { return []msg{{MessageBinary, make4097()}} }, errs.CodeProtocolViolation, "frame_size"},
		{"binary 4096 bytes ok", func(*testing.T) []msg { return []msg{{MessageBinary, protocol.EncodeUpstream(0, make([]byte, 4091))}} }, "", ""},
		{"binary shorter than 6 bytes", func(*testing.T) []msg { return []msg{{MessageBinary, []byte{1, 0, 0, 0, 0}}} }, errs.CodeProtocolViolation, "frame_size"},
		{"header 0x02", func(*testing.T) []msg { return []msg{{MessageBinary, header02}} }, errs.CodeProtocolViolation, "header"},
		{"json over 16384 bytes", func(*testing.T) []msg { return []msg{{MessageText, bigJSON}} }, errs.CodeProtocolViolation, "frame_size"},
		{"version 2", func(*testing.T) []msg { return []msg{{MessageText, version2}} }, errs.CodeUnsupportedProtocolVersion, "version"},
		{"wrong session_id", func(*testing.T) []msg { return []msg{{MessageText, wrongSession}} }, errs.CodeProtocolViolation, "session_id"},
		{"burst 200 binary ok", func(*testing.T) []msg {
			var ms []msg
			for i := range uint32(200) {
				ms = append(ms, msg{MessageBinary, upstream(i)})
			}
			return ms
		}, "", ""},
		{"burst 201 binary in 0 s", func(*testing.T) []msg {
			var ms []msg
			for i := range uint32(201) {
				ms = append(ms, msg{MessageBinary, upstream(i)})
			}
			return ms
		}, errs.CodeProtocolViolation, "rate"},
		{"burst 40 json ok", func(t *testing.T) []msg {
			var ms []msg
			for range 40 {
				ms = append(ms, msg{MessageText, pingMsg(t, eid())})
			}
			return ms
		}, "", ""},
		{"41st json in 0 s", func(t *testing.T) []msg {
			var ms []msg
			for range 41 {
				ms = append(ms, msg{MessageText, pingMsg(t, eid())})
			}
			return ms
		}, errs.CodeProtocolViolation, "rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newReaderRig(t)
			// Nothing reads audioIn here: past 50 frames it overflows by design (dropped, not blocking).
			for _, m := range tc.msgs(t) {
				g.conn.send(m.kind, m.data)
			}
			err := g.run(t)
			if tc.kind == "" {
				require.ErrorIs(t, err, io.EOF, "no violation: the reader ends on the conn's EOF")
				var pe *ProtocolError
				require.False(t, errors.As(err, &pe))
				return
			}
			requireViolation(t, g, err, tc.code, tc.kind)
		})
	}
}

func make4097() []byte {
	b := make([]byte, 4097)
	b[0] = 0x01
	return b
}

func TestReaderBinaryRateRefills(t *testing.T) {
	g := newReaderRig(t)
	done := make(chan error, 1)
	go func() { done <- g.r.Run(context.Background(), g.conn, g.cmdCh, g.audioIn, g.notices) }()
	for i := range uint32(200) {
		g.conn.send(MessageBinary, upstream(i))
	}
	// Once the conn is empty at least 199 tokens are spent, so the refill below is never capped
	// and the total budget is exactly 200 + 100 whichever frame takes the last old token.
	require.Eventually(t, func() bool { return len(g.conn.in) == 0 }, 2*time.Second, time.Millisecond)
	g.clk.Advance(time.Second) // +100 tokens
	for i := range uint32(100) {
		g.conn.send(MessageBinary, upstream(200+i))
	}
	g.conn.send(MessageBinary, upstream(300))
	err := waitErr(t, done, "reader")
	requireViolation(t, g, err, errs.CodeProtocolViolation, "rate")
}

func TestReaderForwardsCommandsAndAudio(t *testing.T) {
	g := newReaderRig(t)
	id := eid()
	g.conn.send(MessageText, pingMsg(t, id))
	g.conn.send(MessageBinary, upstream(5))
	require.ErrorIs(t, g.run(t), io.EOF)

	ev := <-g.cmdCh
	require.Equal(t, "ping", ev.Type)
	require.Equal(t, id, ev.EventID)
	a := <-g.audioIn
	require.Equal(t, uint32(5), a.Seq)
	require.Equal(t, []byte{0xF8, 0xFF, 0xFE}, a.Opus)
	require.Equal(t, t0, a.At)
	require.Equal(t, t0, g.act.Last(), "every incoming frame is connection activity")
}

func TestReaderAudioOverflowDropsNewest(t *testing.T) {
	g := newReaderRig(t)
	for i := range uint32(52) {
		g.conn.send(MessageBinary, upstream(i))
	}
	require.ErrorIs(t, g.run(t), io.EOF)
	require.Len(t, g.audioIn, 50)
	require.Equal(t, uint32(0), (<-g.audioIn).Seq, "the queued frames are kept")
	require.Equal(t, 2.0, promtest.ToFloat64(g.m.audioFramesIn.WithLabelValues("overflow")))
}

func TestInvalidMessageWindow(t *testing.T) {
	unknown := func(t *testing.T) []byte {
		t.Helper()
		b, err := protocol.MarshalClient(protocol.ClientEvent{Type: "nope", EventID: eid(), SessionID: testSID, Payload: struct{}{}})
		require.NoError(t, err)
		return b
	}
	t.Run("11 in 60 s → violation", func(t *testing.T) {
		g := newReaderRig(t)
		for range 11 {
			g.conn.send(MessageText, unknown(t))
		}
		err := g.run(t)
		requireViolation(t, g, err, errs.CodeProtocolViolation, "invalid_burst")
		require.Len(t, g.notices, 10, "each of the first 10 is a non-fatal INVALID_MESSAGE")
		n := <-g.notices
		require.Equal(t, errs.CodeInvalidMessage, n.Code)
		require.NotNil(t, n.RefEventID, "the event_id is recovered when the envelope parses")
	})
	t.Run("10 in 60 s → none", func(t *testing.T) {
		g := newReaderRig(t)
		for range 10 {
			g.conn.send(MessageText, unknown(t))
		}
		require.ErrorIs(t, g.run(t), io.EOF)
		require.Len(t, g.notices, 10)
	})
	t.Run("broken JSON counts too", func(t *testing.T) {
		g := newReaderRig(t)
		for range 11 {
			g.conn.send(MessageText, []byte(`{"type":`))
		}
		err := g.run(t)
		requireViolation(t, g, err, errs.CodeProtocolViolation, "invalid_burst")
		n := <-g.notices
		require.Nil(t, n.RefEventID)
	})
}

func TestBurstWindowSlides(t *testing.T) {
	clk := clock.NewFake(t0)
	w := newBurstWindow(clk, invalidMessageLimit, invalidMessageWindow)
	for range 10 {
		require.False(t, w.Add())
	}
	clk.Advance(61 * time.Second)
	require.False(t, w.Add(), "older entries left the 60 s window")

	a := newBurstWindow(clk, audioFrameInvalidLimit, audioFrameInvalidWindow)
	for range 50 {
		require.False(t, a.Add())
	}
	require.True(t, a.Add(), ">50 AUDIO_FRAME_INVALID in 10 s")
}

func TestDuplicateEventIDIgnored(t *testing.T) {
	g := newReaderRig(t)
	done := make(chan error, 1)
	go func() { done <- g.r.Run(context.Background(), g.conn, g.cmdCh, g.audioIn, g.notices) }()

	recv := func() uuid.UUID {
		t.Helper()
		select {
		case ev := <-g.cmdCh:
			return ev.EventID
		case err := <-done:
			t.Fatalf("reader stopped: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("no command forwarded")
		}
		return uuid.Nil
	}
	first := eid()
	g.conn.send(MessageText, pingMsg(t, first))
	g.conn.send(MessageText, pingMsg(t, first))
	require.Equal(t, first, recv())
	// 64 other ids push `first` out of the LRU; then it is accepted again. The clock moves 1 s
	// per message so the JSON bucket (20/s, burst 40) never runs dry.
	for range 64 {
		g.clk.Advance(time.Second)
		id := eid()
		g.conn.send(MessageText, pingMsg(t, id))
		require.Equal(t, id, recv(), "the duplicate of first was skipped, not forwarded")
	}
	g.clk.Advance(time.Second)
	g.conn.send(MessageText, pingMsg(t, first))
	require.Equal(t, first, recv())
	g.conn.eof()
	require.ErrorIs(t, waitErr(t, done, "reader"), io.EOF)
	require.Empty(t, g.cmdCh, "the duplicate was dropped silently")
	require.Empty(t, g.notices)
}

func TestReaderStopsOnCtx(t *testing.T) {
	g := newReaderRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.r.Run(ctx, g.conn, g.cmdCh, g.audioIn, g.notices) }()
	cancel()
	require.NoError(t, waitErr(t, done, "reader"))
}

func TestTokenBucket(t *testing.T) {
	clk := clock.NewFake(t0)
	b := newBucket(clk, 20, 40)
	for range 40 {
		require.True(t, b.Take())
	}
	require.False(t, b.Take())
	clk.Advance(50 * time.Millisecond) // +1 token
	require.True(t, b.Take())
	require.False(t, b.Take())
	clk.Advance(time.Hour)
	for range 40 {
		require.True(t, b.Take())
	}
	require.False(t, b.Take(), "refill is capped at burst")
}
