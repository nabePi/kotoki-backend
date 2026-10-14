//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type balEnv struct {
	pool *pgxpool.Pool
	c    testutil.ContentIDs
}

type balCtx struct {
	uid, acc, sid uuid.UUID
	version       int64
}

func newBalEnv(t *testing.T) balEnv {
	pool := testutil.NewPostgres(t)
	return balEnv{pool: pool, c: testutil.SeedContent(t, pool)}
}

// fresh creates a user with an account and an open session.
func (e balEnv) fresh(t *testing.T) *balCtx {
	ctx := context.Background()
	uid := testutil.SeedUser(t, e.pool, nil)
	acc := testutil.BalanceAccountRow(uid, nil)
	testutil.MustInsert(t, ctx, e.pool, "balance_accounts", acc)
	sid := testutil.SeedSession(t, e.pool, uid, e.c, nil)
	return &balCtx{uid: uid, acc: acc["id"].(uuid.UUID), sid: sid}
}

func (e balEnv) reservation(t *testing.T, b *balCtx, o testutil.Row) testutil.Row {
	r := testutil.ReservationRow(b.acc, b.uid, b.sid, o)
	testutil.MustInsert(t, context.Background(), e.pool, "balance_reservations", r)
	return r
}

func TestBalanceReservationChecks(t *testing.T) {
	e := newBalEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	try := func(o testutil.Row) error {
		b := e.fresh(t)
		return testutil.TryInsert(ctx, e.pool, "balance_reservations", testutil.ReservationRow(b.acc, b.uid, b.sid, o))
	}
	bad := func(name string, o testutil.Row, con string) {
		t.Run(name, func(t *testing.T) { testutil.RequirePgErr(t, try(o), "23514", con) })
	}
	bad("reserved 450", testutil.Row{"reserved_seconds": 450}, "chk_reservation_reserved")
	bad("held with consumed", testutil.Row{"consumed_seconds": 0}, "chk_reservation_state")
	bad("settled mismatch", testutil.Row{"status": "SETTLED", "consumed_seconds": 100, "released_seconds": 100, "settled_at": now}, "chk_reservation_state")
	bad("released with consumed", testutil.Row{"status": "RELEASED", "consumed_seconds": 10, "released_seconds": 290, "released_at": now}, "chk_reservation_state")
	bad("deadline", testutil.Row{"start_deadline_at": now.Add(-time.Hour), "created_at": now}, "chk_reservation_deadline")
	t.Run("settled zero consumed accepted", func(t *testing.T) {
		require.NoError(t, try(testutil.Row{"status": "SETTLED", "consumed_seconds": 0, "released_seconds": 300, "settled_at": now}))
	})
	t.Run("two reservations one session", func(t *testing.T) {
		b := e.fresh(t)
		e.reservation(t, b, nil)
		testutil.RequirePgErr(t, testutil.TryInsert(ctx, e.pool, "balance_reservations", testutil.ReservationRow(b.acc, b.uid, b.sid, nil)), "23505", "ux_balance_reservations_session")
	})
}

func (b *balCtx) ledger(kind string, resID uuid.UUID, o testutil.Row) testutil.Row {
	b.version++
	r := testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": b.uid, "account_id": b.acc, "entry_type": kind,
		"account_version": b.version, "reference_type": "SESSION", "session_id": b.sid, "reservation_id": resID,
		"available_seconds_after": 0, "reserved_seconds_after": 0,
	}
	switch kind {
	case "RESERVE":
		r["reserved_seconds_delta"], r["available_seconds_delta"], r["idempotency_key"] = 300, -300, "reserve:"+resID.String()
	case "RELEASE":
		r["reserved_seconds_delta"], r["available_seconds_delta"], r["idempotency_key"] = -300, 300, "release:"+resID.String()
	case "CONSUME":
		r["reserved_seconds_delta"], r["available_seconds_delta"], r["idempotency_key"] = -300, 0, "consume:"+resID.String()
	case "GRANT":
		r["reserved_seconds_delta"], r["available_seconds_delta"], r["idempotency_key"] = 0, 300, "grant:"+uuid.NewString()
		r["session_id"], r["reservation_id"], r["reference_type"] = nil, nil, "REGISTRATION"
	case "ADJUSTMENT":
		r["reserved_seconds_delta"], r["available_seconds_delta"], r["idempotency_key"] = 0, 60, "adjust:"+uuid.NewString()
		r["session_id"], r["reservation_id"], r["reference_type"] = nil, nil, "ADMIN"
	}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func TestBalanceLedgerShapes(t *testing.T) {
	e := newBalEnv(t)
	ctx := context.Background()
	admin := testutil.SeedUser(t, e.pool, testutil.Row{"role": "ADMIN"})
	ins := func(r testutil.Row) error { return testutil.TryInsert(ctx, e.pool, "balance_ledger", r) }
	setup := func() (*balCtx, uuid.UUID) {
		b := e.fresh(t)
		return b, e.reservation(t, b, nil)["id"].(uuid.UUID)
	}

	t.Run("RESERVE accepted", func(t *testing.T) {
		b, res := setup()
		require.NoError(t, ins(b.ledger("RESERVE", res, nil)))
	})
	t.Run("RESERVE wrong key", func(t *testing.T) {
		b, res := setup()
		testutil.RequirePgErr(t, ins(b.ledger("RESERVE", res, testutil.Row{"idempotency_key": "reserve:other"})), "23514", "chk_ledger_shape")
	})
	t.Run("GRANT ADMIN without admin_user_id", func(t *testing.T) {
		b, _ := setup()
		testutil.RequirePgErr(t, ins(b.ledger("GRANT", uuid.Nil, testutil.Row{"reference_type": "ADMIN", "reason": "bonus"})), "23514", "chk_ledger_shape")
	})
	t.Run("GRANT ADMIN accepted", func(t *testing.T) {
		b, _ := setup()
		require.NoError(t, ins(b.ledger("GRANT", uuid.Nil, testutil.Row{"reference_type": "ADMIN", "admin_user_id": admin, "reason": "bonus"})))
	})
	t.Run("CONSUME with available delta", func(t *testing.T) {
		b, res := setup()
		testutil.RequirePgErr(t, ins(b.ledger("CONSUME", res, testutil.Row{"available_seconds_delta": 10})), "23514", "chk_ledger_shape")
	})
	t.Run("ADJUSTMENT zero delta", func(t *testing.T) {
		b, _ := setup()
		testutil.RequirePgErr(t, ins(b.ledger("ADJUSTMENT", uuid.Nil, testutil.Row{"available_seconds_delta": 0, "admin_user_id": admin, "reason": "fix"})), "23514", "chk_ledger_shape")
	})
	t.Run("after negative", func(t *testing.T) {
		b, res := setup()
		testutil.RequirePgErr(t, ins(b.ledger("RESERVE", res, testutil.Row{"available_seconds_after": -1})), "23514", "chk_ledger_after_non_negative")
	})
	t.Run("reason too short", func(t *testing.T) {
		b, _ := setup()
		testutil.RequirePgErr(t, ins(b.ledger("GRANT", uuid.Nil, testutil.Row{"reason": "ab"})), "23514", "chk_ledger_reason_len")
	})
	t.Run("entry type", func(t *testing.T) {
		b, res := setup()
		testutil.RequirePgErr(t, ins(b.ledger("RESERVE", res, testutil.Row{"entry_type": "BONUS"})), "23514", "chk_ledger_entry_type")
	})
	t.Run("duplicate idempotency key", func(t *testing.T) {
		b, _ := setup()
		first := b.ledger("GRANT", uuid.Nil, nil)
		require.NoError(t, ins(first))
		testutil.RequirePgErr(t, ins(b.ledger("GRANT", uuid.Nil, testutil.Row{"idempotency_key": first["idempotency_key"]})), "23505", "ux_balance_ledger_idempotency")
	})
	t.Run("duplicate account_version", func(t *testing.T) {
		b, _ := setup()
		require.NoError(t, ins(b.ledger("GRANT", uuid.Nil, nil)))
		testutil.RequirePgErr(t, ins(b.ledger("GRANT", uuid.Nil, testutil.Row{"account_version": b.version})), "23505", "ux_balance_ledger_account_version")
	})
	t.Run("append-only", func(t *testing.T) {
		b, _ := setup()
		r := b.ledger("GRANT", uuid.Nil, nil)
		require.NoError(t, ins(r))
		_, err := e.pool.Exec(ctx, `UPDATE balance_ledger SET reason='changed' WHERE id=$1`, r["id"])
		testutil.RequirePgErr(t, err, "23001", "")
		require.True(t, strings.Contains(err.Error(), "append-only"))
		_, err = e.pool.Exec(ctx, `DELETE FROM balance_ledger WHERE id=$1`, r["id"])
		testutil.RequirePgErr(t, err, "23001", "")
		require.True(t, strings.Contains(err.Error(), "append-only"))
	})
}
