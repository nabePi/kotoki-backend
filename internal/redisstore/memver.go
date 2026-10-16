package redisstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// memVerTTL bounds every memver key so that a user who never returns does not leave a key behind.
const memVerTTL = 30 * 24 * time.Hour

func memVerKey(userID uuid.UUID) string { return "kotoki:user:" + userID.String() + ":memver" }

// MemVerBumper increments the per-user memory version that invalidates cached memory retrieval
// (spec 9.9.9, 11.8.3). It satisfies jobs.MemVerBumper.
type MemVerBumper struct{ rdb redis.UniversalClient }

// NewMemVerBumper returns a MemVerBumper.
func NewMemVerBumper(rdb redis.UniversalClient) *MemVerBumper { return &MemVerBumper{rdb: rdb} }

// Bump runs INCR then EXPIRE (30 days) on kotoki:user:{id}:memver for every user, in one pipeline.
func (b *MemVerBumper) Bump(ctx context.Context, userIDs []uuid.UUID) error {
	if len(userIDs) == 0 {
		return nil
	}
	_, err := b.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, id := range userIDs {
			k := memVerKey(id)
			p.Incr(ctx, k)
			p.Expire(ctx, k, memVerTTL)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("bump memver for %d users: %w", len(userIDs), err)
	}
	return nil
}
