package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

var ok200 = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	httpapi.WriteJSON(w, http.StatusOK, map[string]string{"request_id": httpapi.RequestIDFrom(r.Context())})
})

func jsonLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequestID(t *testing.T) {
	h := httpapi.RequestID(ok200)
	id := "0192f1a0-1111-7000-8000-0000000000aa"
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", id)
	rec := serve(h, req)
	require.Equal(t, id, rec.Header().Get("X-Request-ID"))
	require.Contains(t, rec.Body.String(), id)

	for _, bad := range []string{"", "not-a-uuid", strings.Repeat("a", 200)} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if bad != "" {
			req.Header.Set("X-Request-ID", bad)
		}
		rec := serve(h, req)
		got := rec.Header().Get("X-Request-ID")
		require.NotEqual(t, bad, got)
		parsed, err := uuid.Parse(got)
		require.NoError(t, err)
		require.Equal(t, uuid.Version(7), parsed.Version())
	}
}

func TestRecoverer(t *testing.T) {
	log, buf := jsonLog()
	h := httpapi.RequestID(httpapi.Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") })))
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/x", nil))
	require.Equal(t, 500, rec.Code)
	require.Equal(t, "INTERNAL_ERROR", errorBody(t, rec)["code"])
	require.Equal(t, 1, strings.Count(buf.String(), `"level":"ERROR"`))
	require.Contains(t, buf.String(), `"error_location"`)
	require.NotContains(t, rec.Body.String(), "kaboom")
}

func TestCORS(t *testing.T) {
	h := httpapi.CORS([]string{"http://localhost:3000"})(ok200)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := serve(h, req)
	require.Equal(t, "http://localhost:3000", rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, "Origin", rec.Header().Get("Vary"))
	require.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))

	pre := httptest.NewRequest(http.MethodOptions, "/x", nil)
	pre.Header.Set("Origin", "http://localhost:3000")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	rec = serve(h, pre)
	require.Equal(t, 204, rec.Code)
	require.Equal(t, "GET, POST, PUT, PATCH, DELETE, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
	require.Equal(t, "Authorization, Content-Type, Idempotency-Key, X-Request-ID", rec.Header().Get("Access-Control-Allow-Headers"))
	require.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))

	foreign := httptest.NewRequest(http.MethodGet, "/x", nil)
	foreign.Header.Set("Origin", "http://evil.example")
	rec = serve(h, foreign)
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))

	rec = serve(h, httptest.NewRequest(http.MethodGet, "/x", nil))
	require.Equal(t, 200, rec.Code, "no Origin header: server-to-server calls pass")
}

func TestBodyLimit(t *testing.T) {
	h := httpapi.BodyLimit(65536)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := readAll(r); err != nil {
			httpapi.WriteError(w, r, errTooLarge(err))
			return
		}
		w.WriteHeader(204)
	}))
	rec := serve(h, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", 65537))))
	require.Equal(t, 413, rec.Code)
	require.Equal(t, "PAYLOAD_TOO_LARGE", errorBody(t, rec)["code"])
	rec = serve(h, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(strings.Repeat("a", 65536))))
	require.Equal(t, 204, rec.Code)
}

func TestSecurityHeaders(t *testing.T) {
	h := httpapi.SecurityHeaders(ok200)
	for path, noStore := range map[string]bool{
		"/api/v1/auth/login": true, "/api/v1/me": true, "/api/v1/me/consents": true, "/api/v1/balance": false, "/api/v1/sessions": false,
	} {
		rec := serve(h, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"), path)
		require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), path)
		require.Equal(t, "default-src 'none'; frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"), path)
		if noStore {
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"), path)
		} else {
			require.Empty(t, rec.Header().Get("Cache-Control"), path)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	require.Equal(t, "203.0.113.5", httpapi.ClientIP(mk("10.0.0.9:1234", "203.0.113.5, 10.0.0.2"), trusted).String())
	require.Equal(t, "198.51.100.1", httpapi.ClientIP(mk("198.51.100.1:5555", "203.0.113.5"), trusted).String(), "untrusted peer: XFF is ignored")
	require.Equal(t, "10.0.0.9", httpapi.ClientIP(mk("10.0.0.9:1234", ""), trusted).String())
	require.Equal(t, "198.51.100.1", httpapi.ClientIP(mk("198.51.100.1:5555", "1.2.3.4"), nil).String(), "no trusted proxies: RemoteAddr")
	require.Equal(t, "203.0.113.5", httpapi.ClientIP(mk("10.0.0.9:1", "spoof, 203.0.113.5"), trusted).String(), "the right-most untrusted hop wins")

	var seen netip.Addr
	h := httpapi.ClientIPMiddleware(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = httpapi.ClientIPFrom(r.Context())
	}))
	serve(h, mk("10.0.0.9:1", "203.0.113.5"))
	require.Equal(t, "203.0.113.5", seen.String())
}

type fakeLimiter struct {
	d       ratelimit.Decision
	err     error
	policy  string
	subject string
}

func (f *fakeLimiter) Allow(_ context.Context, p ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	f.policy, f.subject = p.Name, subject
	return f.d, f.err
}

func TestRateLimit(t *testing.T) {
	log, buf := jsonLog()
	p := ratelimit.Get("auth_login_ip")
	fl := &fakeLimiter{d: ratelimit.Decision{Allowed: true, Remaining: 7}}
	h := httpapi.RequestID(httpapi.RateLimit(fl, p, httpapi.IPSubject(nil), log)(ok200))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.7:1"
	rec := serve(h, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "7", rec.Header().Get("RateLimit-Remaining"))
	require.Equal(t, "auth_login_ip", fl.policy)
	require.Equal(t, "203.0.113.7", fl.subject)

	fl.d = ratelimit.Decision{Allowed: false, RetryAfter: 1500 * time.Millisecond}
	rec = serve(h, req)
	require.Equal(t, 429, rec.Code)
	require.Equal(t, "2", rec.Header().Get("Retry-After"))
	require.Equal(t, "RATE_LIMITED", errorBody(t, rec)["code"])

	fl.err = errors.New("limiter down")
	rec = serve(h, req)
	require.Equal(t, 200, rec.Code, "a failing limiter never blocks traffic")
	require.Contains(t, buf.String(), `"level":"WARN"`)
}

func newIssuer(t *testing.T) (*auth.JWTIssuer, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	j, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: []byte(strings.Repeat("k", 32)), Issuer: "kotoki-api", Audience: "kotoki", TTL: 15 * time.Minute}, idgen.NewSequence(t))
	require.NoError(t, err)
	return j, clk
}

func bearer(t *testing.T, j *auth.JWTIssuer, clk *clock.Fake, role auth.Role) (string, uuid.UUID) {
	t.Helper()
	uid := uuid.Must(uuid.NewV7())
	tok, _, err := j.IssueAccess(auth.AccessSubject{UserID: uid, Role: role, FamilyID: uuid.Must(uuid.NewV7())}, clk.Now())
	require.NoError(t, err)
	return tok, uid
}

func TestAuthn(t *testing.T) {
	j, clk := newIssuer(t)
	var principal auth.Principal
	h := httpapi.Authn(j, clk)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, _ = auth.PrincipalFrom(r.Context())
		w.WriteHeader(204)
	}))
	get := func(authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		return serve(h, req)
	}
	for authz, code := range map[string]string{"": "AUTH_REQUIRED", "Basic abc": "AUTH_REQUIRED", "Bearer": "AUTH_REQUIRED", "Bearer garbage": "TOKEN_INVALID"} {
		rec := get(authz)
		require.Equal(t, 401, rec.Code, authz)
		require.Equal(t, code, errorBody(t, rec)["code"], authz)
	}
	tok, uid := bearer(t, j, clk, auth.RoleAdmin)
	require.Equal(t, 204, get("Bearer "+tok).Code)
	require.Equal(t, uid, principal.UserID)
	require.Equal(t, auth.RoleAdmin, principal.Role)

	clk.Advance(16 * time.Minute)
	rec := get("Bearer " + tok)
	require.Equal(t, "TOKEN_EXPIRED", errorBody(t, rec)["code"])
}

type fakeGate struct {
	ok    bool
	err   error
	calls int
}

func (g *fakeGate) IsActiveAdmin(context.Context, uuid.UUID) (bool, error) {
	g.calls++
	return g.ok, g.err
}

func TestRequireAdmin(t *testing.T) {
	j, clk := newIssuer(t)
	log, buf := jsonLog()
	gate := &fakeGate{}
	h := httpapi.Authn(j, clk)(httpapi.RequireAdmin(gate, log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })))
	call := func(role auth.Role) *httptest.ResponseRecorder {
		tok, _ := bearer(t, j, clk, role)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		return serve(h, req)
	}
	rec := call(auth.RoleUser)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, "FORBIDDEN", errorBody(t, rec)["code"])
	require.Zero(t, gate.calls, "a USER claim is refused without touching the DB")

	rec = call(auth.RoleAdmin)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, 1, gate.calls)
	require.Contains(t, buf.String(), `"audit_event":"admin_access_denied"`)

	gate.err = errors.New("db down")
	require.Equal(t, 500, call(auth.RoleAdmin).Code)

	gate.err, gate.ok = nil, true
	require.Equal(t, 204, call(auth.RoleAdmin).Code)
}

func TestOriginCheck(t *testing.T) {
	h := httpapi.OriginCheck([]string{"http://localhost:3000"})(ok200)
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := serve(h, req)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, "ORIGIN_NOT_ALLOWED", errorBody(t, rec)["code"])

	req.Header.Set("Origin", "http://localhost:3000")
	require.Equal(t, 200, serve(h, req).Code)
	require.Equal(t, 200, serve(h, httptest.NewRequest(http.MethodPost, "/x", nil)).Code, "no Origin header is allowed")
}

func TestAccessLogRedactsTicket(t *testing.T) {
	log, buf := jsonLog()
	r := chi.NewRouter()
	r.Use(httpapi.RequestID, httpapi.AccessLog(log))
	r.Get("/api/v1/realtime", ok200)
	serve(r, httptest.NewRequest(http.MethodGet, "/api/v1/realtime?ticket=SECRET&x=1", nil))
	logs := buf.String()
	require.NotContains(t, logs, "SECRET")
	require.Contains(t, logs, `"route":"/api/v1/realtime"`)
	require.Contains(t, logs, `"status":200`)
	require.Contains(t, logs, `"duration_ms"`)
	require.Contains(t, logs, `"request_id"`)
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func errTooLarge(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return errs.New(errs.CodePayloadTooLarge, "too large")
	}
	return err
}
