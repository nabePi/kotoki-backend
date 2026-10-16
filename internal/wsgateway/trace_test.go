package wsgateway_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
)

// wsSpan waits for the finished ws.connection span number n (1-based): the span ends when the
// handler returns, which is after the connection has ended.
func wsSpan(t *testing.T, rec *testutil.TraceRecorder, n int) map[string]attribute.Value {
	t.Helper()
	var spans []tracetest.SpanStub
	require.Eventually(t, func() bool {
		spans = spans[:0]
		for _, s := range rec.Spans() {
			if s.Name == "ws.connection" {
				spans = append(spans, s)
			}
		}
		return len(spans) >= n
	}, 5*time.Second, 10*time.Millisecond, "ws.connection span %d", n)
	m := map[string]attribute.Value{}
	for _, kv := range spans[n-1].Attributes {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func sendCmd(t *testing.T, r *rig, c *websocket.Conn, typ string, payload any) {
	t.Helper()
	raw, err := protocol.MarshalClient(r.H.Cmd(typ, payload))
	require.NoError(t, err)
	require.NoError(t, c.Write(ctx5(t), websocket.MessageText, raw))
}

func TestConnectionSpanOfARefusal(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	r := newRig(t, nil)
	expectRefusal(t, r.mustDial(strings.Repeat("A", 43)), "TICKET_INVALID", 4001)

	m := wsSpan(t, rec, 1)
	require.EqualValues(t, 4001, m["close.code"].AsInt64())
	require.NotContains(t, m, "session.id", "no session is known before the ticket")
	require.NotContains(t, m, "resume")
}

func TestConnectionSpanBeforeTheUpgrade(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	r := newRig(t, nil)
	require.Equal(t, 403, r.get(r.issue(), "https://evil.example").StatusCode)

	m := wsSpan(t, rec, 1)
	require.NotContains(t, m, "close.code", "no socket, no close code")
	require.NotContains(t, m, "session.id")
}

func TestConnectionSpanOfAStartAndAResume(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	r := newRig(t, nil)

	c := r.mustDial(r.issue())
	require.Equal(t, "session.ready", readEvent(t, c).Type)
	sendCmd(t, r, c, "session.start", protocol.SessionStart{Audio: rtharness.ValidAudio})
	for ev := readEvent(t, c); ev.Type != "session.started"; ev = readEvent(t, c) {
		require.NotEqual(t, "error", ev.Type)
	}
	require.NoError(t, c.Close(4321, "bye")) // an application code no server path would pick by itself

	m := wsSpan(t, rec, 1)
	require.Equal(t, r.H.SID.String(), m["session.id"].AsString())
	require.NotEmpty(t, m["conn.id"].AsString())
	require.Contains(t, m, "resume")
	require.False(t, m["resume"].AsBool())
	require.EqualValues(t, 4321, m["close.code"].AsInt64(), "the close the peer sent")

	// the session is ACTIVE and detached: a second connection resumes it
	c2 := r.mustDial(r.issue())
	require.Equal(t, "session.ready", readEvent(t, c2).Type)
	sendCmd(t, r, c2, "session.resume", protocol.SessionResume{LastServerSeq: 0, Audio: rtharness.ValidAudio})
	for ev := readEvent(t, c2); ev.Type != "session.snapshot"; ev = readEvent(t, c2) {
		require.NotEqual(t, "error", ev.Type)
	}
	require.NoError(t, c2.Close(websocket.StatusNormalClosure, "bye"))

	m2 := wsSpan(t, rec, 2)
	require.Contains(t, m2, "resume")
	require.True(t, m2["resume"].AsBool())
	require.NotEqual(t, m["conn.id"].AsString(), m2["conn.id"].AsString(), "every connection has its own id")
	require.Equal(t, r.H.SID.String(), m2["session.id"].AsString())
}

func TestConnectionSpanHoldsNoTicket(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	r := newRig(t, nil)
	ticket := r.issue()
	c := r.mustDial(ticket)
	require.Equal(t, "session.ready", readEvent(t, c).Type)
	require.NoError(t, c.Close(websocket.StatusNormalClosure, "bye"))
	wsSpan(t, rec, 1)
	for _, s := range rec.Spans() {
		for _, kv := range s.Attributes {
			require.NotContains(t, kv.Value.String(), ticket, "span %s attribute %s", s.Name, kv.Key)
		}
	}
}
