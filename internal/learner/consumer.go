package learner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// consumerTimeout bounds one Handle call (spec 9.6.2).
const consumerTimeout = 30 * time.Second

// qSampleTurns is the number of valid turns that gives full sample confidence (spec 12.4).
const qSampleTurns = 12.0

// Codes of events.PermanentError raised by the consumer.
const codeInvariantViolation = "INVARIANT_VIOLATION"

// UserGate tells whether a user was deleted; the consumer acknowledges the events of such a user
// without any domain write (spec 9.7.2).
type UserGate interface {
	UserDeleted(ctx context.Context, userID uuid.UUID) (bool, error)
}

// Metrics are the learner collectors. All methods are safe on a nil *Metrics.
type Metrics struct{ replays *prometheus.CounterVec }

// NewMetrics registers the collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	return &Metrics{replays: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "kotoki_learner_replays_total", Help: "Out-of-order evaluations that replayed a skill, by skill.",
	}, []string{"skill"})}
}

// Replay counts one replay of skill.
func (m *Metrics) Replay(skill Skill) {
	if m != nil {
		m.replays.WithLabelValues(string(skill)).Inc()
	}
}

// Consumer is the queue handler of evaluation.completed. It satisfies amqp.Handler (spec 9.7.1)
// structurally, so this package imports events only, never amqp.
type Consumer struct {
	store   Store
	gate    UserGate
	clock   clock.Clock
	ids     idgen.Generator
	log     *slog.Logger
	metrics *Metrics
}

// NewConsumer returns the handler that applies the evidence of completed evaluations to the
// learner's skill states.
func NewConsumer(s Store, g UserGate, clk clock.Clock, ids idgen.Generator, log *slog.Logger, m *Metrics) *Consumer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Consumer{store: s, gate: g, clock: clk, ids: ids, log: log, metrics: m}
}

// Queue is the queue name and the consumer name in processed_events.
func (c *Consumer) Queue() string { return events.QueueLearner }

// SupportedEvents maps each handled event type to the highest version understood.
func (c *Consumer) SupportedEvents() map[string]int {
	return map[string]int{events.EvaluationCompleted: 1}
}

// Timeout bounds one Handle call.
func (c *Consumer) Timeout() time.Duration { return consumerTimeout }

// OnFailure only logs: the learner keeps no failure state.
func (c *Consumer) OnFailure(ctx context.Context, d events.Delivery, cause error, final bool) {
	c.log.WarnContext(ctx, "learner event failed",
		slog.String("event_id", d.Envelope.EventID.String()), slog.Int("retry_count", d.RetryCount),
		slog.Bool("final", final), slog.String("error", cause.Error()))
}

// Handle applies one evaluation.completed event (spec 12.5.3). Everything it writes, with the
// idempotency row, is one transaction under the user's advisory lock.
func (c *Consumer) Handle(ctx context.Context, d events.Delivery) error {
	env := d.Envelope
	p, err := events.DecodePayload[events.EvaluationCompletedV1](env)
	if err != nil {
		return err
	}
	deleted, err := c.gate.UserDeleted(ctx, env.UserID)
	if err != nil {
		return err
	}
	if deleted {
		err := c.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
			return tx.Processed().MarkProcessed(ctx, events.QueueLearner, env)
		})
		if err != nil {
			return err
		}
		return events.ErrSkippedDeletedUser
	}
	return c.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		return c.apply(ctx, tx, env, p)
	})
}

func (c *Consumer) apply(ctx context.Context, tx Tx, env events.Envelope, p events.EvaluationCompletedV1) error {
	userID := env.UserID
	if err := tx.Processed().MarkProcessed(ctx, events.QueueLearner, env); err != nil {
		return err
	}
	if err := tx.Profiles().LockUser(ctx, userID); err != nil {
		return err
	}
	ev, err := tx.Evaluations().Evidence(ctx, userID, p.EvaluationID)
	switch {
	case errors.Is(err, ErrNotFound):
		return c.mismatch(ctx, env, "evaluation or session belongs to another user", err)
	case err != nil:
		return err // includes ErrEvaluationNotVisible: retried until the evaluation is committed
	}
	if ev.Status != "COMPLETED" {
		return events.Permanent(codeInvariantViolation, errors.New("learner: evaluation is not COMPLETED"))
	}
	if ev.SessionEndedAt.IsZero() {
		return events.Permanent(codeInvariantViolation, errors.New("learner: evaluated session has no ended_at"))
	}
	if err := tx.Profiles().Ensure(ctx, userID); err != nil {
		return err
	}
	if ev.NTurns < MinTurnsForEvidence {
		return nil // too little evidence: processed, no update
	}
	states, err := tx.SkillStates().List(ctx, userID, true)
	if err != nil {
		return err
	}
	byskill := make(map[Skill]StateRow, len(states))
	for _, st := range states {
		byskill[st.Skill] = st
	}
	for _, skill := range AllSkills() {
		if err := c.applySkill(ctx, tx, userID, ev, skill, byskill[skill]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) mismatch(ctx context.Context, env events.Envelope, why string, cause error) error {
	c.log.ErrorContext(ctx, "learner security_mismatch",
		slog.String("event_id", env.EventID.String()), slog.String("reason", why))
	if cause == nil {
		cause = errors.New("learner: " + why)
	}
	return events.Permanent(codeInvariantViolation, cause)
}

func (c *Consumer) applySkill(ctx context.Context, tx Tx, userID uuid.UUID, ev EvaluationEvidence, skill Skill, st StateRow) error {
	score := ev.score(skill)
	if score == nil {
		return nil
	}
	qEval, ok := ev.DimensionConfidence[string(skill)]
	if !ok {
		qEval = DefaultEvalDimConfidence
	}
	qSample := min(1, float64(ev.NTurns)/qSampleTurns)
	cObs := EvidenceConfidence(qEval, qSample)
	if ShouldSkip(cObs) {
		return nil
	}
	dEff := ev.effectiveDifficulty(skill)
	est := Estimate{
		EvaluationID: ev.EvaluationID, AssessedAt: ev.SessionEndedAt, RawScore: *score, DEff: &dEff,
		LObs: EvidenceLevel(skill, *score, dEff), CObs: cObs,
	}
	row := EstimateRow{
		ID: c.ids.New(), EvaluationID: ev.EvaluationID, SessionID: ev.SessionID, Skill: skill, AlgorithmVersion: AlgorithmVersion,
		RawScore: float64(*score), DEff: &dEff, LObs: est.LObs, CObs: est.CObs, AssessedAt: est.AssessedAt,
	}

	if st.LastEvidenceAt == nil || ev.SessionEndedAt.After(*st.LastEvidenceAt) {
		return c.applyInOrder(ctx, tx, userID, ev, st, est, row)
	}
	return c.applyOutOfOrder(ctx, tx, userID, st, row)
}

// applyInOrder is the normal path: the evaluation is newer than every evidence of the skill.
func (c *Consumer) applyInOrder(ctx context.Context, tx Tx, userID uuid.UUID, ev EvaluationEvidence, st StateRow, est Estimate, row EstimateRow) error {
	past, err := tx.Estimates().ListBySkill(ctx, userID, st.Skill)
	if err != nil {
		return err
	}
	hist := make([]TrendPoint, len(past))
	for i, e := range past {
		hist[i] = TrendPoint{At: e.AssessedAt, Level: e.Level}
	}
	next, ap := Step(SkillState{
		Level: st.Level, Confidence: st.Confidence, Count: st.EvidenceCount, Ceiling: st.Ceiling, LastEvidenceAt: st.LastEvidenceAt,
	}, est, hist)
	row.Previous, row.Level, row.Confidence, row.Count, row.Trend = ap.Previous, ap.Level, ap.Confidence, ap.Count, ap.Trend
	inserted, err := tx.Estimates().Insert(ctx, userID, row)
	if err != nil || !inserted {
		return err // not inserted: this evaluation already counted (second idempotency layer)
	}
	evID := ev.EvaluationID
	return tx.SkillStates().Save(ctx, userID, StateRow{
		Skill: st.Skill, Level: next.Level, Confidence: next.Confidence, Ceiling: next.Ceiling, Trend: ap.Trend,
		EvidenceCount: next.Count, LastEvaluationID: &evID, LastEvidenceAt: next.LastEvidenceAt,
	}, c.clock.Now())
}

// applyOutOfOrder stores the estimate with placeholder derived columns and replays the skill from
// its prior over every estimate in (assessed_at, evaluation id) order (spec 12.5.5, S-19).
func (c *Consumer) applyOutOfOrder(ctx context.Context, tx Tx, userID uuid.UUID, st StateRow, row EstimateRow) error {
	row.Previous, row.Level, row.Confidence, row.Count, row.Trend = st.Level, st.Level, st.Confidence, st.EvidenceCount, TrendInsufficient
	inserted, err := tx.Estimates().Insert(ctx, userID, row)
	if err != nil || !inserted {
		return err
	}
	rows, err := tx.Estimates().ListBySkill(ctx, userID, st.Skill)
	if err != nil {
		return err
	}
	slices.SortStableFunc(rows, func(a, b EstimateRow) int {
		if cmp := a.AssessedAt.Compare(b.AssessedAt); cmp != 0 {
			return cmp
		}
		return compareUUID(a.EvaluationID, b.EvaluationID)
	})
	ests := make([]Estimate, len(rows))
	for i, r := range rows {
		ests[i] = Estimate{EvaluationID: r.EvaluationID, AssessedAt: r.AssessedAt, RawScore: int(r.RawScore), DEff: r.DEff, LObs: r.LObs, CObs: r.CObs}
	}
	final, applied := Replay(SkillState{Level: st.PriorLevel, Confidence: st.PriorConfidence}, ests)
	for i, r := range rows {
		a := applied[i]
		r.Previous, r.Level, r.Confidence, r.Count, r.Trend = a.Previous, a.Level, a.Confidence, a.Count, a.Trend
		if err := tx.Estimates().UpdateDerived(ctx, userID, r); err != nil {
			return err
		}
	}
	lastID := rows[len(rows)-1].EvaluationID
	err = tx.SkillStates().Save(ctx, userID, StateRow{
		Skill: st.Skill, Level: final.Level, Confidence: final.Confidence, Ceiling: final.Ceiling,
		Trend: applied[len(applied)-1].Trend, EvidenceCount: final.Count, LastEvaluationID: &lastID, LastEvidenceAt: final.LastEvidenceAt,
	}, c.clock.Now())
	if err != nil {
		return err
	}
	c.metrics.Replay(st.Skill)
	return nil
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// score is the 0-100 score of skill, or nil when the evaluation has none.
func (e EvaluationEvidence) score(skill Skill) *int {
	switch skill {
	case SkillListeningResponse:
		return e.ListeningSignal
	case SkillInteractionComplexity:
		return e.InteractionSignal
	default:
		return e.Scores[skill]
	}
}

// effectiveDifficulty is D_eff of spec 12.4 from the session's adaptive difficulty object. A field
// that is missing or outside 1..5 (an older algorithm version) falls back to the scenario
// difficulty.
func (e EvaluationEvidence) effectiveDifficulty(skill Skill) int {
	var f map[string]json.RawMessage
	if len(e.AdaptiveDifficulty) > 0 {
		_ = json.Unmarshal(e.AdaptiveDifficulty, &f) // a malformed object leaves f nil: every field falls back
	}
	get := func(key string) (int, bool) {
		var v int
		if err := json.Unmarshal(f[key], &v); err != nil || v < 1 || v > 5 {
			return 0, false
		}
		return v, true
	}
	avg := func(a, b string) int {
		x, okx := get(a)
		y, oky := get(b)
		if !okx || !oky {
			return e.ScenarioDifficulty
		}
		return roundHalfUp(float64(x+y) / 2)
	}
	single := func(key string) int {
		if v, ok := get(key); ok {
			return v
		}
		return e.ScenarioDifficulty
	}
	switch skill {
	case SkillGrammar:
		return single("grammar_complexity")
	case SkillVocabulary:
		return single("vocabulary_complexity")
	case SkillFluency:
		return single("expected_response_length")
	case SkillNaturalness:
		return single("scenario_difficulty")
	case SkillListeningResponse:
		return avg("speech_speed", "question_complexity")
	case SkillInteractionComplexity:
		return avg("follow_up_depth", "turn_taking_complexity")
	}
	return e.ScenarioDifficulty
}
