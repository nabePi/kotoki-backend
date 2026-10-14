package balance_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type env struct {
	store *fakes.BalanceStore
	svc   *balance.Service
	ids   idgen.Generator
}

func newEnv(t *testing.T) env {
	t.Helper()
	ids := idgen.NewSequence(t)
	store := fakes.NewBalanceStore(fakes.WithBalanceIDs(ids))
	return env{store: store, svc: balance.NewService(store, clock.NewFake(t0), ids), ids: ids}
}

func (e env) open(t *testing.T, initial int64) uuid.UUID {
	t.Helper()
	uid := e.ids.New()
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return balance.OpenAccount(ctx, tx, e.ids, uid, initial, t0)
	}))
	return uid
}

func TestOpenAccountNoInitial(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 0)
	acc := e.store.Account(uid)
	require.Equal(t, int64(0), acc.AvailableSeconds)
	require.Equal(t, int64(0), acc.ReservedSeconds)
	require.Equal(t, int64(0), acc.Version)
	require.Empty(t, e.store.Ledger())
}

func TestOpenAccountInitialGrant(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 3600)
	acc := e.store.Account(uid)
	require.Equal(t, int64(3600), acc.AvailableSeconds)
	require.Equal(t, int64(1), acc.Version)

	led := e.store.Ledger()
	require.Len(t, led, 1)
	l := led[0]
	require.Equal(t, balance.EntryGrant, l.EntryType)
	require.Equal(t, balance.RefRegistration, l.ReferenceType)
	require.Equal(t, "grant:initial:"+uid.String(), l.IdempotencyKey)
	require.Equal(t, int64(3600), l.AvailableSecondsDelta)
	require.Equal(t, int64(3600), l.AvailableSecondsAfter)
	require.Equal(t, int64(1), l.AccountVersion)
	require.NotNil(t, l.Reason)
	require.Equal(t, "initial balance", *l.Reason)
}

func TestGetBalanceAllowedDurations(t *testing.T) {
	cases := map[int64][]int64{
		1200: {300, 600, 900},
		299:  {},
		3600: {300, 600, 900, 1800, 2700, 3600},
	}
	for available, want := range cases {
		e := newEnv(t)
		uid := e.open(t, available)
		v, err := e.svc.GetBalance(context.Background(), uid)
		require.NoError(t, err)
		require.Equal(t, want, v.AllowedDurations, "available=%d", available)
		require.Equal(t, available, v.AvailableSeconds)
	}
}

func TestGetBalanceUnknownUser(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.GetBalance(context.Background(), uuid.New())
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))
}

func TestDescription(t *testing.T) {
	cases := []struct {
		e    balance.EntryType
		r    balance.ReferenceType
		want string
	}{
		{balance.EntryGrant, balance.RefRegistration, "Welcome balance"},
		{balance.EntryGrant, balance.RefAdmin, "Balance granted"},
		{balance.EntryAdjustment, balance.RefAdmin, "Balance adjustment"},
		{balance.EntryReserve, balance.RefSession, "Reserved for session"},
		{balance.EntryConsume, balance.RefSession, "Used in session"},
		{balance.EntryRelease, balance.RefSession, "Returned from session"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, balance.Description(balance.LedgerEntry{EntryType: c.e, ReferenceType: c.r}))
	}
}

func TestListTransactionsPagination(t *testing.T) {
	e := newEnv(t)
	uid := e.open(t, 0)
	for i := 0; i < 3; i++ {
		require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
			acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
			if err != nil {
				return err
			}
			_, err = tx.Balance().ApplyMovement(ctx, acc, balance.Movement{
				EntryType: balance.EntryGrant, AvailableDelta: 60, ReferenceType: balance.RefRegistration,
				IdempotencyKey: "grant:k" + string(rune('a'+i)), At: t0,
			})
			return err
		}))
	}

	page1, next, err := e.svc.ListTransactions(context.Background(), uid, nil, nil, 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Greater(t, page1[0].ID.String(), page1[1].ID.String(), "id DESC")
	require.NotNil(t, next)
	require.Equal(t, page1[1].ID, *next)

	page2, next2, err := e.svc.ListTransactions(context.Background(), uid, nil, next, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Nil(t, next2)
}
