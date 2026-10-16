//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/redisstore"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// retentionRig applies memory candidates at a chosen instant straight on Postgres, so the stored
// expires_at (spec 11.8.1) and the cleanup/retrieval behaviour around it can be asserted.
type retentionRig struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store memory.Store
	emb   *memProvider
}

func newRetentionRig(t *testing.T, pool *pgxpool.Pool) *retentionRig {
	t.Helper()
	return &retentionRig{t: t, pool: pool, store: postgres.NewStore(pool).MemoryStore(), emb: newMemProvider()}
}

func (r *retentionRig) embed(text string) []float32 {
	r.t.Helper()
	res, err := r.emb.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{text}, Dimensions: 1536})
	require.NoError(r.t, err)
	return res.Vectors[0]
}

// apply stores one candidate for userID as if the worker ran at now.
func (r *retentionRig) apply(userID uuid.UUID, now time.Time, cat memory.Category, key, text string, mod ...func(*memory.Candidate)) memory.Outcome {
	r.t.Helper()
	c := memory.Candidate{
		Category: cat, Key: key, ContentJA: text, DisplayID: "isi " + key, EvidenceType: memory.EvidenceExplicit,
		Confidence: 0.95, Importance: 0.90, Embedding: r.embed(text), EmbeddingModel: "fake-embedding",
	}
	for _, fn := range mod {
		fn(&c)
	}
	ap := memory.NewApplier(idgen.NewV7(), clock.NewFake(now), nil)
	var out memory.Outcome
	require.NoError(r.t, r.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		if err := tx.Memories().LockUser(ctx, userID); err != nil {
			return err
		}
		var err error
		out, err = ap.ApplyOutcome(ctx, tx, userID, c)
		return err
	}))
	return out
}

func (r *retentionRig) expiresAt(id uuid.UUID) time.Time {
	r.t.Helper()
	var at time.Time
	require.NoError(r.t, r.pool.QueryRow(context.Background(), `SELECT expires_at FROM memories WHERE id = $1`, id).Scan(&at))
	return at.UTC()
}

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// Spec 11.8.1 on Postgres: every category stores its window; superseded 30 d; tombstone 90 d.
func TestRetention_TTLsEndToEnd(t *testing.T) {
	pool := testutil.NewPostgres(t)
	r := newRetentionRig(t, pool)
	user := testutil.SeedUser(t, pool, nil)
	T := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		cat  memory.Category
		key  string
		text string
		mod  []func(*memory.Candidate)
		want time.Duration
	}{
		{memory.CategoryProfileFact, "occupation", "バックエンドエンジニアとして働いている。", nil, day(730)},
		{memory.CategoryProfileFact, "plan:interview", "来月、面接を受ける予定がある。", []func(*memory.Candidate){func(c *memory.Candidate) {
			c.IsTemporary, c.ValidDays = true, ptrOf(45)
		}}, day(45)},
		{memory.CategoryPreference, "likes:coffee", "コーヒーが好きでよく飲む。", nil, day(365)},
		{memory.CategoryLearningPattern, "pattern:particles", "助詞の「は」と「が」を間違えやすい。", nil, day(180)},
		{memory.CategoryEpisodic, "episode:s1", "旅行の計画について話した。", nil, day(180)},
	} {
		out := r.apply(user, T, tc.cat, tc.key, tc.text, tc.mod...)
		require.Equal(t, memory.ActionCreated, out.Action, tc.key)
		require.True(t, T.Add(tc.want).Equal(r.expiresAt(out.ID)), "%s: want %s, got %s", tc.key, T.Add(tc.want), r.expiresAt(out.ID))
	}

	// the same slot again with another text: the old row is superseded for 30 days
	first := r.apply(user, T, memory.CategoryPreference, "likes:tea", "紅茶が好きだ。")
	second := r.apply(user, T.Add(day(1)), memory.CategoryPreference, "likes:tea", "お茶は緑茶しか飲まない。")
	require.Equal(t, memory.ActionSuperseded, second.Action)
	require.Equal(t, first.ID, second.SupersededID)
	require.True(t, T.Add(day(1)+day(30)).Equal(r.expiresAt(first.ID)), "superseded keeps 30 days from the supersede")
	require.True(t, T.Add(day(1)+day(365)).Equal(r.expiresAt(second.ID)))

	// a user deletion leaves a 90 day tombstone
	svc := memory.NewService(r.store, fakes.NewMemVerRecorder(), postgres.NewMemoryProfileSettings(pool, idgen.NewV7(), clock.System{}), clock.NewFake(T), nil)
	var coffee uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id FROM memories WHERE user_id = $1 AND memory_key = 'likes:coffee'`, user).Scan(&coffee))
	require.NoError(t, svc.Delete(context.Background(), user, coffee))
	var tomb uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT id FROM memories WHERE user_id = $1 AND status = 'DELETED'`, user).Scan(&tomb))
	require.True(t, T.Add(day(90)).Equal(r.expiresAt(tomb)))
}

func ptrOf[T any](v T) *T { return &v }

func memver(t *testing.T, rdb *redis.Client, user uuid.UUID) int64 {
	t.Helper()
	v, err := redisstore.NewMemCtxStore(rdb).GetMemVer(context.Background(), user)
	require.NoError(t, err)
	return v
}

func TestMemoryCleanup_DeletesExpiredAndBumpsMemver(t *testing.T) {
	pool := testutil.NewPostgres(t)
	rdb := testutil.NewRedis(t)
	r := newRetentionRig(t, pool)
	a, b, c := testutil.SeedUser(t, pool, nil), testutil.SeedUser(t, pool, nil), testutil.SeedUser(t, pool, nil)

	// the worker ran 181 days ago: 180 d memories are expired now, the 730 d fact is not
	past := time.Now().UTC().Add(-day(181))
	lpA := r.apply(a, past, memory.CategoryLearningPattern, "pattern:particles", "助詞の「は」と「が」を間違えやすい。")
	epA := r.apply(a, past, memory.CategoryEpisodic, "episode:s1", "旅行の計画について話した。")
	factA := r.apply(a, past, memory.CategoryProfileFact, "occupation", "バックエンドエンジニアとして働いている。")
	// B has a superseded row (superseded 10 days ago: kept until +20 d); C has one expired row too
	oldB := r.apply(b, past, memory.CategoryPreference, "likes:tea", "紅茶が好きだ。")
	newB := r.apply(b, time.Now().UTC().Add(-day(10)), memory.CategoryPreference, "likes:tea", "お茶は緑茶しか飲まない。")
	require.Equal(t, oldB.ID, newB.SupersededID)
	r.apply(c, past, memory.CategoryEpisodic, "episode:s9", "友達と映画を見に行った。")
	live := r.apply(c, time.Now().UTC(), memory.CategoryProfileFact, "hobby", "週末にランニングをしている。")

	reg := prometheus.NewRegistry()
	m := jobs.NewMetrics(reg)
	job := jobs.NewMemoryCleanupJob(postgres.NewRetentionRepo(pool), redisstore.NewMemVerBumper(rdb), m, nil)
	require.NoError(t, job.Run(context.Background()))

	exists := func(id uuid.UUID) bool {
		return count(t, pool, `SELECT count(*) FROM memories WHERE id = $1`, id) == 1
	}
	require.False(t, exists(lpA.ID), "expired LEARNING_PATTERN is deleted")
	require.False(t, exists(epA.ID), "expired EPISODIC is deleted")
	require.True(t, exists(factA.ID), "the PROFILE_FACT is still inside its 730 days")
	require.True(t, exists(live.ID))
	require.True(t, exists(newB.ID))
	require.True(t, exists(oldB.ID), "superseded 10 days ago: not expired yet")
	require.Equal(t, 3.0, promtest.ToFloat64(m.MemoryCleanupDeleted), "lpA, epA and the episode of C")

	require.Equal(t, int64(1), memver(t, rdb, a), "INCR exactly once for a user with two deleted rows")
	require.Equal(t, int64(1), memver(t, rdb, c))
	require.Equal(t, int64(0), memver(t, rdb, b), "a user without a deleted row is not bumped")

	// a second run has nothing to do and bumps nobody
	require.NoError(t, job.Run(context.Background()))
	require.Equal(t, int64(1), memver(t, rdb, a))
	require.Equal(t, 3.0, promtest.ToFloat64(m.MemoryCleanupDeleted))

	// an expired SUPERSEDED row and an expired tombstone go too (any status)
	_, err := pool.Exec(context.Background(), `UPDATE memories SET expires_at = now() - interval '1 day' WHERE id = $1`, oldB.ID)
	require.NoError(t, err)
	require.NoError(t, job.Run(context.Background()))
	require.False(t, exists(oldB.ID))
	require.Equal(t, int64(1), memver(t, rdb, b))
}

// Spec 11.8: an expired row never reaches a session even before memory_cleanup has deleted it.
func TestRetrieval_NeverReturnsExpiredEvenBeforeCleanup(t *testing.T) {
	rr, rig := newRetrievalRigPG(t)
	r := newRetentionRig(t, rig.pool)
	user := testutil.SeedUser(t, rig.pool, nil)
	const episode = "面接練習（バックエンドエンジニア）。Goを選んだ理由を説明した。"
	past := time.Now().UTC().Add(-day(181))
	core := r.apply(user, past, memory.CategoryProfileFact, "occupation", "バックエンドエンジニアとして働いている。")
	ep := r.apply(user, past, memory.CategoryEpisodic, "episode:old", episode)
	_, err := rig.pool.Exec(context.Background(), `UPDATE memories SET expires_at = now() - interval '1 hour' WHERE id = ANY($1)`, []uuid.UUID{core.ID, ep.ID})
	require.NoError(t, err)

	mc, _ := rr.build(user, episode)
	require.False(t, mc.Degraded)
	require.Empty(t, mc.Core, "expired core memory is not returned")
	require.Empty(t, mc.Episodic, "expired episode is not returned")
	require.Equal(t, 2, count(t, rig.pool, `SELECT count(*) FROM memories WHERE user_id = $1`, user), "cleanup has not run")

	// control: the same rows with a future expires_at are returned
	_, err = rig.pool.Exec(context.Background(), `UPDATE memories SET expires_at = now() + interval '1 day' WHERE id = ANY($1)`, []uuid.UUID{core.ID, ep.ID})
	require.NoError(t, err)
	mc, _ = rr.build(user, episode)
	require.Len(t, mc.Core, 1)
	require.Equal(t, core.ID, mc.Core[0].ID)
	require.Len(t, mc.Episodic, 1)
	require.Equal(t, ep.ID, mc.Episodic[0].ID)
}

// newLearnerAndMemoryRig runs the memory and the learner consumer over one database: the catalog
// comes from the first finished session (SeedFinishedSession seeds its own content).
func newLearnerAndMemoryRig(t *testing.T) (*learnerRig, *memRig) {
	t.Helper()
	pool := testutil.NewPostgres(t)
	mr := newMemRigOn(t, pool)
	boot := mr.session(testutil.FinishedSessionOpts{})
	return newLearnerRigOn(t, pool, boot.Content), mr
}

// account_purge: memories (tombstones included), learner profile/states/estimates of the deleted
// user go; the sessions row stays; another user's rows are untouched.
func TestAccountPurge_RemovesMemoriesAndLearnerRows(t *testing.T) {
	lr, mr := newLearnerAndMemoryRig(t)
	pool := lr.pool
	ctx := context.Background()

	seedUser := func(level string) (uuid.UUID, uuid.UUID) {
		uid := lr.user(level)
		fs := mr.session(testutil.FinishedSessionOpts{UserID: uid})
		require.NoError(t, mr.handle(fs.Envelope))
		ev, sid := lr.eval(uid, learnerEval{endedAt: lr.base, nTurns: 15, scores: [4]int{82, 80, 78, 76}})
		require.NoError(t, lr.handle(lr.delivery(uid, ev, sid)))
		// a tombstone: the user deletes one of the memories
		var id uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM memories WHERE user_id = $1 AND memory_key = 'occupation' AND status = 'ACTIVE'`, uid).Scan(&id))
		svc := memory.NewService(postgres.NewStore(pool).MemoryStore(), fakes.NewMemVerRecorder(), postgres.NewMemoryProfileSettings(pool, idgen.NewV7(), clock.System{}), clock.System{}, nil)
		require.NoError(t, svc.Delete(ctx, uid, id))
		return uid, fs.ID
	}
	gone, goneSession := seedUser("N4")
	other, otherSession := seedUser("N3")

	type counts struct{ memories, tombstones, profiles, states, estimates, sessions int }
	read := func(uid, sid uuid.UUID) counts {
		return counts{
			memories:   count(t, pool, `SELECT count(*) FROM memories WHERE user_id = $1`, uid),
			tombstones: count(t, pool, `SELECT count(*) FROM memories WHERE user_id = $1 AND status = 'DELETED'`, uid),
			profiles:   count(t, pool, `SELECT count(*) FROM learner_profiles WHERE user_id = $1`, uid),
			states:     count(t, pool, `SELECT count(*) FROM learner_skill_states WHERE user_id = $1`, uid),
			estimates:  count(t, pool, `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, uid),
			sessions:   count(t, pool, `SELECT count(*) FROM sessions WHERE id = $1`, sid),
		}
	}
	before := read(gone, goneSession)
	require.Equal(t, 1, before.tombstones, "fixture: a tombstone exists")
	require.Positive(t, before.memories)
	require.Equal(t, 6, before.states)
	require.Positive(t, before.estimates)
	require.Equal(t, 1, before.profiles)
	otherBefore := read(other, otherSession)
	require.Equal(t, before, otherBefore, "both users were built the same way")

	_, err := pool.Exec(ctx, `UPDATE users SET status = 'DELETED', deleted_at = now() - interval '2 hours' WHERE id = $1`, gone)
	require.NoError(t, err)
	job := jobs.NewAccountPurgeJob(postgres.NewPurgeRepo(pool), time.Hour, jobs.NewMetrics(prometheus.NewRegistry()), clock.System{}, nil)
	require.NoError(t, job.Run(ctx))

	after := read(gone, goneSession)
	require.Equal(t, counts{sessions: 1}, after, "memories, tombstones, learner rows gone; the session row stays")
	require.Equal(t, otherBefore, read(other, otherSession), "the other user is untouched")
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM users WHERE id = $1 AND purged_at IS NOT NULL`, gone))
	require.Equal(t, 0, count(t, pool, `SELECT count(*) FROM users WHERE id = $1 AND purged_at IS NOT NULL`, other))
	testutil.AssertReconciled(t, pool)
}

// A session.completed / evaluation.completed of a DELETED user is acknowledged without any write
// but the processed_events row (spec 9.7.2).
func TestAccountPurge_DeletedUserEventsSkipped(t *testing.T) {
	lr, mr := newLearnerAndMemoryRig(t)
	pool := lr.pool

	fs := mr.session(testutil.FinishedSessionOpts{UserStatus: "DELETED"})
	err := mr.handle(fs.Envelope)
	require.ErrorIs(t, err, events.ErrSkippedDeletedUser)
	require.Equal(t, 1, mr.processed(fs.Envelope.EventID))
	require.Equal(t, 0, mr.memCount(fs.UserID, ``))
	require.Equal(t, 0, outboxCount(t, pool, "memory.extracted", fs.ID))
	require.Zero(t, mr.prov.llmCalls.Load(), "no provider call for a deleted user")
	require.Equal(t, 0, count(t, pool, `SELECT count(*) FROM learner_profiles WHERE user_id = $1`, fs.UserID))

	uid := fs.UserID
	ev, sid := lr.eval(uid, learnerEval{endedAt: lr.base, nTurns: 15, scores: [4]int{82, 80, 78, 76}})
	d := lr.delivery(uid, ev, sid)
	require.ErrorIs(t, lr.handle(d), events.ErrSkippedDeletedUser)
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, events.QueueLearner, d.Envelope.EventID))
	for _, tbl := range []string{"memories", "learner_profiles", "learner_skill_states", "proficiency_estimates"} {
		require.Equal(t, 0, count(t, pool, `SELECT count(*) FROM `+tbl+` WHERE user_id = $1`, uid), tbl)
	}
	require.Equal(t, 2, count(t, pool, `SELECT count(*) FROM processed_events WHERE event_id IN ($1, $2)`, fs.Envelope.EventID, d.Envelope.EventID))
}

// A user deleted less than the delay ago is not purged yet (negative control of the purge).
func TestAccountPurge_YoungDeletedUserUntouched(t *testing.T) {
	pool := testutil.NewPostgres(t)
	r := newRetentionRig(t, pool)
	user := testutil.SeedUser(t, pool, testutil.Row{"status": "DELETED", "deleted_at": time.Now().UTC().Add(-10 * time.Minute)})
	out := r.apply(user, time.Now().UTC(), memory.CategoryProfileFact, "occupation", "バックエンドエンジニアとして働いている。")
	job := jobs.NewAccountPurgeJob(postgres.NewPurgeRepo(pool), time.Hour, jobs.NewMetrics(prometheus.NewRegistry()), clock.System{}, nil)
	require.NoError(t, job.Run(context.Background()))
	require.Equal(t, 1, count(t, pool, `SELECT count(*) FROM memories WHERE id = $1`, out.ID))
	require.Equal(t, 0, count(t, pool, `SELECT count(*) FROM users WHERE id = $1 AND purged_at IS NOT NULL`, user))
}
