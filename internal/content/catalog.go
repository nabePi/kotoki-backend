package content

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// CatalogService serves the public catalog (spec 6.12): only entities a session could actually
// be created from. It reads the cached repository.
type CatalogService struct {
	repo Repository
	cfg  ResolverConfig
}

// NewCatalogService builds the catalog; cfg is the resolver's, so "usable" means the same thing
// in both places.
func NewCatalogService(r Repository, cfg ResolverConfig) *CatalogService {
	if cfg.TTSEnabled == nil {
		cfg.TTSEnabled = func(string) bool { return true }
	}
	return &CatalogService{repo: r, cfg: cfg}
}

func notFound(what string) error { return errs.New(errs.CodeNotFound, what+" not found") }

// scenarioView returns the view of an ACTIVE scenario with a PUBLISHED version; ok is false when
// the scenario is not usable. Its topic is checked by the caller.
func (c *CatalogService) scenarioView(ctx context.Context, s Scenario) (ScenarioView, bool, error) {
	if s.Status != StatusActive {
		return ScenarioView{}, false, nil
	}
	sv, err := c.repo.GetPublishedScenarioVersion(ctx, s.ID)
	if errors.Is(err, ErrNotFound) {
		return ScenarioView{}, false, nil
	}
	if err != nil {
		return ScenarioView{}, false, internal(err, "read scenario version failed")
	}
	band, jlpt := DifficultyBand(sv.DifficultyLevel)
	return ScenarioView{
		ID: s.ID, TopicID: s.TopicID, Code: s.Code, Name: s.Name, NameJA: s.NameJA, Description: s.Description,
		VersionID: sv.ID, Version: sv.Version, DifficultyLevel: sv.DifficultyLevel, DifficultyBand: band, JLPTLabel: jlpt,
		RecommendedLevelMin: sv.RecommendedLevelMin, RecommendedLevelMax: sv.RecommendedLevelMax,
		Situation: sv.Situation, AIRole: sv.AIRole, UserRole: sv.UserRole, Goal: sv.Goal,
		Tasks: orEmpty(sv.Tasks), VocabHints: orEmptyHints(sv.VocabHints), GrammarTargets: orEmpty(sv.GrammarTargets),
		Formality: sv.Formality, VisualPrompt: sv.VisualPrompt,
	}, true, nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyHints(h []VocabHint) []VocabHint {
	if h == nil {
		return []VocabHint{}
	}
	return h
}

func (c *CatalogService) topicScenarios(ctx context.Context, t Topic) ([]ScenarioView, error) {
	if t.Status != StatusActive {
		return nil, nil
	}
	list, err := c.repo.ListScenarios(ctx, t.ID)
	if err != nil {
		return nil, internal(err, "list scenarios failed")
	}
	out := make([]ScenarioView, 0, len(list))
	for _, s := range list {
		v, ok, err := c.scenarioView(ctx, s)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// Topics lists ACTIVE topics that have at least one usable scenario.
func (c *CatalogService) Topics(ctx context.Context) ([]TopicView, error) {
	topics, err := c.repo.ListTopics(ctx)
	if err != nil {
		return nil, internal(err, "list topics failed")
	}
	out := make([]TopicView, 0, len(topics))
	for _, t := range topics {
		scs, err := c.topicScenarios(ctx, t)
		if err != nil {
			return nil, err
		}
		if len(scs) == 0 {
			continue
		}
		out = append(out, TopicView{ID: t.ID, Code: t.Code, Name: t.Name, NameJA: t.NameJA, Description: t.Description, IconKey: t.IconKey, ScenarioCount: len(scs)})
	}
	return out, nil
}

// Scenarios lists usable scenarios ordered by topic, scenario sort order and code; topicID narrows
// it to one topic (an unusable topic gives an empty list).
func (c *CatalogService) Scenarios(ctx context.Context, topicID *uuid.UUID) ([]ScenarioView, error) {
	topics, err := c.repo.ListTopics(ctx)
	if err != nil {
		return nil, internal(err, "list topics failed")
	}
	out := []ScenarioView{}
	for _, t := range topics {
		if topicID != nil && t.ID != *topicID {
			continue
		}
		scs, err := c.topicScenarios(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, scs...)
	}
	return out, nil
}

// Scenario returns one usable scenario; anything else is NOT_FOUND.
func (c *CatalogService) Scenario(ctx context.Context, id uuid.UUID) (ScenarioView, error) {
	s, err := c.repo.GetScenario(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return ScenarioView{}, notFound("scenario")
	}
	if err != nil {
		return ScenarioView{}, internal(err, "read scenario failed")
	}
	t, err := c.repo.GetTopic(ctx, s.TopicID)
	if errors.Is(err, ErrNotFound) || (err == nil && t.Status != StatusActive) {
		return ScenarioView{}, notFound("scenario")
	}
	if err != nil {
		return ScenarioView{}, internal(err, "read topic failed")
	}
	v, ok, err := c.scenarioView(ctx, s)
	if err != nil {
		return ScenarioView{}, err
	}
	if !ok {
		return ScenarioView{}, notFound("scenario")
	}
	return v, nil
}

// characterUsable follows steps 2 and 3 of the resolver (spec 6.9): ACTIVE, the primary TTS vendor
// enabled and, in real mode, a configured voice.
func (c *CatalogService) characterUsable(ch Character) bool {
	if ch.Status != StatusActive || !c.cfg.TTSEnabled(NormalizeProviderName(ch.ProviderName)) {
		return false
	}
	if c.cfg.TTSMode == "fake" {
		return true
	}
	return strings.TrimSpace(ch.ModelName) != "" && strings.TrimSpace(ch.VoiceID) != "" && !strings.HasPrefix(ch.VoiceID, placeholderPrefix)
}

func characterView(ch Character) CharacterView {
	return CharacterView{ID: ch.ID, Code: ch.Code, Name: ch.Name, AvatarKey: ch.AvatarKey, Gender: ch.Voice.Gender, Accent: ch.Voice.Accent}
}

// Characters lists usable characters.
func (c *CatalogService) Characters(ctx context.Context) ([]CharacterView, error) {
	list, err := c.repo.ListCharacters(ctx)
	if err != nil {
		return nil, internal(err, "list characters failed")
	}
	out := make([]CharacterView, 0, len(list))
	for _, ch := range list {
		if c.characterUsable(ch) {
			out = append(out, characterView(ch))
		}
	}
	return out, nil
}

// Character returns one usable character; anything else is NOT_FOUND.
func (c *CatalogService) Character(ctx context.Context, id uuid.UUID) (CharacterView, error) {
	ch, err := c.repo.GetCharacter(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return CharacterView{}, notFound("character")
	}
	if err != nil {
		return CharacterView{}, internal(err, "read character failed")
	}
	if !c.characterUsable(ch) {
		return CharacterView{}, notFound("character")
	}
	return characterView(ch), nil
}
