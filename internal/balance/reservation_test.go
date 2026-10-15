package balance_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// inTx runs fn inside a transaction with the user's account locked, like session.Service does.
func (e env) inTx(t *testing.T, uid uuid.UUID, fn func(ctx context.Context, tx balance.Tx, acc balance.Account) error) error {
	t.Helper()
	return e.store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
		require.NoError(t, err)
		return fn(ctx, tx, acc)
	})
}

func (e env) reserve(t *testing.T, uid uuid.UUID, seconds int64) (sid, rid uuid.UUID) {
	t.Helper()
	sid, rid = e.ids.New(), e.ids.New()
	require.NoError(t, e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		_, err := e.svc.Reserve(ctx, tx, acc, balance.ReserveInput{ReservationID: rid, SessionID: sid, Seconds: seconds, At: t0})
		return err
	}))
	return sid, rid
}

func (e env) settle(t *testing.T, uid, sid uuid.UUID, in balance.SettleInput) (balance.SettleResult, error) {
	t.Helper()
	var res balance.SettleResult
	err := e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		in.SessionID = sid
		var err error
		res, err = e.svc.SettleReservation(ctx, tx, acc, in)
		return err
	})
	return res, err
}

func TestReserve_MovesAvailableToReserved(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 1200)
	sid, rid := e.reserve(t, uid, 600)

	acc := e.store.Account(uid)
	require.EqualValues(t, 600, acc.AvailableSeconds)
	require.EqualValues(t, 600, acc.ReservedSeconds)
	led := e.store.Ledger()
	last := led[len(led)-1]
	require.Equal(t, balance.EntryReserve, last.EntryType)
	require.EqualValues(t, -600, last.AvailableSecondsDelta)
	require.EqualValues(t, 600, last.ReservedSecondsDelta)
	require.Equal(t, "reserve:"+rid.String(), last.IdempotencyKey)
	require.Equal(t, balance.RefSession, last.ReferenceType)
	require.Equal(t, sid, *last.SessionID)
	require.Equal(t, rid, *last.ReservationID)

	res := e.store.Reservation(sid)
	require.Equal(t, balance.ReservationHeld, res.Status)
	require.Equal(t, rid, res.ID)
	require.True(t, res.StartDeadlineAt.Equal(t0.Add(5*time.Minute)), "start deadline = at + 5 min (D-035)")
	require.Nil(t, res.ConsumedSeconds)
}

func TestReserve_Validation(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 1200)
	err := e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		_, err := e.svc.Reserve(ctx, tx, acc, balance.ReserveInput{ReservationID: e.ids.New(), SessionID: e.ids.New(), Seconds: 123, At: t0})
		return err
	})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "only the six allowed durations")

	err = e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		_, err := e.svc.Reserve(ctx, tx, acc, balance.ReserveInput{ReservationID: e.ids.New(), SessionID: e.ids.New(), Seconds: 1800, At: t0})
		return err
	})
	require.Equal(t, errs.CodeBalanceInsufficient, errs.CodeOf(err))
	require.EqualValues(t, 1200, e.store.Account(uid).AvailableSeconds, "nothing moved")
	require.Len(t, e.store.Ledger(), 1, "only the initial grant")
}

func TestSettle_Example1(t *testing.T) { // spec 4.5 example 1 / 4.6.5
	e := newEnv(t)
	uid := e.open(t, 1200)
	sid, rid := e.reserve(t, uid, 600)
	ending := t0.Add(7*time.Minute + 400*time.Millisecond)
	res, err := e.settle(t, uid, sid, balance.SettleInput{StartedAt: t0, EndingAt: &ending, EndedAt: t0.Add(7*time.Minute + 2100*time.Millisecond),
		ExpiresAt: t0.Add(10 * time.Minute), At: t0.Add(8 * time.Minute)})
	require.NoError(t, err)
	require.EqualValues(t, 421, res.Charged)
	require.EqualValues(t, 179, res.Released)
	require.EqualValues(t, 420_400, res.ActiveMs)
	require.True(t, res.BillableEnd.Equal(ending))

	acc := e.store.Account(uid)
	require.EqualValues(t, 779, acc.AvailableSeconds)
	require.EqualValues(t, 0, acc.ReservedSeconds)
	led := e.store.Ledger()
	consume, release := led[len(led)-2], led[len(led)-1]
	require.Equal(t, balance.EntryConsume, consume.EntryType)
	require.EqualValues(t, 0, consume.AvailableSecondsDelta)
	require.EqualValues(t, -421, consume.ReservedSecondsDelta)
	require.Equal(t, "consume:"+rid.String(), consume.IdempotencyKey)
	require.Equal(t, balance.EntryRelease, release.EntryType)
	require.EqualValues(t, 179, release.AvailableSecondsDelta)
	require.EqualValues(t, -179, release.ReservedSecondsDelta)
	require.Equal(t, "release:"+rid.String(), release.IdempotencyKey)

	r := e.store.Reservation(sid)
	require.Equal(t, balance.ReservationSettled, r.Status)
	require.EqualValues(t, 421, *r.ConsumedSeconds)
	require.EqualValues(t, 179, *r.ReleasedSeconds)
	require.NotNil(t, r.SettledAt)
	require.Nil(t, r.ReleasedAt)
}

func TestSettle_ChargedZero_OnlyRelease(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 600)
	sid, _ := e.reserve(t, uid, 300)
	ending := t0.Add(45 * time.Second)
	res, err := e.settle(t, uid, sid, balance.SettleInput{StartedAt: t0, EndingAt: &ending, EndedAt: ending, ExpiresAt: t0.Add(5 * time.Minute),
		ServiceFault: true, At: t0.Add(time.Minute)})
	require.NoError(t, err)
	require.EqualValues(t, 0, res.Charged)
	require.EqualValues(t, 300, res.Released)
	led := e.store.Ledger()
	require.Equal(t, balance.EntryRelease, led[len(led)-1].EntryType)
	require.NotEqual(t, balance.EntryConsume, led[len(led)-2].EntryType, "a zero charge writes no CONSUME entry (D-NEW-4-1)")
	require.EqualValues(t, 600, e.store.Account(uid).AvailableSeconds)
	require.EqualValues(t, 0, e.store.Account(uid).ReservedSeconds)
}

func TestSettle_ChargedEqualsReserved_OnlyConsume(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 900)
	sid, _ := e.reserve(t, uid, 900)
	ending := t0.Add(15 * time.Minute)
	res, err := e.settle(t, uid, sid, balance.SettleInput{StartedAt: t0, EndingAt: &ending, EndedAt: ending.Add(8 * time.Second),
		ExpiresAt: t0.Add(15 * time.Minute), At: ending})
	require.NoError(t, err)
	require.EqualValues(t, 900, res.Charged)
	require.EqualValues(t, 0, res.Released)
	led := e.store.Ledger()
	require.Equal(t, balance.EntryConsume, led[len(led)-1].EntryType, "no RELEASE entry when nothing is left")
	require.EqualValues(t, 0, e.store.Account(uid).AvailableSeconds)
	require.EqualValues(t, 0, e.store.Account(uid).ReservedSeconds)
}

func TestSettle_FailureUnder60s_Charge0(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 300)
	sid, _ := e.reserve(t, uid, 300)
	end := t0.Add(59*time.Second + 999*time.Millisecond)
	res, err := e.settle(t, uid, sid, balance.SettleInput{StartedAt: t0, EndingAt: &end, EndedAt: end, ExpiresAt: t0.Add(5 * time.Minute), ServiceFault: true, At: end})
	require.NoError(t, err)
	require.EqualValues(t, 0, res.Charged)
}

func TestSettle_SweeperActiveFailedWithoutEndingAt(t *testing.T) { // example 9
	e := newEnv(t)
	uid := e.open(t, 900)
	sid, _ := e.reserve(t, uid, 900)
	res, err := e.settle(t, uid, sid, balance.SettleInput{StartedAt: t0, EndedAt: t0.Add(5 * time.Minute), ExpiresAt: t0.Add(15 * time.Minute),
		ServiceFault: true, At: t0.Add(6 * time.Minute)})
	require.NoError(t, err)
	require.EqualValues(t, 270, res.Charged)
	require.EqualValues(t, 630, res.Released)
}

func TestRelease_FullReturn(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 1200)
	sid, rid := e.reserve(t, uid, 600)
	var released int64
	require.NoError(t, e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		var err error
		released, err = e.svc.ReleaseReservation(ctx, tx, acc, sid, t0.Add(time.Minute))
		return err
	}))
	require.EqualValues(t, 600, released)
	require.EqualValues(t, 1200, e.store.Account(uid).AvailableSeconds)
	require.EqualValues(t, 0, e.store.Account(uid).ReservedSeconds)
	led := e.store.Ledger()
	last := led[len(led)-1]
	require.Equal(t, balance.EntryRelease, last.EntryType)
	require.EqualValues(t, 600, last.AvailableSecondsDelta)
	require.EqualValues(t, -600, last.ReservedSecondsDelta)
	require.Equal(t, "release:"+rid.String(), last.IdempotencyKey)
	r := e.store.Reservation(sid)
	require.Equal(t, balance.ReservationReleased, r.Status)
	require.EqualValues(t, 0, *r.ConsumedSeconds)
	require.EqualValues(t, 600, *r.ReleasedSeconds)
	require.NotNil(t, r.ReleasedAt)
	require.Nil(t, r.SettledAt)
}

func TestSettle_ReservationNotHeld_ErrReservationState(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 600)
	sid, _ := e.reserve(t, uid, 600)
	require.NoError(t, e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		_, err := e.svc.ReleaseReservation(ctx, tx, acc, sid, t0)
		return err
	}))
	before := len(e.store.Ledger())
	ending := t0.Add(time.Minute)
	_, err := e.settle(t, uid, sid, balance.SettleInput{StartedAt: t0, EndingAt: &ending, EndedAt: ending, ExpiresAt: t0.Add(10 * time.Minute), At: ending})
	require.ErrorIs(t, err, balance.ErrReservationState)
	require.Len(t, e.store.Ledger(), before, "nothing is booked twice")

	err = e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		_, err := e.svc.ReleaseReservation(ctx, tx, acc, sid, t0)
		return err
	})
	require.ErrorIs(t, err, balance.ErrReservationState, "a released reservation cannot be released again")
}

func TestSettle_UnknownSession(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 600)
	_, err := e.settle(t, uid, e.ids.New(), balance.SettleInput{StartedAt: t0, EndedAt: t0, ExpiresAt: t0, At: t0})
	require.ErrorIs(t, err, balance.ErrReservationNotFound)
}

func TestReserve_RollbackLeavesNoReservation(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 1200)
	sid, rid := e.ids.New(), e.ids.New()
	_ = e.inTx(t, uid, func(ctx context.Context, tx balance.Tx, acc balance.Account) error {
		_, err := e.svc.Reserve(ctx, tx, acc, balance.ReserveInput{ReservationID: rid, SessionID: sid, Seconds: 600, At: t0})
		require.NoError(t, err)
		return context.Canceled // the surrounding transaction fails after the reserve
	})
	require.EqualValues(t, 1200, e.store.Account(uid).AvailableSeconds)
	require.Zero(t, e.store.Reservation(sid).ID, "the fake restores reservations on rollback like accounts")
}
