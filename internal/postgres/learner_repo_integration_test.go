//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

type pgLearner struct {
	pool    *pgxpool.Pool
	store   *postgres.Store
	content testutil.ContentIDs
	n       atomic.Int64
}

func newPGLearner(t *testing.T) *pgLearner {
	t.Helper()
	pool := testutil.NewPostgres(t)
	return &pgLearner{pool: pool, store: postgres.NewStore(pool), content: testutil.SeedContent(t, pool)}
}

func mergeRow(base, over testutil.Row) testutil.Row {
	for k, v := range over {
		base[k] = v
	}
	return base
}

// moreContent adds a second topic, scenario, version and character beside p.content.
func (p *pgLearner) moreContent(t *testing.T) testutil.ContentIDs {
	t.Helper()
	ctx := context.Background()
	topic := testutil.TopicRow(nil)
	testutil.MustInsert(ctx, t, p.pool, "topics", topic)
	sc := testutil.ScenarioRow(topic["id"].(uuid.UUID), nil)
	testutil.MustInsert(ctx, t, p.pool, "scenarios", sc)
	sv := testutil.ScenarioVersionRow(sc["id"].(uuid.UUID), 1, testutil.Row{"status": "PUBLISHED", "published_at": time.Now().UTC()})
	testutil.MustInsert(ctx, t, p.pool, "scenario_versions", sv)
	ch := testutil.CharacterRow(nil)
	testutil.MustInsert(ctx, t, p.pool, "characters", ch)
	return testutil.ContentIDs{
		TopicID: topic["id"].(uuid.UUID), ScenarioID: sc["id"].(uuid.UUID), ScenarioVersionID: sv["id"].(uuid.UUID),
		CharacterID: ch["id"].(uuid.UUID), PromptVersionID: p.content.PromptVersionID,
	}
}

func (p *pgLearner) user(t *testing.T) uuid.UUID { return testutil.SeedUser(t, p.pool, nil) }

// evaluation inserts an evaluation of userID (COMPLETED with scores unless o says otherwise) on a
// fresh RESERVED session, or on o["session_id"].
func (p *pgLearner) evaluation(t *testing.T, userID uuid.UUID, o testutil.Row) (evalID, sessionID uuid.UUID) {
	t.Helper()
	sid, ok := o["session_id"].(uuid.UUID)
	if !ok { // ux_sessions_one_open_per_user: an evaluated session is terminal
		sid = p.completed(t, userID, p.content, time.Now().UTC().Add(-time.Duration(p.n.Add(1))*time.Minute), 60, "COMPLETED", "FEMALE", "KANTO")
	}
	id := uuid.Must(uuid.NewV7())
	row := mergeRow(testutil.Row{
		"id": id, "session_id": sid, "user_id": userID, "status": "COMPLETED", "rubric_version": "rubric-v1",
		"source_event_id": uuid.Must(uuid.NewV7()), "grammar_score": 60, "vocabulary_score": 61, "fluency_score": 62,
		"naturalness_score": 63, "task_achievement_score": 64, "overall_score": 62.0,
		"completed_at": time.Now().UTC(), "started_at": time.Now().UTC(),
	}, o)
	testutil.MustInsert(context.Background(), t, p.pool, "evaluations", row)
	return id, sid
}

func (p *pgLearner) finding(t *testing.T, userID, evalID, sessionID uuid.UUID, ordinal int, dimension, typ, severity string) {
	t.Helper()
	testutil.MustInsert(context.Background(), t, p.pool, "evaluation_findings", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "evaluation_id": evalID, "session_id": sessionID, "user_id": userID,
		"ordinal": ordinal, "dimension": dimension, "finding_type": typ, "severity": severity,
		"explanation": "penjelasan", "confidence": 0.9, "source": "LLM",
	})
}

// completed inserts a terminal session of userID ending at endedAt with the given voice.
func (p *pgLearner) completed(t *testing.T, userID uuid.UUID, c testutil.ContentIDs, endedAt time.Time, charged int, status, gender, accent string) uuid.UUID {
	t.Helper()
	snap, err := json.Marshal(map[string]any{
		"character": map[string]any{"id": c.CharacterID, "voice": map[string]any{"gender": gender, "accent": accent}},
		"formality": "POLITE",
	})
	require.NoError(t, err)
	started := endedAt.Add(-400 * time.Second)
	row := testutil.Row{
		"status": status, "end_reason": "USER_ENDED", "starting_at": started.Add(-2 * time.Second), "started_at": started,
		"expires_at": started.Add(300 * time.Second), "ending_at": endedAt, "ended_at": endedAt,
		"charged_seconds": charged, "config_snapshot": snap,
	}
	if status == "FAILED" {
		row["end_reason"] = "PROVIDER_FAILURE"
	}
	return testutil.SeedSession(t, p.pool, userID, c, row)
}

func TestPGLearnerRepo(t *testing.T) {
	repotest.LearnerRepoSuite(t, func(t *testing.T) repotest.LearnerHarness {
		p := newPGLearner(t)
		return repotest.LearnerHarness{
			Store:   p.store.LearnerStore(),
			NewUser: p.user,
			NewEvaluation: func(t *testing.T, userID uuid.UUID) (uuid.UUID, uuid.UUID) {
				return p.evaluation(t, userID, nil)
			},
		}
	})
}

func TestEstimateInsert_RawDuplicateViolatesUnique(t *testing.T) {
	p := newPGLearner(t)
	uid := p.user(t)
	ev, sid := p.evaluation(t, uid, nil)
	require.NoError(t, p.store.LearnerStore().WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		return tx.Profiles().Ensure(ctx, uid)
	}))
	row := func() testutil.Row {
		return testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "user_id": uid, "skill": "GRAMMAR", "source_evaluation_id": ev, "session_id": sid,
			"algorithm_version": 1, "raw_score": 58, "evidence_level": 2.66, "evidence_confidence": 0.5,
			"previous_level": 1.0, "estimated_level": 1.5, "confidence": 0.2, "evidence_count": 1,
			"trend": "INSUFFICIENT_DATA", "assessed_at": time.Now().UTC(),
		}
	}
	testutil.MustInsert(context.Background(), t, p.pool, "proficiency_estimates", row())
	err := testutil.TryInsert(context.Background(), p.pool, "proficiency_estimates", row())
	testutil.RequirePgErr(t, err, "23505", "ux_proficiency_estimates_eval_skill")
}

func TestSkillStateEvidenceConsistencyCheck(t *testing.T) {
	p := newPGLearner(t)
	uid := p.user(t)
	require.NoError(t, p.store.LearnerStore().WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		return tx.Profiles().Ensure(ctx, uid)
	}))
	_, err := p.pool.Exec(context.Background(),
		`UPDATE learner_skill_states SET evidence_count = 1 WHERE user_id = $1 AND skill = 'GRAMMAR'`, uid)
	testutil.RequirePgErr(t, err, "23514", "chk_lss_evidence_consistency")
	// Save surfaces the same CHECK: it does not paper over a state that claims evidence without a date
	err = p.store.LearnerStore().WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		return tx.SkillStates().Save(ctx, uid, learner.StateRow{
			Skill: learner.SkillGrammar, Level: 1, Confidence: 0.1, Trend: learner.TrendStable, EvidenceCount: 1,
		}, time.Now())
	})
	require.Error(t, err)
	testutil.RequirePgErr(t, err, "23514", "chk_lss_evidence_consistency")
}

func TestLearnerIsolation_OtherUserRowsInvisible(t *testing.T) {
	p := newPGLearner(t)
	a, b := p.user(t), p.user(t)
	ctx := context.Background()
	store := p.store.LearnerStore()
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		require.NoError(t, tx.Profiles().Ensure(ctx, a))
		return tx.Profiles().Ensure(ctx, b)
	}))
	// B has a completed session, an evaluation and an estimate; A has nothing of the sort
	sb := p.completed(t, b, p.content, time.Now().UTC().Add(-time.Hour), 120, "COMPLETED", "FEMALE", "KANTO")
	evB, _ := p.evaluation(t, b, testutil.Row{"session_id": sb})
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		_, err := tx.Estimates().Insert(ctx, b, learner.EstimateRow{
			ID: uuid.Must(uuid.NewV7()), EvaluationID: evB, SessionID: sb, Skill: learner.SkillGrammar,
			AlgorithmVersion: 1, RawScore: 70, LObs: 3, CObs: 0.5, AssessedAt: time.Now().UTC(),
			Previous: 1, Level: 2, Confidence: 0.2, Count: 1, Trend: learner.TrendInsufficient,
		})
		return err
	}))
	p.finding(t, b, evB, sb, 1, "GRAMMAR", "PARTICLE_ERROR", "MAJOR")

	rd := store.Reader()
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		st, err := tx.Profiles().Stats(ctx, a)
		require.NoError(t, err)
		require.Zero(t, st.CompletedSessions)
		require.Zero(t, st.PracticeSeconds)

		est, err := tx.Estimates().ListBySkill(ctx, a, learner.SkillGrammar)
		require.NoError(t, err)
		require.Empty(t, est)

		// A cannot read B's evaluation: the row exists, so it is "not found", not "not visible"
		_, err = tx.Evaluations().Evidence(ctx, a, evB)
		require.ErrorIs(t, err, learner.ErrNotFound)
		require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
		require.NotErrorIs(t, err, learner.ErrEvaluationNotVisible)
		return nil
	}))
	stats, n, err := rd.RecentFindings(ctx, a)
	require.NoError(t, err)
	require.Empty(t, stats)
	require.Zero(t, n)
	exp, err := rd.VoiceExposure(ctx, a, time.Now())
	require.NoError(t, err)
	require.Empty(t, exp)
	cnt, err := rd.TopicSessions90d(ctx, a, p.content.TopicID, time.Now())
	require.NoError(t, err)
	require.Zero(t, cnt)

	// and B sees its own
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		st, err := tx.Profiles().Stats(ctx, b)
		require.NoError(t, err)
		require.Equal(t, 1, st.CompletedSessions)
		require.EqualValues(t, 120, st.PracticeSeconds)
		require.NotNil(t, st.LastSessionAt)
		return nil
	}))
	stats, n, err = rd.RecentFindings(ctx, b)
	require.NoError(t, err)
	require.Equal(t, learner.FindingStat{Major: 1}, stats[learner.SkillGrammar])
	require.Equal(t, 1, n)
}

func TestEvidence_ReadsNTurns(t *testing.T) {
	p := newPGLearner(t)
	ctx := context.Background()
	uid := p.user(t)
	endedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	sid := p.completed(t, uid, p.content, endedAt, 120, "COMPLETED", "FEMALE", "KANTO")
	turns := []struct{ user, status string }{
		{"会社で働いています", "COMPLETED"},  // counts
		{"はい、そうです", "INTERRUPTED"},  // counts: INTERRUPTED
		{"毎日電車で行きます", "DISCARDED"},  // discarded
		{"はいはい", "COMPLETED"},       // 4 runes
		{"ちょうど五文字", "COMPLETED"},    // 7 runes: counts
		{"いいえ、ちがう", "FAILED"},       // failed
		{"ありがとうございます", "COMPLETED"}, // counts
	}
	for i, tr := range turns {
		created := endedAt.Add(-time.Duration(len(turns)-i) * 20 * time.Second)
		testutil.MustInsert(ctx, t, p.pool, "session_turns", testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": i + 1, "status": tr.status,
			"user_text": tr.user, "assistant_text": "ええ", "generation": 0,
			"user_started_at": created, "user_ended_at": created.Add(3 * time.Second), "created_at": created,
			"completed_at": created.Add(3 * time.Second),
		})
	}
	// a turn of a user in progress (NULL status, NULL text) never counts
	testutil.MustInsert(ctx, t, p.pool, "session_turns", testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": 99, "generation": 0, "created_at": endedAt.Add(-5 * time.Second),
	})
	evID, _ := p.evaluation(t, uid, testutil.Row{
		"session_id": sid, "listening_response_signal": 71, "interaction_complexity_signal": nil,
		"dimension_confidence": []byte(`{"GRAMMAR":0.82,"LISTENING_RESPONSE":0.6}`),
	})
	var ev learner.EvaluationEvidence
	require.NoError(t, p.store.LearnerStore().WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		var err error
		ev, err = tx.Evaluations().Evidence(ctx, uid, evID)
		return err
	}))
	require.Equal(t, 4, ev.NTurns, "COMPLETED and INTERRUPTED turns of at least 5 runes")
	require.Equal(t, evID, ev.EvaluationID)
	require.Equal(t, sid, ev.SessionID)
	require.Equal(t, uid, ev.UserID)
	require.Equal(t, "COMPLETED", ev.Status)
	require.Equal(t, 60, *ev.Scores[learner.SkillGrammar])
	require.Equal(t, 61, *ev.Scores[learner.SkillVocabulary])
	require.Equal(t, 62, *ev.Scores[learner.SkillFluency])
	require.Equal(t, 63, *ev.Scores[learner.SkillNaturalness])
	require.Equal(t, 71, *ev.ListeningSignal)
	require.Nil(t, ev.InteractionSignal)
	require.InDelta(t, 0.82, ev.DimensionConfidence["GRAMMAR"], 1e-9)
	require.InDelta(t, 0.6, ev.DimensionConfidence["LISTENING_RESPONSE"], 1e-9)
	require.True(t, ev.SessionEndedAt.Equal(endedAt), "session ended_at is the assessment time")
	require.JSONEq(t, `{}`, string(ev.AdaptiveDifficulty))
	require.Equal(t, scenarioDifficultyOf(t, p, p.content.ScenarioVersionID), ev.ScenarioDifficulty)
}

func scenarioDifficultyOf(t *testing.T, p *pgLearner, scenarioVersionID uuid.UUID) int {
	t.Helper()
	var d int
	require.NoError(t, p.pool.QueryRow(context.Background(),
		`SELECT difficulty_level FROM scenario_versions WHERE id = $1`, scenarioVersionID).Scan(&d))
	return d
}

func TestEvidence_NullScoresStayNil(t *testing.T) {
	p := newPGLearner(t)
	uid := p.user(t)
	evID, _ := p.evaluation(t, uid, testutil.Row{
		"insufficient_evidence": true, "grammar_score": nil, "vocabulary_score": nil, "fluency_score": nil,
		"naturalness_score": nil, "task_achievement_score": nil, "overall_score": nil,
	})
	var ev learner.EvaluationEvidence
	require.NoError(t, p.store.LearnerStore().WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		var err error
		ev, err = tx.Evaluations().Evidence(ctx, uid, evID)
		return err
	}))
	for _, k := range []learner.Skill{learner.SkillGrammar, learner.SkillVocabulary, learner.SkillFluency, learner.SkillNaturalness} {
		require.Nil(t, ev.Scores[k], k)
	}
	require.Zero(t, ev.NTurns)
}

func TestEvidence_UserMismatch(t *testing.T) {
	p := newPGLearner(t)
	ctx := context.Background()
	owner, other := p.user(t), p.user(t)
	evID, _ := p.evaluation(t, owner, nil)
	store := p.store.LearnerStore()
	err := store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		_, err := tx.Evaluations().Evidence(ctx, other, evID)
		return err
	})
	require.ErrorIs(t, err, learner.ErrNotFound)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))

	// a payload user that matches the evaluation but not its session is a mismatch as well
	sessionOfOther := testutil.SeedSession(t, p.pool, other, p.content, nil)
	_, err = p.pool.Exec(ctx, `UPDATE evaluations SET session_id = $1 WHERE id = $2`, sessionOfOther, evID)
	require.NoError(t, err)
	err = store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		_, err := tx.Evaluations().Evidence(ctx, owner, evID)
		return err
	})
	require.ErrorIs(t, err, learner.ErrNotFound, "evaluation.user_id != session.user_id")
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestEvidence_NotYetVisibleIsRetryable(t *testing.T) {
	p := newPGLearner(t)
	err := p.store.LearnerStore().WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		_, err := tx.Evaluations().Evidence(ctx, p.user(t), uuid.Must(uuid.NewV7()))
		return err
	})
	require.ErrorIs(t, err, learner.ErrEvaluationNotVisible)
	require.NotErrorIs(t, err, learner.ErrNotFound)
}

func TestRecentFindings_Last5CompletedEvaluations(t *testing.T) {
	p := newPGLearner(t)
	ctx := context.Background()
	uid, other := p.user(t), p.user(t)
	base := time.Now().UTC().Add(-10 * time.Hour)

	// the oldest of six COMPLETED evaluations is outside the window of 5
	old, oldSess := p.evaluation(t, uid, testutil.Row{"completed_at": base})
	p.finding(t, uid, old, oldSess, 1, "GRAMMAR", "PARTICLE_ERROR", "MAJOR")
	p.finding(t, uid, old, oldSess, 2, "GRAMMAR", "PARTICLE_ERROR", "MAJOR")
	for i := 1; i <= 5; i++ {
		ev, sess := p.evaluation(t, uid, testutil.Row{"completed_at": base.Add(time.Duration(i) * time.Hour)})
		switch i {
		case 1:
			p.finding(t, uid, ev, sess, 1, "GRAMMAR", "PARTICLE_ERROR", "MAJOR")
			p.finding(t, uid, ev, sess, 2, "GRAMMAR", "CONJUGATION_ERROR", "MINOR")
			p.finding(t, uid, ev, sess, 3, "GRAMMAR", "TENSE_ASPECT_ERROR", "INFO")
		case 2:
			p.finding(t, uid, ev, sess, 1, "GRAMMAR", "PARTICLE_ERROR", "MINOR")
			p.finding(t, uid, ev, sess, 2, "NATURALNESS", "UNNATURAL_EXPRESSION", "MINOR")
			p.finding(t, uid, ev, sess, 3, "TASK_ACHIEVEMENT", "OFF_TOPIC_RESPONSE", "MAJOR")
		case 3:
			p.finding(t, uid, ev, sess, 1, "VOCABULARY", "WORD_CHOICE_ERROR", "MAJOR")
		}
	}
	// a RUNNING evaluation and another user's evaluation never count
	run, runSess := p.evaluation(t, uid, testutil.Row{
		"status": "RUNNING", "completed_at": nil, "grammar_score": nil, "vocabulary_score": nil, "fluency_score": nil,
		"naturalness_score": nil, "task_achievement_score": nil, "overall_score": nil,
	})
	p.finding(t, uid, run, runSess, 1, "GRAMMAR", "PARTICLE_ERROR", "MAJOR")
	oth, othSess := p.evaluation(t, other, testutil.Row{"completed_at": base.Add(9 * time.Hour)})
	p.finding(t, other, oth, othSess, 1, "FLUENCY", "EXCESSIVE_FILLER", "MAJOR")

	stats, n, err := p.store.LearnerStore().Reader().RecentFindings(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.Equal(t, map[learner.Skill]learner.FindingStat{
		learner.SkillGrammar:     {Major: 1, Minor: 2},
		learner.SkillNaturalness: {Minor: 1},
		learner.SkillVocabulary:  {Major: 1},
	}, stats, "INFO counts nothing, TASK_ACHIEVEMENT is not a skill, the 6th-oldest evaluation is out")
}

func TestRecentFindings_EvaluationsWithoutFindingsStillCount(t *testing.T) {
	p := newPGLearner(t)
	uid := p.user(t)
	p.evaluation(t, uid, nil)
	p.evaluation(t, uid, nil)
	stats, n, err := p.store.LearnerStore().Reader().RecentFindings(context.Background(), uid)
	require.NoError(t, err)
	require.Empty(t, stats)
	require.Equal(t, 2, n)
}

func TestVoiceExposure_Window90d_CompletedOnly(t *testing.T) {
	p := newPGLearner(t)
	ctx := context.Background()
	uid, other := p.user(t), p.user(t)
	now := time.Now().UTC()
	c2 := p.moreContent(t)

	p.completed(t, uid, p.content, now.Add(-24*time.Hour), 100, "COMPLETED", "FEMALE", "KANTO")
	p.completed(t, uid, p.content, now.Add(-48*time.Hour), 200, "COMPLETED", "FEMALE", "KANTO")
	p.completed(t, uid, c2, now.Add(-72*time.Hour), 50, "COMPLETED", "MALE", "KANSAI")
	p.completed(t, uid, p.content, now.Add(-100*24*time.Hour), 300, "COMPLETED", "FEMALE", "KANTO") // outside 90 days
	p.completed(t, uid, p.content, now.Add(-time.Hour), 300, "FAILED", "MALE", "KANSAI")            // not COMPLETED
	p.completed(t, other, p.content, now.Add(-time.Hour), 300, "COMPLETED", "MALE", "KANSAI")       // somebody else's

	rd := p.store.LearnerStore().Reader()
	rows, err := rd.VoiceExposure(ctx, uid, now)
	require.NoError(t, err)
	got := map[string]learner.ExposureRow{}
	for _, r := range rows {
		got[r.Gender+"/"+r.Accent] = r
	}
	require.Len(t, got, 2)
	require.Equal(t, learner.ExposureRow{Gender: "FEMALE", Accent: "KANTO", Sessions: 2, Seconds: 300}, got["FEMALE/KANTO"])
	require.Equal(t, learner.ExposureRow{Gender: "MALE", Accent: "KANSAI", Sessions: 1, Seconds: 50}, got["MALE/KANSAI"])

	nTopic, err := rd.TopicSessions90d(ctx, uid, p.content.TopicID, now)
	require.NoError(t, err)
	require.Equal(t, 2, nTopic, "the old, FAILED and foreign sessions of the topic do not count")
	nTopic2, err := rd.TopicSessions90d(ctx, uid, c2.TopicID, now)
	require.NoError(t, err)
	require.Equal(t, 1, nTopic2)
	nDialect, err := rd.DialectSessions90d(ctx, uid, now)
	require.NoError(t, err)
	require.Equal(t, 1, nDialect)

	// the window moves with the clock handed in: 88 days on, only the newest session is inside it
	later, err := rd.VoiceExposure(ctx, uid, now.Add(88*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []learner.ExposureRow{{Gender: "FEMALE", Accent: "KANTO", Sessions: 1, Seconds: 100}}, later)
}

func TestLearnerTxProcessedSharesTheTransaction(t *testing.T) {
	p := newPGLearner(t)
	ctx := context.Background()
	store := p.store.LearnerStore()
	env := events.Envelope{EventID: uuid.Must(uuid.NewV7()), EventType: "evaluation.completed"}
	boom := context.Canceled
	err := store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		require.NoError(t, tx.Processed().MarkProcessed(ctx, events.QueueLearner, env))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueLearner, env) // the rollback left no row
	}))
	err = store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueLearner, env)
	})
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
}

func TestLearnerLockUserSerialisesWriters(t *testing.T) {
	p := newPGLearner(t)
	ctx := context.Background()
	uid := p.user(t)
	store := p.store.LearnerStore()
	entered, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
			if err := tx.Profiles().LockUser(ctx, uid); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	go func() {
		_ = store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
			err := tx.Profiles().LockUser(ctx, uid)
			close(second)
			return err
		})
	}()
	select {
	case <-second:
		t.Fatal("second writer took the lock while the first holds it")
	case <-time.After(300 * time.Millisecond):
	}
	// another user is not blocked
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		return tx.Profiles().LockUser(ctx, p.user(t))
	}))
	close(release)
	require.NoError(t, <-done)
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second writer never got the lock")
	}
}
