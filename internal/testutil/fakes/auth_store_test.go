package fakes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func newAuthStore(t *testing.T) auth.Store {
	return fakes.NewAuthStore(fakes.NewBalanceStore(), idgen.NewSequence(t))
}

func TestAuthStoreUserContract(t *testing.T)    { repotest.UserRepoSuite(t, newAuthStore) }
func TestAuthStoreRefreshContract(t *testing.T) { repotest.RefreshTokenRepoSuite(t, newAuthStore) }
func TestAuthStoreResetContract(t *testing.T)   { repotest.PasswordResetRepoSuite(t, newAuthStore) }

func TestAuthStoreRollsBackUserAndAccount(t *testing.T) {
	bal := fakes.NewBalanceStore()
	s := fakes.NewAuthStore(bal, idgen.NewSequence(t))
	u := repotest.NewUser("hana@example.com")
	boom := errors.New("boom")

	err := s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		require.NoError(t, tx.Users().Insert(ctx, u))
		require.NoError(t, tx.Accounts().OpenAccount(ctx, u.ID, 3600, u.CreatedAt))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, s.User(u.ID).ID, "user insert rolled back")
	require.Zero(t, bal.Account(u.ID).ID, "account rolled back with the auth tx")
	require.Empty(t, bal.Ledger())
}

func TestAuthStoreCommitsUserAndAccount(t *testing.T) {
	bal := fakes.NewBalanceStore()
	s := fakes.NewAuthStore(bal, idgen.NewSequence(t))
	u := repotest.NewUser("hana@example.com")
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		if err := tx.Users().Insert(ctx, u); err != nil {
			return err
		}
		return tx.Accounts().OpenAccount(ctx, u.ID, 3600, u.CreatedAt)
	}))
	require.Equal(t, u.ID, s.User(u.ID).ID)
	require.Equal(t, int64(3600), bal.Account(u.ID).AvailableSeconds)
}

func TestAuthStoreFailNext(t *testing.T) {
	s := fakes.NewAuthStore(fakes.NewBalanceStore(), idgen.NewSequence(t))
	s.FailNext("Users.Insert", errors.New("db down"))
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		return tx.Users().Insert(ctx, repotest.NewUser("a@example.com"))
	})
	require.EqualError(t, err, "db down")
}
