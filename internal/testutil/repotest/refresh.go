package repotest

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

func hashOf(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func token(user, family uuid.UUID, raw string, expires time.Time) auth.RefreshToken {
	id := uuid.Must(uuid.NewV7())
	return auth.RefreshToken{ID: id, UserID: user, FamilyID: family, TokenHash: hashOf(raw), CreatedAt: now, ExpiresAt: expires}
}

// RefreshTokenRepoSuite runs the auth.RefreshTokenRepository contract.
func RefreshTokenRepoSuite(t *testing.T, newStore func(t *testing.T) auth.Store) {
	t.Helper()
	Each(t, newStore,
		Case[auth.Store]{Name: "LockByHashMissing", Run: func(t *testing.T, s auth.Store) {
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				_, err := tx.RefreshTokens().LockByHash(ctx, hashOf("nope"))
				require.ErrorIs(t, err, auth.ErrRefreshTokenNotFound)
			})
		}},
		Case[auth.Store]{Name: "InsertGetLock", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("a@example.com")
			insertUser(t, s, u)
			tk := token(u.ID, uuid.Must(uuid.NewV7()), "raw-1", now.Add(time.Hour))
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				require.NoError(t, tx.RefreshTokens().Insert(ctx, tk))
				got, err := tx.RefreshTokens().GetByID(ctx, tk.ID)
				require.NoError(t, err)
				require.Equal(t, tk.FamilyID, got.FamilyID)
				locked, err := tx.RefreshTokens().LockByHash(ctx, hashOf("raw-1"))
				require.NoError(t, err)
				require.Equal(t, tk.ID, locked.ID)
			})
		}},
		Case[auth.Store]{Name: "MarkRotated", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("a@example.com")
			insertUser(t, s, u)
			fam := uuid.Must(uuid.NewV7())
			old, next := token(u.ID, fam, "old", now.Add(time.Hour)), token(u.ID, fam, "new", now.Add(time.Hour))
			next.ParentID = &old.ID
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				require.NoError(t, tx.RefreshTokens().Insert(ctx, old))
				require.NoError(t, tx.RefreshTokens().Insert(ctx, next))
				require.NoError(t, tx.RefreshTokens().MarkRotated(ctx, old.ID, next.ID, now))
				got, err := tx.RefreshTokens().GetByID(ctx, old.ID)
				require.NoError(t, err)
				require.NotNil(t, got.RevokeReason)
				require.Equal(t, "ROTATED", *got.RevokeReason)
				require.NotNil(t, got.ReplacedByID)
				require.Equal(t, next.ID, *got.ReplacedByID)
				require.NotNil(t, got.RevokedAt)
			})
		}},
		Case[auth.Store]{Name: "RevokeFamilyCountsNewlyRevoked", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("a@example.com")
			insertUser(t, s, u)
			fam, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			a, b, c := token(u.ID, fam, "a", now.Add(time.Hour)), token(u.ID, fam, "b", now.Add(time.Hour)), token(u.ID, other, "c", now.Add(time.Hour))
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				for _, tk := range []auth.RefreshToken{a, b, c} {
					require.NoError(t, tx.RefreshTokens().Insert(ctx, tk))
				}
				require.NoError(t, tx.RefreshTokens().MarkRotated(ctx, a.ID, b.ID, now))
				n, err := tx.RefreshTokens().RevokeFamily(ctx, fam, "REUSE_DETECTED", now)
				require.NoError(t, err)
				require.Equal(t, int64(1), n, "a was already revoked; only b is new")
				n, err = tx.RefreshTokens().RevokeFamily(ctx, fam, "REUSE_DETECTED", now)
				require.NoError(t, err)
				require.Zero(t, n)
				n, err = tx.RefreshTokens().RevokeAllForUser(ctx, u.ID, "LOGOUT", now)
				require.NoError(t, err)
				require.Equal(t, int64(1), n, "only c remains")
			})
		}},
		Case[auth.Store]{Name: "CountActiveAndDeleteAll", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("a@example.com")
			insertUser(t, s, u)
			fam := uuid.Must(uuid.NewV7())
			live, expired, revoked := token(u.ID, fam, "live", now.Add(time.Hour)), token(u.ID, fam, "expired", now.Add(-time.Hour)), token(u.ID, fam, "revoked", now.Add(time.Hour))
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				for _, tk := range []auth.RefreshToken{live, expired, revoked} {
					require.NoError(t, tx.RefreshTokens().Insert(ctx, tk))
				}
				require.NoError(t, tx.RefreshTokens().MarkRotated(ctx, revoked.ID, live.ID, now))
				n, err := tx.RefreshTokens().CountActive(ctx, u.ID, now)
				require.NoError(t, err)
				require.Equal(t, 1, n)

				require.NoError(t, tx.RefreshTokens().DeleteAllForUser(ctx, u.ID))
				n, err = tx.RefreshTokens().CountActive(ctx, u.ID, now)
				require.NoError(t, err)
				require.Zero(t, n)
				_, err = tx.RefreshTokens().GetByID(ctx, live.ID)
				require.ErrorIs(t, err, auth.ErrRefreshTokenNotFound)
			})
		}},
	)
}
