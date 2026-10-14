package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

var now = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// NewUser returns a valid ACTIVE user with the given email.
func NewUser(email string) auth.User {
	return auth.User{ //nolint:gosec // G101: fixed fake argon2id hash for tests
		ID: uuid.Must(uuid.NewV7()), Email: email, FullName: "Test User",
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		Role:         auth.RoleUser, Status: auth.UserStatusActive, PasswordChangedAt: now,
		TermsVersion: "2026-10-01", TermsAcceptedAt: now, CreatedAt: now, UpdatedAt: now,
	}
}

func inTx(t *testing.T, s auth.Store, fn func(ctx context.Context, tx auth.Tx)) {
	t.Helper()
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		fn(ctx, tx)
		return nil
	}))
}

func insertUser(t *testing.T, s auth.Store, u auth.User) {
	t.Helper()
	inTx(t, s, func(ctx context.Context, tx auth.Tx) { require.NoError(t, tx.Users().Insert(ctx, u)) })
}

// UserRepoSuite runs the auth.UserRepository contract.
func UserRepoSuite(t *testing.T, newStore func(t *testing.T) auth.Store) {
	t.Helper()
	Each(t, newStore,
		Case[auth.Store]{Name: "DuplicateEmailCaseInsensitive", Run: func(t *testing.T, s auth.Store) {
			insertUser(t, s, NewUser("Hana@Example.com"))
			err := s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
				return tx.Users().Insert(ctx, NewUser("hana@example.com"))
			})
			require.ErrorIs(t, err, auth.ErrEmailTaken)
		}},
		Case[auth.Store]{Name: "DuplicatePhone", Run: func(t *testing.T, s auth.Store) {
			phone := "+628123456789"
			a, b := NewUser("a@example.com"), NewUser("b@example.com")
			a.PhoneNumber, b.PhoneNumber = &phone, &phone
			insertUser(t, s, a)
			err := s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error { return tx.Users().Insert(ctx, b) })
			require.ErrorIs(t, err, auth.ErrPhoneTaken)
		}},
		Case[auth.Store]{Name: "GetByEmailCaseInsensitive", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("hana@example.com")
			insertUser(t, s, u)
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				got, err := tx.Users().GetByEmail(ctx, "HANA@EXAMPLE.COM")
				require.NoError(t, err)
				require.Equal(t, u.ID, got.ID)
				_, err = tx.Users().GetByEmail(ctx, "nobody@example.com")
				require.ErrorIs(t, err, auth.ErrUserNotFound)
				_, err = tx.Users().GetByID(ctx, uuid.New())
				require.ErrorIs(t, err, auth.ErrUserNotFound)
			})
		}},
		Case[auth.Store]{Name: "UpdateConsentsRequiresActive", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("sus@example.com")
			u.Status = auth.UserStatusSuspended
			insertUser(t, s, u)
			err := s.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
				_, err := tx.Users().UpdateConsents(ctx, u.ID, "2026-12-01", now)
				return err
			})
			require.ErrorIs(t, err, auth.ErrUserNotActive)

			ok := NewUser("ok@example.com")
			insertUser(t, s, ok)
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				got, err := tx.Users().UpdateConsents(ctx, ok.ID, "2026-12-01", now.Add(time.Hour))
				require.NoError(t, err)
				require.Equal(t, "2026-12-01", got.TermsVersion)
				require.True(t, got.TermsAcceptedAt.Equal(now.Add(time.Hour)))
			})
		}},
		Case[auth.Store]{Name: "UpdateStatusDeletedFillsDeletedAt", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("del@example.com")
			insertUser(t, s, u)
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				require.NoError(t, tx.Users().UpdateStatus(ctx, u.ID, auth.UserStatusDeleted, now))
				got, err := tx.Users().GetByID(ctx, u.ID)
				require.NoError(t, err)
				require.Equal(t, auth.UserStatusDeleted, got.Status)
				require.NotNil(t, got.DeletedAt)
				require.True(t, got.DeletedAt.Equal(now))
			})
		}},
		Case[auth.Store]{Name: "ListFiltersAndCursor", Run: func(t *testing.T, s auth.Store) {
			admin := NewUser("root@example.com")
			admin.Role = auth.RoleAdmin
			susp := NewUser("hana.sus@example.com")
			susp.Status = auth.UserStatusSuspended
			users := []auth.User{NewUser("hana1@example.com"), NewUser("hana2@example.com"), susp, admin}
			for _, u := range users {
				insertUser(t, s, u)
			}
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				all, err := tx.Users().List(ctx, auth.UserFilter{}, nil, 10)
				require.NoError(t, err)
				require.Len(t, all, 4)
				for i := 1; i < len(all); i++ {
					require.Greater(t, all[i-1].ID.String(), all[i].ID.String(), "id DESC")
				}

				hana, err := tx.Users().List(ctx, auth.UserFilter{EmailPrefix: "HANA"}, nil, 10)
				require.NoError(t, err)
				require.Len(t, hana, 3)

				st := auth.UserStatusSuspended
				got, err := tx.Users().List(ctx, auth.UserFilter{Status: &st}, nil, 10)
				require.NoError(t, err)
				require.Len(t, got, 1)

				role := auth.RoleAdmin
				got, err = tx.Users().List(ctx, auth.UserFilter{Role: &role}, nil, 10)
				require.NoError(t, err)
				require.Len(t, got, 1)
				require.Equal(t, admin.ID, got[0].ID)

				page1, err := tx.Users().List(ctx, auth.UserFilter{}, nil, 2)
				require.NoError(t, err)
				require.Len(t, page1, 2)
				page2, err := tx.Users().List(ctx, auth.UserFilter{}, &page1[1].ID, 10)
				require.NoError(t, err)
				require.Len(t, page2, 2)
			})
		}},
	)
}
