package httpapi_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/mailer"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var apiT0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// recordingLimiter allows everything unless a policy is in deny, and records the call order.
type recordingLimiter struct {
	mu       sync.Mutex
	policies []string
	deny     map[string]bool
}

func (l *recordingLimiter) Allow(_ context.Context, p ratelimit.Policy, _ string) (ratelimit.Decision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.policies = append(l.policies, p.Name)
	if l.deny[p.Name] {
		return ratelimit.Decision{Allowed: false, RetryAfter: 3 * time.Second}, nil
	}
	return ratelimit.Decision{Allowed: true, Remaining: 9}, nil
}

func (l *recordingLimiter) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.policies = nil
}

func (l *recordingLimiter) called() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.policies...)
}

type fakeAccountDeleter struct {
	err   error
	calls []uuid.UUID
}

func (d *fakeAccountDeleter) DeleteAccount(_ context.Context, id uuid.UUID, _ time.Time) error {
	d.calls = append(d.calls, id)
	return d.err
}

type apiEnv struct {
	handler http.Handler
	limiter *recordingLimiter
	clk     *clock.Fake
	store   *fakes.AuthStore
	bal     *fakes.BalanceStore
	mail    *mailer.FakeMailer
	deleter *fakeAccountDeleter
	tokens  *auth.JWTIssuer
	logs    *bytes.Buffer
	svc     *auth.Service
	users   *auth.UserService
	deps    httpapi.Deps

	content      *fakes.ContentRepo
	contentStore *fakes.ContentStore
}

const apiTerms = "2026-10-01"

func newAPI(t *testing.T, mutate ...func(*httpapi.Deps)) *apiEnv {
	t.Helper()
	ids := idgen.NewSequence(t)
	bal := fakes.NewBalanceStore(fakes.WithBalanceIDs(ids))
	store := fakes.NewAuthStore(bal, ids)
	clk := clock.NewFake(apiT0)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hasher := auth.NewArgon2Hasher(auth.Argon2Config{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 8, MaxWait: 2 * time.Second})
	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: []byte(strings.Repeat("k", 32)), Issuer: "kotoki-api", Audience: "kotoki", TTL: 15 * time.Minute}, ids)
	require.NoError(t, err)
	fm := mailer.NewFake()
	dispatch := auth.NewMailDispatcher(fm, log, auth.DispatcherOptions{Sleep: func(context.Context, time.Duration) error { return nil }})
	t.Cleanup(func() { _ = dispatch.Close(context.Background()) })
	svc, err := auth.NewService(store, hasher, tokens, dispatch, clk, ids, log, nil, auth.Config{
		TermsVersion: apiTerms, AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour, RefreshReuseGrace: 10 * time.Second,
		ResetTTL: 15 * time.Minute, ResetURLBase: "https://app.example/reset-password", Rand: zeroish{},
	})
	require.NoError(t, err)
	deleter := &fakeAccountDeleter{}
	users := auth.NewUserService(store, hasher, deleter, clk, log, apiTerms)
	limiter := &recordingLimiter{deny: map[string]bool{}}
	balSvc := balance.NewService(bal, clk, ids)
	adminSvc := admin.NewService(fakes.NewAdminStore(store, bal), balSvc, clk, ids, log)

	contentRepo := fakes.NewContentRepo()
	contentStore := fakes.NewContentStore(contentRepo)
	catalog := content.NewCatalogService(contentRepo, content.ResolverConfig{TTSMode: "fake", LLMConfigured: true})
	contentAdmin := content.NewAdminService(content.AdminDeps{Store: contentStore, Clock: clk, IDs: ids,
		Validator: content.ValidatorConfig{TTSEnabled: func(string) bool { return true }}})

	deps := httpapi.Deps{
		Content: catalog, ContentAdmin: contentAdmin,
		Log: log, Limiter: limiter, Tokens: tokens, Clock: clk, Auth: svc, Users: users, Balance: balSvc, Admin: adminSvc, AdminGate: allowAdmin{},
		Cfg: httpapi.Config{
			CORSOrigins: []string{"http://localhost:3000"}, BodyLimit: 64 << 10,
			Cookie: httpapi.CookieConfig{Secure: true, MaxAge: 720 * time.Hour},
		},
	}
	for _, m := range mutate {
		m(&deps)
	}
	return &apiEnv{handler: httpapi.NewRouter(deps), limiter: limiter, clk: clk, store: store, bal: bal, mail: fm, deleter: deleter,
		tokens: tokens, logs: logs, svc: svc, users: users, deps: deps, content: contentRepo, contentStore: contentStore}
}

type allowAdmin struct{}

func (allowAdmin) IsActiveAdmin(context.Context, uuid.UUID) (bool, error) { return true, nil }

// zeroish is a deterministic, non-secret entropy source for golden tests: a counter byte stream.
type zeroish struct{}

var (
	zeroishMu sync.Mutex
	zeroishN  byte
)

func (zeroish) Read(p []byte) (int, error) {
	zeroishMu.Lock()
	defer zeroishMu.Unlock()
	zeroishN++
	for i := range p {
		p[i] = zeroishN + byte(i)
	}
	return len(p), nil
}

var _ io.Reader = zeroish{}

func (e *apiEnv) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "203.0.113.7:4444"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *apiEnv) register(t *testing.T, email string) *httptest.ResponseRecorder {
	t.Helper()
	rec := e.do(http.MethodPost, "/api/v1/auth/register", `{"email":"`+email+`","password":"correct horse battery","full_name":"Hana Tanaka","terms_version":"`+apiTerms+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return rec
}

func refreshCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "kotoki_refresh" {
			return c
		}
	}
	return nil
}

// userID returns the only user registered so far (MutateUser needs the id).
func (e *apiEnv) userID(t *testing.T) uuid.UUID {
	t.Helper()
	toks := e.store.RefreshTokens()
	require.NotEmpty(t, toks)
	return toks[0].UserID
}

// adminAuthz registers admin@example.com, promotes it in the store and logs in so the
// access token carries the ADMIN claim.
func (e *apiEnv) adminAuthz(t *testing.T) (string, uuid.UUID) {
	t.Helper()
	reg := e.register(t, "admin@example.com")
	_ = reg
	id := e.userByEmail(t, "admin@example.com").ID
	e.store.MutateUser(id, func(u *auth.User) { u.Role = auth.RoleAdmin })
	login := e.do(http.MethodPost, "/api/v1/auth/login", `{"email":"admin@example.com","password":"correct horse battery"}`)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	return bearerOf(t, login), id
}

func (e *apiEnv) allUsers(t *testing.T) []auth.User {
	t.Helper()
	var out []auth.User
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx auth.Tx) error {
		var err error
		out, err = tx.Users().List(ctx, auth.UserFilter{}, nil, 100)
		return err
	}))
	return out
}

func (e *apiEnv) userByEmail(t *testing.T, email string) auth.User {
	t.Helper()
	for _, u := range e.allUsers(t) {
		if u.Email == email {
			return u
		}
	}
	t.Fatalf("user %s not found", email)
	return auth.User{}
}
