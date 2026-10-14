package fakes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func TestBalanceStoreContract(t *testing.T) {
	repotest.BalanceRepoSuite(t, func(*testing.T) repotest.BalanceHarness {
		return repotest.BalanceHarness{
			Store:    fakes.NewBalanceStore(),
			SeedUser: func(*testing.T) uuid.UUID { return uuid.Must(uuid.NewV7()) },
		}
	})
}

func TestBalanceStoreRollsBackOnError(t *testing.T) {
	s := fakes.NewBalanceStore()
	uid := uuid.Must(uuid.NewV7())
	boom := errors.New("boom")
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		require.NoError(t, tx.Balance().InsertAccount(ctx, balance.Account{ID: uuid.Must(uuid.NewV7()), UserID: uid}))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, s.Account(uid).ID, "account insert must be rolled back")
}

func TestBalanceStoreFailNext(t *testing.T) {
	s := fakes.NewBalanceStore()
	s.FailNext("InsertAccount", errors.New("disk full"))
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return tx.Balance().InsertAccount(ctx, balance.Account{ID: uuid.Must(uuid.NewV7()), UserID: uuid.Must(uuid.NewV7())})
	})
	require.EqualError(t, err, "disk full")
}
