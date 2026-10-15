package wsgateway

import (
	"context"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/nabePi/kotoki-backend/internal/conversation"
)

// maxCloseReason is the most bytes RFC 6455 leaves for the reason of a close frame (125 minus the
// two bytes of the code).
const maxCloseReason = 123

// conn adapts *websocket.Conn to conversation.Conn: the conversation package sees no WebSocket
// library (D-005).
type conn struct{ c *websocket.Conn }

var _ conversation.Conn = conn{}

// NewConn wraps an upgraded connection.
func NewConn(c *websocket.Conn) conversation.Conn { return conn{c: c} }

func (a conn) Read(ctx context.Context) (conversation.MessageKind, []byte, error) {
	typ, b, err := a.c.Read(ctx)
	if err != nil {
		return 0, nil, err
	}
	if typ == websocket.MessageBinary {
		return conversation.MessageBinary, b, nil
	}
	return conversation.MessageText, b, nil
}

func (a conn) Write(ctx context.Context, kind conversation.MessageKind, p []byte) error {
	typ := websocket.MessageText
	if kind == conversation.MessageBinary {
		typ = websocket.MessageBinary
	}
	return a.c.Write(ctx, typ, p)
}

func (a conn) Ping(ctx context.Context) error { return a.c.Ping(ctx) }

// Close sends a close frame with the application code (§7.4.9) and the reason cut to what fits.
func (a conn) Close(code int, reason string) error {
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
