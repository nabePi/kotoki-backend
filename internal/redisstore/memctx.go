package redisstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

func memCtxKey(sessionID uuid.UUID) string { return "kotoki:session:" + sessionID.String() + ":memctx" }

// MemCtxStore keeps the memory context of a session (JSON memory.MemoryContext) and reads the
// per-user memory version (spec 11.7). It satisfies memory.ContextCache; MemVerBumper writes the
// version.
type MemCtxStore struct{ rdb redis.UniversalClient }

var _ memory.ContextCache = (*MemCtxStore)(nil)

// NewMemCtxStore returns a MemCtxStore.
func NewMemCtxStore(rdb redis.UniversalClient) *MemCtxStore { return &MemCtxStore{rdb: rdb} }

// GetMemVer returns kotoki:user:{id}:memver, 0 when the key does not exist.
func (s *MemCtxStore) GetMemVer(ctx context.Context, userID uuid.UUID) (int64, error) {
	v, err := s.rdb.Get(ctx, memVerKey(userID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get memver: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memver is not an integer: %w", err)
	}
	return n, nil
}

// SetContext stores mc with ttl. A ttl <= 0 keeps the TTL of the key and only writes a key that
// exists (SET XX KEEPTTL), so a refresh never resurrects an expired context.
func (s *MemCtxStore) SetContext(ctx context.Context, sessionID uuid.UUID, mc memory.MemoryContext, ttl time.Duration) error {
	raw, err := json.Marshal(mc)
	if err != nil {
		return fmt.Errorf("encode memctx: %w", err)
	}
	args := redis.SetArgs{TTL: ttl}
	if ttl <= 0 {
		args = redis.SetArgs{Mode: "XX", KeepTTL: true}
	}
	if err := s.rdb.SetArgs(ctx, memCtxKey(sessionID), raw, args).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("set memctx: %w", err)
	}
	return nil
}

// GetContext returns the stored context; nil, nil when it does not exist or cannot be decoded.
func (s *MemCtxStore) GetContext(ctx context.Context, sessionID uuid.UUID) (*memory.MemoryContext, error) {
	raw, err := s.rdb.Get(ctx, memCtxKey(sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get memctx: %w", err)
	}
	var mc memory.MemoryContext
	if err := json.Unmarshal(raw, &mc); err != nil {
		return nil, nil //nolint:nilerr // an unreadable entry counts as missing: the caller rebuilds it
	}
	return &mc, nil
}
