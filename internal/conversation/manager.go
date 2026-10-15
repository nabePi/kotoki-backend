package conversation

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// podinfo of the pod (§7.9.1): refreshed every 5 s with a 15 s TTL.
const (
	podInfoInterval = 5 * time.Second
	podInfoTTL      = 15 * time.Second
)

// Manager is the registry of the sessions of this pod and the entry of every connection
// (§7.1). The gateway does steps 1–6 of §7.3.1 (origin, rate limit, Admission, upgrade, ticket) and
// calls Attach with the upgraded socket; the Manager imports no WebSocket library.
//
// The files split it by what they do: manager.go is the registry, the capacity rules (§7.12.2),
// the TAKEOVER subscriber's start and podinfo; manager_attach.go is Attach, steps 7–11 and the
// table of §7.3.2, and the start of a runtime; manager_resume.go is resume on this pod, takeover
// and the subscriber; drain.go is the graceful shutdown (§7.12.1).
type Manager struct {
	d       Deps
	ctx     context.Context // podCtx: ends every runtime
	cancel  context.CancelFunc
	started time.Time // podinfo.started_at

	mu       sync.Mutex
	runtimes map[uuid.UUID]*Runtime
	pending  int  // start flows between the capacity check and the registry
	draining bool // set by Drain: no runtime is admitted any more
	// changed is closed (and replaced) whenever a runtime or a reserved slot goes away or a runtime
	// is registered: Drain waits on it for the pod to empty.
	changed chan struct{}
	rwg     sync.WaitGroup
	bg      sync.WaitGroup // the TAKEOVER subscriber
	subOnce sync.Once

	// resume is the entry of the resume flow (§7.10) for a session that is ACTIVE: the pod that holds
	// the runtime gives it the connection (attachResume), another pod takes the session over
	// (takeover). A session.start on an ACTIVE session goes the same way with last_server_seq 0
	// (§7.3.2).
	resume func(ctx context.Context, h *handshake, rec session.RuntimeRecord, lastServerSeq int64) error
}

// NewManager creates the Manager of the pod; Close stops it. The TAKEOVER subscriber is not
// started here: the caller starts it with StartSubscriber.
func NewManager(d Deps) *Manager {
	d = d.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		d: d, ctx: ctx, cancel: cancel, started: d.Clock.Now(), runtimes: map[uuid.UUID]*Runtime{}, changed: make(chan struct{}),
	}
	m.resume = m.attachResume
	d.Metrics.SetSessionCapacity(d.Limits.Max)
	return m
}

// ActiveSessions is the number of sessions this pod owns, starting ones included (activeSessions
// of §7.12.2, the KEDA metric): the runtimes of the registry, whatever their state, plus the slots
// of the starts in flight. It equals sessions_active summed over the state labels once no start is
// in flight.
func (m *Manager) ActiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runtimes) + m.pending
}

// Ready is /readyz (§7.12.2): the pod takes new connections while it is not draining and holds
// fewer than REALTIME_READY_MAX_SESSIONS sessions. Connections it already has are not affected.
func (m *Manager) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.draining && len(m.runtimes)+m.pending < m.d.Limits.ReadyMax
}

// Admission is §7.3.1 step 3, asked by the gateway before the upgrade and before the ticket is
// consumed (R-22): the pod is draining (SERVER_DRAINING), or it holds REALTIME_HARD_MAX_SESSIONS
// sessions already (SERVER_AT_CAPACITY). It looks at nothing but the registry: no Redis, no
// Postgres, so a refusal leaves the ticket where it was.
func (m *Manager) Admission() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.draining:
		return errs.New(errs.CodeServerDraining, "the pod is draining")
	case len(m.runtimes)+m.pending >= m.d.Limits.HardMax:
		return errs.New(errs.CodeServerAtCapacity, "the pod holds the maximum number of sessions")
	}
	return nil
}

// Runtime returns the local runtime of a session.
func (m *Manager) Runtime(sid uuid.UUID) (*Runtime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runtimes[sid]
	return r, ok
}

// Draining reports that Drain has begun.
func (m *Manager) Draining() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draining
}

// Close stops every runtime at once and waits for them. It is the hard stop; Drain comes first in
// production and Close is what is left when it did not finish in time.
func (m *Manager) Close() {
	m.cancel()
	m.rwg.Wait()
	m.bg.Wait()
}

// reserve takes a slot for a new start; the code of the refusal when the pod is draining or at
// REALTIME_MAX_SESSIONS (4503).
func (m *Manager) reserve() errs.Code { return m.reserveUpTo(m.d.Limits.Max) }

// reserveUpTo takes a slot unless the pod is draining or already holds limit sessions: Max for a
// new start, HardMax for a takeover (a session that already exists may still be resumed up to
// there, §7.12.2). It returns "" when the slot is taken. A draining pod admits nobody: a runtime
// registered after Drain took its snapshot would never hear the drain.
func (m *Manager) reserveUpTo(limit int) errs.Code {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.draining:
		return errs.CodeServerDraining
	case len(m.runtimes)+m.pending >= limit:
		return errs.CodeServerAtCapacity
	}
	m.pending++
	return ""
}

func (m *Manager) unreserve() {
	m.mu.Lock()
	m.pending--
	m.notifyLocked()
	m.mu.Unlock()
}

// register turns a reserved slot into the runtime r. It reports whether the pod began draining
// meanwhile: the caller then tells the runtime (Drain's snapshot did not include it).
func (m *Manager) register(r *Runtime) (draining bool) {
	m.mu.Lock()
	m.pending--
	m.runtimes[r.sid] = r
	draining = m.draining
	m.notifyLocked()
	m.mu.Unlock()
	m.rwg.Add(1)
	return draining
}

func (m *Manager) onRuntimeExit(r *Runtime) {
	m.mu.Lock()
	if m.runtimes[r.sid] == r {
		delete(m.runtimes, r.sid)
	}
	m.notifyLocked()
	m.mu.Unlock()
	m.rwg.Done()
}

// notifyLocked wakes the waiters of changed; the caller holds m.mu.
func (m *Manager) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// ---- podinfo (§7.9.1) ----

// PodInfoLoop writes kotoki:realtime:podinfo:{pod} now and every 5 s (TTL 15 s) until ctx ends or
// the Manager is closed: active_sessions, draining, started_at, build (the dashboards read it).
// A failed write is logged and tried again at the next interval.
func (m *Manager) PodInfoLoop(ctx context.Context) {
	tk := m.d.NewTicker(podInfoInterval)
	defer tk.Stop()
	for {
		m.writePodInfo(ctx)
		select {
		case <-tk.C():
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Manager) writePodInfo(ctx context.Context) {
	if m.d.Store == nil {
		return
	}
	info := rtstate.PodInfo{ActiveSessions: m.ActiveSessions(), Draining: m.Draining(), StartedAt: m.started, Build: m.d.Build}
	wctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	if err := m.d.Store.WritePodInfo(wctx, m.d.Cfg.PodID, info, podInfoTTL); err != nil {
		m.d.Log.Warn("podinfo not written", "pod", m.d.Cfg.PodID, "error", err.Error())
	}
}
