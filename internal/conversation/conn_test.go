package conversation

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// Test helpers for the I/O plumbing: an in-memory Conn, fake timeouts on a fake clock and a
// manual Ticker. testutil/rtharness (Task 18) carries its own PipeConn built the same way.

const convPkg = "github.com/nabePi/kotoki-backend/internal/conversation"

func noLeak(t *testing.T) { t.Helper(); testutil.NoGoroutineLeak(t, convPkg) }

var t0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

type inMsg struct {
	kind MessageKind
	data []byte
}

type written struct {
	Kind MessageKind
	Data []byte
}

// pipeConn is an in-memory Conn: the test feeds what Read returns and records what Write sends.
type pipeConn struct {
	in chan inMsg

	mu          sync.Mutex
	out         []written
	blockWrites bool
	writeEnter  chan struct{} // receives one value each time Write starts
	pingErr     error
	pings       int
	pingEnter   chan struct{}
	closeCode   int
	closeReason string
	closed      chan struct{}
	closeOnce   sync.Once
}

func newPipeConn() *pipeConn {
	return &pipeConn{
		in:         make(chan inMsg, 1024),
		writeEnter: make(chan struct{}, 1024),
		pingEnter:  make(chan struct{}, 1024),
		closed:     make(chan struct{}),
	}
}

func (p *pipeConn) send(kind MessageKind, b []byte) { p.in <- inMsg{kind, b} }

// eof makes Read return io.EOF once every queued message has been read.
func (p *pipeConn) eof() { close(p.in) }

func (p *pipeConn) Read(ctx context.Context) (MessageKind, []byte, error) {
	select {
	case m, ok := <-p.in:
		if !ok {
			return 0, nil, io.EOF
		}
		return m.kind, m.data, nil
	case <-p.closed:
		return 0, nil, errors.New("pipe: closed")
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (p *pipeConn) Write(ctx context.Context, kind MessageKind, b []byte) error {
	p.writeEnter <- struct{}{}
	p.mu.Lock()
	block := p.blockWrites
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out = append(p.out, written{kind, append([]byte(nil), b...)})
	return nil
}

func (p *pipeConn) Ping(context.Context) error {
	p.mu.Lock()
	p.pings++
	err := p.pingErr
	p.mu.Unlock()
	p.pingEnter <- struct{}{}
	return err
}

func (p *pipeConn) Close(code int, reason string) error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closeCode, p.closeReason = code, reason
		p.mu.Unlock()
		close(p.closed)
	})
	return nil
}

func (p *pipeConn) written() []written {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]written(nil), p.out...)
}

func (p *pipeConn) setPingErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pingErr = err
}

func (p *pipeConn) closedWith() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCode, p.closeReason
}

// fakeTimeouts implements TimeoutFunc on a fake clock: a context expires (cause
// context.DeadlineExceeded) when Advance moves the clock past its deadline.
type fakeTimeouts struct {
	clk *clock.Fake

	mu      sync.Mutex
	pending map[int]fakeDeadline
	next    int
	asked   []time.Duration
}

type fakeDeadline struct {
	at     time.Time
	cancel context.CancelCauseFunc
}

func newFakeTimeouts(clk *clock.Fake) *fakeTimeouts {
	return &fakeTimeouts{clk: clk, pending: map[int]fakeDeadline{}}
}

func (f *fakeTimeouts) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	f.mu.Lock()
	id := f.next
	f.next++
	f.pending[id] = fakeDeadline{at: f.clk.Now().Add(d), cancel: cancel}
	f.asked = append(f.asked, d)
	f.mu.Unlock()
	return ctx, func() {
		f.mu.Lock()
		delete(f.pending, id)
		f.mu.Unlock()
		cancel(context.Canceled)
	}
}

func (f *fakeTimeouts) Advance(d time.Duration) {
	f.clk.Advance(d)
	now := f.clk.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, p := range f.pending {
		if !now.Before(p.at) {
			p.cancel(context.DeadlineExceeded)
			delete(f.pending, id)
		}
	}
}

func (f *fakeTimeouts) durations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.asked...)
}

// manualTicker is a Ticker the test fires by hand.
type manualTicker struct {
	c       chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newManualTicker() *manualTicker {
	return &manualTicker{c: make(chan time.Time), stopped: make(chan struct{})}
}

func (m *manualTicker) C() <-chan time.Time { return m.c }
func (m *manualTicker) Stop()               { m.once.Do(func() { close(m.stopped) }) }

// tick delivers one tick and returns once the receiver has taken it.
func (m *manualTicker) tick(t *testing.T, now time.Time) {
	t.Helper()
	select {
	case m.c <- now:
	case <-time.After(2 * time.Second):
		t.Fatal("ticker: nobody received the tick")
	}
}

// waitSignal waits for one value on ch.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitErr waits for the result of a goroutine.
func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s to return", what)
		return nil
	}
}
