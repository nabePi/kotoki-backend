package conversation

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// ErrHeartbeatTimeout means no frame and no successful ping for longer than WS_IDLE_TIMEOUT: the
// heartbeat closed the connection 4102 (best effort) and the caller runs the disconnect flow.
var ErrHeartbeatTimeout = errors.New("conversation: heartbeat timeout")

// Activity is lastActivity of §7.4.10: the reader touches it for every incoming frame and the
// heartbeat after every successful ping. Safe for concurrent use.
type Activity struct{ last atomic.Int64 }

// NewActivity starts the activity clock at t (the attach time).
func NewActivity(t time.Time) *Activity {
	a := &Activity{}
	a.last.Store(t.UnixNano())
	return a
}

// Touch records activity at t; an older t never moves lastActivity back.
func (a *Activity) Touch(t time.Time) {
	n := t.UnixNano()
	for {
		cur := a.last.Load()
		if n <= cur || a.last.CompareAndSwap(cur, n) {
			return
		}
	}
}

// Last returns the time of the latest activity (UTC).
func (a *Activity) Last() time.Time { return time.Unix(0, a.last.Load()).UTC() }

// Heartbeat pings the connection every Interval (WS_PING_INTERVAL, 15 s; each ping bounded by
// PingTimeout, 15 s) and declares it dead when now − lastActivity > Idle (WS_IDLE_TIMEOUT, 45 s).
type Heartbeat struct {
	Clock       clock.Clock
	Interval    time.Duration
	PingTimeout time.Duration
	Idle        time.Duration
	// NewTicker and WithTimeout default to NewRealTicker and context.WithTimeout.
	NewTicker   func(time.Duration) Ticker
	WithTimeout TimeoutFunc
}

// NewHeartbeat creates a heartbeat pinging every interval (also the ping timeout) and timing out
// after idle; zero values mean 15 s / 45 s.
func NewHeartbeat(clk clock.Clock, interval, idle time.Duration) *Heartbeat {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if idle <= 0 {
		idle = 45 * time.Second
	}
	return &Heartbeat{Clock: clk, Interval: interval, PingTimeout: interval, Idle: idle,
		NewTicker: NewRealTicker, WithTimeout: context.WithTimeout}
}

// Run pings until ctx is done (nil) or the connection idles out: then it closes the connection
// with 4102 HEARTBEAT_TIMEOUT and returns ErrHeartbeatTimeout. A failed ping is not fatal by
// itself; only the idle rule ends the connection.
func (h *Heartbeat) Run(ctx context.Context, conn Conn, act *Activity) error {
	t := h.NewTicker(h.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
		}
		if h.Clock.Now().Sub(act.Last()) > h.Idle {
			_ = conn.Close(protocol.CloseCode(errs.CodeHeartbeatTimeout), string(errs.CodeHeartbeatTimeout)) // best effort
			return ErrHeartbeatTimeout
		}
		pctx, cancel := h.WithTimeout(ctx, h.PingTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err == nil {
			act.Touch(h.Clock.Now())
		}
	}
}
