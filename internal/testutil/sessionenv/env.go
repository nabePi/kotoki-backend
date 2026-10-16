//go:build integration || e2e

// Package sessionenv is the session test harness for the jobs and the M6 gate: real Postgres and
// Redis, the real session and balance services and the real sweeper, all on one fake clock. Only
// the learner adaptive provider and the user gate are fakes.
//
// The clock is fake on purpose. A session that goes from Activate to BeginEnding in under a
// millisecond on the system clock is charged 0 seconds (ComputeCharge rounds down), so no CONSUME
// ledger row is written and exactly-once assertions on it would be meaningless. Advance Clock
// between the transitions instead.
package sessionenv

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

const termsVersion = "2026-10-01"

// Start is the fake clock's initial time.
var Start = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

// Env wires the services of one test. Every SeedActive/SeedReserved call uses a fresh user, since
// an account may have only one open session.
type Env struct {
	Pool     *pgxpool.Pool
	Redis    *redis.Client
	Clock    *clock.Fake
	Store    *postgres.Store
	IDs      idgen.Generator
	Sessions *session.Service
	Sweeper  *session.Sweeper
	Balance  *balance.Service
	Content  testutil.ContentIDs

	mu     sync.Mutex
	fences map[uuid.UUID]int64
}

// ActiveOpts shapes a seeded session. Zero values: 300 s duration and an account of twice the
// duration.
type ActiveOpts struct {
	DurationSeconds int
	Available       int64
	// Beat writes a heartbeat at Clock.Now() for the new session.
	Beat bool
}

// New builds the harness. The books are checked with AssertReconciled when the test ends.
func New(t testing.TB) *Env {
	t.Helper()
	clk := clock.NewFake(Start)
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })

	rdb := testutil.NewRedis(t)
	bal := balance.NewService(store.BalanceStore(), clk, ids)
	resolver := content.NewResolver(postgres.NewContentRepo(pool), content.ResolverConfig{TTSMode: "fake", LLMConfigured: true}, nil)
	svc := session.NewService(session.Deps{
		Store: store.SessionStore(), Reader: postgres.NewSessionRepo(pool), Balance: bal, Resolver: resolver,
		Adaptive: fakes.NewAdaptive(""), Users: fakes.NewUserGate(session.UserGateInfo{Status: "ACTIVE", TermsVersion: termsVersion}),
		OpeningPrompts: &fakes.OpeningSource{}, Clock: clk, IDs: ids, TermsVersion: termsVersion, ServiceName: "api",
	})
	sw := session.NewSweeper(svc, postgres.NewSessionRepo(pool), redisstore.NewHeartbeatReader(rdb), clk, slog.New(slog.DiscardHandler))
	return &Env{
		Pool: pool, Redis: rdb, Clock: clk, Store: store, IDs: ids, Sessions: svc, Sweeper: sw, Balance: bal,
		Content: testutil.SeedContent(t, pool), fences: map[uuid.UUID]int64{},
	}
}

// SeedReserved creates a funded user and a RESERVED session (the real Create).
func (e *Env) SeedReserved(t testing.TB, o ActiveOpts) uuid.UUID {
	t.Helper()
	dur := o.DurationSeconds
	if dur == 0 {
		dur = 300
	}
	avail := o.Available
	if avail == 0 {
		avail = int64(dur) * 2
	}
	ctx := context.Background()
	uid := testutil.SeedUser(t, e.Pool, nil)
	require.NoError(t, e.Store.BalanceStore().WithinTx(ctx, func(ctx context.Context, tx balance.Tx) error {
		return balance.OpenAccount(ctx, tx, e.IDs, uid, avail, e.Clock.Now())
	}))
	res, err := e.Sessions.Create(ctx, session.CreateSessionInput{
		UserID: uid, IdempotencyKey: "env-" + uuid.NewString(), TopicID: e.Content.TopicID, ScenarioID: e.Content.ScenarioID,
		CharacterID: e.Content.CharacterID, DurationSeconds: dur, ConversationStartMode: session.StartUserFirst,
	})
	require.NoError(t, err)
	return res.Session.ID
}

// SeedActive walks a new session through the real transitions Create, ClaimRuntime, BeginStart and
// Activate and returns its id. started_at is Clock.Now().
func (e *Env) SeedActive(t testing.TB, o ActiveOpts) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := e.SeedReserved(t, o)
	var fence int64
	require.NoError(t, e.Store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		s, err := tx.Sessions().Claim(ctx, id, []session.Status{session.StatusReserved})
		fence = s.RuntimeFence
		return err
	}))
	_, err := e.Sessions.BeginStart(ctx, id, fence)
	require.NoError(t, err)
	_, err = e.Sessions.Activate(ctx, id, fence)
	require.NoError(t, err)
	e.mu.Lock()
	e.fences[id] = fence
	e.mu.Unlock()
	if o.Beat {
		e.Beat(t, id)
	}
	return id
}

// Fence returns the runtime fence SeedActive claimed for id.
func (e *Env) Fence(t testing.TB, id uuid.UUID) int64 {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.fences[id]
	require.True(t, ok, "no fence recorded for %s", id)
	return f
}

// Beat records a heartbeat for id at Clock.Now(), as the owning runtime does every 5 s.
func (e *Env) Beat(t testing.TB, id uuid.UUID) {
	t.Helper()
	require.NoError(t, e.Redis.Set(context.Background(), redisstore.HeartbeatKey(id), e.Clock.Now().UnixMilli(), 0).Err())
}

// Count runs a single-count query with id as $1.
func (e *Env) Count(t testing.TB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.Pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}
