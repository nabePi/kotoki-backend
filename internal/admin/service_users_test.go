package admin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func (e *env) addTokens(t *testing.T, userID uuid.UUID, n int) {
	t.Helper()
	require.NoError(t, e.auth.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		for i := 0; i < n; i++ {
			id := e.ids.New()
			tok := auth.RefreshToken{ID: id, UserID: userID, FamilyID: id, TokenHash: []byte(id.String()), CreatedAt: t0, ExpiresAt: t0.Add(720 * 3600 * 1e9)}
			if err := tx.RefreshTokens().Insert(ctx, tok); err != nil {
				return err
			}
		}
		return nil
	}))
}

func (e *env) addResetToken(t *testing.T, userID uuid.UUID) {
	t.Helper()
	require.NoError(t, e.auth.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		return tx.PasswordResets().Insert(ctx, auth.PasswordResetToken{ID: e.ids.New(), UserID: userID,
			TokenHash: []byte(strings.Repeat("r", 32)), CreatedAt: t0, ExpiresAt: t0.Add(15 * 60 * 1e9)})
	}))
}

func changeIn(e *env, adminID, target uuid.UUID, to auth.UserStatus) admin.ChangeStatusInput {
	return admin.ChangeStatusInput{AdminID: adminID, TargetID: target, To: to, Reason: "abuse report", Meta: e.meta()}
}

func TestChangeStatusSuspend(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	e.addTokens(t, u.ID, 2)

	view, err := e.svc.ChangeStatus(context.Background(), changeIn(e, adm.ID, u.ID, auth.UserStatusSuspended))
	require.NoError(t, err)
	require.Equal(t, auth.UserStatusSuspended, view.User.Status)

	rows := e.store.Audit()
	require.Len(t, rows, 1)
	require.Equal(t, admin.ActionUserStatusChange, rows[0].Action)
	require.Equal(t, map[string]any{"from": "ACTIVE", "to": "SUSPENDED", "revoked_refresh_tokens": int64(2)}, rows[0].Details)
	require.Equal(t, "abuse report", *rows[0].Reason)
	for _, tk := range e.auth.RefreshTokens() {
		require.NotNil(t, tk.RevokeReason)
		require.Equal(t, "USER_STATUS_CHANGED", *tk.RevokeReason)
	}
}

func TestChangeStatusReactivate(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	_, err := e.svc.ChangeStatus(context.Background(), changeIn(e, adm.ID, u.ID, auth.UserStatusSuspended))
	require.NoError(t, err)
	view, err := e.svc.ChangeStatus(context.Background(), changeIn(e, adm.ID, u.ID, auth.UserStatusActive))
	require.NoError(t, err)
	require.Equal(t, auth.UserStatusActive, view.User.Status)
	require.Equal(t, map[string]any{"from": "SUSPENDED", "to": "ACTIVE", "revoked_refresh_tokens": int64(0)}, e.store.Audit()[1].Details)
}

func TestChangeStatusRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	gone := e.user(t, "gone@example.com", auth.RoleUser, 0)
	e.auth.MutateUser(gone.ID, func(x *auth.User) { x.Status = auth.UserStatusDeleted })

	for _, to := range []auth.UserStatus{auth.UserStatusActive, auth.UserStatusSuspended} {
		_, err := e.svc.ChangeStatus(ctx, changeIn(e, adm.ID, gone.ID, to))
		require.Equal(t, errs.CodeInvalidUserStatusTransition, errs.CodeOf(err), string(to))
	}
	_, err := e.svc.ChangeStatus(ctx, changeIn(e, adm.ID, adm.ID, auth.UserStatusSuspended))
	require.Equal(t, errs.CodeCannotModifySelf, errs.CodeOf(err))
	_, err = e.svc.ChangeStatus(ctx, changeIn(e, adm.ID, uuid.New(), auth.UserStatusSuspended))
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))

	for _, reason := range []string{"ab", strings.Repeat("x", 501)} {
		in := changeIn(e, adm.ID, u.ID, auth.UserStatusSuspended)
		in.Reason = reason
		_, err = e.svc.ChangeStatus(ctx, in)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}

	require.Empty(t, e.store.Audit())
	same, err := e.svc.ChangeStatus(ctx, changeIn(e, adm.ID, u.ID, auth.UserStatusActive))
	require.NoError(t, err)
	require.Equal(t, auth.UserStatusActive, same.User.Status)
	require.Empty(t, e.store.Audit(), "no-op change writes no audit row")
}

func TestChangeStatusToDeleted(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	u := e.user(t, "hana@example.com", auth.RoleUser, 600)
	e.addTokens(t, u.ID, 2)
	e.addResetToken(t, u.ID)
	reserved := uuid.New()
	e.store.Gate().SetOpen(u.ID, admin.OpenSession{ID: reserved, Status: "RESERVED"})
	ledgerBefore := len(e.bal.Ledger())

	_, err := e.svc.ChangeStatus(context.Background(), changeIn(e, adm.ID, u.ID, auth.UserStatusDeleted))
	require.NoError(t, err)

	got := e.auth.User(u.ID)
	require.Equal(t, auth.UserStatusDeleted, got.Status)
	require.NotNil(t, got.DeletedAt)
	require.True(t, got.DeletedAt.Equal(t0))
	require.Empty(t, e.auth.RefreshTokens())
	require.Empty(t, e.auth.ResetTokens())
	require.Equal(t, []uuid.UUID{reserved}, e.store.Gate().Cancelled())
	require.Equal(t, int64(600), e.bal.Account(u.ID).AvailableSeconds, "balance untouched")
	require.Len(t, e.bal.Ledger(), ledgerBefore)
	require.Len(t, e.store.Audit(), 1)
	require.Equal(t, "DELETED", e.store.Audit()[0].Details["to"])
}

func TestDeleteAccountActiveSessionBlocks(t *testing.T) {
	for _, st := range []string{"STARTING", "ACTIVE", "ENDING"} {
		e := newEnv(t)
		u := e.user(t, "hana@example.com", auth.RoleUser, 0)
		e.addTokens(t, u.ID, 1)
		sid := uuid.New()
		e.store.Gate().SetOpen(u.ID, admin.OpenSession{ID: sid, Status: st})

		err := e.svc.DeleteAccount(context.Background(), u.ID, t0)
		require.Equal(t, errs.CodeAccountHasActiveSession, errs.CodeOf(err), st)
		require.True(t, errors.Is(err, auth.ErrAccountHasActiveSession))
		require.Equal(t, map[string]any{"session_id": sid.String()}, errs.DetailsOf(err))

		require.Equal(t, auth.UserStatusActive, e.auth.User(u.ID).Status)
		require.Len(t, e.auth.RefreshTokens(), 1, "tokens survive a refused deletion")
		require.Empty(t, e.store.Audit())
		require.Empty(t, e.store.Gate().Cancelled())
	}
}

func TestDeleteAccountLockOrder(t *testing.T) {
	e := newEnv(t)
	u := e.user(t, "hana@example.com", auth.RoleUser, 0)
	e.store.Rec.Reset()
	require.NoError(t, e.svc.DeleteAccount(context.Background(), u.ID, t0))
	require.Equal(t, []string{"Balance.LockAccountByUserID", "Users.LockByID", "SessionGate.OpenSessions"}, e.store.Rec.Calls())
}

func TestGetUserDetail(t *testing.T) {
	e := newEnv(t)
	u := e.user(t, "hana@example.com", auth.RoleUser, 600)
	e.addTokens(t, u.ID, 3)
	sid := uuid.New()
	e.store.Gate().SetOpen(u.ID, admin.OpenSession{ID: sid, Status: "ACTIVE"})

	d, err := e.svc.GetUser(context.Background(), u.ID)
	require.NoError(t, err)
	require.Equal(t, u.ID, d.User.ID)
	require.Equal(t, int64(600), d.Balance.AvailableSeconds)
	require.NotNil(t, d.OpenSessionID)
	require.Equal(t, sid, *d.OpenSessionID)
	require.Equal(t, 3, d.ActiveRefreshTokenCount)

	_, err = e.svc.GetUser(context.Background(), uuid.New())
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))
}

func TestListUsersViews(t *testing.T) {
	e := newEnv(t)
	a := e.user(t, "hana1@example.com", auth.RoleUser, 300)
	b := e.user(t, "hana2@example.com", auth.RoleUser, 900)
	e.user(t, "other@example.com", auth.RoleAdmin, 0)

	views, next, err := e.svc.ListUsers(context.Background(), auth.UserFilter{EmailPrefix: "hana"}, nil, 10)
	require.NoError(t, err)
	require.Len(t, views, 2)
	require.Nil(t, next)
	got := map[uuid.UUID]int64{}
	for _, v := range views {
		got[v.User.ID] = v.Balance.AvailableSeconds
	}
	require.Equal(t, map[uuid.UUID]int64{a.ID: 300, b.ID: 900}, got)

	p1, next, err := e.svc.ListUsers(context.Background(), auth.UserFilter{}, nil, 2)
	require.NoError(t, err)
	require.Len(t, p1, 2)
	require.NotNil(t, next)
	require.Equal(t, p1[1].User.ID, *next)
	p2, next2, err := e.svc.ListUsers(context.Background(), auth.UserFilter{}, next, 2)
	require.NoError(t, err)
	require.Len(t, p2, 1)
	require.Nil(t, next2)
}
