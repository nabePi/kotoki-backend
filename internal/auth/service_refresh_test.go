package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func rawHash(t *testing.T, raw string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(raw)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return sum[:]
}

func (e *authEnv) tokenByRaw(t *testing.T, raw string) auth.RefreshToken {
	t.Helper()
	h := rawHash(t, raw)
	for _, tk := range e.store.RefreshTokens() {
		if string(tk.TokenHash) == string(h) {
			return tk
		}
	}
	t.Fatal("refresh token not stored")
	return auth.RefreshToken{}
}

func (e *authEnv) refresh(raw string) (auth.RefreshResult, error) {
	return e.svc.Refresh(context.Background(), auth.RefreshInput{RefreshToken: raw, UserAgent: "ua", IP: testIP})
}

func TestRefreshRotates(t *testing.T) {
	e := newAuthEnv(t)
	reg := e.register(t, "hana@example.com")
	e.store.MutateUser(reg.User.ID, func(u *auth.User) { u.Role = auth.RoleAdmin })
	e.clk.Advance(time.Minute)

	res, err := e.refresh(reg.RefreshToken)
	require.NoError(t, err)
	old, next := e.tokenByRaw(t, reg.RefreshToken), e.tokenByRaw(t, res.RefreshToken)
	require.Equal(t, old.FamilyID, next.FamilyID)
	require.NotNil(t, next.ParentID)
	require.Equal(t, old.ID, *next.ParentID)
	require.NotNil(t, old.RevokeReason)
	require.Equal(t, "ROTATED", *old.RevokeReason)
	require.NotNil(t, old.ReplacedByID)
	require.Equal(t, next.ID, *old.ReplacedByID)
	require.True(t, next.ExpiresAt.Equal(e.clk.Now().Add(720*time.Hour)))
	require.True(t, res.RefreshExpiresAt.Equal(next.ExpiresAt))

	claims, err := e.tokens.ParseAccess(res.AccessToken, e.clk.Now())
	require.NoError(t, err)
	require.Equal(t, auth.RoleAdmin, claims.Role, "role comes from the DB, not the old token")
	require.Equal(t, old.FamilyID, claims.FamilyID)
	require.Equal(t, 15*time.Minute, res.ExpiresIn)
}

func TestRefreshReuseOutsideGrace(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	b, err := e.refresh(a.RefreshToken)
	require.NoError(t, err)
	e.clk.Advance(10*time.Second + time.Millisecond)

	_, err = e.refresh(a.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))

	tb := e.tokenByRaw(t, b.RefreshToken)
	require.NotNil(t, tb.RevokeReason, "the revoke must survive the 401 (commit-then-error)")
	require.Equal(t, "REUSE_DETECTED", *tb.RevokeReason)
	_, err = e.refresh(b.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))

	logs := e.logs.String()
	require.Contains(t, logs, `"level":"WARN"`)
	require.Contains(t, logs, "refresh_reuse_detected")
	require.NotContains(t, logs, a.RefreshToken)
	require.NotContains(t, logs, b.RefreshToken)
}

func TestRefreshWithinGrace(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	b, err := e.refresh(a.RefreshToken)
	require.NoError(t, err)

	e.clk.Advance(10 * time.Second)
	_, err = e.refresh(a.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenRotated, errs.CodeOf(err))
	_, err = e.refresh(b.RefreshToken)
	require.NoError(t, err, "the newest token stays valid after a 409")

	e2 := newAuthEnv(t)
	a2 := e2.register(t, "hana@example.com")
	b2, err := e2.refresh(a2.RefreshToken)
	require.NoError(t, err)
	e2.clk.Advance(10*time.Second + time.Millisecond)
	_, err = e2.refresh(a2.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))
	_, err = e2.refresh(b2.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))
}

func TestRefreshGraceButReplacementRevoked(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	b, err := e.refresh(a.RefreshToken)
	require.NoError(t, err)
	tb := e.tokenByRaw(t, b.RefreshToken)
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		_, rerr := tx.RefreshTokens().RevokeFamily(ctx, tb.FamilyID, "LOGOUT", e.clk.Now())
		return rerr
	}))
	_, err = e.refresh(a.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))
}

func TestRefreshExpired(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	e.clk.Advance(720*time.Hour - time.Millisecond)
	_, err := e.refresh(a.RefreshToken)
	require.NoError(t, err, "one millisecond before expiry is still valid")

	e2 := newAuthEnv(t)
	a2 := e2.register(t, "hana@example.com")
	e2.clk.Advance(720 * time.Hour)
	_, err = e2.refresh(a2.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err), "now == expires_at is expired")
}

func TestRefreshSuspendedUser(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	e.store.MutateUser(a.User.ID, func(u *auth.User) { u.Status = auth.UserStatusSuspended })

	_, err := e.refresh(a.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))
	tk := e.tokenByRaw(t, a.RefreshToken)
	require.NotNil(t, tk.RevokeReason)
	require.Equal(t, "USER_STATUS_CHANGED", *tk.RevokeReason, "revoke persisted despite the error")
}

func TestRefreshUnknownToken(t *testing.T) {
	e := newAuthEnv(t)
	for _, raw := range []string{"", "garbage", "!!!not-base64!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!", base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		_, err := e.refresh(raw)
		require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err), raw)
	}
}

func TestRefreshConcurrent(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.refresh(a.RefreshToken)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, rotated := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errs.CodeOf(err) == errs.CodeRefreshTokenRotated:
			rotated++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, rotated)
}

func TestLogout(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	b, err := e.refresh(a.RefreshToken)
	require.NoError(t, err)
	e.clk.Advance(time.Minute)

	require.NoError(t, e.svc.Logout(context.Background(), b.RefreshToken))
	tk := e.tokenByRaw(t, b.RefreshToken)
	require.NotNil(t, tk.RevokeReason)
	require.Equal(t, "LOGOUT", *tk.RevokeReason)
	_, err = e.refresh(b.RefreshToken)
	require.Equal(t, errs.CodeRefreshTokenInvalid, errs.CodeOf(err))

	require.NoError(t, e.svc.Logout(context.Background(), "garbage"))
	require.NoError(t, e.svc.Logout(context.Background(), ""))
}

func TestRefreshEvents(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	b, err := e.refresh(a.RefreshToken)
	require.NoError(t, err)
	_, _ = e.refresh("garbage")
	e.clk.Advance(time.Minute)
	_, _ = e.refresh(a.RefreshToken) // reuse outside grace
	_ = b

	got := e.events.all()
	require.Equal(t, [2]string{"register", "success"}, got[0])
	require.Equal(t, [2]string{"refresh", "success"}, got[1])
	require.Equal(t, [2]string{"refresh", "failure"}, got[2])
	require.Equal(t, [2]string{"refresh_reuse", "failure"}, got[3])
}

func TestRefreshLogsNoToken(t *testing.T) {
	e := newAuthEnv(t)
	a := e.register(t, "hana@example.com")
	b, err := e.refresh(a.RefreshToken)
	require.NoError(t, err)
	require.NoError(t, e.svc.Logout(context.Background(), b.RefreshToken))
	_, _ = e.refresh("garbage-token-value")
	logs := e.logs.String()
	for _, secret := range []string{a.RefreshToken, b.RefreshToken, "garbage-token-value", a.AccessToken} {
		require.NotContains(t, logs, secret)
	}
	require.Contains(t, logs, `"audit_event":"logout"`)
}
