package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func tokenFromMail(t *testing.T, m auth.MailMessage) string {
	t.Helper()
	const marker = "https://app.example/reset-password?token="
	i := strings.Index(m.TextBody, marker)
	require.GreaterOrEqual(t, i, 0, m.TextBody)
	rest := m.TextBody[i+len(marker):]
	if j := strings.IndexAny(rest, "\r\n "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func (e *authEnv) forgot(email string) error {
	return e.svc.ForgotPassword(context.Background(), auth.ForgotInput{Email: email, IP: testIP})
}

func (e *authEnv) requestReset(t *testing.T, email string) (raw string) {
	t.Helper()
	n := len(e.mail.all())
	require.NoError(t, e.forgot(email))
	return tokenFromMail(t, e.waitMail(t, n+1)[n])
}

func (e *authEnv) reset(raw, pw string) error {
	return e.svc.ResetPassword(context.Background(), auth.ResetInput{Token: raw, NewPassword: pw})
}

func TestForgotActiveUser(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	raw := e.requestReset(t, "Hana@Example.com")

	tokens := e.store.ResetTokens()
	require.Len(t, tokens, 1)
	require.Equal(t, reg.User.ID, tokens[0].UserID)
	require.True(t, tokens[0].ExpiresAt.Equal(t0.Add(15*time.Minute)))
	b, err := base64.RawURLEncoding.DecodeString(raw)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	require.Equal(t, sum[:], tokens[0].TokenHash)

	msg := e.mail.all()[0]
	require.Equal(t, "hana@example.com", msg.To)
	require.Equal(t, "password_reset", msg.Tag)
	for _, tk := range tokens {
		require.NotContains(t, string(tk.TokenHash), raw, "raw token must not be stored")
	}
}

func TestForgotInvalidatesPrevious(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	first := e.requestReset(t, "hana@example.com")
	e.clk.Advance(time.Minute)
	second := e.requestReset(t, "hana@example.com")
	require.NotEqual(t, first, second)

	require.Equal(t, errs.CodeResetTokenInvalid, errs.CodeOf(e.reset(first, "brand new password 1")))
	require.NoError(t, e.reset(second, "brand new password 1"))
}

func TestForgotUnknownSuspendedLimited(t *testing.T) {
	e := newAuthEnv(t)
	sus := e.register(t, "sus@example.com")
	e.store.MutateUser(sus.User.ID, func(u *auth.User) { u.Status = auth.UserStatusSuspended })
	e.register(t, "ok@example.com")

	require.NoError(t, e.forgot("nobody@example.com"))
	require.NoError(t, e.forgot("sus@example.com"))
	require.NoError(t, e.svc.ForgotPassword(context.Background(), auth.ForgotInput{Email: "ok@example.com", IP: testIP, EmailLimited: true}))

	require.Empty(t, e.store.ResetTokens())
	require.Empty(t, e.drainMail(t))
}

func TestForgotInvalidEmailFormat(t *testing.T) {
	e := newAuthEnv(t)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(e.forgot("not-an-email")))
}

func TestResetSuccess(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	other := e.requestReset(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	_ = other
	e.clk.Advance(time.Minute)

	require.NoError(t, e.reset(raw, "brand new password 1"))

	u := e.store.User(reg.User.ID)
	ok, err := e.hasher.Verify(context.Background(), "brand new password 1", u.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, u.PasswordChangedAt.Equal(e.clk.Now()))

	used := 0
	for _, tk := range e.store.ResetTokens() {
		if tk.UsedAt != nil {
			used++
		} else {
			require.NotNil(t, tk.InvalidatedAt, "every other open token is invalidated")
		}
	}
	require.Equal(t, 1, used)
	for _, rt := range e.store.RefreshTokens() {
		require.NotNil(t, rt.RevokeReason)
		require.Equal(t, "PASSWORD_RESET", *rt.RevokeReason)
	}
	_, err = e.svc.Login(context.Background(), auth.LoginInput{Email: "hana@example.com", Password: "brand new password 1", IP: testIP})
	require.NoError(t, err)
}

func TestResetTwice(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	require.NoError(t, e.reset(raw, "brand new password 1"))
	require.Equal(t, errs.CodeResetTokenInvalid, errs.CodeOf(e.reset(raw, "another new password 2")))
}

func TestResetExpiredBoundary(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	e.clk.Advance(15*time.Minute - time.Millisecond)
	require.NoError(t, e.reset(raw, "brand new password 1"))

	e2 := newAuthEnv(t)
	e2.register(t, "hana@example.com")
	raw2 := e2.requestReset(t, "hana@example.com")
	e2.clk.Advance(15 * time.Minute)
	require.Equal(t, errs.CodeResetTokenInvalid, errs.CodeOf(e2.reset(raw2, "brand new password 1")), "now == expires_at is expired")
}

func TestResetWeakPasswordKeepsToken(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	err := e.reset(raw, "short")
	require.Equal(t, errs.CodePasswordTooWeak, errs.CodeOf(err))
	require.NoError(t, e.reset(raw, "brand new password 1"), "a weak attempt does not burn the token")
}

func TestResetMalformedToken(t *testing.T) {
	e := newAuthEnv(t)
	for _, raw := range []string{"", "short", strings.Repeat("!", 43), base64.RawURLEncoding.EncodeToString(make([]byte, 16))} {
		require.Equal(t, errs.CodeResetTokenInvalid, errs.CodeOf(e.reset(raw, "brand new password 1")), raw)
	}
	// well-formed but unknown
	require.Equal(t, errs.CodeResetTokenInvalid, errs.CodeOf(e.reset(base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "brand new password 1")))
}

func TestResetUserNotActive(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = auth.UserStatusSuspended })
	require.Equal(t, errs.CodeResetTokenInvalid, errs.CodeOf(e.reset(raw, "brand new password 1")))
}

func TestResetEvents(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	_ = e.reset("garbage", "brand new password 1")
	_ = e.reset(raw, "short")
	require.NoError(t, e.reset(raw, "brand new password 1"))

	got := e.events.all()
	require.Equal(t, [][2]string{
		{"register", "success"}, {"password_reset", "failure"}, {"password_reset", "failure"}, {"password_reset", "success"},
	}, got)
}

func TestForgotLogsNoSecrets(t *testing.T) {
	e := newAuthEnv(t)
	e.register(t, "hana@example.com")
	raw := e.requestReset(t, "hana@example.com")
	require.NoError(t, e.reset(raw, "brand new password 1"))

	logs := e.logs.String()
	for _, secret := range []string{raw, "hana@example.com", "brand new password 1"} {
		require.NotContains(t, logs, secret)
	}
	require.Contains(t, logs, `"audit_event":"password_reset_requested"`)
	require.Contains(t, logs, `"audit_event":"password_reset_completed"`)
}
