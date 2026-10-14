package redisstore

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// bucketLua is the atomic token bucket of spec 13.4.3. Time comes from the
// Redis server so pod clock skew cannot matter.
const bucketLua = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local cap, rate, cost = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local b = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(b[1]) or cap
local ts = tonumber(b[2]) or now
tokens = math.min(cap, tokens + math.max(0, now - ts) * rate)
local allowed, retry_ms = 0, 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry_ms = math.ceil((cost - tokens) / rate)
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(cap / rate) + 1000)
return {allowed, math.floor(tokens), retry_ms}
`

const warnEvery = 10 * time.Second

// RateLimiter is the Redis token-bucket limiter. When Redis fails it falls back
// to a local limiter (fail-open), never returning the Redis error.
type RateLimiter struct {
	rdb      redis.UniversalClient
	script   *redis.Script
	fallback ratelimit.Limiter
	clk      clock.Clock
	log      *slog.Logger
	m        *metrics.RateLimit

	mu       sync.Mutex
	lastWarn time.Time
}

var _ ratelimit.Limiter = (*RateLimiter)(nil)

// NewRateLimiter returns a RateLimiter; m may be nil to skip metrics.
func NewRateLimiter(rdb redis.UniversalClient, fallback ratelimit.Limiter, clk clock.Clock, log *slog.Logger, m *metrics.RateLimit) *RateLimiter {
	return &RateLimiter{rdb: rdb, script: redis.NewScript(bucketLua), fallback: fallback, clk: clk, log: log, m: m}
}

// Allow takes one token for (policy, subject).
func (l *RateLimiter) Allow(ctx context.Context, p ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	d, err := l.viaRedis(ctx, p, subject)
	if err != nil {
		l.backendError(ctx, err)
		d, err = l.fallback.Allow(ctx, p, subject)
		if err != nil {
			return ratelimit.Decision{Allowed: true}, nil // both limiters failed: never block users
		}
	}
	if !d.Allowed && l.m != nil {
		l.m.Rejected(p.Name)
	}
	return d, nil
}

func (l *RateLimiter) viaRedis(ctx context.Context, p ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	key := fmt.Sprintf("kotoki:ratelimit:%s:%s", p.Name, subject)
	res, err := l.script.Run(ctx, l.rdb, []string{key}, p.Capacity, strconv.FormatFloat(p.RatePerMs(), 'g', -1, 64), 1).Slice()
	if err != nil {
		return ratelimit.Decision{}, err
	}
	if len(res) != 3 {
		return ratelimit.Decision{}, fmt.Errorf("ratelimit: unexpected script reply %v", res)
	}
	allowed, _ := res[0].(int64)
	remaining, _ := res[1].(int64)
	retryMs, _ := res[2].(int64)
	return ratelimit.Decision{Allowed: allowed == 1, Remaining: int(remaining), RetryAfter: time.Duration(retryMs) * time.Millisecond}, nil
}

func (l *RateLimiter) backendError(ctx context.Context, err error) {
	if l.m != nil {
		l.m.BackendError()
	}
	l.mu.Lock()
	now := l.clk.Now()
	warn := l.lastWarn.IsZero() || now.Sub(l.lastWarn) >= warnEvery
	if warn {
		l.lastWarn = now
	}
	l.mu.Unlock()
	if warn {
		l.log.WarnContext(ctx, "rate limiter backend unavailable, using local limiter", "error", err.Error())
	}
}
