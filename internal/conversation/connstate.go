package conversation

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
)

// connDrainWait bounds how long closeAfterDrain waits for the writer to empty the queue before it
// closes anyway (WS_WRITE_TIMEOUT: a stuck write cannot hold the close back longer than that).
const connDrainWait = 5 * time.Second

// connExit reports that one goroutine of a connection ended. The first one cancels the
// connection (a dead reader, writer or heartbeat means a dead connection); err is nil, or wraps
// context.Canceled, when the goroutine ended because the connection was cancelled.
type connExit struct {
	who string // reader | writer | heartbeat
	err error
}

// failed reports a real failure (read/write error, violation, heartbeat timeout, panic), as
// opposed to the goroutine ending because the connection was cancelled.
func (e connExit) failed() bool { return e.err != nil && !errors.Is(e.err, context.Canceled) }

// connState is one WebSocket connection of a session: the Conn, its Outbound queue and the three
// goroutines that serve it (reader, writer, heartbeat), plus the per-connection channels the
// reader and the writer feed. It exists from the moment the Manager accepts the socket (the
// handshake runs on it, before any Runtime exists) and is handed to the Runtime after the first
// command; the Runtime drops it on disconnect and a reconnect brings a new connState.
//
// Ownership: the channels are written by the goroutines named below and read by whoever holds the
// connection (Manager.Attach during the handshake, Runtime.loop afterwards); activeGen is read by
// the writer and stored by Runtime.loop (§7.7.2: the writer drops audio of a generation that is
// no longer active).
type connState struct {
	id   string
	conn Conn
	sid  uuid.UUID
	m    *Metrics

	out    *Outbound
	act    *Activity
	reader *Reader
	writer *Writer
	hb     *Heartbeat

	cmdCh     chan protocol.ClientEvent // reader → loop (cap 16)
	notices   chan ReaderNotice         // reader → loop (INVALID_MESSAGE, cap 16)
	audioIn   chan AudioIn              // reader → audio pipeline (cap 50)
	acks      chan WriterAck            // writer → loop (writerAcks, cap 64)
	exits     chan connExit             // goroutines → whoever holds the connection
	activeGen atomic.Uint32

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeMu  sync.Mutex
	closing  bool
	closedCh chan struct{} // closed when the close frame (if any) has been sent
}

// Channel capacities of §7.7.2.
const (
	cmdChCap    = 16
	noticesCap  = 16
	audioInCap  = 50
	writerAcks  = 64
	connExitCap = 4
)

// newConnState builds the connection of session sid over conn. seen is the per-session dedupe of
// client event_ids (nil: a new cache); parent (the request context of the gateway) bounds every
// goroutine. Nothing runs until start.
func newConnState(parent context.Context, d Deps, conn Conn, sid uuid.UUID, seen *EventIDCache) *connState {
	ctx, cancel := context.WithCancel(parent)
	act := NewActivity(d.Clock.Now())
	rc := ReaderConfigFromTuning(d.Cfg, sid, d.Clock)
	rc.Activity, rc.Seen, rc.Metrics = act, seen, d.Metrics
	cs := &connState{
		id: d.IDs.New().String(), conn: conn, sid: sid, m: d.Metrics,
		out:     NewOutbound(orInt(d.Cfg.WSOutboundQueue, 256), d.Metrics),
		act:     act,
		reader:  NewReader(rc),
		writer:  NewWriter(d.Cfg.WSWriteTimeout, d.Metrics),
		hb:      NewHeartbeat(d.Clock, d.Cfg.WSPingInterval, d.Cfg.WSIdleTimeout),
		cmdCh:   make(chan protocol.ClientEvent, cmdChCap),
		notices: make(chan ReaderNotice, noticesCap),
		audioIn: make(chan AudioIn, audioInCap),
		acks:    make(chan WriterAck, writerAcks),
		exits:   make(chan connExit, connExitCap),
		ctx:     ctx, cancel: cancel, closedCh: make(chan struct{}),
	}
	cs.hb.NewTicker = d.NewTicker
	return cs
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// start launches the reader, the writer and the heartbeat.
func (cs *connState) start() {
	cs.goSafe("reader", func() error { return cs.reader.Run(cs.ctx, cs.conn, cs.cmdCh, cs.audioIn, cs.notices) })
	cs.goSafe("writer", func() error { return cs.writer.Run(cs.ctx, cs.conn, cs.out, &cs.activeGen, cs.acks) })
	cs.goSafe("heartbeat", func() error { return cs.hb.Run(cs.ctx, cs.conn, cs.act) })
}

// goSafe runs fn as one goroutine of the connection. A panic is recovered and reported as that
// goroutine's exit with a *panicError; the loop (or the handshake) answers with FATAL{SERVER_ERROR}.
func (cs *connState) goSafe(who string, fn func() error) {
	cs.wg.Add(1)
	go func() {
		defer cs.wg.Done()
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = &panicError{who: "connection " + who, value: p, stack: debug.Stack()}
				}
			}()
			err = fn()
		}()
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			cs.cancel() // any exit ends the whole connection, except a violation: the loop still has to answer it on this connection
		}
		select {
		case cs.exits <- connExit{who: who, err: err}:
		default: // already reported by another goroutine; the first exit is enough
		}
	}()
}

// beginClose marks the connection as closing; false when a close is already under way.
func (cs *connState) beginClose() bool {
	cs.closeMu.Lock()
	defer cs.closeMu.Unlock()
	if cs.closing {
		return false
	}
	cs.closing = true
	return true
}

// closeAfterDrain closes the socket with code once the writer has written everything queued (a
// terminal event goes out before the close frame), bounded by connDrainWait, and cancels the
// connection. It does not block the caller. Safe to call more than once: the first call wins.
func (cs *connState) closeAfterDrain(code int, reason string) {
	if !cs.beginClose() {
		return
	}
	go func() {
		defer close(cs.closedCh)
		cs.waitDrained()
		_ = cs.conn.Close(code, reason) // best effort: the peer may be gone already
		cs.m.WSClose(code)
		cs.cancel()
	}()
}

// closeNow closes the socket at once, dropping what is queued (slow consumer, hard stop). Safe to
// call more than once: the first close wins.
func (cs *connState) closeNow(code int, reason string) {
	if !cs.beginClose() {
		return
	}
	defer close(cs.closedCh)
	_ = cs.conn.Close(code, reason) // best effort
	cs.m.WSClose(code)
	cs.cancel()
}

// waitDrained polls until the writer holds nothing, the connection is cancelled or connDrainWait
// (real time: a socket write is real I/O) has passed.
func (cs *connState) waitDrained() {
	deadline := time.NewTimer(connDrainWait)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for !cs.out.Drained() {
		select {
		case <-poll.C:
		case <-cs.ctx.Done():
			return
		case <-deadline.C:
			return
		}
	}
}

// wait blocks until every goroutine of the connection has exited and a close in progress has
// finished.
func (cs *connState) wait() {
	cs.wg.Wait()
	cs.closeMu.Lock()
	closing := cs.closing
	cs.closeMu.Unlock()
	if closing {
		<-cs.closedCh
	}
}
