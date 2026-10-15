package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

var tenAM = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

func at(h, m, s, ms int) time.Time {
	return time.Date(2026, 10, 14, h, m, s, ms*int(time.Millisecond), time.UTC)
}

// active walks a new session to ACTIVE at 10:00:00.000 under fence f and returns it.
func (e *env) active(t *testing.T, duration int) (session.Session, int64) {
	t.Helper()
	e.clk.Set(tenAM)
	s := e.reserved(t, "key-"+uuid.NewString()[:8], func(in *session.CreateSessionInput) { in.DurationSeconds = duration })
	fence := e.claim(t, s.ID, session.StatusReserved)
	_, err := e.svc.BeginStart(context.Background(), s.ID, fence)
	require.NoError(t, err)
	got, err := e.svc.Activate(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Equal(t, tenAM, *got.StartedAt)
	return got, fence
}

func (e *env) outboxPayloads(t *testing.T) []events.SessionCompletedPayload {
	t.Helper()
	var out []events.SessionCompletedPayload
	for _, ev := range e.store.Outbox.Events() {
		out = append(out, ev.Payload.(events.SessionCompletedPayload))
	}
	return out
}

func TestBeginEndingSetsEndingAtCoalesceHandoff(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 900)
	e.clk.Set(at(10, 3, 0, 0))
	_, err := e.svc.MarkHandoff(context.Background(), s.ID, fence)
	require.NoError(t, err)
	e.clk.Set(at(10, 3, 31, 0))
	got, err := e.svc.BeginEnding(context.Background(), s.ID, nil, session.EndReconnectTimeout, "handoff_not_resumed", session.ActorSweeper)
	require.NoError(t, err)
	require.Equal(t, session.StatusEnding, got.Status)
	require.Equal(t, at(10, 3, 0, 0), *got.EndingAt, "ending_at = handoff_at")
	require.Equal(t, "handoff_not_resumed", *got.FailureDetail)
	evs := e.store.Events(s.ID)
	require.Equal(t, session.EventEnding, evs[len(evs)-1].Type)
	require.JSONEq(t, `{"end_reason":"RECONNECT_TIMEOUT","detail":"handoff_not_resumed"}`, string(evs[len(evs)-1].Payload))
	sent := e.control.Sent()
	require.Equal(t, "ENDING", sent[len(sent)-1].Msg.Status)
}

func TestBeginEndingNoopWhenAlreadyEndingFirstReasonWins(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 900)
	e.clk.Set(at(10, 5, 0, 0))
	first, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	e.clk.Set(at(10, 5, 9, 0))
	second, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndProviderFailure, "late", session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, first.Version, second.Version)
	require.Equal(t, session.EndUserEnded, *second.EndReason)
	require.Equal(t, at(10, 5, 0, 0), *second.EndingAt)
	require.Len(t, e.control.Sent(), 1, "the repeat publishes nothing")
}

func TestBeginEndingSweeperBumpsFenceRuntimeDoesNot(t *testing.T) {
	e := newEnv(t, 3600)
	s, fence := e.active(t, 900)
	e.clk.Set(at(10, 5, 0, 0))
	byRuntime, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, fence, byRuntime.RuntimeFence)
	_, err = e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)

	s2, fence2 := e.active(t, 900)
	bySweeper, err := e.svc.BeginEnding(context.Background(), s2.ID, nil, session.EndDurationElapsed, "", session.ActorSweeper)
	require.NoError(t, err)
	require.Equal(t, fence2+1, bySweeper.RuntimeFence, "the live runtime now gets ErrFenceLost")
	_, err = e.svc.BeginEnding(context.Background(), s2.ID, &fence2, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err, "ENDING already: idempotent even for the stale fence")
}

func TestBeginEndingRejectsWrongStateAndFenceAndReason(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 900)
	stale := fence + 3
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &stale, session.EndUserEnded, "", session.ActorRealtime)
	require.ErrorIs(t, err, session.ErrFenceLost)
	_, err = e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserCancelled, "", session.ActorRealtime)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = e.svc.Cancel(context.Background(), e.userID, s.ID)
	require.Error(t, err, "an ACTIVE session cannot be cancelled")
}

func TestFinalizeUserEndedExample11(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	e.clk.Set(at(10, 8, 5, 0))
	got, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, session.StatusCompleted, got.Status)
	require.Equal(t, 420, got.ChargedSeconds, "billable_end is ending_at, the flush after it is free")
	require.Equal(t, at(10, 8, 5, 0), *got.EndedAt)
	require.Equal(t, fence+1, got.RuntimeFence, "terminal: the fence moves on")

	acc := e.bal.Account(e.userID)
	require.Equal(t, int64(1800-420), acc.AvailableSeconds)
	require.Zero(t, acc.ReservedSeconds)
	res := e.bal.Reservation(s.ID)
	require.Equal(t, balance.ReservationSettled, res.Status)
	require.Equal(t, int64(420), *res.ConsumedSeconds)
	require.Equal(t, int64(180), *res.ReleasedSeconds)
	require.Equal(t, 1, e.count(balance.EntryConsume))
	require.Equal(t, 1, e.count(balance.EntryRelease))

	evs := e.store.Events(s.ID)
	last := evs[len(evs)-1]
	require.Equal(t, session.EventCompleted, last.Type)
	require.JSONEq(t, `{"charged_seconds":420,"released_seconds":180,"active_ms":420000,"billable_end":"2026-10-14T10:07:00Z","disconnected_seconds":0}`, string(last.Payload))
}

func TestFinalizeExample1(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 300))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	e.clk.Set(at(10, 7, 4, 0))
	got, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, 421, got.ChargedSeconds, "420.3 s rounds up")
	require.Equal(t, int64(179), *e.bal.Reservation(s.ID).ReleasedSeconds)
	require.Equal(t, int64(1800-421), e.bal.Account(e.userID).AvailableSeconds)
}

func TestFinalizeProviderFailureUnder60sCharge0FailedOutboxFinalStatusFailed(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 900)
	e.clk.Set(at(10, 0, 40, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndProviderFailure, "stt_down", session.ActorRealtime)
	require.NoError(t, err)
	got, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Zero(t, got.ChargedSeconds)
	require.Equal(t, int64(1800), e.bal.Account(e.userID).AvailableSeconds)
	require.Equal(t, 0, e.count(balance.EntryConsume), "zero entries are not written")
	evs := e.store.Events(s.ID)
	require.Equal(t, session.EventFailed, evs[len(evs)-1].Type)
	p := e.outboxPayloads(t)
	require.Len(t, p, 1)
	require.Equal(t, "FAILED", p[0].FinalStatus)
	require.Equal(t, "PROVIDER_FAILURE", p[0].EndReason)
	require.Zero(t, p[0].ChargedSeconds)
}

func TestFinalizeServiceFaultOver60sGetsThirtySecondsOff(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 900)
	e.clk.Set(at(10, 2, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndServerError, "", session.ActorRealtime)
	require.NoError(t, err)
	got, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	require.Equal(t, 90, got.ChargedSeconds, "120 s active minus the 30 s goodwill")
}

func TestFinalizeTerminalIsIdempotentBeforeFenceCheck(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	first, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)

	stale := fence // the terminal transition already moved the fence on
	again, err := e.svc.Finalize(context.Background(), s.ID, &stale, session.ActorRealtime)
	require.NoError(t, err, "finished already: success, whatever the fence")
	require.Equal(t, first.Version, again.Version)
	bySweeper, err := e.svc.Finalize(context.Background(), s.ID, nil, session.ActorSweeper)
	require.NoError(t, err)
	require.Equal(t, first.Version, bySweeper.Version)

	require.Equal(t, 1, e.count(balance.EntryConsume))
	require.Equal(t, 1, e.count(balance.EntryRelease))
	require.Equal(t, 1, e.store.Outbox.Len(), "exactly one session.completed")
	require.Len(t, e.metrics.ended, 1, "and one count")
}

func TestFinalizeStaleFenceInEndingErrFenceLost(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, nil, session.EndDurationElapsed, "", session.ActorSweeper)
	require.NoError(t, err)
	_, err = e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.ErrorIs(t, err, session.ErrFenceLost, "the sweeper moved the fence: the old runtime must stop")
	require.Equal(t, session.StatusEnding, e.store.Session(s.ID).Status)
	require.Zero(t, e.count(balance.EntryConsume))
}

func TestFinalizeWrongStatus(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	_, err := e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.ErrorIs(t, err, session.ErrInvalidTransition, "ACTIVE must go through BeginEnding")
	_, err = e.svc.Finalize(context.Background(), uuid.New(), nil, session.ActorSweeper)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func TestFinalizeFailsDanglingTurns(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	ctx := context.Background()
	t1, t2 := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	require.NoError(t, e.store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		require.NoError(t, tx.Turns().Insert(ctx, fence, session.TurnStart{ID: t1, SessionID: s.ID, Sequence: 1, Generation: 1, CreatedAt: tenAM}))
		done := "ok"
		require.NoError(t, tx.Turns().Finalize(ctx, fence, t1, session.TurnFinal{CreatedAt: tenAM, Status: session.TurnCompleted, AssistantText: &done, CompletedAt: tenAM}))
		return tx.Turns().Insert(ctx, fence, session.TurnStart{ID: t2, SessionID: s.ID, Sequence: 2, Generation: 1, CreatedAt: tenAM})
	}))
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(ctx, s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(ctx, s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	turns := e.store.Turns(s.ID)
	require.Len(t, turns, 2)
	require.Equal(t, session.TurnFailed, *turns[1].Status)
	require.Equal(t, "SESSION_ENDED", *turns[1].ErrorCode)
}

func TestEndedMetricFinalizeAndFailActive(t *testing.T) {
	e := newEnv(t, 3600)
	ctx := context.Background()
	s, fence := e.active(t, 600)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(ctx, s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(ctx, s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(ctx, s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)

	s2, _ := e.active(t, 600)
	e.clk.Set(at(10, 5, 0, 0))
	_, err = e.svc.FailActive(ctx, s2.ID, at(10, 5, 0, 0), "heartbeat_lost")
	require.NoError(t, err)
	_, err = e.svc.FailActive(ctx, s2.ID, at(10, 5, 0, 0), "heartbeat_lost")
	require.NoError(t, err)

	require.Equal(t, [][3]string{{"api", "USER_ENDED", "COMPLETED"}, {"api", "SERVER_ERROR", "FAILED"}}, e.metrics.ended)
}

func TestFinalizeOutboxPayload(t *testing.T) {
	e := newEnv(t, 3600)
	ctx := context.Background()
	s, fence := e.active(t, 600)
	mk := func(seq int, user string, st session.TurnStatus) {
		id := uuid.Must(uuid.NewV7())
		require.NoError(t, e.store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
			require.NoError(t, tx.Turns().Insert(ctx, fence, session.TurnStart{ID: id, SessionID: s.ID, Sequence: seq, Generation: 1, CreatedAt: tenAM}))
			if user != "" {
				require.NoError(t, tx.Turns().UpdateUser(ctx, fence, id, session.TurnUser{CreatedAt: tenAM, Text: user}))
			}
			reply := "r"
			return tx.Turns().Finalize(ctx, fence, id, session.TurnFinal{CreatedAt: tenAM, Status: st, AssistantText: &reply, CompletedAt: tenAM})
		}))
	}
	mk(1, "こんにちは", session.TurnCompleted)
	mk(2, "はい", session.TurnInterrupted)
	mk(3, "ノイズ", session.TurnDiscarded)
	mk(4, "", session.TurnCompleted)
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(ctx, s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(ctx, s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)

	evs := e.store.Outbox.Events()
	require.Len(t, evs, 1)
	require.Equal(t, "REALTIME", evs[0].Producer)
	require.Equal(t, "session.completed", evs[0].EventType)
	require.Equal(t, s.ID, evs[0].AggregateID)
	require.Equal(t, e.userID, evs[0].UserID)
	p := evs[0].Payload.(events.SessionCompletedPayload)
	require.Equal(t, 3, p.TurnCount, "DISCARDED is not counted")
	require.Equal(t, 2, p.UserTurnCount, "turns with a user text, DISCARDED excluded")
	require.Equal(t, "COMPLETED", p.FinalStatus)
	require.Equal(t, 420, p.ActiveSeconds)
	require.Equal(t, 420, p.ChargedSeconds)
	require.Equal(t, 600, p.DurationSeconds)
	require.Equal(t, tenAM, p.StartedAt)
	require.Equal(t, at(10, 7, 0, 0), p.EndedAt)

	// a sweeper's close is produced by the WORKER
	s2, _ := e.active(t, 600)
	e.clk.Set(at(10, 5, 0, 0))
	_, err = e.svc.BeginEnding(ctx, s2.ID, nil, session.EndDurationElapsed, "", session.ActorSweeper)
	require.NoError(t, err)
	_, err = e.svc.Finalize(ctx, s2.ID, nil, session.ActorSweeper)
	require.NoError(t, err)
	require.Equal(t, "WORKER", e.store.Outbox.Events()[1].Producer)
}

func TestFailActiveExample9(t *testing.T) {
	e := newEnv(t, 1800)
	s, _ := e.active(t, 900)
	e.clk.Set(at(10, 6, 0, 0))
	got, err := e.svc.FailActive(context.Background(), s.ID, at(10, 5, 0, 0), "heartbeat_lost")
	require.NoError(t, err)
	require.Equal(t, session.StatusFailed, got.Status)
	require.Equal(t, session.EndServerError, *got.EndReason)
	require.Equal(t, at(10, 5, 0, 0), *got.EndedAt, "ended_at = the estimated end, not now")
	require.Nil(t, got.EndingAt)
	require.Equal(t, 270, got.ChargedSeconds, "300 s active minus 30 s for a service fault")
	require.Equal(t, "heartbeat_lost", *got.FailureDetail)
	evs := e.store.Events(s.ID)
	last := evs[len(evs)-1]
	require.Equal(t, session.EventFailed, last.Type)
	require.Equal(t, session.ActorSweeper, last.Actor)
	require.Contains(t, string(last.Payload), `"estimated_end":"2026-10-14T10:05:00Z"`)
	p := e.outboxPayloads(t)
	require.Len(t, p, 1)
	require.Equal(t, "FAILED", p[0].FinalStatus)
	require.Equal(t, 300, p[0].ActiveSeconds)
	require.Equal(t, "WORKER", e.store.Outbox.Events()[0].Producer)
}

func TestFailActiveEstimatedEndAfterExpiryClampsToExpires(t *testing.T) {
	e := newEnv(t, 1800)
	s, _ := e.active(t, 300)
	e.clk.Set(at(10, 9, 0, 0))
	got, err := e.svc.FailActive(context.Background(), s.ID, at(10, 8, 0, 0), "heartbeat_lost")
	require.NoError(t, err)
	require.Equal(t, at(10, 5, 0, 0), *got.EndedAt, "never later than expires_at")
	require.Equal(t, 270, got.ChargedSeconds)

	// a heartbeat from before the session started cannot move ended_at before started_at
	s2, _ := e.active(t, 300)
	got2, err := e.svc.FailActive(context.Background(), s2.ID, tenAM.Add(-time.Minute), "heartbeat_lost")
	require.NoError(t, err)
	require.Equal(t, tenAM, *got2.EndedAt)
	require.Zero(t, got2.ChargedSeconds)
}

func TestMarkHandoff(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 900)
	e.clk.Set(at(10, 3, 0, 0))
	before := e.store.Session(s.ID)
	got, err := e.svc.MarkHandoff(context.Background(), s.ID, fence)
	require.NoError(t, err)
	require.Equal(t, at(10, 3, 0, 0), *got.HandoffAt)
	require.Equal(t, before.Version, got.Version, "no version bump")
	require.Equal(t, before.RuntimeFence, got.RuntimeFence)

	stale := fence + 7
	_, err = e.svc.MarkHandoff(context.Background(), s.ID, stale)
	require.ErrorIs(t, err, session.ErrFenceLost)

	newFence, claimed, err := e.svc.ClaimRuntime(context.Background(), s.ID, []session.Status{session.StatusActive})
	require.NoError(t, err)
	require.Equal(t, fence+1, newFence)
	require.Nil(t, claimed.HandoffAt, "attaching ends the handoff")
	_, err = e.svc.MarkHandoff(context.Background(), s.ID, fence)
	require.ErrorIs(t, err, session.ErrFenceLost, "the old runtime lost the session")
}

func TestMarkHandoffNeedsActive(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	_, err := e.svc.MarkHandoff(context.Background(), s.ID, fence)
	require.ErrorIs(t, err, session.ErrInvalidTransition)
	_, err = e.svc.MarkHandoff(context.Background(), uuid.New(), 1)
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func TestClaimRuntimeDisallowedStatusErrInvalidTransition(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	_, _, err := e.svc.ClaimRuntime(context.Background(), s.ID, []session.Status{session.StatusActive})
	require.ErrorIs(t, err, session.ErrInvalidTransition)
	fence, got, err := e.svc.ClaimRuntime(context.Background(), s.ID, []session.Status{session.StatusReserved})
	require.NoError(t, err)
	require.Equal(t, int64(1), fence)
	require.Equal(t, s.Version, got.Version, "a claim is not a transition")
	_, _, err = e.svc.ClaimRuntime(context.Background(), uuid.New(), []session.Status{session.StatusReserved})
	require.Equal(t, errs.CodeSessionNotFound, errs.CodeOf(err))
}

func TestAddDisconnected(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 300)
	e.clk.Set(at(10, 0, 20, 0))
	got, err := e.svc.AddDisconnected(context.Background(), s.ID, fence, 12)
	require.NoError(t, err)
	require.Equal(t, 12, got.DisconnectedSeconds)

	_, err = e.svc.AddDisconnected(context.Background(), s.ID, fence+1, 5)
	require.ErrorIs(t, err, session.ErrFenceLost)
	_, err = e.svc.AddDisconnected(context.Background(), s.ID, fence, -1)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	e.clk.Set(at(10, 5, 5, 0)) // the episode ends 5 s after expires_at: only the part before counts
	got, err = e.svc.AddDisconnected(context.Background(), s.ID, fence, 20)
	require.NoError(t, err)
	require.Equal(t, 12+15, got.DisconnectedSeconds)
}

func TestAddDisconnectedOnReservedIsInvalidTransition(t *testing.T) {
	e := newEnv(t, 1800)
	s := e.reserved(t, "key-0000001")
	fence := e.claim(t, s.ID, session.StatusReserved)
	_, err := e.svc.AddDisconnected(context.Background(), s.ID, fence, 3)
	require.ErrorIs(t, err, session.ErrInvalidTransition)
}

func TestSetSummary(t *testing.T) {
	e := newEnv(t, 1800)
	s, fence := e.active(t, 600)
	require.ErrorIs(t, e.svc.SetSummary(context.Background(), s.ID, "x"), session.ErrInvalidTransition, "not ended yet")
	e.clk.Set(at(10, 7, 0, 0))
	_, err := e.svc.BeginEnding(context.Background(), s.ID, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.svc.Finalize(context.Background(), s.ID, &fence, session.ActorRealtime)
	require.NoError(t, err)
	version := e.store.Session(s.ID).Version
	require.NoError(t, e.svc.SetSummary(context.Background(), s.ID, "Kita membahas wawancara."))
	require.NoError(t, e.svc.SetSummary(context.Background(), s.ID, "Ringkasan lain"))
	got := e.store.Session(s.ID)
	require.Equal(t, "Kita membahas wawancara.", *got.SummaryText, "never overwritten")
	require.Equal(t, version, got.Version, "not a transition")

	cancelled := e.reserved(t, "key-0000099")
	_, err = e.svc.Cancel(context.Background(), e.userID, cancelled.ID)
	require.NoError(t, err)
	require.Error(t, e.svc.SetSummary(context.Background(), cancelled.ID, "x"), "a session that was never ACTIVE has no summary")
}
