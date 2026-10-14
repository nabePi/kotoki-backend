package redisstore

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

// FakeFaults is the Redis-backed fake.FaultSource (spec 8.9.6): a goroutine GETs the fault
// document every interval and swaps an atomic snapshot, so provider calls never touch
// Redis. A missing key means no runtime faults; a Redis outage keeps the last snapshot for
// at most 10 s, then clears it.
type FakeFaults struct{ *fake.Poller }

var _ fake.FaultSource = (*FakeFaults)(nil)

// NewFakeFaults builds the poller over rdb for key. Call Run (usually in a goroutine) to
// start polling.
func NewFakeFaults(rdb redis.Cmdable, key string, interval time.Duration, clk clock.Clock, log *slog.Logger) *FakeFaults {
	fetch := func(ctx context.Context) ([]byte, bool, error) {
		b, err := rdb.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return b, true, nil
	}
	return &FakeFaults{Poller: fake.NewPoller(fetch, interval, clk, log)}
}
