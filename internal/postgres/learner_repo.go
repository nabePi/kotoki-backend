package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// learnerRepo implements the four learner repository ports over one sqlc handle. Every query is
// scoped by the user_id it is given (db/queries/learner.sql); none reads across users.
type learnerRepo struct {
	q     *sqlcgen.Queries
	ids   idgen.Generator
	clock clock.Clock
}

var (
	_ learner.ProfileRepository    = (*learnerRepo)(nil)
	_ learner.SkillStateRepository = (*learnerRepo)(nil)
	_ learner.EstimateRepository   = (*learnerRepo)(nil)
	_ learner.EvaluationReader     = (*learnerRepo)(nil)
	_ learner.Reader               = (*learnerRepo)(nil)
)

// NewLearnerRepo returns the learner repositories over q (a pool or a transaction).
func NewLearnerRepo(q *sqlcgen.Queries, ids idgen.Generator, c clock.Clock) *learnerRepo { //nolint:revive // returned type is only used through the learner ports
	return &learnerRepo{q: q, ids: ids, clock: c}
}

// learnerTx adapts *Tx to learner.Tx. *Tx cannot satisfy the port itself: its Evaluations accessor
// already belongs to the evaluation feature and returns another type.
type learnerTx struct{ t *Tx }

var _ learner.Tx = learnerTx{}

func (x learnerTx) repo() *learnerRepo {
	return NewLearnerRepo(x.t.Queries(), x.t.IDs(), x.t.Clock())
}

func (x learnerTx) Profiles() learner.ProfileRepository       { return x.repo() }
func (x learnerTx) SkillStates() learner.SkillStateRepository { return x.repo() }
func (x learnerTx) Estimates() learner.EstimateRepository     { return x.repo() }
func (x learnerTx) Evaluations() learner.EvaluationReader     { return x.repo() }
func (x learnerTx) Processed() events.ProcessedEvents         { return x.t.Processed() }

type learnerStoreView struct{ s *Store }

var _ learner.Store = learnerStoreView{}

// LearnerStore returns the learner.Store view of s.
func (s *Store) LearnerStore() learner.Store { return learnerStoreView{s: s} }

func (v learnerStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx learner.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, learnerTx{t: tx}) })
}

func (v learnerStoreView) Reader() learner.Reader {
	return NewLearnerRepo(v.s.Queries(), v.s.ids, v.s.clock)
}

// NewLearnerReader returns the non-transactional learner reader over pool.
func NewLearnerReader(pool *pgxpool.Pool) learner.Reader {
	return NewLearnerRepo(sqlcgen.New(pool), idgen.NewV7(), clock.System{})
}

func num(v float64, scale int) pgtype.Numeric { return numScale(v, scale) }

func numFloat(n pgtype.Numeric) float64 {
	if p := numPtr(n); p != nil {
		return *p
	}
	return 0
}

func (r *learnerRepo) Ensure(ctx context.Context, userID uuid.UUID) error {
	at := tsz(r.clock.Now())
	if err := r.q.EnsureLearnerProfile(ctx, sqlcgen.EnsureLearnerProfileParams{UserID: userID, At: at}); err != nil {
		return internalErr(err, "ensure learner profile failed")
	}
	for _, k := range learner.AllSkills() {
		p := learner.PriorFor(nil, k)
		if err := r.q.EnsureLearnerSkillState(ctx, sqlcgen.EnsureLearnerSkillStateParams{
			ID: r.ids.New(), UserID: userID, Skill: string(k),
			PriorLevel: num(p.Level, 2), PriorConfidence: num(p.Confidence, 3), At: at,
		}); err != nil {
			return internalErr(err, "ensure learner skill state failed")
		}
	}
	return nil
}

func toProfile(p sqlcgen.LearnerProfile) learner.Profile {
	return learner.Profile{
		UserID: p.UserID, Nickname: fromText(p.Nickname), SelfDeclaredLevel: fromText(p.SelfDeclaredLevel),
		TargetLevel: fromText(p.TargetLevel), LearningGoal: fromText(p.LearningGoal), LearningGoalNote: fromText(p.LearningGoalNote),
		PreferredSpeechSpeed: p.PreferredSpeechSpeed, PreferredFormality: p.PreferredFormality, DialectPreference: p.DialectPreference,
		MemoryEnabled: p.MemoryEnabled, OnboardingCompletedAt: fromTszPtr(p.OnboardingCompletedAt),
		CreatedAt: fromTsz(p.CreatedAt), UpdatedAt: fromTsz(p.UpdatedAt),
	}
}

func (r *learnerRepo) Get(ctx context.Context, userID uuid.UUID) (learner.Profile, error) {
	p, err := r.q.GetLearnerProfile(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return learner.Profile{}, learner.ErrNotFound
	}
	if err != nil {
		return learner.Profile{}, internalErr(err, "get learner profile failed")
	}
	return toProfile(p), nil
}

func (r *learnerRepo) Update(ctx context.Context, userID uuid.UUID, row learner.ProfileRow) (learner.Profile, error) {
	p, err := r.q.UpdateLearnerProfile(ctx, sqlcgen.UpdateLearnerProfileParams{
		Nickname: textPtr(row.Nickname), SelfDeclaredLevel: textPtr(row.SelfDeclaredLevel), TargetLevel: textPtr(row.TargetLevel),
		LearningGoal: textPtr(row.LearningGoal), LearningGoalNote: textPtr(row.LearningGoalNote),
		PreferredSpeechSpeed: row.PreferredSpeechSpeed, PreferredFormality: row.PreferredFormality,
		DialectPreference: row.DialectPreference, MemoryEnabled: row.MemoryEnabled,
		OnboardingCompletedAt: tszPtr(row.OnboardingCompletedAt), At: tsz(r.clock.Now()), UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return learner.Profile{}, learner.ErrNotFound
	}
	if err != nil {
		return learner.Profile{}, internalErr(err, "update learner profile failed")
	}
	return toProfile(p), nil
}

func (r *learnerRepo) LockUser(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.LockLearnerUser(ctx, userID.String()); err != nil {
		return internalErr(err, "lock learner user failed")
	}
	return nil
}

func (r *learnerRepo) ResetPriors(ctx context.Context, userID uuid.UUID, level *string, now time.Time) error {
	for _, k := range learner.AllSkills() {
		p := learner.PriorFor(level, k)
		if _, err := r.q.UpdateLearnerSkillPrior(ctx, sqlcgen.UpdateLearnerSkillPriorParams{
			PriorLevel: num(p.Level, 2), PriorConfidence: num(p.Confidence, 3), At: tsz(now), UserID: userID, Skill: string(k),
		}); err != nil {
			return internalErr(err, "reset learner priors failed")
		}
	}
	return nil
}

func (r *learnerRepo) Stats(ctx context.Context, userID uuid.UUID) (learner.ProfileStats, error) {
	s, err := r.q.GetLearnerStats(ctx, userID)
	if err != nil {
		return learner.ProfileStats{}, internalErr(err, "get learner stats failed")
	}
	return learner.ProfileStats{
		CompletedSessions: int(s.CompletedSessions), PracticeSeconds: s.PracticeSeconds, LastSessionAt: fromTszPtr(s.LastSessionAt),
	}, nil
}

func toState(s sqlcgen.LearnerSkillState) learner.StateRow {
	return learner.StateRow{
		Skill: learner.Skill(s.Skill), PriorLevel: numFloat(s.PriorLevel), PriorConfidence: numFloat(s.PriorConfidence),
		Level: numFloat(s.EstimatedLevel), Confidence: numFloat(s.Confidence), Ceiling: numPtr(s.DemonstratedCeiling),
		Trend: learner.Trend(s.Trend), EvidenceCount: int(s.EvidenceCount),
		LastEvaluationID: fromNullUUID(s.LastEvaluationID), LastEvidenceAt: fromTszPtr(s.LastEvidenceAt),
	}
}

func (r *learnerRepo) List(ctx context.Context, userID uuid.UUID, forUpdate bool) ([]learner.StateRow, error) {
	var rows []sqlcgen.LearnerSkillState
	var err error
	if forUpdate {
		rows, err = r.q.ListLearnerSkillStatesForUpdate(ctx, userID)
	} else {
		rows, err = r.q.ListLearnerSkillStates(ctx, userID)
	}
	if err != nil {
		return nil, internalErr(err, "list learner skill states failed")
	}
	out := make([]learner.StateRow, len(rows))
	for i, s := range rows {
		out[i] = toState(s)
	}
	return out, nil
}

func numOpt(v *float64, scale int) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{}
	}
	return num(*v, scale)
}

func (r *learnerRepo) Save(ctx context.Context, userID uuid.UUID, row learner.StateRow, now time.Time) error {
	n, err := r.q.SaveLearnerSkillState(ctx, sqlcgen.SaveLearnerSkillStateParams{
		EstimatedLevel: num(row.Level, 2), DemonstratedCeiling: numOpt(row.Ceiling, 2), Confidence: num(row.Confidence, 3),
		Trend: string(row.Trend), EvidenceCount: int32(row.EvidenceCount), //nolint:gosec // G115: evidence count is small
		LastEvaluationID: nullUUID(row.LastEvaluationID), LastEvidenceAt: tszPtr(row.LastEvidenceAt),
		At: tsz(now), UserID: userID, Skill: string(row.Skill),
	})
	if err != nil {
		return internalErr(err, "save learner skill state failed")
	}
	if n == 0 {
		return learner.ErrNotFound
	}
	return nil
}

func (r *learnerRepo) Reset(ctx context.Context, userID uuid.UUID, now time.Time) error {
	if err := r.q.ResetLearnerSkillStates(ctx, sqlcgen.ResetLearnerSkillStatesParams{At: tsz(now), UserID: userID}); err != nil {
		return internalErr(err, "reset learner skill states failed")
	}
	return nil
}

func toEstimate(e sqlcgen.ProficiencyEstimate) learner.EstimateRow {
	return learner.EstimateRow{
		ID: e.ID, EvaluationID: e.SourceEvaluationID, SessionID: e.SessionID, Skill: learner.Skill(e.Skill),
		AlgorithmVersion: int(e.AlgorithmVersion), RawScore: numFloat(e.RawScore), DEff: fromInt2(e.EffectiveDifficulty),
		LObs: numFloat(e.EvidenceLevel), CObs: numFloat(e.EvidenceConfidence), AssessedAt: fromTsz(e.AssessedAt),
		Previous: numFloat(e.PreviousLevel), Level: numFloat(e.EstimatedLevel), Confidence: numFloat(e.Confidence),
		Count: int(e.EvidenceCount), Trend: learner.Trend(e.Trend),
	}
}

func (r *learnerRepo) Insert(ctx context.Context, userID uuid.UUID, row learner.EstimateRow) (bool, error) {
	n, err := r.q.InsertProficiencyEstimate(ctx, sqlcgen.InsertProficiencyEstimateParams{
		ID: row.ID, UserID: userID, Skill: string(row.Skill), SourceEvaluationID: row.EvaluationID, SessionID: row.SessionID,
		AlgorithmVersion: int16(row.AlgorithmVersion), //nolint:gosec // G115: a small constant
		RawScore:         num(row.RawScore, 2), EffectiveDifficulty: int2Ptr(row.DEff),
		EvidenceLevel: num(row.LObs, 2), EvidenceConfidence: num(row.CObs, 3), PreviousLevel: num(row.Previous, 2),
		EstimatedLevel: num(row.Level, 2), Confidence: num(row.Confidence, 3), EvidenceCount: int32(row.Count), //nolint:gosec // G115: small
		Trend: string(row.Trend), AssessedAt: tsz(row.AssessedAt), At: tsz(r.clock.Now()),
	})
	if err != nil {
		return false, internalErr(err, "insert proficiency estimate failed")
	}
	return n > 0, nil
}

func (r *learnerRepo) ListBySkill(ctx context.Context, userID uuid.UUID, skill learner.Skill) ([]learner.EstimateRow, error) {
	rows, err := r.q.ListProficiencyEstimatesBySkill(ctx, sqlcgen.ListProficiencyEstimatesBySkillParams{UserID: userID, Skill: string(skill)})
	if err != nil {
		return nil, internalErr(err, "list proficiency estimates failed")
	}
	out := make([]learner.EstimateRow, len(rows))
	for i, e := range rows {
		out[i] = toEstimate(e)
	}
	return out, nil
}

func (r *learnerRepo) UpdateDerived(ctx context.Context, userID uuid.UUID, row learner.EstimateRow) error {
	n, err := r.q.UpdateProficiencyEstimateDerived(ctx, sqlcgen.UpdateProficiencyEstimateDerivedParams{
		PreviousLevel: num(row.Previous, 2), EstimatedLevel: num(row.Level, 2), Confidence: num(row.Confidence, 3),
		EvidenceCount: int32(row.Count), Trend: string(row.Trend), ID: row.ID, UserID: userID, //nolint:gosec // G115: small
	})
	if err != nil {
		return internalErr(err, "update proficiency estimate failed")
	}
	if n == 0 {
		return learner.ErrNotFound
	}
	return nil
}

func (r *learnerRepo) DeleteAll(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.DeleteProficiencyEstimates(ctx, userID); err != nil {
		return internalErr(err, "delete proficiency estimates failed")
	}
	return nil
}

func (r *learnerRepo) History(ctx context.Context, userID uuid.UUID, skill learner.Skill, after *learner.HistoryCursor, limit int) ([]learner.EstimateRow, error) {
	p := sqlcgen.ListProficiencyHistoryParams{UserID: userID, Skill: string(skill), RowLimit: int32(limit)} //nolint:gosec // G115: page size is capped by the service
	if after != nil {
		p.AfterAt, p.AfterID = tsz(after.AssessedAt), uuid.NullUUID{UUID: after.ID, Valid: true}
	}
	rows, err := r.q.ListProficiencyHistory(ctx, p)
	if err != nil {
		return nil, internalErr(err, "list proficiency history failed")
	}
	out := make([]learner.EstimateRow, len(rows))
	for i, e := range rows {
		out[i] = toEstimate(e)
	}
	return out, nil
}

func (r *learnerRepo) Evidence(ctx context.Context, userID, evaluationID uuid.UUID) (learner.EvaluationEvidence, error) {
	row, err := r.q.GetLearnerEvidence(ctx, sqlcgen.GetLearnerEvidenceParams{EvaluationID: evaluationID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		exists, qerr := r.q.SysLearnerEvaluationExists(ctx, evaluationID)
		if qerr != nil {
			return learner.EvaluationEvidence{}, internalErr(qerr, "probe evaluation failed")
		}
		if exists {
			// somebody else's evaluation, or a session that is not the evaluation's user's
			return learner.EvaluationEvidence{}, errs.Wrap(learner.ErrNotFound, errs.CodeNotFound, fmt.Sprintf("evaluation %s", evaluationID))
		}
		return learner.EvaluationEvidence{}, learner.ErrEvaluationNotVisible
	}
	if err != nil {
		return learner.EvaluationEvidence{}, internalErr(err, "get learner evidence failed")
	}
	scores := map[learner.Skill]*int{
		learner.SkillGrammar: fromInt2(row.GrammarScore), learner.SkillVocabulary: fromInt2(row.VocabularyScore),
		learner.SkillFluency: fromInt2(row.FluencyScore), learner.SkillNaturalness: fromInt2(row.NaturalnessScore),
	}
	return learner.EvaluationEvidence{
		EvaluationID: row.ID, SessionID: row.SessionID, UserID: row.UserID, Status: row.Status, Scores: scores,
		ListeningSignal: fromInt2(row.ListeningResponseSignal), InteractionSignal: fromInt2(row.InteractionComplexitySignal),
		DimensionConfidence: unmarshalOr(row.DimensionConfidence, map[string]float64{}),
		SessionEndedAt:      fromTsz(row.SessionEndedAt), AdaptiveDifficulty: json.RawMessage(row.AdaptiveDifficulty),
		ScenarioDifficulty: int(row.ScenarioDifficulty), NTurns: int(row.NTurns),
	}, nil
}

func (r *learnerRepo) RecentFindings(ctx context.Context, userID uuid.UUID) (map[learner.Skill]learner.FindingStat, int, error) {
	rows, err := r.q.CountRecentFindingsBySkill(ctx, userID)
	if err != nil {
		return nil, 0, internalErr(err, "count recent findings failed")
	}
	stats := make(map[learner.Skill]learner.FindingStat, len(rows))
	for _, row := range rows {
		if k, ok := learner.ParseSkill(row.Dimension); ok { // TASK_ACHIEVEMENT is not a skill
			stats[k] = learner.FindingStat{Major: int(row.Major), Minor: int(row.Minor)}
		}
	}
	// The findings query yields no row for evaluations without findings, so the evaluation count
	// of its rows cannot stand alone.
	n, err := r.q.CountRecentLearnerEvaluations(ctx, userID)
	if err != nil {
		return nil, 0, internalErr(err, "count recent evaluations failed")
	}
	return stats, int(n), nil
}

func (r *learnerRepo) VoiceExposure(ctx context.Context, userID uuid.UUID, now time.Time) ([]learner.ExposureRow, error) {
	rows, err := r.q.GetVoiceExposure(ctx, sqlcgen.GetVoiceExposureParams{UserID: userID, Now: tsz(now)})
	if err != nil {
		return nil, internalErr(err, "get voice exposure failed")
	}
	out := make([]learner.ExposureRow, len(rows))
	for i, e := range rows {
		out[i] = learner.ExposureRow{Gender: e.Gender, Accent: e.Accent, Sessions: int(e.Sessions), Seconds: e.Seconds}
	}
	return out, nil
}

func (r *learnerRepo) TopicSessions90d(ctx context.Context, userID, topicID uuid.UUID, now time.Time) (int, error) {
	n, err := r.q.CountTopicSessions90d(ctx, sqlcgen.CountTopicSessions90dParams{UserID: userID, TopicID: topicID, Now: tsz(now)})
	if err != nil {
		return 0, internalErr(err, "count topic sessions failed")
	}
	return int(n), nil
}

func (r *learnerRepo) DialectSessions90d(ctx context.Context, userID uuid.UUID, now time.Time) (int, error) {
	n, err := r.q.CountDialectSessions90d(ctx, sqlcgen.CountDialectSessions90dParams{UserID: userID, Now: tsz(now)})
	if err != nil {
		return 0, internalErr(err, "count dialect sessions failed")
	}
	return int(n), nil
}
