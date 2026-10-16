package wsgateway

import (
	"context"
	"sync/atomic"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/nabePi/kotoki-backend/internal/conversation"
)

// maxCloseReason is the most bytes RFC 6455 leaves for the reason of a close frame (125 minus the
// two bytes of the code).
const maxCloseReason = 123

// conn adapts *websocket.Conn to conversation.Conn: the conversation package sees no WebSocket
// library (D-005).
//
// It also remembers the close code of the connection (the one this side sent, else the one the
// peer sent) for the ws.connection span.
type conn struct {
	c    *websocket.Conn
	code atomic.Int64 // first close code seen; 0 = none
}

var _ conversation.Conn = (*conn)(nil)

// NewConn wraps an upgraded connection.
func NewConn(c *websocket.Conn) conversation.Conn { return newConn(c) }

func newConn(c *websocket.Conn) *conn { return &conn{c: c} }

// closeCode is the first close code seen on the connection, 0 when it ended without one.
func (a *conn) closeCode() int { return int(a.code.Load()) }

func (a *conn) Read(ctx context.Context) (conversation.MessageKind, []byte, error) {
	typ, b, err := a.c.Read(ctx)
	if err != nil {
		if st := websocket.CloseStatus(err); st != -1 {
			a.code.CompareAndSwap(0, int64(st))
		}
		return 0, nil, err
	}
	if typ == websocket.MessageBinary {
		return conversation.MessageBinary, b, nil
	}
	return conversation.MessageText, b, nil
}

func (a *conn) Write(ctx context.Context, kind conversation.MessageKind, p []byte) error {
	typ := websocket.MessageText
	if kind == conversation.MessageBinary {
		typ = websocket.MessageBinary
	}
	return a.c.Write(ctx, typ, p)
}

func (a *conn) Ping(ctx context.Context) error { return a.c.Ping(ctx) }

// Close sends a close frame with the application code (§7.4.9) and the reason cut to what fits.
func (a *conn) Close(code int, reason string) error {
	a.code.CompareAndSwap(0, int64(code))
	return a.c.Close(websocket.StatusCode(code), truncateReason(reason))
}

// truncateReason cuts s to maxCloseReason bytes without splitting a UTF-8 sequence.
func truncateReason(s string) string {
	if len(s) <= maxCloseReason {
		return s
	}
	s = s[:maxCloseReason]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
