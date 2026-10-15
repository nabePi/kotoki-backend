package conversation

import (
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// Error windows of §7.4.8: more than limit events inside the window → PROTOCOL_VIOLATION.
const (
	invalidMessageLimit     = 10
	invalidMessageWindow    = 60 * time.Second
	audioFrameInvalidLimit  = 50
	audioFrameInvalidWindow = 10 * time.Second
)

// bucket is a token bucket on a clock.Clock (§7.4.10): it starts full with burst tokens and
// refills at rate tokens per second, capped at burst. Not safe for concurrent use (one reader).
type bucket struct {
	clk    clock.Clock
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(clk clock.Clock, rate, burst int) *bucket {
	return &bucket{clk: clk, rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: clk.Now()}
}

// Take spends one token; false when the bucket is empty.
func (b *bucket) Take() bool {
	now := b.clk.Now()
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(b.burst, b.tokens+el.Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// burstWindow counts events in a sliding window; Add reports whether more than limit events
// fell inside the window. Safe for concurrent use: the audio pipeline may share one with others.
type burstWindow struct {
	clk    clock.Clock
	limit  int
	window time.Duration

	mu sync.Mutex
	at []time.Time // oldest first, at most limit+1 entries
}

func newBurstWindow(clk clock.Clock, limit int, window time.Duration) *burstWindow {
	return &burstWindow{clk: clk, limit: limit, window: window}
}

// Add records one event now and reports whether the window now holds more than limit events.
func (w *burstWindow) Add() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clk.Now()
	keep := 0
	for keep < len(w.at) && now.Sub(w.at[keep]) >= w.window {
		keep++
	}
	w.at = append(w.at[keep:], now)
	if len(w.at) > w.limit+1 {
		w.at = w.at[len(w.at)-w.limit-1:]
	}
	return len(w.at) > w.limit
}
