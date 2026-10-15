//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/mailer"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const apiTerms = "2026-10-01"

func tokenFromBody(t *testing.T, body string) string {
	t.Helper()
	const marker = "?token="
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, body)
	rest := body[i+len(marker):]
	if j := strings.IndexAny(rest, "\r\n "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// gate wires the real PostgreSQL and Redis adapters behind the real router, as cmd/api does.
type gate struct {
	t       *testing.T
	srv     *httptest.Server
	pool    *pgxpool.Pool
	clk     *clock.Fake
	mail    *mailer.FakeMailer
	tickets *redisstore.TicketStore
	rdbTTL  func(ctx context.Context, ticket string) time.Duration
	logs    *bytes.Buffer
}

func newGate(t *testing.T) *gate {
	t.Helper()
	pool := testutil.NewPostgres(t)
	rdb := testutil.NewRedis(t)
	ids, clk := idgen.NewV7(), clock.NewFake(time.Now().UTC().Truncate(time.Millisecond))
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	reg := prometheus.NewRegistry()

	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: []byte(strings.Repeat("g", 32)), Issuer: "kotoki-api", Audience: "kotoki", TTL: 15 * time.Minute}, ids)
	require.NoError(t, err)
	hasher := auth.NewArgon2Hasher(auth.Argon2Config{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 8, MaxWait: 2 * time.Second})
	fm := mailer.NewFake()
	dispatcher := auth.NewMailDispatcher(fm, log, auth.DispatcherOptions{})
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	balSvc := balance.NewService(store.BalanceStore(), clk, ids)
	adminSvc := admin.NewService(store.AdminStore(), balSvc, clk, ids, log)
	authSvc, err := auth.NewService(store.AuthStore(), hasher, tokens, dispatcher, clk, ids, log, metrics.NewAuth(reg), auth.Config{
		TermsVersion: apiTerms, AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour, RefreshReuseGrace: 10 * time.Second,
		ResetTTL: 15 * time.Minute, ResetURLBase: "https://app.example/reset-password"})
	require.NoError(t, err)
	userSvc := auth.NewUserService(store.AuthStore(), hasher, adminSvc, clk, log, apiTerms)

	router := httpapi.NewRouter(httpapi.Deps{
		Log: log, Clock: clk, Tokens: tokens, Metrics: metrics.NewHTTP(reg, "kotoki-api"),
		Limiter: redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(clk, 1000), clk, log, metrics.NewRateLimit(reg)),
		Cfg: httpapi.Config{CORSOrigins: []string{"http://localhost:3000"}, BodyLimit: 64 << 10,
			Cookie: httpapi.CookieConfig{Secure: false, MaxAge: 720 * time.Hour}},
		Auth: authSvc, Users: userSvc, Balance: balSvc, Admin: adminSvc, AdminGate: adminSvc,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
	g := &gate{t: t, srv: srv, pool: pool, clk: clk, mail: fm, logs: logs, tickets: redisstore.NewTicketStore(rdb, clk)}
	g.rdbTTL = func(ctx context.Context, ticket string) time.Duration {
		sum := sha256.Sum256([]byte(ticket))
		return rdb.PTTL(ctx, "kotoki:ws_ticket:"+hex.EncodeToString(sum[:])).Val()
	}
	return g
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (g *gate) call(method, path, body string, headers ...string) resp {
	g.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, g.srv.URL+path, rd)
	require.NoError(g.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(g.t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(g.t, err)
	return resp{code: res.StatusCode, header: res.Header, body: b}
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.body, &m), string(r.body))
	return m
}

func (r resp) errCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (r resp) cookie() string {
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == "kotoki_refresh" {
			return c.Name + "=" + c.Value
		}
	}
	return ""
}

func (r resp) bearer(t *testing.T) string {
	t.Helper()
	tok, _ := r.json(t)["access_token"].(string)
	require.NotEmpty(t, tok)
	return "Bearer " + tok
}

func (g *gate) register(email string) resp {
	g.t.Helper()
	r := g.call(http.MethodPost, "/api/v1/auth/register",
		`{"email":"`+email+`","password":"correct horse battery","full_name":"Hana Tanaka","terms_version":"`+apiTerms+`"}`)
	require.Equal(g.t, http.StatusCreated, r.code, string(r.body))
	return r
}

func (g *gate) login(email string) resp {
	g.t.Helper()
	r := g.call(http.MethodPost, "/api/v1/auth/login", `{"email":"`+email+`","password":"correct horse battery"}`)
	require.Equal(g.t, http.StatusOK, r.code, string(r.body))
	return r
}

func (g *gate) refresh(cookie string) resp {
	return g.call(http.MethodPost, "/api/v1/auth/refresh", "", "Cookie", cookie)
}

func TestM2GateAuthFlow(t *testing.T) {
	g := newGate(t)
	reg := g.register("hana@example.com")
	login := g.login("hana@example.com")
	a := login.cookie()
	require.NotEmpty(t, a)
	require.NotEqual(t, reg.cookie(), a)

	ref := g.refresh(a)
	require.Equal(t, 200, ref.code, string(ref.body))
	b := ref.cookie()
	require.NotEmpty(t, b)

	g.clk.Advance(9 * time.Second)
	again := g.refresh(a)
	require.Equal(t, 409, again.code)
	require.Equal(t, "REFRESH_TOKEN_ROTATED", again.errCode(t))
	require.Equal(t, 200, g.refresh(b).code, "the newest token stays valid after the 409")
	// b was rotated by that call; use the current token for logout
	cur := g.refresh(b)
	_ = cur

	// log in again for a clean token to log out with
	fresh := g.login("hana@example.com").cookie()
	require.Equal(t, 204, g.call(http.MethodPost, "/api/v1/auth/logout", "", "Cookie", fresh).code)
	dead := g.refresh(fresh)
	require.Equal(t, 401, dead.code)
	require.Equal(t, "REFRESH_TOKEN_INVALID", dead.errCode(t))
}

func TestM2GateRefreshReuse(t *testing.T) {
	g := newGate(t)
	g.register("hana@example.com")
	a := g.login("hana@example.com").cookie()
	b := g.refresh(a).cookie()
	require.NotEmpty(t, b)

	g.clk.Advance(11 * time.Second)
	reuse := g.refresh(a)
	require.Equal(t, 401, reuse.code)
	require.Equal(t, "REFRESH_TOKEN_INVALID", reuse.errCode(t))
	newest := g.refresh(b)
	require.Equal(t, 401, newest.code, "the whole family is revoked, the newest token too")
}

func TestM2GateResetFlow(t *testing.T) {
	g := newGate(t)
	g.register("hana@example.com")
	oldCookie := g.login("hana@example.com").cookie()

	require.Equal(t, 202, g.call(http.MethodPost, "/api/v1/auth/forgot-password", `{"email":"hana@example.com"}`).code)
	require.Eventually(t, func() bool { return len(g.mail.Messages()) == 1 }, 3*time.Second, 10*time.Millisecond)

	unknown := g.call(http.MethodPost, "/api/v1/auth/forgot-password", `{"email":"nobody@example.com"}`)
	known := g.call(http.MethodPost, "/api/v1/auth/forgot-password", `{"email":"hana@example.com"}`)
	require.Equal(t, 202, unknown.code)
	require.Equal(t, string(known.body), string(unknown.body), "identical generic answer")
	time.Sleep(100 * time.Millisecond)
	require.Len(t, g.mail.Messages(), 2, "the unknown address got no mail")
	token := tokenFromBody(t, g.mail.Last().TextBody) // a newer request supersedes the first token
	g.mail.Reset()

	reset := g.call(http.MethodPost, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"another long passphrase"}`)
	require.Equal(t, 204, reset.code, string(reset.body))
	require.Equal(t, 401, g.refresh(oldCookie).code, "reset revokes every refresh token")

	again := g.call(http.MethodPost, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"another long passphrase"}`)
	require.Equal(t, 400, again.code)
	require.Equal(t, "RESET_TOKEN_INVALID", again.errCode(t))
}

func TestM2GateConsent(t *testing.T) {
	g := newGate(t)
	g.register("hana@example.com")
	var authz string
	acceptedAt := func() time.Time {
		var at time.Time
		require.NoError(t, g.pool.QueryRow(context.Background(), `SELECT terms_accepted_at FROM users WHERE email='hana@example.com'`).Scan(&at))
		return at
	}
	first := acceptedAt()

	g.clk.Advance(time.Hour)
	authz = g.login("hana@example.com").bearer(t) // login after the clock jump: access tokens live 15 minutes
	require.Equal(t, 200, g.call(http.MethodPut, "/api/v1/me/consents", `{"terms_version":"`+apiTerms+`"}`, "Authorization", authz).code)
	require.True(t, acceptedAt().Equal(first), "the same version leaves terms_accepted_at alone")

	_, err := g.pool.Exec(context.Background(), `UPDATE users SET terms_version='2025-01-01' WHERE email='hana@example.com'`)
	require.NoError(t, err)
	require.Equal(t, 200, g.call(http.MethodPut, "/api/v1/me/consents", `{"terms_version":"`+apiTerms+`"}`, "Authorization", authz).code)
	require.True(t, acceptedAt().After(first), "a new version moves terms_accepted_at")
}

func TestM2GateAdminGrant(t *testing.T) {
	g := newGate(t)
	g.register("admin@example.com")
	hana := g.register("hana@example.com")
	hanaID := userID(t, g.pool, "hana@example.com")
	userAuthz := hana.bearer(t)

	// the same statement `make promote-admin` runs
	_, err := g.pool.Exec(context.Background(), `UPDATE users SET role='ADMIN', updated_at=now() WHERE email='admin@example.com'`)
	require.NoError(t, err)
	adm := g.login("admin@example.com").bearer(t)

	grantPath := "/api/v1/admin/users/" + hanaID.String() + "/balance-grants"
	require.Equal(t, 403, g.call(http.MethodPost, grantPath, `{"seconds":1800,"reason":"Beta tester grant"}`, "Authorization", userAuthz, "Idempotency-Key", uuid.NewString()).code)

	key := uuid.NewString()
	first := g.call(http.MethodPost, grantPath, `{"seconds":1800,"reason":"Beta tester grant"}`, "Authorization", adm, "Idempotency-Key", key)
	require.Equal(t, 201, first.code, string(first.body))
	replay := g.call(http.MethodPost, grantPath, `{"seconds":1800,"reason":"Beta tester grant"}`, "Authorization", adm, "Idempotency-Key", key)
	require.Equal(t, 200, replay.code)
	require.Equal(t, "true", replay.header.Get("Idempotent-Replayed"))

	bal := g.call(http.MethodGet, "/api/v1/balance", "", "Authorization", userAuthz)
	require.EqualValues(t, 1800, bal.json(t)["available_seconds"])
	tx := g.call(http.MethodGet, "/api/v1/balance/transactions", "", "Authorization", userAuthz)
	require.Contains(t, string(tx.body), `"entry_type":"GRANT"`)
	require.NotContains(t, string(tx.body), "idempotency_key")

	adj := g.call(http.MethodPost, "/api/v1/admin/users/"+hanaID.String()+"/balance-adjustments", `{"seconds_delta":-300,"reason":"correction"}`, "Authorization", adm, "Idempotency-Key", uuid.NewString())
	require.Equal(t, 201, adj.code, string(adj.body))
	st := g.call(http.MethodPatch, "/api/v1/admin/users/"+hanaID.String(), `{"status":"SUSPENDED","reason":"abuse report"}`, "Authorization", adm)
	require.Equal(t, 200, st.code, string(st.body))

	var n int
	require.NoError(t, g.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit_logs WHERE action IN ('BALANCE_GRANT','BALANCE_ADJUSTMENT','USER_STATUS_CHANGE')`).Scan(&n))
	require.Equal(t, 3, n, "one audit row per grant (not per replay), adjustment and status change")
	require.NoError(t, g.pool.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger WHERE idempotency_key = $1`, "grant:"+key).Scan(&n))
	require.Equal(t, 1, n)

	logs := g.call(http.MethodGet, "/api/v1/admin/audit-logs?action=BALANCE_GRANT", "", "Authorization", adm)
	require.Equal(t, 200, logs.code)
	require.Contains(t, string(logs.body), `"action":"BALANCE_GRANT"`)
}

func TestM2GateDeleteAccount(t *testing.T) {
	g := newGate(t)
	reg := g.register("hana@example.com")
	authz := reg.bearer(t)

	wrong := g.call(http.MethodDelete, "/api/v1/me", `{"password":"wrong password!!"}`, "Authorization", authz)
	require.Equal(t, 401, wrong.code)
	ok := g.call(http.MethodDelete, "/api/v1/me", `{"password":"correct horse battery"}`, "Authorization", authz)
	require.Equal(t, 202, ok.code, string(ok.body))

	login := g.call(http.MethodPost, "/api/v1/auth/login", `{"email":"hana@example.com","password":"correct horse battery"}`)
	require.Equal(t, 401, login.code)
	var tokens int
	require.NoError(t, g.pool.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens`).Scan(&tokens))
	require.Zero(t, tokens)
	var status string
	require.NoError(t, g.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE email='hana@example.com'`).Scan(&status))
	require.Equal(t, "DELETED", status)
}

func TestM2GateTicketTTL(t *testing.T) {
	g := newGate(t)
	raw, err := g.tickets.Issue(context.Background(), auth.WSTicket{UserID: uuid.New(), SessionID: uuid.New(), IssuedAt: g.clk.Now()}, 30*time.Second)
	require.NoError(t, err)
	ttl := g.rdbTTL(context.Background(), raw)
	require.Greater(t, ttl, time.Duration(0))
	require.LessOrEqual(t, ttl, 30*time.Second)
}

func userID(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email=$1`, email).Scan(&id))
	return id
}
