//go:build integration

package integration_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

const createTerms = "2026-10-01"

type sessEnv struct {
	pool    *pgxpool.Pool
	store   *postgres.Store
	svc     *session.Service
	ids     idgen.Generator
	content testutil.ContentIDs
	userID  uuid.UUID
}

// newSessEnv wires the real Postgres store, balance service and content resolver; only the learner
// adaptive provider and the user gate are fakes. A funded account is opened for the user.
func newSessEnv(t *testing.T, available int64, wrap func(session.Store) session.Store) *sessEnv {
	t.Helper()
	return newSessEnvClock(t, available, clock.System{}, wrap)
}

func newSessEnvClock(t *testing.T, available int64, clk clock.Clock, wrap func(session.Store) session.Store) *sessEnv {
	t.Helper()
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
	e := &sessEnv{pool: pool, store: store, ids: ids, content: testutil.SeedContent(t, pool), userID: testutil.SeedUser(t, pool, nil)}
	require.NoError(t, store.BalanceStore().WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return balance.OpenAccount(ctx, tx, ids, e.userID, available, clk.Now())
	}))

	st := store.SessionStore()
	if wrap != nil {
		st = wrap(st)
	}
	resolver := content.NewResolver(postgres.NewContentRepo(pool), content.ResolverConfig{TTSMode: "fake", LLMConfigured: true}, nil)
	e.svc = session.NewService(session.Deps{
		Store: st, Reader: postgres.NewSessionRepo(pool), Balance: balance.NewService(store.BalanceStore(), clk, ids), Resolver: resolver,
		Adaptive: fakes.NewAdaptive(""), Users: fakes.NewUserGate(session.UserGateInfo{Status: "ACTIVE", TermsVersion: createTerms}),
		OpeningPrompts: &fakes.OpeningSource{}, Clock: clk, IDs: ids, TermsVersion: createTerms, ServiceName: "api",
	})
	return e
}

func (e *sessEnv) input(key string, duration int) session.CreateSessionInput {
	return session.CreateSessionInput{UserID: e.userID, IdempotencyKey: key, TopicID: e.content.TopicID, ScenarioID: e.content.ScenarioID,
		CharacterID: e.content.CharacterID, DurationSeconds: duration, ConversationStartMode: session.StartUserFirst}
}

func TestCreate20ParallelDifferentKeys(t *testing.T) {
	e := newSessEnv(t, 600, nil)
	const n = 20
	var ok, open atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Create(context.Background(), e.input("parallel-key-"+uuid.NewString()[:8]+string(rune('a'+i)), 300))
			switch {
			case err == nil:
				ok.Add(1)
			case errs.CodeOf(err) == errs.CodeSessionAlreadyOpen:
				open.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), ok.Load(), "exactly one request wins the account lock")
	require.Equal(t, int32(n-1), open.Load())

	var reserves int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger WHERE entry_type = 'RESERVE'`).Scan(&reserves))
	require.Equal(t, 1, reserves)

	// after the winner is cancelled the minutes are back and the next round again has one winner
	var winner uuid.UUID
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT id FROM sessions WHERE user_id = $1`, e.userID).Scan(&winner))
	_, err := e.svc.Cancel(context.Background(), e.userID, winner)
	require.NoError(t, err)
	ok.Store(0)
	open.Store(0)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Create(context.Background(), e.input("second-round-"+uuid.NewString()[:8]+string(rune('a'+i)), 300))
			switch {
			case err == nil:
				ok.Add(1)
			case errs.CodeOf(err) == errs.CodeSessionAlreadyOpen:
				open.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), ok.Load())
	require.Equal(t, int32(n-1), open.Load())
}

func TestCreate20ParallelSameKey(t *testing.T) {
	e := newSessEnv(t, 600, nil)
	const n = 20
	ids := make([]uuid.UUID, n)
	var replayed atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.svc.Create(context.Background(), e.input("same-key-for-everyone", 300))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			ids[i] = res.Session.ID
			if res.Replayed {
				replayed.Add(1)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		require.Equal(t, ids[0], id, "everyone gets the same session")
	}
	require.Equal(t, int32(n-1), replayed.Load())
	var reserves int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger WHERE entry_type = 'RESERVE'`).Scan(&reserves))
	require.Equal(t, 1, reserves)
}

// rivalStore commits a competing session right before the first INSERT of a transaction, which is
// the race the unique indexes exist for.
type rivalStore struct {
	session.Store
	once  sync.Once
	rival func()
}

func (s *rivalStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx session.Tx) error) error {
	return s.Store.WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		return fn(ctx, rivalTx{Tx: tx, s: s})
	})
}

type rivalTx struct {
	session.Tx
	s *rivalStore
}

func (t rivalTx) Sessions() session.Repository { return rivalRepo{Repository: t.Tx.Sessions(), s: t.s} }

type rivalRepo struct {
	session.Repository
	s *rivalStore
}

func (r rivalRepo) Insert(ctx context.Context, v session.Session) error {
	r.s.once.Do(r.s.rival)
	return r.Repository.Insert(ctx, v)
}

func TestCreateUniqueViolationBackstop(t *testing.T) {
	var rivalID uuid.UUID
	var e *sessEnv
	e = newSessEnv(t, 1200, func(inner session.Store) session.Store {
		rs := &rivalStore{Store: inner}
		// a request that never took the account lock commits a session of the same user between the
		// checks and the INSERT: ux_sessions_one_open_per_user fires (23505)
		rs.rival = func() { rivalID = testutil.SeedSession(t, e.pool, e.userID, e.content, nil) }
		return rs
	})
	_, err := e.svc.Create(context.Background(), e.input("our-key-0001", 300))
	require.Equal(t, errs.CodeSessionAlreadyOpen, errs.CodeOf(err), "the transaction rolled back and the answer was decided again outside it")
	require.Equal(t, map[string]any{"session_id": rivalID}, errs.DetailsOf(err))

	var reserves int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger WHERE entry_type = 'RESERVE'`).Scan(&reserves))
	require.Zero(t, reserves, "nothing was reserved")
	// the rival row has no reservation; reconciliation (R2/R4) would complain about it, so this test
	// has none of its own to settle
}
