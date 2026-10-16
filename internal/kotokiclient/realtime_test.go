package kotokiclient

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

var testSession = uuid.MustParse("01929a40-1111-7abc-8def-000000000001")

// wsServer serves one WebSocket per request through fn and returns the ws:// URL.
func wsServer(t *testing.T, fn func(ctx context.Context, c *websocket.Conn, r *http.Request)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		fn(r.Context(), c, r)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/realtime"
}

func serverEvent(typ string, seq int64, gen *uint32, payload any) []byte {
	b, err := protocol.MarshalServer(protocol.ServerEvent{
		Type: typ, EventID: uuid.New(), ServerSeq: seq, SessionID: testSession, Generation: gen,
		TS: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), Payload: payload,
	})
	if err != nil {
		panic(err)
	}
	return b
}

func ptr[T any](v T) *T { return &v }

func writeText(ctx context.Context, t *testing.T, c *websocket.Conn, b []byte) {
	t.Helper()
	require.NoError(t, c.Write(ctx, websocket.MessageText, b))
}

func writeFrame(ctx context.Context, t *testing.T, c *websocket.Conn, gen, seq uint32, payload byte) {
	t.Helper()
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, protocol.EncodeDownstream(gen, seq, []byte{payload})))
}

func TestDialHandshake(t *testing.T) {
	type seen struct{ ticket, origin, sub string }
	ch := make(chan seen, 1)
	url := wsServer(t, func(_ context.Context, c *websocket.Conn, r *http.Request) {
		ch <- seen{r.URL.Query().Get("ticket"), r.Header.Get("Origin"), c.Subprotocol()}
		_ = c.Close(websocket.StatusNormalClosure, "bye")
	})
	rc, err := Dial(context.Background(), url, "tkt-1", "http://localhost:3000")
	require.NoError(t, err)
	got := <-ch
	require.Equal(t, seen{"tkt-1", "http://localhost:3000", Subprotocol}, got)
	<-rc.Done()
	var ce websocket.CloseError
	require.ErrorAs(t, rc.Err(), &ce)
	require.Equal(t, websocket.StatusNormalClosure, ce.Code)
	_, open := <-rc.Events()
	require.False(t, open, "Events closes with the connection")
	_, open = <-rc.Audio()
	require.False(t, open, "Audio closes with the connection")
}

func TestDialRefusedBeforeUpgradeKeepsTheTicketOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 503, map[string]any{"error": map[string]any{"code": "SERVER_DRAINING", "message": "draining", "request_id": "r", "details": nil}})
	}))
	defer srv.Close()
	_, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), "secret-ticket", "")
	var de *DialError
	require.ErrorAs(t, err, &de)
	require.Equal(t, 503, de.Status)
	require.Equal(t, "SERVER_DRAINING", de.API.Code)
	require.NotContains(t, err.Error(), "secret-ticket")

	// A refused TCP connection must not leak the ticket through the URL in the error either.
	_, err = Dial(context.Background(), "ws://127.0.0.1:1/x", "secret-ticket", "")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-ticket")
}

func TestSendAudioHeader(t *testing.T) {
	frames := make(chan []byte, 300)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		for {
			typ, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			require.Equal(t, websocket.MessageBinary, typ)
			frames <- b
		}
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	defer func() { _ = rc.Close(websocket.StatusNormalClosure, "") }()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, rc.SendAudio(ctx, []byte{0xAA, byte(i)}))
	}
	for i := 0; i < 3; i++ {
		b := <-frames
		require.Equal(t, []byte{0x01, 0, 0, 0, byte(i), 0xAA, byte(i)}, b, "frame %d", i)
	}
	// Big-endian for real: sequence 256 sets the second-lowest byte, not the lowest.
	for i := 3; i <= 256; i++ {
		require.NoError(t, rc.SendAudio(ctx, []byte{0xBB}))
	}
	var last []byte
	for i := 3; i <= 256; i++ {
		last = <-frames
		require.Equal(t, uint32(i), binary.BigEndian.Uint32(last[1:5]), "seq of frame %d", i)
	}
	require.Equal(t, []byte{0x01, 0x00, 0x00, 0x01, 0x00, 0xBB}, last)
}

func TestSendAudioPacketBounds(t *testing.T) {
	frames := make(chan []byte, 4)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			frames <- b
		}
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	defer func() { _ = rc.Close(websocket.StatusNormalClosure, "") }()
	ctx := context.Background()

	require.ErrorIs(t, rc.SendAudio(ctx, nil), ErrBadPacket)
	require.ErrorIs(t, rc.SendAudio(ctx, make([]byte, 4092)), ErrBadPacket)
	require.NoError(t, rc.SendAudio(ctx, make([]byte, 4091)))
	b := <-frames
	require.Len(t, b, 4096, "the largest legal frame is exactly MaxBinary")
	// Rejected packets did not consume a sequence number.
	require.NoError(t, rc.SendAudio(ctx, []byte{1}))
	b = <-frames
	require.Equal(t, uint32(1), binary.BigEndian.Uint32(b[1:5]))
}

func TestSendFillsEnvelope(t *testing.T) {
	got := make(chan []byte, 1)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		_, b, err := c.Read(ctx)
		if err == nil {
			got <- b
		}
	})
	clk := clock.NewFake(time.Date(2026, 10, 16, 9, 30, 0, 123_000_000, time.UTC))
	ids := idgen.NewSequence(t)
	rc, err := Dial(context.Background(), url, "t", "", WithDialClock(clk), WithDialIDs(ids))
	require.NoError(t, err)
	defer func() { _ = rc.Close(websocket.StatusNormalClosure, "") }()

	err = rc.Send(context.Background(), protocol.ClientEvent{
		Type: "session.end", SessionID: testSession, Payload: protocol.SessionEnd{Reason: "USER_ENDED"},
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(<-got, &m))
	require.Equal(t, "session.end", m["type"])
	require.EqualValues(t, 1, m["version"])
	require.Equal(t, testSession.String(), m["session_id"])
	require.Equal(t, "2026-10-16T09:30:00.123Z", m["ts"])
	_, err = uuid.Parse(m["event_id"].(string))
	require.NoError(t, err)
	require.Equal(t, map[string]any{"reason": "USER_ENDED"}, m["payload"])
	// The event round-trips through the server decoder (a real server would accept it).
	b, _ := json.Marshal(m)
	ev, err := protocol.DecodeClientEvent(b)
	require.NoError(t, err)
	require.Equal(t, protocol.SessionEnd{Reason: "USER_ENDED"}, ev.Payload)
}

func TestDropsStaleGeneration(t *testing.T) {
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		// Frames before any generation event belong to generation 0 and pass.
		writeFrame(ctx, t, c, 0, 0, 0xA0)
		writeText(ctx, t, c, serverEvent("ai.generation_started", 1, ptr(uint32(3)), protocol.AIGenerationStarted{Kind: "REPLY"}))
		writeFrame(ctx, t, c, 2, 0, 0xB2) // stale: older than the generation just started
		writeFrame(ctx, t, c, 3, 0, 0xB3)
		writeFrame(ctx, t, c, 3, 1, 0xB4)
		writeText(ctx, t, c, serverEvent("ai.interrupted", 2, nil, protocol.AIInterrupted{Reason: "BARGE_IN", ActiveGeneration: 5}))
		writeFrame(ctx, t, c, 4, 0, 0xC4) // stale: the cancelled generation was 4
		writeFrame(ctx, t, c, 3, 2, 0xC3) // stale
		writeFrame(ctx, t, c, 5, 0, 0xC5)
		writeText(ctx, t, c, serverEvent("session.snapshot", 3, ptr(uint32(7)), protocol.SessionSnapshot{ActiveGeneration: 7, Replay: "COMPLETE"}))
		writeFrame(ctx, t, c, 6, 0, 0xD6) // stale: the snapshot says 7
		writeFrame(ctx, t, c, 7, 0, 0xD7)
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)

	var payloads []byte
	var gens []uint32
	for f := range rc.Audio() {
		gens = append(gens, f.Generation)
		payloads = append(payloads, f.Payload...)
	}
	require.Equal(t, []uint32{0, 3, 3, 5, 7}, gens)
	require.Equal(t, []byte{0xA0, 0xB3, 0xB4, 0xC5, 0xD7}, payloads)
	require.EqualValues(t, 4, rc.StaleDropped())
	require.EqualValues(t, 7, rc.ActiveGeneration())

	var types []string
	for ev := range rc.Events() {
		types = append(types, ev.Value.Type)
	}
	require.Equal(t, []string{"ai.generation_started", "ai.interrupted", "session.snapshot"}, types)
}

func TestFrameOrderAgainstEventsIsTheServers(t *testing.T) {
	// A frame sent right after ai.interrupted must be judged by the new generation even though
	// the consumer reads Audio long after the event: the decision is made in stream order.
	release := make(chan struct{})
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		writeText(ctx, t, c, serverEvent("ai.generation_started", 1, ptr(uint32(1)), protocol.AIGenerationStarted{Kind: "REPLY"}))
		writeFrame(ctx, t, c, 1, 0, 0x11)
		writeText(ctx, t, c, serverEvent("ai.interrupted", 2, nil, protocol.AIInterrupted{ActiveGeneration: 2}))
		writeFrame(ctx, t, c, 1, 1, 0x12) // late frame of the old generation
		writeFrame(ctx, t, c, 2, 0, 0x21)
		<-release
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	// Wait until the reader has consumed everything (the generation 2 frame is queued).
	require.Eventually(t, func() bool { return len(rc.Audio()) == 2 }, 5*time.Second, time.Millisecond)
	close(release)
	var got []byte
	for f := range rc.Audio() {
		got = append(got, f.Payload...)
	}
	require.Equal(t, []byte{0x11, 0x21}, got)
}

func TestReceivedTimestampsFollowTheClock(t *testing.T) {
	clk := clock.NewFake(t0)
	step := make(chan struct{})
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		writeText(ctx, t, c, serverEvent("pong", 1, nil, protocol.Pong{ClientTS: 1, ServerTS: 2}))
		<-step
		writeFrame(ctx, t, c, 0, 0, 0x01)
		<-step
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	rc, err := Dial(context.Background(), url, "t", "", WithDialClock(clk))
	require.NoError(t, err)
	ev := <-rc.Events()
	require.True(t, ev.At.Equal(t0))
	require.Equal(t, "pong", ev.Value.Type)
	clk.Advance(750 * time.Millisecond)
	step <- struct{}{}
	f := <-rc.Audio()
	require.True(t, f.At.Equal(t0.Add(750*time.Millisecond)))
	close(step)
}

func TestUnknownEventsAreIgnored(t *testing.T) {
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		writeText(ctx, t, c, []byte(`{"type":"future.thing","version":1,"event_id":"`+uuid.NewString()+`","server_seq":1,"session_id":"`+testSession.String()+`","ts":"2026-10-16T09:00:00.000Z","payload":{}}`))
		writeText(ctx, t, c, []byte(`not json`))
		require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte{0x09, 1, 2})) // not a downstream frame
		writeText(ctx, t, c, serverEvent("pong", 2, nil, protocol.Pong{}))
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	var types []string
	for ev := range rc.Events() {
		types = append(types, ev.Value.Type)
	}
	require.Equal(t, []string{"pong"}, types)
	require.Empty(t, rc.Audio())
}

func TestCloseSendsCodeAndStopsTheReader(t *testing.T) {
	closed := make(chan websocket.CloseError, 1)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		_, _, err := c.Read(ctx)
		var ce websocket.CloseError
		if ok := errors.As(err, &ce); ok {
			closed <- ce
		}
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	require.NoError(t, rc.Close(4000, "done"))
	ce := <-closed
	require.Equal(t, websocket.StatusCode(4000), ce.Code)
	require.Equal(t, "done", ce.Reason)
	select {
	case <-rc.Done():
	default:
		t.Fatal("reader still running after Close")
	}
	require.NoError(t, rc.Close(websocket.StatusNormalClosure, ""), "Close is idempotent")
}

func TestCloseUnblocksAReaderStuckOnAFullBuffer(t *testing.T) {
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		for i := 0; i < audioBuffer+50; i++ {
			if c.Write(ctx, websocket.MessageBinary, protocol.EncodeDownstream(0, uint32(i), []byte{1})) != nil {
				return
			}
		}
		_, _, _ = c.Read(ctx) // answers the close handshake like a real server
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(rc.Audio()) == audioBuffer }, 5*time.Second, time.Millisecond)
	done := make(chan struct{})
	go func() { _ = rc.Close(websocket.StatusNormalClosure, ""); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second): // well under the library's 5 s close-handshake timeout
		t.Fatal("Close hangs while the reader is blocked on a full Audio buffer")
	}
}

func TestAbortDropsTheTransportWithoutCloseHandshake(t *testing.T) {
	readErr := make(chan error, 1)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn, _ *http.Request) {
		_, _, err := c.Read(ctx)
		readErr <- err
	})
	rc, err := Dial(context.Background(), url, "t", "")
	require.NoError(t, err)
	require.NoError(t, rc.Abort())
	err = <-readErr
	require.Error(t, err)
	require.Equal(t, websocket.StatusCode(-1), websocket.CloseStatus(err), "the server saw the transport vanish, not a close frame: %v", err)
	<-rc.Done()
	_, open := <-rc.Events()
	require.False(t, open)
	// Idempotent, and Close afterwards is a no-op.
	require.NoError(t, rc.Abort())
	require.NoError(t, rc.Close(websocket.StatusNormalClosure, ""))
}
