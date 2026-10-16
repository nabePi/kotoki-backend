//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type learnerRig struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *postgres.Store
	cons    *learner.Consumer
	svc     *learner.Service
	content testutil.ContentIDs
	base    time.Time
}

func newLearnerRig(t *testing.T) *learnerRig {
	t.Helper()
	pool := testutil.NewPostgres(t)
	store := postgres.NewStore(pool)
	// session_turns is partitioned: the first partitions are the current month and the next three,
	// so the sessions of a test live in the middle of next month
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month()+1, 5, 10, 0, 0, 0, time.UTC)
	return &learnerRig{
		t: t, pool: pool, store: store, content: testutil.SeedContent(t, pool), base: base,
		cons: learner.NewConsumer(store.LearnerStore(), store.LearnerUserGate(), clock.System{}, idgen.NewV7(), nil, nil),
		svc:  learner.NewService(learner.Deps{Store: store.LearnerStore(), Clock: clock.System{}, IDs: idgen.NewV7()}),
	}
}

func (r *learnerRig) user(level string) uuid.UUID {
	r.t.Helper()
	uid := testutil.SeedUser(r.t, r.pool, nil)
	if level != "" {
		_, err := r.svc.PatchProfile(context.Background(), uid, learner.PatchInput{SelfDeclaredLevel: lset(level)})
		require.NoError(r.t, err)
	}
	return uid
}

type learnerEval struct {
	endedAt  time.Time
	nTurns   int
	adaptive string
	scores   [4]int // GRAMMAR, VOCABULARY, FLUENCY, NATURALNESS
	conf     map[string]float64
	status   string
}

// eval seeds a COMPLETED session of userID with nTurns valid turns and its evaluation; it returns
// the evaluation and session ids.
func (r *learnerRig) eval(userID uuid.UUID, e learnerEval) (evID, sessionID uuid.UUID) {
	r.t.Helper()
	ctx := context.Background()
	started := e.endedAt.Add(-400 * time.Second)
	if e.adaptive == "" {
		e.adaptive = `{}`
	}
	sessionID = testutil.SeedSession(r.t, r.pool, userID, r.content, testutil.Row{
		"status": "COMPLETED", "end_reason": "USER_ENDED", "starting_at": started.Add(-2 * time.Second), "started_at": started,
		"expires_at": started.Add(300 * time.Second), "ending_at": e.endedAt, "ended_at": e.endedAt,
		"charged_seconds": 240, "adaptive_difficulty": []byte(e.adaptive),
	})
	for i := 0; i < e.nTurns; i++ {
		created := started.Add(time.Duration(i+1) * time.Second)
		testutil.MustInsert(ctx, r.t, r.pool, "session_turns", testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "session_id": sessionID, "sequence_number": i + 1, "status": "COMPLETED",
			"user_text": "今日は天気がいいですね。", "assistant_text": "そうですね。", "generation": 0,
			"user_started_at": created, "user_ended_at": created.Add(time.Second), "created_at": created, "completed_at": created,
		})
	}
	conf, err := json.Marshal(e.conf)
	require.NoError(r.t, err)
	status := e.status
	if status == "" {
		status = "COMPLETED"
	}
	evID = uuid.Must(uuid.NewV7())
	row := testutil.Row{
		"id": evID, "session_id": sessionID, "user_id": userID, "status": status, "rubric_version": "rubric-v1",
		"source_event_id": uuid.Must(uuid.NewV7()), "grammar_score": e.scores[0], "vocabulary_score": e.scores[1],
		"fluency_score": e.scores[2], "naturalness_score": e.scores[3], "task_achievement_score": 70, "overall_score": 70.0,
		"dimension_confidence": conf, "started_at": e.endedAt,
	}
	switch status {
	case "COMPLETED":
		row["completed_at"] = e.endedAt
	case "FAILED":
		row["failure_code"] = "INTERNAL_ERROR"
	}
	testutil.MustInsert(ctx, r.t, r.pool, "evaluations", row)
	return evID, sessionID
}

func (r *learnerRig) delivery(userID, evID, sessionID uuid.UUID) events.Delivery {
	r.t.Helper()
	ev := events.NewEvaluationCompleted(uuid.Must(uuid.NewV7()), time.Now().UTC(), "evaluation", userID, events.EvaluationCompletedV1{
		EvaluationID: evID, SessionID: sessionID, RubricVersion: "rubric-v1", CompletedAt: time.Now().UTC(),
	})
	env, err := ev.Envelope("", nil)
	require.NoError(r.t, err)
	return events.Delivery{Envelope: env, Queue: events.QueueLearner}
}

func (r *learnerRig) handle(d events.Delivery) error {
	return r.cons.Handle(context.Background(), d)
}

type skillRow struct {
	Level, Confidence float64
	Ceiling           *float64
	Trend             string
	Count             int
	LastEval          *uuid.UUID
	LastAt            *time.Time
}

func (r *learnerRig) skill(userID uuid.UUID, skill string) skillRow {
	r.t.Helper()
	var s skillRow
	require.NoError(r.t, r.pool.QueryRow(context.Background(), `
		SELECT estimated_level::float8, confidence::float8, demonstrated_ceiling::float8, trend, evidence_count,
		       last_evaluation_id, last_evidence_at
		  FROM learner_skill_states WHERE user_id = $1 AND skill = $2`, userID, skill).
		Scan(&s.Level, &s.Confidence, &s.Ceiling, &s.Trend, &s.Count, &s.LastEval, &s.LastAt))
	return s
}

// dump is every learner row of the user that a replay can touch, in a stable order and without
// the ids that differ per run.
func (r *learnerRig) dump(userID uuid.UUID) string {
	r.t.Helper()
	ctx := context.Background()
	var out string
	rows, err := r.pool.Query(ctx, `
		SELECT skill, estimated_level::text, confidence::text, demonstrated_ceiling::text, trend, evidence_count::text,
		       last_evidence_at::text, prior_level::text, prior_confidence::text
		  FROM learner_skill_states WHERE user_id = $1 ORDER BY skill`, userID)
	require.NoError(r.t, err)
	for rows.Next() {
		var a, b, c, e, f, h, i string
		var d, g *string
		require.NoError(r.t, rows.Scan(&a, &b, &c, &d, &e, &f, &g, &h, &i))
		out += fmt.Sprintf("S %s %s %s %v %s %s %v %s %s\n", a, b, c, deref(d), e, f, deref(g), h, i)
	}
	rows.Close()
	est, err := r.pool.Query(ctx, `
		SELECT skill, assessed_at::text, raw_score::text, effective_difficulty::text, evidence_level::text, evidence_confidence::text,
		       previous_level::text, estimated_level::text, confidence::text, evidence_count::text, trend
		  FROM proficiency_estimates WHERE user_id = $1 ORDER BY skill, assessed_at`, userID)
	require.NoError(r.t, err)
	for est.Next() {
		v := make([]string, 11)
		ptrs := make([]any, 11)
		for i := range v {
			ptrs[i] = &v[i]
		}
		var deff *string
		ptrs[3] = &deff
		require.NoError(r.t, est.Scan(ptrs...))
		out += fmt.Sprintf("E %v %v\n", v, deref(deff))
	}
	est.Close()
	require.NoError(r.t, est.Err())
	return out
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func (r *learnerRig) count(sql string, args ...any) int {
	r.t.Helper()
	return count(r.t, r.pool, sql, args...)
}

// goldenEvals are the three sessions of spec 12.5.4 for GRAMMAR (the other skills get scores too).
func (r *learnerRig) goldenEvals(userID uuid.UUID) [3]events.Delivery {
	r.t.Helper()
	specs := []learnerEval{
		{endedAt: r.base, nTurns: 15, scores: [4]int{82, 75, 70, 65}, conf: map[string]float64{"GRAMMAR": 0.80},
			adaptive: `{"grammar_complexity":2,"vocabulary_complexity":2,"expected_response_length":2,"scenario_difficulty":3}`},
		{endedAt: r.base.AddDate(0, 0, 3), nTurns: 9, scores: [4]int{91, 75, 70, 65}, conf: map[string]float64{"GRAMMAR": 0.85},
			adaptive: `{"grammar_complexity":2,"vocabulary_complexity":2,"expected_response_length":2,"scenario_difficulty":3}`},
		{endedAt: r.base.AddDate(0, 0, 7), nTurns: 14, scores: [4]int{58, 75, 70, 65}, conf: map[string]float64{"GRAMMAR": 0.80},
			adaptive: `{"grammar_complexity":3,"vocabulary_complexity":2,"expected_response_length":2,"scenario_difficulty":4}`},
	}
	var out [3]events.Delivery
	for i, s := range specs {
		ev, sid := r.eval(userID, s)
		out[i] = r.delivery(userID, ev, sid)
	}
	return out
}

func TestM8Learner_EvaluationCompleted_ChangesSkillState(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	ev, sid := r.eval(uid, learnerEval{
		endedAt: r.base, nTurns: 15, scores: [4]int{82, 80, 78, 76}, conf: map[string]float64{"GRAMMAR": 0.80},
		adaptive: `{"grammar_complexity":2}`,
	})
	d := r.delivery(uid, ev, sid)
	require.NoError(t, r.handle(d))

	g := r.skill(uid, "GRAMMAR")
	require.Equal(t, 2.16, g.Level)
	require.Equal(t, 0.280, g.Confidence)
	require.Equal(t, 1, g.Count)
	require.Equal(t, 2.16, *g.Ceiling)
	require.Equal(t, "INSUFFICIENT_DATA", g.Trend)
	require.Equal(t, ev, *g.LastEval)
	require.True(t, g.LastAt.Equal(r.base))
	require.Equal(t, 4, r.count(`SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, uid), "one row per scored skill")
	require.Zero(t, r.skill(uid, "LISTENING_RESPONSE").Count, "a skill without a signal is untouched")
	require.Equal(t, 1, r.count(`SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, events.QueueLearner, d.Envelope.EventID))
}

func TestM8Learner_GoldenThreeSessions_ViaConsumer(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	d := r.goldenEvals(uid)
	want := []struct {
		level, conf float64
		trend       string
	}{{2.16, 0.280, "INSUFFICIENT_DATA"}, {2.27, 0.389, "INSUFFICIENT_DATA"}, {2.35, 0.502, "IMPROVING"}}
	for i := range d {
		require.NoError(t, r.handle(d[i]))
		g := r.skill(uid, "GRAMMAR")
		require.Equal(t, want[i].level, g.Level, "session %d", i+1)
		require.Equal(t, want[i].conf, g.Confidence, "session %d", i+1)
		require.Equal(t, want[i].trend, g.Trend, "session %d", i+1)
		require.Equal(t, i+1, g.Count)
	}
}

func TestM8Learner_OutOfOrder_AllPermutations(t *testing.T) {
	r := newLearnerRig(t)
	ordered := r.user("N4")
	for _, d := range r.goldenEvals(ordered) {
		require.NoError(t, r.handle(d))
	}
	want := r.dump(ordered)
	require.Contains(t, want, "IMPROVING")

	for _, p := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		uid := r.user("N4")
		d := r.goldenEvals(uid)
		for _, i := range p {
			require.NoError(t, r.handle(d[i]), "order %v", p)
		}
		require.Equal(t, want, r.dump(uid), "order %v", p)
		g := r.skill(uid, "GRAMMAR")
		require.True(t, g.LastAt.Equal(r.base.AddDate(0, 0, 7)), "last_evidence_at is the end of the latest session")
	}
}

func TestM8Learner_ReplayTwice_NoChange(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	d := r.goldenEvals(uid)
	for _, i := range []int{2, 0, 1} {
		require.NoError(t, r.handle(d[i]))
	}
	before := r.dump(uid)
	for _, dl := range d {
		require.ErrorIs(t, r.handle(dl), events.ErrAlreadyProcessed)
		// the same evaluation announced again under a new event id: ON CONFLICT keeps it counted once
		again := r.delivery(uid, dl.Envelope.AggregateID, uuid.Must(uuid.NewV7()))
		require.NoError(t, r.handle(again))
	}
	require.Equal(t, before, r.dump(uid))
}

func TestM8Learner_DuplicateEvent_SingleEffect(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	d := r.goldenEvals(uid)[0]
	require.NoError(t, r.handle(d))
	require.ErrorIs(t, r.handle(d), events.ErrAlreadyProcessed)
	require.Equal(t, 1, r.skill(uid, "GRAMMAR").Count)
	require.Equal(t, 1, r.count(`SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, events.QueueLearner, d.Envelope.EventID))

	// parallel copies of one event: one effect
	uid2 := r.user("N4")
	d2 := r.goldenEvals(uid2)[0]
	errsC := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errsC <- r.handle(d2) }()
	}
	wg.Wait()
	close(errsC)
	ok := 0
	for err := range errsC {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, events.ErrAlreadyProcessed)
			require.Equal(t, amqp.ClassAck, amqp.Classify(err, false))
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 1, r.skill(uid2, "GRAMMAR").Count)
	require.Equal(t, 2.16, r.skill(uid2, "GRAMMAR").Level)
}

func TestM8Learner_ConcurrentSameUser_Serialized(t *testing.T) {
	r := newLearnerRig(t)
	serial := r.user("N4")
	for _, d := range r.goldenEvals(serial) {
		require.NoError(t, r.handle(d))
	}
	want := r.dump(serial)

	for round := 0; round < 4; round++ {
		uid := r.user("N4")
		d := r.goldenEvals(uid)
		var wg sync.WaitGroup
		errsC := make(chan error, 3)
		start := make(chan struct{})
		for i := range d {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errsC <- r.handle(d[2-i]) // newest first: the others arrive out of order
			}()
		}
		close(start)
		wg.Wait()
		close(errsC)
		for err := range errsC {
			require.NoError(t, err)
		}
		require.Equal(t, want, r.dump(uid), "round %d", round)
	}
}

func TestM8Learner_DeletedUser_AckedWithoutWrites(t *testing.T) {
	r := newLearnerRig(t)
	uid := testutil.SeedUser(t, r.pool, testutil.Row{"status": "DELETED", "deleted_at": time.Now().UTC()})
	ev, sid := r.eval(uid, learnerEval{endedAt: r.base, nTurns: 15, scores: [4]int{82, 80, 78, 76}})
	d := r.delivery(uid, ev, sid)
	err := r.handle(d)
	require.ErrorIs(t, err, events.ErrSkippedDeletedUser)
	require.Equal(t, amqp.ClassAck, amqp.Classify(err, false))
	require.Equal(t, 1, r.count(`SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, events.QueueLearner, d.Envelope.EventID))
	require.Zero(t, r.count(`SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, uid))
	require.Zero(t, r.count(`SELECT count(*) FROM learner_profiles WHERE user_id = $1`, uid))
	require.Zero(t, r.count(`SELECT count(*) FROM learner_skill_states WHERE user_id = $1`, uid))
}

func TestM8Learner_UserMismatch_DeadLettered(t *testing.T) {
	r := newLearnerRig(t)
	victim, attacker := r.user("N4"), r.user("N4")
	ev, sid := r.eval(victim, learnerEval{endedAt: r.base, nTurns: 15, scores: [4]int{82, 80, 78, 76}})
	before := r.dump(victim)
	d := r.delivery(attacker, ev, sid) // the event claims another user than the evaluation's
	err := r.handle(d)
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
	require.Equal(t, amqp.ClassDead, amqp.Classify(err, false))
	require.Zero(t, r.count(`SELECT count(*) FROM processed_events WHERE event_id = $1`, d.Envelope.EventID), "rolled back")
	require.Equal(t, before, r.dump(victim))
	require.Zero(t, r.count(`SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, attacker))
}

func TestM8Learner_EvaluationNotCommitted_Retryable(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	d := r.delivery(uid, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	err := r.handle(d)
	require.True(t, errors.Is(err, learner.ErrEvaluationNotVisible), "%v", err)
	require.Equal(t, amqp.ClassRetry, amqp.Classify(err, false))
	require.Zero(t, r.count(`SELECT count(*) FROM processed_events WHERE event_id = $1`, d.Envelope.EventID))
}

func TestM8Learner_EvaluationNotCompleted_DeadLettered(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	ev, sid := r.eval(uid, learnerEval{endedAt: r.base, nTurns: 15, scores: [4]int{82, 80, 78, 76}, status: "FAILED"})
	err := r.handle(r.delivery(uid, ev, sid))
	require.Equal(t, amqp.ClassDead, amqp.Classify(err, false), "%v", err)
	require.Zero(t, r.skill(uid, "GRAMMAR").Count)
}

func TestM8Learner_FewTurns_ProcessedWithoutUpdate(t *testing.T) {
	r := newLearnerRig(t)
	uid := r.user("N4")
	ev, sid := r.eval(uid, learnerEval{endedAt: r.base, nTurns: 3, scores: [4]int{82, 80, 78, 76}})
	d := r.delivery(uid, ev, sid)
	require.NoError(t, r.handle(d))
	require.Equal(t, 1, r.count(`SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, events.QueueLearner, d.Envelope.EventID))
	require.Zero(t, r.count(`SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`, uid))
}

func TestM8Learner_UserGate(t *testing.T) {
	r := newLearnerRig(t)
	ctx := context.Background()
	gate := r.store.LearnerUserGate()
	active := testutil.SeedUser(t, r.pool, nil)
	suspended := testutil.SeedUser(t, r.pool, testutil.Row{"status": "SUSPENDED"})
	deleted := testutil.SeedUser(t, r.pool, testutil.Row{"status": "DELETED", "deleted_at": time.Now().UTC()})
	for id, want := range map[uuid.UUID]bool{active: false, suspended: false, deleted: true, uuid.Must(uuid.NewV7()): true} {
		got, err := gate.UserDeleted(ctx, id)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

// TestM8Learner_WaitsForUserAdvisoryLock holds the learner advisory lock of one user in another
// transaction: that user's event waits for it, another user's event does not.
func TestM8Learner_WaitsForUserAdvisoryLock(t *testing.T) {
	r := newLearnerRig(t)
	ctx := context.Background()
	locked, free := r.user("N4"), r.user("N4")
	dl := r.goldenEvals(locked)[0]
	df := r.goldenEvals(free)[0]

	done := make(chan error, 1)
	err := r.store.LearnerStore().WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		if err := tx.Profiles().LockUser(ctx, locked); err != nil {
			return err
		}
		go func() { done <- r.handle(dl) }()
		require.NoError(t, r.handle(df), "another user is not blocked by the lock")
		select {
		case err := <-done:
			t.Errorf("the event of the locked user ran under the lock: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		require.Zero(t, r.skill(locked, "GRAMMAR").Count)
		return nil
	})
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the event did not proceed after the lock was released")
	}
	require.Equal(t, 1, r.skill(locked, "GRAMMAR").Count)
	require.Equal(t, 1, r.skill(free, "GRAMMAR").Count)
}
