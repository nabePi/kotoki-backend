package auth_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

type fakeDeleter struct {
	mu    sync.Mutex
	calls []struct {
		user uuid.UUID
		at   time.Time
	}
	err error
}

func (d *fakeDeleter) DeleteAccount(_ context.Context, userID uuid.UUID, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, struct {
		user uuid.UUID
		at   time.Time
	}{userID, at})
	return d.err
}

type userEnv struct {
	svc     *auth.UserService
	store   *fakes.AuthStore
	clk     *clock.Fake
	deleter *fakeDeleter
	logs    *bytes.Buffer
	authEnv *authEnv
}

func newUserEnv(t *testing.T) *userEnv {
	t.Helper()
	ae := newAuthEnv(t)
	clk := ae.clk
	logs := &bytes.Buffer{}
	del := &fakeDeleter{}
	svc := auth.NewUserService(ae.store, ae.hasher, del, clk, slog.New(slog.NewJSONHandler(logs, nil)), termsNow)
	return &userEnv{svc: svc, store: ae.store, clk: clk, deleter: del, logs: logs, authEnv: ae}
}

func TestMeNotFound(t *testing.T) {
	e := newUserEnv(t)
	_, err := e.svc.Me(context.Background(), uuid.New())
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))

	reg := e.authEnv.register(t, "hana@example.com")
	u, err := e.svc.Me(context.Background(), reg.User.ID)
	require.NoError(t, err)
	require.Equal(t, "hana@example.com", u.Email)
	require.Equal(t, termsNow, e.svc.CurrentTermsVersion())
}

func TestUpdateConsents(t *testing.T) {
	e := newUserEnv(t)
	reg := e.authEnv.register(t, "hana@example.com")
	ctx := context.Background()

	_, err := e.svc.UpdateConsents(ctx, auth.ConsentsInput{UserID: reg.User.ID, TermsVersion: "1999-01-01"})
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Equal(t, map[string]any{"fields": []map[string]any{{"field": "terms_version", "reason": "not_current"}}}, errs.DetailsOf(err))

	e.clk.Advance(time.Hour)
	same, err := e.svc.UpdateConsents(ctx, auth.ConsentsInput{UserID: reg.User.ID, TermsVersion: termsNow})
	require.NoError(t, err)
	require.True(t, same.TermsAcceptedAt.Equal(t0), "same version keeps terms_accepted_at")

	e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.TermsVersion = "2025-01-01" })
	fresh, err := e.svc.UpdateConsents(ctx, auth.ConsentsInput{UserID: reg.User.ID, TermsVersion: termsNow})
	require.NoError(t, err)
	require.True(t, fresh.TermsAcceptedAt.Equal(e.clk.Now()), "stale version is re-accepted now")
	require.Contains(t, e.logs.String(), `"audit_event":"consent_updated"`)

	e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = auth.UserStatusSuspended })
	_, err = e.svc.UpdateConsents(ctx, auth.ConsentsInput{UserID: reg.User.ID, TermsVersion: termsNow})
	require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err))
}

func TestDeleteAccount(t *testing.T) {
	e := newUserEnv(t)
	reg := e.authEnv.register(t, "hana@example.com")
	ctx := context.Background()

	err := e.svc.DeleteAccount(ctx, reg.User.ID, "wrong password!!")
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err))
	require.Empty(t, e.deleter.calls)

	for _, st := range []auth.UserStatus{auth.UserStatusSuspended, auth.UserStatusDeleted} {
		e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = st })
		err = e.svc.DeleteAccount(ctx, reg.User.ID, "correct horse battery")
		require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err), string(st))
	}
	require.Empty(t, e.deleter.calls)

	e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = auth.UserStatusActive })
	e.clk.Advance(time.Minute)
	require.NoError(t, e.svc.DeleteAccount(ctx, reg.User.ID, "correct horse battery"))
	require.Len(t, e.deleter.calls, 1)
	require.Equal(t, reg.User.ID, e.deleter.calls[0].user)
	require.True(t, e.deleter.calls[0].at.Equal(e.clk.Now()))
	require.Contains(t, e.logs.String(), `"audit_event":"account_deletion_requested"`)
	require.NotContains(t, e.logs.String(), "correct horse battery")

	e.deleter.err = auth.ErrAccountHasActiveSession
	err = e.svc.DeleteAccount(ctx, reg.User.ID, "correct horse battery")
	require.True(t, errors.Is(err, auth.ErrAccountHasActiveSession))
}

func TestRequireActive(t *testing.T) {
	e := newUserEnv(t)
	reg := e.authEnv.register(t, "hana@example.com")
	u, err := e.svc.RequireActive(context.Background(), reg.User.ID)
	require.NoError(t, err)
	require.Equal(t, reg.User.ID, u.ID)

	for _, st := range []auth.UserStatus{auth.UserStatusSuspended, auth.UserStatusDeleted} {
		e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = st })
		_, err = e.svc.RequireActive(context.Background(), reg.User.ID)
		require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err), string(st))
	}
	_, err = e.svc.RequireActive(context.Background(), uuid.New())
	require.Equal(t, errs.CodeUserNotFound, errs.CodeOf(err))
}
