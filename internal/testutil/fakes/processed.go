package fakes

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
)

var _ events.ProcessedEvents = (*Processed)(nil)

// Processed is an in-memory events.ProcessedEvents with an IsProcessed pre-check, safe for
// concurrent use. It has no transaction: transactional fakes drop rows with Forget on rollback.
type Processed struct {
	FailInjector
	mu   sync.Mutex
	seen map[processedKey]struct{}
}

type processedKey struct {
	consumer string
	id       uuid.UUID
}

// NewProcessed returns an empty Processed.
func NewProcessed() *Processed { return &Processed{seen: map[processedKey]struct{}{}} }

// MarkProcessed implements events.ProcessedEvents; the second call for the same (consumer,
// event_id) returns events.ErrAlreadyProcessed.
func (p *Processed) MarkProcessed(_ context.Context, consumer string, ev events.Envelope) error {
	if err := p.Take("MarkProcessed"); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	k := processedKey{consumer, ev.EventID}
	if _, ok := p.seen[k]; ok {
		return events.ErrAlreadyProcessed
	}
	p.seen[k] = struct{}{}
	return nil
}

// IsProcessed reports whether MarkProcessed succeeded for (consumer, eventID).
func (p *Processed) IsProcessed(_ context.Context, consumer string, eventID uuid.UUID) (bool, error) {
	if err := p.Take("IsProcessed"); err != nil {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.seen[processedKey{consumer, eventID}]
	return ok, nil
}

// Forget removes a row (a rolled-back transaction).
func (p *Processed) Forget(consumer string, eventID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.seen, processedKey{consumer, eventID})
}

// Len is the number of recorded pairs.
func (p *Processed) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen)
}
