//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

type pgSession struct {
	pool    *pgxpool.Pool
	store   *postgres.Store
	content testutil.ContentIDs
}

func newPGSession(t *testing.T) *pgSession {
	t.Helper()
	pool := testutil.NewPostgres(t)
	return &pgSession{pool: pool, store: postgres.NewStore(pool), content: testutil.SeedContent(t, pool)}
}

// build returns an uninserted RESERVED session for owner (a fresh user when nil).
func (p *pgSession) build(t *testing.T, owner *uuid.UUID, over testutil.Row) session.Session {
	t.Helper()
	var uid uuid.UUID
	if owner != nil {
		uid = *owner
	} else {
		uid = testutil.SeedUser(t, p.pool, nil)
	}
	row := testutil.SessionRow(uid, p.content, over)
	return session.Session{
		ID: row["id"].(uuid.UUID), UserID: uid, Status: session.StatusReserved, ConversationStartMode: session.StartMode(row["conversation_start_mode"].(string)),
		DurationSeconds: row["duration_seconds"].(int), TopicID: p.content.TopicID, ScenarioID: p.content.ScenarioID, ScenarioVersionID: p.content.ScenarioVersionID,
		CharacterID: p.content.CharacterID, PromptVersionID: p.content.PromptVersionID, ConfigSnapshot: json.RawMessage(row["config_snapshot"].([]byte)),
		AdaptiveDifficulty: json.RawMessage(row["adaptive_difficulty"].([]byte)), ReservedSeconds: row["reserved_seconds"].(int),
		CreateIdempotencyKey: row["create_idempotency_key"].(string), CreateRequestHash: row["create_request_hash"].([]byte),
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestSessionRepoSuite(t *testing.T) {
	repotest.RunSessionRepoSuite(t, func(t *testing.T) repotest.SessionHarness {
		p := newPGSession(t)
		return repotest.SessionHarness{
			Store: p.store.SessionStore(),
			Build: func(t *testing.T, owner *uuid.UUID) session.Session { return p.build(t, owner, nil) },
		}
	})
}

func TestSessionRepoCheckConstraints(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	u := testutil.SeedUser(t, p.pool, nil)

	reject := func(name string, over testutil.Row) {
		err := testutil.TryInsert(ctx, p.pool, "sessions", testutil.SessionRow(testutil.SeedUser(t, p.pool, nil), p.content, over))
		require.Error(t, err, name)
	}
	reject("COMPLETED with PROVIDER_FAILURE", testutil.Row{"status": "COMPLETED", "end_reason": "PROVIDER_FAILURE",
		"starting_at": time.Now(), "started_at": time.Now(), "ending_at": time.Now(), "ended_at": time.Now()})
	reject("config_snapshot is not an object", testutil.Row{"config_snapshot": []byte(`[]`)})
	reject("ACTIVE without started_at", testutil.Row{"status": "ACTIVE"})
	reject("opening prompt on USER_FIRST", testutil.Row{"opening_prompt_version_id": p.content.PromptVersionID})
	reject("handoff before start", testutil.Row{"status": "ACTIVE", "starting_at": time.Now(), "started_at": time.Now(), "expires_at": time.Now().Add(300 * time.Second),
		"handoff_at": time.Now().Add(-time.Hour)})

	// the tables carry no legacy profile columns (D-014)
	for _, col := range []string{"provider_profile", "character_version_id", "voice_profile_id", "conversation_style_version_id", "expression_profile_version_id"} {
		var n int
		require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'sessions' AND column_name = $1`, col).Scan(&n))
		require.Zero(t, n, col)
	}
	_ = u
}

func TestSessionRepoSnapshotKeepsVoiceFallback(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	s := p.build(t, nil, nil)
	snap := `{"character":{"id":"` + p.content.CharacterID.String() + `","code":"HARUKA","name":"Haruka","persona":"x","provider_name":"ELEVENLABS","model_name":"m","voice_id":"v",` +
		`"voice":{"gender":"FEMALE","accent":"KANTO","speaking_rate":1,"fallback":{"provider_name":"CARTESIA","model_name":"sonic","voice_id":"fb"}}},"formality":"KEIGO"}`
	s.ConfigSnapshot = json.RawMessage(snap)
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) }))
	got, err := postgres.NewSessionRepo(p.pool).GetByID(ctx, s.ID)
	require.NoError(t, err)
	require.JSONEq(t, snap, string(got.ConfigSnapshot))
}

// TestConfigSnapshotImmutable: no UPDATE of sessions in the sqlc sources assigns config_snapshot.
func TestConfigSnapshotImmutable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "db", "queries", "sessions.sql"))
	require.NoError(t, err)
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	updates := regexp.MustCompile(`(?is)UPDATE\s+sessions\s+SET.*?;`).FindAllString(strings.Join(code, "\n"), -1)
	require.NotEmpty(t, updates)
	for _, u := range updates {
		require.NotContains(t, strings.ToLower(u), "config_snapshot")
		require.NotContains(t, strings.ToLower(u), "adaptive_difficulty", "the adaptive snapshot is immutable too")
	}
}

func TestSessionPartitions(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	for _, tbl := range []string{"session_turns", "session_events"} {
		rows, err := p.pool.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class parent ON parent.oid = i.inhparent WHERE parent.relname = $1`, tbl)
		require.NoError(t, err)
		var names []string
		for rows.Next() {
			var n string
			require.NoError(t, rows.Scan(&n))
			names = append(names, n)
		}
		rows.Close()
		require.GreaterOrEqual(t, len(names), 4, tbl+": the current month and three more")
		now := time.Now().UTC()
		require.Contains(t, names, tbl+"_p"+now.Format("200601"))
		require.Contains(t, names, tbl+"_p"+now.AddDate(0, 1, 0).Format("200601"))

		// no foreign key points at a partitioned table (S-6)
		var refs int
		require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND confrelid = $1::regclass`, tbl).Scan(&refs))
		require.Zero(t, refs, tbl)
	}

	// a turn created next month lands in next month's partition; one outside the range is refused
	s := p.build(t, nil, nil)
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) }))
	next := time.Now().UTC().AddDate(0, 1, 0)
	id := uuid.Must(uuid.NewV7())
	_, err := p.pool.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, generation, created_at) VALUES ($1, $2, 1, 1, $3)`, id, s.ID, next)
	require.NoError(t, err)
	var part string
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM session_turns WHERE id = $1`, id).Scan(&part))
	require.Equal(t, "session_turns_p"+next.Format("200601"), part)
	_, err = p.pool.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, generation, created_at) VALUES ($1, $2, 2, 1, $3)`, uuid.Must(uuid.NewV7()), s.ID, time.Now().UTC().AddDate(5, 0, 0))
	require.Error(t, err, "no DEFAULT partition")

	// partition pruning: a created_at bound keeps the plan to the relevant partitions
	var plan string
	rows, err := p.pool.Query(ctx, `EXPLAIN SELECT * FROM session_turns WHERE session_id = $1 AND created_at >= $2 AND created_at <= $3`, s.ID, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan += line + "\n"
	}
	rows.Close()
	require.Contains(t, plan, "session_turns_p"+time.Now().UTC().Format("200601"))
	require.NotContains(t, plan, "session_turns_p"+next.AddDate(0, 2, 0).Format("200601"))
}

func TestSessionTurnStatusAcceptsNull(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	s := p.build(t, nil, nil)
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) }))
	_, err := p.pool.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, generation, created_at) VALUES ($1, $2, 1, 1, now())`, uuid.Must(uuid.NewV7()), s.ID)
	require.NoError(t, err, "chk_turn_status accepts NULL (turn in progress)")
	_, err = p.pool.Exec(ctx, `INSERT INTO session_turns (id, session_id, sequence_number, generation, status, created_at) VALUES ($1, $2, 2, 1, 'BOGUS', now())`, uuid.Must(uuid.NewV7()), s.ID)
	require.Error(t, err)
}

func TestSessionTurnColumnsHaveNoExpressionsOrAudio(t *testing.T) {
	p := newPGSession(t)
	for _, col := range []string{"expressions", "expression_profile_id", "user_audio_key", "audio_key"} {
		var n int
		require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_name = 'session_turns' AND column_name = $1`, col).Scan(&n))
		require.Zero(t, n, col)
	}
}

func TestSessionAbandonCandidates(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	s := p.build(t, nil, nil)
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) }))
	acc := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, p.pool, "balance_accounts", testutil.Row{"id": acc, "user_id": s.UserID, "available_seconds": 0, "reserved_seconds": 300})
	testutil.MustInsert(ctx, t, p.pool, "balance_reservations", testutil.ReservationRow(acc, s.UserID, s.ID, testutil.Row{"created_at": time.Now().UTC().Add(-10 * time.Minute), "start_deadline_at": time.Now().UTC().Add(-time.Minute)}))
	ids, err := postgres.NewSessionRepo(p.pool).ListAbandonCandidates(ctx, time.Now().UTC(), 10)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{s.ID}, ids)
	ids, err = postgres.NewSessionRepo(p.pool).ListAbandonCandidates(ctx, time.Now().UTC().Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, ids)
}
