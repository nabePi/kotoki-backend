package fakes

import (
	"context"
	"slices"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/events"
)

var _ events.OutboxWriter = (*Outbox)(nil)

// Outbox is an in-memory events.OutboxWriter that records what was enqueued. Transactional fakes
// (the session store) call Mark/Rollback to drop what a failed transaction enqueued.
type Outbox struct {
	FailInjector
	mu     sync.Mutex
	events []events.Event
}

// Enqueue implements events.OutboxWriter.
func (o *Outbox) Enqueue(_ context.Context, ev events.Event) error {
	if err := o.Take("Enqueue"); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, ev)
	return nil
}

// Events returns the enqueued events in order.
func (o *Outbox) Events() []events.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

// Len is the number of enqueued events.
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.events)
}

// Truncate keeps the first n events (a rolled-back transaction).
func (o *Outbox) Truncate(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if n < len(o.events) {
		o.events = o.events[:n]
	}
}
