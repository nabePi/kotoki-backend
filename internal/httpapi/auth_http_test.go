package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func goldenFile(t *testing.T, group, name string, body []byte) {
	t.Helper()
	dir := filepath.Join(testutil.ModuleRoot(t), "testdata", "api", "rest", group)
	path := filepath.Join(dir, name)
	if *update {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(path, body, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run: go test ./internal/httpapi -update")
	require.Equal(t, string(want), string(body))
}

func bearerOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out.AccessToken)
	return "Bearer " + out.AccessToken
}

func cookieHdr(c *http.Cookie) string { return "kotoki_refresh=" + c.Value }

func TestAuthFlowRegisterLoginRefreshLogout(t *testing.T) {
	e := newAPI(t)

	reg := e.register(t, "hana@example.com")
	goldenFile(t, "auth", "register_201.json", reg.Body.Bytes())
	c := refreshCookie(t, reg)
	require.NotNil(t, c)
	require.Equal(t, "/api/v1/auth", c.Path)
	require.True(t, c.HttpOnly)
	require.True(t, c.Secure)
	require.Equal(t, http.SameSiteStrictMode, c.SameSite)
	require.Equal(t, 2592000, c.MaxAge)
	require.Equal(t, "no-store", reg.Header().Get("Cache-Control"))
	require.Equal(t, "application/json; charset=utf-8", reg.Header().Get("Content-Type"))

	login := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"hana@example.com","password":"correct horse battery"}`)
	require.Equal(t, http.StatusOK, login.Code)
	var lb map[string]any
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &lb))
	require.Equal(t, "Bearer", lb["token_type"])
	require.EqualValues(t, 900, lb["expires_in"])
	require.Contains(t, lb, "user")
	a := refreshCookie(t, login)

	ref := e.do(http.MethodPost, "/api/v1/auth/refresh", "", "Cookie", cookieHdr(a))
	require.Equal(t, http.StatusOK, ref.Code, ref.Body.String())
	b := refreshCookie(t, ref)
	require.NotNil(t, b)
	require.NotEqual(t, a.Value, b.Value)
	var rb map[string]any
	require.NoError(t, json.Unmarshal(ref.Body.Bytes(), &rb))
	require.NotContains(t, rb, "user", "refresh returns no user object")
	require.Contains(t, rb, "access_token")

	again := e.do(http.MethodPost, "/api/v1/auth/refresh", "", "Cookie", cookieHdr(a))
	require.Equal(t, http.StatusConflict, again.Code)
	require.Equal(t, "REFRESH_TOKEN_ROTATED", errorBody(t, again)["code"])
	require.Empty(t, again.Result().Cookies(), "a 409 must not touch the cookie")

	out := e.do(http.MethodPost, "/api/v1/auth/logout", "", "Cookie", cookieHdr(b))
	require.Equal(t, http.StatusNoContent, out.Code)
	cleared := refreshCookie(t, out)
	require.NotNil(t, cleared)
	require.Less(t, cleared.MaxAge, 0)
	require.Empty(t, out.Body.String())

	dead := e.do(http.MethodPost, "/api/v1/auth/refresh", "", "Cookie", cookieHdr(b))
	require.Equal(t, http.StatusUnauthorized, dead.Code)
	require.Equal(t, "REFRESH_TOKEN_INVALID", errorBody(t, dead)["code"])
	require.Less(t, refreshCookie(t, dead).MaxAge, 0, "a 401 clears the cookie")

	noCookie := e.do(http.MethodPost, "/api/v1/auth/logout", "")
	require.Equal(t, http.StatusNoContent, noCookie.Code, "logout is idempotent")
}

func TestRefreshOriginCheck(t *testing.T) {
	e := newAPI(t)
	reg := e.register(t, "hana@example.com")
	c := refreshCookie(t, reg)

	bad := e.do(http.MethodPost, "/api/v1/auth/refresh", "", "Cookie", cookieHdr(c), "Origin", "http://evil.example")
	require.Equal(t, http.StatusForbidden, bad.Code)
	require.Equal(t, "ORIGIN_NOT_ALLOWED", errorBody(t, bad)["code"])
	ok := e.do(http.MethodPost, "/api/v1/auth/refresh", "", "Cookie", cookieHdr(c), "Origin", "http://localhost:3000")
	require.Equal(t, http.StatusOK, ok.Code)
}

func TestRegisterValidationErrors(t *testing.T) {
	e := newAPI(t)
	rec := e.do(http.MethodPost, "/api/v1/auth/register", `{"email":"a@b.co","password":"correct horse battery","full_name":"H","terms_version":"1999"}`)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, "VALIDATION_FAILED", errorBody(t, rec)["code"])

	rec = e.do(http.MethodPost, "/api/v1/auth/register", `{"email":"a@b.co","password":"short","full_name":"H","terms_version":"`+apiTerms+`"}`)
	require.Equal(t, "PASSWORD_TOO_WEAK", errorBody(t, rec)["code"])

	e.register(t, "dup@example.com")
	rec = e.do(http.MethodPost, "/api/v1/auth/register", `{"email":"dup@example.com","password":"correct horse battery","full_name":"H","terms_version":"`+apiTerms+`"}`)
	require.Equal(t, 409, rec.Code)
	require.Equal(t, "EMAIL_ALREADY_REGISTERED", errorBody(t, rec)["code"])
}

func TestLoginEmailRateLimited(t *testing.T) {
	e := newAPI(t)
	e.register(t, "hana@example.com")
	e.limiter.deny["auth_login_email"] = true
	e.limiter.reset()

	rec := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"hana@example.com","password":"correct horse battery"}`)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "RATE_LIMITED", errorBody(t, rec)["code"])
	require.Equal(t, "3", rec.Header().Get("Retry-After"))
	require.Equal(t, []string{"auth_login_ip", "auth_login_email"}, e.limiter.called())
	require.Len(t, e.store.RefreshTokens(), 1, "no session was issued: the password was never checked")
}

func TestForgotAlways202(t *testing.T) {
	e := newAPI(t)
	e.register(t, "hana@example.com")

	bodies := map[string]string{}
	post := func(name, email string) {
		rec := e.do(http.MethodPost, "/api/v1/auth/forgot-password", `{"email":"`+email+`"}`)
		require.Equal(t, http.StatusAccepted, rec.Code, name)
		bodies[name] = rec.Body.String()
	}
	post("registered", "hana@example.com")
	post("unknown", "nobody@example.com")
	e.limiter.deny["auth_forgot_email"] = true
	post("limited", "hana@example.com")

	require.Equal(t, `{"status":"ACCEPTED"}`+"\n", bodies["registered"])
	require.Equal(t, bodies["registered"], bodies["unknown"])
	require.Equal(t, bodies["registered"], bodies["limited"])
	require.Eventually(t, func() bool { return len(e.mail.Messages()) == 1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Len(t, e.mail.Messages(), 1, "only the registered, non-limited request sends mail")

	rec := e.do(http.MethodPost, "/api/v1/auth/forgot-password", `{"email":"not-an-email"}`)
	require.Equal(t, 400, rec.Code)
}

func TestResetPassword(t *testing.T) {
	e := newAPI(t)
	e.register(t, "hana@example.com")
	require.Equal(t, 202, e.do(http.MethodPost, "/api/v1/auth/forgot-password", `{"email":"hana@example.com"}`).Code)
	require.Eventually(t, func() bool { return len(e.mail.Messages()) == 1 }, 2*time.Second, 5*time.Millisecond)
	token := tokenFromBody(t, e.mail.Last().TextBody)

	rec := e.do(http.MethodPost, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"another long passphrase"}`)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Body.String())

	rec = e.do(http.MethodPost, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"another long passphrase"}`)
	require.Equal(t, 400, rec.Code)
	require.Equal(t, "RESET_TOKEN_INVALID", errorBody(t, rec)["code"])

	rec = e.do(http.MethodPost, "/api/v1/auth/reset-password", `{"token":"short","new_password":"another long passphrase"}`)
	require.Equal(t, "RESET_TOKEN_INVALID", errorBody(t, rec)["code"])
}

func tokenFromBody(t *testing.T, body string) string {
	t.Helper()
	const marker = "?token="
	i := indexOf(body, marker)
	require.GreaterOrEqual(t, i, 0, body)
	rest := body[i+len(marker):]
	for j, r := range rest {
		if r == '\n' || r == '\r' || r == ' ' {
			return rest[:j]
		}
	}
	return rest
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestTerms(t *testing.T) {
	e := newAPI(t)
	rec := e.do(http.MethodGet, "/api/v1/auth/terms", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"terms_version":"2026-10-01"}`, rec.Body.String())
	require.Equal(t, []string{"api_ip_default"}, e.limiter.called())
	goldenFile(t, "auth", "terms_200.json", rec.Body.Bytes())
}

func TestCookieWithoutSecureInLocal(t *testing.T) {
	e := newAPI(t, func(d *httpapi.Deps) { d.Cfg.Cookie.Secure = false })
	reg := e.register(t, "hana@example.com")
	require.False(t, refreshCookie(t, reg).Secure)
}

func TestSecurityAndRequestIDHeaders(t *testing.T) {
	e := newAPI(t)
	rec := e.do(http.MethodGet, "/api/v1/auth/terms", "", "X-Request-ID", "0192f1a0-1111-7000-8000-0000000000aa")
	require.Equal(t, "0192f1a0-1111-7000-8000-0000000000aa", rec.Header().Get("X-Request-ID"))
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}
