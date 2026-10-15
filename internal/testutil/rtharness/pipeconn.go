package rtharness

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
)

// Written is one message the server wrote to the PipeConn.
type Written struct {
	Kind conversation.MessageKind
	Data []byte
}

type inMsg struct {
	kind conversation.MessageKind
	data []byte
}

// PipeConn is an in-memory conversation.Conn: the test sends what Read returns and reads what the
// server Wrote. It has the pattern of the pipeConn of the conversation package tests, exported for
// the tests of every package that uses the harness.
type PipeConn struct {
	in chan inMsg

	mu          sync.Mutex
	out         []Written
	sent        int // messages queued with Send
	delivered   int // messages Read has returned
	processed   int // messages the reader finished with (it called Read again)
	pings       int
	closeCode   int
	closeReason string
	closedFlag  bool
	closed      chan struct{}
	closeOnce   sync.Once
}

// NewPipeConn returns an open connection.
func NewPipeConn() *PipeConn {
	return &PipeConn{in: make(chan inMsg, 4096), closed: make(chan struct{})}
}

// Send queues a message for the server's Read.
func (p *PipeConn) Send(kind conversation.MessageKind, b []byte) {
	p.mu.Lock()
	p.sent++
	p.mu.Unlock()
	p.in <- inMsg{kind, append([]byte(nil), b...)}
}

// SendEvent marshals and sends a client event.
func (p *PipeConn) SendEvent(t testing.TB, ev protocol.ClientEvent) {
	t.Helper()
	b, err := protocol.MarshalClient(ev)
	if err != nil {
		t.Fatalf("rtharness: marshal client event: %v", err)
	}
	p.Send(conversation.MessageText, b)
}

// EOF makes Read return io.EOF once the queued messages have been read (the client went away).
func (p *PipeConn) EOF() { close(p.in) }

// Idle reports that every message sent so far has been read and fully handled by the reader
// (it is back in Read), so nothing is in flight between the test and the server's inputs.
func (p *PipeConn) Idle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.processed == p.sent
}

// Read implements conversation.Conn.
func (p *PipeConn) Read(ctx context.Context) (conversation.MessageKind, []byte, error) {
	p.mu.Lock()
	p.processed = p.delivered // the previous message was handled before this call
	p.mu.Unlock()
	select {
	case m, ok := <-p.in:
		if !ok {
			return 0, nil, io.EOF
		}
		p.mu.Lock()
		p.delivered++
		p.mu.Unlock()
		return m.kind, m.data, nil
	case <-p.closed:
		return 0, nil, errors.New("rtharness: pipe closed")
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

// Write implements conversation.Conn.
func (p *PipeConn) Write(ctx context.Context, kind conversation.MessageKind, b []byte) error {
	select {
	case <-p.closed:
		return errors.New("rtharness: pipe closed")
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out = append(p.out, Written{kind, append([]byte(nil), b...)})
	return nil
}

// Ping implements conversation.Conn.
func (p *PipeConn) Ping(context.Context) error {
	p.mu.Lock()
	p.pings++
	p.mu.Unlock()
	return nil
}

// Close implements conversation.Conn; the first close code wins.
func (p *PipeConn) Close(code int, reason string) error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closeCode, p.closeReason, p.closedFlag = code, reason, true
		p.mu.Unlock()
		close(p.closed)
	})
	return nil
}

// Written returns what the server wrote so far, in order.
func (p *PipeConn) Written() []Written {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Written(nil), p.out...)
}

// Closed is closed once the server closed the connection.
func (p *PipeConn) Closed() <-chan struct{} { return p.closed }

// CloseInfo returns the code and reason of the first close, and whether there was one.
func (p *PipeConn) CloseInfo() (code int, reason string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCode, p.closeReason, p.closedFlag
}

// Pings counts the heartbeat pings.
func (p *PipeConn) Pings() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pings
}
