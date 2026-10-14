package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func registerInput(email string) auth.RegisterInput {
	return auth.RegisterInput{Email: email, Password: "correct horse battery", FullName: "Hana Tanaka",
		TermsVersion: termsNow, UserAgent: "test-agent", IP: testIP}
}

func TestRegisterSuccess(t *testing.T) {
	e := newAuthEnv(t)
	res := e.register(t, "Hana@Example.com")

	u := e.store.User(res.User.ID)
	require.Equal(t, "hana@example.com", u.Email)
	require.Equal(t, auth.RoleUser, u.Role)
	require.Equal(t, auth.UserStatusActive, u.Status)
	require.True(t, u.TermsAcceptedAt.Equal(t0))
	require.True(t, strings.HasPrefix(u.PasswordHash, "$argon2id$"))
	acc := e.bal.Account(u.ID)
	require.Equal(t, int64(0), acc.AvailableSeconds)
	require.Equal(t, int64(0), acc.ReservedSeconds)

	tokens := e.store.RefreshTokens()
	require.Len(t, tokens, 1)
	require.Equal(t, tokens[0].ID, tokens[0].FamilyID)
	require.Nil(t, tokens[0].ParentID)
	require.True(t, tokens[0].ExpiresAt.Equal(t0.Add(720*60*60*1e9)))
	require.Equal(t, 15*60*1e9, float64(res.ExpiresIn))
	require.NotEmpty(t, res.RefreshToken)
	require.Len(t, res.RefreshToken, 43)

	claims, err := e.tokens.ParseAccess(res.AccessToken, t0)
	require.NoError(t, err)
	require.Equal(t, auth.RoleUser, claims.Role)
	require.Equal(t, tokens[0].FamilyID, claims.FamilyID)
}

func TestRegisterTermsVersion(t *testing.T) {
	e := newAuthEnv(t)
	in := registerInput("a@example.com")
	in.TermsVersion = ""
	_, err := e.svc.Register(context.Background(), in)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Equal(t, map[string]any{"fields": []map[string]any{{"field": "terms_version", "reason": "required"}}}, errs.DetailsOf(err))

	in.TermsVersion = "2020-01-01"
	_, err = e.svc.Register(context.Background(), in)
	require.Equal(t, map[string]any{"fields": []map[string]any{{"field": "terms_version", "reason": "not_current"}}}, errs.DetailsOf(err))
}

func TestRegisterConflicts(t *testing.T) {
	e := newAuthEnv(t)
	phone := "+628123456789"
	in := registerInput("a@example.com")
	in.PhoneNumber = &phone
	_, err := e.svc.Register(context.Background(), in)
	require.NoError(t, err)

	_, err = e.svc.Register(context.Background(), registerInput("A@example.com"))
	require.Equal(t, errs.CodeEmailAlreadyRegistered, errs.CodeOf(err))

	dup := registerInput("b@example.com")
	dup.PhoneNumber = &phone
	_, err = e.svc.Register(context.Background(), dup)
	require.Equal(t, errs.CodePhoneAlreadyRegistered, errs.CodeOf(err))

	weak := registerInput("c@example.com")
	weak.Password = "short"
	_, err = e.svc.Register(context.Background(), weak)
	require.Equal(t, errs.CodePasswordTooWeak, errs.CodeOf(err))

	require.Len(t, e.store.RefreshTokens(), 1, "failed registrations leave no state")
	require.Len(t, e.bal.Ledger(), 0)
}

func TestRegisterInitialBalance(t *testing.T) {
	e := newAuthEnv(t, func(c *auth.Config) { c.InitialBalanceSeconds = 3600 })
	res := e.register(t, "a@example.com")
	require.Equal(t, int64(3600), e.bal.Account(res.User.ID).AvailableSeconds)
	led := e.bal.Ledger()
	require.Len(t, led, 1)
	require.Equal(t, balance.EntryGrant, led[0].EntryType)
	require.Equal(t, "grant:initial:"+res.User.ID.String(), led[0].IdempotencyKey)
}

func TestRegisterRollsBackWhenAccountOpeningFails(t *testing.T) {
	e := newAuthEnv(t)
	e.store.FailNext("OpenAccount", context.DeadlineExceeded)
	_, err := e.svc.Register(context.Background(), registerInput("a@example.com"))
	require.Error(t, err)
	_, err = e.svc.Login(context.Background(), auth.LoginInput{Email: "a@example.com", Password: "correct horse battery", IP: testIP})
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err), "user insert was rolled back")
}

func TestLoginSuccess(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	e.clk.Advance(time.Hour)

	res, err := e.svc.Login(context.Background(), auth.LoginInput{Email: "HANA@example.com", Password: "correct horse battery", UserAgent: "ua", IP: testIP})
	require.NoError(t, err)
	u := e.store.User(reg.User.ID)
	require.NotNil(t, u.LastLoginAt)
	require.True(t, u.LastLoginAt.Equal(t0.Add(time.Hour)))
	require.Len(t, e.store.RefreshTokens(), 2)
	require.NotEqual(t, reg.RefreshToken, res.RefreshToken)
	claims, err := e.tokens.ParseAccess(res.AccessToken, e.clk.Now())
	require.NoError(t, err)
	require.NotEqual(t, newUUID(), claims.FamilyID)
}

func TestLoginInvalid(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")

	_, err := e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "wrong password!!", IP: testIP})
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err))

	before := e.hasher.count()
	_, err = e.svc.Login(context.Background(), auth.LoginInput{Email: "nobody@example.com", Password: "whatever password", IP: testIP})
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err))
	require.Equal(t, before+1, e.hasher.count(), "unknown email still runs exactly one Verify (dummy hash)")

	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		return tx.Users().UpdateStatus(ctx, reg.User.ID, auth.UserStatusDeleted, t0)
	}))
	before = e.hasher.count()
	_, err = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "correct horse battery", IP: testIP})
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err))
	require.Equal(t, before+1, e.hasher.count())
}

func TestLoginSuspended(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		return tx.Users().UpdateStatus(ctx, reg.User.ID, auth.UserStatusSuspended, t0)
	}))
	_, err := e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "correct horse battery", IP: testIP})
	require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err))
	_, err = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "wrong password!!", IP: testIP})
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err), "status is only revealed after a correct password")
}

func TestLoginRehash(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	oldHash := e.store.User(reg.User.ID).PasswordHash
	e.hasher.forceRehash = true
	e.clk.Advance(time.Hour)

	_, err := e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "correct horse battery", IP: testIP})
	require.NoError(t, err)
	u := e.store.User(reg.User.ID)
	require.NotEqual(t, oldHash, u.PasswordHash)
	require.True(t, u.PasswordChangedAt.Equal(t0), "rehash must not touch password_changed_at")
}

func TestAuthEventsRegisterLogin(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	_, _ = e.svc.Register(context.Background(), registerInput("hana@example.com"))
	_, _ = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "correct horse battery", IP: testIP})
	_, _ = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "nope nope nope!", IP: testIP})
	require.Equal(t, [][2]string{
		{"register", "success"}, {"register", "failure"}, {"login", "success"}, {"login", "failure"},
	}, e.events.all())
}

func TestAuthLogsNoSecrets(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	_, _ = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "correct horse battery", IP: testIP})
	_, _ = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "wrong password!!", IP: testIP})

	logs := e.logs.String()
	for _, secret := range []string{"correct horse battery", "wrong password!!", "hana@example.com"} {
		require.NotContains(t, logs, secret)
	}
	for _, ev := range []string{`"audit_event":"register"`, `"audit_event":"login_succeeded"`, `"audit_event":"login_failed"`} {
		require.Contains(t, logs, ev)
	}
	require.Contains(t, logs, `"audit":true`)
}
