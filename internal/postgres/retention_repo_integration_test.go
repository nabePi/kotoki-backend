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

const day = 24 * time.Hour

type retEnv struct {
	pool    *pgxpool.Pool
	repo    *postgres.RetentionRepo
	parts   *postgres.PartitionRepo
	user    uuid.UUID
	content testutil.ContentIDs
}

func newRetEnv(t *testing.T) *retEnv {
	t.Helper()
	pool := testutil.NewPostgres(t)
	return &retEnv{
		pool: pool, repo: postgres.NewRetentionRepo(pool), parts: postgres.NewPartitionRepo(pool),
		user: testutil.SeedUser(t, pool, nil), content: testutil.SeedContent(t, pool),
	}
}

// ensureAround creates the partitions of both tables for the months of the given instants.
func (e *retEnv) ensureAround(t *testing.T, ts ...time.Time) {
	t.Helper()
	have := map[string]bool{}
	for _, table := range []string{"session_turns", "session_events"} {
		infos, err := e.parts.List(context.Background(), table)
		require.NoError(t, err)
		for _, p := range infos {
			have[p.Name] = true
		}
	}
	for _, at := range ts {
		for _, table := range []string{"session_turns", "session_events"} {
			if !have[table+"_p"+at.UTC().Format("200601")] {
				ensureMonth(t, e.parts, table, at)
				have[table+"_p"+at.UTC().Format("200601")] = true
			}
		}
	}
}

// endedSession seeds a COMPLETED session that ended at endedAt (started 5 minutes earlier).
func (e *retEnv) endedSession(t *testing.T, endedAt time.Time, over testutil.Row) uuid.UUID {
	t.Helper()
	started := endedAt.Add(-5 * time.Minute)
	row := testutil.Row{
		"status": "COMPLETED", "end_reason": "USER_ENDED", "duration_seconds": 300,
		"starting_at": started.Add(-time.Second), "started_at": started, "expires_at": started.Add(300 * time.Second),
		"ending_at": endedAt, "ended_at": endedAt, "summary_text": "ホテルの予約を練習しました。",
		"created_at": started.Add(-time.Minute),
	}
	for k, v := range over {
		row[k] = v
	}
	return testutil.SeedSession(t, e.pool, e.user, e.content, row)
}

func (e *retEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (e *retEnv) turnExists(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM session_turns WHERE id = $1`, id) == 1
}

func TestRetention_Turns_NotDeletedBefore180DaysAfterEnded(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cutoff := now.Add(-180*day - 2*time.Hour) // what the job passes: T-180d-2h
	e.ensureAround(t, now.Add(-180*day-3*time.Hour), now.Add(-180*day-time.Hour), now.Add(-180*day-2*time.Hour))
	sid := e.endedSession(t, now.Add(-179*day), nil)
	inMargin := insertTurnAt(t, e.pool, sid, 1, now.Add(-180*day-time.Hour)) // 1 h older than 180 d: inside the margin
	pastMargin := insertTurnAt(t, e.pool, sid, 2, now.Add(-180*day-3*time.Hour))
	recent := insertTurnAt(t, e.pool, sid, 3, now.Add(-time.Hour))

	n, err := e.repo.DeleteTurnsBefore(ctx, cutoff, 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, e.turnExists(t, inMargin), "created 180d1h ago: not deleted, the 2 hour margin keeps it")
	assert.False(t, e.turnExists(t, pastMargin), "created 180d3h ago: deleted")
	assert.True(t, e.turnExists(t, recent))

	n, err = e.repo.DeleteTurnsBefore(ctx, cutoff, 5000)
	require.NoError(t, err)
	assert.Zero(t, n, "second run: nothing left to delete")
}

func TestRetention_PartitionBoundaryRowsDeletedPrecisely(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	e.ensureAround(t, monthUTC(2024, time.February), monthUTC(2024, time.March))
	sid := e.endedSession(t, time.Now().UTC(), nil)
	cutoff := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC) // inside the March partition
	prevMonth := insertTurnAt(t, e.pool, sid, 1, time.Date(2024, 2, 29, 23, 0, 0, 0, time.UTC))
	justBefore := insertTurnAt(t, e.pool, sid, 2, cutoff.Add(-time.Second))
	atCutoff := insertTurnAt(t, e.pool, sid, 3, cutoff)
	after := insertTurnAt(t, e.pool, sid, 4, cutoff.Add(time.Second))

	n, err := e.repo.DeleteTurnsBefore(ctx, cutoff, 5000)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.False(t, e.turnExists(t, prevMonth))
	assert.False(t, e.turnExists(t, justBefore), "in the boundary partition, older than the cutoff: deleted")
	assert.True(t, e.turnExists(t, atCutoff), "created_at == cutoff is not older than it")
	assert.True(t, e.turnExists(t, after))
}

func TestRetention_Turns_BatchLimit(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	e.ensureAround(t, monthUTC(2024, time.March))
	sid := e.endedSession(t, time.Now().UTC(), nil)
	for i := 1; i <= 5; i++ {
		insertTurnAt(t, e.pool, sid, i, time.Date(2024, 3, 1, 0, i, 0, 0, time.UTC))
	}
	cutoff := time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)
	n, err := e.repo.DeleteTurnsBefore(ctx, cutoff, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 3, e.count(t, `SELECT count(*) FROM session_turns`))
	n, _ = e.repo.DeleteTurnsBefore(ctx, cutoff, 2)
	assert.Equal(t, 2, n)
	n, _ = e.repo.DeleteTurnsBefore(ctx, cutoff, 2)
	assert.Equal(t, 1, n)
}

func TestRetention_Events_365d(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	cutoff := now.Add(-365 * day)
	e.ensureAround(t, cutoff.Add(-time.Hour), cutoff.Add(time.Hour), now.Add(-180*day))
	sid := e.endedSession(t, now.Add(-300*day), nil)
	insertEventAt(t, e.pool, sid, cutoff.Add(-time.Hour)) // 365d1h: deleted
	insertEventAt(t, e.pool, sid, cutoff.Add(time.Hour))  // 364d23h: kept
	insertEventAt(t, e.pool, sid, now.Add(-180*day))      // kept
	insertEventAt(t, e.pool, sid, cutoff)                 // created_at == cutoff is not older than it: kept

	n, err := e.repo.DeleteEventsBefore(ctx, cutoff, 5000)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 3, e.count(t, `SELECT count(*) FROM session_events WHERE session_id = $1`, sid))
	n, _ = e.repo.DeleteEventsBefore(ctx, cutoff, 5000)
	assert.Zero(t, n)
}

func TestRetention_SessionSummary_Nulled_AndPurgedAtSet(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	before := now.Add(-180 * day)
	old := e.endedSession(t, before.Add(-time.Hour), nil)
	fresh := e.endedSession(t, before.Add(24*time.Hour), nil) // 179 days ago
	cancelled := testutil.SeedSession(t, e.pool, e.user, e.content, testutil.Row{
		"status": "CANCELLED", "end_reason": "USER_CANCELLED", "ended_at": before.Add(-48 * time.Hour), "charged_seconds": 0,
		"created_at": before.Add(-49 * time.Hour),
	})
	_ = cancelled

	n, err := e.repo.PurgeSessionSummaries(ctx, before, 1000)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "every session that ended more than 180 days ago, whatever its status")

	var summary *string
	var purged *time.Time
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT summary_text, transcript_purged_at FROM sessions WHERE id = $1`, old).Scan(&summary, &purged))
	assert.Nil(t, summary)
	require.NotNil(t, purged)
	assert.WithinDuration(t, now, *purged, time.Minute)

	require.NoError(t, e.pool.QueryRow(ctx, `SELECT summary_text, transcript_purged_at FROM sessions WHERE id = $1`, fresh).Scan(&summary, &purged))
	require.NotNil(t, summary, "ended 179 days ago: untouched")
	assert.Equal(t, "ホテルの予約を練習しました。", *summary)
	assert.Nil(t, purged)

	n, err = e.repo.PurgeSessionSummaries(ctx, before, 1000)
	require.NoError(t, err)
	assert.Zero(t, n, "second run: already purged")
}

func TestRetention_SessionSummary_BatchLimit(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	before := time.Now().UTC().Add(-180 * day)
	for i := 0; i < 3; i++ {
		e.endedSession(t, before.Add(-time.Duration(i+1)*time.Hour), nil)
	}
	n, err := e.repo.PurgeSessionSummaries(ctx, before, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, _ = e.repo.PurgeSessionSummaries(ctx, before, 2)
	assert.Equal(t, 1, n)
}

func (e *retEnv) evaluation(t *testing.T, completedAt time.Time, findings int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	sid := e.endedSession(t, completedAt.Add(-time.Minute), nil)
	id := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, e.pool, "evaluations", testutil.Row{
		"id": id, "session_id": sid, "user_id": e.user, "status": "COMPLETED",
		"grammar_score": 71, "vocabulary_score": 62, "fluency_score": 80, "naturalness_score": 55, "task_achievement_score": 90,
		"overall_score": 71.60, "listening_response_signal": 66, "interaction_complexity_signal": 44,
		"dimension_confidence": []byte(`{"GRAMMAR":0.8}`), "dimension_comments": []byte(`{"GRAMMAR":"Partikel dengan benar."}`),
		"summary_feedback": "Bagus sekali.", "strengths": []byte(`["Ramah"]`), "focus_areas": []byte(`["Partikel"]`),
		"recommendations": []byte(`[{"title":"Latih","detail":"x","example_ja":"y"}]`),
		"rubric_version":  "v1", "source_event_id": uuid.Must(uuid.NewV7()), "completed_at": completedAt,
	})
	for i := 1; i <= findings; i++ {
		testutil.MustInsert(ctx, t, e.pool, "evaluation_findings", testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "evaluation_id": id, "session_id": sid, "user_id": e.user, "ordinal": i,
			"dimension": "GRAMMAR", "finding_type": "PARTICLE_ERROR", "severity": "MINOR", "original_text": "学校に勉強します",
			"corrected_text": "学校で勉強します", "explanation": "Gunakan で.", "confidence": 0.9, "source": "LLM",
		})
	}
	return id
}

func TestRetention_EvaluationDetails_FindingsDeletedScoresKept(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	before := time.Now().UTC().Add(-180 * day)
	old := e.evaluation(t, before.Add(-time.Hour), 3)
	fresh := e.evaluation(t, before.Add(time.Hour), 2)

	n, err := e.repo.PurgeEvaluationDetails(ctx, before, 500)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	var (
		grammar, vocab, fluency, natural, task int
		overall                                string
		listening, interaction                 int
		fb                                     *string
		comments, strengths, focus, recs       string
		purged                                 *time.Time
	)
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT grammar_score, vocabulary_score, fluency_score, naturalness_score,
task_achievement_score, overall_score::text, listening_response_signal, interaction_complexity_signal,
summary_feedback, dimension_comments::text, strengths::text, focus_areas::text, recommendations::text, details_purged_at
FROM evaluations WHERE id = $1`, old).Scan(&grammar, &vocab, &fluency, &natural, &task, &overall, &listening, &interaction,
		&fb, &comments, &strengths, &focus, &recs, &purged))
	assert.Equal(t, []int{71, 62, 80, 55, 90, 66, 44}, []int{grammar, vocab, fluency, natural, task, listening, interaction}, "scores and signals stay")
	assert.Equal(t, "71.60", overall)
	assert.Nil(t, fb)
	assert.Equal(t, "{}", comments)
	assert.Equal(t, "[]", strengths)
	assert.Equal(t, "[]", focus)
	assert.Equal(t, "[]", recs)
	require.NotNil(t, purged)
	assert.Zero(t, e.count(t, `SELECT count(*) FROM evaluation_findings WHERE evaluation_id = $1`, old))

	// The recent evaluation is untouched.
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT summary_feedback, details_purged_at FROM evaluations WHERE id = $1`, fresh).Scan(&fb, &purged))
	require.NotNil(t, fb)
	assert.Nil(t, purged)
	assert.Equal(t, 2, e.count(t, `SELECT count(*) FROM evaluation_findings WHERE evaluation_id = $1`, fresh))

	n, err = e.repo.PurgeEvaluationDetails(ctx, before, 500)
	require.NoError(t, err)
	assert.Zero(t, n, "second run: 0 rows")
}

func TestRetention_EvaluationDetails_BatchLimit(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	before := time.Now().UTC().Add(-180 * day)
	for i := 1; i <= 3; i++ {
		e.evaluation(t, before.Add(-time.Duration(i)*time.Hour), 1)
	}
	n, err := e.repo.PurgeEvaluationDetails(ctx, before, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM evaluation_findings`), "only the third evaluation keeps its finding")
	n, _ = e.repo.PurgeEvaluationDetails(ctx, before, 2)
	assert.Equal(t, 1, n)
}

func (e *retEnv) refreshToken(t *testing.T, family, id uuid.UUID, parent *uuid.UUID, expires time.Time, revoked *time.Time) {
	t.Helper()
	hash := make([]byte, 32)
	copy(hash, id[:])
	row := testutil.Row{
		"id": id, "user_id": e.user, "family_id": family, "token_hash": hash,
		"created_at": expires.Add(-30 * day), "expires_at": expires,
	}
	if parent != nil {
		row["parent_id"] = *parent
	}
	if revoked != nil {
		row["revoked_at"] = *revoked
		row["revoke_reason"] = "ROTATED"
	}
	testutil.MustInsert(context.Background(), t, e.pool, "refresh_tokens", row)
}

func (e *retEnv) familyExists(t *testing.T, family uuid.UUID) bool {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM refresh_tokens WHERE family_id = $1`, family) > 0
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func TestRetention_Tokens(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	ret := 7 * day
	ptr := func(v time.Time) *time.Time { return &v }

	// (a) every token expired 8 days ago, a rotation chain: parent FKs inside the family
	famOld := newID()
	a1, a2 := famOld, newID()
	e.refreshToken(t, famOld, a1, nil, now.Add(-9*day), ptr(now.Add(-9*day-time.Hour)))
	e.refreshToken(t, famOld, a2, &a1, now.Add(-8*day), nil)
	// (b) one token still valid
	famLive := newID()
	b1, b2 := famLive, newID()
	e.refreshToken(t, famLive, b1, nil, now.Add(-20*day), ptr(now.Add(-19*day)))
	e.refreshToken(t, famLive, b2, &b1, now.Add(10*day), nil)
	// (c) expired only 6 days ago: not past the 7 day retention
	famRecent := newID()
	e.refreshToken(t, famRecent, famRecent, nil, now.Add(-6*day), nil)
	// (d) revoked 8 days ago although the expiry is in the future
	famRevoked := newID()
	e.refreshToken(t, famRevoked, famRevoked, nil, now.Add(20*day), ptr(now.Add(-8*day)))
	// (e) expired 8 days ago, revoked 3 days ago: LEAST(expires, revoked) is the expiry, 8 days ago
	famMixed := newID()
	e.refreshToken(t, famMixed, famMixed, nil, now.Add(-8*day), ptr(now.Add(-3*day)))
	// (f) revoked 3 days ago, expiry in the future: kept
	famRevoked3 := newID()
	e.refreshToken(t, famRevoked3, famRevoked3, nil, now.Add(20*day), ptr(now.Add(-3*day)))

	resetOld, resetRecent := newID(), newID()
	hash := func(id uuid.UUID) []byte { h := make([]byte, 32); copy(h, id[:]); return h }
	for id, created := range map[uuid.UUID]time.Time{resetOld: now.Add(-25 * time.Hour), resetRecent: now.Add(-23 * time.Hour)} {
		testutil.MustInsert(ctx, t, e.pool, "password_reset_tokens", testutil.Row{
			"id": id, "user_id": e.user, "token_hash": hash(id), "created_at": created, "expires_at": created.Add(15 * time.Minute),
		})
	}

	n, err := e.repo.DeleteExpiredTokens(ctx, now, ret, 1000)
	require.NoError(t, err)
	assert.Equal(t, 2+1+1+1, n, "famOld (2 rows), famRevoked, famMixed, resetOld")
	assert.False(t, e.familyExists(t, famOld), "(a) all tokens expired > 7 days ago")
	assert.True(t, e.familyExists(t, famLive), "(b) an active token keeps the whole family")
	assert.Equal(t, 2, e.count(t, `SELECT count(*) FROM refresh_tokens WHERE family_id = $1`, famLive))
	assert.True(t, e.familyExists(t, famRecent), "(c) expired only 6 days ago")
	assert.False(t, e.familyExists(t, famRevoked), "(d) revoked > 7 days ago")
	assert.False(t, e.familyExists(t, famMixed), "(e)")
	assert.True(t, e.familyExists(t, famRevoked3), "(f) revoked 3 days ago")
	assert.Zero(t, e.count(t, `SELECT count(*) FROM password_reset_tokens WHERE id = $1`, resetOld), "older than 24 hours")
	assert.Equal(t, 1, e.count(t, `SELECT count(*) FROM password_reset_tokens WHERE id = $1`, resetRecent), "23 hours old")

	n, err = e.repo.DeleteExpiredTokens(ctx, now, ret, 1000)
	require.NoError(t, err)
	assert.Zero(t, n, "second run: nothing left")

	// A longer retention keeps what a shorter one deleted: the window is the argument, not 7 days.
	famX := newID()
	e.refreshToken(t, famX, famX, nil, now.Add(-8*day), nil)
	n, err = e.repo.DeleteExpiredTokens(ctx, now, 30*day, 1000)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, e.familyExists(t, famX))
}

func TestRetention_Tokens_BatchLimit(t *testing.T) {
	e := newRetEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		id := newID()
		e.refreshToken(t, id, id, nil, now.Add(-10*day), nil)
	}
	n, err := e.repo.DeleteExpiredTokens(ctx, now, 7*day, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, _ = e.repo.DeleteExpiredTokens(ctx, now, 7*day, 2)
	assert.Equal(t, 1, n)
}
