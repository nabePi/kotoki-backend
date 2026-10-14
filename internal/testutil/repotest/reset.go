package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

func resetToken(user uuid.UUID, raw string) auth.PasswordResetToken {
	return auth.PasswordResetToken{ID: uuid.Must(uuid.NewV7()), UserID: user, TokenHash: hashOf(raw), CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
}

// PasswordResetRepoSuite runs the auth.PasswordResetRepository contract.
func PasswordResetRepoSuite(t *testing.T, newStore func(t *testing.T) auth.Store) {
	t.Helper()
	Each(t, newStore,
		Case[auth.Store]{Name: "FindByHashReturnsEmailAndStatus", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("hana@example.com")
			insertUser(t, s, u)
			tk := resetToken(u.ID, "r1")
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				require.NoError(t, tx.PasswordResets().Insert(ctx, tk))
				got, err := tx.PasswordResets().FindByHash(ctx, hashOf("r1"))
				require.NoError(t, err)
				require.Equal(t, tk.ID, got.Token.ID)
				require.Equal(t, "hana@example.com", got.Email)
				require.Equal(t, auth.UserStatusActive, got.UserStatus)
				locked, err := tx.PasswordResets().LockByHash(ctx, hashOf("r1"))
				require.NoError(t, err)
				require.Equal(t, tk.ID, locked.Token.ID)
				_, err = tx.PasswordResets().FindByHash(ctx, hashOf("nope"))
				require.ErrorIs(t, err, auth.ErrResetTokenNotFound)
			})
		}},
		Case[auth.Store]{Name: "InvalidateOpenForUser", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("hana@example.com")
			insertUser(t, s, u)
			a, b := resetToken(u.ID, "a"), resetToken(u.ID, "b")
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				require.NoError(t, tx.PasswordResets().Insert(ctx, a))
				require.NoError(t, tx.PasswordResets().Insert(ctx, b))
				require.NoError(t, tx.PasswordResets().MarkUsed(ctx, a.ID, now))
				require.NoError(t, tx.PasswordResets().InvalidateOpenForUser(ctx, u.ID, now))
				ga, _ := tx.PasswordResets().FindByHash(ctx, hashOf("a"))
				gb, _ := tx.PasswordResets().FindByHash(ctx, hashOf("b"))
				require.Nil(t, ga.Token.InvalidatedAt, "used token is left alone")
				require.NotNil(t, ga.Token.UsedAt)
				require.NotNil(t, gb.Token.InvalidatedAt)
			})
		}},
		Case[auth.Store]{Name: "InvalidateOthersSkipsExcept", Run: func(t *testing.T, s auth.Store) {
			u := NewUser("hana@example.com")
			insertUser(t, s, u)
			keep, other := resetToken(u.ID, "keep"), resetToken(u.ID, "other")
			inTx(t, s, func(ctx context.Context, tx auth.Tx) {
				require.NoError(t, tx.PasswordResets().Insert(ctx, keep))
				require.NoError(t, tx.PasswordResets().Insert(ctx, other))
				require.NoError(t, tx.PasswordResets().InvalidateOthers(ctx, u.ID, keep.ID, now))
				gk, _ := tx.PasswordResets().FindByHash(ctx, hashOf("keep"))
				goth, _ := tx.PasswordResets().FindByHash(ctx, hashOf("other"))
				require.Nil(t, gk.Token.InvalidatedAt)
				require.NotNil(t, goth.Token.InvalidatedAt)
			})
		}},
	)
}
