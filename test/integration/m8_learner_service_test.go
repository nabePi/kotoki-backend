//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestM8LearnerService_ProfileLifecycle_Postgres(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewPostgres(t)
	content := testutil.SeedContent(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clk := clock.NewFake(now)
	store := postgres.NewStore(pool, postgres.WithClock(clk), postgres.WithIDs(idgen.NewV7()))
	svc := learner.NewService(learner.Deps{Store: store.LearnerStore(), Clock: clk, IDs: idgen.NewV7()})
	uid := testutil.SeedUser(t, pool, nil)
	other := testutil.SeedUser(t, pool, nil)

	// the first read creates the profile and the six states
	p, err := svc.GetProfile(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, uid, p.UserID)
	require.True(t, p.MemoryEnabled)
	require.InDelta(t, 1.00, p.OverallLevel, 1e-9)
	var states int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM learner_skill_states WHERE user_id = $1`, uid).Scan(&states))
	require.Equal(t, 6, states)

	// onboarding with no evidence sets the priors of the table
	p, err = svc.PatchProfile(ctx, uid, learner.PatchInput{
		Nickname: lset(" Budi "), SelfDeclaredLevel: lset("N4"), TargetLevel: lset("N2"), LearningGoal: lset("JOB_INTERVIEW"),
		SpeechSpeed: lset("SLOWER"), MemoryEnabled: lset(false),
	})
	require.NoError(t, err)
	require.Equal(t, "Budi", *p.Nickname)
	require.False(t, p.MemoryEnabled)
	require.NotNil(t, p.OnboardingCompletedAt)
	require.True(t, p.OnboardingCompletedAt.Equal(now))
	require.InDelta(t, 1.75, p.OverallLevel, 1e-9, "(3 receptive x 2.00 + 3 productive x 1.50) / 6")
	v, err := svc.Skills(ctx, uid)
	require.NoError(t, err)
	require.InDelta(t, 2.00, v.Skills[0].EstimatedLevel, 1e-9)
	require.InDelta(t, 1.50, v.Skills[2].EstimatedLevel, 1e-9)
	require.Equal(t, learner.SkillGrammar, v.Skills[0].Skill)
	require.True(t, v.Skills[0].IsProvisional)
	require.Empty(t, v.FocusSkills)
	require.Empty(t, v.VoiceExposure.Items)

	// evidence arrives (what the worker will write): a completed session, an evaluation, an estimate
	sid := testutil.SeedSession(t, pool, uid, content, testutil.Row{
		"status": "COMPLETED", "end_reason": "USER_ENDED", "starting_at": now.Add(-time.Hour), "started_at": now.Add(-time.Hour),
		"expires_at": now.Add(-time.Hour).Add(300 * time.Second), "ending_at": now.Add(-50 * time.Minute),
		"ended_at": now.Add(-50 * time.Minute), "charged_seconds": 240,
	})
	evID := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, pool, "evaluations", testutil.Row{
		"id": evID, "session_id": sid, "user_id": uid, "status": "COMPLETED", "rubric_version": "rubric-v1",
		"source_event_id": uuid.Must(uuid.NewV7()), "grammar_score": 60, "vocabulary_score": 61, "fluency_score": 62,
		"naturalness_score": 63, "task_achievement_score": 64, "overall_score": 62.0, "completed_at": now, "started_at": now,
	})
	last := now.Add(-50 * time.Minute)
	require.NoError(t, store.LearnerStore().WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		_, err := tx.Estimates().Insert(ctx, uid, learner.EstimateRow{
			ID: uuid.Must(uuid.NewV7()), EvaluationID: evID, SessionID: sid, Skill: learner.SkillGrammar, AlgorithmVersion: 1,
			RawScore: 60, LObs: 1.71, CObs: 0.5, AssessedAt: last, Previous: 2.0, Level: 1.9, Confidence: 0.3, Count: 1,
			Trend: learner.TrendInsufficient,
		})
		if err != nil {
			return err
		}
		return tx.SkillStates().Save(ctx, uid, learner.StateRow{
			Skill: learner.SkillGrammar, Level: 1.9, Confidence: 0.3, Ceiling: lptr(2.0), Trend: learner.TrendInsufficient,
			EvidenceCount: 1, LastEvaluationID: &evID, LastEvidenceAt: &last,
		}, now)
	}))

	// a late onboarding level changes the profile column only
	p, err = svc.PatchProfile(ctx, uid, learner.PatchInput{SelfDeclaredLevel: lset("N1")})
	require.NoError(t, err)
	require.Equal(t, "N1", *p.SelfDeclaredLevel)
	require.EqualValues(t, 240, p.TotalPracticeSeconds)
	require.Equal(t, 1, p.CompletedSessionCount)
	v, err = svc.Skills(ctx, uid)
	require.NoError(t, err)
	g := v.Skills[0]
	require.InDelta(t, 1.90, g.EstimatedLevel, 1e-9, "the state with evidence kept its level")
	require.Equal(t, 1, g.EvidenceCount)
	require.NotNil(t, g.LastAssessedAt)
	require.InDelta(t, 2.00, v.Skills[1].EstimatedLevel, 1e-9, "no state with evidence is rewritten, and the others keep the N4 prior")

	hist, next, err := svc.History(ctx, uid, learner.SkillGrammar, nil, 20)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Nil(t, next)
	require.Equal(t, evID, hist[0].EvaluationID)

	// the other user sees none of it
	po, err := svc.GetProfile(ctx, other)
	require.NoError(t, err)
	require.Nil(t, po.Nickname)
	require.Zero(t, po.CompletedSessionCount)
	ho, _, err := svc.History(ctx, other, learner.SkillGrammar, nil, 20)
	require.NoError(t, err)
	require.Empty(t, ho)

	// reset: priors back (the N4 priors), history gone, profile intact
	require.NoError(t, svc.ResetSkills(ctx, uid))
	v, err = svc.Skills(ctx, uid)
	require.NoError(t, err)
	require.InDelta(t, 2.00, v.Skills[0].EstimatedLevel, 1e-9)
	require.Zero(t, v.Skills[0].EvidenceCount)
	require.Nil(t, v.Skills[0].LastAssessedAt)
	hist, _, err = svc.History(ctx, uid, learner.SkillGrammar, nil, 20)
	require.NoError(t, err)
	require.Empty(t, hist)
	p, err = svc.GetProfile(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, "Budi", *p.Nickname)
	require.Equal(t, "N1", *p.SelfDeclaredLevel)
	var evals int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM evaluations WHERE user_id = $1`, uid).Scan(&evals))
	require.Equal(t, 1, evals, "evaluations are not touched by a reset")

	// a patch the database would refuse is rejected before it gets there
	_, err = svc.PatchProfile(ctx, uid, learner.PatchInput{Nickname: lset("a{b")})
	require.Error(t, err)
}

func lptr[T any](v T) *T { return &v }

func lset[T any](v T) learner.Field[T] { return learner.Field[T]{Set: true, Value: v} }
