package driver

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// KV is the part of Redis the driver needs.
type KV interface {
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Del(ctx context.Context, key string) error
	// DelPattern deletes every key matching the glob pattern and returns how many it deleted.
	DelPattern(ctx context.Context, pattern string) (int, error)
}

// GoRedis adapts a go-redis client to KV.
type GoRedis struct{ Client redis.Cmdable }

// Set implements KV.
func (g GoRedis) Set(ctx context.Context, key string, v []byte, ttl time.Duration) error {
	return g.Client.Set(ctx, key, v, ttl).Err()
}

// Del implements KV.
func (g GoRedis) Del(ctx context.Context, key string) error { return g.Client.Del(ctx, key).Err() }

// DelPattern implements KV with SCAN and DEL (never KEYS: the Redis may be a shared one).
func (g GoRedis) DelPattern(ctx context.Context, pattern string) (int, error) {
	var (
		cursor uint64
		n      int
	)
	for {
		keys, next, err := g.Client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return n, err
		}
		if len(keys) > 0 {
			d, err := g.Client.Del(ctx, keys...).Result()
			if err != nil {
				return n, err
			}
			n += int(d)
		}
		if cursor = next; cursor == 0 {
			return n, nil
		}
	}
}

// rateLimitPatterns match the budgets of starting a session, for every user and every address
// (internal/redisstore/ratelimit.go: kotoki:ratelimit:<policy>:<subject>): creating the session, asking
// for the ticket, the WebSocket handshake of the address and the attach of the user. Nothing else of the
// limiter is touched (login, register, the default API limits).
var rateLimitPatterns = []string{
	"kotoki:ratelimit:session_create_user:*",
	"kotoki:ratelimit:ticket_user:*",
	"kotoki:ratelimit:ws_connect_ip:*",
	"kotoki:ratelimit:ws_attach_user:*",
}

// Redis performs KindFault actions: a fault set is written as the fault document, an empty one
// removes it (the revert). The document carries `until` = now + Until, and the key the same TTL, so
// a run that dies before its revert leaves nothing behind that lasts (spec 8.9.6).
type Redis struct {
	KV    KV
	Key   string        // default faultdoc.RedisKey
	Until time.Duration // default 30 minutes
	Now   func() time.Time
}

// Do implements scenario.Driver.
func (r Redis) Do(ctx context.Context, a scenario.Action) error {
	if a.Kind == scenario.KindReset {
		return r.reset(ctx, a)
	}
	if a.Kind != scenario.KindFault {
		return fmt.Errorf("driver: redis handles fault and reset actions, not %q", a.Kind)
	}
	key := r.Key
	if key == "" {
		key = faultdoc.RedisKey
	}
	if len(a.Fault) == 0 {
		if err := r.KV.Del(ctx, key); err != nil {
			return fmt.Errorf("driver: clear faults: %w", err)
		}
		return nil
	}
	ttl := r.Until
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	doc, err := faultdoc.Build(now().Add(ttl), a.Fault)
	if err != nil {
		return err
	}
	if err := r.KV.Set(ctx, key, doc, ttl); err != nil {
		return fmt.Errorf("driver: set faults: %w", err)
	}
	return nil
}

func (r Redis) reset(ctx context.Context, a scenario.Action) error {
	if len(a.Argv) != 1 || a.Argv[0] != scenario.ResetRateLimits {
		return fmt.Errorf("driver: unknown reset %q (only %q)", a.Argv, scenario.ResetRateLimits)
	}
	for _, p := range rateLimitPatterns {
		if _, err := r.KV.DelPattern(ctx, p); err != nil {
			return fmt.Errorf("driver: reset rate limits: %w", err)
		}
	}
	return nil
}
