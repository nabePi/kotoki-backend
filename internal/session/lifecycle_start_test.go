package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// reserved creates a RESERVED session for the env's user.
func (e *env) reserved(t *testing.T, key string, mod ...func(*session.CreateSessionInput)) session.Session {
	t.Helper()
	in := e.input(key)
	for _, m := range mod {
		m(&in)
	}
	res, err := e.svc.Create(context.Background(), in)
	require.NoError(t, err)
	return res.Session
}

// claim is what the runtime does first: raise the fence and keep it.
func (e *env) claim(t *testing.T, id uuid.UUID, allowed ...session.Status) int64 {
	t.Helper()
	var fence int64
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx session.Tx) error {
		s, err := tx.Sessions().Claim(ctx, id, allowed)
		fence = s.RuntimeFence
		return err
	}))
	return fence
}

func (e *env) count(entry balance.EntryType) int {
	n := 0
	for _, l := range e.bal.Ledger() {
		if l.EntryType == entry {
			n++
		}
	}
	return n
}

func eventTypes(evs []session.Event) []session.EventType {
	out := make([]session.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func TestCancelReleasesFullReservation(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	got, err := e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.NoError(t, err)
	require.Equal(t, session.StatusCancelled, got.Status)
	require.Equal(t, session.EndUserCancelled, *got.EndReason)
	require.Zero(t, got.ChargedSeconds)
	require.Equal(t, s.RuntimeFence+1, got.RuntimeFence, "a stray runtime holding the old fence stops")
	require.NotNil(t, got.EndedAt)

	acc := e.bal.Account(e.userID)
	require.Equal(t, int64(1800), acc.AvailableSeconds)
	require.Zero(t, acc.ReservedSeconds)
	require.Equal(t, balance.ReservationReleased, e.bal.Reservation(s.ID).Status)
	require.Equal(t, []session.EventType{session.EventReserved, session.EventCancelled}, eventTypes(e.store.Events(s.ID)))
	last := e.store.Events(s.ID)[1]
	require.JSONEq(t, `{"released_seconds":900}`, string(last.Payload))

	sent := e.control.Sent()
	require.Len(t, sent, 1)
	require.Equal(t, "status_changed", sent[0].Msg.Type)
	require.Equal(t, "CANCELLED", sent[0].Msg.Status)
	require.Equal(t, "USER_CANCELLED", sent[0].Msg.EndReason)
	require.Equal(t, got.Version, sent[0].Msg.Version)
}

func TestCancelIdempotent(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	first, err := e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.NoError(t, err)
	second, err := e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.NoError(t, err)
	require.Equal(t, first.Version, second.Version)
	require.Equal(t, 1, e.count(balance.EntryRelease))
	require.Len(t, e.control.Sent(), 1, "a repeat publishes nothing")
	require.Equal(t, [][3]string{{"api", "USER_CANCELLED", "CANCELLED"}}, e.metrics.ended)
}

func TestCancelNotReservedStatusSessionNotCancellable(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	_, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err)

	_, err = e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.Equal(t, errs.CodeSessionNotCancellable, errs.CodeOf(err))
	require.Equal(t, map[string]any{"status": "STARTING"}, errs.DetailsOf(err))
	require.ErrorIs(t, err, session.ErrInvalidTransition)
	require.Equal(t, session.StatusStarting, e.store.Session(s.ID).Status)
}

func TestCancelOtherUsersSession404(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	_, err := e.svc.Cancel(context.Background(), uuid.New(), s.ID)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
	_, err = e.svc.Cancel(context.Background(), e.userID, uuid.New())
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
	require.Equal(t, session.StatusReserved, e.store.Session(s.ID).Status)
}

func TestAbandonBeforeDeadlineSkips(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	e.clk.Advance(5 * time.Minute) // exactly at the deadline is not past it
	got, err := e.svc.Abandon(context.Background(), s.ID, session.ActorCleanup)
	require.NoError(t, err)
	require.Equal(t, session.StatusReserved, got.Status)
	require.Equal(t, balance.ReservationHeld, e.bal.Reservation(s.ID).Status)
	require.Empty(t, e.metrics.ended)
}

func TestAbandonAfterDeadlineReleases(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	e.clk.Advance(5*time.Minute + time.Second)
	got, err := e.svc.Abandon(context.Background(), s.ID, session.ActorCleanup)
	require.NoError(t, err)
	require.Equal(t, session.StatusAbandoned, got.Status)
	require.Equal(t, session.EndNotStarted, *got.EndReason)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
	evs := e.store.Events(s.ID)
	require.Equal(t, session.EventAbandoned, evs[1].Type)
	require.Equal(t, session.ActorCleanup, evs[1].Actor)
	require.Equal(t, [][3]string{{"api", "NOT_STARTED", "ABANDONED"}}, e.metrics.ended)

	again, err := e.svc.Abandon(context.Background(), s.ID, session.ActorCleanup)
	require.NoError(t, err)
	require.Equal(t, got.Version, again.Version)
	require.Equal(t, 1, e.count(balance.EntryRelease))
}

func TestBeginStartAIFirstSelectsPublishedOpening(t *testing.T) {
	e := newEnv(t, 1800)
	opening := uuid.New()
	e.opening.Set(&opening)
	s := e.reserved(t, "key-0000001", func(in *session.CreateSessionInput) { in.ConversationStartMode = session.StartAIFirst })
	fence := e.claim(t, s.ID, session.StatusReserved)

	got, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Equal(t, session.StatusStarting, got.Status)
	require.NotNil(t, got.OpeningPromptVersionID)
	require.Equal(t, opening, *got.OpeningPromptVersionID)
	require.NotNil(t, got.StartingAt)
	evs := e.store.Events(s.ID)
	require.Equal(t, session.EventStarting, evs[1].Type)
	require.Equal(t, session.ActorRealtime, evs[1].Actor)
	require.JSONEq(t, `{"opening_prompt_version_id":"`+opening.String()+`"}`, string(evs[1].Payload))

	again, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err, "idempotent for the same fence")
	require.Equal(t, got.Version, again.Version)
}

func TestBeginStartAIFirstNoPublishedOpeningNullAndWarn(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001", func(in *session.CreateSessionInput) { in.ConversationStartMode = session.StartAIFirst })
	fence := e.claim(t, s.ID, session.StatusReserved)
	got, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err, "a missing OPENING never fails the session")
	require.Equal(t, session.StatusStarting, got.Status)
	require.Nil(t, got.OpeningPromptVersionID)
	require.JSONEq(t, `{"opening_prompt_version_id":null}`, string(e.store.Events(s.ID)[1].Payload))
	require.Contains(t, e.logs.String(), "session.opening_prompt_missing")
	require.Contains(t, e.logs.String(), `"level":"WARN"`)
}

func TestBeginStartUserFirstAlwaysNull(t *testing.T) {
	e := newEnv(t, 1800)
	opening := uuid.New()
	e.opening.Set(&opening)
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	got, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Nil(t, got.OpeningPromptVersionID)
	require.NotContains(t, e.logs.String(), "opening_prompt_missing")
}

func TestBeginStartAfterDeadline(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	e.clk.Advance(5*time.Minute + time.Second)
	_, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.ErrorIs(t, err, session.ErrStartDeadlinePassed)
	got := e.store.Session(s.ID)
	require.Equal(t, session.StatusAbandoned, got.Status)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
	require.Equal(t, session.ActorRealtime, e.store.Events(s.ID)[1].Actor)
}

func TestBeginStartWrongStateOrFence(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	_, err := e.svc.BeginStart(context.Background(), s.ID, fence+5)
	require.ErrorIs(t, err, session.ErrFenceLost)
	_, err = e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.NoError(t, err)
	_, err = e.svc.BeginStart(context.Background(), s.ID, fence)
	require.ErrorIs(t, err, session.ErrInvalidTransition)
	_, err = e.svc.BeginStart(context.Background(), uuid.New(), fence)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func (e *env) starting(t *testing.T) (session.Session, int64) {
	t.Helper()
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	got, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err)
	return got, fence
}

func TestActivateSetsStartedAndExpires(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.starting(t)
	e.clk.Advance(3 * time.Second)
	got, err := e.svc.Activate(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Equal(t, session.StatusActive, got.Status)
	require.Equal(t, e.clk.Now(), *got.StartedAt)
	require.Equal(t, got.StartedAt.Add(900*time.Second), *got.ExpiresAt)
	evs := e.store.Events(s.ID)
	require.Equal(t, session.EventStarted, evs[2].Type)

	again, err := e.svc.Activate(context.Background(), s.ID, fence)
	require.NoError(t, err, "idempotent")
	require.Equal(t, got.Version, again.Version)
	_, err = e.svc.Activate(context.Background(), s.ID, fence+1)
	require.ErrorIs(t, err, session.ErrInvalidTransition, "ACTIVE under another fence is not this caller's success")
}

func TestActivateAfter10s(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.starting(t)
	e.clk.Advance(10*time.Second + time.Millisecond)
	_, err := e.svc.Activate(context.Background(), s.ID, fence)
	require.ErrorIs(t, err, session.ErrStartTimeout)
	got := e.store.Session(s.ID)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Equal(t, session.EndProviderFailure, *got.EndReason)
	require.Zero(t, got.ChargedSeconds)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
	require.Zero(t, e.store.Outbox.Len(), "a session that never became ACTIVE publishes no session.completed")
}

func TestFailStartReleasesAndBumpsFence(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.starting(t)
	got, err := e.svc.FailStart(context.Background(), s.ID, &fence, session.EndServerError, "stt_connect_timeout", session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Equal(t, fence+1, got.RuntimeFence)
	require.Equal(t, "stt_connect_timeout", *got.FailureDetail)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
	evs := e.store.Events(s.ID)
	require.Equal(t, session.EventFailed, evs[2].Type)
	require.JSONEq(t, `{"released_seconds":900,"failure_detail":"stt_connect_timeout"}`, string(evs[2].Payload))
	require.Zero(t, e.store.Outbox.Len())

	again, err := e.svc.FailStart(context.Background(), s.ID, &fence, session.EndServerError, "stt_connect_timeout", session.ActorRealtime)
	require.NoError(t, err, "already FAILED: success, even though the fence moved")
	require.Equal(t, got.Version, again.Version)
	require.Equal(t, 1, e.count(balance.EntryRelease))

	_, err = e.svc.FailStart(context.Background(), s.ID, nil, session.EndUserEnded, "x", session.ActorSweeper)
	require.Error(t, err, "USER_ENDED is not a failure reason")
}

func TestFailStartWithStaleFence(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.starting(t)
	stale := fence - 1
	_, err := e.svc.FailStart(context.Background(), s.ID, &stale, session.EndServerError, "x", session.ActorRealtime)
	require.ErrorIs(t, err, session.ErrFenceLost)
	require.Equal(t, session.StatusStarting, e.store.Session(s.ID).Status)
	got, err := e.svc.FailStart(context.Background(), s.ID, nil, session.EndServerError, "starting_stuck", session.ActorSweeper)
	require.NoError(t, err, "the sweeper has no fence")
	require.Equal(t, session.StatusFailed, got.Status)
}

func TestEndedMetricCancelAbandonFailStart(t *testing.T) {
	e := newEnv(t, 3600)
	ctx := context.Background()
	a := e.reserved(t, "key-0000001")
	_, err := e.svc.Cancel(ctx, e.userID, a.ID)
	require.NoError(t, err)
	_, err = e.svc.Cancel(ctx, e.userID, a.ID) // no-op: not counted again
	require.NoError(t, err)

	b := e.reserved(t, "key-0000002")
	e.clk.Advance(6 * time.Minute)
	_, err = e.svc.Abandon(ctx, b.ID, session.ActorCleanup)
	require.NoError(t, err)

	c := e.reserved(t, "key-0000003")
	fence := e.claim(t, c.ID, session.StatusReserved)
	_, err = e.svc.BeginStart(ctx, c.ID, fence)
	require.NoError(t, err)
	_, err = e.svc.FailStart(ctx, c.ID, &fence, session.EndProviderFailure, "x", session.ActorRealtime)
	require.NoError(t, err)

	require.Equal(t, [][3]string{
		{"api", "USER_CANCELLED", "CANCELLED"}, {"api", "NOT_STARTED", "ABANDONED"}, {"api", "PROVIDER_FAILURE", "FAILED"},
	}, e.metrics.ended)
}

func TestCancelInTxLeavesPublishingToTheCommit(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx session.Tx) error {
		got, err := e.svc.CancelInTx(ctx, tx, s.ID, session.ActorAdmin)
		require.NoError(t, err)
		require.Equal(t, session.StatusCancelled, got.Status)
		return nil
	}))
	require.Equal(t, session.ActorAdmin, e.store.Events(s.ID)[1].Actor)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
}
