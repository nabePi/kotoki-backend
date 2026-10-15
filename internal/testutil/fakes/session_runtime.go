package fakes

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// rtSession is the state the fake keeps per session on top of the seeded record.
type rtSession struct {
	rec        session.RuntimeRecord
	endReason  session.EndReason
	endingAt   time.Time
	disc       int
	handoffAt  *time.Time
	settlement *session.Settlement
}

// SessionRuntime is an in-memory fenced session facade with the contract of
// *session.RuntimeService (spec 7.13.1): transitions follow session.CanTransition, ClaimRuntime
// raises the fence, a call with another fence is session.ErrFenceLost, repeating a finished step is
// a no-op. Safe for concurrent use; FailNext("<Method>", err) fails the next call of a method.
type SessionRuntime struct {
	FailInjector
	// Recorder, when set, receives the name of every method called (before the armed failure
	// and the fence check), so a test can assert the order of transitions.
	Recorder *CallRecorder
	clk      clock.Clock
	mu       sync.Mutex
	sessions map[uuid.UUID]*rtSession
	claims   [][]session.Status
}

// NewSessionRuntime returns an empty fake driven by clk.
func NewSessionRuntime(clk clock.Clock) *SessionRuntime {
	return &SessionRuntime{clk: clk, sessions: map[uuid.UUID]*rtSession{}}
}

// Seed adds or replaces a session.
func (r *SessionRuntime) Seed(rec session.RuntimeRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[rec.ID] = &rtSession{rec: rec, disc: rec.DisconnectedSeconds}
}

// Claims returns the allowed-status lists of the ClaimRuntime calls so far.
func (r *SessionRuntime) Claims() [][]session.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]session.Status(nil), r.claims...)
}

// Fence returns runtime_fence of a session (0 when unknown), the number the fenced writes of the
// other fakes compare against.
func (r *SessionRuntime) Fence(id uuid.UUID) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		return s.rec.RuntimeFence
	}
	return 0
}

// Status returns the status of a session, for assertions.
func (r *SessionRuntime) Status(id uuid.UUID) session.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		return s.rec.Status
	}
	return ""
}

// Disconnected returns disconnected_seconds of a session, for assertions.
func (r *SessionRuntime) Disconnected(id uuid.UUID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		return s.disc
	}
	return 0
}

// HandoffAt returns handoff_at of a session (nil: not handed off).
func (r *SessionRuntime) HandoffAt(id uuid.UUID) *time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[id]; s != nil {
		return s.handoffAt
	}
	return nil
}

// get returns the session after the armed failure and the fence check; fence < 0 skips the check.
// The caller holds r.mu.
func (r *SessionRuntime) get(method string, id uuid.UUID, fence int64) (*rtSession, error) {
	r.Recorder.Record(method)
	if err := r.Take(method); err != nil {
		return nil, err
	}
	s := r.sessions[id]
	if s == nil {
		return nil, session.ErrNotFound
	}
	if fence >= 0 && s.rec.RuntimeFence != fence {
		return nil, session.ErrFenceLost
	}
	return s, nil
}

func invalid(from, to session.Status) error {
	return &transitionError{from: from, to: to}
}

type transitionError struct{ from, to session.Status }

func (e *transitionError) Error() string {
	return "invalid session transition " + string(e.from) + " -> " + string(e.to)
}
func (e *transitionError) Is(target error) bool { return target == session.ErrInvalidTransition }

// GetForAttach implements conversation.SessionRuntime.
func (r *SessionRuntime) GetForAttach(_ context.Context, id uuid.UUID) (session.RuntimeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("GetForAttach", id, -1)
	if err != nil {
		return session.RuntimeRecord{}, err
	}
	return s.rec, nil
}

// ClaimRuntime implements conversation.SessionRuntime: the fence rises by one.
func (r *SessionRuntime) ClaimRuntime(_ context.Context, id uuid.UUID, allowed []session.Status) (int64, session.RuntimeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims = append(r.claims, append([]session.Status(nil), allowed...))
	s, err := r.get("ClaimRuntime", id, -1)
	if err != nil {
		return 0, session.RuntimeRecord{}, err
	}
	ok := false
	for _, a := range allowed {
		ok = ok || a == s.rec.Status
	}
	if !ok {
		return 0, session.RuntimeRecord{}, invalid(s.rec.Status, "")
	}
	s.rec.RuntimeFence++
	s.handoffAt = nil
	return s.rec.RuntimeFence, s.rec, nil
}

// MarkStarting implements conversation.SessionRuntime (RESERVED → STARTING; STARTING is a no-op).
func (r *SessionRuntime) MarkStarting(_ context.Context, id uuid.UUID, fence int64) (session.RuntimeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("MarkStarting", id, fence)
	if err != nil {
		return session.RuntimeRecord{}, err
	}
	if s.rec.Status == session.StatusStarting {
		return s.rec, nil
	}
	if !session.CanTransition(s.rec.Status, session.StatusStarting) {
		return session.RuntimeRecord{}, invalid(s.rec.Status, session.StatusStarting)
	}
	s.rec.Status = session.StatusStarting
	return s.rec, nil
}

// Activate implements conversation.SessionRuntime (STARTING → ACTIVE; ACTIVE returns the same clock).
func (r *SessionRuntime) Activate(_ context.Context, id uuid.UUID, fence int64) (time.Time, time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("Activate", id, fence)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if s.rec.Status != session.StatusActive {
		if !session.CanTransition(s.rec.Status, session.StatusActive) {
			return time.Time{}, time.Time{}, invalid(s.rec.Status, session.StatusActive)
		}
		now := r.clk.Now()
		exp := now.Add(time.Duration(s.rec.DurationSeconds) * time.Second)
		s.rec.Status, s.rec.StartedAt, s.rec.ExpiresAt = session.StatusActive, &now, &exp
	}
	return *s.rec.StartedAt, *s.rec.ExpiresAt, nil
}

// settle ends a session in terminal and computes what it was charged. The caller holds r.mu.
func (r *SessionRuntime) settle(s *rtSession, terminal session.Status) session.Settlement {
	now := r.clk.Now()
	st := session.Settlement{DisconnectedSeconds: s.disc, EndedAt: now, EndReason: s.endReason}
	if s.rec.StartedAt != nil {
		st.StartedAt = *s.rec.StartedAt
		endingAt := s.endingAt
		if endingAt.IsZero() {
			endingAt = now
		}
		elapsed := int(math.Ceil(endingAt.Sub(*s.rec.StartedAt).Seconds()))
		st.ChargedSeconds = max(0, min(s.rec.DurationSeconds, elapsed-s.disc))
	}
	s.rec.Status = terminal
	s.settlement = &st
	return st
}

// FailStart implements conversation.SessionRuntime (STARTING → FAILED, nothing charged).
func (r *SessionRuntime) FailStart(_ context.Context, id uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("FailStart", id, fence)
	if err != nil {
		return session.Settlement{}, err
	}
	if s.settlement != nil && s.rec.Status == session.StatusFailed {
		return *s.settlement, nil
	}
	if s.rec.Status != session.StatusStarting {
		return session.Settlement{}, invalid(s.rec.Status, session.StatusFailed)
	}
	s.endReason = reason
	st := r.settle(s, session.StatusFailed)
	st.ChargedSeconds = 0
	s.settlement = &st
	return st, nil
}

// AddDisconnected implements conversation.SessionRuntime: seconds accumulate.
func (r *SessionRuntime) AddDisconnected(_ context.Context, id uuid.UUID, fence int64, seconds int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("AddDisconnected", id, fence)
	if err != nil {
		return err
	}
	if s.rec.Status != session.StatusActive && s.rec.Status != session.StatusEnding {
		return invalid(s.rec.Status, s.rec.Status)
	}
	s.disc += seconds
	s.rec.DisconnectedSeconds = s.disc
	return nil
}

// MarkHandoff implements conversation.SessionRuntime (handoff_at = now; not a transition).
func (r *SessionRuntime) MarkHandoff(_ context.Context, id uuid.UUID, fence int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("MarkHandoff", id, fence)
	if err != nil {
		return err
	}
	now := r.clk.Now()
	s.handoffAt = &now
	return nil
}

// BeginEnding implements conversation.SessionRuntime (ACTIVE → ENDING; ENDING is a no-op).
func (r *SessionRuntime) BeginEnding(_ context.Context, id uuid.UUID, fence int64, reason session.EndReason) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("BeginEnding", id, fence)
	if err != nil {
		return err
	}
	if s.rec.Status == session.StatusEnding {
		return nil
	}
	if !session.CanTransition(s.rec.Status, session.StatusEnding) {
		return invalid(s.rec.Status, session.StatusEnding)
	}
	s.rec.Status, s.endReason, s.endingAt = session.StatusEnding, reason, r.clk.Now()
	return nil
}

// Complete implements conversation.SessionRuntime (ENDING → COMPLETED, or FAILED when the stored
// end reason is a failure; a finished session answers its settlement again).
func (r *SessionRuntime) Complete(_ context.Context, id uuid.UUID, fence int64) (session.Settlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("Complete", id, fence)
	if err != nil {
		return session.Settlement{}, err
	}
	if s.settlement != nil && s.rec.Status.Terminal() {
		return *s.settlement, nil
	}
	to := session.TerminalStatusFor(s.endReason)
	if to != session.StatusCompleted && to != session.StatusFailed {
		to = session.StatusCompleted
	}
	if s.rec.Status != session.StatusEnding || !session.CanTransition(s.rec.Status, to) {
		return session.Settlement{}, invalid(s.rec.Status, to)
	}
	return r.settle(s, to), nil
}

// Fail implements conversation.SessionRuntime (ENDING → FAILED).
func (r *SessionRuntime) Fail(_ context.Context, id uuid.UUID, fence int64, _ session.EndReason) (session.Settlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.get("Fail", id, fence)
	if err != nil {
		return session.Settlement{}, err
	}
	if s.settlement != nil && s.rec.Status == session.StatusFailed {
		return *s.settlement, nil
	}
	if s.rec.Status != session.StatusEnding {
		return session.Settlement{}, invalid(s.rec.Status, session.StatusFailed)
	}
	return r.settle(s, session.StatusFailed), nil
}
