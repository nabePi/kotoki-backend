package fakes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// EvaluationStore is an in-memory evaluation.Store and evaluation.Reader. WithinTx serializes
// callers and, when fn fails, restores the evaluations, drops the enqueued outbox events and
// forgets the processed_events rows written in the transaction (a rollback). The constraints of
// the evaluations tables that the contract relies on (completed scores, insufficient evidence,
// failure code, finding ordinals and type/dimension) are enforced the way the database does.
type EvaluationStore struct {
	FailInjector
	// Outbox records what transactions enqueued (committed transactions only).
	Outbox *Outbox
	// Processed records the idempotency rows of committed transactions.
	Processed *Processed

	mu       sync.Mutex
	clock    clock.Clock
	rows     map[uuid.UUID]evaluation.Evaluation // by session id
	findings map[uuid.UUID][]evaluation.FindingRow
}

var (
	_ evaluation.Store  = (*EvaluationStore)(nil)
	_ evaluation.Reader = (*EvaluationStore)(nil)
)

// EvaluationOption customises NewEvaluationStore.
type EvaluationOption func(*EvaluationStore)

// WithEvaluationClock sets the clock used for started_at, created_at and updated_at.
func WithEvaluationClock(c clock.Clock) EvaluationOption {
	return func(s *EvaluationStore) { s.clock = c }
}

// NewEvaluationStore returns an empty store.
func NewEvaluationStore(opts ...EvaluationOption) *EvaluationStore {
	s := &EvaluationStore{
		Outbox: &Outbox{}, Processed: NewProcessed(), clock: clock.System{},
		rows: map[uuid.UUID]evaluation.Evaluation{}, findings: map[uuid.UUID][]evaluation.FindingRow{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func cloneEvaluation(e evaluation.Evaluation) evaluation.Evaluation {
	e.DimensionConfidence = maps.Clone(e.DimensionConfidence)
	e.DimensionComments = maps.Clone(e.DimensionComments)
	e.Strengths = slices.Clone(e.Strengths)
	e.FocusAreas = slices.Clone(e.FocusAreas)
	e.Recommendations = slices.Clone(e.Recommendations)
	return e
}

type evalState struct {
	rows     map[uuid.UUID]evaluation.Evaluation
	findings map[uuid.UUID][]evaluation.FindingRow
}

func (s *EvaluationStore) snapshot() evalState {
	st := evalState{rows: make(map[uuid.UUID]evaluation.Evaluation, len(s.rows)), findings: make(map[uuid.UUID][]evaluation.FindingRow, len(s.findings))}
	for k, v := range s.rows {
		st.rows[k] = cloneEvaluation(v)
	}
	for k, v := range s.findings {
		st.findings[k] = slices.Clone(v)
	}
	return st
}

// WithinTx implements evaluation.Store.
func (s *EvaluationStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx evaluation.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.snapshot()
	queued := s.Outbox.Len()
	tx := &evalTx{s: s}
	if err := fn(ctx, tx); err != nil {
		s.rows, s.findings = before.rows, before.findings
		s.Outbox.Truncate(queued)
		for _, k := range tx.marked {
			s.Processed.Forget(k.consumer, k.id)
		}
		return err
	}
	return nil
}

// GetBySession implements evaluation.Reader.
func (s *EvaluationStore) GetBySession(_ context.Context, sessionID uuid.UUID) (evaluation.Evaluation, bool, error) {
	if err := s.Take("GetBySession"); err != nil {
		return evaluation.Evaluation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[sessionID]
	return cloneEvaluation(e), ok, nil
}

// ListFindings implements evaluation.Reader.
func (s *EvaluationStore) ListFindings(_ context.Context, evaluationID uuid.UUID) ([]evaluation.FindingRow, error) {
	if err := s.Take("ListFindings"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.findings[evaluationID])
	slices.SortFunc(out, func(a, b evaluation.FindingRow) int { return a.Ordinal - b.Ordinal })
	return out, nil
}

// Evaluation returns the stored row of sessionID (false when none).
func (s *EvaluationStore) Evaluation(sessionID uuid.UUID) (evaluation.Evaluation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[sessionID]
	return cloneEvaluation(e), ok
}

type processedKey2 struct {
	consumer string
	id       uuid.UUID
}

type evalTx struct {
	s      *EvaluationStore
	marked []processedKey2
}

func (t *evalTx) Evaluations() evaluation.Repository { return &evalRepo{s: t.s} }
func (t *evalTx) Processed() events.ProcessedEvents  { return &evalProcessed{tx: t} }
func (t *evalTx) Outbox() events.OutboxWriter        { return t.s.Outbox }

type evalProcessed struct{ tx *evalTx }

func (p *evalProcessed) MarkProcessed(ctx context.Context, consumer string, ev events.Envelope) error {
	if err := p.tx.s.Processed.MarkProcessed(ctx, consumer, ev); err != nil {
		return err
	}
	p.tx.marked = append(p.tx.marked, processedKey2{consumer, ev.EventID})
	return nil
}

type evalRepo struct{ s *EvaluationStore }

func (r *evalRepo) Claim(_ context.Context, in evaluation.ClaimInput) (evaluation.ClaimResult, error) {
	if err := r.s.Take("Claim"); err != nil {
		return evaluation.ClaimResult{}, err
	}
	now := r.s.clock.Now().UTC()
	e, ok := r.s.rows[in.SessionID]
	if !ok {
		e = evaluation.Evaluation{
			ID: in.ID, SessionID: in.SessionID, UserID: in.UserID, SourceEventID: in.SourceEventID,
			DimensionConfidence: map[string]float64{}, DimensionComments: map[string]string{},
			Strengths: []string{}, FocusAreas: []string{}, Recommendations: []evaluation.Recommendation{},
			CreatedAt: now,
		}
	} else if e.Status == evaluation.StatusCompleted {
		return evaluation.ClaimResult{}, nil
	}
	e.Status = evaluation.StatusRunning
	e.AttemptCount++
	e.StartedAt = &now
	e.FailureCode, e.LastError = nil, nil
	e.RubricVersion = in.RubricVersion
	if in.PromptVersionID != uuid.Nil {
		pv := in.PromptVersionID
		e.PromptVersionID = &pv
	} else {
		e.PromptVersionID = nil
	}
	e.UpdatedAt = now
	r.s.rows[in.SessionID] = e
	return evaluation.ClaimResult{ID: e.ID, Claimed: true, AttemptCount: e.AttemptCount}, nil
}

func (r *evalRepo) Complete(_ context.Context, id uuid.UUID, u evaluation.CompletedUpdate) (bool, error) {
	if err := r.s.Take("Complete"); err != nil {
		return false, err
	}
	if err := checkCompleted(u); err != nil {
		return false, err
	}
	for sid, e := range r.s.rows {
		if e.ID != id {
			continue
		}
		if e.Status == evaluation.StatusCompleted {
			return false, nil
		}
		at := u.CompletedAt.UTC()
		e.Status = evaluation.StatusCompleted
		e.InsufficientEvidence = u.InsufficientEvidence
		e.Scores, e.OverallScore = u.Scores, u.OverallScore
		e.ListeningResponseSignal, e.InteractionComplexitySignal = u.ListeningResponseSignal, u.InteractionComplexitySignal
		e.DimensionConfidence = nonNilMap(u.DimensionConfidence)
		e.DimensionComments = nonNilMap(u.DimensionComments)
		sf := u.SummaryFeedback
		e.SummaryFeedback = &sf
		e.Strengths, e.FocusAreas = nonNilSlice(u.Strengths), nonNilSlice(u.FocusAreas)
		e.Recommendations = nonNilSlice(u.Recommendations)
		e.UserTurnCount, e.UserSpeechSeconds = &u.UserTurnCount, &u.UserSpeechSeconds
		e.ActiveSeconds, e.ScenarioDifficultyLevel = &u.ActiveSeconds, &u.ScenarioDifficultyLevel
		e.Provider, e.Model, e.LLMCallCount = u.Provider, u.Model, u.LLMCallCount
		e.InputTokens, e.OutputTokens = u.InputTokens, u.OutputTokens
		e.FailureCode, e.LastError = nil, nil
		e.CompletedAt, e.UpdatedAt = &at, at
		r.s.rows[sid] = e
		return true, nil
	}
	return false, nil
}

func nonNilMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return map[string]V{}
	}
	return maps.Clone(m)
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}

// checkCompleted mirrors chk_evaluations_completed_scores and chk_evaluations_insufficient.
func checkCompleted(u evaluation.CompletedUpdate) error {
	scores := []*int{u.Scores.Grammar, u.Scores.Vocabulary, u.Scores.Fluency, u.Scores.Naturalness, u.Scores.TaskAchievement}
	for _, p := range append(slices.Clone(scores), u.ListeningResponseSignal, u.InteractionComplexitySignal) {
		if p != nil && (*p < 0 || *p > 100) {
			return errors.New("fake: chk_evaluations_scores violated")
		}
	}
	if u.OverallScore != nil && (*u.OverallScore < 0 || *u.OverallScore > 100) {
		return errors.New("fake: chk_evaluations_scores violated")
	}
	if u.InsufficientEvidence {
		if slices.ContainsFunc(scores, func(p *int) bool { return p != nil }) || u.OverallScore != nil {
			return errors.New("fake: chk_evaluations_insufficient violated")
		}
		return nil
	}
	if slices.Contains(scores, nil) || u.OverallScore == nil {
		return errors.New("fake: chk_evaluations_completed_scores violated")
	}
	return nil
}

func (r *evalRepo) ReplaceFindings(_ context.Context, evaluationID, _, _ uuid.UUID, fs []evaluation.FindingRow) error {
	if err := r.s.Take("ReplaceFindings"); err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, f := range fs {
		if seen[f.Ordinal] {
			return fmt.Errorf("fake: uq_evaluation_findings_ordinal violated (ordinal %d)", f.Ordinal)
		}
		seen[f.Ordinal] = true
		if err := checkFinding(f.Finding); err != nil {
			return err
		}
	}
	now := r.s.clock.Now().UTC()
	out := make([]evaluation.FindingRow, 0, len(fs))
	for _, f := range fs {
		if f.CreatedAt.IsZero() {
			f.CreatedAt = now
		}
		f.CreatedAt = f.CreatedAt.UTC()
		out = append(out, f)
	}
	r.s.findings[evaluationID] = out
	return nil
}

// checkFinding mirrors chk_ef_type_dimension and chk_ef_confidence.
func checkFinding(f evaluation.Finding) error {
	if f.Confidence < 0 || f.Confidence > 1 {
		return errors.New("fake: chk_ef_confidence violated")
	}
	if f.Type == evaluation.FindingStrength {
		if f.Severity != evaluation.SeverityInfo {
			return errors.New("fake: chk_ef_type_dimension violated")
		}
		return nil
	}
	if want, ok := evaluation.FindingDimension[f.Type]; !ok || want != f.Dimension {
		return errors.New("fake: chk_ef_type_dimension violated")
	}
	return nil
}

func (r *evalRepo) MarkPending(_ context.Context, sessionID uuid.UUID, lastError string) error {
	if err := r.s.Take("MarkPending"); err != nil {
		return err
	}
	e, ok := r.s.rows[sessionID]
	if !ok || e.Status != evaluation.StatusRunning {
		return nil
	}
	le := cutRunes(lastError, 2000)
	e.Status, e.LastError, e.UpdatedAt = evaluation.StatusPending, &le, r.s.clock.Now().UTC()
	r.s.rows[sessionID] = e
	return nil
}

func (r *evalRepo) MarkFailed(_ context.Context, sessionID uuid.UUID, code evaluation.FailureCode, lastError string) (evaluation.Evaluation, bool, error) {
	if err := r.s.Take("MarkFailed"); err != nil {
		return evaluation.Evaluation{}, false, err
	}
	e, ok := r.s.rows[sessionID]
	if !ok || (e.Status != evaluation.StatusRunning && e.Status != evaluation.StatusPending) {
		return evaluation.Evaluation{}, false, nil
	}
	le := cutRunes(lastError, 2000)
	c := code
	e.Status, e.FailureCode, e.LastError, e.UpdatedAt = evaluation.StatusFailed, &c, &le, r.s.clock.Now().UTC()
	r.s.rows[sessionID] = e
	return cloneEvaluation(e), true, nil
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
