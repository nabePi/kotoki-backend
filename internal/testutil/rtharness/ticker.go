package rtharness

import (
	"sync"
	"time"
)

// fireWait bounds how long Fire waits for a ticker whose consumer is busy or gone, so a test
// fails visibly instead of hanging.
const fireWait = 2 * time.Second

// ManualTicker is a conversation.Ticker that ticks only when the test says so. The channel is
// unbuffered: Fire returns once the consumer has received the tick, so the work of that tick has
// started (and, for the single-threaded runtime loop, a later Runtime.Barrier waits for its end).
type ManualTicker struct {
	d       time.Duration
	c       chan time.Time
	stopped chan struct{}
	once    sync.Once
}

// C implements conversation.Ticker.
func (t *ManualTicker) C() <-chan time.Time { return t.c }

// Stop implements conversation.Ticker; later Fire calls skip the ticker.
func (t *ManualTicker) Stop() { t.once.Do(func() { close(t.stopped) }) }

// Interval is the duration the ticker was created with.
func (t *ManualTicker) Interval() time.Duration { return t.d }

// fire delivers one tick; false when the ticker is stopped or its consumer did not take it.
func (t *ManualTicker) fire(now time.Time) bool {
	select {
	case <-t.stopped:
		return false
	default:
	}
	timer := time.NewTimer(fireWait)
	defer timer.Stop()
	select {
	case t.c <- now:
		return true
	case <-t.stopped:
		return false
	case <-timer.C:
		return false
	}
}

// Tickers creates the ManualTickers of one harness (Deps.NewTicker) and fires them by interval.
type Tickers struct {
	mu   sync.Mutex
	list []*ManualTicker
}

// New is Deps.NewTicker.
func (ts *Tickers) New(d time.Duration) *ManualTicker {
	t := &ManualTicker{d: d, c: make(chan time.Time), stopped: make(chan struct{})}
	ts.mu.Lock()
	ts.list = append(ts.list, t)
	ts.mu.Unlock()
	return t
}

// Fire ticks every live ticker whose interval satisfies match, in creation order, and returns how
// many took the tick.
func (ts *Tickers) Fire(now time.Time, match func(time.Duration) bool) int {
	ts.mu.Lock()
	live := ts.list[:0]
	for _, t := range ts.list {
		select {
		case <-t.stopped:
		default:
			live = append(live, t)
		}
	}
	ts.list = live
	snapshot := append([]*ManualTicker(nil), live...)
	ts.mu.Unlock()
	n := 0
	for _, t := range snapshot {
		if match(t.d) && t.fire(now) {
			n++
		}
	}
	return n
}
