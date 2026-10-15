package conversation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// AttachTicket is what the gateway got from the consumed WS ticket (§7.3.1 step 5).
type AttachTicket struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
}

// AttachError is the reason a connection was refused or ended by the Manager before it was
// handed to a Runtime; Code is the error event the client received.
type AttachError struct {
	Code errs.Code
}

func (e *AttachError) Error() string { return "conversation: attach refused: " + string(e.Code) }

// readyFrameHeaders are the frame_header strings of session.ready (§7.4.5).
const (
	upstreamFrameHeader   = "0x01|u32 seq"
	downstreamFrameHeader = "0x02|u32 generation|u32 seq"
	wsWriteFallback       = 5 * time.Second // write bound for a refusal written without a writer goroutine
)

// Manager is the registry of the sessions of this pod and the entry of every connection
// (§7.1): Attach runs §7.3.1 steps 7–11 and hands the connection to a Runtime. The gateway does
// steps 1–6 (origin, rate limit, capacity, upgrade, ticket) and calls Attach with the upgraded
// socket; the Manager imports no WebSocket library.
//
// Task 18 holds the skeleton: admission by capacity, the handshake, the start flow. Task 21 adds
// resume on the pod that holds the runtime, the takeover of a session from another pod and the
// TAKEOVER subscriber; Task 23 adds the drain around Attach, podinfo and the draining flag.
type Manager struct {
	d      Deps
	ctx    context.Context // podCtx: ends every runtime
	cancel context.CancelFunc

	mu       sync.Mutex
	runtimes map[uuid.UUID]*Runtime
	pending  int // start flows between the capacity check and the registry
	rwg      sync.WaitGroup
	bg       sync.WaitGroup // the TAKEOVER subscriber

	// resume is the entry of the resume flow (§7.10) for a session that is ACTIVE: the pod that holds
	// the runtime gives it the connection (attachResume), another pod takes the session over
	// (takeover). A session.start on an ACTIVE session goes the same way with last_server_seq 0
	// (§7.3.2).
	resume func(ctx context.Context, h *handshake, rec session.RuntimeRecord, lastServerSeq int64) error
}

// NewManager creates the Manager of the pod; Close stops it.
func NewManager(d Deps) *Manager {
	d = d.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{d: d, ctx: ctx, cancel: cancel, runtimes: map[uuid.UUID]*Runtime{}}
	m.resume = m.attachResume
	d.Metrics.SetSessionCapacity(d.Limits.Max)
	m.subscribe()
	return m
}

// Active is the number of sessions this pod owns, starting ones included (activeSessions of
// §7.12.2, the KEDA metric).
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runtimes) + m.pending
}

// Runtime returns the local runtime of a session.
func (m *Manager) Runtime(sid uuid.UUID) (*Runtime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runtimes[sid]
	return r, ok
}

// Close stops every runtime at once and waits for them. It is the hard stop; the graceful drain
// of Task 23 comes first in production.
func (m *Manager) Close() {
	m.cancel()
	m.rwg.Wait()
	m.bg.Wait()
}

// reserve takes a slot for a new start; false when the pod is at REALTIME_MAX_SESSIONS (4503).
func (m *Manager) reserve() bool { return m.reserveUpTo(m.d.Limits.Max) }

// reserveUpTo takes a slot unless the pod already holds limit sessions: Max for a new start,
// HardMax for a takeover (a session that already exists may still be resumed up to there, §7.12.2).
func (m *Manager) reserveUpTo(limit int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.runtimes)+m.pending >= limit {
		return false
	}
	m.pending++
	return true
}

func (m *Manager) unreserve() {
	m.mu.Lock()
	m.pending--
	m.mu.Unlock()
}

// register turns a reserved slot into the runtime r.
func (m *Manager) register(r *Runtime) {
	m.mu.Lock()
	m.pending--
	m.runtimes[r.sid] = r
	m.mu.Unlock()
	m.rwg.Add(1)
}

func (m *Manager) onRuntimeExit(r *Runtime) {
	m.mu.Lock()
	if m.runtimes[r.sid] == r {
		delete(m.runtimes, r.sid)
	}
	m.mu.Unlock()
	m.rwg.Done()
}

// handshake is the connection between session.ready and the hand-over to a Runtime: it owns the
// connState and numbers the events it sends (server_seq 1 for session.ready; the runtime that is
// created continues after them).
type handshake struct {
	m    *Manager
	cs   *connState
	sid  uuid.UUID
	seq  int64
	seen *EventIDCache
}

// send queues one server event of the handshake.
func (h *handshake) send(typ string, payload any) {
	h.seq++
	ev := protocol.ServerEvent{
		Type: typ, Version: protocol.Version, EventID: h.m.d.IDs.New(), ServerSeq: h.seq, SessionID: h.sid,
		TS: h.m.d.Clock.Now(), Payload: payload,
	}
	if err := h.cs.out.EnqueueEvent(ev); err != nil {
		h.m.d.Log.Warn("handshake event not queued", "type", typ, "error", err.Error())
	}
}

// fail answers a fatal error: the event, then the close with the code of the catalog.
func (h *handshake) fail(code errs.Code, opts ...protocol.ErrorOption) error {
	h.send("error", protocol.NewError(code, h.m.d.IDs.New(), nil, nil, opts...))
	h.cs.closeAfterDrain(protocol.CloseCode(code), string(code))
	return &AttachError{Code: code}
}

// Attach serves one upgraded connection (§7.3.1 steps 7–11): it loads the session, checks the
// owner and the status (§7.3.2), sends session.ready, waits for the first command and hands the
// connection to a Runtime. It returns when the connection has ended: the Runtime owns the
// connection after the hand-over, so the return only means this socket is done. ctx is the
// request context of the gateway; its end closes the connection.
func (m *Manager) Attach(ctx context.Context, conn Conn, t AttachTicket) error {
	rec, err := m.d.Session.GetForAttach(ctx, t.SessionID)
	if err != nil {
		code := errs.CodeServiceUnavailable
		if errors.Is(err, session.ErrNotFound) {
			code = errs.CodeSessionNotFound
		}
		return m.refuse(ctx, conn, t.SessionID, code)
	}
	if rec.UserID != t.UserID {
		return m.refuse(ctx, conn, t.SessionID, errs.CodeForbidden)
	}
	switch rec.Status { //nolint:exhaustive // §7.3.2: every other status is not resumable
	case session.StatusReserved, session.StatusActive:
	case session.StatusStarting:
		return m.refuse(ctx, conn, t.SessionID, errs.CodeSessionStateConflict, protocol.WithRetryable(true))
	default:
		return m.refuse(ctx, conn, t.SessionID, errs.CodeSessionNotResumable)
	}

	seen := NewEventIDCache()
	if r, ok := m.Runtime(t.SessionID); ok { // the dedupe of client event_ids outlives the connection (§7.4.2)
		seen = r.seen
	}
	cs := newConnState(ctx, m.d, conn, t.SessionID, seen)
	cs.start()
	defer cs.wait()
	h := &handshake{m: m, cs: cs, sid: t.SessionID, seen: seen}

	expected := "session.start"
	if rec.Status == session.StatusActive {
		expected = "session.resume"
	}
	h.send("session.ready", m.ready(rec, expected))
	first, err := m.awaitFirst(ctx, h)
	if err != nil {
		return err
	}
	return m.dispatchFirst(ctx, h, rec, first)
}

// ready builds the payload of session.ready.
func (m *Manager) ready(rec session.RuntimeRecord, expected string) protocol.SessionReady {
	params := protocol.AudioParams{Codec: "opus", SampleRate: 48000, Channels: 1, FrameMS: FrameMs}
	return protocol.SessionReady{
		Status: string(rec.Status), ExpectedCommand: expected, ConversationStartMode: string(rec.ConversationStartMode),
		ProtocolVersion: protocol.Version,
		Audio: protocol.ReadyAudio{
			Upstream:   protocol.AudioStream{AudioParams: params, FrameHeader: upstreamFrameHeader},
			Downstream: protocol.AudioStream{AudioParams: params, FrameHeader: downstreamFrameHeader},
		},
		Limits: protocol.Limits{
			MaxJSONBytes: int(orInt64(m.d.Cfg.WSMaxJSONBytes, 16384)), MaxBinaryBytes: int(orInt64(m.d.Cfg.WSMaxBinaryBytes, protocol.MaxBinary)),
			MaxBinaryPerSecond: orInt(m.d.Cfg.WSBinaryRate, 100),
		},
		HeartbeatIntervalS: int(orDur(m.d.Cfg.WSPingInterval, 15*time.Second) / time.Second),
		ServerTime:         protocol.Timestamp{Time: m.d.Clock.Now()},
	}
}

func orInt64(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}

// refuse answers a connection that is turned away before it has a connState (steps 7–9): the
// error event and the close are written straight to the socket (no writer goroutine exists yet).
func (m *Manager) refuse(ctx context.Context, conn Conn, sid uuid.UUID, code errs.Code, opts ...protocol.ErrorOption) error {
	ev := protocol.ServerEvent{
		Type: "error", Version: protocol.Version, EventID: m.d.IDs.New(), ServerSeq: 1, SessionID: sid, TS: m.d.Clock.Now(),
		Payload: protocol.NewError(code, m.d.IDs.New(), nil, nil, opts...),
	}
	if env, err := protocol.MarshalServer(ev); err == nil {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wsWriteFallback)
		_ = conn.Write(wctx, MessageText, env) // best effort: the peer may be gone
		cancel()
	}
	_ = conn.Close(protocol.CloseCode(code), string(code))
	m.d.Metrics.WSClose(protocol.CloseCode(code))
	if code == errs.CodeServiceUnavailable {
		m.d.Metrics.Connection(ConnRejectedUnavailable)
	} else {
		m.d.Metrics.Connection(ConnRejectedState)
	}
	return &AttachError{Code: code}
}

// awaitFirst is step 10: the first command, at most WS_HANDSHAKE_TIMEOUT after session.ready. The
// deadline is a time on the runtime clock checked on a ticker, like every other deadline.
func (m *Manager) awaitFirst(ctx context.Context, h *handshake) (protocol.ClientEvent, error) {
	deadline := m.d.Clock.Now().Add(m.d.Cfg.WSHandshakeTimeout)
	tk := m.d.NewTicker(loopTick)
	defer tk.Stop()
	for {
		select {
		case ev := <-h.cs.cmdCh:
			return ev, nil
		case n := <-h.cs.notices:
			h.send("error", protocol.NewError(n.Code, m.d.IDs.New(), n.RefEventID, nil))
		case x := <-h.cs.exits:
			var pe *ProtocolError
			var pan *panicError
			switch {
			case errors.As(x.err, &pe):
				return protocol.ClientEvent{}, h.violation(pe)
			case errors.As(x.err, &pan):
				m.d.Log.Error("panic recovered during the handshake", "error", pan.Error(), "stack", string(pan.stack))
				return protocol.ClientEvent{}, h.fail(errs.CodeInternalError)
			}
			code := exitCloseCode(x.who)
			h.cs.closeNow(code, closeReason(code)) // the peer is gone or the connection broke: release the socket
			return protocol.ClientEvent{}, fmt.Errorf("conversation: connection ended before the first command: %w", x.err)
		case <-tk.C():
			if !m.d.Clock.Now().Before(deadline) {
				return protocol.ClientEvent{}, h.fail(errs.CodeHandshakeTimeout)
			}
		case <-ctx.Done():
			return protocol.ClientEvent{}, ctx.Err()
		case <-m.ctx.Done():
			return protocol.ClientEvent{}, h.fail(errs.CodeServerDraining)
		}
	}
}

// violation answers a fatal ProtocolError found by the reader before the session has a runtime:
// the error and the 1008 close. There is no fence yet, so no PROTOCOL_VIOLATION row is written.
func (h *handshake) violation(pe *ProtocolError) error {
	return h.fail(pe.Code)
}

// dispatchFirst is step 11 and the table of §7.3.2: the first command must be session.start or
// session.resume, with the audio format of D-071, and fit the status.
func (m *Manager) dispatchFirst(ctx context.Context, h *handshake, rec session.RuntimeRecord, first protocol.ClientEvent) error {
	switch p := first.Payload.(type) {
	case protocol.SessionStart:
		if !p.Audio.Valid() {
			m.d.Metrics.protocolViolation(ViolationAudioFormat)
			return h.fail(errs.CodeUnsupportedAudioFormat)
		}
		if rec.Status == session.StatusActive { // §7.3.2: an idempotent start is a resume from 0
			return m.resume(ctx, h, rec, 0)
		}
		return m.startRuntime(ctx, h, rec)
	case protocol.SessionResume:
		if !p.Audio.Valid() {
			m.d.Metrics.protocolViolation(ViolationAudioFormat)
			return h.fail(errs.CodeUnsupportedAudioFormat)
		}
		if rec.Status != session.StatusActive { // RESERVED: the client must start
			m.d.Metrics.Connection(ConnRejectedState)
			return h.fail(errs.CodeSessionStateConflict)
		}
		return m.resume(ctx, h, rec, p.LastServerSeq)
	}
	m.d.Metrics.protocolViolation(ViolationHeader)
	return h.fail(errs.CodeProtocolViolation)
}

// attachResume is the resume flow (§7.10) for a session that is ACTIVE: the pod that holds the
// runtime gives it the new connection, any other pod takes the session over.
func (m *Manager) attachResume(ctx context.Context, h *handshake, rec session.RuntimeRecord, lastServerSeq int64) error {
	if r, ok := m.Runtime(rec.ID); ok {
		return m.resumeSamePod(ctx, h, r, lastServerSeq)
	}
	return m.takeover(ctx, h, rec, lastServerSeq)
}

// startRuntime is §7.6.3 steps 1–2 followed by the hand-over: capacity, ClaimRuntime, MarkStarting,
// then the Runtime takes the connection and runs steps 3–9.
func (m *Manager) startRuntime(ctx context.Context, h *handshake, rec session.RuntimeRecord) error {
	if !m.reserve() {
		m.d.Metrics.Connection(ConnRejectedCapacity)
		return h.fail(errs.CodeServerAtCapacity)
	}
	fence, _, err := m.d.Session.ClaimRuntime(ctx, rec.ID, []session.Status{session.StatusReserved})
	if err != nil {
		m.unreserve()
		return m.startRefused(h, err)
	}
	started, err := m.d.Session.MarkStarting(ctx, rec.ID, fence) // the newer record: it carries opening_prompt_version_id
	if err != nil {
		m.unreserve()
		if errors.Is(err, session.ErrStartDeadlinePassed) {
			m.d.Metrics.Connection(ConnRejectedState)
			return h.fail(errs.CodeSessionStartDeadlinePassed)
		}
		return m.startRefused(h, err)
	}
	// The runtime outlives the connection and the request: its context is the pod's.
	r := newRuntime(m.ctx, m.d, started, fence, h.cs, h.seen, m.onRuntimeExit) //nolint:contextcheck // runtime context = pod context, not the request context
	r.serverSeq = h.seq                                                        // session.ready was server_seq 1
	m.register(r)
	m.d.Metrics.Connection(ConnAccepted)
	r.begin() //nolint:contextcheck // the runtime goroutines run on the runtime context (pod context), not the request context
	return nil
}

// startRefused maps an error of ClaimRuntime/MarkStarting to the refusal of the client.
func (m *Manager) startRefused(h *handshake, err error) error {
	switch {
	case errors.Is(err, session.ErrNotFound):
		m.d.Metrics.Connection(ConnRejectedState)
		return h.fail(errs.CodeSessionNotFound)
	case errors.Is(err, session.ErrInvalidTransition), errors.Is(err, session.ErrFenceLost):
		m.d.Metrics.Connection(ConnRejectedState)
		return h.fail(errs.CodeSessionStateConflict, protocol.WithRetryable(true)) // the status moved under us
	}
	m.d.Log.Error("start refused", "session_id", h.sid.String(), "error", err.Error())
	m.d.Metrics.Connection(ConnRejectedUnavailable)
	return h.fail(errs.CodeServiceUnavailable)
}
