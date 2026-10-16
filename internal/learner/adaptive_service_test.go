package learner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

type fakeLoader struct {
	mu   sync.Mutex
	cfg  content.SessionConfig
	err  error
	refs []content.SessionRefs
}

func (l *fakeLoader) Load(_ context.Context, refs content.SessionRefs) (content.SessionConfig, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refs = append(l.refs, refs)
	return l.cfg, l.err
}

type adaptiveEnv struct {
	svc    *learner.Service
	store  *fakes.LearnerStore
	loader *fakeLoader
	user   uuid.UUID
	rc     content.ResolvedConfig
}

// newAdaptiveEnv returns a service over a loader that answers a scenario of difficulty b.
func newAdaptiveEnv(t *testing.T, b int, accent content.Accent, formality content.Formality) *adaptiveEnv {
	t.Helper()
	clk := clock.NewFake(svcNow)
	store := fakes.NewLearnerStore(fakes.WithLearnerClock(clk))
	topic, sv := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	loader := &fakeLoader{cfg: content.SessionConfig{
		Topic:           content.Topic{ID: topic},
		ScenarioVersion: content.ScenarioVersion{ID: sv, DifficultyLevel: b, Formality: formality},
	}}
	return &adaptiveEnv{
		svc:   learner.NewService(learner.Deps{Store: store, Content: loader, Clock: clk, IDs: idgen.NewSequence(t)}),
		store: store, loader: loader, user: newUser(),
		rc: content.ResolvedConfig{
			TopicID: topic, ScenarioVersionID: sv, PromptVersionID: uuid.Must(uuid.NewV7()),
			ConfigSnapshot: content.SessionSnapshot{
				Character: content.CharacterSnapshot{Voice: content.Voice{Gender: "FEMALE", Accent: accent, SpeakingRate: 1}},
				Formality: formality,
			},
		},
	}
}

func (e *adaptiveEnv) compute(t *testing.T) learner.AdaptiveDifficulty {
	t.Helper()
	ad, err := e.svc.ComputeAdaptiveDifficulty(context.Background(), learner.AdaptiveRequest{UserID: e.user, Config: e.rc})
	require.NoError(t, err)
	return ad
}

func (e *adaptiveEnv) patch(t *testing.T, in learner.PatchInput) {
	t.Helper()
	_, err := e.svc.PatchProfile(context.Background(), e.user, in)
	require.NoError(t, err)
}

func TestComputeAdaptive_NewUserUsesPriors(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	ad := e.compute(t)
	require.Equal(t, learner.AlgorithmVersion, ad.AlgorithmVersion)
	require.Equal(t, 3, ad.ScenarioDifficulty)
	require.Equal(t, 1, ad.VocabularyComplexity, "Stretch(1.0) = clamp(R(1.3) = 1, 1, 4)")
	require.Equal(t, 1, ad.GrammarComplexity)
	require.Equal(t, 3, ad.Scaffolding, "gap 3 - 1.00 >= 1.5")
	require.Equal(t, learner.FormalityPolite, ad.Formality)
	require.Equal(t, learner.DialectNone, ad.DialectExposure)
	require.Equal(t, 0, ad.InSessionAdjustments)
	require.Empty(t, ad.FocusSkills)

	_, ok := e.store.Profile(e.user)
	require.True(t, ok, "the profile and its six states are ensured")
	require.Len(t, e.store.States(e.user), 6)

	require.Len(t, e.loader.refs, 1)
	refs := e.loader.refs[0]
	require.Equal(t, e.rc.TopicID, refs.TopicID)
	require.Equal(t, e.rc.ScenarioVersionID, refs.ScenarioVersionID)
	require.Equal(t, e.rc.PromptVersionID, refs.PromptVersionID)
	snap, err := e.rc.ConfigSnapshot.MarshalJSONB()
	require.NoError(t, err)
	require.JSONEq(t, string(snap), string(refs.ConfigSnapshot), "the snapshot of the resolved config, not a live row")
}

func TestComputeAdaptive_UsesOnboardingLevel(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	e.patch(t, learner.PatchInput{SelfDeclaredLevel: set("N3")}) // receptive 3.00, productive 2.50
	ad := e.compute(t)
	require.Equal(t, 3, ad.VocabularyComplexity, "R(3.3)")
	require.Equal(t, 3, ad.GrammarComplexity)
	require.Equal(t, 3, ad.ExpectedResponseLength, "FLUENCY 2.50: R(2.8)")
	require.Equal(t, 1, ad.Scaffolding, "gap 3 - 3.00 = 0")
}

func TestComputeAdaptive_UsesPreferencesAndScenarioFocus(t *testing.T) {
	speedOf := func(pref string) int {
		e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
		e.patch(t, learner.PatchInput{SelfDeclaredLevel: set("N3"), SpeechSpeed: set(pref)})
		return e.compute(t).SpeechSpeed
	}
	require.Equal(t, 3, speedOf("NORMAL"))
	require.Equal(t, 2, speedOf("SLOWER"))
	require.Equal(t, 4, speedOf("FASTER"))

	formalityOf := func(pref string) learner.Formality {
		e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
		e.patch(t, learner.PatchInput{Formality: set(pref)})
		return e.compute(t).Formality
	}
	require.Equal(t, learner.FormalityPolite, formalityOf("AUTO"))
	require.Equal(t, learner.FormalityCasual, formalityOf("CASUAL"))

	// one MINOR finding in one evaluation: f = 1/3, P = 0.5/3 = 0.167 < 0.30; the scenario focus adds 0.2
	focusOf := func(scenarioFocus []string) []learner.Skill {
		e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
		e.loader.cfg.ScenarioVersion.EvaluationFocus = scenarioFocus
		e.store.SetFindings(e.user, map[learner.Skill]learner.FindingStat{learner.SkillFluency: {Minor: 1}}, 1)
		return e.compute(t).FocusSkills
	}
	require.Empty(t, focusOf(nil))
	require.Equal(t, []learner.Skill{learner.SkillFluency}, focusOf([]string{"FLUENCY"}))
	require.Empty(t, focusOf([]string{"NOT_A_SKILL"}), "an unknown focus name is ignored")
}

func TestComputeAdaptive_TopicNoveltyCountsTopicSessions(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	base := e.compute(t).TopicNovelty
	e.store.SetTopicSessions(e.user, e.rc.TopicID, 2)
	require.Equal(t, base+1, e.compute(t).TopicNovelty, "two sessions on this topic raise the novelty by 2 x 0.5")
	e.store.SetTopicSessions(e.user, uuid.Must(uuid.NewV7()), 4)
	require.Equal(t, base+1, e.compute(t).TopicNovelty, "sessions of another topic do not count")
}

func TestComputeAdaptive_DialectUsesVoiceAccent(t *testing.T) {
	dialect := func(accent content.Accent, level, pref string, sessions int) learner.DialectExposure {
		e := newAdaptiveEnv(t, 3, accent, content.FormalityPolite)
		e.patch(t, learner.PatchInput{SelfDeclaredLevel: set(level), Dialect: set(pref)})
		e.store.SetDialectSessions(e.user, sessions)
		return e.compute(t).DialectExposure
	}
	require.Equal(t, learner.DialectLight, dialect(content.AccentKansai, "N3", "ALLOW_DIALECT", 0))
	require.Equal(t, learner.DialectNone, dialect(content.AccentKanto, "N3", "ALLOW_DIALECT", 0), "the accent comes from the snapshot")
	require.Equal(t, learner.DialectNone, dialect(content.AccentKansai, "N3", "STANDARD_ONLY", 0))
	require.Equal(t, learner.DialectNone, dialect(content.AccentKansai, "N5", "ALLOW_DIALECT", 0), "Ld = (1.00 + 1.00)/2 < 2.0")
	require.Equal(t, learner.DialectFull, dialect(content.AccentKansai, "N1", "ALLOW_DIALECT", 2))
	require.Equal(t, learner.DialectLight, dialect(content.AccentKansai, "N1", "ALLOW_DIALECT", 1))
}

func TestComputeAdaptive_FormalityComesFromSnapshot(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	e.loader.cfg.ScenarioVersion.Formality = content.FormalityKeigo // the live row says otherwise
	require.Equal(t, learner.FormalityPolite, e.compute(t).Formality, "Config.ConfigSnapshot.Formality (D-014), not a reload")
}

func TestComputeAdaptive_ScenarioDifficultyComesFromLoadedVersion(t *testing.T) {
	for _, b := range []int{1, 4, 5} {
		e := newAdaptiveEnv(t, b, content.AccentKanto, content.FormalityPolite)
		require.Equal(t, b, e.compute(t).ScenarioDifficulty)
	}
}

func TestComputeAdaptive_KeigoSoftensForWeakGrammar(t *testing.T) {
	e := newAdaptiveEnv(t, 4, content.AccentKanto, content.FormalityKeigo)
	require.Equal(t, learner.FormalityKeigoLight, e.compute(t).Formality, "L(GRAMMAR) 1.00 < 2.5")
	e = newAdaptiveEnv(t, 4, content.AccentKanto, content.FormalityKeigo)
	e.patch(t, learner.PatchInput{SelfDeclaredLevel: set("N2")})
	require.Equal(t, learner.FormalityKeigo, e.compute(t).Formality)
}

func TestComputeAdaptive_UsesEffectiveLevelAndConfidence(t *testing.T) {
	e := newAdaptiveEnv(t, 5, content.AccentKanto, content.FormalityPolite)
	e.patch(t, learner.PatchInput{SelfDeclaredLevel: set("N5")})
	// GRAMMAR dropped to 2.00 after reaching 3.50: the effective level is max(2.00, 3.50 - 0.5) = 3.00
	ceiling, last := 3.50, svcNow
	e.saveState(t, e.user, learner.StateRow{
		Skill: learner.SkillGrammar, Level: 2.00, Confidence: 0.6, Ceiling: &ceiling, Trend: learner.TrendDeclining,
		EvidenceCount: 3, LastEvidenceAt: &last,
	})
	ad := e.compute(t)
	require.Equal(t, 3, ad.GrammarComplexity, "Stretch(3.00, 5) = R(3.3); the stored level 2.00 would give R(2.3) = 2")
}

func TestComputeAdaptive_DBErrorPropagates(t *testing.T) {
	boom := errors.New("connection reset")
	for _, method := range []string{"Ensure", "GetProfile", "ListStates", "RecentFindings", "TopicSessions90d", "DialectSessions90d"} {
		e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
		e.store.FailNext(method, boom)
		_, err := e.svc.ComputeAdaptiveDifficulty(context.Background(), learner.AdaptiveRequest{UserID: e.user, Config: e.rc})
		require.ErrorIs(t, err, boom, method)
	}
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	e.loader.err = boom
	_, err := e.svc.ComputeAdaptiveDifficulty(context.Background(), learner.AdaptiveRequest{UserID: e.user, Config: e.rc})
	require.ErrorIs(t, err, boom, "a content load failure is not papered over with a default")
	_, ok := e.store.Profile(e.user)
	require.False(t, ok, "nothing is written when the content cannot be loaded")
}

func TestComputeAdaptive_WithoutContentLoader(t *testing.T) {
	svc := learner.NewService(learner.Deps{Store: fakes.NewLearnerStore(), Clock: clock.NewFake(svcNow), IDs: idgen.NewSequence(t)})
	_, err := svc.ComputeAdaptiveDifficulty(context.Background(), learner.AdaptiveRequest{UserID: newUser()})
	require.Error(t, err)
}

func TestComputeAdaptive_OtherUsersStateIsInvisible(t *testing.T) {
	e := newAdaptiveEnv(t, 5, content.AccentKanto, content.FormalityPolite)
	other := newUser()
	e.saveState(t, other, learner.StateRow{
		Skill: learner.SkillGrammar, Level: 5, Confidence: 0.9, Trend: learner.TrendImproving, EvidenceCount: 3, LastEvidenceAt: ptr(svcNow),
	})
	e.store.SetFindings(other, map[learner.Skill]learner.FindingStat{learner.SkillGrammar: {Major: 3}}, 1)
	e.store.SetDialectSessions(other, 5)
	ad := e.compute(t)
	require.Equal(t, 3, ad.GrammarComplexity, "Stretch(1.00, 5) = clamp(R(1.3), 3, 5); the other user's 5.00 would give 5")
	require.Empty(t, ad.FocusSkills)
	require.Equal(t, learner.DialectNone, ad.DialectExposure)
}

func ptr[T any](v T) *T { return &v }

func TestPromptContext_ReadsNicknameOnly(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	e.patch(t, learner.PatchInput{
		Nickname: set("Budi"), LearningGoal: set("JOB_INTERVIEW"), LearningGoalNote: set("RAHASIA-CATATAN-123"),
		SelfDeclaredLevel: set("N4"),
	})
	ad := e.compute(t)
	pc, err := e.svc.PromptContext(context.Background(), e.user, ad)
	require.NoError(t, err)
	require.Equal(t, "Budi", pc.DisplayName)
	require.NotEmpty(t, pc.GoalLine)
	require.Contains(t, pc.Adaptation, "<conversation_difficulty>")
	require.Equal(t, learner.NewPromptContext(learner.ProfileSnapshot{Nickname: ptr("Budi"), LearningGoal: ptr("JOB_INTERVIEW")}, ad), pc)
	all := pc.DisplayName + pc.GoalLine + pc.Adaptation + strings.Join(pc.FocusSkills, ",")
	require.NotContains(t, all, "RAHASIA-CATATAN-123", "learning_goal_note never reaches the prompt")

	// no nickname: empty display name
	e2 := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	pc, err = e2.svc.PromptContext(context.Background(), e2.user, e2.compute(t))
	require.NoError(t, err)
	require.Empty(t, pc.DisplayName)
	require.Empty(t, pc.GoalLine)
}

func TestPromptContext_StoreErrorPropagates(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	boom := errors.New("connection reset")
	e.store.FailNext("GetProfile", boom)
	_, err := e.svc.PromptContext(context.Background(), e.user, learner.AdaptiveDifficulty{})
	require.ErrorIs(t, err, boom)
}

func TestRerender_NoStorage(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKanto, content.FormalityPolite)
	e.patch(t, learner.PatchInput{Nickname: set("Budi"), LearningGoal: set("TRAVEL")})
	ad := e.compute(t)
	pc, err := e.svc.PromptContext(context.Background(), e.user, ad)
	require.NoError(t, err)

	e.store.FailAlways(errors.New("the database is down"))
	ad.SpeechSpeed, ad.InSessionAdjustments = 5, 1
	got := e.svc.Rerender(pc, ad)
	require.Equal(t, learner.Rerender(pc, ad), got)
	require.Equal(t, "Budi", got.DisplayName)
	require.NotEqual(t, pc.Adaptation, got.Adaptation)
}

func TestAdaptiveDifficultyJSONRoundTrip(t *testing.T) {
	e := newAdaptiveEnv(t, 3, content.AccentKansai, content.FormalityPolite)
	raw, err := json.Marshal(e.compute(t))
	require.NoError(t, err)
	var back learner.AdaptiveDifficulty
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, learner.AlgorithmVersion, back.AlgorithmVersion)
}

// saveState writes a state through the store the way the worker would.
func (e *adaptiveEnv) saveState(t *testing.T, userID uuid.UUID, row learner.StateRow) {
	t.Helper()
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		if err := tx.Profiles().Ensure(ctx, userID); err != nil {
			return err
		}
		return tx.SkillStates().Save(ctx, userID, row, svcNow)
	}))
}
