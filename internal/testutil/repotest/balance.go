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
	// SeedSession returns the id of a session row owned by userID (balance_reservations has a
	// foreign key to sessions in PostgreSQL); nil means any fresh uuid will do (the fake).
	SeedSession func(t *testing.T, userID uuid.UUID) uuid.UUID
}

func (h BalanceHarness) session(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	if h.SeedSession != nil {
		return h.SeedSession(t, userID)
	}
	return uuid.Must(uuid.NewV7())
}

func heldReservation(userID, accountID, sessionID uuid.UUID, seconds int64) balance.Reservation {
	return balance.Reservation{
		ID: uuid.Must(uuid.NewV7()), UserID: userID, AccountID: accountID, SessionID: sessionID, ReservedSeconds: seconds,
		Status: balance.ReservationHeld, StartDeadlineAt: at.Add(5 * time.Minute), CreatedAt: at,
	}
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
			adminID, reason := h.SeedUser(t), "manual fix"
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
					AdminUserID: &adminID, Reason: &reason, IdempotencyKey: "adjust:x", At: at})
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
		Case[BalanceHarness]{Name: "ReservationLifecycleSettled", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			sid := h.session(t, uid)
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				repo := tx.Balance()
				_, err := repo.LockReservationBySessionID(ctx, sid)
				require.ErrorIs(t, err, balance.ErrReservationNotFound)

				acc, err := repo.LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				res := heldReservation(uid, acc.ID, sid, 600)
				require.NoError(t, repo.InsertReservation(ctx, res))
				got, err := repo.LockReservationBySessionID(ctx, sid)
				require.NoError(t, err)
				require.Equal(t, res.ID, got.ID)
				require.Equal(t, balance.ReservationHeld, got.Status)
				require.Nil(t, got.ConsumedSeconds)
				require.True(t, got.StartDeadlineAt.Equal(at.Add(5*time.Minute)))

				require.NoError(t, repo.MarkReservationSettled(ctx, res.ID, 421, 179, at.Add(time.Hour)))
				got, err = repo.LockReservationBySessionID(ctx, sid)
				require.NoError(t, err)
				require.Equal(t, balance.ReservationSettled, got.Status)
				require.EqualValues(t, 421, *got.ConsumedSeconds)
				require.EqualValues(t, 179, *got.ReleasedSeconds)
				require.NotNil(t, got.SettledAt)
				require.Nil(t, got.ReleasedAt)
				return nil
			}))
		}},
		Case[BalanceHarness]{Name: "ReservationLifecycleReleased", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			sid := h.session(t, uid)
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				repo := tx.Balance()
				acc, err := repo.LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				res := heldReservation(uid, acc.ID, sid, 300)
				require.NoError(t, repo.InsertReservation(ctx, res))
				require.NoError(t, repo.MarkReservationReleased(ctx, res.ID, at.Add(time.Minute)))
				got, err := repo.LockReservationBySessionID(ctx, sid)
				require.NoError(t, err)
				require.Equal(t, balance.ReservationReleased, got.Status)
				require.EqualValues(t, 0, *got.ConsumedSeconds)
				require.EqualValues(t, 300, *got.ReleasedSeconds)
				require.NotNil(t, got.ReleasedAt)
				require.Nil(t, got.SettledAt)
				return nil
			}))
		}},
		Case[BalanceHarness]{Name: "ReservationConstraints", Run: func(t *testing.T, h BalanceHarness) {
			uid := openAccount(t, h)
			sid := h.session(t, uid)
			inconsistent := func(name string, fn func(ctx context.Context, repo balance.Repository, res balance.Reservation) error) {
				err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
					repo := tx.Balance()
					acc, err := repo.LockAccountByUserID(ctx, uid)
					require.NoError(t, err)
					res := heldReservation(uid, acc.ID, h.session(t, uid), 600)
					require.NoError(t, repo.InsertReservation(ctx, res))
					return fn(ctx, repo, res)
				})
				require.Error(t, err, name)
			}
			inconsistent("SETTLED must satisfy consumed + released = reserved", func(ctx context.Context, repo balance.Repository, res balance.Reservation) error {
				return repo.MarkReservationSettled(ctx, res.ID, 400, 100, at)
			})
			inconsistent("consumed above reserved", func(ctx context.Context, repo balance.Repository, res balance.Reservation) error {
				return repo.MarkReservationSettled(ctx, res.ID, 700, -100, at)
			})
			require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				repo := tx.Balance()
				acc, err := repo.LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				require.NoError(t, repo.InsertReservation(ctx, heldReservation(uid, acc.ID, sid, 600)))
				return nil
			}))
			err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
				acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
				require.NoError(t, err)
				return tx.Balance().InsertReservation(ctx, heldReservation(uid, acc.ID, sid, 600))
			})
			require.Error(t, err, "one reservation per session (ux_balance_reservations_session)")
		}},
	)
}
