//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type purgeEnv struct {
	*retEnv
	purge *postgres.PurgeRepo
}

func newPurgeEnv(t *testing.T) *purgeEnv {
	t.Helper()
	e := newRetEnv(t)
	return &purgeEnv{retEnv: e, purge: postgres.NewPurgeRepo(e.pool)}
}

// otherUser returns an env whose rows belong to a different user of the same database.
func (e *purgeEnv) otherUser(t *testing.T) *retEnv {
	t.Helper()
	o := *e.retEnv
	o.user = testutil.SeedUser(t, e.pool, nil)
	return &o
}

func (e *purgeEnv) deleteUser(t *testing.T, deletedAt time.Time) uuid.UUID {
	t.Helper()
	return testutil.SeedUser(t, e.pool, testutil.Row{"status": "DELETED", "deleted_at": deletedAt})
}

func (e *purgeEnv) memory(t *testing.T, user uuid.UUID, key string) uuid.UUID {
	t.Helper()
	return seedMemory(t, e.pool, user, key, "ACTIVE", time.Now().UTC().Add(time.Hour))
}

func (e *purgeEnv) learner(t *testing.T, user, evaluation, session uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	testutil.MustInsert(ctx, t, e.pool, "learner_profiles", testutil.Row{"user_id": user, "nickname": "Budi"})
	testutil.MustInsert(ctx, t, e.pool, "learner_skill_states", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": user, "skill": "GRAMMAR", "prior_level": 1.0, "prior_confidence": 0.3,
		"estimated_level": 1.5, "confidence": 0.4, "evidence_count": 1, "last_evaluation_id": evaluation, "last_evidence_at": time.Now().UTC(),
	})
	testutil.MustInsert(ctx, t, e.pool, "proficiency_estimates", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": user, "skill": "GRAMMAR", "source_evaluation_id": evaluation, "session_id": session,
		"algorithm_version": 1, "raw_score": 71, "evidence_level": 1.5, "evidence_confidence": 0.5, "previous_level": 1.0,
		"estimated_level": 1.5, "confidence": 0.4, "evidence_count": 1, "trend": "STABLE", "assessed_at": time.Now().UTC(),
	})
}

func TestPurgeRepo_ListCandidates(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	newest := e.deleteUser(t, now.Add(-2*time.Hour))
	oldest := e.deleteUser(t, now.Add(-30*time.Hour))
	middle := e.deleteUser(t, now.Add(-5*time.Hour))
	e.deleteUser(t, now.Add(-30*time.Minute)) // inside the delay
	e.deleteUser(t, cutoff)                   // deleted_at == cutoff: not before it
	active := testutil.SeedUser(t, e.pool, nil)
	suspended := testutil.SeedUser(t, e.pool, testutil.Row{"status": "SUSPENDED"})
	purged := e.deleteUser(t, now.Add(-50*time.Hour))
	_, err := e.pool.Exec(ctx, `UPDATE users SET purged_at = $2 WHERE id = $1`, purged, now.Add(-time.Hour))
	require.NoError(t, err)

	got, err := e.purge.ListCandidates(ctx, cutoff, 50)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{oldest, middle, newest}, got, "DELETED, not purged, deleted before the cutoff, oldest deleted_at first")
	assert.NotContains(t, got, active)
	assert.NotContains(t, got, suspended)

	got, err = e.purge.ListCandidates(ctx, cutoff, 2)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{oldest, middle}, got, "limit applies after ordering")
}

func TestPurgeRepo_DeleteMemories_OnlyThatUser(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	other := e.otherUser(t)
	mine1, mine2 := e.memory(t, e.user, "k_1"), seedMemory(t, e.pool, e.user, "k_2", "DELETED", time.Now().UTC().Add(time.Hour))
	theirs := e.memory(t, other.user, "k_1")

	require.NoError(t, e.purge.DeleteMemories(ctx, e.user))
	assert.False(t, memoryExists(t, e.pool, mine1))
	assert.False(t, memoryExists(t, e.pool, mine2), "tombstones go too")
	assert.True(t, memoryExists(t, e.pool, theirs))
	require.NoError(t, e.purge.DeleteMemories(ctx, e.user), "idempotent")
}

func TestPurgeRepo_DeleteEvaluations_FindingsFirst_LearnerLinksFollowForeignKeys(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	other := e.otherUser(t)
	ev := e.evaluation(t, now.Add(-time.Hour), 3)
	otherEv := other.evaluation(t, now.Add(-time.Hour), 2)
	var sid uuid.UUID
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT session_id FROM evaluations WHERE id = $1`, ev).Scan(&sid))
	e.learner(t, e.user, ev, sid)

	require.NoError(t, e.purge.DeleteEvaluations(ctx, e.user))
	assert.Zero(t, e.count(t, `SELECT count(*) FROM evaluations WHERE user_id = $1`, e.user))
	assert.Zero(t, e.count(t, `SELECT count(*) FROM evaluation_findings WHERE user_id = $1`, e.user))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM evaluations WHERE id = $1`, otherEv))
	assert.Equal(t, 2, e.count(t, `SELECT count(*) FROM evaluation_findings WHERE evaluation_id = $1`, otherEv))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM learner_skill_states WHERE user_id = $1 AND last_evaluation_id IS NULL`, e.user),
		"last_evaluation_id is set NULL")
	assert.Zero(t, e.count(t, `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, e.user), "estimates cascade with their evaluation")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, e.user), "sessions stay")
	require.NoError(t, e.purge.DeleteEvaluations(ctx, e.user), "idempotent")
}

func TestPurgeRepo_DeleteLearner(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	other := e.otherUser(t)
	ev := e.evaluation(t, now.Add(-time.Hour), 0)
	otherEv := other.evaluation(t, now.Add(-time.Hour), 0)
	var sid, otherSid uuid.UUID
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT session_id FROM evaluations WHERE id = $1`, ev).Scan(&sid))
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT session_id FROM evaluations WHERE id = $1`, otherEv).Scan(&otherSid))
	e.learner(t, e.user, ev, sid)
	e.learner(t, other.user, otherEv, otherSid)

	require.NoError(t, e.purge.DeleteLearner(ctx, e.user))
	for _, tbl := range []string{"proficiency_estimates", "learner_skill_states", "learner_profiles"} {
		assert.Zero(t, e.count(t, `SELECT count(*) FROM `+tbl+` WHERE user_id = $1`, e.user), tbl)
		assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM `+tbl+` WHERE user_id = $1`, other.user), tbl+" of another user")
	}
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM evaluations WHERE id = $1`, ev), "evaluations are a separate step")
	require.NoError(t, e.purge.DeleteLearner(ctx, e.user), "idempotent")
}

func TestPurgeRepo_DeleteTranscripts_BatchesAcrossSessions_KeepsSessions(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	other := e.otherUser(t)
	e.ensureAround(t, now, now.Add(-45*day))
	s1 := e.endedSession(t, now.Add(-time.Hour), nil)
	s2 := e.endedSession(t, now.Add(-50*day), nil)
	so := other.endedSession(t, now.Add(-time.Hour), nil)
	for i := 1; i <= 5; i++ {
		insertTurnAt(t, e.pool, s1, i, now.Add(-time.Duration(i)*time.Minute-time.Hour))
	}
	for i := 1; i <= 3; i++ {
		insertTurnAt(t, e.pool, s2, i, now.Add(-45*day).Add(time.Duration(i)*time.Minute))
	}
	insertTurnAt(t, e.pool, so, 1, now.Add(-time.Hour))
	for i := 0; i < 4; i++ {
		insertEventAt(t, e.pool, s1, now.Add(-time.Duration(i+1)*time.Minute))
	}
	insertEventAt(t, e.pool, s2, now.Add(-45*day))
	insertEventAt(t, e.pool, so, now.Add(-time.Minute))

	require.NoError(t, e.purge.DeleteTranscripts(ctx, e.user, 2), "batch 2 forces several rounds per table")
	ids := []uuid.UUID{s1, s2}
	assert.Zero(t, e.count(t, `SELECT count(*) FROM session_turns WHERE session_id = ANY($1)`, ids))
	assert.Zero(t, e.count(t, `SELECT count(*) FROM session_events WHERE session_id = ANY($1)`, ids))
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM session_turns WHERE session_id = $1`, so), "another user's transcript stays")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM session_events WHERE session_id = $1`, so))
	assert.Equal(t, 2, e.count(t, `SELECT count(*) FROM sessions WHERE id = ANY($1)`, ids), "sessions rows are kept")
	require.NoError(t, e.purge.DeleteTranscripts(ctx, e.user, 2), "idempotent")
}

func TestPurgeRepo_DeleteTranscripts_NoSessions(t *testing.T) {
	e := newPurgeEnv(t)
	require.NoError(t, e.purge.DeleteTranscripts(context.Background(), e.user, 1000))
}

func TestPurgeRepo_ClearSummaries(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	other := e.otherUser(t)
	mine := e.endedSession(t, now.Add(-time.Hour), nil)
	theirs := other.endedSession(t, now.Add(-time.Hour), nil)

	require.NoError(t, e.purge.ClearSummaries(ctx, e.user))
	var s *string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT summary_text FROM sessions WHERE id = $1`, mine).Scan(&s))
	assert.Nil(t, s)
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT summary_text FROM sessions WHERE id = $1`, theirs).Scan(&s))
	require.NotNil(t, s)
	require.NoError(t, e.purge.ClearSummaries(ctx, e.user), "idempotent")
}

func TestPurgeRepo_Anonymize(t *testing.T) {
	e := newPurgeEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	uid := testutil.SeedUser(t, e.pool, testutil.Row{
		"status": "DELETED", "deleted_at": now.Add(-2 * time.Hour), "phone_number": "+628123456789", "full_name": "Budi Santoso",
	})
	other := testutil.SeedUser(t, e.pool, nil)

	require.NoError(t, e.purge.Anonymize(ctx, uid, now))
	var email, name, hash string
	var phone *string
	var purged *time.Time
	row := func(id uuid.UUID) {
		require.NoError(t, e.pool.QueryRow(ctx, `SELECT email, phone_number, full_name, password_hash, purged_at FROM users WHERE id = $1`, id).
			Scan(&email, &phone, &name, &hash, &purged))
	}
	row(uid)
	assert.Equal(t, "deleted+"+uid.String()+"@deleted.invalid", email)
	assert.Nil(t, phone)
	assert.Equal(t, "deleted", name)
	assert.Equal(t, "!", hash)
	require.NotNil(t, purged)
	assert.WithinDuration(t, now, *purged, time.Second)

	// A second call is a no-op and does not move purged_at.
	require.NoError(t, e.purge.Anonymize(ctx, uid, now.Add(time.Hour)))
	row(uid)
	assert.WithinDuration(t, now, *purged, time.Second)

	// A user that is not DELETED must never be anonymized.
	require.Error(t, e.purge.Anonymize(ctx, other, now))
	row(other)
	assert.NotEqual(t, "deleted", name)
	assert.Nil(t, purged)

	require.Error(t, e.purge.Anonymize(ctx, uuid.Must(uuid.NewV7()), now), "unknown user")
}
