package auth_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var (
	t0       = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	testIP   = netip.MustParseAddr("203.0.113.7")
	jwtKey   = []byte(strings.Repeat("k", 32))
	termsNow = "2026-10-01"
)

type eventRecorder struct {
	mu     sync.Mutex
	events [][2]string
}

func (r *eventRecorder) Record(event, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, [2]string{event, result})
}

func (r *eventRecorder) all() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]string(nil), r.events...)
}

// countingHasher wraps a PasswordHasher and counts Verify calls.
type countingHasher struct {
	auth.PasswordHasher
	mu          sync.Mutex
	verifies    int
	forceRehash bool
	afterVerify func() // runs after each Verify, to interleave a concurrent writer
}

func (h *countingHasher) Verify(ctx context.Context, pw, enc string) (bool, error) {
	h.mu.Lock()
	h.verifies++
	h.mu.Unlock()
	ok, err := h.PasswordHasher.Verify(ctx, pw, enc)
	if h.afterVerify != nil {
		h.afterVerify()
	}
	return ok, err
}

func (h *countingHasher) NeedsRehash(enc string) bool {
	return h.forceRehash || h.PasswordHasher.NeedsRehash(enc)
}

func (h *countingHasher) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.verifies
}

type mailSink struct {
	mu   sync.Mutex
	msgs []auth.MailMessage
}

func (m *mailSink) Send(_ context.Context, msg auth.MailMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

type authEnv struct {
	svc      *auth.Service
	store    *fakes.AuthStore
	bal      *fakes.BalanceStore
	clk      *clock.Fake
	hasher   *countingHasher
	tokens   *auth.JWTIssuer
	events   *eventRecorder
	mail     *mailSink
	dispatch *auth.MailDispatcher
	logs     *bytes.Buffer
	ids      idgen.Generator
}

func newAuthEnv(t *testing.T, mutate ...func(*auth.Config)) *authEnv {
	t.Helper()
	ids := idgen.NewSequence(t)
	bal := fakes.NewBalanceStore(fakes.WithBalanceIDs(ids))
	store := fakes.NewAuthStore(bal, ids)
	clk := clock.NewFake(t0)
	hasher := &countingHasher{PasswordHasher: auth.NewArgon2Hasher(auth.Argon2Config{
		MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 8, MaxWait: 2 * time.Second})}
	tokens, err := auth.NewJWTIssuer(auth.JWTConfig{Secret: jwtKey, Issuer: "kotoki-api", Audience: "kotoki", TTL: 15 * time.Minute}, ids)
	require.NoError(t, err)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sink := &mailSink{}
	dispatch := auth.NewMailDispatcher(sink, log, auth.DispatcherOptions{Sleep: func(context.Context, time.Duration) error { return nil }})
	t.Cleanup(func() { _ = dispatch.Close(context.Background()) })
	events := &eventRecorder{}
	cfg := auth.Config{
		TermsVersion: termsNow, AccessTTL: 15 * time.Minute, RefreshTTL: 720 * time.Hour,
		RefreshReuseGrace: 10 * time.Second, ResetTTL: 15 * time.Minute, ResetURLBase: "https://app.example/reset-password",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	svc, err := auth.NewService(store, hasher, tokens, dispatch, clk, ids, log, events, cfg)
	require.NoError(t, err)
	return &authEnv{svc: svc, store: store, bal: bal, clk: clk, hasher: hasher, tokens: tokens, events: events,
		mail: sink, dispatch: dispatch, logs: logs, ids: ids}
}

func (e *authEnv) register(t *testing.T, email string) auth.AuthResult {
	t.Helper()
	res, err := e.svc.Register(context.Background(), auth.RegisterInput{
		Email: email, Password: "correct horse battery", FullName: "Hana Tanaka", TermsVersion: termsNow,
		UserAgent: "test-agent", IP: testIP,
	})
	require.NoError(t, err)
	return res
}

func newUUID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func (m *mailSink) all() []auth.MailMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]auth.MailMessage(nil), m.msgs...)
}

// drainMail closes the dispatcher (flushing the queue) and returns everything sent.
func (e *authEnv) drainMail(t *testing.T) []auth.MailMessage {
	t.Helper()
	require.NoError(t, e.dispatch.Close(context.Background()))
	return e.mail.all()
}

// waitMail waits until at least n messages were delivered.
func (e *authEnv) waitMail(t *testing.T, n int) []auth.MailMessage {
	t.Helper()
	require.Eventually(t, func() bool { return len(e.mail.all()) >= n }, 2*time.Second, 5*time.Millisecond)
	return e.mail.all()
}
