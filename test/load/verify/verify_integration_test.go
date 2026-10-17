//go:build integration && !windows

package verify

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/sessionenv"
)

// env breaks the books on purpose, so the harness's own end-of-test reconciliation is off.
func newEnv(t *testing.T) *sessionenv.Env { return sessionenv.New(t, sessionenv.SkipFinalReconcile()) }

// finish runs one real, billed lifecycle (60 s of active time on the fake clock).
func finish(t *testing.T, e *sessionenv.Env) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := e.SeedActive(t, sessionenv.ActiveOpts{})
	fence := e.Fence(t, id)
	e.Clock.Advance(time.Minute)
	_, err := e.Sessions.BeginEnding(ctx, id, &fence, session.EndUserEnded, "", session.ActorRealtime)
	require.NoError(t, err)
	_, err = e.Sessions.Finalize(ctx, id, &fence, session.ActorRealtime)
	require.NoError(t, err)
	return id
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err, sql)
}

// completedEvent returns the id of the session.completed event of the session, creating the
// outbox row when the lifecycle did not write one.
func completedEvent(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `SELECT event_id FROM event_outbox WHERE event_type = 'session.completed' AND aggregate_id = $1`, sessionID).Scan(&id)
	if err == nil {
		return id
	}
	require.ErrorIs(t, err, pgx.ErrNoRows)
	id = uuid.New()
	exec(t, pool, `INSERT INTO event_outbox (event_id, event_type, aggregate_type, aggregate_id, user_id, producer, payload, occurred_at, status, published_at)
	               SELECT $1, 'session.completed', 'SESSION', s.id, s.user_id, 'REALTIME', '{}', now(), 'PUBLISHED', now() FROM sessions s WHERE s.id = $2`, id, sessionID)
	return id
}

// evaluate adds what the evaluation consumer leaves behind for the session: a COMPLETED evaluation
// and the processed_events row of the session.completed event.
func evaluate(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) {
	t.Helper()
	ev := completedEvent(t, pool, sessionID)
	exec(t, pool, `INSERT INTO evaluations (id, session_id, user_id, source_event_id, status, insufficient_evidence, rubric_version, completed_at)
	               SELECT $1, s.id, s.user_id, $3, 'COMPLETED', true, 'rubric-v1', now() FROM sessions s WHERE s.id = $2`, uuid.New(), sessionID, ev)
	exec(t, pool, `INSERT INTO processed_events (consumer, event_id, event_type) VALUES ('evaluation.session_completed', $1, 'session.completed')`, ev)
}

func run(t *testing.T, pool *pgxpool.Pool, o Options) []Violation {
	t.Helper()
	vs, err := Run(context.Background(), pool, o)
	require.NoError(t, err)
	return vs
}

func only(t *testing.T, vs []Violation, check string) Violation {
	t.Helper()
	require.Len(t, vs, 1, "%v", vs)
	require.Equal(t, check, vs[0].Check)
	require.NotEmpty(t, vs[0].Detail)
	return vs[0]
}

func TestVerify_CleanDatabase(t *testing.T) {
	e := newEnv(t)
	done := finish(t, e)
	evaluate(t, e.Pool, done)
	e.SeedActive(t, sessionenv.ActiveOpts{}) // a HELD reservation on an ACTIVE session is consistent
	vs := run(t, e.Pool, Options{AwaitEvaluations: time.Millisecond})
	require.Equal(t, []Violation{}, vs, "non-nil empty slice: a clean run says so")
}

func TestVerify_V1_BalanceDiffersFromLedger(t *testing.T) {
	e := newEnv(t)
	finish(t, e)
	exec(t, e.Pool, `UPDATE balance_accounts SET available_seconds = available_seconds + 1 WHERE id = (SELECT id FROM balance_accounts ORDER BY created_at LIMIT 1)`)
	only(t, run(t, e.Pool, Options{}), "V1")
}

func TestVerify_V2_NegativeBalance(t *testing.T) {
	e := newEnv(t)
	finish(t, e)
	// The CHECKs and the append-only trigger are dropped (a disposable database) and the ledger is kept
	// in step with the account, so that V1 stays quiet and only V2 fires.
	exec(t, e.Pool, `ALTER TABLE balance_accounts DROP CONSTRAINT chk_balance_available_non_negative`)
	exec(t, e.Pool, `ALTER TABLE balance_ledger DROP CONSTRAINT chk_ledger_after_non_negative`)
	exec(t, e.Pool, `INSERT INTO balance_ledger (id, user_id, account_id, entry_type, available_seconds_delta, reserved_seconds_delta, available_seconds_after, reserved_seconds_after, account_version, reference_type, admin_user_id, reason, idempotency_key)
	                 SELECT $1, a.user_id, a.id, 'ADJUSTMENT', -1 - a.available_seconds, 0, -1, a.reserved_seconds, a.version + 1, 'ADMIN', a.user_id, 'drive negative', 'adjust:neg'
	                   FROM balance_accounts a ORDER BY a.created_at LIMIT 1`, uuid.New())
	exec(t, e.Pool, `UPDATE balance_accounts SET available_seconds = -1, version = version + 1 WHERE id = (SELECT id FROM balance_accounts ORDER BY created_at LIMIT 1)`)
	only(t, run(t, e.Pool, Options{}), "V2")
}

func TestVerify_V3_HeldReservationOfTerminalSession(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	exec(t, e.Pool, `UPDATE balance_reservations SET status = 'HELD', consumed_seconds = NULL, released_seconds = NULL, settled_at = NULL WHERE session_id = $1`, id)
	only(t, run(t, e.Pool, Options{}), "V3")
}

func TestVerify_V4_SecondConsume(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	// a double CONSUME cannot be written under the schema's uniqueness; drop those so that the check itself is tested
	exec(t, e.Pool, `ALTER TABLE balance_ledger DROP CONSTRAINT ux_balance_ledger_idempotency`)
	exec(t, e.Pool, `ALTER TABLE balance_ledger DROP CONSTRAINT ux_balance_ledger_account_version`)
	exec(t, e.Pool, `ALTER TABLE balance_ledger DROP CONSTRAINT chk_ledger_after_non_negative`)
	exec(t, e.Pool, `INSERT INTO balance_ledger (id, user_id, account_id, entry_type, available_seconds_delta, reserved_seconds_delta, available_seconds_after, reserved_seconds_after, account_version, session_id, reservation_id, reference_type, idempotency_key)
	                 SELECT $1, user_id, account_id, 'CONSUME', 0, -1, available_seconds_after, reserved_seconds_after, account_version + 100, session_id, reservation_id, 'SESSION', idempotency_key
	                   FROM balance_ledger WHERE session_id = $2 AND entry_type = 'CONSUME'`, uuid.New(), id)
	vs := run(t, e.Pool, Options{})
	found := false
	for _, v := range vs {
		if v.Check == "V4" {
			found = true
		}
	}
	require.True(t, found, "%v", vs)
}

func TestVerify_V5_ChargedOverReserved(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	exec(t, e.Pool, `ALTER TABLE sessions DROP CONSTRAINT chk_sessions_charged`)
	exec(t, e.Pool, `UPDATE sessions SET charged_seconds = reserved_seconds WHERE id = $1`, id)
	require.Empty(t, run(t, e.Pool, Options{}), "charged equal to reserved is the limit, not a violation")
	exec(t, e.Pool, `UPDATE sessions SET charged_seconds = reserved_seconds + 1 WHERE id = $1`, id)
	only(t, run(t, e.Pool, Options{}), "V5")
}

func TestVerify_V6_DuplicateTurnSequence(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	now := time.Now().UTC()
	for i, created := range []time.Time{now, now.Add(time.Second)} {
		testutil.MustInsert(context.Background(), t, e.Pool, "session_turns", testutil.Row{
			"id": uuid.New(), "session_id": id, "sequence_number": 1, "status": "COMPLETED", "generation": 0,
			"created_at": created, "completed_at": created.Add(time.Duration(i) * time.Millisecond),
		})
	}
	only(t, run(t, e.Pool, Options{}), "V6")
}

func TestVerify_V7_DoubleEvaluation(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	evaluate(t, e.Pool, id)
	exec(t, e.Pool, `ALTER TABLE evaluations DROP CONSTRAINT uq_evaluations_session`)
	exec(t, e.Pool, `INSERT INTO evaluations (id, session_id, user_id, source_event_id, status, insufficient_evidence, rubric_version, completed_at)
	                 SELECT $1, s.id, s.user_id, $3, 'COMPLETED', true, 'rubric-v1', now() FROM sessions s WHERE s.id = $2`, uuid.New(), id, completedEvent(t, e.Pool, id))
	only(t, run(t, e.Pool, Options{}), "V7")
}

func TestVerify_V7_EvaluationWithoutProcessedEvent(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	evaluate(t, e.Pool, id)
	exec(t, e.Pool, `DELETE FROM processed_events WHERE consumer = 'evaluation.session_completed'`)
	only(t, run(t, e.Pool, Options{}), "V7")
}

func TestVerify_V8_CompletedSessionWithoutEvaluation(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	completedEvent(t, e.Pool, id)

	require.Empty(t, run(t, e.Pool, Options{}), "V8 is off without AwaitEvaluations")

	start := time.Now()
	vs := run(t, e.Pool, Options{AwaitEvaluations: 300 * time.Millisecond, PollEvery: 50 * time.Millisecond})
	only(t, vs, "V8")
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "it waits the whole window before it reports")
}

func TestVerify_V8_FailedEvaluationDoesNotCount(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	exec(t, e.Pool, `INSERT INTO evaluations (id, session_id, user_id, source_event_id, status, failure_code, rubric_version)
	                 SELECT $1, s.id, s.user_id, $3, 'FAILED', 'INTERNAL_ERROR', 'rubric-v1' FROM sessions s WHERE s.id = $2`, uuid.New(), id, completedEvent(t, e.Pool, id))
	only(t, run(t, e.Pool, Options{AwaitEvaluations: 50 * time.Millisecond, PollEvery: 10 * time.Millisecond}), "V8")
}

func TestVerify_V8_NoOutboxRowIsStillAViolation(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	exec(t, e.Pool, `DELETE FROM event_outbox WHERE aggregate_id = $1`, id)
	only(t, run(t, e.Pool, Options{AwaitEvaluations: 50 * time.Millisecond, PollEvery: 10 * time.Millisecond}), "V8")
}

func TestVerify_V8_LastLookAtTheEndOfTheWindow(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	completedEvent(t, e.Pool, id)
	go func() {
		time.Sleep(150 * time.Millisecond)
		evaluate(t, e.Pool, id)
	}()
	// the poll interval is longer than the window: only the final look can see the evaluation
	require.Empty(t, run(t, e.Pool, Options{AwaitEvaluations: 600 * time.Millisecond, PollEvery: time.Hour}))
}

func TestVerify_V8_EvaluationArrivingInTheWindowIsAccepted(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	completedEvent(t, e.Pool, id)
	go func() {
		time.Sleep(200 * time.Millisecond)
		evaluate(t, e.Pool, id)
	}()
	start := time.Now()
	vs := run(t, e.Pool, Options{AwaitEvaluations: 8 * time.Second, PollEvery: 50 * time.Millisecond})
	require.Empty(t, vs)
	require.Less(t, time.Since(start), 5*time.Second, "it returns as soon as every evaluation is there")
}

func TestVerify_SinceFiltersOldRows(t *testing.T) {
	e := newEnv(t)
	id := finish(t, e)
	completedEvent(t, e.Pool, id)
	exec(t, e.Pool, `UPDATE balance_reservations SET status = 'HELD', consumed_seconds = NULL, released_seconds = NULL, settled_at = NULL WHERE session_id = $1`, id) // V3
	exec(t, e.Pool, `ALTER TABLE sessions DROP CONSTRAINT chk_sessions_charged`)
	exec(t, e.Pool, `UPDATE sessions SET charged_seconds = reserved_seconds + 1 WHERE id = $1`, id) // V5
	now := time.Now().UTC()
	for _, c := range []time.Time{now, now.Add(time.Second)} { // V6
		testutil.MustInsert(context.Background(), t, e.Pool, "session_turns", testutil.Row{
			"id": uuid.New(), "session_id": id, "sequence_number": 1, "status": "COMPLETED", "generation": 0, "created_at": c, "completed_at": c,
		})
	}
	opts := Options{AwaitEvaluations: 50 * time.Millisecond, PollEvery: 10 * time.Millisecond} // V8

	all := run(t, e.Pool, opts)
	checks := map[string]bool{}
	for _, v := range all {
		checks[v.Check] = true
	}
	for _, c := range []string{"V3", "V5", "V6", "V8"} {
		require.True(t, checks[c], "%s expected in %v", c, all)
	}

	var created time.Time
	require.NoError(t, e.Pool.QueryRow(context.Background(), `SELECT created_at FROM sessions WHERE id = $1`, id).Scan(&created))
	opts.Since = created.Add(time.Minute)
	for _, v := range run(t, e.Pool, opts) {
		require.NotContains(t, []string{"V3", "V4", "V5", "V6", "V7", "V8"}, v.Check, "rows before Since are ignored: %v", v)
	}
}

func TestVerify_V1V2IgnoreSince(t *testing.T) {
	e := newEnv(t)
	finish(t, e)
	exec(t, e.Pool, `UPDATE balance_accounts SET available_seconds = available_seconds + 1 WHERE id = (SELECT id FROM balance_accounts ORDER BY created_at LIMIT 1)`)
	only(t, run(t, e.Pool, Options{Since: time.Now().Add(time.Hour)}), "V1")
}

func TestVerify_ChecksRunReadOnlyWithTimeout(t *testing.T) {
	e := newEnv(t)
	evaluate(t, e.Pool, finish(t, e))
	var modes, timeouts []string
	var writeErr error
	opts := Options{AwaitEvaluations: time.Millisecond}
	opts.probe = func(ctx context.Context, tx pgx.Tx) {
		var ro, to string
		require.NoError(t, tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&ro))
		require.NoError(t, tx.QueryRow(ctx, `SHOW statement_timeout`).Scan(&to))
		modes, timeouts = append(modes, ro), append(timeouts, to)
		if writeErr == nil {
			sp, err := tx.Begin(ctx) // a savepoint: the refused write must not abort the check
			require.NoError(t, err)
			_, writeErr = sp.Exec(ctx, `UPDATE balance_accounts SET available_seconds = available_seconds`)
			_ = sp.Rollback(ctx)
		}
	}
	run(t, e.Pool, opts)
	require.Len(t, modes, 8, "every check runs in its own transaction")
	for i := range modes {
		require.Equal(t, "on", modes[i])
		require.Equal(t, "1min", timeouts[i])
	}
	require.Error(t, writeErr, "a write inside a check's transaction is refused")
	require.Contains(t, writeErr.Error(), "read-only")
}

func TestVerify_ContextCancelIsAnError(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, e.Pool, Options{})
	require.Error(t, err)
}
