package admin_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type env struct {
	svc   *admin.Service
	store *fakes.AdminStore
	auth  *fakes.AuthStore
	bal   *fakes.BalanceStore
	clk   *clock.Fake
	ids   idgen.Generator
	logs  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ids := idgen.NewSequence(t)
	bal := fakes.NewBalanceStore(fakes.WithBalanceIDs(ids))
	as := fakes.NewAuthStore(bal, ids)
	store := fakes.NewAdminStore(as, bal)
	clk := clock.NewFake(t0)
	logs := &bytes.Buffer{}
	svc := admin.NewService(store, balance.NewService(bal, clk, ids), clk, ids, slog.New(slog.NewJSONHandler(logs, nil)))
	return &env{svc: svc, store: store, auth: as, bal: bal, clk: clk, ids: ids, logs: logs}
}

// user creates a user with an account holding `seconds` available.
func (e *env) user(t *testing.T, email string, role auth.Role, seconds int64) auth.User {
	t.Helper()
	u := repotest.NewUser(email)
	u.ID = e.ids.New()
	u.Role = role
	require.NoError(t, e.auth.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		if err := tx.Users().Insert(ctx, u); err != nil {
			return err
		}
		return tx.Accounts().OpenAccount(ctx, u.ID, seconds, t0)
	}))
	return u
}

func (e *env) meta() admin.RequestMeta {
	return admin.RequestMeta{RequestID: "req-123", UserAgent: "ua"}
}

func grantReq(e *env, adminID, target uuid.UUID, seconds int64, key string) admin.GrantRequest {
	return admin.GrantRequest{
		GrantInput: balance.GrantInput{UserID: target, AdminUserID: adminID, Seconds: seconds, Reason: "support credit", IdempotencyKey: key, At: t0},
		Meta:       e.meta(),
	}
}

func adjustReq(e *env, adminID, target uuid.UUID, delta int64, key string) admin.AdjustRequest {
	return admin.AdjustRequest{
		AdjustInput: balance.AdjustInput{UserID: target, AdminUserID: adminID, SecondsDelta: delta, Reason: "manual fix", IdempotencyKey: key, At: t0},
		Meta:        e.meta(),
	}
}
