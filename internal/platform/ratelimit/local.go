package ratelimit

import (
	"container/list"
	"context"
	"math"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// Local is an in-memory token-bucket limiter with LRU eviction. It is the
// fail-open fallback when Redis is unavailable (spec 13.4.3).
type Local struct {
	clk        clock.Clock
	maxEntries int
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      *list.List // front = most recently used
}

var _ Limiter = (*Local)(nil)

type bucket struct {
	key    string
	tokens float64
	at     time.Time
}

// NewLocal returns a limiter keeping at most maxEntries buckets.
func NewLocal(clk clock.Clock, maxEntries int) *Local {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Local{clk: clk, maxEntries: maxEntries, entries: map[string]*list.Element{}, order: list.New()}
}

// Len reports how many buckets are held.
func (l *Local) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

// Allow takes one token from the bucket of (policy, subject).
func (l *Local) Allow(_ context.Context, p Policy, subject string) (Decision, error) {
	now := l.clk.Now()
	key := p.Name + "\x00" + subject
	rate := p.RatePerMs()
	capacity := float64(p.Capacity)

	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if el, ok := l.entries[key]; ok {
		l.order.MoveToFront(el)
		b = el.Value.(*bucket)
	} else {
		b = &bucket{key: key, tokens: capacity, at: now}
		l.entries[key] = l.order.PushFront(b)
		for l.order.Len() > l.maxEntries {
			oldest := l.order.Back()
			delete(l.entries, oldest.Value.(*bucket).key)
			l.order.Remove(oldest)
		}
	}
	elapsedMs := float64(now.Sub(b.at) / time.Millisecond)
	b.tokens = math.Min(capacity, b.tokens+math.Max(0, elapsedMs)*rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return Decision{Allowed: true, Remaining: int(math.Floor(b.tokens))}, nil
	}
	retryMs := math.Ceil((1 - b.tokens) / rate)
	return Decision{Allowed: false, Remaining: 0, RetryAfter: time.Duration(retryMs) * time.Millisecond}, nil
}
