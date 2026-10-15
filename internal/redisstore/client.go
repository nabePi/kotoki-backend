package redisstore

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// NewClient builds a Redis client (Sentinel failover when SentinelMaster is
// set, otherwise REDIS_URL) with the tracing hook installed. Consumers accept
// redis.UniversalClient, or redis.Cmdable when they only need commands.
func NewClient(cfg config.Redis, m *metrics.Redis) (redis.UniversalClient, error) {
	var c redis.UniversalClient
	if cfg.SentinelMaster != "" {
		c = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName: cfg.SentinelMaster, SentinelAddrs: cfg.SentinelAddrs, Password: cfg.Password.Reveal(),
			PoolSize: cfg.PoolSize, DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout,
			DisableIdentity: true, // CLIENT SETINFO is unknown to Redis 7.0/7.1 and logs noise
			// Without this go-redis ignores context deadlines on the socket (the 200 ms bound of the realtime state).
			ContextTimeoutEnabled: true,
		})
	} else {
		opt, perr := redis.ParseURL(cfg.URL.Reveal())
		if perr != nil {
			// The parse error quotes the raw URL (credentials included): never wrap it.
			return nil, errs.New(errs.CodeServiceUnavailable, "REDIS_URL is invalid")
		}
		if cfg.Password != "" {
			opt.Password = cfg.Password.Reveal()
		}
		if cfg.PoolSize > 0 {
			opt.PoolSize = cfg.PoolSize
		}
		if cfg.DialTimeout > 0 {
			opt.DialTimeout = cfg.DialTimeout
		}
		if cfg.ReadTimeout > 0 {
			opt.ReadTimeout = cfg.ReadTimeout
		}
		if cfg.WriteTimeout > 0 {
			opt.WriteTimeout = cfg.WriteTimeout
		}
		opt.DisableIdentity = true
		opt.ContextTimeoutEnabled = true
		c = redis.NewClient(opt)
	}
	c.AddHook(NewTraceHook(m))
	return c, nil
}

// Ping checks connectivity.
func Ping(ctx context.Context, c redis.UniversalClient) error { return c.Ping(ctx).Err() }
