package provider

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// semaphoreSlot bounds concurrent calls per (vendor, capability) per process (spec 8.5.6).
type semaphoreSlot struct {
	sem   *semaphore.Weighted
	limit int
	wait  time.Duration
	inUse atomic.Int64
}

func newSemaphoreSlot(limit int, wait time.Duration) *semaphoreSlot {
	return &semaphoreSlot{sem: semaphore.NewWeighted(int64(limit)), limit: limit, wait: wait}
}

// Limit is the configured concurrency limit.
func (s *semaphoreSlot) Limit() int { return s.limit }

// InUse is the number of slots currently held.
func (s *semaphoreSlot) InUse() int { return int(s.inUse.Load()) }

func localConcurrency(p Name, c Capability) *Error {
	return &Error{Provider: p, Capability: c, Kind: KindRateLimited, VendorCode: vendorCodeLocal, Retryable: true,
		Message: "local concurrency limit reached"}
}

// Acquire takes one slot. With wait == 0 it only tries; otherwise it waits up to wait.
// Full → RATE_LIMITED{local_concurrency}; ctx ended first → CANCELLED/TIMEOUT. The
// returned release is idempotent.
func (s *semaphoreSlot) Acquire(ctx context.Context, p Name, c Capability) (release func(), err *Error) {
	if s.wait <= 0 {
		if !s.sem.TryAcquire(1) {
			return nil, localConcurrency(p, c)
		}
	} else {
		wctx, cancel := context.WithTimeout(ctx, s.wait)
		aerr := s.sem.Acquire(wctx, 1)
		cancel()
		if aerr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, &Error{Provider: p, Capability: c, Kind: KindOf(cerr), Cause: cerr}
			}
			return nil, localConcurrency(p, c)
		}
	}
	s.inUse.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.inUse.Add(-1)
			s.sem.Release(1)
		})
	}, nil
}
