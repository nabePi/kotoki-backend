package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

type sessionLoader struct {
	cfg content.SessionConfig
	err error
}

func (l sessionLoader) Load(context.Context, content.SessionRefs) (content.SessionConfig, error) {
	return l.cfg, l.err
}

func adaptiveRig(t *testing.T, loader sessionLoader) (adaptiveAdapter, *fakes.LearnerStore, session.AdaptiveRequest) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC))
	store := fakes.NewLearnerStore(fakes.WithLearnerClock(clk))
	svc := learner.NewService(learner.Deps{Store: store, Content: loader, Clock: clk, IDs: idgen.NewSequence(t)})
	req := session.AdaptiveRequest{UserID: uuid.New(), Config: content.ResolvedConfig{
		TopicID: uuid.New(), ScenarioVersionID: uuid.New(), PromptVersionID: uuid.New(),
		ConfigSnapshot: content.SessionSnapshot{
			Character: content.CharacterSnapshot{Voice: content.Voice{Accent: content.AccentKansai}}, Formality: content.FormalityPolite,
		},
	}}
	return adaptiveAdapter{svc: svc}, store, req
}

func TestAdapter_MarshalsAdaptiveJSON(t *testing.T) {
	loader := sessionLoader{cfg: content.SessionConfig{ScenarioVersion: content.ScenarioVersion{DifficultyLevel: 3, Formality: content.FormalityPolite}}}
	ad, _, req := adaptiveRig(t, loader)
	raw, err := ad.Adaptive(context.Background(), req)
	require.NoError(t, err)

	want, err := json.Marshal(learner.AdaptiveDifficulty{
		AlgorithmVersion: 1, ScenarioDifficulty: 3, VocabularyComplexity: 1, GrammarComplexity: 1, SpeechSpeed: 1,
		TTSSpeakingRate: 0.80, MaxAISentencesPerTurn: 2, TopicNovelty: 2, TopicAbstraction: 1, QuestionComplexity: 1,
		FollowUpDepth: 1, MaxFollowUps: 0, ExpectedResponseLength: 1, Spontaneity: 1, Scaffolding: 3,
		Formality: learner.FormalityPolite, DialectExposure: learner.DialectNone, TurnTakingComplexity: 1,
		FocusSkills: []learner.Skill{}, InSessionAdjustments: 0,
	})
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(raw), "a new user with no onboarding: every dial from the 1.00 priors")

	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	require.EqualValues(t, 1, back["algorithm_version"])
	require.Equal(t, []any{}, back["focus_skills"], "never null")
}

func TestAdapter_ErrorIsReturnedAsIs(t *testing.T) {
	boom := errors.New("connection reset")
	ad, store, req := adaptiveRig(t, sessionLoader{err: boom})
	raw, err := ad.Adaptive(context.Background(), req)
	require.ErrorIs(t, err, boom)
	require.Nil(t, raw, "no fallback snapshot")
	_, ok := store.Profile(req.UserID)
	require.False(t, ok)

	// session.Service.Create wraps it as SERVICE_UNAVAILABLE; the adapter must not hide its cause
	ad, store, req = adaptiveRig(t, sessionLoader{cfg: content.SessionConfig{ScenarioVersion: content.ScenarioVersion{DifficultyLevel: 3}}})
	store.FailNext("RecentFindings", errs.New(errs.CodeInternalError, "boom"))
	_, err = ad.Adaptive(context.Background(), req)
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err))
}
