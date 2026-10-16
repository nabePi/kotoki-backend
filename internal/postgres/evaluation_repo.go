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

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type evaluationRepo struct {
	q     *sqlcgen.Queries
	clock clock.Clock
}

var _ evaluation.Repository = (*evaluationRepo)(nil)

// Evaluations returns the transaction's evaluation repository. Together with Processed and Outbox
// the *Tx satisfies evaluation.Tx.
func (t *Tx) Evaluations() evaluation.Repository {
	return &evaluationRepo{q: t.Queries(), clock: t.Clock()}
}

var _ evaluation.Tx = (*Tx)(nil)

type evaluationStoreView struct{ s *Store }

// EvaluationStore returns the evaluation.Store view of s.
func (s *Store) EvaluationStore() evaluation.Store { return evaluationStoreView{s: s} }

func (v evaluationStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx evaluation.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, tx) })
}

// numScale converts v to a numeric with the given number of decimals (the column scale).
func numScale(v float64, scale int) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(fmt.Sprintf("%.*f", scale, v)) // a finite float always formats to a valid numeric
	return n
}

func numPtr(n pgtype.Numeric) *float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return nil
	}
	return &f.Float64
}

func int2Ptr(p *int) pgtype.Int2 {
	if p == nil {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: int16(*p), Valid: true} //nolint:gosec // G115: scores are 0-100 (CHECK)
}

func fromInt2(v pgtype.Int2) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int16)
	return &n
}

func int4Ptr(p *int) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*p), Valid: true} //nolint:gosec // G115: token and turn counts fit int32
}

func jsonOr(v any, empty string) []byte {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return []byte(empty)
	}
	return b
}

func unmarshalOr[T any](raw []byte, empty T) T {
	if len(raw) == 0 {
		return empty
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return empty
	}
	return out
}

func (r *evaluationRepo) Claim(ctx context.Context, in evaluation.ClaimInput) (evaluation.ClaimResult, error) {
	row, err := r.q.ClaimEvaluation(ctx, sqlcgen.ClaimEvaluationParams{
		ID: in.ID, SessionID: in.SessionID, UserID: in.UserID, RubricVersion: in.RubricVersion,
		PromptVersionID: uuid.NullUUID{UUID: in.PromptVersionID, Valid: in.PromptVersionID != uuid.Nil},
		SourceEventID:   in.SourceEventID, At: tsz(r.clock.Now()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.ClaimResult{}, nil // COMPLETED: the upsert's WHERE kept the row out
	}
	if err != nil {
		return evaluation.ClaimResult{}, internalErr(err, "claim evaluation failed")
	}
	return evaluation.ClaimResult{ID: row.ID, Claimed: true, AttemptCount: int(row.AttemptCount)}, nil
}

func (r *evaluationRepo) Complete(ctx context.Context, id uuid.UUID, u evaluation.CompletedUpdate) (bool, error) {
	var overall pgtype.Numeric
	if u.OverallScore != nil {
		overall = numScale(*u.OverallScore, 2)
	}
	n, err := r.q.CompleteEvaluation(ctx, sqlcgen.CompleteEvaluationParams{
		ID: id, InsufficientEvidence: u.InsufficientEvidence,
		GrammarScore: int2Ptr(u.Scores.Grammar), VocabularyScore: int2Ptr(u.Scores.Vocabulary), FluencyScore: int2Ptr(u.Scores.Fluency),
		NaturalnessScore: int2Ptr(u.Scores.Naturalness), TaskAchievementScore: int2Ptr(u.Scores.TaskAchievement),
		OverallScore: overall, ListeningResponseSignal: int2Ptr(u.ListeningResponseSignal),
		InteractionComplexitySignal: int2Ptr(u.InteractionComplexitySignal),
		DimensionConfidence:         jsonOr(u.DimensionConfidence, "{}"), DimensionComments: jsonOr(u.DimensionComments, "{}"),
		SummaryFeedback: u.SummaryFeedback, Strengths: jsonOr(u.Strengths, "[]"), FocusAreas: jsonOr(u.FocusAreas, "[]"),
		Recommendations: jsonOr(u.Recommendations, "[]"),
		UserTurnCount:   int32(u.UserTurnCount), UserSpeechSeconds: int32(u.UserSpeechSeconds), //nolint:gosec // G115: bounded by session length
		ActiveSeconds: int32(u.ActiveSeconds), ScenarioDifficultyLevel: int16(u.ScenarioDifficultyLevel), //nolint:gosec // G115: bounded
		Provider: textPtr(u.Provider), Model: textPtr(u.Model), LlmCallCount: int16(u.LLMCallCount), //nolint:gosec // G115: 0-2
		InputTokens: int4Ptr(u.InputTokens), OutputTokens: int4Ptr(u.OutputTokens), At: tsz(u.CompletedAt),
	})
	if err != nil {
		return false, internalErr(err, "complete evaluation failed")
	}
	return n > 0, nil
}

func (r *evaluationRepo) ReplaceFindings(ctx context.Context, evaluationID, userID, sessionID uuid.UUID, fs []evaluation.FindingRow) error {
	if err := r.q.DeleteEvaluationFindings(ctx, evaluationID); err != nil {
		return internalErr(err, "delete evaluation findings failed")
	}
	for _, f := range fs {
		created := f.CreatedAt
		if created.IsZero() {
			created = r.clock.Now()
		}
		err := r.q.InsertEvaluationFinding(ctx, sqlcgen.InsertEvaluationFindingParams{
			ID: f.ID, EvaluationID: evaluationID, SessionID: sessionID, UserID: userID,
			SessionTurnID: nullUUID(f.TurnID), TurnSequenceNumber: int4Ptr(f.TurnSeq), Ordinal: int16(f.Ordinal), //nolint:gosec // G115: at most 18
			Dimension: string(f.Dimension), FindingType: string(f.Type), Severity: string(f.Severity),
			OriginalText: textPtr(f.Original), CorrectedText: textPtr(f.Corrected), Explanation: f.Explanation,
			PatternKey: textPtr(f.PatternKey), Confidence: numScale(f.Confidence, 2), Source: f.Source, CreatedAt: tsz(created),
		})
		if err != nil {
			return internalErr(err, "insert evaluation finding failed")
		}
	}
	return nil
}

func (r *evaluationRepo) MarkPending(ctx context.Context, sessionID uuid.UUID, lastError string) error {
	_, err := r.q.MarkEvaluationPending(ctx, sqlcgen.MarkEvaluationPendingParams{
		SessionID: sessionID, LastError: lastError, At: tsz(r.clock.Now())})
	if err != nil {
		return internalErr(err, "mark evaluation pending failed")
	}
	return nil
}

func (r *evaluationRepo) MarkFailed(ctx context.Context, sessionID uuid.UUID, code evaluation.FailureCode, lastError string) (evaluation.Evaluation, bool, error) {
	row, err := r.q.MarkEvaluationFailed(ctx, sqlcgen.MarkEvaluationFailedParams{
		SessionID: sessionID, FailureCode: string(code), LastError: lastError, At: tsz(r.clock.Now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Evaluation{}, false, nil
	}
	if err != nil {
		return evaluation.Evaluation{}, false, internalErr(err, "mark evaluation failed failed")
	}
	return toEvaluation(row), true, nil
}

func toEvaluation(r sqlcgen.Evaluation) evaluation.Evaluation {
	e := evaluation.Evaluation{
		ID: r.ID, SessionID: r.SessionID, UserID: r.UserID, Status: evaluation.Status(r.Status), InsufficientEvidence: r.InsufficientEvidence,
		Scores: evaluation.Scores{
			Grammar: fromInt2(r.GrammarScore), Vocabulary: fromInt2(r.VocabularyScore), Fluency: fromInt2(r.FluencyScore),
			Naturalness: fromInt2(r.NaturalnessScore), TaskAchievement: fromInt2(r.TaskAchievementScore),
		},
		OverallScore: numPtr(r.OverallScore), ListeningResponseSignal: fromInt2(r.ListeningResponseSignal),
		InteractionComplexitySignal: fromInt2(r.InteractionComplexitySignal),
		DimensionConfidence:         unmarshalOr(r.DimensionConfidence, map[string]float64{}),
		DimensionComments:           unmarshalOr(r.DimensionComments, map[string]string{}),
		SummaryFeedback:             fromText(r.SummaryFeedback), Strengths: unmarshalOr(r.Strengths, []string{}),
		FocusAreas: unmarshalOr(r.FocusAreas, []string{}), Recommendations: unmarshalOr(r.Recommendations, []evaluation.Recommendation{}),
		UserTurnCount: fromInt4(r.UserTurnCount), UserSpeechSeconds: fromInt4(r.UserSpeechSeconds), ActiveSeconds: fromInt4(r.ActiveSeconds),
		ScenarioDifficultyLevel: fromInt2(r.ScenarioDifficultyLevel), RubricVersion: r.RubricVersion,
		PromptVersionID: fromNullUUID(r.PromptVersionID), Provider: fromText(r.Provider), Model: fromText(r.Model),
		LLMCallCount: int(r.LlmCallCount), InputTokens: fromInt4(r.InputTokens), OutputTokens: fromInt4(r.OutputTokens),
		AttemptCount: int(r.AttemptCount), LastError: fromText(r.LastError), SourceEventID: r.SourceEventID,
		DetailsPurgedAt: fromTszPtr(r.DetailsPurgedAt), CreatedAt: fromTsz(r.CreatedAt), StartedAt: fromTszPtr(r.StartedAt),
		CompletedAt: fromTszPtr(r.CompletedAt), UpdatedAt: fromTsz(r.UpdatedAt),
	}
	if r.FailureCode.Valid {
		c := evaluation.FailureCode(r.FailureCode.String)
		e.FailureCode = &c
	}
	return e
}

func toFindingRow(r sqlcgen.EvaluationFinding) evaluation.FindingRow {
	conf := 0.0
	if p := numPtr(r.Confidence); p != nil {
		conf = *p
	}
	return evaluation.FindingRow{
		ID: r.ID, Ordinal: int(r.Ordinal), CreatedAt: fromTsz(r.CreatedAt),
		Finding: evaluation.Finding{
			Type: evaluation.FindingType(r.FindingType), Dimension: evaluation.Dimension(r.Dimension), Severity: evaluation.Severity(r.Severity),
			TurnSeq: fromInt4(r.TurnSequenceNumber), TurnID: fromNullUUID(r.SessionTurnID),
			Original: fromText(r.OriginalText), Corrected: fromText(r.CorrectedText), PatternKey: fromText(r.PatternKey),
			Explanation: r.Explanation, Confidence: conf, Source: r.Source,
		},
	}
}

type evaluationReader struct{ q *sqlcgen.Queries }

// NewEvaluationReader returns an evaluation.Reader over pool.
func NewEvaluationReader(pool *pgxpool.Pool) evaluation.Reader {
	return evaluationReader{q: sqlcgen.New(pool)}
}

func (r evaluationReader) GetBySession(ctx context.Context, sessionID uuid.UUID) (evaluation.Evaluation, bool, error) {
	row, err := r.q.GetEvaluationBySession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Evaluation{}, false, nil
	}
	if err != nil {
		return evaluation.Evaluation{}, false, internalErr(err, "get evaluation failed")
	}
	return toEvaluation(row), true, nil
}

func (r evaluationReader) ListFindings(ctx context.Context, evaluationID uuid.UUID) ([]evaluation.FindingRow, error) {
	rows, err := r.q.ListEvaluationFindings(ctx, evaluationID)
	if err != nil {
		return nil, internalErr(err, "list evaluation findings failed")
	}
	out := make([]evaluation.FindingRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, toFindingRow(row))
	}
	return out, nil
}

type evaluationSource struct{ q *sqlcgen.Queries }

// NewEvaluationSource returns an evaluation.Source over pool.
func NewEvaluationSource(pool *pgxpool.Pool) evaluation.Source {
	return evaluationSource{q: sqlcgen.New(pool)}
}

func (s evaluationSource) LoadSession(ctx context.Context, id uuid.UUID) (evaluation.SessionInfo, error) {
	r, err := s.q.GetSessionForEvaluation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.SessionInfo{}, evaluation.ErrSessionNotFound
	}
	if err != nil {
		return evaluation.SessionInfo{}, internalErr(err, "load session for evaluation failed")
	}
	return evaluation.SessionInfo{
		ID: r.ID, UserID: r.UserID, ScenarioID: r.ScenarioID, ScenarioVersionID: r.ScenarioVersionID, TopicID: r.TopicID,
		Status: r.Status, StartingAt: fromTszPtr(r.StartingAt), StartedAt: fromTszPtr(r.StartedAt), EndedAt: fromTszPtr(r.EndedAt),
		DisconnectedSeconds: int(r.DisconnectedSeconds), AdaptiveDifficulty: r.AdaptiveDifficulty, ConfigSnapshot: r.ConfigSnapshot,
		TranscriptPurgedAt: fromTszPtr(r.TranscriptPurgedAt), UserStatus: r.UserStatus,
	}, nil
}

func (s evaluationSource) LoadTurns(ctx context.Context, sessionID uuid.UUID, from, to time.Time) ([]evaluation.TurnRow, error) {
	rows, err := s.q.ListTurnsForEvaluation(ctx, sqlcgen.ListTurnsForEvaluationParams{
		SessionID: sessionID, CreatedFrom: tsz(from), CreatedTo: tsz(to)})
	if err != nil {
		return nil, internalErr(err, "load turns for evaluation failed")
	}
	out := make([]evaluation.TurnRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, evaluation.TurnRow{
			ID: r.ID, CreatedAt: fromTsz(r.CreatedAt), Seq: int(r.SequenceNumber), Status: fromText(r.Status),
			UserText: fromText(r.UserText), AssistantText: fromText(r.AssistantText), AssistantSpokenText: fromText(r.AssistantSpokenText),
			UserStartedAt: fromTszPtr(r.UserStartedAt), UserEndedAt: fromTszPtr(r.UserEndedAt),
		})
	}
	return out, nil
}
