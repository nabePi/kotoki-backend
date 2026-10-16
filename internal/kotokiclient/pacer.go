package kotokiclient

import (
	"context"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// FrameInterval is the spacing of upstream audio frames (§7.4.4: one 20 ms Opus frame).
const FrameInterval = 20 * time.Millisecond

// Pacer paces a real-time frame loop against an absolute schedule: the n-th Wait returns at
// start + n×20 ms, however late the previous waits woke up, so lateness never accumulates into
// drift. If the caller falls behind, Wait returns at once until it has caught up. The clock and
// the sleep are injected (no direct time.Now) so tests are deterministic. Not safe for
// concurrent use.
//
// Typical loop: send frame 0 immediately, then `Wait` before every following frame.
type Pacer struct {
	start time.Time
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	n     int64
}

// NewPacer returns a Pacer whose schedule starts at start. now reports the current time and
// sleep blocks for d or until ctx is done (returning ctx.Err()).
func NewPacer(start time.Time, now func() time.Time, sleep func(ctx context.Context, d time.Duration) error) *Pacer {
	return &Pacer{start: start, now: now, sleep: sleep}
}

// NewClockPacer is NewPacer with the clock's time and a real, context-aware sleep.
func NewClockPacer(start time.Time, clk clock.Clock) *Pacer {
	return NewPacer(start, clk.Now, SleepContext)
}

// SleepContext sleeps for d or until ctx is done, whichever is first.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait blocks until the next slot, start + n×20 ms for n = 1, 2, ….
func (p *Pacer) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.n++
	target := p.start.Add(time.Duration(p.n) * FrameInterval)
	if d := target.Sub(p.now()); d > 0 {
		return p.sleep(ctx, d)
	}
	return nil
}
