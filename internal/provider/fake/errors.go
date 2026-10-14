package fake

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// retryableByKind follows the default column of the taxonomy table (spec 8.3).
func retryableByKind(k provider.ErrorKind) bool {
	switch k {
	case provider.KindTimeout, provider.KindRateLimited, provider.KindUnavailable, provider.KindInvalidResponse:
		return true
	}
	return false
}

func errorFor(name provider.Name, c provider.Capability, kind provider.ErrorKind) *provider.Error {
	e := provider.NewError(name, c, kind, "fake: injected "+string(kind))
	e.Retryable = retryableByKind(kind)
	e.VendorCode = "fake_injected"
	return e
}

func cancelledErr(name provider.Name, c provider.Capability, cause error) *provider.Error {
	e := provider.NewError(name, c, provider.KindCancelled, "fake: context ended")
	e.Cause = cause
	return e
}

// sleep waits d or until ctx ends; it reports whether the full wait elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitterSource is a seeded, mutex-guarded PRNG shared by the requests of one fake.
type jitterSource struct {
	mu sync.Mutex
	r  *rand.Rand
}

func newJitterSource(seed int64, salt uint64) *jitterSource {
	return &jitterSource{r: rand.New(rand.NewPCG(uint64(seed), salt))} //nolint:gosec // deterministic test PRNG, not security-sensitive
}

// delay returns base ± jitter milliseconds (never negative).
func (j *jitterSource) delay(baseMS, jitterMS int) time.Duration {
	ms := baseMS
	if jitterMS > 0 {
		j.mu.Lock()
		ms += j.r.IntN(2*jitterMS+1) - jitterMS
		j.mu.Unlock()
	}
	return time.Duration(max(ms, 0)) * time.Millisecond
}
