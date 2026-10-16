//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestMemorySource_Queries(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPostgres(t)
	src := postgres.NewMemorySource(pool, clock.System{})
	fs := testutil.SeedFinishedSession(t, pool, testutil.FinishedSessionOpts{
		ActiveSeconds: 120,
		Turns: []testutil.TurnSeed{
			{UserText: "一番目の発話です。", AssistantText: "はい。", UserSeconds: 5},
			{UserText: "二番目は中断されました。", AssistantText: "途中で", Status: "INTERRUPTED", UserSeconds: 5},
			{UserText: "三番目は失敗しました。", AssistantText: "x", Status: "FAILED", UserSeconds: 5},
			{UserText: "四番目の発話です。", AssistantText: "いいですね。", UserSeconds: 5},
		},
	})

	info, err := src.Session(ctx, fs.ID)
	require.NoError(t, err)
	require.Equal(t, fs.UserID, info.UserID, "the session owner")
	require.False(t, info.EndedAt.IsZero())
	require.NotEmpty(t, info.ScenarioName)
	_, err = src.Session(ctx, uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, memory.ErrNotFound)

	turns, err := src.Turns(ctx, fs.ID)
	require.NoError(t, err)
	require.Len(t, turns, 3, "COMPLETED and INTERRUPTED only")
	for i, want := range []struct {
		seq    int
		status string
	}{{1, "COMPLETED"}, {2, "INTERRUPTED"}, {4, "COMPLETED"}} {
		require.Equal(t, want.seq, turns[i].Seq, "ordered by sequence_number")
		require.Equal(t, want.status, turns[i].Status)
	}
	require.NotNil(t, turns[0].UserText)
	require.Equal(t, "一番目の発話です。", *turns[0].UserText)
	require.Equal(t, fs.TurnIDs[0], turns[0].ID)

	deleted, err := src.UserDeleted(ctx, fs.UserID)
	require.NoError(t, err)
	require.False(t, deleted)
	gone := testutil.SeedUser(t, pool, testutil.Row{"status": "DELETED", "deleted_at": time.Now().UTC()})
	deleted, err = src.UserDeleted(ctx, gone)
	require.NoError(t, err)
	require.True(t, deleted)
	deleted, err = src.UserDeleted(ctx, uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	require.True(t, deleted, "a user without a row counts as deleted")

	// Existing: non-episodic, own, not expired, newest observation first, at most limit
	other := testutil.SeedUser(t, pool, nil)
	now := time.Now().UTC()
	older := seedMemory(t, pool, fs.UserID, "older", "ACTIVE", now.Add(24*time.Hour))
	newer := seedMemory(t, pool, fs.UserID, "newer", "ACTIVE", now.Add(24*time.Hour))
	expired := seedMemory(t, pool, fs.UserID, "expired", "ACTIVE", now.Add(-time.Hour))
	seedMemory(t, pool, other, "foreign", "ACTIVE", now.Add(24*time.Hour))
	seedMemory(t, pool, fs.UserID, "superseded", "SUPERSEDED", now.Add(24*time.Hour))
	for id, ago := range map[uuid.UUID]time.Duration{older: 3 * time.Hour, newer: time.Minute, expired: 0} {
		_, err := pool.Exec(ctx, `UPDATE memories SET first_observed_at = $2, last_observed_at = $2 WHERE id = $1`, id, now.Add(-ago))
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO memories (id, user_id, category, memory_key, content, display_text, evidence_type, confidence, importance,
		first_observed_at, last_observed_at, expires_at) VALUES ($1, $2, 'EPISODIC', 'episode:x', '旅行の話', 'cerita', 'EXPLICIT_USER_STATEMENT', 0.9, 0.5, $3, $3, $4)`,
		uuid.Must(uuid.NewV7()), fs.UserID, now, now.Add(24*time.Hour))
	require.NoError(t, err)

	lines, err := src.Existing(ctx, fs.UserID, 150)
	require.NoError(t, err)
	require.Equal(t, []string{"newer", "older"}, []string{lines[0].Key, lines[1].Key}, "newest observation first")
	require.Len(t, lines, 2, "no episodic, foreign, expired or superseded memory")
	lines, err = src.Existing(ctx, fs.UserID, 1)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	require.Equal(t, "newer", lines[0].Key)
}

func TestMemoryPromptSource_Published(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPostgres(t)
	src := postgres.NewMemoryPromptSource(pool)
	_, _, err := src.Published(ctx, "MEMORY_EXTRACTION")
	require.ErrorIs(t, err, memory.ErrPromptNotFound, "no seed yet")

	testutil.ApplySeed(t, pool)
	tpl, id, err := src.Published(ctx, "MEMORY_EXTRACTION")
	require.NoError(t, err)
	require.Equal(t, "01920000-000d-7000-8000-000000000005", id.String())
	require.NotEmpty(t, tpl.OutputSchema)
	require.NotEmpty(t, tpl.System)
}

func TestMemoryProfileSettings_CreatesProfile(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPostgres(t)
	user := testutil.SeedUser(t, pool, nil)
	st := postgres.NewMemoryProfileSettings(pool, idgen.NewV7(), clock.System{})
	on, err := st.EnsureMemoryEnabled(ctx, user)
	require.NoError(t, err)
	require.True(t, on, "memory_enabled defaults to true and the profile exists afterwards")
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM learner_profiles WHERE user_id = $1`, user).Scan(&n))
	require.Equal(t, 1, n)
}
