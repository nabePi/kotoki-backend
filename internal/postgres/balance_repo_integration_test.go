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
	content testutil.ContentIDs
	pool    *pgxpool.Pool
	store   *postgres.Store
	svc     *balance.Service
	ids     idgen.Generator
}

func newPGBalance(t *testing.T) *pgBalance {
	t.Helper()
	pool := testutil.NewPostgres(t)
	ids := idgen.NewV7()
	clk := clock.NewFake(t0)
	store := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	t.Cleanup(func() { testutil.AssertReconciled(t, pool) })
	return &pgBalance{pool: pool, store: store, svc: balance.NewService(store.BalanceStore(), clk, ids), ids: ids}
}

func (p *pgBalance) openUser(t *testing.T, initial int64) uuid.UUID {
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
		var skip atomic.Bool
		t.Cleanup(func() {
			if !skip.Load() {
				testutil.AssertReconciled(t, pool)
			}
		})
		content := testutil.SeedContent(t, pool)
		return repotest.BalanceHarness{
			Store:    store.BalanceStore(),
			SeedUser: func(t *testing.T) uuid.UUID { return testutil.SeedUser(t, pool, nil) },
			// every session gets its own user: one open session per user is unique
			SeedSession: func(t *testing.T, _ uuid.UUID) uuid.UUID {
				return testutil.SeedSession(t, pool, testutil.SeedUser(t, pool, nil), content, nil)
			},
			Unreconciled: func() { skip.Store(true) },
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

// sessionRow seeds a session for reservation tests; the content (topic, scenario, …) is seeded once.
func (p *pgBalance) sessionRow(t *testing.T, uid uuid.UUID, o testutil.Row) uuid.UUID {
	t.Helper()
	if p.content.TopicID == uuid.Nil {
		p.content = testutil.SeedContent(t, p.pool)
	}
	return testutil.SeedSession(t, p.pool, uid, p.content, o)
}

func TestPGReserveSettleExample1ReconcilesClean(t *testing.T) { // (a)
	p := newPGBalance(t)
	uid := p.openUser(t, 1200)
	sid := p.sessionRow(t, uid, testutil.Row{"duration_seconds": 600, "reserved_seconds": 600})
	rid := p.ids.New()
	ctx := context.Background()
	require.NoError(t, p.store.BalanceStore().WithinTx(ctx, func(ctx context.Context, tx balance.Tx) error {
		acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
		require.NoError(t, err)
		_, err = p.svc.Reserve(ctx, tx, acc, balance.ReserveInput{ReservationID: rid, SessionID: sid, Seconds: 600, At: t0})
		return err
	}))
	testutil.AssertReconciled(t, p.pool) // a held reservation on a RESERVED session is consistent

	ending := t0.Add(7*time.Minute + 400*time.Millisecond)
	var res balance.SettleResult
	require.NoError(t, p.store.BalanceStore().WithinTx(ctx, func(ctx context.Context, tx balance.Tx) error {
		acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
		require.NoError(t, err)
		res, err = p.svc.SettleReservation(ctx, tx, acc, balance.SettleInput{SessionID: sid, StartedAt: t0, EndingAt: &ending,
			EndedAt: t0.Add(7*time.Minute + 2100*time.Millisecond), ExpiresAt: t0.Add(10 * time.Minute), At: t0.Add(8 * time.Minute)})
		return err
	}))
	require.EqualValues(t, 421, res.Charged)
	acc, err := p.svc.GetBalance(ctx, uid)
	require.NoError(t, err)
	require.EqualValues(t, 779, acc.AvailableSeconds)
	require.EqualValues(t, 0, acc.ReservedSeconds)

	// the session service (T16–T18) does this in the same transaction; here by hand
	_, err = p.pool.Exec(ctx, `UPDATE sessions SET status='COMPLETED', end_reason='USER_ENDED', starting_at=$2, started_at=$2,
	    expires_at=$3, ending_at=$4, ended_at=$5, charged_seconds=421 WHERE id=$1`,
		sid, t0, t0.Add(10*time.Minute), ending, t0.Add(7*time.Minute+2100*time.Millisecond))
	require.NoError(t, err)
}

func TestPGLedgerShapeRejectsInconsistentReserve(t *testing.T) { // (b)
	p := newPGBalance(t)
	uid := p.openUser(t, 600)
	err := p.store.BalanceStore().WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
		require.NoError(t, err)
		_, err = tx.Balance().ApplyMovement(ctx, acc, balance.Movement{
			EntryType: balance.EntryReserve, AvailableDelta: -300, ReservedDelta: 200, ReferenceType: balance.RefSession,
			IdempotencyKey: "reserve:bad", At: t0})
		return err
	})
	require.Error(t, err, "RESERVE must satisfy available_delta = −reserved_delta (chk_ledger_shape)")
}

func TestPGReservationStateRejectsInconsistentSettle(t *testing.T) { // (c)
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	ids := idgen.NewV7()
	uid := testutil.SeedUser(t, pool, nil)
	content := testutil.SeedContent(t, pool)
	sid := testutil.SeedSession(t, pool, uid, content, nil)
	err := store.BalanceStore().WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		require.NoError(t, balance.OpenAccount(ctx, tx, ids, uid, 600, t0))
		acc, err := tx.Balance().LockAccountByUserID(ctx, uid)
		require.NoError(t, err)
		res := balance.Reservation{ID: ids.New(), UserID: uid, AccountID: acc.ID, SessionID: sid, ReservedSeconds: 300,
			Status: balance.ReservationHeld, StartDeadlineAt: t0.Add(5 * time.Minute), CreatedAt: t0}
		require.NoError(t, tx.Balance().InsertReservation(ctx, res))
		return tx.Balance().MarkReservationSettled(ctx, res.ID, 100, 100, t0) // 100 + 100 ≠ 300
	})
	require.Error(t, err, "chk_reservation_state")

	uid2 := testutil.SeedUser(t, pool, nil)
	sid2 := testutil.SeedSession(t, pool, uid2, content, nil)
	require.NoError(t, store.BalanceStore().WithinTx(context.Background(), func(ctx context.Context, tx balance.Tx) error {
		require.NoError(t, balance.OpenAccount(ctx, tx, ids, uid2, 0, t0))
		acc, err := tx.Balance().LockAccountByUserID(ctx, uid2)
		require.NoError(t, err)
		return tx.Balance().InsertReservation(ctx, balance.Reservation{ID: ids.New(), UserID: uid2, AccountID: acc.ID, SessionID: sid2,
			ReservedSeconds: 300, Status: balance.ReservationHeld, StartDeadlineAt: t0.Add(5 * time.Minute), CreatedAt: t0})
	}))
	_, dbErr := pool.Exec(context.Background(), `UPDATE balance_reservations SET status='SETTLED', consumed_seconds=1, released_seconds=1, settled_at=now() WHERE session_id=$1`, sid2)
	testutil.RequirePgErr(t, dbErr, "23514", "chk_reservation_state")
}

func (p *pgBalance) breakInvariants(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := p.pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func runCheck(t *testing.T, p *pgBalance, check string) []string {
	t.Helper()
	rows, err := postgres.NewReconcileRepo(p.pool).RunCheck(context.Background(), check)
	require.NoError(t, err)
	return rows
}

func TestPGReconcileDetectsViolations(t *testing.T) { // (d), (e), (f)
	pool := testutil.NewPostgres(t) // no AssertReconciled: this test breaks the invariants on purpose
	p := &pgBalance{pool: pool, store: postgres.NewStore(pool), ids: idgen.NewV7()}
	p.svc = balance.NewService(p.store.BalanceStore(), clock.NewFake(t0), p.ids)
	ctx := context.Background()
	repo := postgres.NewReconcileRepo(pool)

	uid := p.openUser(t, 600)
	for _, c := range []string{"R1", "R2", "R3", "R4", "R5", "R6"} {
		require.Empty(t, runCheck(t, p, c), "a fresh account is consistent: %s", c)
	}
	p.breakInvariants(t, `UPDATE balance_accounts SET available_seconds = available_seconds + 5 WHERE user_id = $1`, uid)
	require.NotEmpty(t, runCheck(t, p, "R1"), "R1: the balance no longer equals the ledger sum")
	require.NotEmpty(t, runCheck(t, p, "R3"), "R3: the last ledger entry no longer matches the account")
	require.Contains(t, runCheck(t, p, "R1")[0], uid.String())
	p.breakInvariants(t, `UPDATE balance_accounts SET available_seconds = available_seconds - 5 WHERE user_id = $1`, uid)

	// (e) a HELD reservation on a COMPLETED session
	completed := p.sessionRow(t, p.openUser(t, 300), testutil.Row{"status": "COMPLETED", "end_reason": "USER_ENDED", "starting_at": t0, "started_at": t0,
		"expires_at": t0.Add(5 * time.Minute), "ending_at": t0.Add(time.Minute), "ended_at": t0.Add(time.Minute)})
	var acc uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM balance_accounts LIMIT 1`).Scan(&acc))
	p.breakInvariants(t, `INSERT INTO balance_reservations (id, user_id, account_id, session_id, reserved_seconds, status, start_deadline_at, created_at, updated_at)
	    SELECT $1, user_id, id, $2, 300, 'HELD', now() + interval '5 minutes', now(), now() FROM balance_accounts WHERE id = $3`, p.ids.New(), completed, acc)
	require.NotEmpty(t, runCheck(t, p, "R4"))

	// (f) a HELD reservation stuck 11 minutes past its deadline on a RESERVED session
	stuck := p.sessionRow(t, p.openUser(t, 300), nil)
	p.breakInvariants(t, `INSERT INTO balance_reservations (id, user_id, account_id, session_id, reserved_seconds, status, start_deadline_at, created_at, updated_at)
	    SELECT $1, user_id, id, $2, 300, 'HELD', now() - interval '11 minutes', now() - interval '16 minutes', now() FROM balance_accounts WHERE id = $3`, p.ids.New(), stuck, acc)
	require.NotEmpty(t, runCheck(t, p, "R6"))

	_, err := repo.RunCheck(ctx, "R7")
	require.Error(t, err)

	v, err := p.svc.Reconcile(ctx, repo)
	require.NoError(t, err)
	var checks []string
	for _, x := range v {
		checks = append(checks, x.Check)
	}
	require.Contains(t, checks, "R4")
	require.Contains(t, checks, "R6")
}
