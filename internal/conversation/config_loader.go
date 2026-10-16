package conversation

import (
	"container/list"
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// defaultConfigCacheSize is used when NewConfigLoader gets size <= 0.
const defaultConfigCacheSize = 128

// lru is a small mutex-guarded LRU map; versions are immutable, so entries never expire.
type lru[K comparable, V any] struct {
	mu   sync.Mutex
	size int
	ll   *list.List
	m    map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	k K
	v V
}

func newLRU[K comparable, V any](size int) *lru[K, V] {
	return &lru[K, V]{size: size, ll: list.New(), m: map[K]*list.Element{}}
}

func (c *lru[K, V]) get(k K) (v V, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return v, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(lruEntry[K, V]).v, true
}

func (c *lru[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[k]; ok {
		e.Value = lruEntry[K, V]{k, v}
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(lruEntry[K, V]{k, v})
	for c.ll.Len() > c.size {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(lruEntry[K, V]).k)
	}
}

// versionKey identifies the immutable content a session runs on.
type versionKey struct{ topic, scenarioVersion, promptVersion uuid.UUID }

// versionSet is the cached, session-independent part of a content.SessionConfig.
type versionSet struct {
	topic    content.Topic
	scenario content.ScenarioVersion
	prompt   content.PromptVersion
}

type configLoader struct {
	l       content.SessionConfigLoader
	prompts interface {
		GetPromptVersion(ctx context.Context, id uuid.UUID) (content.PromptVersion, error)
	}
	versions *lru[versionKey, versionSet]
	openings *lru[uuid.UUID, content.PromptVersion]
}

// NewConfigLoader returns a ConfigLoader that caches scenario/prompt versions per version ID in an
// LRU of the given size (<= 0: 128). Versions are loaded by ID only, never by PUBLISHED status
// (S-2), and the character snapshot is decoded per session, never cached.
func NewConfigLoader(l content.SessionConfigLoader, prompts interface {
	GetPromptVersion(ctx context.Context, id uuid.UUID) (content.PromptVersion, error)
}, size int) ConfigLoader {
	if size <= 0 {
		size = defaultConfigCacheSize
	}
	return &configLoader{l: l, prompts: prompts, versions: newLRU[versionKey, versionSet](size), openings: newLRU[uuid.UUID, content.PromptVersion](size)}
}

// LoadSnapshot implements ConfigLoader.
func (c *configLoader) LoadSnapshot(ctx context.Context, rec session.RuntimeRecord) (ConfigSnapshot, error) {
	snap, err := content.ParseSnapshot(rec.ConfigSnapshot)
	if err != nil {
		return ConfigSnapshot{}, errs.Wrap(err, errs.CodeContentMisconfigured, "session config snapshot is invalid")
	}
	key := versionKey{rec.TopicID, rec.ScenarioVersionID, rec.PromptVersionID}
	vs, ok := c.versions.get(key)
	if !ok {
		cfg, err := c.l.Load(ctx, content.SessionRefs{
			TopicID: rec.TopicID, ScenarioVersionID: rec.ScenarioVersionID, PromptVersionID: rec.PromptVersionID,
			ConfigSnapshot: rec.ConfigSnapshot,
		})
		if err != nil {
			return ConfigSnapshot{}, err
		}
		vs = versionSet{topic: cfg.Topic, scenario: cfg.ScenarioVersion, prompt: cfg.PromptVersion}
		c.versions.put(key, vs)
	}
	out := ConfigSnapshot{SessionConfig: content.SessionConfig{Topic: vs.topic, ScenarioVersion: vs.scenario, Snapshot: snap, PromptVersion: vs.prompt}}
	if rec.OpeningPromptVersionID == nil {
		return out, nil
	}
	id := *rec.OpeningPromptVersionID
	op, ok := c.openings.get(id)
	if !ok {
		if op, err = c.prompts.GetPromptVersion(ctx, id); err != nil {
			return ConfigSnapshot{}, errs.Wrap(err, errs.CodeInternalError, "load opening prompt version failed")
		}
		c.openings.put(id, op)
	}
	out.Opening = &op
	return out, nil
}
