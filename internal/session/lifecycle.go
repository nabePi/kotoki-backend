package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// TransitionError is ErrInvalidTransition with the statuses involved: From is where the session
// actually was, To where the caller wanted it.
type TransitionError struct{ From, To Status }

// Error implements error.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid session transition %s → %s", e.From, e.To)
}

// Unwrap lets errors.Is(err, ErrInvalidTransition) hold.
func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

func notFound() error { return errs.Wrap(ErrNotFound, errs.CodeSessionNotFound, "session not found") }

func mapNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	return err
}

func ptrOf[T any](v T) *T { return &v }

// releaseSpec describes one of the three release transitions of spec 4.6.2.
type releaseSpec struct {
	from, to      Status
	reason        EndReason
	detail        string
	actor         Actor
	event         EventType
	fence         Fence // non-nil only for FailStart by the runtime
	pastDeadline  bool  // ABANDONED: only once start_deadline_at has passed
	failureDetail bool
}

func (s *Service) appendEvent(ctx context.Context, tx Tx, sessionID uuid.UUID, typ EventType, actor Actor, from, to Status, at time.Time, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "encode session event payload failed")
	}
	return tx.Events().Append(ctx, Event{ID: s.d.IDs.New(), SessionID: sessionID, Type: typ, Actor: actor, FromStatus: &from, ToStatus: &to, Payload: raw, CreatedAt: at})
}

func balanceErr(err error) error {
	switch {
	case errors.Is(err, balance.ErrReservationState):
		return errs.Wrap(err, errs.CodeInternalError, "reservation_state_mismatch")
	case errors.Is(err, balance.ErrReservationNotFound):
		return errs.Wrap(err, errs.CodeInternalError, "reservation missing for the session")
	}
	return err
}

// releaseInTx is steps 1–7 of spec 4.6.2 inside the caller's transaction. It returns the session,
// whether it changed anything (false: idempotent no-op, or ABANDONED before the deadline) and an
// error. Lock order: balance account → session → reservation.
func (s *Service) releaseInTx(ctx context.Context, tx Tx, id uuid.UUID, sp releaseSpec) (Session, bool, error) {
	owner, err := tx.Sessions().OwnerOf(ctx, id)
	if err != nil {
		return Session{}, false, mapNotFound(err)
	}
	acc, err := tx.Balance().LockAccountByUserID(ctx, owner)
	if err != nil {
		return Session{}, false, errs.Wrap(err, errs.CodeInternalError, "lock balance account failed")
	}
	sess, err := tx.Sessions().GetForUpdate(ctx, id)
	if err != nil {
		return Session{}, false, mapNotFound(err)
	}
	switch {
	case sess.Status == sp.to:
		return sess, false, nil
	case sess.Status != sp.from:
		return Session{}, false, &TransitionError{From: sess.Status, To: sp.to}
	}
	now := s.d.Clock.Now()
	if sp.pastDeadline {
		res, err := tx.Balance().LockReservationBySessionID(ctx, id)
		if err != nil {
			return Session{}, false, balanceErr(err)
		}
		if !now.After(res.StartDeadlineAt) {
			return sess, false, nil
		}
	}
	released, err := s.d.Balance.ReleaseReservation(ctx, tx, acc, id, now)
	if err != nil {
		return Session{}, false, balanceErr(err)
	}
	patch := Patch{To: sp.to, At: now, EndReason: &sp.reason, EndedAt: &now, ChargedSeconds: ptrOf(0), BumpFence: true}
	if sp.failureDetail {
		patch.FailureDetail = &sp.detail
	}
	updated, ok, err := tx.Sessions().CAS(ctx, id, sp.from, sp.fence, patch)
	if err != nil {
		return Session{}, false, err
	}
	if !ok {
		// the row lock is ours, so only the fence can differ
		return Session{}, false, ErrFenceLost
	}
	payload := map[string]any{"released_seconds": released}
	if sp.failureDetail {
		payload["failure_detail"] = sp.detail
	}
	if err := s.appendEvent(ctx, tx, id, sp.event, sp.actor, sp.from, sp.to, now, payload); err != nil {
		return Session{}, false, err
	}
	return updated, true, nil
}

// releaseTx runs releaseInTx in its own transaction and, once it committed a change, tells the
// runtime and counts the end.
func (s *Service) releaseTx(ctx context.Context, id uuid.UUID, sp releaseSpec) (Session, error) {
	var out Session
	var changed bool
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		out, changed, err = s.releaseInTx(ctx, tx, id, sp)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if changed {
		s.afterEnd(ctx, out, true)
	}
	return out, nil
}

// afterEnd publishes the control message (when the transition committed) and counts the end.
func (s *Service) afterEnd(ctx context.Context, sess Session, publish bool) {
	if publish {
		s.publishControl(ctx, sess)
	}
	if sess.EndReason != nil {
		s.d.Metrics.Ended(s.d.ServiceName, string(*sess.EndReason), string(sess.Status))
	}
}

// Cancel is RESERVED → CANCELLED by the owner: the whole reservation goes back. A session of
// another user does not exist for the caller.
func (s *Service) Cancel(ctx context.Context, userID, sessionID uuid.UUID) (Session, error) {
	owner, err := s.d.Reader.OwnerOf(ctx, sessionID)
	if err != nil {
		return Session{}, mapNotFound(err)
	}
	if owner != userID {
		return Session{}, notFound()
	}
	sess, err := s.releaseTx(ctx, sessionID, releaseSpec{from: StatusReserved, to: StatusCancelled, reason: EndUserCancelled, actor: ActorUser, event: EventCancelled})
	var te *TransitionError
	if errors.As(err, &te) {
		return Session{}, errs.WithDetails(errs.Wrap(err, errs.CodeSessionNotCancellable, "session cannot be cancelled"), map[string]any{"status": string(te.From)})
	}
	return sess, err
}

// CancelInTx is the same cancellation for admin.Service inside its own transaction (account
// deletion, spec 3.13.3): the caller commits, so no control message is published from here.
func (s *Service) CancelInTx(ctx context.Context, tx Tx, sessionID uuid.UUID, actor Actor) (Session, error) {
	sess, changed, err := s.releaseInTx(ctx, tx, sessionID, releaseSpec{from: StatusReserved, to: StatusCancelled, reason: EndUserCancelled, actor: actor, event: EventCancelled})
	if err != nil {
		return Session{}, err
	}
	if changed {
		s.afterEnd(ctx, sess, false)
	}
	return sess, nil
}

// Abandon is RESERVED → ABANDONED once start_deadline_at has passed (job reservation_cleanup, or
// BeginStart when the start comes too late). Before the deadline it changes nothing and returns
// the session as it is.
func (s *Service) Abandon(ctx context.Context, sessionID uuid.UUID, actor Actor) (Session, error) {
	return s.releaseTx(ctx, sessionID, releaseSpec{from: StatusReserved, to: StatusAbandoned, reason: EndNotStarted, actor: actor, event: EventAbandoned, pastDeadline: true})
}

// FailStart is STARTING → FAILED with nothing charged. The runtime passes its fence, the sweeper nil.
func (s *Service) FailStart(ctx context.Context, sessionID uuid.UUID, fence Fence, r EndReason, detail string, actor Actor) (Session, error) {
	if TerminalStatusFor(r) != StatusFailed {
		return Session{}, errs.New(errs.CodeInternalError, "FailStart needs a failure end reason")
	}
	return s.releaseTx(ctx, sessionID, releaseSpec{from: StatusStarting, to: StatusFailed, reason: r, detail: detail, actor: actor, event: EventFailed, fence: fence, failureDetail: true})
}

// BeginStart is RESERVED → STARTING under the runtime's fence (T2). For AI_FIRST it records the
// PUBLISHED OPENING prompt version, or NULL when there is none: that is not an error, the runtime
// speaks the scenario's opening_line instead (S-2). OPENING is read before the transaction.
func (s *Service) BeginStart(ctx context.Context, sessionID uuid.UUID, fence int64) (Session, error) {
	opening, err := s.d.OpeningPrompts.PublishedPromptVersionID(ctx, "OPENING")
	if err != nil {
		return Session{}, errs.Wrap(err, errs.CodeInternalError, "read OPENING prompt failed")
	}
	var out Session
	late := false
	err = s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		out, late = Session{}, false
		sess, err := tx.Sessions().GetForUpdate(ctx, sessionID)
		if err != nil {
			return mapNotFound(err)
		}
		if sess.Status == StatusStarting && sess.RuntimeFence == fence {
			out = sess
			return nil
		}
		if sess.Status != StatusReserved {
			return &TransitionError{From: sess.Status, To: StatusStarting}
		}
		if sess.RuntimeFence != fence {
			return ErrFenceLost
		}
		res, err := tx.Balance().LockReservationBySessionID(ctx, sessionID)
		if err != nil {
			return balanceErr(err)
		}
		now := s.d.Clock.Now()
		if now.After(res.StartDeadlineAt) {
			late = true
			return nil
		}
		patch := Patch{To: StatusStarting, At: now, StartingAt: &now}
		if sess.ConversationStartMode == StartAIFirst {
			patch.OpeningPromptVersionID = opening
		}
		updated, ok, err := tx.Sessions().CAS(ctx, sessionID, StatusReserved, &fence, patch)
		if err != nil {
			return err
		}
		if !ok {
			return ErrFenceLost
		}
		if err := s.appendEvent(ctx, tx, sessionID, EventStarting, ActorRealtime, StatusReserved, StatusStarting, now, map[string]any{"opening_prompt_version_id": updated.OpeningPromptVersionID}); err != nil {
			return err
		}
		if sess.ConversationStartMode == StartAIFirst && opening == nil {
			s.d.Log.WarnContext(ctx, "session.opening_prompt_missing", "session_id", sessionID.String())
		}
		out = updated
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if late {
		if _, err := s.Abandon(ctx, sessionID, ActorRealtime); err != nil {
			return Session{}, err
		}
		return Session{}, ErrStartDeadlinePassed
	}
	return out, nil
}

// Activate is STARTING → ACTIVE under the runtime's fence (T3): started_at is now and expires_at is
// started_at + duration. When STARTING lasted longer than StartTimeout the session is failed
// instead (PROVIDER_FAILURE, nothing charged) and ErrStartTimeout comes back.
func (s *Service) Activate(ctx context.Context, sessionID uuid.UUID, fence int64) (Session, error) {
	var out Session
	timedOut := false
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		out, timedOut = Session{}, false
		sess, err := tx.Sessions().GetForUpdate(ctx, sessionID)
		if err != nil {
			return mapNotFound(err)
		}
		if sess.Status == StatusActive && sess.RuntimeFence == fence {
			out = sess
			return nil
		}
		if sess.Status != StatusStarting {
			return &TransitionError{From: sess.Status, To: StatusActive}
		}
		if sess.RuntimeFence != fence {
			return ErrFenceLost
		}
		now := s.d.Clock.Now()
		if sess.StartingAt != nil && now.Sub(*sess.StartingAt) > StartTimeout {
			timedOut = true
			return nil
		}
		expires := now.Add(time.Duration(sess.DurationSeconds) * time.Second)
		updated, ok, err := tx.Sessions().CAS(ctx, sessionID, StatusStarting, &fence, Patch{To: StatusActive, At: now, StartedAt: &now, ExpiresAt: &expires})
		if err != nil {
			return err
		}
		if !ok {
			return ErrFenceLost
		}
		if err := s.appendEvent(ctx, tx, sessionID, EventStarted, ActorRealtime, StatusStarting, StatusActive, now, map[string]any{"expires_at": expires}); err != nil {
			return err
		}
		out = updated
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if timedOut {
		if _, err := s.FailStart(ctx, sessionID, &fence, EndProviderFailure, "start_timeout", ActorRealtime); err != nil {
			return Session{}, err
		}
		return Session{}, ErrStartTimeout
	}
	return out, nil
}
