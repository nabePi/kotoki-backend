package provider

import (
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// BreakerState is the circuit state; the value doubles as the gauge value.
type BreakerState int

// Breaker states.
const (
	BreakerClosed   BreakerState = 0
	BreakerOpen     BreakerState = 1
	BreakerHalfOpen BreakerState = 2
)

// BreakerConfig are the PROVIDER_CB_* parameters.
type BreakerConfig struct {
	Threshold      int
	Window         time.Duration
	OpenDuration   time.Duration
	HalfOpenProbes int
}

// Breaker is a per-(vendor, capability) circuit breaker (D-043, spec 8.5.4). It has no
// vendor fallback: it fails fast while open and probes for recovery.
type Breaker struct {
	cfg          BreakerConfig
	clk          clock.Clock
	onTransition func(from, to BreakerState)

	mu       sync.Mutex
	state    BreakerState
	failures []time.Time
	openedAt time.Time
	probes   int    // probes in flight (HALF_OPEN)
	epoch    uint64 // bumped on every transition so stale probe tickets are ignored
}

func newBreaker(cfg BreakerConfig, clk clock.Clock, onTransition func(from, to BreakerState)) *Breaker {
	if cfg.HalfOpenProbes < 1 {
		cfg.HalfOpenProbes = 1
	}
	return &Breaker{cfg: cfg, clk: clk, onTransition: onTransition}
}

// Ticket must be finished with Done exactly once.
type Ticket struct {
	b     *Breaker
	probe bool
	epoch uint64
}

// State returns the current state.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

type transition struct{ from, to BreakerState }

// move changes the state under b.mu and returns the transition to report afterwards.
func (b *Breaker) move(to BreakerState) transition {
	from := b.state
	b.state = to
	b.epoch++
	return transition{from, to}
}

func (b *Breaker) notify(ts ...transition) {
	if b.onTransition == nil {
		return
	}
	for _, t := range ts {
		b.onTransition(t.from, t.to)
	}
}

// Allow asks permission for one call. The returned Ticket must be completed with Done.
func (b *Breaker) Allow() (Ticket, bool) {
	var tr []transition
	defer func() { b.notify(tr...) }()
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	switch b.state {
	case BreakerOpen:
		if now.Before(b.openedAt.Add(b.cfg.OpenDuration)) {
			return Ticket{}, false
		}
		tr = append(tr, b.move(BreakerHalfOpen))
		b.probes = 1
		return Ticket{b: b, probe: true, epoch: b.epoch}, true
	case BreakerHalfOpen:
		if b.probes >= b.cfg.HalfOpenProbes {
			return Ticket{}, false
		}
		b.probes++
		return Ticket{b: b, probe: true, epoch: b.epoch}, true
	default:
		return Ticket{b: b, epoch: b.epoch}, true
	}
}

// Done reports the outcome: nil is a success, errors that CountsForBreaker are failures,
// anything else (cancellation, content filter, local concurrency) is ignored.
func (t Ticket) Done(err error) {
	if t.b == nil {
		return
	}
	var tr []transition
	defer func() { t.b.notify(tr...) }()
	b := t.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.probe {
		if t.epoch != b.epoch || b.state != BreakerHalfOpen {
			return // the breaker moved on while this probe ran
		}
		switch {
		case err == nil:
			b.failures = b.failures[:0]
			b.probes = 0
			tr = append(tr, b.move(BreakerClosed))
		case CountsForBreaker(err):
			b.openedAt = b.clk.Now()
			b.probes = 0
			tr = append(tr, b.move(BreakerOpen))
		default:
			b.probes-- // no verdict: free the probe slot
		}
		return
	}
	if err == nil || !CountsForBreaker(err) || b.state != BreakerClosed {
		return
	}
	now := b.clk.Now()
	cutoff := now.Add(-b.cfg.Window)
	kept := b.failures[:0]
	for _, f := range b.failures {
		if !f.Before(cutoff) {
			kept = append(kept, f)
		}
	}
	kept = append(kept, now)
	b.failures = kept
	if len(b.failures) >= b.cfg.Threshold {
		b.openedAt = now
		tr = append(tr, b.move(BreakerOpen))
	}
}

// WouldAllow reports whether Allow would admit a call right now, without consuming a
// probe slot or changing state.
func (b *Breaker) WouldAllow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerOpen:
		return !b.clk.Now().Before(b.openedAt.Add(b.cfg.OpenDuration))
	case BreakerHalfOpen:
		return b.probes < b.cfg.HalfOpenProbes
	default:
		return true
	}
}
