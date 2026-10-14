package admin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestAdminGrantWritesAudit(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	key := uuid.NewString()

	res, err := e.svc.Grant(context.Background(), grantReq(e, adm.ID, u.ID, 1800, key))
	require.NoError(t, err)
	require.Equal(t, int64(1800), res.Account.AvailableSeconds)

	rows := e.store.Audit()
	require.Len(t, rows, 1)
	a := rows[0]
	require.Equal(t, admin.ActionBalanceGrant, a.Action)
	require.Equal(t, "USER", a.TargetType)
	require.Equal(t, u.ID.String(), a.TargetID)
	require.Equal(t, u.ID, *a.TargetUserID)
	require.Equal(t, adm.ID, a.AdminUserID)
	require.Equal(t, key, *a.IdempotencyKey)
	require.Equal(t, "req-123", a.RequestID)
	require.Equal(t, map[string]any{"seconds": int64(1800), "ledger_entry_id": res.Entry.ID.String(), "available_after": int64(1800)}, a.Details)
}

func TestAdminGrantReplayNoSecondAudit(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	req := grantReq(e, adm.ID, u.ID, 600, uuid.NewString())
	_, err := e.svc.Grant(context.Background(), req)
	require.NoError(t, err)
	res, err := e.svc.Grant(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.Replayed)
	require.Len(t, e.store.Audit(), 1)
}

func TestAdminGrantAuditFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	e.store.FailNext("Audit.Insert", errors.New("audit down"))

	_, err := e.svc.Grant(context.Background(), grantReq(e, adm.ID, u.ID, 600, uuid.NewString()))
	require.EqualError(t, err, "audit down")
	require.Equal(t, int64(0), e.bal.Account(u.ID).AvailableSeconds, "a grant without its audit row must not exist")
	require.Empty(t, e.bal.Ledger())
	require.Empty(t, e.store.Audit())
}

func TestAdminGrantUnknownUser(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	_, err := e.svc.Grant(context.Background(), grantReq(e, adm.ID, uuid.New(), 60, uuid.NewString()))
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))
}

func TestAdminGrantDuplicateMovementRetry(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	e.bal.FailNext("ApplyMovement", balance.ErrDuplicateMovement)
	res, err := e.svc.Grant(context.Background(), grantReq(e, adm.ID, u.ID, 300, uuid.NewString()))
	require.NoError(t, err)
	require.Equal(t, int64(300), res.Account.AvailableSeconds)
	require.Len(t, e.store.Audit(), 1)
}

func TestAdminAdjustAuditDetails(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 900)

	res, err := e.svc.Adjust(context.Background(), adjustReq(e, adm.ID, u.ID, -300, uuid.NewString()))
	require.NoError(t, err)
	rows := e.store.Audit()
	require.Len(t, rows, 1)
	require.Equal(t, admin.ActionBalanceAdjustment, rows[0].Action)
	require.Equal(t, map[string]any{
		"seconds_delta": int64(-300), "ledger_entry_id": res.Entry.ID.String(),
		"available_before": int64(900), "available_after": int64(600),
	}, rows[0].Details)

	_, err = e.svc.Adjust(context.Background(), adjustReq(e, adm.ID, u.ID, -601, uuid.NewString()))
	require.Equal(t, errs.CodeBalanceAdjustmentNegative, errs.CodeOf(err))
	require.Len(t, e.store.Audit(), 1, "a rejected adjustment writes no audit row")
}

func TestAdminListAuditFilters(t *testing.T) {
	e := newEnv(t)
	a1 := e.user(t, "a1@example.com", auth.RoleAdmin, 0)
	a2 := e.user(t, "a2@example.com", auth.RoleAdmin, 0)
	u1 := e.user(t, "u1@example.com", auth.RoleUser, 100)
	u2 := e.user(t, "u2@example.com", auth.RoleUser, 100)
	ctx := context.Background()
	for _, r := range []struct {
		adm, target uuid.UUID
		grant       bool
	}{{a1.ID, u1.ID, true}, {a1.ID, u2.ID, true}, {a2.ID, u1.ID, true}, {a1.ID, u1.ID, false}} {
		var err error
		if r.grant {
			_, err = e.svc.Grant(ctx, grantReq(e, r.adm, r.target, 60, uuid.NewString()))
		} else {
			_, err = e.svc.Adjust(ctx, adjustReq(e, r.adm, r.target, -10, uuid.NewString()))
		}
		require.NoError(t, err)
	}

	all, next, err := e.svc.ListAudit(ctx, admin.AuditFilter{}, nil, 10)
	require.NoError(t, err)
	require.Len(t, all, 4)
	require.Nil(t, next)

	byAdmin, _, err := e.svc.ListAudit(ctx, admin.AuditFilter{AdminUserID: &a2.ID}, nil, 10)
	require.NoError(t, err)
	require.Len(t, byAdmin, 1)
	byTarget, _, err := e.svc.ListAudit(ctx, admin.AuditFilter{TargetUserID: &u1.ID}, nil, 10)
	require.NoError(t, err)
	require.Len(t, byTarget, 3)
	byAction, _, err := e.svc.ListAudit(ctx, admin.AuditFilter{Action: admin.ActionBalanceAdjustment}, nil, 10)
	require.NoError(t, err)
	require.Len(t, byAction, 1)

	p1, next, err := e.svc.ListAudit(ctx, admin.AuditFilter{}, nil, 3)
	require.NoError(t, err)
	require.Len(t, p1, 3)
	require.NotNil(t, next)
	require.Equal(t, p1[2].ID, *next)
	p2, next2, err := e.svc.ListAudit(ctx, admin.AuditFilter{}, next, 3)
	require.NoError(t, err)
	require.Len(t, p2, 1)
	require.Nil(t, next2)
}

func TestAdminListTransactions(t *testing.T) {
	e := newEnv(t)
	u := e.user(t, "hana@example.com", auth.RoleUser, 600)
	got, next, err := e.svc.ListTransactions(context.Background(), u.ID, nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Nil(t, next)
	_, _, err = e.svc.ListTransactions(context.Background(), uuid.New(), nil, nil, 10)
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))
}

func TestIsActiveAdmin(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	usr := e.user(t, "hana@example.com", auth.RoleUser, 0)
	sus := e.user(t, "sus@example.com", auth.RoleAdmin, 0)
	e.auth.MutateUser(sus.ID, func(u *auth.User) { u.Status = auth.UserStatusSuspended })

	for id, want := range map[uuid.UUID]bool{adm.ID: true, usr.ID: false, sus.ID: false, uuid.New(): false} {
		ok, err := e.svc.IsActiveAdmin(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, want, ok)
	}
}
