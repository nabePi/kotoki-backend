//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func outboxRow(o testutil.Row) testutil.Row {
	r := testutil.Row{
		"event_id": uuid.Must(uuid.NewV7()), "event_type": "session.completed", "aggregate_type": "SESSION",
		"aggregate_id": uuid.Must(uuid.NewV7()), "user_id": uuid.Must(uuid.NewV7()), "producer": "API",
		"payload": []byte(`{}`), "occurred_at": time.Now().UTC(),
	}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func TestOutboxChecks(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	ins := func(o testutil.Row) error { return testutil.TryInsert(ctx, pool, "event_outbox", outboxRow(o)) }
	bad := func(name string, o testutil.Row, con string) {
		t.Run(name, func(t *testing.T) { testutil.RequirePgErr(t, ins(o), "23514", con) })
	}
	bad("published without published_at", testutil.Row{"status": "PUBLISHED"}, "chk_event_outbox_published_at")
	bad("producer CRON", testutil.Row{"producer": "CRON"}, "chk_event_outbox_producer")
	bad("aggregate USER", testutil.Row{"aggregate_type": "USER"}, "chk_event_outbox_aggregate_type")
	bad("status DONE", testutil.Row{"status": "DONE"}, "chk_event_outbox_status")
	bad("payload too large", testutil.Row{"payload": []byte(`{"x":"` + strings.Repeat("a", 65537) + `"}`)}, "chk_event_outbox_payload_size")
	bad("attempts negative", testutil.Row{"attempts": -1}, "chk_event_outbox_attempts")
	t.Run("trace context accepted", func(t *testing.T) {
		require.NoError(t, ins(testutil.Row{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "tracestate": "vendor=1"}))
	})
	t.Run("ready index used", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SET enable_seqscan = off`)
		require.NoError(t, err)
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()
		_, err = conn.Exec(ctx, `SET enable_seqscan = off`)
		require.NoError(t, err)
		rows, err := conn.Query(ctx, `EXPLAIN SELECT event_id FROM event_outbox WHERE status='PENDING' ORDER BY available_at, event_id LIMIT 10`)
		require.NoError(t, err)
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var l string
			require.NoError(t, rows.Scan(&l))
			plan.WriteString(l + "\n")
		}
		require.Contains(t, plan.String(), "idx_event_outbox_ready")
	})
}

func TestProcessedEvents(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	row := func(consumer string, id uuid.UUID) testutil.Row {
		return testutil.Row{"consumer": consumer, "event_id": id, "event_type": "session.completed"}
	}
	id := uuid.Must(uuid.NewV7())
	testutil.MustInsert(t, ctx, pool, "processed_events", row("evaluation.session_completed", id))
	testutil.RequirePgErr(t, testutil.TryInsert(ctx, pool, "processed_events", row("evaluation.session_completed", id)), "23505", "processed_events_pkey")
	testutil.RequirePgErr(t, testutil.TryInsert(ctx, pool, "processed_events", row("nobody", uuid.Must(uuid.NewV7()))), "23514", "chk_processed_events_consumer")
}

func TestAdminAudit(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	admin := testutil.SeedUser(t, pool, testutil.Row{"role": "ADMIN"})
	row := func(o testutil.Row) testutil.Row {
		r := testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "admin_user_id": admin, "action": "CHARACTER_UPDATED",
			"target_type": "CHARACTER", "target_id": uuid.NewString(), "request_id": "req-1",
		}
		for k, v := range o {
			r[k] = v
		}
		return r
	}
	testutil.RequirePgErr(t, testutil.TryInsert(ctx, pool, "admin_audit_logs", row(testutil.Row{"action": "HACK"})), "23514", "chk_admin_audit_action")
	testutil.RequirePgErr(t, testutil.TryInsert(ctx, pool, "admin_audit_logs", row(testutil.Row{"target_type": "X"})), "23514", "chk_admin_audit_target_type")
	ok := row(nil)
	testutil.MustInsert(t, ctx, pool, "admin_audit_logs", ok)
	_, err := pool.Exec(ctx, `UPDATE admin_audit_logs SET reason='x' WHERE id=$1`, ok["id"])
	testutil.RequirePgErr(t, err, "23001", "")
	_, err = pool.Exec(ctx, `DELETE FROM admin_audit_logs WHERE id=$1`, ok["id"])
	testutil.RequirePgErr(t, err, "23001", "")
}

func TestDownAuditKeepsRejectMutation(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	m := newMigrator(t, dsn)
	require.NoError(t, m.Migrate(18))
	hasFn := func() bool {
		return len(queryStrings(t, dsn, `SELECT proname FROM pg_proc WHERE proname='reject_mutation'`)) == 1
	}
	require.NoError(t, m.Steps(-1))
	require.Empty(t, queryStrings(t, dsn, `SELECT tablename FROM pg_tables WHERE tablename='admin_audit_logs'`))
	require.True(t, hasFn(), "reject_mutation belongs to 000013")
	require.NoError(t, m.Migrate(12))
	require.False(t, hasFn(), "000013 down drops reject_mutation")
	require.NoError(t, m.Up())
}
