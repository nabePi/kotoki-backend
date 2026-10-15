package auth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func (e *authEnv) login(email string) (auth.AuthResult, error) {
	return e.svc.Login(context.Background(), auth.LoginInput{Email: email, Password: "correct horse battery", UserAgent: "ua", IP: testIP})
}

// A reset that commits between password verification and token issue must win:
// no refresh family may survive it and the rehash must not restore the old hash.
func TestLoginRacingPasswordReset(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	e.hasher.forceRehash = true
	const resetHash = "reset-hash-sentinel"
	e.hasher.afterVerify = func() {
		e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.PasswordHash = resetHash })
		e.hasher.afterVerify = nil
	}
	before := len(e.store.RefreshTokens())

	_, err := e.login("hana@example.com")
	require.Equal(t, errs.CodeInvalidCredentials, errs.CodeOf(err))
	require.Len(t, e.store.RefreshTokens(), before, "no family minted after the reset")
	require.Equal(t, resetHash, e.store.User(reg.User.ID).PasswordHash, "rehash must not overwrite the reset hash")
}

func TestLoginRacingSuspension(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	e.hasher.afterVerify = func() {
		e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Status = auth.UserStatusSuspended })
		e.hasher.afterVerify = nil
	}
	_, err := e.login("hana@example.com")
	require.Equal(t, errs.CodeAccountSuspended, errs.CodeOf(err))
}
