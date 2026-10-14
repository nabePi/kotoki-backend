//go:build integration

package migrations_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type sessEnv struct {
	pool *pgxpool.Pool
	uid  uuid.UUID
	c    testutil.ContentIDs
}

func newSessEnv(t *testing.T) sessEnv {
	pool := testutil.NewPostgres(t)
	return sessEnv{pool: pool, uid: testutil.SeedUser(t, pool, nil), c: testutil.SeedContent(t, pool)}
}

func (e sessEnv) try(o testutil.Row) error {
	return testutil.TryInsert(context.Background(), e.pool, "sessions", testutil.SessionRow(e.uid, e.c, o))
}

func TestSessionStateChecks(t *testing.T) {
	e := newSessEnv(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	cases := []struct {
		name string
		o    testutil.Row
		con  string
	}{
		{"completed provider failure", testutil.Row{"status": "COMPLETED", "end_reason": "PROVIDER_FAILURE", "started_at": now, "starting_at": now, "ending_at": now, "ended_at": now, "expires_at": now.Add(300 * time.Second)}, "chk_sessions_state"},
		{"active without started_at", testutil.Row{"status": "ACTIVE"}, "chk_sessions_state"},
		{"cancelled charged", testutil.Row{"status": "CANCELLED", "end_reason": "USER_CANCELLED", "ended_at": now, "charged_seconds": 5}, "chk_sessions_state"},
		{"duration", testutil.Row{"duration_seconds": 450, "reserved_seconds": 450}, "chk_sessions_duration"},
		{"reserved", testutil.Row{"reserved_seconds": 600}, "chk_sessions_reserved"},
		{"idem key", testutil.Row{"create_idempotency_key": "short"}, "chk_sessions_idem_key"},
		{"expires", testutil.Row{"status": "ACTIVE", "started_at": now, "starting_at": now, "expires_at": now.Add(time.Hour)}, "chk_sessions_expires"},
		{"opening prompt on USER_FIRST", testutil.Row{"opening_prompt_version_id": e.c.PromptVersionID}, "chk_sessions_opening_prompt"},
		{"handoff before start", testutil.Row{"status": "ACTIVE", "started_at": now, "starting_at": now, "expires_at": now.Add(300 * time.Second), "handoff_at": now.Add(-time.Minute)}, "chk_sessions_handoff"},
		{"config_snapshot not object", testutil.Row{"config_snapshot": []byte(`[]`)}, "chk_sessions_config_snapshot"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { testutil.RequirePgErr(t, e.try(c.o), "23514", c.con) })
	}
	t.Run("config_snapshot required", func(t *testing.T) {
		testutil.RequirePgErr(t, e.try(testutil.Row{"config_snapshot": nil}), "23502", "")
	})
	t.Run("valid ACTIVE accepted", func(t *testing.T) {
		require.NoError(t, e.try(testutil.Row{"status": "ACTIVE", "started_at": now, "starting_at": now, "expires_at": now.Add(300 * time.Second)}))
	})
}

func TestSessionsHaveNoLegacyContentColumns(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	cols := func(table string) map[string]bool {
		rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name=$1`, table)
		require.NoError(t, err)
		defer rows.Close()
		m := map[string]bool{}
		for rows.Next() {
			var c string
			require.NoError(t, rows.Scan(&c))
			m[c] = true
		}
		return m
	}
	s, tt := cols("sessions"), cols("session_turns")
	for _, c := range []string{"character_version_id", "voice_profile_id", "conversation_style_version_id", "expression_profile_version_id", "provider_profile", "tier"} {
		require.False(t, s[c], c)
	}
	require.False(t, tt["expressions"])
	for _, c := range []string{"character_id", "scenario_version_id", "prompt_version_id", "opening_prompt_version_id", "adaptive_difficulty", "runtime_fence", "config_snapshot"} {
		require.True(t, s[c], c)
	}
	require.True(t, tt["tts_voice_id"])
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname='chk_sessions_provider_profile'`).Scan(&n))
	require.Zero(t, n)
	var del string
	require.NoError(t, pool.QueryRow(ctx, `SELECT confdeltype::text FROM pg_constraint WHERE conrelid='sessions'::regclass AND contype='f' AND confrelid='characters'::regclass`).Scan(&del))
	require.Equal(t, "r", del) // ON DELETE RESTRICT
}

func TestOneOpenSessionPerUser(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	first := testutil.SessionRow(e.uid, e.c, nil)
	testutil.MustInsert(ctx, t, e.pool, "sessions", first)
	testutil.RequirePgErr(t, e.try(nil), "23505", "ux_sessions_one_open_per_user")
	_, err := e.pool.Exec(ctx, `UPDATE sessions SET status='CANCELLED', end_reason='USER_CANCELLED', ended_at=now() WHERE id=$1`, first["id"])
	require.NoError(t, err)
	require.NoError(t, e.try(nil))
}

func TestIdempotencyKeyUnique(t *testing.T) {
	e := newSessEnv(t)
	now := time.Now().UTC()
	done := testutil.Row{"status": "CANCELLED", "end_reason": "USER_CANCELLED", "ended_at": now, "create_idempotency_key": "same-key-123"}
	require.NoError(t, e.try(done))
	testutil.RequirePgErr(t, e.try(done), "23505", "ux_sessions_user_idempotency")
}

func (e sessEnv) session(t *testing.T) uuid.UUID {
	return testutil.SeedSession(t, e.pool, e.uid, e.c, nil)
}

func turn(sid uuid.UUID, o testutil.Row) testutil.Row {
	r := testutil.Row{"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": 1, "generation": 0}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func TestTurnStatusNullable(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	sid := e.session(t)
	ins := func(o testutil.Row) error { return testutil.TryInsert(ctx, e.pool, "session_turns", turn(sid, o)) }
	require.NoError(t, ins(testutil.Row{"sequence_number": 1}))
	testutil.RequirePgErr(t, ins(testutil.Row{"sequence_number": 2, "status": "COMPLETED"}), "23514", "chk_turn_completed")
	testutil.RequirePgErr(t, ins(testutil.Row{"sequence_number": 3, "status": "X", "completed_at": time.Now().UTC()}), "23514", "chk_turn_status")
	testutil.RequirePgErr(t, ins(testutil.Row{"sequence_number": 0, "user_text": "halo"}), "23514", "chk_turn_opening")
	testutil.RequirePgErr(t, ins(testutil.Row{"sequence_number": 4, "stt_provider": "stt"}), "23514", "chk_turn_provider_upper")
}

func TestEventChecks(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	sid := e.session(t)
	ev := func(o testutil.Row) error {
		r := testutil.Row{"id": uuid.Must(uuid.NewV7()), "session_id": sid, "event_type": "BARGE_IN", "actor": "REALTIME"}
		for k, v := range o {
			r[k] = v
		}
		return testutil.TryInsert(ctx, e.pool, "session_events", r)
	}
	testutil.RequirePgErr(t, ev(testutil.Row{"event_type": "BOGUS"}), "23514", "chk_session_events_type")
	testutil.RequirePgErr(t, ev(testutil.Row{"actor": "X"}), "23514", "chk_session_events_actor")
	require.NoError(t, ev(testutil.Row{"event_type": "SESSION_RESERVED", "actor": "USER", "to_status": "RESERVED"}))
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func TestPartitionsExist(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	m0 := monthStart(clock.System{}.Now())
	for _, table := range []string{"session_turns", "session_events"} {
		rows, err := pool.Query(ctx, `SELECT c.relname, pg_get_expr(c.relpartbound, c.oid) FROM pg_inherits i
			JOIN pg_class c ON c.oid=i.inhrelid JOIN pg_class p ON p.oid=i.inhparent WHERE p.relname=$1 ORDER BY 1`, table)
		require.NoError(t, err)
		var got []string
		for rows.Next() {
			var name, bound string
			require.NoError(t, rows.Scan(&name, &bound))
			require.NotEqual(t, "DEFAULT", bound)
			got = append(got, name)
		}
		rows.Close()
		var want []string
		for i := 0; i < 4; i++ {
			want = append(want, fmt.Sprintf("%s_p%s", table, m0.AddDate(0, i, 0).Format("200601")))
		}
		require.Equal(t, want, got, table)
	}
}

func TestInsertRoutesToMonthPartition(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	sid := e.session(t)
	next := monthStart(clock.System{}.Now()).AddDate(0, 1, 1)
	r := turn(sid, testutil.Row{"created_at": next})
	testutil.MustInsert(ctx, t, e.pool, "session_turns", r)
	var rel string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM session_turns WHERE id=$1`, r["id"]).Scan(&rel))
	require.Equal(t, "session_turns_p"+next.Format("200601"), rel)
}

func TestInsertOutsideRangeFails(t *testing.T) {
	e := newSessEnv(t)
	sid := e.session(t)
	far := monthStart(clock.System{}.Now()).AddDate(0, 6, 1)
	err := testutil.TryInsert(context.Background(), e.pool, "session_turns", turn(sid, testutil.Row{"created_at": far}))
	testutil.RequirePgErr(t, err, "23514", "")
	require.Contains(t, err.Error(), "no partition")
}

func TestNoFKPointsToPartitioned(t *testing.T) {
	pool := testutil.NewPostgres(t)
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_constraint WHERE contype='f' AND confrelid IN ('session_turns'::regclass,'session_events'::regclass)`).Scan(&n))
	require.Zero(t, n)
}

func TestDeleteTurnKeepsEvents(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	sid := e.session(t)
	tr := turn(sid, nil)
	testutil.MustInsert(ctx, t, e.pool, "session_turns", tr)
	evID := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, e.pool, "session_events", testutil.Row{"id": evID, "session_id": sid, "event_type": "TURN_FAILED", "actor": "REALTIME", "turn_id": tr["id"]})
	_, err := e.pool.Exec(ctx, `DELETE FROM session_turns WHERE id=$1`, tr["id"])
	require.NoError(t, err)
	var got uuid.UUID
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT turn_id FROM session_events WHERE id=$1`, evID).Scan(&got))
	require.Equal(t, tr["id"], got)
}

func TestTurnPruning(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	sid := e.session(t)
	from := monthStart(clock.System{}.Now())
	q := fmt.Sprintf(`EXPLAIN SELECT * FROM session_turns WHERE session_id='%s' AND created_at >= '%s' AND created_at < '%s'`,
		sid, from.Format(time.RFC3339), from.AddDate(0, 1, 0).Format(time.RFC3339))
	rows, err := e.pool.Query(ctx, q)
	require.NoError(t, err)
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var l string
		require.NoError(t, rows.Scan(&l))
		plan.WriteString(l + "\n")
	}
	parts := map[string]bool{}
	for _, m := range regexp.MustCompile(`session_turns_p\d{6}`).FindAllString(plan.String(), -1) {
		parts[m] = true
	}
	require.Len(t, parts, 1, plan.String())
}
