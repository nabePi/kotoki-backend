package fakes

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

var _ memory.ContextCache = (*MemoryContextCache)(nil)

// MemoryContextCache is an in-memory memory.ContextCache. Contexts are kept as JSON like Redis does.
// FailNext/FailAlways with "GetMemVer", "SetContext" or "GetContext" make Redis fail.
type MemoryContextCache struct {
	FailInjector
	mu   sync.Mutex
	ver  map[uuid.UUID]int64
	ctxs map[uuid.UUID][]byte
	ttls map[uuid.UUID]time.Duration
	args map[uuid.UUID]time.Duration
	gets int
}

// NewMemoryContextCache returns an empty cache.
func NewMemoryContextCache() *MemoryContextCache {
	return &MemoryContextCache{ver: map[uuid.UUID]int64{}, ctxs: map[uuid.UUID][]byte{}, ttls: map[uuid.UUID]time.Duration{}, args: map[uuid.UUID]time.Duration{}}
}

// Bump increments the memver of userID (what MemVerBumper does).
func (c *MemoryContextCache) Bump(userID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ver[userID]++
}

// GetMemVer implements memory.ContextCache.
func (c *MemoryContextCache) GetMemVer(_ context.Context, userID uuid.UUID) (int64, error) {
	if err := c.Take("GetMemVer"); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	return c.ver[userID], nil
}

// MemVerReads is the number of GetMemVer calls that reached the store.
func (c *MemoryContextCache) MemVerReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

// SetContext implements memory.ContextCache (ttl <= 0 keeps the TTL and needs an existing entry).
func (c *MemoryContextCache) SetContext(_ context.Context, id uuid.UUID, mc memory.MemoryContext, ttl time.Duration) error {
	if err := c.Take("SetContext"); err != nil {
		return err
	}
	raw, err := json.Marshal(mc)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args[id] = ttl
	if ttl <= 0 {
		if _, ok := c.ctxs[id]; !ok {
			return nil
		}
	} else {
		c.ttls[id] = ttl
	}
	c.ctxs[id] = raw
	return nil
}

// GetContext implements memory.ContextCache.
func (c *MemoryContextCache) GetContext(_ context.Context, id uuid.UUID) (*memory.MemoryContext, error) {
	if err := c.Take("GetContext"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, ok := c.ctxs[id]
	if !ok {
		return nil, nil
	}
	var mc memory.MemoryContext
	if json.Unmarshal(raw, &mc) != nil {
		return nil, nil
	}
	return &mc, nil
}

// TTL is the TTL the context of the session was stored with.
func (c *MemoryContextCache) TTL(id uuid.UUID) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttls[id]
}

// LastTTLArg is the ttl argument of the latest SetContext call for the session.
func (c *MemoryContextCache) LastTTLArg(id uuid.UUID) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.args[id]
}
