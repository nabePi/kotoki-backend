package learner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var svcNow = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

type svcEnv struct {
	svc   *learner.Service
	store *fakes.LearnerStore
	clock *clock.Fake
}

func newSvc(t *testing.T) svcEnv {
	t.Helper()
	clk := clock.NewFake(svcNow)
	store := fakes.NewLearnerStore(fakes.WithLearnerClock(clk))
	return svcEnv{
		svc:   learner.NewService(learner.Deps{Store: store, Clock: clk, IDs: idgen.NewSequence(t)}),
		store: store, clock: clk,
	}
}

func newUser() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func set[T any](v T) learner.Field[T] { return learner.Field[T]{Set: true, Value: v} }

func setNull[T any]() learner.Field[T] { return learner.Field[T]{Set: true, Null: true} }

// violations returns the (field, reason) pairs of a VALIDATION_FAILED error.
func violations(t *testing.T, err error) map[string]string {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), err.Error())
	d, ok := errs.DetailsOf(err).(map[string]any)
	require.True(t, ok, "details are {fields: [...]}")
	list, ok := d["fields"].([]map[string]any)
	require.True(t, ok)
	out := map[string]string{}
	for _, f := range list {
		out[f["field"].(string)] = f["reason"].(string)
	}
	return out
}

// saveState writes a state through the store the way the worker would.
func (e svcEnv) saveState(t *testing.T, userID uuid.UUID, row learner.StateRow) {
	t.Helper()
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		if err := tx.Profiles().Ensure(ctx, userID); err != nil {
			return err
		}
		return tx.SkillStates().Save(ctx, userID, row, svcNow)
	}))
}

func evidenceState(skill learner.Skill, level, conf float64, trend learner.Trend, count int, last time.Time) learner.StateRow {
	ev := uuid.Must(uuid.NewV7())
	ceiling := level
	return learner.StateRow{
		Skill: skill, Level: level, Confidence: conf, Trend: trend, EvidenceCount: count,
		Ceiling: &ceiling, LastEvaluationID: &ev, LastEvidenceAt: &last,
	}
}

func skillOf(t *testing.T, v learner.SkillsView, k learner.Skill) learner.SkillView {
	t.Helper()
	for _, s := range v.Skills {
		if s.Skill == k {
			return s
		}
	}
	t.Fatalf("no skill %s", k)
	return learner.SkillView{}
}

func TestField_UnmarshalJSON(t *testing.T) {
	var in struct {
		A learner.Field[string] `json:"a"`
		B learner.Field[string] `json:"b"`
		C learner.Field[bool]   `json:"c"`
		D learner.Field[string] `json:"d"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"a":"x","b":null,"c":false}`), &in))
	require.Equal(t, learner.Field[string]{Set: true, Value: "x"}, in.A)
	require.Equal(t, learner.Field[string]{Set: true, Null: true}, in.B)
	require.Equal(t, learner.Field[bool]{Set: true, Value: false}, in.C, "false is set, not absent")
	require.Equal(t, learner.Field[string]{}, in.D, "an absent key stays unset")
	require.Error(t, json.Unmarshal([]byte(`{"a":5}`), &in), "a wrong type is a decode error")
}

func TestPatch_EnumValidation(t *testing.T) {
	cases := []struct {
		name   string
		in     learner.PatchInput
		field  string
		reason string
	}{
		{"self level", learner.PatchInput{SelfDeclaredLevel: set("N9")}, "self_declared_level", "invalid_format"},
		{"self level lower case", learner.PatchInput{SelfDeclaredLevel: set("n4")}, "self_declared_level", "invalid_format"},
		{"target level BEGINNER", learner.PatchInput{TargetLevel: set("BEGINNER")}, "target_level", "invalid_format"},
		{"goal", learner.PatchInput{LearningGoal: set("HOBBY")}, "learning_goal", "invalid_format"},
		{"goal note 301 runes", learner.PatchInput{LearningGoalNote: set(strings.Repeat("あ", 301))}, "learning_goal_note", "out_of_range"},
		{"speed", learner.PatchInput{SpeechSpeed: set("FAST")}, "preferences.speech_speed", "invalid_format"},
		{"formality", learner.PatchInput{Formality: set("KEIGO")}, "preferences.formality", "invalid_format"},
		{"dialect", learner.PatchInput{Dialect: set("FULL")}, "preferences.dialect", "invalid_format"},
		{"null speed", learner.PatchInput{SpeechSpeed: setNull[string]()}, "preferences.speech_speed", "invalid_format"},
		{"null memory", learner.PatchInput{MemoryEnabled: setNull[bool]()}, "preferences.memory_enabled", "invalid_format"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSvc(t)
			u := newUser()
			_, err := e.svc.PatchProfile(context.Background(), u, c.in)
			require.Equal(t, map[string]string{c.field: c.reason}, violations(t, err))
			_, ok := e.store.Profile(u)
			require.False(t, ok, "a rejected patch writes nothing, not even the profile")
		})
	}

	t.Run("accepted values", func(t *testing.T) {
		e := newSvc(t)
		got, err := e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{
			SelfDeclaredLevel: set("BEGINNER"), TargetLevel: set("N1"), LearningGoal: set("LIVING_IN_JAPAN"),
			LearningGoalNote: set(strings.Repeat("あ", 300)), SpeechSpeed: set("FASTER"), Formality: set("CASUAL"),
			Dialect: set("ALLOW_DIALECT"), MemoryEnabled: set(false),
		})
		require.NoError(t, err)
		require.Equal(t, "BEGINNER", *got.SelfDeclaredLevel)
		require.Equal(t, "N1", *got.TargetLevel)
		require.Equal(t, "LIVING_IN_JAPAN", *got.LearningGoal)
		require.Equal(t, "FASTER", got.PreferredSpeechSpeed)
		require.Equal(t, "CASUAL", got.PreferredFormality)
		require.Equal(t, "ALLOW_DIALECT", got.DialectPreference)
		require.False(t, got.MemoryEnabled)
	})

	t.Run("every violation is reported at once", func(t *testing.T) {
		e := newSvc(t)
		_, err := e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{
			SelfDeclaredLevel: set("N9"), Nickname: set(""), SpeechSpeed: set("x"),
		})
		require.Equal(t, map[string]string{
			"self_declared_level": "invalid_format", "nickname": "out_of_range", "preferences.speech_speed": "invalid_format",
		}, violations(t, err))
	})
}

func TestPatch_NicknameRules(t *testing.T) {
	ok := []struct{ in, want string }{
		{" Budi ", "Budi"},
		{"ｈａｎａ", "hana"},   // NFKC folds full-width letters
		{"　Budi　", "Budi"}, // NFKC turns the ideographic space into one TrimSpace removes
		{strings.Repeat("あ", 30), strings.Repeat("あ", 30)}, // 30 runes is the limit
		{"山田 太郎", "山田 太郎"},
	}
	for _, c := range ok {
		e := newSvc(t)
		got, err := e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{Nickname: set(c.in)})
		require.NoError(t, err, c.in)
		require.NotNil(t, got.Nickname)
		require.Equal(t, c.want, *got.Nickname, c.in)
	}

	bad := []struct{ name, in, reason string }{
		{"empty", "", "out_of_range"},
		{"blank", "   ", "out_of_range"},
		{"31 runes", strings.Repeat("あ", 31), "out_of_range"},
		{"31 runes after trim", " " + strings.Repeat("a", 31) + " ", "out_of_range"},
		{"brace", "a{b", "invalid_format"},
		{"close brace", "ab}", "invalid_format"},
		{"angle open", "<b>", "invalid_format"},
		{"angle close", "a>b", "invalid_format"},
		{"newline", "a\nb", "invalid_format"},
		{"tab", "a\tb", "invalid_format"},
		{"control", "a\x00b", "invalid_format"},
		{"carriage return", "ab\r", "invalid_format"},
		{"line separator", "a\u2028b", "invalid_format"},
		{"paragraph separator", "a\u2029b", "invalid_format"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			e := newSvc(t)
			_, err := e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{Nickname: set(c.in)})
			require.Equal(t, map[string]string{"nickname": c.reason}, violations(t, err))
		})
	}

	t.Run("null clears", func(t *testing.T) {
		e := newSvc(t)
		u := newUser()
		_, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{Nickname: set("Budi")})
		require.NoError(t, err)
		got, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{Nickname: setNull[string]()})
		require.NoError(t, err)
		require.Nil(t, got.Nickname)
	})
}

func TestPatch_OnboardingResetsPriorsWhenNoEvidence(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	got, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{SelfDeclaredLevel: set("N4")})
	require.NoError(t, err)
	n4 := "N4"
	for _, s := range e.store.States(u) {
		p := learner.PriorFor(&n4, s.Skill)
		require.InDelta(t, p.Level, s.PriorLevel, 1e-9, s.Skill)
		require.InDelta(t, p.Level, s.Level, 1e-9, s.Skill)
		require.InDelta(t, p.Confidence, s.PriorConfidence, 1e-9, s.Skill)
		require.InDelta(t, p.Confidence, s.Confidence, 1e-9, s.Skill)
	}
	require.NotNil(t, got.OnboardingCompletedAt)
	require.True(t, got.OnboardingCompletedAt.Equal(svcNow))
	first := *got.OnboardingCompletedAt

	e.clock.Advance(48 * time.Hour)
	got, err = e.svc.PatchProfile(context.Background(), u, learner.PatchInput{SelfDeclaredLevel: set("N2")})
	require.NoError(t, err)
	n2 := "N2"
	for _, s := range e.store.States(u) {
		p := learner.PriorFor(&n2, s.Skill)
		require.InDelta(t, p.Level, s.Level, 1e-9, "the priors are replaced: "+string(s.Skill))
		require.InDelta(t, p.Level, s.PriorLevel, 1e-9, s.Skill)
	}
	require.True(t, got.OnboardingCompletedAt.Equal(first), "onboarding_completed_at is set once")

	// clearing the level goes back to the skipped-onboarding prior
	_, err = e.svc.PatchProfile(context.Background(), u, learner.PatchInput{SelfDeclaredLevel: setNull[string]()})
	require.NoError(t, err)
	for _, s := range e.store.States(u) {
		require.InDelta(t, 1.00, s.Level, 1e-9, s.Skill)
		require.InDelta(t, 0.050, s.Confidence, 1e-9, s.Skill)
	}

	// a patch without the level leaves the priors alone
	_, err = e.svc.PatchProfile(context.Background(), u, learner.PatchInput{SelfDeclaredLevel: set("N3")})
	require.NoError(t, err)
	_, err = e.svc.PatchProfile(context.Background(), u, learner.PatchInput{Nickname: set("Budi")})
	require.NoError(t, err)
	n3 := "N3"
	for _, s := range e.store.States(u) {
		require.InDelta(t, learner.PriorFor(&n3, s.Skill).Level, s.Level, 1e-9, s.Skill)
	}
}

func TestPatch_OnboardingAfterEvidenceKeepsStates(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 2.4, 0.4, learner.TrendStable, 1, svcNow.Add(-time.Hour)))
	before := e.store.States(u)

	got, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{SelfDeclaredLevel: set("N1")})
	require.NoError(t, err)
	require.Equal(t, before, e.store.States(u), "no state is touched once there is evidence")
	require.Equal(t, "N1", *got.SelfDeclaredLevel, "the profile column still changes")
	require.Nil(t, got.OnboardingCompletedAt, "onboarding is not completed by a late level")
}

func TestPatch_NullDeletesNullableFields(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	_, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{
		Nickname: set("Budi"), SelfDeclaredLevel: set("N4"), TargetLevel: set("N2"),
		LearningGoal: set("JOB_INTERVIEW"), LearningGoalNote: set("catatan"),
	})
	require.NoError(t, err)
	got, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{
		Nickname: setNull[string](), TargetLevel: setNull[string](), LearningGoal: setNull[string](), LearningGoalNote: setNull[string](),
	})
	require.NoError(t, err)
	require.Nil(t, got.Nickname)
	require.Nil(t, got.TargetLevel)
	require.Nil(t, got.LearningGoal)
	require.Nil(t, got.LearningGoalNote)
	require.Equal(t, "N4", *got.SelfDeclaredLevel, "a key that is absent keeps its value")
}

func TestPatch_EmptyPatchChangesNothing(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	want, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{Nickname: set("Budi"), MemoryEnabled: set(false)})
	require.NoError(t, err)
	e.clock.Advance(time.Hour)
	got, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{})
	require.NoError(t, err)
	require.Equal(t, "Budi", *got.Nickname)
	require.False(t, got.MemoryEnabled)
	require.Equal(t, want.PreferredSpeechSpeed, got.PreferredSpeechSpeed)
	require.Equal(t, want.CreatedAt, got.CreatedAt)
	require.Equal(t, want.UpdatedAt, got.UpdatedAt, "an empty patch writes nothing")
}

func TestPatch_ClearingTheLevelDoesNotCompleteOnboarding(t *testing.T) {
	e := newSvc(t)
	got, err := e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{SelfDeclaredLevel: setNull[string]()})
	require.NoError(t, err)
	require.Nil(t, got.SelfDeclaredLevel)
	require.Nil(t, got.OnboardingCompletedAt, "skipping onboarding is not completing it")
}

func TestGetProfile_CreatesProfileAndReportsStats(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	last := svcNow.Add(-2 * time.Hour)
	e.store.SetStats(u, learner.ProfileStats{CompletedSessions: 4, PracticeSeconds: 900, LastSessionAt: &last})
	got, err := e.svc.GetProfile(context.Background(), u)
	require.NoError(t, err)
	require.Equal(t, u, got.UserID)
	require.Nil(t, got.Nickname)
	require.True(t, got.MemoryEnabled)
	require.Equal(t, 4, got.CompletedSessionCount)
	require.EqualValues(t, 900, got.TotalPracticeSeconds)
	require.True(t, got.LastSessionAt.Equal(last))
	require.InDelta(t, 1.00, got.OverallLevel, 1e-9, "default priors average to 1.00")
	require.Equal(t, "N5", got.OverallLevelLabel)
	_, ok := e.store.Profile(u)
	require.True(t, ok, "a read creates the profile (EnsureProfile)")
	require.Len(t, e.store.States(u), 6)
}

func TestOverallLevelWeighted(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	e.saveState(t, u, learner.StateRow{Skill: learner.SkillGrammar, Level: 4.0, Confidence: 0.9, Trend: learner.TrendInsufficient})
	e.saveState(t, u, learner.StateRow{Skill: learner.SkillVocabulary, Level: 2.0, Confidence: 0.1, Trend: learner.TrendInsufficient})
	// a confidence of 0.01 weighs 0.05, not 0.01
	e.saveState(t, u, learner.StateRow{Skill: learner.SkillFluency, Level: 5.0, Confidence: 0.01, Trend: learner.TrendInsufficient})
	// the weights: 0.9 + 0.1 + 0.05x4 = 1.2; the sum: 3.6 + 0.2 + 0.25 + 0.05x3 = 4.2
	got, err := e.svc.GetProfile(context.Background(), u)
	require.NoError(t, err)
	require.Equal(t, 3.50, got.OverallLevel)
	require.Equal(t, "N2", got.OverallLevelLabel)
}

func TestOverallLevelUsesDecayedConfidenceAndRoundsTo2Decimals(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	// GRAMMAR's evidence is 90 days old: its confidence 0.8 halves to 0.4; the rest weigh 0.05
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 4.0, 0.8, learner.TrendStable, 2, svcNow.AddDate(0, 0, -90)))
	got, err := e.svc.GetProfile(context.Background(), u)
	require.NoError(t, err)
	// (4.0x0.4 + 5x0.05x1.0) / (0.4 + 0.25) = 1.85 / 0.65 = 2.846153...
	require.Equal(t, 2.85, got.OverallLevel)
}

func TestSkills_SixInD097Order_ProvisionalFlags(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	recent := svcNow.Add(-time.Hour)
	e.saveState(t, u, evidenceState(learner.SkillFluency, 2.50, 0.50, learner.TrendStable, 3, recent))           // 3 and 0.50: firm
	e.saveState(t, u, evidenceState(learner.SkillNaturalness, 2.50, 0.29, learner.TrendStable, 5, recent))       // low confidence
	e.saveState(t, u, evidenceState(learner.SkillListeningResponse, 2.50, 0.60, learner.TrendStable, 2, recent)) // few evidence
	e.saveState(t, u, evidenceState(learner.SkillInteractionComplexity, 2.50, 0.30, learner.TrendStable, 3, recent))

	v, err := e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	require.Len(t, v.Skills, 6)
	for i, k := range learner.AllSkills() {
		require.Equal(t, k, v.Skills[i].Skill, "D-097 order")
	}
	require.Equal(t, learner.AlgorithmVersion, v.AlgorithmVersion)

	g := skillOf(t, v, learner.SkillGrammar) // untouched prior
	require.True(t, g.IsProvisional)
	require.Equal(t, learner.TrendInsufficient, g.Trend)
	require.Nil(t, g.LastAssessedAt)
	require.Nil(t, g.DemonstratedCeiling)
	require.Zero(t, g.EvidenceCount)
	require.InDelta(t, 1.00, g.EstimatedLevel, 1e-9)
	require.Equal(t, "N5", g.LevelLabel)
	require.Equal(t, "LOW", g.ConfidenceLabel)

	f := skillOf(t, v, learner.SkillFluency)
	require.False(t, f.IsProvisional)
	require.Equal(t, 3, f.EvidenceCount)
	require.Equal(t, learner.TrendStable, f.Trend)
	require.InDelta(t, 0.50, f.Confidence, 1e-9)
	require.Equal(t, "MEDIUM", f.ConfidenceLabel)
	require.Equal(t, "N3", f.LevelLabel)
	require.NotNil(t, f.LastAssessedAt)
	require.True(t, f.LastAssessedAt.Equal(recent))
	require.NotNil(t, f.DemonstratedCeiling)
	require.InDelta(t, 2.50, *f.DemonstratedCeiling, 1e-9)

	require.True(t, skillOf(t, v, learner.SkillNaturalness).IsProvisional, "confidence 0.29 < 0.30")
	require.Equal(t, "LOW", skillOf(t, v, learner.SkillNaturalness).ConfidenceLabel)
	require.True(t, skillOf(t, v, learner.SkillListeningResponse).IsProvisional, "2 evidence < 3")
	require.Equal(t, "HIGH", skillOf(t, v, learner.SkillListeningResponse).ConfidenceLabel)
	require.False(t, skillOf(t, v, learner.SkillInteractionComplexity).IsProvisional, "confidence 0.30 is not < 0.30")
}

func TestSkills_ConfidenceIsDecayedEffective(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 3.0, 0.80, learner.TrendStable, 4, svcNow.AddDate(0, 0, -90)))
	e.saveState(t, u, evidenceState(learner.SkillVocabulary, 3.0, 0.80, learner.TrendStable, 4, svcNow))
	v, err := e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	g := skillOf(t, v, learner.SkillGrammar)
	require.InDelta(t, 0.40, g.Confidence, 1e-9, "0.80 x 0.5^(90/90)")
	require.Equal(t, "MEDIUM", g.ConfidenceLabel)
	require.False(t, g.IsProvisional)
	require.InDelta(t, 3.0, g.EstimatedLevel, 1e-9, "the estimate itself does not decay")
	require.InDelta(t, 0.80, skillOf(t, v, learner.SkillVocabulary).Confidence, 1e-9)

	e.clock.Advance(180 * 24 * time.Hour)
	v, err = e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	require.InDelta(t, 0.80*0.125, skillOf(t, v, learner.SkillGrammar).Confidence, 1e-9, "270 days: three half-lives")
	require.True(t, skillOf(t, v, learner.SkillGrammar).IsProvisional, "a decayed confidence below 0.30 is provisional again")
}

func TestSkills_FocusSkillsWithoutScenarioFocus(t *testing.T) { // spec 12.7 worked example
	e := newSvc(t)
	u := newUser()
	now := svcNow
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 2.35, 0.50, learner.TrendImproving, 3, now))
	e.saveState(t, u, evidenceState(learner.SkillVocabulary, 3.10, 0.50, learner.TrendStable, 3, now))
	e.saveState(t, u, evidenceState(learner.SkillFluency, 2.40, 0.45, learner.TrendStable, 3, now))
	e.saveState(t, u, evidenceState(learner.SkillNaturalness, 2.20, 0.50, learner.TrendStable, 3, now))
	e.saveState(t, u, evidenceState(learner.SkillListeningResponse, 2.90, 0.50, learner.TrendStable, 3, now))
	e.saveState(t, u, evidenceState(learner.SkillInteractionComplexity, 2.00, 0.10, learner.TrendStable, 3, now))
	e.store.SetFindings(u, map[learner.Skill]learner.FindingStat{
		learner.SkillGrammar: {Major: 2, Minor: 4}, learner.SkillNaturalness: {Minor: 3},
	}, 3)
	v, err := e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	require.Equal(t, []learner.Skill{learner.SkillGrammar, learner.SkillNaturalness}, v.FocusSkills)

	// without findings the same levels give no focus: P stays below 0.30 everywhere
	e.store.SetFindings(u, nil, 0)
	v, err = e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	require.Equal(t, []learner.Skill{}, v.FocusSkills, "empty, never nil")
}

func TestSkills_FocusUsesTheUsersOwnFindings(t *testing.T) {
	e := newSvc(t)
	a, b := newUser(), newUser()
	// 9 MAJOR GRAMMAR findings over one evaluation: f = 1, so P_GRAMMAR = 0.5 for a user whose levels are even
	e.store.SetFindings(a, map[learner.Skill]learner.FindingStat{learner.SkillGrammar: {Major: 9}}, 1)
	va, err := e.svc.Skills(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, []learner.Skill{learner.SkillGrammar}, va.FocusSkills)
	vb, err := e.svc.Skills(context.Background(), b)
	require.NoError(t, err)
	require.Equal(t, []learner.Skill{}, vb.FocusSkills, "A's findings are not B's")
}

func TestSkills_FocusUsesEffectiveLevelAndConfidence(t *testing.T) {
	ctx := context.Background()
	// GRAMMAR scored 4.0 once and now reads 1.0, but the adaptation level is max(1.0, 4.0-0.5) = 3.5, level with the rest
	e := newSvc(t)
	u := newUser()
	g := evidenceState(learner.SkillGrammar, 1.0, 0.9, learner.TrendStable, 4, svcNow)
	c := 4.0
	g.Ceiling = &c
	e.saveState(t, u, g)
	for _, k := range learner.AllSkills()[1:] {
		e.saveState(t, u, evidenceState(k, 3.5, 0.9, learner.TrendStable, 4, svcNow))
	}
	v, err := e.svc.Skills(ctx, u)
	require.NoError(t, err)
	require.Empty(t, v.FocusSkills, "the ceiling lifts the effective level, so there is no gap")
	require.InDelta(t, 1.0, skillOf(t, v, learner.SkillGrammar).EstimatedLevel, 1e-9, "the view still shows the estimate")

	// the same gap with a confidence that has decayed over 270 days: 0.9 x 0.125 = 0.1125; P = 0.1125 x 2.08 = 0.23 < 0.30
	e = newSvc(t)
	u = newUser()
	old := svcNow.AddDate(0, 0, -270)
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 1.0, 0.9, learner.TrendStable, 4, old))
	for _, k := range learner.AllSkills()[1:] {
		e.saveState(t, u, evidenceState(k, 3.5, 0.9, learner.TrendStable, 4, svcNow))
	}
	v, err = e.svc.Skills(ctx, u)
	require.NoError(t, err)
	require.Empty(t, v.FocusSkills, "a decayed confidence does not push a skill into the focus")

	// fresh confidence, same gap: the focus appears (guards the two cases above)
	e = newSvc(t)
	u = newUser()
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 1.0, 0.9, learner.TrendStable, 4, svcNow))
	for _, k := range learner.AllSkills()[1:] {
		e.saveState(t, u, evidenceState(k, 3.5, 0.9, learner.TrendStable, 4, svcNow))
	}
	v, err = e.svc.Skills(ctx, u)
	require.NoError(t, err)
	require.Equal(t, []learner.Skill{learner.SkillGrammar}, v.FocusSkills)
}

func TestSkills_FocusReadsTrends(t *testing.T) {
	// DECLINING adds 0.30 to P: a skill level with the others still enters the focus
	e := newSvc(t)
	u := newUser()
	for _, k := range learner.AllSkills() {
		tr := learner.TrendStable
		if k == learner.SkillFluency {
			tr = learner.TrendDeclining
		}
		e.saveState(t, u, evidenceState(k, 3.0, 0.5, tr, 4, svcNow))
	}
	v, err := e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	require.Equal(t, []learner.Skill{learner.SkillFluency}, v.FocusSkills)
}

func TestVoiceExposure_ShareAndSuggestion(t *testing.T) {
	rows := func(fk, fs int, ms int) []learner.ExposureRow {
		return []learner.ExposureRow{
			{Gender: "FEMALE", Accent: "KANTO", Sessions: fk, Seconds: int64(fs)},
			{Gender: "MALE", Accent: "KANSAI", Sessions: 2, Seconds: int64(ms)},
		}
	}
	cases := []struct {
		name       string
		rows       []learner.ExposureRow
		suggestion bool
		shares     []float64 // in the order of Items
	}{
		{"balanced", rows(5, 3000, 1800), false, []float64{0.63, 0.38}}, // 3000/4800, 1800/4800
		{"over 80 percent with 5 sessions", rows(3, 9000, 1000), true, []float64{0.90, 0.10}},
		{"exactly 80 percent is not over", rows(3, 800, 200), false, []float64{0.80, 0.20}},
		{"over 80 percent but 4 sessions", rows(2, 9000, 1000), false, []float64{0.90, 0.10}},
		{"zero seconds", []learner.ExposureRow{{Gender: "FEMALE", Accent: "KANTO", Sessions: 6}}, false, []float64{0}},
		{"single combination", []learner.ExposureRow{{Gender: "FEMALE", Accent: "KANTO", Sessions: 6, Seconds: 600}}, true, []float64{1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSvc(t)
			u := newUser()
			e.store.SetExposure(u, c.rows)
			v, err := e.svc.Skills(context.Background(), u)
			require.NoError(t, err)
			require.Equal(t, 90, v.VoiceExposure.WindowDays)
			require.Len(t, v.VoiceExposure.Items, len(c.shares))
			for i, want := range c.shares {
				require.Equal(t, want, v.VoiceExposure.Items[i].Share, "share %d", i)
			}
			if c.suggestion {
				require.NotNil(t, v.VoiceExposure.ExposureSuggestion)
				require.Equal(t, "TRY_OTHER_VOICE", *v.VoiceExposure.ExposureSuggestion)
			} else {
				require.Nil(t, v.VoiceExposure.ExposureSuggestion)
			}
		})
	}

	t.Run("no sessions", func(t *testing.T) {
		e := newSvc(t)
		v, err := e.svc.Skills(context.Background(), newUser())
		require.NoError(t, err)
		require.Equal(t, learner.VoiceExposureView{WindowDays: 90, Items: []learner.ExposureItem{}}, v.VoiceExposure)
	})

	t.Run("items are ordered by seconds then voice", func(t *testing.T) {
		e := newSvc(t)
		u := newUser()
		e.store.SetExposure(u, []learner.ExposureRow{
			{Gender: "MALE", Accent: "KANSAI", Sessions: 1, Seconds: 100},
			{Gender: "MALE", Accent: "KANTO", Sessions: 1, Seconds: 300},
			{Gender: "FEMALE", Accent: "KANTO", Sessions: 1, Seconds: 100},
		})
		v, err := e.svc.Skills(context.Background(), u)
		require.NoError(t, err)
		got := []string{}
		for _, it := range v.VoiceExposure.Items {
			got = append(got, it.Gender+"/"+it.Accent)
		}
		require.Equal(t, []string{"MALE/KANTO", "FEMALE/KANTO", "MALE/KANSAI"}, got)
	})
}

func TestResetSkills_RestoresPriorsDeletesEstimates(t *testing.T) {
	e := newSvc(t)
	u, other := newUser(), newUser()
	ctx := context.Background()
	_, err := e.svc.PatchProfile(ctx, u, learner.PatchInput{SelfDeclaredLevel: set("N3"), Nickname: set("Budi")})
	require.NoError(t, err)
	e.saveState(t, u, evidenceState(learner.SkillGrammar, 4.1, 0.7, learner.TrendImproving, 6, svcNow))
	e.saveState(t, other, evidenceState(learner.SkillGrammar, 4.1, 0.7, learner.TrendImproving, 6, svcNow))
	insert := func(user uuid.UUID) {
		require.NoError(t, e.store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
			ok, err := tx.Estimates().Insert(ctx, user, learner.EstimateRow{
				ID: uuid.Must(uuid.NewV7()), EvaluationID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()),
				Skill: learner.SkillGrammar, AlgorithmVersion: 1, RawScore: 70, LObs: 3, CObs: 0.5, AssessedAt: svcNow,
				Previous: 3, Level: 4.1, Confidence: 0.7, Count: 6, Trend: learner.TrendImproving,
			})
			require.True(t, ok)
			return err
		}))
	}
	insert(u)
	insert(other)
	env := events.Envelope{EventID: uuid.Must(uuid.NewV7()), EventType: "evaluation.completed"}
	require.NoError(t, e.store.WithinTx(ctx, func(ctx context.Context, tx learner.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueLearner, env)
	}))

	require.NoError(t, e.svc.ResetSkills(ctx, u))
	n3 := "N3"
	for _, s := range e.store.States(u) {
		p := learner.PriorFor(&n3, s.Skill)
		require.InDelta(t, p.Level, s.Level, 1e-9, s.Skill)
		require.InDelta(t, p.Confidence, s.Confidence, 1e-9, s.Skill)
		require.Nil(t, s.Ceiling)
		require.Equal(t, learner.TrendInsufficient, s.Trend)
		require.Zero(t, s.EvidenceCount)
		require.Nil(t, s.LastEvidenceAt)
		require.Nil(t, s.LastEvaluationID)
	}
	require.Empty(t, e.store.Estimates(u))
	prof, ok := e.store.Profile(u)
	require.True(t, ok)
	require.Equal(t, "Budi", *prof.Nickname, "the profile is not touched")
	require.Equal(t, 1, e.store.Processed.Len(), "processed_events survive a reset")

	// the other user is untouched
	require.Len(t, e.store.Estimates(other), 1)
	require.Equal(t, 6, stateOfStore(t, e.store.States(other), learner.SkillGrammar).EvidenceCount)

	// idempotent
	require.NoError(t, e.svc.ResetSkills(ctx, u))
	require.Empty(t, e.store.Estimates(u))
	for _, s := range e.store.States(u) {
		require.Zero(t, s.EvidenceCount)
	}

	// a user without a profile still resets (the profile is created)
	require.NoError(t, e.svc.ResetSkills(ctx, newUser()))
}

func stateOfStore(t *testing.T, rows []learner.StateRow, k learner.Skill) learner.StateRow {
	t.Helper()
	for _, r := range rows {
		if r.Skill == k {
			return r
		}
	}
	t.Fatalf("no state %s", k)
	return learner.StateRow{}
}

func insertEstimates(t *testing.T, e svcEnv, user uuid.UUID, skill learner.Skill, n int) []learner.EstimateRow {
	t.Helper()
	var rows []learner.EstimateRow
	require.NoError(t, e.store.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		if err := tx.Profiles().Ensure(ctx, user); err != nil {
			return err
		}
		for i := range n {
			d := 3
			r := learner.EstimateRow{
				ID: uuid.Must(uuid.NewV7()), EvaluationID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7()),
				Skill: skill, AlgorithmVersion: 1, RawScore: 60, DEff: &d, LObs: 2.5, CObs: 0.5,
				AssessedAt: svcNow.Add(time.Duration(i) * time.Hour), Previous: 2, Level: 2.1 + float64(i)/100, Confidence: 0.3,
				Count: i + 1, Trend: learner.TrendStable,
			}
			if _, err := tx.Estimates().Insert(ctx, user, r); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return nil
	}))
	return rows
}

func TestHistory_Paging(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	rows := insertEstimates(t, e, u, learner.SkillGrammar, 5)
	insertEstimates(t, e, u, learner.SkillFluency, 2)

	page1, next, err := e.svc.History(context.Background(), u, learner.SkillGrammar, nil, 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Equal(t, rows[4].ID, page1[0].ID, "newest first")
	require.Equal(t, rows[3].ID, page1[1].ID)
	require.NotNil(t, next)
	require.Equal(t, learner.HistoryCursor{AssessedAt: page1[1].AssessedAt, ID: page1[1].ID}, *next)

	page2, next, err := e.svc.History(context.Background(), u, learner.SkillGrammar, next, 2)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{rows[2].ID, rows[1].ID}, []uuid.UUID{page2[0].ID, page2[1].ID})
	require.NotNil(t, next)

	page3, next, err := e.svc.History(context.Background(), u, learner.SkillGrammar, next, 2)
	require.NoError(t, err)
	require.Len(t, page3, 1, "the last page is short")
	require.Nil(t, next, "and has no next cursor")

	exact, next, err := e.svc.History(context.Background(), u, learner.SkillFluency, nil, 2)
	require.NoError(t, err)
	require.Len(t, exact, 2)
	require.Nil(t, next, "exactly one full page leaves no cursor (limit+1 probe)")

	def, _, err := e.svc.History(context.Background(), u, learner.SkillGrammar, nil, 0)
	require.NoError(t, err)
	require.Len(t, def, 5, "limit 0 means the default 20")

	empty, next, err := e.svc.History(context.Background(), u, learner.SkillNaturalness, nil, 20)
	require.NoError(t, err)
	require.Empty(t, empty)
	require.Nil(t, next)
}

func TestHistory_LimitIsCapped(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	insertEstimates(t, e, u, learner.SkillGrammar, 105)
	got, next, err := e.svc.History(context.Background(), u, learner.SkillGrammar, nil, 1000)
	require.NoError(t, err)
	require.Len(t, got, 100, "the maximum page is 100")
	require.NotNil(t, next)
}

func TestHistory_InvalidCursor(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	for name, c := range map[string]learner.HistoryCursor{
		"zero":      {},
		"no id":     {AssessedAt: svcNow},
		"no time":   {ID: uuid.Must(uuid.NewV7())},
		"nil uuid":  {AssessedAt: svcNow, ID: uuid.Nil},
		"zero time": {AssessedAt: time.Time{}, ID: uuid.Must(uuid.NewV7())},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := e.svc.History(context.Background(), u, learner.SkillGrammar, &c, 20)
			require.Equal(t, errs.CodeInvalidCursor, errs.CodeOf(err))
		})
	}
}

func TestHistory_UnknownSkill(t *testing.T) {
	e := newSvc(t)
	_, _, err := e.svc.History(context.Background(), newUser(), learner.Skill("TASK_ACHIEVEMENT"), nil, 20)
	require.Equal(t, map[string]string{"skill": "invalid_format"}, violations(t, err))
	_, _, err = e.svc.History(context.Background(), newUser(), learner.Skill(""), nil, 20)
	require.Equal(t, map[string]string{"skill": "invalid_format"}, violations(t, err))
}

func TestService_UsersAreIsolated(t *testing.T) {
	e := newSvc(t)
	ctx := context.Background()
	a, b := newUser(), newUser()
	_, err := e.svc.PatchProfile(ctx, b, learner.PatchInput{Nickname: set("Bee"), SelfDeclaredLevel: set("N1"), MemoryEnabled: set(false)})
	require.NoError(t, err)
	e.saveState(t, b, evidenceState(learner.SkillGrammar, 4.9, 0.9, learner.TrendImproving, 9, svcNow))
	insertEstimates(t, e, b, learner.SkillGrammar, 3)

	pa, err := e.svc.GetProfile(ctx, a)
	require.NoError(t, err)
	require.Nil(t, pa.Nickname)
	require.True(t, pa.MemoryEnabled)
	require.Nil(t, pa.SelfDeclaredLevel)

	sa, err := e.svc.Skills(ctx, a)
	require.NoError(t, err)
	require.Zero(t, skillOf(t, sa, learner.SkillGrammar).EvidenceCount)
	hist, _, err := e.svc.History(ctx, a, learner.SkillGrammar, nil, 20)
	require.NoError(t, err)
	require.Empty(t, hist)

	// A's writes and reset leave B alone
	_, err = e.svc.PatchProfile(ctx, a, learner.PatchInput{Nickname: set("Aye"), SelfDeclaredLevel: set("N5")})
	require.NoError(t, err)
	require.NoError(t, e.svc.ResetSkills(ctx, a))
	pb, err := e.svc.GetProfile(ctx, b)
	require.NoError(t, err)
	require.Equal(t, "Bee", *pb.Nickname)
	require.False(t, pb.MemoryEnabled)
	require.Equal(t, 9, skillOf(t, mustSkills(t, e, b), learner.SkillGrammar).EvidenceCount)
	require.Len(t, e.store.Estimates(b), 3)
}

func mustSkills(t *testing.T, e svcEnv, u uuid.UUID) learner.SkillsView {
	t.Helper()
	v, err := e.svc.Skills(context.Background(), u)
	require.NoError(t, err)
	return v
}

func TestService_StoreFailuresSurface(t *testing.T) {
	boom := errors.New("db down")
	ctx := context.Background()

	e := newSvc(t)
	e.store.FailNext("Ensure", boom)
	_, err := e.svc.GetProfile(ctx, newUser())
	require.ErrorIs(t, err, boom)

	e = newSvc(t)
	e.store.FailNext("ListStates", boom)
	_, err = e.svc.Skills(ctx, newUser())
	require.ErrorIs(t, err, boom)

	e = newSvc(t)
	e.store.FailNext("RecentFindings", boom)
	_, err = e.svc.Skills(ctx, newUser())
	require.ErrorIs(t, err, boom)

	e = newSvc(t)
	e.store.FailNext("VoiceExposure", boom)
	_, err = e.svc.Skills(ctx, newUser())
	require.ErrorIs(t, err, boom)

	e = newSvc(t)
	u := newUser()
	e.store.FailNext("UpdateProfile", boom)
	_, err = e.svc.PatchProfile(ctx, u, learner.PatchInput{Nickname: set("Budi")})
	require.ErrorIs(t, err, boom)
	_, ok := e.store.Profile(u)
	require.False(t, ok, "the failed patch rolled back, even the Ensure")

	e = newSvc(t)
	u = newUser()
	e.store.FailNext("DeleteAll", boom)
	require.ErrorIs(t, e.svc.ResetSkills(ctx, u), boom)
	_, ok = e.store.Profile(u)
	require.False(t, ok)

	e = newSvc(t)
	e.store.FailNext("History", boom)
	_, _, err = e.svc.History(ctx, newUser(), learner.SkillGrammar, nil, 5)
	require.ErrorIs(t, err, boom)
}

func TestPatch_OnboardingIsAtomicWithTheProfileWrite(t *testing.T) {
	e := newSvc(t)
	u := newUser()
	boom := errors.New("db down")
	e.store.FailNext("UpdateProfile", boom) // after ResetPriors ran
	_, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{SelfDeclaredLevel: set("N1")})
	require.ErrorIs(t, err, boom)
	require.Empty(t, e.store.States(u), "the prior reset rolled back with the rest")
}

func TestPatch_OnboardingReadsTheStatesWithALock(t *testing.T) {
	// the worker writes the states under the same user lock; the decision "no evidence yet" must read them FOR UPDATE
	boom := errors.New("lock timeout")
	e := newSvc(t)
	e.store.FailNext("ListStatesForUpdate", boom)
	_, err := e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{SelfDeclaredLevel: set("N4")})
	require.ErrorIs(t, err, boom)

	// a patch that does not decide on the states does not take that read
	e = newSvc(t)
	e.store.FailNext("ListStatesForUpdate", boom)
	_, err = e.svc.PatchProfile(context.Background(), newUser(), learner.PatchInput{Nickname: set("Budi")})
	require.NoError(t, err)
}

func TestPatch_TakesTheUserLock(t *testing.T) {
	// the lock serialises PATCH/reset against the evaluation worker (spec 12.5.3)
	e := newSvc(t)
	u := newUser()
	boom := errors.New("lock timeout")
	e.store.FailNext("LockUser", boom)
	_, err := e.svc.PatchProfile(context.Background(), u, learner.PatchInput{Nickname: set("Budi")})
	require.ErrorIs(t, err, boom, "PatchProfile locks the user")

	e = newSvc(t)
	e.store.FailNext("LockUser", boom)
	require.ErrorIs(t, e.svc.ResetSkills(context.Background(), u), boom, "ResetSkills locks the user")
}
