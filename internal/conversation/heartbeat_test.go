package conversation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

type hbRig struct {
	conn   *pipeConn
	clk    *clock.Fake
	tos    *fakeTimeouts
	ticker *manualTicker
	act    *Activity
	done   chan error
	period time.Duration
}

func startHeartbeat(t *testing.T) *hbRig {
	t.Helper()
	noLeak(t)
	clk := clock.NewFake(t0)
	r := &hbRig{conn: newPipeConn(), clk: clk, tos: newFakeTimeouts(clk), ticker: newManualTicker(), act: NewActivity(t0), done: make(chan error, 1)}
	h := NewHeartbeat(clk, 15*time.Second, 45*time.Second)
	h.WithTimeout = r.tos.WithTimeout
	h.NewTicker = func(d time.Duration) Ticker { r.period = d; return r.ticker }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- h.Run(ctx, r.conn, r.act) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
			t.Error("heartbeat did not stop")
		}
	})
	return r
}

// step advances the fake clock by 15 s and fires one tick.
func (r *hbRig) step(t *testing.T) {
	t.Helper()
	r.clk.Advance(15 * time.Second)
	r.ticker.tick(t, r.clk.Now())
}

func TestHeartbeatPingKeepsConnectionAlive(t *testing.T) {
	r := startHeartbeat(t)
	for range 5 { // 75 s, every ping succeeds
		r.step(t)
		waitSignal(t, r.conn.pingEnter, "ping")
	}
	require.Equal(t, 15*time.Second, r.period, "ping every 15 s")
	require.Equal(t, 15*time.Second, r.tos.durations()[0], "ping timeout 15 s")
	require.Eventually(t, func() bool { return r.act.Last().Equal(r.clk.Now()) }, 2*time.Second, time.Millisecond,
		"a successful ping is activity")
	code, _ := r.conn.closedWith()
	require.Zero(t, code)
}

func TestHeartbeatIdleTimeout(t *testing.T) {
	r := startHeartbeat(t)
	r.conn.setPingErr(errors.New("no pong"))
	for range 3 { // 15, 30, 45 s: idle is not yet > 45 s
		r.step(t)
		waitSignal(t, r.conn.pingEnter, "ping")
	}
	select {
	case err := <-r.done:
		t.Fatalf("heartbeat stopped at exactly 45 s idle: %v", err)
	default:
	}
	// The reader saw a frame at 50 s: idle restarts from there.
	r.act.Touch(r.clk.Now().Add(5 * time.Second))
	r.step(t) // 60 s: idle 10 s
	waitSignal(t, r.conn.pingEnter, "ping")
	for range 3 { // 75, 90 → ping; 105 s: idle 55 s > 45 s
		r.step(t)
		select {
		case <-r.conn.pingEnter:
		case err := <-r.done:
			require.ErrorIs(t, err, ErrHeartbeatTimeout)
			code, reason := r.conn.closedWith()
			require.Equal(t, 4102, code)
			require.Equal(t, "HEARTBEAT_TIMEOUT", reason)
			require.Equal(t, t0.Add(105*time.Second), r.clk.Now())
			r.done <- err
			return
		case <-time.After(2 * time.Second):
			t.Fatal("heartbeat neither pinged nor stopped")
		}
	}
	t.Fatal("heartbeat never timed out")
}

func TestActivityTouchIsMonotonic(t *testing.T) {
	a := NewActivity(t0)
	a.Touch(t0.Add(time.Second))
	a.Touch(t0) // an older touch (racing goroutine) never moves lastActivity back
	require.Equal(t, t0.Add(time.Second), a.Last())
}
