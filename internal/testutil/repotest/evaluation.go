package repotest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
)

// EvaluationHarness is one fresh evaluation store plus what it needs to accept rows: PostgreSQL
// requires a real user, session and prompt version, a fake does not.
type EvaluationHarness struct {
	Store  evaluation.Store
	Reader evaluation.Reader
	// PromptVersionID is an existing prompt version (evaluations.prompt_version_id is a FK).
	PromptVersionID uuid.UUID
	// NewSession returns a fresh user and one of its sessions.
	NewSession func(t *testing.T) (userID, sessionID uuid.UUID)
}

// evalCase is the identity of one evaluation under test.
type evalCase struct {
	user, session, event uuid.UUID
}

func (h EvaluationHarness) newCase(t *testing.T) evalCase {
	t.Helper()
	u, s := h.NewSession(t)
	return evalCase{user: u, session: s, event: uuid.Must(uuid.NewV7())}
}

func (h EvaluationHarness) claimInput(c evalCase) evaluation.ClaimInput {
	return evaluation.ClaimInput{
		ID: uuid.Must(uuid.NewV7()), SessionID: c.session, UserID: c.user,
		PromptVersionID: h.PromptVersionID, SourceEventID: c.event, RubricVersion: "rubric-v1",
	}
}

func (h EvaluationHarness) tx(t *testing.T, fn func(ctx context.Context, r evaluation.Repository)) {
	t.Helper()
	require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		fn(ctx, tx.Evaluations())
		return nil
	}))
}

func (h EvaluationHarness) claim(t *testing.T, c evalCase) evaluation.ClaimResult {
	t.Helper()
	var res evaluation.ClaimResult
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		var err error
		res, err = r.Claim(ctx, h.claimInput(c))
		require.NoError(t, err)
	})
	return res
}

func (h EvaluationHarness) get(t *testing.T, c evalCase) evaluation.Evaluation {
	t.Helper()
	e, ok, err := h.Reader.GetBySession(context.Background(), c.session)
	require.NoError(t, err)
	require.True(t, ok, "evaluation row expected")
	return e
}

func intp(n int) *int         { return &n }
func f64p(f float64) *float64 { return &f }
func strp(s string) *string   { return &s }

var evalAt = time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)

func normalUpdate() evaluation.CompletedUpdate {
	return evaluation.CompletedUpdate{
		Scores:       evaluation.Scores{Grammar: intp(52), Vocabulary: intp(55), Fluency: intp(45), Naturalness: intp(50), TaskAchievement: intp(60)},
		OverallScore: f64p(52.43), ListeningResponseSignal: intp(62), InteractionComplexitySignal: intp(38),
		DimensionConfidence: map[string]float64{"GRAMMAR": 0.8, "VOCABULARY": 0.7, "LISTENING_RESPONSE": 0.65},
		DimensionComments:   map[string]string{"GRAMMAR": "Partikel masih keliru.", "FLUENCY": "Terlalu pendek."},
		SummaryFeedback:     "Ringkasan umpan balik.",
		Strengths:           []string{"Konsisten memakai です・ます."},
		FocusAreas:          []string{"Partikel で", "Bentuk lampau kata sifat-i"},
		Recommendations:     []evaluation.Recommendation{{Title: "Latihan で", Detail: "Buat 5 kalimat.", ExampleJA: strp("図書館で勉強します。")}, {Title: "Tanpa contoh", Detail: "Detail."}},
		UserTurnCount:       7, UserSpeechSeconds: 95, ActiveSeconds: 280, ScenarioDifficultyLevel: 3,
		Provider: strp("OPENAI"), Model: strp("gpt-test"), LLMCallCount: 2, InputTokens: intp(5400), OutputTokens: intp(900),
		CompletedAt: evalAt,
	}
}

func insufficientUpdate() evaluation.CompletedUpdate {
	return evaluation.CompletedUpdate{
		InsufficientEvidence: true, SummaryFeedback: evaluation.InsufficientSummary,
		UserTurnCount: 2, UserSpeechSeconds: 12, ActiveSeconds: 40, ScenarioDifficultyLevel: 1,
		CompletedAt: evalAt,
	}
}

// EvaluationRepoSuite runs the contract of evaluation.Repository, Tx, Store and Reader against
// mk's store: claim, completion, findings replacement, state transitions and transaction
// rollback including the idempotency row and outbox.
func EvaluationRepoSuite(t *testing.T, mk func(t *testing.T) EvaluationHarness) {
	t.Helper()
	Each(t, mk,
		Case[EvaluationHarness]{"claim creates RUNNING attempt 1", evalClaimCreates},
		Case[EvaluationHarness]{"claim again from RUNNING, PENDING and FAILED increments attempt and clears failure", evalClaimReclaims},
		Case[EvaluationHarness]{"claim on COMPLETED returns Claimed=false and changes nothing", evalClaimCompleted},
		Case[EvaluationHarness]{"Complete insufficient row has no scores", evalCompleteInsufficient},
		Case[EvaluationHarness]{"Complete normal stores every column and sets completed_at", evalCompleteNormal},
		Case[EvaluationHarness]{"Complete again returns false and keeps the first result", evalCompleteTwice},
		Case[EvaluationHarness]{"Complete rejects scores inconsistent with insufficient_evidence", evalCompleteInconsistent},
		Case[EvaluationHarness]{"ReplaceFindings deletes old rows then inserts, ordinals unique", evalReplaceFindings},
		Case[EvaluationHarness]{"findings reject a type that does not belong to the dimension", evalFindingsTypeDimension},
		Case[EvaluationHarness]{"MarkPending only from RUNNING", evalMarkPending},
		Case[EvaluationHarness]{"MarkFailed from RUNNING or PENDING changes once, COMPLETED untouched", evalMarkFailed},
		Case[EvaluationHarness]{"rollback discards claim, processed row and outbox", evalRollback},
		Case[EvaluationHarness]{"committed MarkProcessed is reported on the second try", evalProcessedTwice},
	)
}

func evalClaimCreates(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	in := h.claimInput(c)
	var res evaluation.ClaimResult
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		var err error
		res, err = r.Claim(ctx, in)
		require.NoError(t, err)
	})
	require.True(t, res.Claimed)
	require.Equal(t, 1, res.AttemptCount)
	require.Equal(t, in.ID, res.ID)

	e := h.get(t, c)
	require.Equal(t, in.ID, e.ID)
	require.Equal(t, evaluation.StatusRunning, e.Status)
	require.Equal(t, 1, e.AttemptCount)
	require.Equal(t, c.user, e.UserID)
	require.Equal(t, c.event, e.SourceEventID)
	require.Equal(t, "rubric-v1", e.RubricVersion)
	require.NotNil(t, e.PromptVersionID)
	require.Equal(t, h.PromptVersionID, *e.PromptVersionID)
	require.NotNil(t, e.StartedAt)
	require.Greater(t, e.StartedAt.Year(), 2000, "started_at is a real time")
	require.False(t, e.UpdatedAt.Before(e.CreatedAt))
	require.Nil(t, e.CompletedAt)
	require.Nil(t, e.FailureCode)
	require.False(t, e.InsufficientEvidence)
	require.Nil(t, e.OverallScore)
}

func evalClaimReclaims(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	first := h.claim(t, c)
	require.Equal(t, 1, first.AttemptCount)

	second := h.claim(t, c) // RUNNING -> RUNNING after a crash
	require.True(t, second.Claimed)
	require.Equal(t, 2, second.AttemptCount)
	require.Equal(t, first.ID, second.ID, "the existing row keeps its id")

	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.MarkPending(ctx, c.session, "provider timeout"))
	})
	require.Equal(t, evaluation.StatusPending, h.get(t, c).Status)
	third := h.claim(t, c)
	require.Equal(t, 3, third.AttemptCount)
	e := h.get(t, c)
	require.Equal(t, evaluation.StatusRunning, e.Status)
	require.Nil(t, e.LastError, "last_error cleared by the claim")

	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		_, changed, err := r.MarkFailed(ctx, c.session, evaluation.FailInvalidModelOutput, "bad output")
		require.NoError(t, err)
		require.True(t, changed)
	})
	require.Equal(t, evaluation.StatusFailed, h.get(t, c).Status)
	fourth := h.claim(t, c)
	require.True(t, fourth.Claimed)
	require.Equal(t, 4, fourth.AttemptCount)
	e = h.get(t, c)
	require.Equal(t, evaluation.StatusRunning, e.Status)
	require.Nil(t, e.FailureCode, "failure_code cleared by the claim")
	require.Nil(t, e.LastError)
	require.Equal(t, first.ID, e.ID)
}

func evalClaimCompleted(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		ok, err := r.Complete(ctx, res.ID, normalUpdate())
		require.NoError(t, err)
		require.True(t, ok)
	})
	before := h.get(t, c)

	again := h.claim(t, c)
	require.False(t, again.Claimed)
	require.Equal(t, uuid.Nil, again.ID)
	require.Equal(t, before, h.get(t, c), "a COMPLETED row is untouched by the claim")
}

func evalCompleteInsufficient(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		ok, err := r.Complete(ctx, res.ID, insufficientUpdate())
		require.NoError(t, err)
		require.True(t, ok)
	})
	e := h.get(t, c)
	require.Equal(t, evaluation.StatusCompleted, e.Status)
	require.True(t, e.InsufficientEvidence)
	require.Equal(t, evaluation.Scores{}, e.Scores)
	require.Nil(t, e.OverallScore)
	require.Nil(t, e.ListeningResponseSignal)
	require.Empty(t, e.DimensionConfidence)
	require.Equal(t, evaluation.InsufficientSummary, *e.SummaryFeedback)
	require.Empty(t, e.Strengths)
	require.Empty(t, e.FocusAreas)
	require.Empty(t, e.Recommendations)
	require.Equal(t, 2, *e.UserTurnCount)
	require.Equal(t, 12, *e.UserSpeechSeconds)
	require.Equal(t, 40, *e.ActiveSeconds)
	require.Equal(t, 0, e.LLMCallCount)
	require.Nil(t, e.Provider)
	require.Nil(t, e.InputTokens)
	require.NotNil(t, e.CompletedAt)
}

func evalCompleteNormal(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)
	u := normalUpdate()
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		ok, err := r.Complete(ctx, res.ID, u)
		require.NoError(t, err)
		require.True(t, ok)
	})
	e := h.get(t, c)
	require.Equal(t, evaluation.StatusCompleted, e.Status)
	require.False(t, e.InsufficientEvidence)
	require.Equal(t, u.Scores, e.Scores)
	require.InDelta(t, 52.43, *e.OverallScore, 1e-9)
	require.Equal(t, 62, *e.ListeningResponseSignal)
	require.Equal(t, 38, *e.InteractionComplexitySignal)
	require.InDeltaMapValues(t, u.DimensionConfidence, e.DimensionConfidence, 1e-9)
	require.Equal(t, u.DimensionComments, e.DimensionComments)
	require.Equal(t, u.SummaryFeedback, *e.SummaryFeedback)
	require.Equal(t, u.Strengths, e.Strengths)
	require.Equal(t, u.FocusAreas, e.FocusAreas)
	require.Equal(t, u.Recommendations, e.Recommendations)
	require.Equal(t, 7, *e.UserTurnCount)
	require.Equal(t, 95, *e.UserSpeechSeconds)
	require.Equal(t, 280, *e.ActiveSeconds)
	require.Equal(t, 3, *e.ScenarioDifficultyLevel)
	require.Equal(t, "OPENAI", *e.Provider)
	require.Equal(t, "gpt-test", *e.Model)
	require.Equal(t, 2, e.LLMCallCount)
	require.Equal(t, 5400, *e.InputTokens)
	require.Equal(t, 900, *e.OutputTokens)
	require.NotNil(t, e.CompletedAt)
	require.True(t, evalAt.Equal(*e.CompletedAt), "completed_at is the given time")
	require.Nil(t, e.FailureCode)
	require.Equal(t, 1, e.AttemptCount)
}

func evalCompleteTwice(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		ok, err := r.Complete(ctx, res.ID, normalUpdate())
		require.NoError(t, err)
		require.True(t, ok)
	})
	second := normalUpdate()
	second.SummaryFeedback = "lain"
	second.Scores.Grammar = intp(99)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		ok, err := r.Complete(ctx, res.ID, second)
		require.NoError(t, err)
		require.False(t, ok)
	})
	e := h.get(t, c)
	require.Equal(t, "Ringkasan umpan balik.", *e.SummaryFeedback)
	require.Equal(t, 52, *e.Scores.Grammar)

	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		ok, err := r.Complete(ctx, uuid.Must(uuid.NewV7()), normalUpdate())
		require.NoError(t, err)
		require.False(t, ok, "unknown id")
	})
}

func evalCompleteInconsistent(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)

	missing := normalUpdate()
	missing.Scores.Fluency = nil
	err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		_, err := tx.Evaluations().Complete(ctx, res.ID, missing)
		return err
	})
	require.Error(t, err, "a normal COMPLETED row needs all five scores and overall")

	scored := insufficientUpdate()
	scored.Scores.Grammar = intp(50)
	err = h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		_, err := tx.Evaluations().Complete(ctx, res.ID, scored)
		return err
	})
	require.Error(t, err, "insufficient evidence means no scores")

	require.Equal(t, evaluation.StatusRunning, h.get(t, c).Status, "failed completions change nothing")
}

func finding(ord int, typ evaluation.FindingType, dim evaluation.Dimension, sev evaluation.Severity, seq *int, turn *uuid.UUID) evaluation.FindingRow {
	return evaluation.FindingRow{
		ID: uuid.Must(uuid.NewV7()), Ordinal: ord, CreatedAt: evalAt,
		Finding: evaluation.Finding{
			Type: typ, Dimension: dim, Severity: sev, TurnSeq: seq, TurnID: turn,
			Original: strp("会社に働いていました"), Corrected: strp("会社で働いていました"), PatternKey: strp("particle.de_ni_location"),
			Explanation: "Tempat aktivitas ditandai で.", Confidence: 0.95, Source: "LLM",
		},
	}
}

func evalReplaceFindings(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)
	turn := uuid.Must(uuid.NewV7())
	a := finding(1, evaluation.FindingParticleError, evaluation.DimGrammar, evaluation.SeverityMajor, intp(1), &turn)
	b := finding(2, evaluation.FindingWordChoiceError, evaluation.DimVocabulary, evaluation.SeverityMinor, intp(2), nil)
	b.PatternKey = nil
	s := finding(3, evaluation.FindingStrength, evaluation.DimNaturalness, evaluation.SeverityInfo, nil, nil)
	s.Original, s.Corrected, s.Confidence = nil, nil, 0.7

	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.ReplaceFindings(ctx, res.ID, c.user, c.session, []evaluation.FindingRow{s, a, b}))
	})
	got, err := h.Reader.ListFindings(context.Background(), res.ID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, []int{1, 2, 3}, []int{got[0].Ordinal, got[1].Ordinal, got[2].Ordinal}, "ordered by ordinal")
	require.Equal(t, a.ID, got[0].ID)
	require.Equal(t, evaluation.FindingParticleError, got[0].Type)
	require.Equal(t, evaluation.DimGrammar, got[0].Dimension)
	require.Equal(t, evaluation.SeverityMajor, got[0].Severity)
	require.Equal(t, 1, *got[0].TurnSeq)
	require.Equal(t, turn, *got[0].TurnID)
	require.Equal(t, "会社に働いていました", *got[0].Original)
	require.Equal(t, "会社で働いていました", *got[0].Corrected)
	require.Equal(t, "particle.de_ni_location", *got[0].PatternKey)
	require.Equal(t, "Tempat aktivitas ditandai で.", got[0].Explanation)
	require.InDelta(t, 0.95, got[0].Confidence, 1e-9)
	require.Equal(t, "LLM", got[0].Source)
	require.True(t, evalAt.Equal(got[0].CreatedAt))
	require.Nil(t, got[1].TurnID)
	require.Nil(t, got[1].PatternKey)
	require.Nil(t, got[2].TurnSeq)
	require.Nil(t, got[2].Original)
	require.InDelta(t, 0.7, got[2].Confidence, 1e-9)

	// Replacing deletes the old rows first (their ordinals may be reused).
	n1 := finding(1, evaluation.FindingConjugationError, evaluation.DimGrammar, evaluation.SeverityMinor, intp(2), nil)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.ReplaceFindings(ctx, res.ID, c.user, c.session, []evaluation.FindingRow{n1}))
	})
	got, err = h.Reader.ListFindings(context.Background(), res.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, n1.ID, got[0].ID)

	// Another evaluation's findings are untouched.
	other := h.newCase(t)
	ores := h.claim(t, other)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.ReplaceFindings(ctx, ores.ID, other.user, other.session,
			[]evaluation.FindingRow{finding(1, evaluation.FindingParticleError, evaluation.DimGrammar, evaluation.SeverityMajor, intp(1), nil)}))
		require.NoError(t, r.ReplaceFindings(ctx, res.ID, c.user, c.session, nil))
	})
	got, err = h.Reader.ListFindings(context.Background(), res.ID)
	require.NoError(t, err)
	require.Empty(t, got, "replacing with nothing leaves no finding")
	got, err = h.Reader.ListFindings(context.Background(), ores.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// Duplicate ordinals in one batch are rejected and roll the transaction back.
	d1 := finding(1, evaluation.FindingParticleError, evaluation.DimGrammar, evaluation.SeverityMajor, intp(1), nil)
	d2 := finding(1, evaluation.FindingConjugationError, evaluation.DimGrammar, evaluation.SeverityMinor, intp(2), nil)
	err = h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		return tx.Evaluations().ReplaceFindings(ctx, res.ID, c.user, c.session, []evaluation.FindingRow{d1, d2})
	})
	require.Error(t, err)
}

func evalFindingsTypeDimension(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	res := h.claim(t, c)
	bad := finding(1, evaluation.FindingParticleError, evaluation.DimVocabulary, evaluation.SeverityMajor, intp(1), nil)
	err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		return tx.Evaluations().ReplaceFindings(ctx, res.ID, c.user, c.session, []evaluation.FindingRow{bad})
	})
	require.Error(t, err)
	badStrength := finding(1, evaluation.FindingStrength, evaluation.DimGrammar, evaluation.SeverityMajor, nil, nil)
	err = h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		return tx.Evaluations().ReplaceFindings(ctx, res.ID, c.user, c.session, []evaluation.FindingRow{badStrength})
	})
	require.Error(t, err, "STRENGTH must be INFO")
	got, lerr := h.Reader.ListFindings(context.Background(), res.ID)
	require.NoError(t, lerr)
	require.Empty(t, got)
}

func evalMarkPending(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.MarkPending(ctx, c.session, "no row yet"), "no row is a no-op")
	})
	_, ok, err := h.Reader.GetBySession(context.Background(), c.session)
	require.NoError(t, err)
	require.False(t, ok, "MarkPending never creates a row")

	h.claim(t, c)
	long := strings.Repeat("あ", 2500)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.MarkPending(ctx, c.session, long))
	})
	e := h.get(t, c)
	require.Equal(t, evaluation.StatusPending, e.Status)
	require.Len(t, []rune(*e.LastError), 2000, "last_error is cut to 2,000 characters")
	require.False(t, e.UpdatedAt.Before(*e.StartedAt), "updated_at moves with the transition")

	// PENDING is not RUNNING: a second MarkPending keeps the first message.
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.MarkPending(ctx, c.session, "second"))
	})
	require.Len(t, []rune(*h.get(t, c).LastError), 2000)

	// FAILED and COMPLETED are never moved back to PENDING.
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		_, _, err := r.MarkFailed(ctx, c.session, evaluation.FailInternalError, "x")
		require.NoError(t, err)
		require.NoError(t, r.MarkPending(ctx, c.session, "y"))
	})
	require.Equal(t, evaluation.StatusFailed, h.get(t, c).Status)

	done := h.newCase(t)
	dres := h.claim(t, done)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		_, err := r.Complete(ctx, dres.ID, normalUpdate())
		require.NoError(t, err)
		require.NoError(t, r.MarkPending(ctx, done.session, "z"))
	})
	require.Equal(t, evaluation.StatusCompleted, h.get(t, done).Status)
}

func evalMarkFailed(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	var changed bool
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		_, changed, _ = r.MarkFailed(ctx, c.session, evaluation.FailInternalError, "no row")
	})
	require.False(t, changed, "no row: nothing to fail")

	h.claim(t, c)
	h.claim(t, c) // attempt 2
	var ev evaluation.Evaluation
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		var err error
		ev, changed, err = r.MarkFailed(ctx, c.session, evaluation.FailProviderUnavailable, "provider down")
		require.NoError(t, err)
	})
	require.True(t, changed)
	require.Equal(t, evaluation.StatusFailed, ev.Status)
	require.Equal(t, 2, ev.AttemptCount)
	require.Equal(t, evaluation.FailProviderUnavailable, *ev.FailureCode)
	require.Equal(t, "provider down", *ev.LastError)
	require.Equal(t, c.session, ev.SessionID)
	require.Equal(t, c.user, ev.UserID)
	require.False(t, ev.UpdatedAt.Before(*ev.StartedAt), "updated_at moves with the transition")
	stored := h.get(t, c)
	require.Equal(t, evaluation.StatusFailed, stored.Status)
	require.Equal(t, evaluation.FailProviderUnavailable, *stored.FailureCode)

	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		_, changed, _ = r.MarkFailed(ctx, c.session, evaluation.FailInternalError, "again")
	})
	require.False(t, changed, "FAILED is not failed twice")
	require.Equal(t, evaluation.FailProviderUnavailable, *h.get(t, c).FailureCode)

	// PENDING also fails.
	p := h.newCase(t)
	h.claim(t, p)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		require.NoError(t, r.MarkPending(ctx, p.session, "retry"))
		_, changed, _ = r.MarkFailed(ctx, p.session, evaluation.FailInvalidModelOutput, "invalid")
	})
	require.True(t, changed)
	require.Equal(t, evaluation.FailInvalidModelOutput, *h.get(t, p).FailureCode)

	// COMPLETED stays COMPLETED.
	d := h.newCase(t)
	dres := h.claim(t, d)
	h.tx(t, func(ctx context.Context, r evaluation.Repository) {
		_, err := r.Complete(ctx, dres.ID, normalUpdate())
		require.NoError(t, err)
		_, changed, _ = r.MarkFailed(ctx, d.session, evaluation.FailInternalError, "late")
	})
	require.False(t, changed)
	require.Equal(t, evaluation.StatusCompleted, h.get(t, d).Status)
	require.Nil(t, h.get(t, d).FailureCode)
}

func sessionCompletedEnvelope(c evalCase) events.Envelope {
	return events.Envelope{EventID: c.event, EventType: "session.completed", EventVersion: 1, AggregateID: c.session, UserID: c.user}
}

func evalRollback(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	boom := errors.New("boom")
	err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		res, err := tx.Evaluations().Claim(ctx, h.claimInput(c))
		require.NoError(t, err)
		require.True(t, res.Claimed)
		require.NoError(t, tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, sessionCompletedEnvelope(c)))
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, ok, gerr := h.Reader.GetBySession(context.Background(), c.session)
	require.NoError(t, gerr)
	require.False(t, ok, "the claim is rolled back")
	require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, sessionCompletedEnvelope(c))
	}), "the processed row is rolled back with it")
}

func evalProcessedTwice(t *testing.T, h EvaluationHarness) {
	c := h.newCase(t)
	require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, sessionCompletedEnvelope(c))
	}))
	err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, sessionCompletedEnvelope(c))
	})
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
}
