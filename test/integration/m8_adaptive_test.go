//go:build integration

package integration_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// adaptiveRig adds the real content loader and a scenario of difficulty 5 to a learnerRig.
type adaptiveRig struct {
	*learnerRig
	svc *learner.Service
	rc  content.ResolvedConfig
}

func newAdaptiveRig(t *testing.T) *adaptiveRig {
	t.Helper()
	r := newLearnerRig(t)
	ctx := context.Background()
	topic := testutil.TopicRow(nil)
	testutil.MustInsert(ctx, t, r.pool, "topics", topic)
	sc := testutil.ScenarioRow(topic["id"].(uuid.UUID), nil)
	testutil.MustInsert(ctx, t, r.pool, "scenarios", sc)
	sv := testutil.ScenarioVersionRow(sc["id"].(uuid.UUID), 1, testutil.Row{
		"status": "PUBLISHED", "published_at": time.Now().UTC(), "difficulty_level": 5,
	})
	testutil.MustInsert(ctx, t, r.pool, "scenario_versions", sv)
	svc := learner.NewService(learner.Deps{
		Store: r.store.LearnerStore(), Content: content.NewSessionConfigLoader(postgres.NewContentRepo(r.pool)),
		Clock: clock.System{}, IDs: idgen.NewV7(),
	})
	return &adaptiveRig{learnerRig: r, svc: svc, rc: content.ResolvedConfig{
		TopicID: topic["id"].(uuid.UUID), ScenarioID: sc["id"].(uuid.UUID), ScenarioVersionID: sv["id"].(uuid.UUID),
		CharacterID: r.content.CharacterID, PromptVersionID: r.content.PromptVersionID,
		ConfigSnapshot: content.SessionSnapshot{
			Character: content.CharacterSnapshot{ID: r.content.CharacterID, Voice: content.Voice{Gender: "FEMALE", Accent: content.AccentKanto, SpeakingRate: 1}},
			Formality: content.FormalityPolite,
		},
	}}
}

func (r *adaptiveRig) compute(userID uuid.UUID) learner.AdaptiveDifficulty {
	r.t.Helper()
	ad, err := r.svc.ComputeAdaptiveDifficulty(context.Background(), learner.AdaptiveRequest{UserID: userID, Config: r.rc})
	require.NoError(r.t, err)
	return ad
}

func TestM8ComputeAdaptive_ReflectsSkillStateAfterEvaluation(t *testing.T) {
	r := newAdaptiveRig(t)
	uid := r.user("N2") // receptive 4.00, productive 3.50
	before := r.compute(uid)
	require.Equal(t, 5, before.ScenarioDifficulty, "difficulty_level of the scenario version, read through the content loader")
	require.Equal(t, 4, before.GrammarComplexity, "Stretch(4.00, 5) = R(4.3)")
	require.Equal(t, 2, before.Scaffolding, "gap 5 - 4.00 = 1.0")

	// three sessions in which GRAMMAR fails at the difficulty it was given
	for i := 0; i < 3; i++ {
		ev, sid := r.eval(uid, learnerEval{
			endedAt: r.base.AddDate(0, 0, i), nTurns: 12, scores: [4]int{0, 100, 100, 100},
			adaptive: `{"grammar_complexity":5,"vocabulary_complexity":5,"expected_response_length":5,"scenario_difficulty":5}`,
		})
		require.NoError(t, r.handle(r.delivery(uid, ev, sid)))
	}
	require.Less(t, r.skill(uid, "GRAMMAR").Level, 3.5)

	after := r.compute(uid)
	require.LessOrEqual(t, after.GrammarComplexity, before.GrammarComplexity)
	require.Greater(t, after.Scaffolding, before.Scaffolding, "a weaker grammar level gets more scaffolding")
	require.Equal(t, 3, after.Scaffolding)

	// the same call for another user is not influenced by it
	other := r.user("N2")
	require.Equal(t, before, r.compute(other))
}

func TestM8ComputeAdaptive_P95OnFixture(t *testing.T) {
	r := newAdaptiveRig(t)
	uid := r.user("N3")
	ev, sid := r.eval(uid, learnerEval{endedAt: r.base, nTurns: 12, scores: [4]int{80, 70, 60, 50}})
	require.NoError(t, r.handle(r.delivery(uid, ev, sid)))
	r.compute(uid) // warm up the pool and the plans

	const calls = 50
	took := make([]time.Duration, calls)
	for i := range took {
		start := time.Now()
		r.compute(uid)
		took[i] = time.Since(start)
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	p95 := took[calls*95/100-1]
	t.Logf("ComputeAdaptiveDifficulty p95 = %s (target 20 ms is a metric, the bound is 100 ms)", p95)
	require.Less(t, p95, 100*time.Millisecond)
}
