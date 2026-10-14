//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type pgBalance struct {
	pool  *pgxpool.Pool
	store *postgres.Store
	svc   *balance.Service
	ids   idgen.Generator
}

func newPGBalance(t *testing.T) pgBalance {
	t.Helper()
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	clk := clock.NewFake(t0)
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
	return pgBalance{pool: pool, store: store, svc: balance.NewService(store.BalanceStore(), clk, ids), ids: ids}
}

func (p pgBalance) openUser(t *testing.T, initial int64) uuid.UUID {
	t.Helper()
	uid := testutil.SeedUser(t, p.pool, nil)
	require.NoError(t, p.store.BalanceStore().WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		return balance.OpenAccount(ctx, tx, p.ids, uid, initial, t0)
	}))
	return uid
}

func TestPGBalanceRepoSuite(t *testing.T) {
	repotest.BalanceRepoSuite(t, func(t *testing.T) repotest.BalanceHarness {
		pool := testutil.NewPostgres(t)
		store := postgres.NewStore(pool)
		t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
		return repotest.BalanceHarness{
			Store:    store.BalanceStore(),
			SeedUser: func(t *testing.T) uuid.UUID { return testutil.SeedUser(t, pool, nil) },
		}
	})
}

func TestPGLedgerAppendOnly(t *testing.T) {
	p := newPGBalance(t)
	p.openUser(t, 600)
	ctx := context.Background()
	_, err := p.pool.Exec(ctx, `UPDATE balance_ledger SET reason='x'`)
	testutil.RequirePgErr(t, err, "23001", "")
	_, err = p.pool.Exec(ctx, `DELETE FROM balance_ledger`)
	testutil.RequirePgErr(t, err, "23001", "")
}

func grant(user uuid.UUID, seconds int64, key string) balance.GrantInput {
	return balance.GrantInput{UserID: user, AdminUserID: uuid.Nil, Seconds: seconds, Reason: "support credit", IdempotencyKey: key, At: t0}
}

func TestPGGrantConcurrentSameKey(t *testing.T) {
	p := newPGBalance(t)
	uid := p.openUser(t, 0)
	admin := testutil.SeedUser(t, p.pool, testutil.Row{"role": "ADMIN"})
	key := uuid.NewString()

	var wg sync.WaitGroup
	var replayed, fresh atomic.Int32
	errsCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := grant(uid, 900, key)
			in.AdminUserID = admin
			res, err := p.svc.Grant(context.Background(), in)
			if err != nil {
				errsCh <- err
				return
			}
			if res.Replayed {
				replayed.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		t.Fatalf("unexpected error: %v", err)
	}
	require.Equal(t, int32(1), fresh.Load())
	require.Equal(t, int32(19), replayed.Load())

	var n int
	var available int64
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT count(*) FROM balance_ledger WHERE idempotency_key=$1`, "grant:"+key).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT available_seconds FROM balance_accounts WHERE user_id=$1`, uid).Scan(&available))
	require.Equal(t, int64(900), available)
}

func TestPGGrantSameKeyDifferentUsers(t *testing.T) {
	p := newPGBalance(t)
	admin := testutil.SeedUser(t, p.pool, testutil.Row{"role": "ADMIN"})
	users := make([]uuid.UUID, 20)
	for i := range users {
		users[i] = p.openUser(t, 0)
	}
	key := uuid.NewString()

	var wg sync.WaitGroup
	var ok, reused atomic.Int32
	for _, uid := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := grant(uid, 300, key)
			in.AdminUserID = admin
			_, err := p.svc.Grant(context.Background(), in)
			switch {
			case err == nil:
				ok.Add(1)
			case errs.CodeOf(err) == errs.CodeIdempotencyKeyReused:
				reused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), ok.Load())
	require.Equal(t, int32(19), reused.Load())
}

func TestPGAdjustNegativeRejected(t *testing.T) {
	p := newPGBalance(t)
	uid := p.openUser(t, 600)
	admin := testutil.SeedUser(t, p.pool, testutil.Row{"role": "ADMIN"})
	_, err := p.svc.Adjust(context.Background(), balance.AdjustInput{
		UserID: uid, AdminUserID: admin, SecondsDelta: -601, Reason: "manual fix", IdempotencyKey: uuid.NewString(), At: t0})
	require.Equal(t, errs.CodeBalanceAdjustmentNegative, errs.CodeOf(err))
	var available int64
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT available_seconds FROM balance_accounts WHERE user_id=$1`, uid).Scan(&available))
	require.Equal(t, int64(600), available)
}

func TestPGGrantSecondsMatchLedger(t *testing.T) {
	p := newPGBalance(t)
	uid := p.openUser(t, 3600)
	admin := testutil.SeedUser(t, p.pool, testutil.Row{"role": "ADMIN"})
	in := grant(uid, 1800, uuid.NewString())
	in.AdminUserID = admin
	res, err := p.svc.Grant(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, int64(5400), res.Account.AvailableSeconds)
	require.Equal(t, int64(2), res.Entry.AccountVersion)

	view, err := p.svc.GetBalance(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, int64(5400), view.AvailableSeconds)
	require.Equal(t, []int64{300, 600, 900, 1800, 2700, 3600}, view.AllowedDurations)

	entries, _, err := p.svc.ListTransactions(context.Background(), uid, []balance.EntryType{balance.EntryGrant}, nil, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
}
