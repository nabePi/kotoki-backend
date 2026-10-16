package learner

import (
	"context"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// AdaptiveRequest asks for the adaptive difficulty of a new session of UserID: Config is what the
// content resolver chose for it (spec 12.8).
type AdaptiveRequest struct {
	UserID uuid.UUID
	Config content.ResolvedConfig
}

// ComputeAdaptiveDifficulty reads the learner's state with a handful of small queries and applies
// the pure ComputeAdaptiveDifficulty of spec 12.8.3. The queries run outside the reservation
// transaction of session creation. A failure is returned as is: there is no default snapshot.
func (s *Service) ComputeAdaptiveDifficulty(ctx context.Context, req AdaptiveRequest) (AdaptiveDifficulty, error) {
	if s.content == nil {
		return AdaptiveDifficulty{}, errs.New(errs.CodeInternalError, "learner: no session config loader")
	}
	snapshot, err := req.Config.ConfigSnapshot.MarshalJSONB()
	if err != nil {
		return AdaptiveDifficulty{}, errs.Wrap(err, errs.CodeInternalError, "encode config snapshot failed")
	}
	cfg, err := s.content.Load(ctx, content.SessionRefs{
		TopicID: req.Config.TopicID, ScenarioVersionID: req.Config.ScenarioVersionID,
		PromptVersionID: req.Config.PromptVersionID, ConfigSnapshot: snapshot,
	})
	if err != nil {
		return AdaptiveDifficulty{}, err
	}

	var (
		profile Profile
		states  []StateRow
	)
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		profile, states = Profile{}, nil
		if err := s.EnsureProfile(ctx, tx, req.UserID); err != nil {
			return err
		}
		var err error
		if profile, err = tx.Profiles().Get(ctx, req.UserID); err != nil {
			return err
		}
		states, err = tx.SkillStates().List(ctx, req.UserID, false)
		return err
	})
	if err != nil {
		return AdaptiveDifficulty{}, err
	}

	now := s.clock.Now()
	rd := s.store.Reader()
	findings, evaluations, err := rd.RecentFindings(ctx, req.UserID)
	if err != nil {
		return AdaptiveDifficulty{}, err
	}
	topicSessions, err := rd.TopicSessions90d(ctx, req.UserID, req.Config.TopicID, now)
	if err != nil {
		return AdaptiveDifficulty{}, err
	}
	dialectSessions, err := rd.DialectSessions90d(ctx, req.UserID, now)
	if err != nil {
		return AdaptiveDifficulty{}, err
	}

	fi := focusInput(states, findings, evaluations, now)
	for _, name := range cfg.ScenarioVersion.EvaluationFocus {
		if k, ok := ParseSkill(name); ok {
			fi.ScenarioFocus = append(fi.ScenarioFocus, k)
		}
	}
	trends := make(map[Skill]string, len(fi.Trends))
	for k, t := range fi.Trends {
		trends[k] = string(t)
	}
	return ComputeAdaptiveDifficulty(AdaptiveInput{
		Levels: fi.Levels, Confidences: fi.Confidences, Trends: trends,
		FindingStats: findings, EvaluationsConsidered: evaluations,
		ScenarioDifficulty: cfg.ScenarioVersion.DifficultyLevel,
		ScenarioFormality:  Formality(req.Config.ConfigSnapshot.Formality), // scenario_versions.formality (D-014)
		ScenarioFocus:      fi.ScenarioFocus,
		VoiceAccent:        string(req.Config.ConfigSnapshot.Character.Voice.Accent),
		TopicSessions90d:   topicSessions,
		DialectSessions90d: dialectSessions,
		PrefSpeechSpeed:    profile.PreferredSpeechSpeed,
		PrefFormality:      profile.PreferredFormality,
		DialectPreference:  profile.DialectPreference,
		Focus:              FocusSkills(fi),
	}), nil
}

// PromptContext builds the learner block of the conversation prompt: the nickname and the learning
// goal of the profile (never users.full_name) and the rendered adaptation of ad.
func (s *Service) PromptContext(ctx context.Context, userID uuid.UUID, ad AdaptiveDifficulty) (prompt.LearnerContext, error) {
	var p Profile
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		p = Profile{}
		if err := s.EnsureProfile(ctx, tx, userID); err != nil {
			return err
		}
		var err error
		p, err = tx.Profiles().Get(ctx, userID)
		return err
	})
	if err != nil {
		return prompt.LearnerContext{}, err
	}
	return NewPromptContext(ProfileSnapshot{Nickname: p.Nickname, LearningGoal: p.LearningGoal}, ad), nil
}

// Rerender re-derives the adaptation text of prev after an in-session adjustment without touching
// storage. With PromptContext it makes *Service a conversation.LearnerContextProvider.
func (s *Service) Rerender(prev prompt.LearnerContext, ad AdaptiveDifficulty) prompt.LearnerContext {
	return Rerender(prev, ad)
}
