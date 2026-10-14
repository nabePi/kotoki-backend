package session

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// ClaimRuntime hands the session to the calling runtime: it raises runtime_fence and ends a
// handoff, only while the status is one of allowed (RESERVED to start, ACTIVE to resume). The
// returned fence is what the runtime must pass to every later call.
func (s *Service) ClaimRuntime(ctx context.Context, sessionID uuid.UUID, allowed []Status) (int64, Session, error) {
	var out Session
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, err := tx.Sessions().GetByID(ctx, sessionID); err != nil {
			return mapNotFound(err)
		}
		var err error
		out, err = tx.Sessions().Claim(ctx, sessionID, allowed)
		return err
	})
	if err != nil {
		return 0, Session{}, err
	}
	return out.RuntimeFence, out, nil
}

// whyNoRow explains a fenced write that matched no row: the session is gone, in the wrong status
// (checked first, like the spec's ErrInvalidTransition) or held by another fence.
func (s *Service) whyNoRow(ctx context.Context, id uuid.UUID, fence int64, to Status, statuses ...Status) error {
	cur, err := s.d.Reader.GetByID(ctx, id)
	if err != nil {
		return mapNotFound(err)
	}
	for _, st := range statuses {
		if cur.Status == st {
			if cur.RuntimeFence != fence {
				return ErrFenceLost
			}
			return errs.New(errs.CodeInternalError, "fenced session write matched no row although status and fence match")
		}
	}
	return &TransitionError{From: cur.Status, To: to}
}

// AddDisconnected adds seconds of a disconnect episode to disconnected_seconds (spec 5.6.5); it is
// not a transition.
func (s *Service) AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) (Session, error) {
	if seconds < 0 {
		return Session{}, validationError("seconds", "out_of_range")
	}
	var out Session
	var ok bool
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		out, ok, err = tx.Sessions().AddDisconnected(ctx, sessionID, fence, seconds, s.d.Clock.Now())
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, s.whyNoRow(ctx, sessionID, fence, StatusActive, StatusActive, StatusEnding)
	}
	return out, nil
}

// MarkHandoff records that the owning runtime is draining (handoff_at = now); not a transition,
// and neither version nor runtime_fence move (S-10).
func (s *Service) MarkHandoff(ctx context.Context, sessionID uuid.UUID, fence int64) (Session, error) {
	var ok bool
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		ok, err = tx.Sessions().MarkHandoff(ctx, sessionID, fence, s.d.Clock.Now())
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, s.whyNoRow(ctx, sessionID, fence, StatusActive, StatusActive)
	}
	return s.d.Reader.GetByID(ctx, sessionID)
}

// SetSummary stores the summary of an ended session that was ACTIVE; it never overwrites one and
// is not a transition (Bab 9 §9.9.6).
func (s *Service) SetSummary(ctx context.Context, sessionID uuid.UUID, text string) error {
	cur, err := s.d.Reader.GetByID(ctx, sessionID)
	if err != nil {
		return mapNotFound(err)
	}
	if (cur.Status != StatusCompleted && cur.Status != StatusFailed) || cur.StartedAt == nil {
		return &TransitionError{From: cur.Status, To: cur.Status}
	}
	if cur.SummaryText != nil {
		return nil
	}
	return s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		return mapNotFound(tx.Sessions().SetSummary(ctx, sessionID, text, s.d.Clock.Now()))
	})
}

func endReasonOK(r EndReason) bool {
	switch r {
	case EndUserEnded, EndDurationElapsed, EndReconnectTimeout, EndProviderFailure, EndServerError:
		return true
	}
	return false
}

// BeginEnding is ACTIVE → ENDING (T7). ending_at, the base of billable_end, is handoff_at when the
// session was handed off and now otherwise (D-036). The first end_reason wins: a session that is
// already ENDING (or finished) is returned as it is. A non-runtime caller (fence nil) raises the
// fence so a live runtime stops.
func (s *Service) BeginEnding(ctx context.Context, sessionID uuid.UUID, fence Fence, r EndReason, detail string, actor Actor) (Session, error) {
	sess, _, err := s.beginEnding(ctx, sessionID, fence, r, detail, actor, nil)
	return sess, err
}

// EndStaleHandoff is rule (e) of the sweeper: BeginEnding(RECONNECT_TIMEOUT) for a session whose
// handoff began before the cutoff and has not been taken over since. The condition is re-checked
// under the row lock; when a new runtime has claimed the session in the meantime nothing changes and
// changed is false.
func (s *Service) EndStaleHandoff(ctx context.Context, sessionID uuid.UUID, handoffBefore time.Time) (Session, bool, error) {
	return s.beginEnding(ctx, sessionID, nil, EndReconnectTimeout, "handoff_not_resumed", ActorSweeper, func(sess Session) bool {
		return sess.HandoffAt != nil && sess.HandoffAt.Before(handoffBefore)
	})
}

// beginEnding is BeginEnding with an optional extra guard evaluated on an ACTIVE session under the
// row lock; changed tells whether this call made the transition.
func (s *Service) beginEnding(ctx context.Context, sessionID uuid.UUID, fence Fence, r EndReason, detail string, actor Actor, onlyIf func(Session) bool) (Session, bool, error) {
	if !endReasonOK(r) {
		return Session{}, false, validationError("end_reason", "not_allowed")
	}
	var out Session
	changed := false
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		out, changed = Session{}, false
		sess, err := tx.Sessions().GetForUpdate(ctx, sessionID)
		if err != nil {
			return mapNotFound(err)
		}
		if sess.Status == StatusEnding || sess.Status == StatusCompleted || sess.Status == StatusFailed {
			out = sess
			return nil
		}
		if sess.Status != StatusActive {
			return &TransitionError{From: sess.Status, To: StatusEnding}
		}
		if fence != nil && sess.RuntimeFence != *fence {
			return ErrFenceLost
		}
		if onlyIf != nil && !onlyIf(sess) {
			out = sess
			return nil
		}
		now := s.d.Clock.Now()
		endingAt := now
		if sess.HandoffAt != nil {
			endingAt = *sess.HandoffAt
		}
		patch := Patch{To: StatusEnding, At: now, EndReason: &r, EndingAt: &endingAt, BumpFence: fence == nil}
		if detail != "" {
			patch.FailureDetail = &detail
		}
		updated, ok, err := tx.Sessions().CAS(ctx, sessionID, StatusActive, fence, patch)
		if err != nil {
			return err
		}
		if !ok {
			return ErrFenceLost
		}
		if err := s.appendEvent(ctx, tx, sessionID, EventEnding, actor, StatusActive, StatusEnding, now, map[string]any{"end_reason": string(r), "detail": detail}); err != nil {
			return err
		}
		out, changed = updated, true
		return nil
	})
	if err != nil {
		return Session{}, false, err
	}
	if changed {
		s.publishControl(ctx, out)
	}
	return out, changed, nil
}

// settleSpec describes the two settle paths of spec 4.6.3.
type settleSpec struct {
	from         Status
	fence        Fence
	actor        Actor
	endedAt      time.Time // Finalize: now; FailActive: LEAST(estimated end, expires_at)
	endingAt     *time.Time
	reason       EndReason
	detail       string
	estimatedEnd *time.Time
}

// settleInTx is steps 3–10 of spec 4.6.3 on a locked account and a locked session.
func (s *Service) settleInTx(ctx context.Context, tx Tx, acc balance.Account, sess Session, sp settleSpec) (Session, error) {
	now := s.d.Clock.Now()
	fault := sp.reason == EndProviderFailure || sp.reason == EndServerError
	res, err := s.d.Balance.SettleReservation(ctx, tx, acc, balance.SettleInput{
		SessionID: sess.ID, StartedAt: *sess.StartedAt, EndedAt: sp.endedAt, ExpiresAt: *sess.ExpiresAt, EndingAt: sp.endingAt,
		DisconnectedSeconds: int64(sess.DisconnectedSeconds), ServiceFault: fault, At: now,
	})
	if err != nil {
		return Session{}, balanceErr(err)
	}
	if _, err := tx.Turns().FailDangling(ctx, sess.ID, *sess.StartingAt, now); err != nil {
		return Session{}, err
	}
	to := TerminalStatusFor(sp.reason)
	charged := int(res.Charged)
	patch := Patch{To: to, At: now, EndReason: &sp.reason, EndedAt: &sp.endedAt, ChargedSeconds: &charged, BumpFence: true}
	if sp.detail != "" {
		patch.FailureDetail = &sp.detail
	}
	updated, ok, err := tx.Sessions().CAS(ctx, sess.ID, sp.from, sp.fence, patch)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, ErrFenceLost
	}

	payload := map[string]any{
		"charged_seconds": res.Charged, "released_seconds": res.Released, "active_ms": res.ActiveMs,
		"billable_end": res.BillableEnd, "disconnected_seconds": sess.DisconnectedSeconds,
	}
	if sp.estimatedEnd != nil {
		payload["estimated_end"] = *sp.estimatedEnd
	}
	typ := EventCompleted
	if to == StatusFailed {
		typ = EventFailed
	}
	if err := s.appendEvent(ctx, tx, sess.ID, typ, sp.actor, sp.from, to, now, payload); err != nil {
		return Session{}, err
	}

	turns, userTurns, _, err := tx.Turns().Stats(ctx, sess.ID, *sess.StartingAt)
	if err != nil {
		return Session{}, err
	}
	ev := events.NewSessionCompleted(s.d.IDs.New(), now, s.d.Producer(sp.fence), sess.UserID, events.SessionCompletedPayload{
		SessionID: sess.ID, FinalStatus: string(to), EndReason: string(sp.reason), ConversationStartMode: string(sess.ConversationStartMode),
		TopicID: sess.TopicID, ScenarioID: sess.ScenarioID, ScenarioVersionID: sess.ScenarioVersionID, CharacterID: sess.CharacterID,
		PromptVersionID: sess.PromptVersionID, DurationSeconds: sess.DurationSeconds, StartedAt: *sess.StartedAt, EndedAt: sp.endedAt,
		ActiveSeconds: events.ActiveSeconds(res.ActiveMs), ChargedSeconds: charged, TurnCount: turns, UserTurnCount: userTurns,
	})
	if err := tx.Outbox().Enqueue(ctx, ev); err != nil {
		return Session{}, err
	}
	return updated, nil
}

// settleTx locks account → session, applies the guards of spec 4.6.3 step 2 and settles.
func (s *Service) settleTx(ctx context.Context, sessionID uuid.UUID, from Status, fence Fence, plan func(sess Session) settleSpec) (Session, error) {
	owner, err := s.d.Reader.OwnerOf(ctx, sessionID)
	if err != nil {
		return Session{}, mapNotFound(err)
	}
	var out Session
	changed := false
	err = s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		out, changed = Session{}, false
		acc, err := tx.Balance().LockAccountByUserID(ctx, owner)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternalError, "lock balance account failed")
		}
		sess, err := tx.Sessions().GetForUpdate(ctx, sessionID)
		if err != nil {
			return mapNotFound(err)
		}
		if sess.Status == StatusCompleted || sess.Status == StatusFailed {
			out = sess // the sweeper and the runtime may race: the second one finds it done
			return nil
		}
		if fence != nil && sess.RuntimeFence != *fence {
			return ErrFenceLost
		}
		if sess.Status != from {
			return &TransitionError{From: sess.Status, To: StatusCompleted}
		}
		sp := plan(sess)
		sp.from, sp.fence = from, fence
		out, err = s.settleInTx(ctx, tx, acc, sess, sp)
		changed = err == nil
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

// Finalize is ENDING → COMPLETED | FAILED (T8, T9): the status follows the stored end_reason, the
// charge follows spec 4.5 with billable_end taken from ending_at, and ended_at is now.
func (s *Service) Finalize(ctx context.Context, sessionID uuid.UUID, fence Fence, actor Actor) (Session, error) {
	return s.settleTx(ctx, sessionID, StatusEnding, fence, func(sess Session) settleSpec {
		reason := EndServerError
		if sess.EndReason != nil {
			reason = *sess.EndReason
		}
		return settleSpec{actor: actor, endedAt: s.d.Clock.Now(), endingAt: sess.EndingAt, reason: reason}
	})
}

// FailActive is ACTIVE → FAILED (T10) for the sweeper when the owner's heartbeat is gone:
// end_reason SERVER_ERROR, ended_at = LEAST(estimatedEnd, expires_at) and nothing after it is billed.
func (s *Service) FailActive(ctx context.Context, sessionID uuid.UUID, estimatedEnd time.Time, detail string) (Session, error) {
	return s.settleTx(ctx, sessionID, StatusActive, nil, func(sess Session) settleSpec {
		ended := estimatedEnd
		if sess.ExpiresAt != nil && ended.After(*sess.ExpiresAt) {
			ended = *sess.ExpiresAt
		}
		if sess.StartedAt != nil && ended.Before(*sess.StartedAt) {
			ended = *sess.StartedAt
		}
		est := estimatedEnd
		return settleSpec{actor: ActorSweeper, endedAt: ended, reason: EndServerError, detail: detail, estimatedEnd: &est}
	})
}
