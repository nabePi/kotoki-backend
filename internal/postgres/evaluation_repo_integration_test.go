//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

type pgEvaluation struct {
	pool    *pgxpool.Pool
	store   *postgres.Store
	content testutil.ContentIDs
}

func newPGEvaluation(t *testing.T) *pgEvaluation {
	t.Helper()
	pool := testutil.NewPostgres(t)
	return &pgEvaluation{pool: pool, store: postgres.NewStore(pool), content: testutil.SeedContent(t, pool)}
}

func (p *pgEvaluation) newSession(t *testing.T, userRow, sessionRow testutil.Row) (uuid.UUID, uuid.UUID) {
	t.Helper()
	uid := testutil.SeedUser(t, p.pool, userRow)
	return uid, testutil.SeedSession(t, p.pool, uid, p.content, sessionRow)
}

func TestPGEvaluationRepo(t *testing.T) {
	repotest.EvaluationRepoSuite(t, func(t *testing.T) repotest.EvaluationHarness {
		p := newPGEvaluation(t)
		return repotest.EvaluationHarness{
			Store: p.store.EvaluationStore(), Reader: postgres.NewEvaluationReader(p.pool), PromptVersionID: p.content.PromptVersionID,
			NewSession: func(t *testing.T) (uuid.UUID, uuid.UUID) { return p.newSession(t, nil, nil) },
		}
	})
}

func TestPGEvaluationFindingConstraintsAreEnforcedByTheDatabase(t *testing.T) {
	p := newPGEvaluation(t)
	uid, sid := p.newSession(t, nil, nil)
	store := p.store.EvaluationStore()
	var evalID uuid.UUID
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		res, err := tx.Evaluations().Claim(ctx, evaluation.ClaimInput{
			ID: uuid.Must(uuid.NewV7()), SessionID: sid, UserID: uid, PromptVersionID: p.content.PromptVersionID,
			SourceEventID: uuid.Must(uuid.NewV7()), RubricVersion: "rubric-v1"})
		evalID = res.ID
		return err
	}))

	// The raw insert bypasses the repository: the CHECK itself must refuse the pair.
	_, err := p.pool.Exec(context.Background(),
		`INSERT INTO evaluation_findings (id, evaluation_id, session_id, user_id, ordinal, dimension, finding_type, severity, explanation, confidence, source)
		 VALUES ($1, $2, $3, $4, 1, 'VOCABULARY', 'PARTICLE_ERROR', 'MAJOR', 'x', 0.9, 'LLM')`,
		uuid.Must(uuid.NewV7()), evalID, sid, uid)
	require.ErrorContains(t, err, "chk_ef_type_dimension")
}

func TestPGEvaluationCompleteInsufficientSatisfiesCheck(t *testing.T) {
	p := newPGEvaluation(t)
	uid, sid := p.newSession(t, nil, nil)
	store := p.store.EvaluationStore()
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		r := tx.Evaluations()
		res, err := r.Claim(ctx, evaluation.ClaimInput{ID: uuid.Must(uuid.NewV7()), SessionID: sid, UserID: uid,
			PromptVersionID: p.content.PromptVersionID, SourceEventID: uuid.Must(uuid.NewV7()), RubricVersion: "rubric-v1"})
		require.NoError(t, err)
		ok, err := r.Complete(ctx, res.ID, evaluation.CompletedUpdate{
			InsufficientEvidence: true, SummaryFeedback: evaluation.InsufficientSummary, UserTurnCount: 1, UserSpeechSeconds: 3, ActiveSeconds: 20,
			ScenarioDifficultyLevel: 1, CompletedAt: time.Now().UTC()})
		require.NoError(t, err)
		require.True(t, ok)
		return nil
	}))
	var n int
	require.NoError(t, p.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM evaluations WHERE session_id = $1 AND insufficient_evidence AND status = 'COMPLETED' AND grammar_score IS NULL AND overall_score IS NULL AND llm_call_count = 0`, sid).Scan(&n))
	require.Equal(t, 1, n)
}

func TestPGEvaluationSourceLoadTurnsSkipsDiscardedAndBoundsWindow(t *testing.T) {
	p := newPGEvaluation(t)
	_, sid := p.newSession(t, nil, nil)
	now := time.Now().UTC()
	insert := func(seq int, status *string, user *string) {
		row := testutil.Row{"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": seq, "generation": 1, "created_at": now}
		if status != nil {
			row["status"], row["completed_at"] = *status, now
		}
		if user != nil {
			row["user_text"] = *user
			row["user_started_at"], row["user_ended_at"] = now.Add(-3*time.Second), now
		}
		testutil.MustInsert(context.Background(), t, p.pool, "session_turns", row)
	}
	completed, discarded := "COMPLETED", "DISCARDED"
	hello, ghost := "会社で働いています", "ノイズ"
	insert(3, &completed, &hello)
	insert(1, &completed, &hello)
	insert(2, &discarded, &ghost)
	insert(4, nil, nil) // a turn in progress (status NULL) is returned; eligibility is the domain's call

	src := postgres.NewEvaluationSource(p.pool)
	turns, err := src.LoadTurns(context.Background(), sid, now.Add(-time.Minute), now.Add(time.Minute))
	require.NoError(t, err)
	seqs := make([]int, len(turns))
	for i, tr := range turns {
		seqs[i] = tr.Seq
	}
	require.Equal(t, []int{1, 3, 4}, seqs, "DISCARDED is skipped and the rest is ordered by sequence_number")
	require.Equal(t, "COMPLETED", *turns[0].Status)
	require.Equal(t, hello, *turns[0].UserText)
	require.NotNil(t, turns[0].UserStartedAt)
	require.Nil(t, turns[2].Status)
	require.Nil(t, turns[2].UserText)

	none, err := src.LoadTurns(context.Background(), sid, now.Add(time.Hour), now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Empty(t, none, "created_at outside the window")
	none, err = src.LoadTurns(context.Background(), sid, now.Add(-2*time.Hour), now.Add(-time.Hour))
	require.NoError(t, err)
	require.Empty(t, none)

	other, err := src.LoadTurns(context.Background(), uuid.Must(uuid.NewV7()), now.Add(-time.Minute), now.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, other, "turns of another session")
}

func TestPGEvaluationSourceLoadSession(t *testing.T) {
	p := newPGEvaluation(t)
	started, ended := time.Now().UTC().Add(-10*time.Minute).Truncate(time.Millisecond), time.Now().UTC().Truncate(time.Millisecond)
	uid, sid := p.newSession(t, testutil.Row{"status": "SUSPENDED"}, testutil.Row{
		"status": "COMPLETED", "end_reason": "USER_ENDED", "ending_at": ended, "starting_at": started.Add(-time.Second), "started_at": started, "ended_at": ended, "disconnected_seconds": 12,
		"adaptive_difficulty": []byte(`{"formality":"POLITE"}`)})

	src := postgres.NewEvaluationSource(p.pool)
	info, err := src.LoadSession(context.Background(), sid)
	require.NoError(t, err)
	require.Equal(t, sid, info.ID)
	require.Equal(t, uid, info.UserID)
	require.Equal(t, p.content.ScenarioID, info.ScenarioID)
	require.Equal(t, p.content.ScenarioVersionID, info.ScenarioVersionID)
	require.Equal(t, p.content.TopicID, info.TopicID)
	require.Equal(t, "COMPLETED", info.Status)
	require.Equal(t, "SUSPENDED", info.UserStatus, "the owner's status comes from users")
	require.True(t, started.Equal(*info.StartedAt))
	require.True(t, ended.Equal(*info.EndedAt))
	require.NotNil(t, info.StartingAt)
	require.Equal(t, 12, info.DisconnectedSeconds)
	require.JSONEq(t, `{"formality":"POLITE"}`, string(info.AdaptiveDifficulty))
	require.Contains(t, string(info.ConfigSnapshot), "Haruka")
	require.Nil(t, info.TranscriptPurgedAt)

	_, err = src.LoadSession(context.Background(), uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, evaluation.ErrSessionNotFound)
}

func TestPGEvaluationReaderMissingIsNotAnError(t *testing.T) {
	p := newPGEvaluation(t)
	r := postgres.NewEvaluationReader(p.pool)
	_, ok, err := r.GetBySession(context.Background(), uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	require.False(t, ok)
	fs, err := r.ListFindings(context.Background(), uuid.Must(uuid.NewV7()))
	require.NoError(t, err)
	require.Empty(t, fs)
}
