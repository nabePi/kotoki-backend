package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

// BalanceHarness is one fresh store plus a way to create users it accepts
// (the PostgreSQL store needs real users rows; the fake does not).
type BalanceHarness struct {
	Store    balance.Store
	SeedUser func(t *testing.T) uuid.UUID
}

var at = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func grant(key string, seconds int64) balance.Movement {
	return balance.Movement{
		EntryType: balance.EntryGrant, AvailableDelta: seconds, ReferenceType: balance.RefRegistration,
		IdempotencyKey: key, At: at,
	}
}

func openAccount(t *testing.T, h BalanceHarness) uuid.UUID {
	t.Helper()
	uid := h.SeedUser(t)
	require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return tx.Balance().InsertAccount(ctx, balance.Account{ID: uuid.Must(uuid.NewV7()), UserID: uid, CreatedAt: at, UpdatedAt: at})
	}))
	return uid
}

// BalanceRepoSuite runs the balance.Repository contract against any store.
func BalanceRepoSuite(t *testing.T, newHarness func(t *testing.T) BalanceHarness) {
	t.Helper()
	Each(t, newHarness,
		Case[BalanceHarness]{Name: "LockAndGetAccount", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				locked, err := tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				got, err := tx.Balance().GetAccountByUserID(ctx, uid)
				require.NoError(t, err)
				require.Equal(t, uid, locked.UserID)
				require.Equal(t, locked.ID, got.ID)
				require.Zero(t, got.Version)
				return nil
			}))
		}},
		Case[BalanceHarness]{Name: "MissingAccount", Run: func(t *testing.T, h BalanceHarness) {
			uid := h.SeedUser(t)
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				_, err := tx.Balance().LockAccountByUserID(ctx, uid)
				require.ErrorIs(t, err, balance.ErrAccountNotFound)
				_, err = tx.Balance().GetAccountByUserID(ctx, uid)
				require.ErrorIs(t, err, balance.ErrAccountNotFound)
				return nil
			}))
		}},
		Case[BalanceHarness]{Name: "ApplyMovementBumpsVersionAndFillsAfter", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				e1, err := tx.Balance().ApplyMovement(ctx, acc, grant("grant:one", 600))
				require.NoError(t, err)
				require.Equal(t, int64(600), e1.AvailableSecondsAfter)
				require.Equal(t, int64(1), e1.AccountVersion)

				acc, err = tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				require.Equal(t, int64(600), acc.AvailableSeconds)
				require.Equal(t, int64(1), acc.Version)
				e2, err := tx.Balance().ApplyMovement(ctx, acc, grant("grant:two", 300))
				require.NoError(t, err)
				require.Equal(t, int64(900), e2.AvailableSecondsAfter)
				require.Equal(t, int64(2), e2.AccountVersion)
				return nil
			}))
		}},
		Case[BalanceHarness]{Name: "DuplicateKeyIsErrDuplicateMovement", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				_, err = tx.Balance().ApplyMovement(ctx, acc, grant("grant:dup", 60))
				require.NoError(t, err)
				acc, err = tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				_, err = tx.Balance().ApplyMovement(ctx, acc, grant("grant:dup", 60))
				return err
			})
			require.ErrorIs(t, err, balance.ErrDuplicateMovement)
		}},
		Case[BalanceHarness]{Name: "FindByIdempotencyKey", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				_, ok, err := tx.Balance().FindLedgerByIdempotencyKey(ctx, "grant:nope")
				require.NoError(t, err)
				require.False(t, ok)
				acc, _ := tx.Balance().LockAccountByUserID(ctx, uid)
				want, err := tx.Balance().ApplyMovement(ctx, acc, grant("grant:found", 60))
				require.NoError(t, err)
				got, ok, err := tx.Balance().FindLedgerByIdempotencyKey(ctx, "grant:found")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, want.ID, got.ID)
				return nil
			}))
		}},
		Case[BalanceHarness]{Name: "ListLedgerOrderFilterCursor", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			var ids []uuid.UUID
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				for _, k := range []string{"grant:a", "grant:b", "grant:c"} {
					acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
					require.NoError(t, err)
					e, err := tx.Balance().ApplyMovement(ctx, acc, grant(k, 60))
					require.NoError(t, err)
					ids = append(ids, e.ID)
				}
				acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				_, err = tx.Balance().ApplyMovement(ctx, acc, balance.Movement{
					EntryType: balance.EntryAdjustment, AvailableDelta: 10, ReferenceType: balance.RefAdmin,
					IdempotencyKey: "adjust:x", At: at})
				return err
			}))
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				all, err := tx.Balance().ListLedger(ctx, uid, nil, nil, 10)
				require.NoError(t, err)
				require.Len(t, all, 4)
				for i := 1; i < len(all); i++ {
					require.Greater(t, all[i-1].ID.String(), all[i].ID.String(), "id DESC")
				}

				grants, err := tx.Balance().ListLedger(ctx, uid, []balance.EntryType{balance.EntryGrant}, nil, 10)
				require.NoError(t, err)
				require.Len(t, grants, 3)

				page, err := tx.Balance().ListLedger(ctx, uid, []balance.EntryType{balance.EntryGrant}, &ids[2], 10)
				require.NoError(t, err)
				require.Len(t, page, 2)
				require.Equal(t, ids[1], page[0].ID)

				other, err := tx.Balance().ListLedger(ctx, h.SeedUser(t), nil, nil, 10)
				require.NoError(t, err)
				require.Empty(t, other)
				return nil
			}))
		}},
	)
}
