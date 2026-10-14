//go:build integration

package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

type pgAdmin struct {
	pool  *pgxpool.Pool
	store *postgres.Store
	svc   *admin.Service
	ids   idgen.Generator
}

func newPGAdmin(t *testing.T) pgAdmin {
	t.Helper()
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	clk := clock.NewFake(t0)
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
	svc := admin.NewService(store.AdminStore(), balance.NewService(store.BalanceStore(), clk, ids), clk, ids, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return pgAdmin{pool: pool, store: store, svc: svc, ids: ids}
}

// user seeds a user and its balance account through the production path.
func (p pgAdmin) user(t *testing.T, role auth.Role, seconds int64) uuid.UUID {
	t.Helper()
	u := repotest.NewUser(uuid.NewString() + "@example.com")
	u.Role = role
	require.NoError(t, p.store.AuthStore().WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		if err := tx.Users().Insert(ctx, u); err != nil {
			return err
		}
		return tx.Accounts().OpenAccount(ctx, u.ID, seconds, t0)
	}))
	return u.ID
}

func TestPGAuditRepoSuite(t *testing.T) {
	repotest.AuditRepoSuite(t, func(t *testing.T) admin.Store {
		pool := testutil.NewPostgres(t)
		return postgres.NewStore(pool).AdminStore()
	})
}

func TestPGAuditAppendOnly(t *testing.T) {
	p := newPGAdmin(t)
	adm, target := p.user(t, auth.RoleAdmin, 0), p.user(t, auth.RoleUser, 0)
	_, err := p.svc.Grant(context.Background(), admin.GrantRequest{
		GrantInput: balance.GrantInput{UserID: target, AdminUserID: adm, Seconds: 60, Reason: "support credit", IdempotencyKey: uuid.NewString(), At: t0},
		Meta:       admin.RequestMeta{RequestID: "req-1"},
	})
	require.NoError(t, err)
	_, err = p.pool.Exec(context.Background(), `UPDATE admin_audit_logs SET reason='x'`)
	testutil.RequirePgErr(t, err, "23001", "")
	_, err = p.pool.Exec(context.Background(), `DELETE FROM admin_audit_logs`)
	testutil.RequirePgErr(t, err, "23001", "")
}

func TestPGAdminGrantAtomicWithAudit(t *testing.T) {
	p := newPGAdmin(t)
	adm, target := p.user(t, auth.RoleAdmin, 0), p.user(t, auth.RoleUser, 0)
	ctx := context.Background()
	key := uuid.NewString()

	res, err := p.svc.Grant(ctx, admin.GrantRequest{
		GrantInput: balance.GrantInput{UserID: target, AdminUserID: adm, Seconds: 1800, Reason: "support credit", IdempotencyKey: key, At: t0},
		Meta:       admin.RequestMeta{RequestID: "req-1", UserAgent: "ua"},
	})
	require.NoError(t, err)
	require.Equal(t, int64(1800), res.Account.AvailableSeconds)
	var n int
	var details []byte
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*), max(details::text)::bytea FROM admin_audit_logs WHERE action='BALANCE_GRANT'`).Scan(&n, &details))
	require.Equal(t, 1, n)
	require.Contains(t, string(details), res.Entry.ID.String())

	// an audit row the DB rejects must undo the balance change made in the same tx
	err = p.store.AdminStore().WithinTx(ctx, func(ctx context.Context, tx admin.Tx) error {
		if _, gerr := balance.NewService(p.store.BalanceStore(), clock.NewFake(t0), p.ids).GrantTx(ctx, tx, balance.GrantInput{
			UserID: target, AdminUserID: adm, Seconds: 600, Reason: "will roll back", IdempotencyKey: uuid.NewString(), At: t0}); gerr != nil {
			return gerr
		}
		return tx.Audit().Insert(ctx, admin.AuditLog{ID: p.ids.New(), AdminUserID: adm, Action: "BOGUS", TargetType: "USER",
			TargetID: target.String(), RequestID: "req-2", Details: map[string]any{}, CreatedAt: t0})
	})
	require.Error(t, err)
	var available int64
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT available_seconds FROM balance_accounts WHERE user_id=$1`, target).Scan(&available))
	require.Equal(t, int64(1800), available, "the rolled-back grant left no trace")
}

func TestPGAdminDeleteAccountFlow(t *testing.T) {
	p := newPGAdmin(t)
	ctx := context.Background()
	u := p.user(t, auth.RoleUser, 600)
	require.NoError(t, p.store.AuthStore().WithinTx(ctx, func(ctx context.Context, tx auth.Tx) error {
		id := p.ids.New()
		if err := tx.RefreshTokens().Insert(ctx, auth.RefreshToken{ID: id, UserID: u, FamilyID: id, TokenHash: make([]byte, 32), CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err != nil {
			return err
		}
		h := make([]byte, 32)
		h[0] = 1
		return tx.PasswordResets().Insert(ctx, auth.PasswordResetToken{ID: p.ids.New(), UserID: u, TokenHash: h, CreatedAt: t0, ExpiresAt: t0.Add(15 * time.Minute)})
	}))

	require.NoError(t, p.svc.DeleteAccount(ctx, u, t0))

	var status string
	var deleted *time.Time
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT status, deleted_at FROM users WHERE id=$1`, u).Scan(&status, &deleted))
	require.Equal(t, "DELETED", status)
	require.NotNil(t, deleted)
	for _, q := range []string{`SELECT count(*) FROM refresh_tokens`, `SELECT count(*) FROM password_reset_tokens`} {
		var n int
		require.NoError(t, p.pool.QueryRow(ctx, q).Scan(&n))
		require.Zero(t, n, q)
	}
	var available int64
	var ledger int
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT available_seconds FROM balance_accounts WHERE user_id=$1`, u).Scan(&available))
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM balance_ledger WHERE user_id=$1`, u).Scan(&ledger))
	require.Equal(t, int64(600), available)
	require.Equal(t, 1, ledger)
}

func TestPGSessionGateNoSessions(t *testing.T) {
	p := newPGAdmin(t)
	u := p.user(t, auth.RoleUser, 0)
	require.NoError(t, p.store.AdminStore().WithinTx(context.Background(), func(ctx context.Context, tx admin.Tx) error {
		open, err := tx.SessionGate().OpenSessions(ctx, u)
		require.NoError(t, err)
		require.Empty(t, open)
		return nil
	}))
}

// TestPGSessionGateCancelReservedNotWired pins the M2 stub; M3 replaces CancelReserved
// with session.Service.CancelInTx and turns this into a real cancellation test.
func TestPGSessionGateCancelReservedNotWired(t *testing.T) {
	p := newPGAdmin(t)
	err := p.store.AdminStore().WithinTx(context.Background(), func(ctx context.Context, tx admin.Tx) error {
		return tx.SessionGate().CancelReserved(ctx, uuid.New(), t0)
	})
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err))
	require.Contains(t, err.Error(), "not wired")
}

func TestPGTicketSessionReaderNotFound(t *testing.T) {
	pool := testutil.NewPostgres(t)
	_, err := postgres.NewTicketSessionReader(pool).GetTicketTarget(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, auth.ErrSessionNotFound)
}
