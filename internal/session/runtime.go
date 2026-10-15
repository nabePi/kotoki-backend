package session

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RuntimeService is the fenced facade the realtime runtime talks to (spec 5.2, 7.13.1): thin
// adapters over Service with the signatures conversation.SessionRuntime declares. Every method is
// idempotent and fenced; all transitions are attributed to REALTIME.
type RuntimeService struct {
	s    *Service
	repo Repository
}

// NewRuntimeService builds the facade; repo serves the read-only GetForAttach.
func NewRuntimeService(s *Service, repo Repository) *RuntimeService {
	return &RuntimeService{s: s, repo: repo}
}

// GetForAttach reads what the runtime needs to attach to a session, whatever its status.
func (r *RuntimeService) GetForAttach(ctx context.Context, sessionID uuid.UUID) (RuntimeRecord, error) {
	rec, err := r.repo.GetRuntimeRecord(ctx, sessionID)
	if err != nil {
		return RuntimeRecord{}, mapNotFound(err)
	}
	return rec, nil
}

// ClaimRuntime takes the session under a new fence (spec 7.9.3).
func (r *RuntimeService) ClaimRuntime(ctx context.Context, sessionID uuid.UUID, allowed []Status) (int64, RuntimeRecord, error) {
	fence, sess, err := r.s.ClaimRuntime(ctx, sessionID, allowed)
	if err != nil {
		return 0, RuntimeRecord{}, err
	}
	return fence, RuntimeRecordOf(sess), nil
}

// MarkStarting is RESERVED → STARTING (BeginStart). The record carries opening_prompt_version_id:
// the runtime uses it as it is and never looks up an OPENING version itself (S-2).
func (r *RuntimeService) MarkStarting(ctx context.Context, sessionID uuid.UUID, fence int64) (RuntimeRecord, error) {
	sess, err := r.s.BeginStart(ctx, sessionID, fence)
	if err != nil {
		return RuntimeRecord{}, err
	}
	return RuntimeRecordOf(sess), nil
}

// Activate is STARTING → ACTIVE and returns the clock of the session.
func (r *RuntimeService) Activate(ctx context.Context, sessionID uuid.UUID, fence int64) (time.Time, time.Time, error) {
	sess, err := r.s.Activate(ctx, sessionID, fence)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return *sess.StartedAt, *sess.ExpiresAt, nil
}

// settlement is what the runtime learns about a finished session; times of a session that never
// started are zero.
func settlement(s Session) Settlement {
	out := Settlement{ChargedSeconds: s.ChargedSeconds, DisconnectedSeconds: s.DisconnectedSeconds}
	if s.StartedAt != nil {
		out.StartedAt = *s.StartedAt
	}
	if s.EndedAt != nil {
		out.EndedAt = *s.EndedAt
	}
	if s.EndReason != nil {
		out.EndReason = *s.EndReason
	}
	return out
}

// FailStart is STARTING → FAILED with nothing charged.
func (r *RuntimeService) FailStart(ctx context.Context, sessionID uuid.UUID, fence int64, reason EndReason) (Settlement, error) {
	sess, err := r.s.FailStart(ctx, sessionID, &fence, reason, "", ActorRealtime)
	if err != nil {
		return Settlement{}, err
	}
	return settlement(sess), nil
}

// AddDisconnected adds a disconnect episode (spec 5.6.5).
func (r *RuntimeService) AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) error {
	_, err := r.s.AddDisconnected(ctx, sessionID, fence, seconds)
	return err
}

// MarkHandoff records the start of a drain handoff (S-10).
func (r *RuntimeService) MarkHandoff(ctx context.Context, sessionID uuid.UUID, fence int64) error {
	_, err := r.s.MarkHandoff(ctx, sessionID, fence)
	return err
}

// BeginEnding is ACTIVE → ENDING; a session already ENDING is left as it is.
func (r *RuntimeService) BeginEnding(ctx context.Context, sessionID uuid.UUID, fence int64, reason EndReason) error {
	_, err := r.s.BeginEnding(ctx, sessionID, &fence, reason, "", ActorRealtime)
	return err
}

// Complete is ENDING → COMPLETED (or FAILED, when the stored end_reason is a failure): it settles.
func (r *RuntimeService) Complete(ctx context.Context, sessionID uuid.UUID, fence int64) (Settlement, error) {
	sess, err := r.s.Finalize(ctx, sessionID, &fence, ActorRealtime)
	if err != nil {
		return Settlement{}, err
	}
	return settlement(sess), nil
}

// Fail is ENDING → FAILED. The terminal status follows the end_reason stored by the first
// BeginEnding; a different reason here is only validated: it is logged and the stored one is used.
func (r *RuntimeService) Fail(ctx context.Context, sessionID uuid.UUID, fence int64, reason EndReason) (Settlement, error) {
	sess, err := r.s.Finalize(ctx, sessionID, &fence, ActorRealtime)
	if err != nil {
		return Settlement{}, err
	}
	if sess.EndReason != nil && *sess.EndReason != reason {
		r.s.d.Log.WarnContext(ctx, "session.fail_reason_mismatch", "session_id", sessionID.String(), "stored", string(*sess.EndReason), "given", string(reason))
	}
	return settlement(sess), nil
}
