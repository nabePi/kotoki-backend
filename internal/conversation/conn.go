package conversation

import (
	"context"
	"time"
)

// MessageKind is the WebSocket message type (spec §7.13.1).
type MessageKind uint8

const (
	// MessageText carries a JSON envelope.
	MessageText MessageKind = 1
	// MessageBinary carries a D-073 audio frame.
	MessageBinary MessageKind = 2
)

// Conn is an upgraded WebSocket connection (spec §7.13.1). The wsgateway adapter implements it
// over *websocket.Conn; coder/websocket is imported only there. Read is called only by the
// reader goroutine and Write only by the writer goroutine. Read returns a buffer the caller owns.
type Conn interface {
	Read(ctx context.Context) (MessageKind, []byte, error)
	Write(ctx context.Context, kind MessageKind, p []byte) error // bounded by WS_WRITE_TIMEOUT by the caller
	Ping(ctx context.Context) error                              // D-082 heartbeat; the adapter uses the library ping
	Close(code int, reason string) error                         // close code §7.4.9
}

// TimeoutFunc derives a context that expires after d. Production code uses
// context.WithTimeout; tests substitute one driven by a fake clock.
type TimeoutFunc func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)

// Ticker is a source of periodic ticks (time.Ticker in production, a manual ticker in tests).
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// NewRealTicker wraps time.NewTicker.
func NewRealTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }
