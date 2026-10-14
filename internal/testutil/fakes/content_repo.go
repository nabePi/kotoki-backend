package fakes

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
)

// ContentRepo is an in-memory content.Repository. Calls counts every method call so cache tests
// can count hits to the "database".
type ContentRepo struct {
	FailInjector
	mu               sync.Mutex
	topics           map[uuid.UUID]content.Topic
	scenarios        map[uuid.UUID]content.Scenario
	scenarioVersions map[uuid.UUID]content.ScenarioVersion
	characters       map[uuid.UUID]content.Character
	promptVersions   map[uuid.UUID]content.PromptVersion
	calls            map[string]int
}

var _ content.Repository = (*ContentRepo)(nil)

// NewContentRepo returns an empty repository.
func NewContentRepo() *ContentRepo {
	return &ContentRepo{
		topics: map[uuid.UUID]content.Topic{}, scenarios: map[uuid.UUID]content.Scenario{}, scenarioVersions: map[uuid.UUID]content.ScenarioVersion{},
		characters: map[uuid.UUID]content.Character{}, promptVersions: map[uuid.UUID]content.PromptVersion{}, calls: map[string]int{},
	}
}

// PutTopic upserts a topic (the other Put methods do the same for their entity).
func (r *ContentRepo) PutTopic(t content.Topic) { r.locked(func() { r.topics[t.ID] = t }) }

// PutScenario upserts a scenario.
func (r *ContentRepo) PutScenario(s content.Scenario) { r.locked(func() { r.scenarios[s.ID] = s }) }

// PutScenarioVersion upserts a scenario version.
func (r *ContentRepo) PutScenarioVersion(v content.ScenarioVersion) {
	r.locked(func() { r.scenarioVersions[v.ID] = v })
}

// PutCharacter upserts a character.
func (r *ContentRepo) PutCharacter(c content.Character) { r.locked(func() { r.characters[c.ID] = c }) }

// PutPromptVersion upserts a prompt version.
func (r *ContentRepo) PutPromptVersion(v content.PromptVersion) {
	r.locked(func() { r.promptVersions[v.ID] = v })
}

// Calls returns how many times method was called.
func (r *ContentRepo) Calls(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[method]
}

func (r *ContentRepo) locked(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *ContentRepo) enter(method string) error {
	r.mu.Lock()
	r.calls[method]++
	r.mu.Unlock()
	return r.Take(method)
}

// GetTopic implements content.Repository.
func (r *ContentRepo) GetTopic(_ context.Context, id uuid.UUID) (content.Topic, error) {
	if err := r.enter("GetTopic"); err != nil {
		return content.Topic{}, err
	}
	return get(r, r.topics, id)
}

// GetTopicByID implements content.Repository.
func (r *ContentRepo) GetTopicByID(_ context.Context, id uuid.UUID) (content.Topic, error) {
	if err := r.enter("GetTopicByID"); err != nil {
		return content.Topic{}, err
	}
	return get(r, r.topics, id)
}

// GetScenario implements content.Repository.
func (r *ContentRepo) GetScenario(_ context.Context, id uuid.UUID) (content.Scenario, error) {
	if err := r.enter("GetScenario"); err != nil {
		return content.Scenario{}, err
	}
	return get(r, r.scenarios, id)
}

// GetScenarioVersion implements content.Repository.
func (r *ContentRepo) GetScenarioVersion(_ context.Context, id uuid.UUID) (content.ScenarioVersion, error) {
	if err := r.enter("GetScenarioVersion"); err != nil {
		return content.ScenarioVersion{}, err
	}
	return get(r, r.scenarioVersions, id)
}

// GetCharacter implements content.Repository.
func (r *ContentRepo) GetCharacter(_ context.Context, id uuid.UUID) (content.Character, error) {
	if err := r.enter("GetCharacter"); err != nil {
		return content.Character{}, err
	}
	return get(r, r.characters, id)
}

// GetPromptVersion implements content.Repository.
func (r *ContentRepo) GetPromptVersion(_ context.Context, id uuid.UUID) (content.PromptVersion, error) {
	if err := r.enter("GetPromptVersion"); err != nil {
		return content.PromptVersion{}, err
	}
	return get(r, r.promptVersions, id)
}

func get[T any](r *ContentRepo, m map[uuid.UUID]T, id uuid.UUID) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := m[id]; ok {
		return v, nil
	}
	var zero T
	return zero, content.ErrNotFound
}

// GetPublishedScenarioVersion implements content.Repository.
func (r *ContentRepo) GetPublishedScenarioVersion(_ context.Context, scenarioID uuid.UUID) (content.ScenarioVersion, error) {
	if err := r.enter("GetPublishedScenarioVersion"); err != nil {
		return content.ScenarioVersion{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.scenarioVersions {
		if v.ScenarioID == scenarioID && v.Status == content.VersionPublished {
			return v, nil
		}
	}
	return content.ScenarioVersion{}, content.ErrNotFound
}

// GetPublishedPromptVersion implements content.Repository.
func (r *ContentRepo) GetPublishedPromptVersion(_ context.Context, key content.PromptKey) (content.PromptVersion, error) {
	if err := r.enter("GetPublishedPromptVersion"); err != nil {
		return content.PromptVersion{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.promptVersions {
		if v.Key == key && v.Status == content.VersionPublished {
			return v, nil
		}
	}
	return content.PromptVersion{}, content.ErrNotFound
}

func sortedActive[T any](r *ContentRepo, m map[uuid.UUID]T, active func(T) bool, less func(a, b T) int) []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []T
	for _, v := range m {
		if active(v) {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, less)
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

// ListTopics implements content.Repository.
func (r *ContentRepo) ListTopics(context.Context) ([]content.Topic, error) {
	if err := r.enter("ListTopics"); err != nil {
		return nil, err
	}
	return sortedActive(r, r.topics, func(t content.Topic) bool { return t.Status == content.StatusActive }, func(a, b content.Topic) int {
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		return compare(a.Code, b.Code)
	}), nil
}

// ListScenarios implements content.Repository.
func (r *ContentRepo) ListScenarios(_ context.Context, topicID uuid.UUID) ([]content.Scenario, error) {
	if err := r.enter("ListScenarios"); err != nil {
		return nil, err
	}
	return sortedActive(r, r.scenarios, func(s content.Scenario) bool { return s.TopicID == topicID && s.Status == content.StatusActive }, func(a, b content.Scenario) int {
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		return compare(a.Code, b.Code)
	}), nil
}

func (r *ContentRepo) activeCharacters() []content.Character {
	return sortedActive(r, r.characters, func(c content.Character) bool { return c.Status == content.StatusActive }, func(a, b content.Character) int {
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		return compare(a.Code, b.Code)
	})
}

// ListCharacters implements content.Repository.
func (r *ContentRepo) ListCharacters(context.Context) ([]content.Character, error) {
	if err := r.enter("ListCharacters"); err != nil {
		return nil, err
	}
	return r.activeCharacters(), nil
}

// ListActiveCharacters implements content.Repository.
func (r *ContentRepo) ListActiveCharacters(context.Context) ([]content.Character, error) {
	if err := r.enter("ListActiveCharacters"); err != nil {
		return nil, err
	}
	return r.activeCharacters(), nil
}

// ListPublishedScenarioVersions implements content.Repository.
func (r *ContentRepo) ListPublishedScenarioVersions(context.Context) ([]content.ScenarioVersion, error) {
	if err := r.enter("ListPublishedScenarioVersions"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []content.ScenarioVersion
	for _, v := range r.scenarioVersions {
		if v.Status == content.VersionPublished {
			out = append(out, v)
		}
	}
	return out, nil
}

// ListPublishedPromptVersions implements content.Repository.
func (r *ContentRepo) ListPublishedPromptVersions(context.Context) ([]content.PromptVersion, error) {
	if err := r.enter("ListPublishedPromptVersions"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []content.PromptVersion
	for _, v := range r.promptVersions {
		if v.Status == content.VersionPublished {
			out = append(out, v)
		}
	}
	return out, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
