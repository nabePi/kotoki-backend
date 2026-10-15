//go:build integration || e2e

package testutil

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

const redisImage = "redis:7.4"

var rd struct {
	once sync.Once
	url  string
	err  error
}

// NewRedis returns a client on a flushed Redis. TEST_REDIS_URL overrides the container.
func NewRedis(t testing.TB) *redis.Client {
	t.Helper()
	rd.once.Do(func() {
		if v := os.Getenv("TEST_REDIS_URL"); v != "" {
			rd.url = v
			return
		}
		var c *tcredis.RedisContainer
		c, rd.err = tcredis.Run(context.Background(), redisImage)
		if rd.err == nil {
			rd.url, rd.err = c.ConnectionString(context.Background())
		}
	})
	if rd.err != nil {
		t.Fatalf("testutil: redis: %v", rd.err)
	}
	opt, err := redis.ParseURL(rd.url)
	if err != nil {
		t.Fatalf("testutil: redis url: %v", err)
	}
	cl := redis.NewClient(opt)
	if err := cl.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("testutil: flushall: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}
