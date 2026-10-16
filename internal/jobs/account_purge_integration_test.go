//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/sessionenv"
)

var seedCounter atomic.Int64

const (
	purgeName   = "Budi Santoso"
	purgeNeedle = "Budi"
)

// purgeFixture is a user with data in every table the purge has to clear, one finished and billed
// session (so ledger, reservation and account rows exist), and the user already DELETED.
type purgeFixture struct {
	user, session, evaluation uuid.UUID
	email                     string
}

func seedPurgeUser(t *testing.T, e *sessionenv.Env) purgeFixture {
	t.Helper()
	ctx := context.Background()
	sid := finishSession(t, e)
	var uid uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, sid).Scan(&uid))
	email := "budi." + uid.String()[24:] + "@example.test"
	phone := fmt.Sprintf("+62812%08d", seedCounter.Add(1))
	_, err := e.Pool.Exec(ctx, `UPDATE users SET email = $2, full_name = $3, phone_number = $4 WHERE id = $1`, uid, email, purgeName, phone)
	require.NoError(t, err)
	_, err = e.Pool.Exec(ctx, `UPDATE sessions SET summary_text = $2 WHERE id = $1`, sid, purgeName+"さんがホテルの予約を練習しました。")
	require.NoError(t, err)

	now := time.Now().UTC()
	parts := jobs.NewPgPartitionStore(postgres.NewPartitionRepo(e.Pool))
	require.NoError(t, parts.Ensure(ctx, "session_turns", now))
	require.NoError(t, parts.Ensure(ctx, "session_events", now))
	for i := 1; i <= 3; i++ {
		testutil.MustInsert(ctx, t, e.Pool, "session_turns", testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": i, "generation": 1, "created_at": now, "user_text": purgeName + "です",
		})
	}
	testutil.MustInsert(ctx, t, e.Pool, "session_events", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "session_id": sid, "event_type": "BARGE_IN", "actor": "REALTIME", "created_at": now,
	})

	ev := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, e.Pool, "evaluations", testutil.Row{
		"id": ev, "session_id": sid, "user_id": uid, "status": "PENDING", "rubric_version": "v1", "source_event_id": uuid.Must(uuid.NewV7()),
		"summary_feedback": purgeName + " sudah bagus.",
	})
	testutil.MustInsert(ctx, t, e.Pool, "evaluation_findings", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "evaluation_id": ev, "session_id": sid, "user_id": uid, "ordinal": 1, "dimension": "GRAMMAR",
		"finding_type": "PARTICLE_ERROR", "severity": "MINOR", "original_text": purgeName, "explanation": "x", "confidence": 0.9, "source": "LLM",
	})
	testutil.MustInsert(ctx, t, e.Pool, "learner_profiles", testutil.Row{"user_id": uid, "nickname": purgeNeedle})
	testutil.MustInsert(ctx, t, e.Pool, "learner_skill_states", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": uid, "skill": "GRAMMAR", "prior_level": 1.0, "prior_confidence": 0.3, "estimated_level": 1.5,
		"confidence": 0.4, "evidence_count": 1, "last_evaluation_id": ev, "last_evidence_at": now,
	})
	testutil.MustInsert(ctx, t, e.Pool, "proficiency_estimates", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": uid, "skill": "GRAMMAR", "source_evaluation_id": ev, "session_id": sid, "algorithm_version": 1,
		"raw_score": 71, "evidence_level": 1.5, "evidence_confidence": 0.5, "previous_level": 1.0, "estimated_level": 1.5, "confidence": 0.4,
		"evidence_count": 1, "trend": "STABLE", "assessed_at": now,
	})
	for i := 1; i <= 2; i++ {
		testutil.MustInsert(ctx, t, e.Pool, "memories", testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "user_id": uid, "category": "PREFERENCE", "memory_key": fmt.Sprintf("k_%d", i), "content": purgeName + "は猫が好き",
			"display_text": purgeName + " suka kucing", "evidence_type": "EXPLICIT_USER_STATEMENT", "confidence": 0.9, "importance": 0.5,
			"first_observed_at": now, "last_observed_at": now, "expires_at": now.Add(24 * time.Hour),
		})
	}
	_, err = e.Pool.Exec(ctx, `UPDATE users SET status = 'DELETED', deleted_at = $2 WHERE id = $1`, uid, e.Clock.Now())
	require.NoError(t, err)
	return purgeFixture{user: uid, session: sid, evaluation: ev, email: email}
}

func purgeJobFor(e *sessionenv.Env, s jobs.PurgeStore) (*jobs.AccountPurgeJob, *jobs.Metrics, *bytes.Buffer) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	logs := &bytes.Buffer{}
	return jobs.NewAccountPurgeJob(s, time.Hour, m, e.Clock, slog.New(slog.NewJSONHandler(logs, nil))), m, logs
}

type userCounts struct{ memories, evaluations, findings, learner, estimates, skillStates, turns, events, summaries int }

func countUserData(t *testing.T, e *sessionenv.Env, f purgeFixture) userCounts {
	t.Helper()
	return userCounts{
		memories:    e.Count(t, `SELECT count(*) FROM memories WHERE user_id = $1`, f.user),
		evaluations: e.Count(t, `SELECT count(*) FROM evaluations WHERE user_id = $1`, f.user),
		findings:    e.Count(t, `SELECT count(*) FROM evaluation_findings WHERE user_id = $1`, f.user),
		learner:     e.Count(t, `SELECT count(*) FROM learner_profiles WHERE user_id = $1`, f.user),
		estimates:   e.Count(t, `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, f.user),
		skillStates: e.Count(t, `SELECT count(*) FROM learner_skill_states WHERE user_id = $1`, f.user),
		turns:       e.Count(t, `SELECT count(*) FROM session_turns WHERE session_id = $1`, f.session),
		events:      e.Count(t, `SELECT count(*) FROM session_events WHERE session_id = $1`, f.session),
		summaries:   e.Count(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND summary_text IS NOT NULL`, f.user),
	}
}

func isPurged(t *testing.T, e *sessionenv.Env, uid uuid.UUID) bool {
	t.Helper()
	return e.Count(t, `SELECT count(*) FROM users WHERE id = $1 AND purged_at IS NOT NULL`, uid) == 1
}

type financials struct{ accounts, reservations, ledger, sessions, processed, audit int }

func countFinancials(t *testing.T, e *sessionenv.Env, uid uuid.UUID) financials {
	t.Helper()
	return financials{
		accounts:     e.Count(t, `SELECT count(*) FROM balance_accounts WHERE user_id = $1`, uid),
		reservations: e.Count(t, `SELECT count(*) FROM balance_reservations WHERE user_id = $1`, uid),
		ledger:       e.Count(t, `SELECT count(*) FROM balance_ledger WHERE user_id = $1`, uid),
		sessions:     e.Count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, uid),
		processed:    e.Count(t, `SELECT count(*) FROM processed_events`),
		audit:        e.Count(t, `SELECT count(*) FROM admin_audit_logs`),
	}
}

func TestAccountPurge_RemovesDataKeepsFinancials(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	gone := seedPurgeUser(t, e)
	young := seedPurgeUser(t, e) // deleted_at is inside the delay: must stay untouched
	active := seedPurgeUser(t, e)
	_, err := e.Pool.Exec(ctx, `UPDATE users SET status = 'ACTIVE', deleted_at = NULL WHERE id = $1`, active.user)
	require.NoError(t, err)
	e.Clock.Advance(2 * time.Hour)
	_, err = e.Pool.Exec(ctx, `UPDATE users SET deleted_at = $2 WHERE id = $1`, young.user, e.Clock.Now().Add(-30*time.Minute))
	require.NoError(t, err)

	admin := testutil.SeedUser(t, e.Pool, testutil.Row{"role": "ADMIN"})
	testutil.MustInsert(ctx, t, e.Pool, "admin_audit_logs", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "admin_user_id": admin, "action": "USER_STATUS_CHANGE", "target_type": "USER",
		"target_id": gone.user.String(), "target_user_id": gone.user, "request_id": "req-1",
	})
	testutil.MustInsert(ctx, t, e.Pool, "processed_events", testutil.Row{
		"consumer": "memory.session_completed", "event_id": uuid.Must(uuid.NewV7()), "event_type": "session.completed",
	})
	before := countFinancials(t, e, gone.user)
	require.Equal(t, 1, before.accounts)
	require.Positive(t, before.ledger)
	require.Equal(t, 1, before.reservations)
	youngBefore, activeBefore := countUserData(t, e, young), countUserData(t, e, active)
	require.Equal(t, userCounts{2, 1, 1, 1, 1, 1, 3, activeBefore.events, 1}, activeBefore, "fixture sanity")
	require.Greater(t, activeBefore.events, 1, "the real session lifecycle wrote events too")

	job, m, logs := purgeJobFor(e, postgres.NewPurgeRepo(e.Pool))
	require.NoError(t, job.Run(ctx))

	assert.Equal(t, userCounts{}, countUserData(t, e, gone), "every purgeable row of the user is gone")
	assert.Equal(t, before, countFinancials(t, e, gone.user), "accounts, reservations, ledger, sessions, processed_events, audit logs unchanged")
	var email, name, hash string
	var phone *string
	require.NoError(t, e.Pool.QueryRow(ctx, `SELECT email, phone_number, full_name, password_hash FROM users WHERE id = $1`, gone.user).Scan(&email, &phone, &name, &hash))
	assert.Equal(t, "deleted+"+gone.user.String()+"@deleted.invalid", email)
	assert.Nil(t, phone)
	assert.Equal(t, "deleted", name)
	assert.Equal(t, "!", hash)
	assert.True(t, isPurged(t, e, gone.user))

	assert.Equal(t, youngBefore, countUserData(t, e, young), "deleted_at newer than the delay: untouched")
	assert.False(t, isPurged(t, e, young.user))
	assert.Equal(t, activeBefore, countUserData(t, e, active), "ACTIVE user: untouched")
	assert.False(t, isPurged(t, e, active.user))
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))
	assert.Contains(t, logs.String(), `"audit_event":"account_purged"`)
	assert.NotContains(t, logs.String(), purgeNeedle, "no PII in the audit log")
	assert.NotContains(t, logs.String(), gone.email)
}

// failing wraps the real repository and fails one step once, before it touches the database.
type failing struct {
	*postgres.PurgeRepo
	step string
	once bool
}

func (f *failing) trip(step string) error {
	if f.step == step && !f.once {
		f.once = true
		return errors.New("injected crash at " + step)
	}
	return nil
}

func (f *failing) DeleteMemories(ctx context.Context, u uuid.UUID) error {
	if err := f.trip("memories"); err != nil {
		return err
	}
	return f.PurgeRepo.DeleteMemories(ctx, u)
}

func (f *failing) DeleteEvaluations(ctx context.Context, u uuid.UUID) error {
	if err := f.trip("evaluations"); err != nil {
		return err
	}
	return f.PurgeRepo.DeleteEvaluations(ctx, u)
}

func (f *failing) DeleteLearner(ctx context.Context, u uuid.UUID) error {
	if err := f.trip("learner"); err != nil {
		return err
	}
	return f.PurgeRepo.DeleteLearner(ctx, u)
}

func (f *failing) DeleteTranscripts(ctx context.Context, u uuid.UUID, batch int) error {
	if err := f.trip("transcripts"); err != nil {
		return err
	}
	return f.PurgeRepo.DeleteTranscripts(ctx, u, batch)
}

func (f *failing) ClearSummaries(ctx context.Context, u uuid.UUID) error {
	if err := f.trip("summaries"); err != nil {
		return err
	}
	return f.PurgeRepo.ClearSummaries(ctx, u)
}

func (f *failing) Anonymize(ctx context.Context, u uuid.UUID, now time.Time) error {
	if err := f.trip("anonymize"); err != nil {
		return err
	}
	return f.PurgeRepo.Anonymize(ctx, u, now)
}

// A crash at any step leaves purged_at NULL (and the user row un-anonymized), the next run
// completes the purge, and a run after that finds nothing to do.
func TestAccountPurge_RerunAfterCrash_Idempotent(t *testing.T) {
	for _, step := range []string{"memories", "evaluations", "learner", "transcripts", "summaries", "anonymize"} {
		t.Run(step, func(t *testing.T) {
			e := sessionenv.New(t)
			ctx := context.Background()
			f := seedPurgeUser(t, e)
			e.Clock.Advance(2 * time.Hour)
			fin := countFinancials(t, e, f.user)
			store := &failing{PurgeRepo: postgres.NewPurgeRepo(e.Pool), step: step}
			job, m, _ := purgeJobFor(e, store)

			err := job.Run(ctx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "injected crash at "+step)
			assert.False(t, isPurged(t, e, f.user), "purged_at stays NULL until every step is done")
			assert.Equal(t, 1, e.Count(t, `SELECT count(*) FROM users WHERE id = $1 AND email = $2`, f.user, f.email), "not anonymized yet")
			assert.Equal(t, 0.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))

			require.NoError(t, job.Run(ctx), "second run finishes the purge from the first step")
			assert.True(t, isPurged(t, e, f.user))
			assert.Equal(t, userCounts{}, countUserData(t, e, f))
			assert.Equal(t, fin, countFinancials(t, e, f.user))
			assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))

			var purgedAt time.Time
			require.NoError(t, e.Pool.QueryRow(ctx, `SELECT purged_at FROM users WHERE id = $1`, f.user).Scan(&purgedAt))
			e.Clock.Advance(time.Hour)
			require.NoError(t, job.Run(ctx), "third run: nothing to do")
			assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))
			var again time.Time
			require.NoError(t, e.Pool.QueryRow(ctx, `SELECT purged_at FROM users WHERE id = $1`, f.user).Scan(&again))
			assert.True(t, purgedAt.Equal(again), "purged_at is not rewritten")
		})
	}
}

func TestAccountPurge_NoPIIRemainsInSummaries(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	f := seedPurgeUser(t, e)
	e.Clock.Advance(2 * time.Hour)
	job, _, _ := purgeJobFor(e, postgres.NewPurgeRepo(e.Pool))
	require.NoError(t, job.Run(ctx))

	assert.Zero(t, e.Count(t, `SELECT count(*) FROM sessions s WHERE s.user_id = $1 AND (s.summary_text IS NOT NULL OR s::text LIKE '%'||$2||'%')`, f.user, purgeNeedle))
	assert.Zero(t, e.Count(t, `SELECT count(*) FROM users u WHERE u.id = $1 AND (u::text LIKE '%'||$2||'%' OR u::text LIKE '%'||$3||'%')`, f.user, purgeNeedle, "example.test"))
	assert.Zero(t, e.Count(t, `SELECT count(*) FROM session_turns WHERE user_text LIKE '%'||$1||'%'`, purgeNeedle))
	assert.Zero(t, e.Count(t, `SELECT count(*) FROM learner_profiles WHERE nickname = $1`, purgeNeedle))
}

func TestAccountPurge_Max50PerRun_OldestFirst(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	base := e.Clock.Now()
	ids := make([]uuid.UUID, 55)
	for i := range ids {
		// ids[0] is the newest deletion; ids[54] the oldest.
		ids[i] = testutil.SeedUser(t, e.Pool, testutil.Row{"status": "DELETED", "deleted_at": base.Add(-time.Duration(i) * time.Minute)})
	}
	e.Clock.Advance(2 * time.Hour)
	job, m, _ := purgeJobFor(e, postgres.NewPurgeRepo(e.Pool))

	require.NoError(t, job.Run(ctx))
	assert.Equal(t, 50.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))
	for i, id := range ids {
		assert.Equal(t, i >= 5, isPurged(t, e, id), "user %d: the 50 oldest deleted_at go first", i)
	}
	require.NoError(t, job.Run(ctx))
	assert.Equal(t, 55.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))
	for _, id := range ids {
		assert.True(t, isPurged(t, e, id))
	}
}

func TestAccountPurge_OnlyDeletedPastDelay(t *testing.T) {
	e := sessionenv.New(t)
	ctx := context.Background()
	now := e.Clock.Now()
	justInside := testutil.SeedUser(t, e.Pool, testutil.Row{"status": "DELETED", "deleted_at": now.Add(-time.Hour + time.Second)})
	exactly := testutil.SeedUser(t, e.Pool, testutil.Row{"status": "DELETED", "deleted_at": now.Add(-time.Hour)})
	past := testutil.SeedUser(t, e.Pool, testutil.Row{"status": "DELETED", "deleted_at": now.Add(-time.Hour - time.Second)})
	suspended := testutil.SeedUser(t, e.Pool, testutil.Row{"status": "SUSPENDED"})
	active := testutil.SeedUser(t, e.Pool, nil)
	job, _, _ := purgeJobFor(e, postgres.NewPurgeRepo(e.Pool))

	require.NoError(t, job.Run(ctx))
	assert.True(t, isPurged(t, e, past))
	assert.False(t, isPurged(t, e, exactly), "deleted_at == now - delay is not strictly older")
	assert.False(t, isPurged(t, e, justInside))
	assert.False(t, isPurged(t, e, suspended))
	assert.False(t, isPurged(t, e, active))
}
