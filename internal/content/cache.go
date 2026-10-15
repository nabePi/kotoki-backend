package content

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const byIDCapacity = 1000

// Invalidator is implemented by the cached repository: admin mutations call it so that this
// pod serves fresh content at once (other pods follow within the TTL).
type Invalidator interface{ Invalidate() }

// CachedRepository wraps a Repository with an in-process cache (spec 6.9): live reads live for
// the TTL (default 30 s), by-id reads sit in a 1,000-entry LRU without TTL.
type CachedRepository struct {
	next Repository
	ttl  time.Duration
	clk  clock.Clock

	mu    sync.Mutex
	live  map[string]liveEntry
	byID  map[string]*list.Element
	order *list.List // front = most recent
}

type liveEntry struct {
	val     any
	err     error // only ErrNotFound is cached
	expires time.Time
}

type idEntry struct {
	key string
	val any
}

var (
	_ Repository  = (*CachedRepository)(nil)
	_ Invalidator = (*CachedRepository)(nil)
)

// NewCachedRepository wraps r.
func NewCachedRepository(r Repository, ttl time.Duration, c clock.Clock) Repository {
	return &CachedRepository{next: r, ttl: ttl, clk: c, live: map[string]liveEntry{}, byID: map[string]*list.Element{}, order: list.New()}
}

// Invalidate drops every cached entry.
func (c *CachedRepository) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = map[string]liveEntry{}
	c.byID = map[string]*list.Element{}
	c.order = list.New()
}

func cachedLive[T any](c *CachedRepository, key string, load func() (T, error)) (T, error) {
	now := c.clk.Now()
	c.mu.Lock()
	if e, ok := c.live[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		if e.err != nil {
			var zero T
			return zero, e.err
		}
		return e.val.(T), nil //nolint:forcetypeassert // the key encodes the method, so the type is fixed
	}
	c.mu.Unlock()
	v, err := load()
	if err != nil && !errors.Is(err, ErrNotFound) {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	c.live[key] = liveEntry{val: v, err: err, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return v, err
}

func cachedByID[T any](c *CachedRepository, key string, load func() (T, error)) (T, error) {
	c.mu.Lock()
	if el, ok := c.byID[key]; ok {
		c.order.MoveToFront(el)
		v := el.Value.(*idEntry).val.(T) //nolint:forcetypeassert // the key encodes the method
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := load()
	if err != nil {
		var zero T
		return zero, err // not-found is not cached: the row may appear a moment later
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[key]; ok { // another goroutine loaded it meanwhile
		c.order.MoveToFront(el)
		return v, nil
	}
	c.byID[key] = c.order.PushFront(&idEntry{key: key, val: v})
	if c.order.Len() > byIDCapacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byID, oldest.Value.(*idEntry).key)
	}
	return v, nil
}

func k(method string, id uuid.UUID) string { return method + ":" + id.String() }

// GetTopic implements Repository.
func (c *CachedRepository) GetTopic(ctx context.Context, id uuid.UUID) (Topic, error) {
	return cachedLive(c, k("GetTopic", id), func() (Topic, error) { return c.next.GetTopic(ctx, id) })
}

// GetScenario implements Repository.
func (c *CachedRepository) GetScenario(ctx context.Context, id uuid.UUID) (Scenario, error) {
	return cachedLive(c, k("GetScenario", id), func() (Scenario, error) { return c.next.GetScenario(ctx, id) })
}

// GetPublishedScenarioVersion implements Repository.
func (c *CachedRepository) GetPublishedScenarioVersion(ctx context.Context, scenarioID uuid.UUID) (ScenarioVersion, error) {
	return cachedLive(c, k("GetPublishedScenarioVersion", scenarioID), func() (ScenarioVersion, error) {
		return c.next.GetPublishedScenarioVersion(ctx, scenarioID)
	})
}

// GetCharacter implements Repository.
func (c *CachedRepository) GetCharacter(ctx context.Context, id uuid.UUID) (Character, error) {
	return cachedLive(c, k("GetCharacter", id), func() (Character, error) { return c.next.GetCharacter(ctx, id) })
}

// GetPublishedPromptVersion implements Repository.
func (c *CachedRepository) GetPublishedPromptVersion(ctx context.Context, key PromptKey) (PromptVersion, error) {
	return cachedLive(c, "GetPublishedPromptVersion:"+string(key), func() (PromptVersion, error) {
		return c.next.GetPublishedPromptVersion(ctx, key)
	})
}

// ListTopics implements Repository.
func (c *CachedRepository) ListTopics(ctx context.Context) ([]Topic, error) {
	return cachedLive(c, "ListTopics", func() ([]Topic, error) { return c.next.ListTopics(ctx) })
}

// ListScenarios implements Repository.
func (c *CachedRepository) ListScenarios(ctx context.Context, topicID uuid.UUID) ([]Scenario, error) {
	return cachedLive(c, k("ListScenarios", topicID), func() ([]Scenario, error) { return c.next.ListScenarios(ctx, topicID) })
}

// ListCharacters implements Repository.
func (c *CachedRepository) ListCharacters(ctx context.Context) ([]Character, error) {
	return cachedLive(c, "ListCharacters", func() ([]Character, error) { return c.next.ListCharacters(ctx) })
}

// ListPublishedScenarioVersions implements Repository.
func (c *CachedRepository) ListPublishedScenarioVersions(ctx context.Context) ([]ScenarioVersion, error) {
	return cachedLive(c, "ListPublishedScenarioVersions", func() ([]ScenarioVersion, error) { return c.next.ListPublishedScenarioVersions(ctx) })
}

// ListActiveCharacters implements Repository.
func (c *CachedRepository) ListActiveCharacters(ctx context.Context) ([]Character, error) {
	return cachedLive(c, "ListActiveCharacters", func() ([]Character, error) { return c.next.ListActiveCharacters(ctx) })
}

// ListPublishedPromptVersions implements Repository.
func (c *CachedRepository) ListPublishedPromptVersions(ctx context.Context) ([]PromptVersion, error) {
	return cachedLive(c, "ListPublishedPromptVersions", func() ([]PromptVersion, error) { return c.next.ListPublishedPromptVersions(ctx) })
}

// GetScenarioVersion implements Repository.
func (c *CachedRepository) GetScenarioVersion(ctx context.Context, id uuid.UUID) (ScenarioVersion, error) {
	return cachedByID(c, k("GetScenarioVersion", id), func() (ScenarioVersion, error) { return c.next.GetScenarioVersion(ctx, id) })
}

// GetPromptVersion implements Repository.
func (c *CachedRepository) GetPromptVersion(ctx context.Context, id uuid.UUID) (PromptVersion, error) {
	return cachedByID(c, k("GetPromptVersion", id), func() (PromptVersion, error) { return c.next.GetPromptVersion(ctx, id) })
}

// GetTopicByID implements Repository.
func (c *CachedRepository) GetTopicByID(ctx context.Context, id uuid.UUID) (Topic, error) {
	return cachedByID(c, k("GetTopicByID", id), func() (Topic, error) { return c.next.GetTopicByID(ctx, id) })
}
