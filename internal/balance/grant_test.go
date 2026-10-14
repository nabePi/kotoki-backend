package balance_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func (e env) ledgerWithKey(key string) []balance.LedgerEntry {
	var out []balance.LedgerEntry
	for _, l := range e.store.Ledger() {
		if l.IdempotencyKey == key {
			out = append(out, l)
		}
	}
	return out
}

func grantIn(user uuid.UUID, seconds int64, key string) balance.GrantInput {
	return balance.GrantInput{UserID: user, AdminUserID: uuid.New(), Seconds: seconds, Reason: "support credit", IdempotencyKey: key, At: t0}
}

func adjustIn(user uuid.UUID, delta int64, key string) balance.AdjustInput {
	return balance.AdjustInput{UserID: user, AdminUserID: uuid.New(), SecondsDelta: delta, Reason: "manual fix", IdempotencyKey: key, At: t0}
}

func TestGrantHappyPath(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 0)
	key := uuid.NewString()
	res, err := e.svc.Grant(context.Background(), grantIn(uid, 1800, key))
	require.NoError(t, err)
	require.False(t, res.Replayed)
	require.Equal(t, "grant:"+key, res.Entry.IdempotencyKey)
	require.Equal(t, balance.RefAdmin, res.Entry.ReferenceType)
	require.Equal(t, int64(1800), res.Entry.AvailableSecondsAfter)
	require.NotNil(t, res.Entry.Reason)
	require.Equal(t, "support credit", *res.Entry.Reason)
	require.Equal(t, int64(1800), res.Account.AvailableSeconds)
}

func TestGrantReplay(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 0)
	in := grantIn(uid, 600, uuid.NewString())
	first, err := e.svc.Grant(context.Background(), in)
	require.NoError(t, err)
	second, err := e.svc.Grant(context.Background(), in)
	require.NoError(t, err)
	require.True(t, second.Replayed)
	require.Equal(t, first.Entry.ID, second.Entry.ID)
	require.Len(t, e.ledgerWithKey("grant:"+in.IdempotencyKey), 1)
	require.Equal(t, int64(600), e.store.Account(uid).AvailableSeconds)
}

func TestGrantKeyReused(t *testing.T) {
	e := newEnv(t)
	a, b := e.open(t, 0), e.open(t, 0)
	key := uuid.NewString()
	_, err := e.svc.Grant(context.Background(), grantIn(a, 300, key))
	require.NoError(t, err)

	_, err = e.svc.Grant(context.Background(), grantIn(a, 600, key))
	require.Equal(t, errs.CodeIdempotencyKeyReused, errs.CodeOf(err))
	_, err = e.svc.Grant(context.Background(), grantIn(b, 300, key))
	require.Equal(t, errs.CodeIdempotencyKeyReused, errs.CodeOf(err))
	require.Equal(t, int64(0), e.store.Account(b).AvailableSeconds)
}

func TestGrantValidation(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 0)
	key := uuid.NewString()
	cases := []struct {
		name string
		in   balance.GrantInput
		code errs.Code
	}{
		{"seconds 0", grantIn(uid, 0, key), errs.CodeValidationFailed},
		{"seconds too big", grantIn(uid, 360001, key), errs.CodeValidationFailed},
		{"reason short", func() balance.GrantInput { i := grantIn(uid, 60, key); i.Reason = "ab"; return i }(), errs.CodeValidationFailed},
		{"reason long", func() balance.GrantInput { i := grantIn(uid, 60, key); i.Reason = strings.Repeat("x", 501); return i }(), errs.CodeValidationFailed},
		{"key empty", grantIn(uid, 60, ""), errs.CodeIdempotencyKeyRequired},
		{"key not uuid", grantIn(uid, 60, "x"), errs.CodeIdempotencyKeyInvalid},
	}
	for _, c := range cases {
		_, err := e.svc.Grant(context.Background(), c.in)
		require.Equal(t, c.code, errs.CodeOf(err), c.name)
	}
	require.Equal(t, int64(0), e.store.Account(uid).AvailableSeconds)

	ok := grantIn(uid, 360000, key)
	_, err := e.svc.Grant(context.Background(), ok)
	require.NoError(t, err)
}

func TestGrantUnknownUser(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Grant(context.Background(), grantIn(uuid.New(), 60, uuid.NewString()))
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))
}

func TestAdjust(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 600)

	_, err := e.svc.Adjust(context.Background(), adjustIn(uid, -601, uuid.NewString()))
	require.Equal(t, errs.CodeBalanceAdjustmentNegative, errs.CodeOf(err))
	require.Equal(t, map[string]any{"available_seconds": int64(600), "seconds_delta": int64(-601)}, errs.DetailsOf(err))
	require.Equal(t, int64(600), e.store.Account(uid).AvailableSeconds)

	key := uuid.NewString()
	res, err := e.svc.Adjust(context.Background(), adjustIn(uid, -600, key))
	require.NoError(t, err)
	require.Equal(t, "adjust:"+key, res.Entry.IdempotencyKey)
	require.Equal(t, balance.EntryAdjustment, res.Entry.EntryType)
	require.Equal(t, int64(0), res.Account.AvailableSeconds)

	again, err := e.svc.Adjust(context.Background(), adjustIn(uid, -600, key))
	require.NoError(t, err)
	require.True(t, again.Replayed)
	require.Len(t, e.ledgerWithKey("adjust:"+key), 1)

	_, err = e.svc.Adjust(context.Background(), adjustIn(uid, 10, key))
	require.Equal(t, errs.CodeIdempotencyKeyReused, errs.CodeOf(err))

	for _, delta := range []int64{0, 360001, -360001} {
		_, err = e.svc.Adjust(context.Background(), adjustIn(uid, delta, uuid.NewString()))
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "delta %d", delta)
	}
	up, err := e.svc.Adjust(context.Background(), adjustIn(uid, 360000, uuid.NewString()))
	require.NoError(t, err)
	require.Equal(t, int64(360000), up.Account.AvailableSeconds)
}

func TestGrantDuplicateMovementRetry(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 0)
	e.store.FailNext("ApplyMovement", balance.ErrDuplicateMovement)
	in := grantIn(uid, 300, uuid.NewString())
	res, err := e.svc.Grant(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, int64(300), res.Account.AvailableSeconds)
	require.Len(t, e.ledgerWithKey("grant:"+in.IdempotencyKey), 1)
}

func TestValidateAdminIdempotencyKey(t *testing.T) {
	require.NoError(t, balance.ValidateAdminIdempotencyKey(uuid.NewString()))
	require.Equal(t, errs.CodeIdempotencyKeyRequired, errs.CodeOf(balance.ValidateAdminIdempotencyKey("")))
	require.Equal(t, errs.CodeIdempotencyKeyInvalid, errs.CodeOf(balance.ValidateAdminIdempotencyKey("nope")))
}
