//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func seedMemory(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, key, status string, expires time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	row := testutil.Row{
		"id": id, "user_id": user, "category": "PREFERENCE", "memory_key": key, "content": "コーヒーが好き", "display_text": "Suka kopi",
		"evidence_type": "EXPLICIT_USER_STATEMENT", "confidence": 0.9, "importance": 0.5, "status": status,
		"first_observed_at": now.Add(-time.Hour), "last_observed_at": now.Add(-time.Hour), "expires_at": expires,
	}
	if status == "DELETED" {
		row["content"], row["display_text"], row["evidence_type"] = "", "", "USER_DELETED"
	}
	testutil.MustInsert(context.Background(), t, pool, "memories", row)
	return id
}

func memoryExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM memories WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func TestDeleteExpiredMemories_HardDeletesExpiredOnly(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewRetentionRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	ua := testutil.SeedUser(t, pool, nil)
	ub := testutil.SeedUser(t, pool, nil)
	deletedUser := testutil.SeedUser(t, pool, testutil.Row{"status": "DELETED", "deleted_at": now.Add(-time.Minute)})

	expiredActive := seedMemory(t, pool, ua, "pref_a", "ACTIVE", now.Add(-time.Hour))
	expiredSuperseded := seedMemory(t, pool, ub, "pref_b", "SUPERSEDED", now.Add(-2*time.Hour))
	expiredTombstone := seedMemory(t, pool, ub, "pref_c", "DELETED", now.Add(-3*time.Hour))
	future := seedMemory(t, pool, ua, "pref_d", "ACTIVE", now.Add(time.Hour))
	futureTombstone := seedMemory(t, pool, ua, "pref_e", "DELETED", now.Add(24*time.Hour))
	// Memory of a DELETED user is account_purge's business: not expired, so it stays.
	ofDeleted := seedMemory(t, pool, deletedUser, "pref_f", "ACTIVE", now.Add(time.Hour))

	users, err := repo.DeleteExpiredMemories(ctx, 1000)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{ua, ub, ub}, users, "one user_id per deleted row")

	assert.False(t, memoryExists(t, pool, expiredActive))
	assert.False(t, memoryExists(t, pool, expiredSuperseded))
	assert.False(t, memoryExists(t, pool, expiredTombstone))
	assert.True(t, memoryExists(t, pool, future))
	assert.True(t, memoryExists(t, pool, futureTombstone))
	assert.True(t, memoryExists(t, pool, ofDeleted))

	users, err = repo.DeleteExpiredMemories(ctx, 1000)
	require.NoError(t, err)
	assert.Empty(t, users, "second run deletes nothing")
}

func TestDeleteExpiredMemories_LimitAndOrder(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewRetentionRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	u := testutil.SeedUser(t, pool, nil)
	oldest := seedMemory(t, pool, u, "k_1", "ACTIVE", now.Add(-5*time.Hour))
	mid := seedMemory(t, pool, u, "k_2", "ACTIVE", now.Add(-4*time.Hour))
	newest := seedMemory(t, pool, u, "k_3", "ACTIVE", now.Add(-3*time.Hour))

	users, err := repo.DeleteExpiredMemories(ctx, 2)
	require.NoError(t, err)
	assert.Len(t, users, 2, "limit caps the batch")
	assert.False(t, memoryExists(t, pool, oldest), "ordered by expires_at: oldest first")
	assert.False(t, memoryExists(t, pool, mid))
	assert.True(t, memoryExists(t, pool, newest))
}

func TestDeleteExpiredMemories_SupersededByReferenceCleared(t *testing.T) {
	pool := testutil.NewPostgres(t)
	repo := postgres.NewRetentionRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	u := testutil.SeedUser(t, pool, nil)
	successor := seedMemory(t, pool, u, "k_new", "ACTIVE", now.Add(time.Hour))
	old := seedMemory(t, pool, u, "k_old", "SUPERSEDED", now.Add(time.Hour))
	_, err := pool.Exec(ctx, `UPDATE memories SET superseded_by = $1 WHERE id = $2`, old, successor)
	require.Error(t, err, "only SUPERSEDED rows may carry superseded_by (sanity: chk_memories_superseded_by)")
	_, err = pool.Exec(ctx, `UPDATE memories SET superseded_by = $1 WHERE id = $2`, successor, old)
	require.NoError(t, err)
	// the successor expires while the superseded row is still alive
	_, err = pool.Exec(ctx, `UPDATE memories SET expires_at = $1 WHERE id = $2`, now.Add(-time.Hour), successor)
	require.NoError(t, err)

	_, err = repo.DeleteExpiredMemories(ctx, 1000)
	require.NoError(t, err)
	assert.False(t, memoryExists(t, pool, successor))
	var ref *uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT superseded_by FROM memories WHERE id = $1`, old).Scan(&ref))
	assert.Nil(t, ref, "FK ON DELETE SET NULL")
}
