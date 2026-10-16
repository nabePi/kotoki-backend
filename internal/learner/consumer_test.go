package learner_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var evalT0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

type consRig struct {
	t     *testing.T
	store *fakes.LearnerStore
	gate  *fakes.LearnerUserGate
	cons  *learner.Consumer
	reg   *prometheus.Registry
	user  uuid.UUID
}

func newConsRig(t *testing.T) *consRig {
	t.Helper()
	clk := clock.NewFake(evalT0.AddDate(0, 1, 0))
	store := fakes.NewLearnerStore(fakes.WithLearnerClock(clk))
	gate := fakes.NewLearnerUserGate()
	reg := prometheus.NewRegistry()
	return &consRig{
		t: t, store: store, gate: gate, reg: reg, user: newUser(),
		cons: learner.NewConsumer(store, gate, clk, idgen.NewSequence(t), nil, learner.NewMetrics(reg)),
	}
}

// onboard gives the rig's user the priors of level (the state an onboarded user starts from).
func (r *consRig) onboard(level string) {
	r.t.Helper()
	svc := learner.NewService(learner.Deps{Store: r.store, Clock: clock.NewFake(evalT0), IDs: idgen.NewSequence(r.t)})
	_, err := svc.PatchProfile(context.Background(), r.user, learner.PatchInput{SelfDeclaredLevel: learner.Field[string]{Set: true, Value: level}})
	require.NoError(r.t, err)
}

// evalSpec describes one evaluation; the zero value of a score is "no score".
type evalSpec struct {
	id        uuid.UUID
	user      uuid.UUID
	endedAt   time.Time
	nTurns    int
	grammar   *int
	vocab     *int
	fluency   *int
	natural   *int
	listening *int
	interact  *int
	conf      map[string]float64
	adaptive  string
	scenario  int
	status    string
}

func ip(v int) *int { return &v }

func evalID(n int) uuid.UUID {
	return uuid.MustParse("00000000-0000-0000-0000-00000000000" + string(rune('0'+n)))
}

// add registers the evidence of spec in the store and returns the delivery of its event.
func (r *consRig) add(s evalSpec) events.Delivery {
	r.t.Helper()
	if s.id == uuid.Nil {
		s.id = uuid.Must(uuid.NewV7())
	}
	if s.user == uuid.Nil {
		s.user = r.user
	}
	if s.status == "" {
		s.status = "COMPLETED"
	}
	if s.scenario == 0 {
		s.scenario = 3
	}
	if s.adaptive == "" {
		s.adaptive = `{}`
	}
	sid := uuid.Must(uuid.NewV7())
	r.store.SetEvidence(learner.EvaluationEvidence{
		EvaluationID: s.id, SessionID: sid, UserID: s.user, Status: s.status,
		Scores: map[learner.Skill]*int{
			learner.SkillGrammar: s.grammar, learner.SkillVocabulary: s.vocab, learner.SkillFluency: s.fluency, learner.SkillNaturalness: s.natural,
		},
		ListeningSignal: s.listening, InteractionSignal: s.interact, DimensionConfidence: s.conf,
		SessionEndedAt: s.endedAt, AdaptiveDifficulty: json.RawMessage(s.adaptive), ScenarioDifficulty: s.scenario, NTurns: s.nTurns,
	})
	return r.delivery(s.id, sid, s.user, uuid.Must(uuid.NewV7()))
}

func (r *consRig) delivery(evID, sessionID, user, eventID uuid.UUID) events.Delivery {
	r.t.Helper()
	ev := events.NewEvaluationCompleted(eventID, evalT0, "evaluation", user, events.EvaluationCompletedV1{
		EvaluationID: evID, SessionID: sessionID, RubricVersion: "rubric-v1", SessionEndedAt: evalT0, CompletedAt: evalT0,
	})
	env, err := ev.Envelope("00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", nil)
	require.NoError(r.t, err)
	return events.Delivery{Envelope: env, Queue: events.QueueLearner}
}

func (r *consRig) state(skill learner.Skill) learner.StateRow {
	r.t.Helper()
	for _, s := range r.store.States(r.user) {
		if s.Skill == skill {
			return s
		}
	}
	r.t.Fatalf("no state for %s", skill)
	return learner.StateRow{}
}

func (r *consRig) handle(d events.Delivery) error {
	return r.cons.Handle(context.Background(), d)
}

func (r *consRig) processed(d events.Delivery) bool {
	ok, err := r.store.Processed.IsProcessed(context.Background(), events.QueueLearner, d.Envelope.EventID)
	require.NoError(r.t, err)
	return ok
}

func TestConsumerContract(t *testing.T) {
	c := newConsRig(t).cons
	require.Equal(t, "learner.evaluation_completed", c.Queue())
	require.Equal(t, map[string]int{"evaluation.completed": 1}, c.SupportedEvents())
	require.Equal(t, 30*time.Second, c.Timeout())
}

func TestLearner_BrokenPayloadIsPermanentDecodeError(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 10, grammar: ip(80)})
	d.Envelope.Payload = json.RawMessage(`{"evaluation_id": 5`)
	var perm *events.PermanentError
	require.ErrorAs(t, r.handle(d), &perm)
	require.Equal(t, "DECODE_ERROR", perm.Code)
}

func TestLearner_NTurnsBelow4_NoUpdateButProcessed(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 3, grammar: ip(90), vocab: ip(90)})
	require.NoError(t, r.handle(d))
	require.True(t, r.processed(d))
	require.Empty(t, r.store.Estimates(r.user))
	for _, s := range r.store.States(r.user) {
		require.Zero(t, s.EvidenceCount, s.Skill)
	}
	require.Len(t, r.store.States(r.user), 6, "the profile is ensured even without evidence")
}

func TestLearner_NullSkillScore_NoRow(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80), listening: ip(70)})
	require.NoError(t, r.handle(d))
	got := map[learner.Skill]bool{}
	for _, e := range r.store.Estimates(r.user) {
		got[e.Skill] = true
	}
	require.Equal(t, map[learner.Skill]bool{learner.SkillGrammar: true, learner.SkillListeningResponse: true}, got)
	require.Zero(t, r.state(learner.SkillVocabulary).EvidenceCount, "a skill without a score keeps its state")
	require.Equal(t, 1, r.state(learner.SkillGrammar).EvidenceCount)
}

func TestLearner_InsufficientEvidenceEvaluation_NoRows(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12}) // every score and signal NULL
	require.NoError(t, r.handle(d))
	require.True(t, r.processed(d))
	require.Empty(t, r.store.Estimates(r.user))
}

func TestLearner_TinyEvidenceConfidenceSkipsSkill(t *testing.T) {
	r := newConsRig(t)
	// q_eval 0.10 x q_sample (4/12) = 0.033 < 0.05
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 4, grammar: ip(80), vocab: ip(80), conf: map[string]float64{"GRAMMAR": 0.10}})
	require.NoError(t, r.handle(d))
	ests := r.store.Estimates(r.user)
	require.Len(t, ests, 1, "only VOCABULARY (default q_eval 0.70 x 1/3 = 0.233) has evidence")
	require.Equal(t, learner.SkillVocabulary, ests[0].Skill)
}

func TestLearner_DeletedUserSkipped(t *testing.T) {
	r := newConsRig(t)
	r.gate.MarkDeleted(r.user)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80)})
	err := r.handle(d)
	require.ErrorIs(t, err, events.ErrSkippedDeletedUser)
	require.True(t, r.processed(d), "MarkProcessed alone is committed")
	require.Empty(t, r.store.Estimates(r.user))
	_, ok := r.store.Profile(r.user)
	require.False(t, ok, "no domain write for a deleted user")
	// a redelivery is simply acknowledged
	require.ErrorIs(t, r.handle(d), events.ErrAlreadyProcessed)
}

func TestLearner_UserGateErrorIsRetryable(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80)})
	boom := errors.New("connection reset")
	r.gate.FailNext("UserDeleted", boom)
	require.ErrorIs(t, r.handle(d), boom)
	require.False(t, r.processed(d))
	require.NoError(t, r.handle(d))
	require.Equal(t, 1, r.state(learner.SkillGrammar).EvidenceCount)
}

func TestLearner_EvaluationNotVisible_Retryable(t *testing.T) {
	r := newConsRig(t)
	d := r.delivery(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), r.user, uuid.Must(uuid.NewV7()))
	err := r.handle(d)
	require.ErrorIs(t, err, learner.ErrEvaluationNotVisible)
	var perm *events.PermanentError
	require.False(t, errors.As(err, &perm), "not visible yet is retried, never dead-lettered")
	require.False(t, r.processed(d), "the transaction rolled back the idempotency row")

	// the evaluation commits: the redelivery applies it
	d2 := r.add(evalSpec{id: d.Envelope.AggregateID, endedAt: evalT0, nTurns: 12, grammar: ip(80)})
	d2.Envelope = d.Envelope
	require.NoError(t, r.handle(d2))
	require.Equal(t, 1, r.state(learner.SkillGrammar).EvidenceCount)
}

func TestLearner_StatusNotCompleted_Permanent(t *testing.T) {
	for _, status := range []string{"FAILED", "RUNNING", "PENDING"} {
		r := newConsRig(t)
		d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80), status: status})
		var perm *events.PermanentError
		require.ErrorAs(t, r.handle(d), &perm, status)
		require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
		require.False(t, r.processed(d))
		require.Empty(t, r.store.Estimates(r.user))
	}
}

func TestLearner_UserMismatch_Permanent_DLQ(t *testing.T) {
	r := newConsRig(t)
	victim := newUser()
	// the evaluation belongs to the victim, the event claims r.user
	d := r.add(evalSpec{user: victim, endedAt: evalT0, nTurns: 12, grammar: ip(80)})
	d.Envelope.UserID = r.user
	var perm *events.PermanentError
	require.ErrorAs(t, r.handle(d), &perm)
	require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
	require.False(t, r.processed(d))
	require.Empty(t, r.store.Estimates(r.user))
	require.Empty(t, r.store.Estimates(victim), "nothing is written for the victim either")
	for _, s := range r.store.States(victim) {
		require.Zero(t, s.EvidenceCount)
	}
}

func TestLearner_SessionWithoutEndedAt_Permanent(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{nTurns: 12, grammar: ip(80)}) // zero endedAt
	var perm *events.PermanentError
	require.ErrorAs(t, r.handle(d), &perm)
	require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
}

func TestLearner_DEffFallbackToScenarioDifficulty(t *testing.T) {
	r := newConsRig(t)
	// scenario 4, adaptive object without grammar_complexity: D_eff = 4, s = 82 -> L_obs = 4 + 12/30 = 4.40
	// -> clamped to [2, 5]; vocabulary_complexity present (2): L_obs = 2 + 12/30 = 2.40
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(82), vocab: ip(82), scenario: 4, adaptive: `{"vocabulary_complexity":2}`})
	require.NoError(t, r.handle(d))
	by := map[learner.Skill]learner.EstimateRow{}
	for _, e := range r.store.Estimates(r.user) {
		by[e.Skill] = e
	}
	require.Equal(t, 4, *by[learner.SkillGrammar].DEff)
	require.Equal(t, 4.40, by[learner.SkillGrammar].LObs)
	require.Equal(t, 2, *by[learner.SkillVocabulary].DEff)
	require.Equal(t, 2.40, by[learner.SkillVocabulary].LObs)
}

func TestLearner_DEffMalformedAdaptiveFallsBack(t *testing.T) {
	for name, adaptive := range map[string]string{
		"not json": `nope`, "null": `null`, "string value": `{"grammar_complexity":"2"}`, "zero": `{"grammar_complexity":0}`,
		"six": `{"grammar_complexity":6}`,
	} {
		r := newConsRig(t)
		d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(70), scenario: 3, adaptive: adaptive})
		require.NoError(t, r.handle(d), name)
		require.Equal(t, 3, *r.store.Estimates(r.user)[0].DEff, name)
	}
}

func TestLearner_DEffListeningAndInteractionAveraging(t *testing.T) {
	r := newConsRig(t)
	adaptive := `{"speech_speed":2,"question_complexity":3,"follow_up_depth":4,"turn_taking_complexity":4,"scenario_difficulty":5,"grammar_complexity":1,"expected_response_length":2}`
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, listening: ip(70), interact: ip(70), adaptive: adaptive, scenario: 1})
	require.NoError(t, r.handle(d))
	by := map[learner.Skill]int{}
	for _, e := range r.store.Estimates(r.user) {
		by[e.Skill] = *e.DEff
	}
	require.Equal(t, 3, by[learner.SkillListeningResponse], "round_half_up((2+3)/2) = 3")
	require.Equal(t, 4, by[learner.SkillInteractionComplexity])

	// one of the two fields missing: the scenario difficulty
	r2 := newConsRig(t)
	d2 := r2.add(evalSpec{endedAt: evalT0, nTurns: 12, listening: ip(70), adaptive: `{"speech_speed":5}`, scenario: 2})
	require.NoError(t, r2.handle(d2))
	require.Equal(t, 2, *r2.store.Estimates(r2.user)[0].DEff)
}

func TestLearner_FluencyAndNaturalnessDifficulty(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, fluency: ip(70), natural: ip(70), scenario: 2,
		adaptive: `{"scenario_difficulty":5,"expected_response_length":4}`})
	require.NoError(t, r.handle(d))
	by := map[learner.Skill]int{}
	for _, e := range r.store.Estimates(r.user) {
		by[e.Skill] = *e.DEff
	}
	require.Equal(t, 4, by[learner.SkillFluency], "expected_response_length")
	require.Equal(t, 5, by[learner.SkillNaturalness], "scenario_difficulty of the adaptive object, not the scenario's")
}

func goldenSessions(r *consRig) [3]events.Delivery {
	return [3]events.Delivery{
		r.add(evalSpec{id: evalID(1), endedAt: evalT0, nTurns: 15, grammar: ip(82), conf: map[string]float64{"GRAMMAR": 0.80},
			scenario: 3, adaptive: `{"grammar_complexity":2}`}),
		r.add(evalSpec{id: evalID(2), endedAt: evalT0.AddDate(0, 0, 3), nTurns: 9, grammar: ip(91), conf: map[string]float64{"GRAMMAR": 0.85},
			scenario: 3, adaptive: `{"grammar_complexity":2}`}),
		r.add(evalSpec{id: evalID(3), endedAt: evalT0.AddDate(0, 0, 7), nTurns: 14, grammar: ip(58), conf: map[string]float64{"GRAMMAR": 0.80},
			scenario: 4, adaptive: `{"grammar_complexity":3}`}),
	}
}

type stateSnapshot struct {
	states    []learner.StateRow
	estimates []learner.EstimateRow
}

func (r *consRig) snapshot() stateSnapshot {
	s := stateSnapshot{states: r.store.States(r.user), estimates: r.store.Estimates(r.user)}
	// estimate and session ids differ with the delivery order; the rest of the row must not
	for i := range s.estimates {
		s.estimates[i].ID, s.estimates[i].SessionID = uuid.Nil, uuid.Nil
	}
	return s
}

func TestLearner_GoldenThreeSessions_InOrder(t *testing.T) {
	r := newConsRig(t)
	r.onboard("N4")
	d := goldenSessions(r)
	want := []struct {
		level, conf float64
		trend       learner.Trend
	}{{2.16, 0.280, learner.TrendInsufficient}, {2.27, 0.389, learner.TrendInsufficient}, {2.35, 0.502, learner.TrendImproving}}
	for i, dl := range d {
		require.NoError(t, r.handle(dl))
		st := r.state(learner.SkillGrammar)
		require.Equal(t, want[i].level, st.Level, "session %d", i+1)
		require.Equal(t, want[i].conf, st.Confidence, "session %d", i+1)
		require.Equal(t, want[i].trend, st.Trend, "session %d", i+1)
		require.Equal(t, i+1, st.EvidenceCount)
	}
	st := r.state(learner.SkillGrammar)
	require.Equal(t, evalID(3), *st.LastEvaluationID)
	require.True(t, st.LastEvidenceAt.Equal(evalT0.AddDate(0, 0, 7)))
	require.Equal(t, 2.35, *st.Ceiling)
}

func TestLearner_OutOfOrder_AllPermutations(t *testing.T) {
	inOrder := newConsRig(t)
	inOrder.onboard("N4")
	for _, d := range goldenSessions(inOrder) {
		require.NoError(t, inOrder.handle(d))
	}
	want := inOrder.snapshot()
	require.Equal(t, 2.35, want.states[0].Level)

	perms := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, p := range perms {
		r := newConsRig(t)
		r.onboard("N4")
		d := goldenSessions(r)
		for _, i := range p {
			require.NoError(t, r.handle(d[i]), "order %v", p)
		}
		got := r.snapshot()
		require.Equal(t, want.states, got.states, "order %v", p)
		require.Equal(t, want.estimates, got.estimates, "order %v: derived columns of every estimate", p)
		require.True(t, got.states[0].LastEvidenceAt.Equal(evalT0.AddDate(0, 0, 7)), "last_evidence_at is the latest session end")
		require.Equal(t, evalID(3), *got.states[0].LastEvaluationID)
	}
}

func TestLearner_OutOfOrder_CountsReplays(t *testing.T) {
	r := newConsRig(t)
	r.onboard("N4")
	d := goldenSessions(r)
	require.NoError(t, r.handle(d[2]))
	require.NoError(t, r.handle(d[0]))
	require.NoError(t, r.handle(d[1]))
	require.Equal(t, 2.0, testutil.ToFloat64(counterFor(t, r.reg, "GRAMMAR")), "two evaluations arrived late")
	// the in-order path never counts a replay
	r2 := newConsRig(t)
	for _, dl := range goldenSessions(r2) {
		require.NoError(t, r2.handle(dl))
	}
	require.Equal(t, 0.0, testutil.ToFloat64(counterFor(t, r2.reg, "GRAMMAR")))
}

func counterFor(t *testing.T, reg *prometheus.Registry, skill string) prometheus.Collector {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "x"})
	for _, mf := range mfs {
		if mf.GetName() != "kotoki_learner_replays_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "skill" && l.GetValue() == skill {
					c.Add(m.GetCounter().GetValue())
				}
			}
		}
	}
	return c
}

func TestLearner_SameEndedAtIsReplayedDeterministically(t *testing.T) {
	// two evaluations that ended at the same instant: either arrival order gives the same state
	mk := func(first int) stateSnapshot {
		r := newConsRig(t)
		a := r.add(evalSpec{id: evalID(1), endedAt: evalT0, nTurns: 12, grammar: ip(90), adaptive: `{"grammar_complexity":2}`})
		b := r.add(evalSpec{id: evalID(2), endedAt: evalT0, nTurns: 12, grammar: ip(50), adaptive: `{"grammar_complexity":2}`})
		order := []events.Delivery{a, b}
		if first == 1 {
			order = []events.Delivery{b, a}
		}
		for _, d := range order {
			require.NoError(t, r.handle(d))
		}
		return r.snapshot()
	}
	require.Equal(t, mk(0), mk(1))
}

func TestLearner_ReplayTwice_NoChange(t *testing.T) {
	r := newConsRig(t)
	r.onboard("N4")
	d := goldenSessions(r)
	require.NoError(t, r.handle(d[2]))
	require.NoError(t, r.handle(d[0]))
	require.NoError(t, r.handle(d[1]))
	before := r.snapshot()
	// redelivery of every event, then the same evaluations under new event ids
	for _, dl := range d {
		require.ErrorIs(t, r.handle(dl), events.ErrAlreadyProcessed)
	}
	for _, dl := range d {
		again := r.delivery(dl.Envelope.AggregateID, uuid.Must(uuid.NewV7()), r.user, uuid.Must(uuid.NewV7()))
		require.NoError(t, r.handle(again))
	}
	require.Equal(t, before, r.snapshot(), "a second event for an evaluation already counted changes nothing")
}

func TestLearner_DuplicateEvent_SingleEffect(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80), vocab: ip(80)})
	require.NoError(t, r.handle(d))
	before := r.snapshot()
	d.Redelivered = true
	require.ErrorIs(t, r.handle(d), events.ErrAlreadyProcessed)
	require.Equal(t, before, r.snapshot())
	require.Equal(t, 1, r.state(learner.SkillGrammar).EvidenceCount)
	require.Len(t, r.store.Estimates(r.user), 2)
}

func TestLearner_FailureRollsBackIdempotencyRow(t *testing.T) {
	for _, method := range []string{"InsertEstimate", "SaveState", "ListStatesForUpdate", "Ensure", "Evidence"} {
		r := newConsRig(t)
		d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80), vocab: ip(75)})
		boom := errors.New("connection reset by peer")
		r.store.FailNext(method, boom)
		err := r.handle(d)
		require.ErrorIs(t, err, boom, method)
		var perm *events.PermanentError
		require.False(t, errors.As(err, &perm), "%s: a database failure is retryable", method)
		require.False(t, r.processed(d), method)
		require.Empty(t, r.store.Estimates(r.user), method)
		for _, s := range r.store.States(r.user) {
			require.Zero(t, s.EvidenceCount, "%s: %s", method, s.Skill)
		}
		// the retry applies the event once
		require.NoError(t, r.handle(d), method)
		require.Equal(t, 1, r.state(learner.SkillGrammar).EvidenceCount, method)
		require.Len(t, r.store.Estimates(r.user), 2, method)
	}
}

func TestLearner_OutOfOrderFailureRollsBack(t *testing.T) {
	r := newConsRig(t)
	r.onboard("N4")
	d := goldenSessions(r)
	require.NoError(t, r.handle(d[2]))
	before := r.snapshot()
	boom := errors.New("deadlock detected")
	r.store.FailNext("UpdateDerived", boom)
	require.ErrorIs(t, r.handle(d[0]), boom)
	require.Equal(t, before, r.snapshot(), "the estimate inserted before the replay failed is rolled back")
	require.False(t, r.processed(d[0]))
	require.NoError(t, r.handle(d[0]))
	require.Equal(t, 2, r.state(learner.SkillGrammar).EvidenceCount)
}

func TestLearner_TakesUserLockBeforeAnyWrite(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80)})
	boom := errors.New("lock timeout")
	r.store.FailNext("LockUser", boom)
	require.ErrorIs(t, r.handle(d), boom)
	require.False(t, r.processed(d))
	_, ok := r.store.Profile(r.user)
	require.False(t, ok, "nothing is written before the user lock is held")
}

func TestLearner_OnFailureOnlyLogs(t *testing.T) {
	r := newConsRig(t)
	d := r.add(evalSpec{endedAt: evalT0, nTurns: 12, grammar: ip(80)})
	r.cons.OnFailure(context.Background(), d, errors.New("boom"), true)
	require.Empty(t, r.store.Estimates(r.user))
}
