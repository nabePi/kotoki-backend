//go:build integration

package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func newPGAuthStore(t *testing.T) (*pgxpool.Pool, auth.Store) {
	t.Helper()
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool, postgres.WithIDs(idgen.NewV7()), postgres.WithClock(clock.System{}))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
	return pool, store.AuthStore()
}

func authStoreFactory(t *testing.T) auth.Store {
	_, s := newPGAuthStore(t)
	return s
}

func TestPGUserRepoSuite(t *testing.T)          { repotest.UserRepoSuite(t, authStoreFactory) }
func TestPGRefreshTokenRepoSuite(t *testing.T)  { repotest.RefreshTokenRepoSuite(t, authStoreFactory) }
func TestPGPasswordResetRepoSuite(t *testing.T) { repotest.PasswordResetRepoSuite(t, authStoreFactory) }

func TestPGRefreshFamilyDeleteOrdering(t *testing.T) {
	_, s := newPGAuthStore(t)
	u := repotest.NewUser("hana@example.com")
	ctx := context.Background()
	require.NoError(t, s.WithinTx(ctx, func(ctx context.Context, tx auth.Tx) error {
		if err := tx.Users().Insert(ctx, u); err != nil {
			return err
		}
		fam := uuid.Must(uuid.NewV7())
		mk := func(id uuid.UUID, parent *uuid.UUID, hash byte) auth.RefreshToken {
			h := make([]byte, 32)
			h[0] = hash
			return auth.RefreshToken{ID: id, UserID: u.ID, FamilyID: fam, ParentID: parent, TokenHash: h,
				CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
		}
		a, b, c := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		for _, tk := range []auth.RefreshToken{mk(a, nil, 1), mk(b, &a, 2), mk(c, &b, 3)} {
			if err := tx.RefreshTokens().Insert(ctx, tk); err != nil {
				return err
			}
		}
		if err := tx.RefreshTokens().MarkRotated(ctx, a, b, t0); err != nil {
			return err
		}
		if err := tx.RefreshTokens().MarkRotated(ctx, b, c, t0); err != nil {
			return err
		}
		// parent_id / replaced_by_id form a chain: one DELETE must remove them all
		return tx.RefreshTokens().DeleteAllForUser(ctx, u.ID)
	}))
}

func TestPGRegisterAtomic(t *testing.T) {
	pool, s := newPGAuthStore(t)
	ctx := context.Background()
	failing := repotest.NewUser("rollback@example.com")
	boom := context.DeadlineExceeded

	err := s.WithinTx(ctx, func(ctx context.Context, tx auth.Tx) error {
		if err := tx.Users().Insert(ctx, failing); err != nil {
			return err
		}
		if err := tx.Accounts().OpenAccount(ctx, failing.ID, 3600, t0); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	var n int
	for _, q := range []string{`SELECT count(*) FROM users`, `SELECT count(*) FROM balance_accounts`, `SELECT count(*) FROM balance_ledger`} {
		require.NoError(t, pool.QueryRow(ctx, q).Scan(&n))
		require.Zero(t, n, q)
	}

	ok := repotest.NewUser("ok@example.com")
	require.NoError(t, s.WithinTx(ctx, func(ctx context.Context, tx auth.Tx) error {
		if err := tx.Users().Insert(ctx, ok); err != nil {
			return err
		}
		return tx.Accounts().OpenAccount(ctx, ok.ID, 3600, t0)
	}))
	var key string
	var after int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT idempotency_key, available_seconds_after FROM balance_ledger`).Scan(&key, &after))
	require.Equal(t, "grant:initial:"+ok.ID.String(), key)
	require.Equal(t, int64(3600), after)
}

func TestPGRefreshReuseCommitsRevoke(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	clk := clock.NewFake(t0)
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })

	hasher := auth.NewArgon2Hasher(auth.Argon2Config{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 4, MaxWait: time.Second})
	jwtIssuer, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: []byte("0123456789abcdef0123456789abcdef"), Issuer: "kotoki-api", Audience: "kotoki", TTL: 15 * time.Minute}, ids)
	require.NoError(t, err)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dispatcher := auth.NewMailDispatcher(noMail{}, log, auth.DispatcherOptions{})
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	svc, err := auth.NewService(store.AuthStore(), hasher, jwtIssuer, dispatcher, clk, ids, log, nil, auth.Config{
		TermsVersion: "2026-10-01", AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour,
		RefreshReuseGrace: 10 * time.Second, ResetTTL: 15 * time.Minute, ResetURLBase: "https://app.example/reset"})
	require.NoError(t, err)

	reg, err := svc.Register(context.Background(), auth.RegisterInput{Email: "hana@example.com", Password: "correct horse battery",
		FullName: "Hana", TermsVersion: "2026-10-01", IP: netip.MustParseAddr("203.0.113.7")})
	require.NoError(t, err)
	b, err := svc.Refresh(context.Background(), auth.RefreshInput{RefreshToken: reg.RefreshToken})
	require.NoError(t, err)
	clk.Advance(11 * time.Second)

	_, err = svc.Refresh(context.Background(), auth.RefreshInput{RefreshToken: reg.RefreshToken})
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))
	_, err = svc.Refresh(context.Background(), auth.RefreshInput{RefreshToken: b.RefreshToken})
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err), "the newest token was revoked by the committed reuse reaction")

	var reason string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT revoke_reason FROM refresh_tokens WHERE replaced_by_id IS NULL AND revoke_reason IS NOT NULL ORDER BY created_at DESC LIMIT 1`).Scan(&reason))
	require.Equal(t, "REUSE_DETECTED", reason)
}

type noMail struct{}

func (noMail) Send(context.Context, auth.MailMessage) error { return nil }

func TestPGCountActive(t *testing.T) {
	_, s := newPGAuthStore(t)
	u := repotest.NewUser("hana@example.com")
	ctx := context.Background()
	require.NoError(t, s.WithinTx(ctx, func(ctx context.Context, tx auth.Tx) error {
		require.NoError(t, tx.Users().Insert(ctx, u))
		id := uuid.Must(uuid.NewV7())
		h := make([]byte, 32)
		require.NoError(t, tx.RefreshTokens().Insert(ctx, auth.RefreshToken{ID: id, UserID: u.ID, FamilyID: id, TokenHash: h,
			CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}))
		before, err := tx.RefreshTokens().CountActive(ctx, u.ID, t0)
		require.NoError(t, err)
		after, err := tx.RefreshTokens().CountActive(ctx, u.ID, t0.Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, before)
		require.Equal(t, 0, after, "expires_at == now is no longer active; the time comes from the parameter, not now()")
		return nil
	}))
}
