//go:build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type violation struct {
	name       string
	run        func(t *testing.T, db testutil.DBTX) error
	sqlstate   string
	constraint string
}

func hash(n int) []byte { return make([]byte, n) }

func userWith(o testutil.Row) func(*testing.T, testutil.DBTX) error {
	return func(_ *testing.T, db testutil.DBTX) error {
		return testutil.TryInsert(context.Background(), db, "users", testutil.UserRow(o))
	}
}

func tokenRow(userID uuid.UUID, o testutil.Row) testutil.Row {
	r := testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": userID, "family_id": uuid.Must(uuid.NewV7()),
		"token_hash": append(hash(31), byte(time.Now().UnixNano())), "expires_at": time.Now().Add(time.Hour),
	}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func TestIdentitySchemaViolations(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPostgres(t)
	uid := testutil.SeedUser(t, pool, nil)
	now := time.Now().UTC()

	cases := []violation{
		{"role", userWith(testutil.Row{"role": "SUPERUSER"}), "23514", "chk_users_role"},
		{"deleted without deleted_at", userWith(testutil.Row{"status": "DELETED"}), "23514", "chk_users_deleted_at"},
		{"purged active", userWith(testutil.Row{"purged_at": now}), "23514", "chk_users_purged_at"},
		{"email len", userWith(testutil.Row{"email": "ab"}), "23514", "chk_users_email_len"},
		{"phone fmt", userWith(testutil.Row{"phone_number": "0812"}), "23514", "chk_users_phone_fmt"},
		{"terms version", userWith(testutil.Row{"terms_version": "bad version"}), "23514", "chk_users_terms_version"},
		{"email case-insensitive", func(t *testing.T, db testutil.DBTX) error {
			testutil.MustInsert(t, ctx, db, "users", testutil.UserRow(testutil.Row{"email": "A@x.id"}))
			return testutil.TryInsert(ctx, db, "users", testutil.UserRow(testutil.Row{"email": "a@x.id"}))
		}, "23505", "ux_users_email"},
		{"duplicate phone", func(t *testing.T, db testutil.DBTX) error {
			testutil.MustInsert(t, ctx, db, "users", testutil.UserRow(testutil.Row{"phone_number": "+628123456789"}))
			return testutil.TryInsert(ctx, db, "users", testutil.UserRow(testutil.Row{"phone_number": "+628123456789"}))
		}, "23505", "ux_users_phone"},
		{"refresh hash len", func(_ *testing.T, db testutil.DBTX) error {
			return testutil.TryInsert(ctx, db, "refresh_tokens", tokenRow(uid, testutil.Row{"token_hash": hash(31)}))
		}, "23514", "chk_refresh_hash_len"},
		{"revoked without reason", func(_ *testing.T, db testutil.DBTX) error {
			return testutil.TryInsert(ctx, db, "refresh_tokens", tokenRow(uid, testutil.Row{"token_hash": hash(32), "revoked_at": now}))
		}, "23514", "chk_refresh_revoke"},
		{"revoke reason", func(_ *testing.T, db testutil.DBTX) error {
			return testutil.TryInsert(ctx, db, "refresh_tokens", tokenRow(uid, testutil.Row{"token_hash": hash(32), "revoked_at": now, "revoke_reason": "X"}))
		}, "23514", "chk_refresh_revoke_reason"},
		{"duplicate refresh hash", func(t *testing.T, db testutil.DBTX) error {
			testutil.MustInsert(t, ctx, db, "refresh_tokens", tokenRow(uid, testutil.Row{"token_hash": hash(32)}))
			return testutil.TryInsert(ctx, db, "refresh_tokens", tokenRow(uid, testutil.Row{"token_hash": hash(32)}))
		}, "23505", "ux_refresh_tokens_hash"},
		{"reset expiry", func(_ *testing.T, db testutil.DBTX) error {
			return testutil.TryInsert(ctx, db, "password_reset_tokens", testutil.Row{
				"id": uuid.Must(uuid.NewV7()), "user_id": uid, "token_hash": hash(32),
				"created_at": now, "expires_at": now})
		}, "23514", "chk_reset_expiry"},
		{"balance available", func(_ *testing.T, db testutil.DBTX) error {
			u := testutil.SeedUser(t, db, nil)
			return testutil.TryInsert(ctx, db, "balance_accounts", testutil.BalanceAccountRow(u, testutil.Row{"available_seconds": -1}))
		}, "23514", "chk_balance_available_non_negative"},
		{"balance reserved", func(_ *testing.T, db testutil.DBTX) error {
			u := testutil.SeedUser(t, db, nil)
			return testutil.TryInsert(ctx, db, "balance_accounts", testutil.BalanceAccountRow(u, testutil.Row{"reserved_seconds": -1}))
		}, "23514", "chk_balance_reserved_non_negative"},
		{"two accounts per user", func(t *testing.T, db testutil.DBTX) error {
			u := testutil.SeedUser(t, db, nil)
			testutil.MustInsert(t, ctx, db, "balance_accounts", testutil.BalanceAccountRow(u, nil))
			return testutil.TryInsert(ctx, db, "balance_accounts", testutil.BalanceAccountRow(u, nil))
		}, "23505", "ux_balance_accounts_user"},
		{"delete user with account", func(t *testing.T, db testutil.DBTX) error {
			u := testutil.SeedUser(t, db, nil)
			testutil.MustInsert(t, ctx, db, "balance_accounts", testutil.BalanceAccountRow(u, nil))
			_, err := db.Exec(ctx, "DELETE FROM users WHERE id=$1", u)
			return err
		}, "23503", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			testutil.RequirePgErr(t, c.run(t, tx), c.sqlstate, c.constraint)
		})
	}

	t.Run("two NULL phones allowed", func(t *testing.T) {
		testutil.SeedUser(t, pool, nil)
		testutil.SeedUser(t, pool, nil)
	})
}
