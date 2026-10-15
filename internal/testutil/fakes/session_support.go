package fakes

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var (
	_ session.ControlPublisher    = (*ControlPublisher)(nil)
	_ session.OpeningPromptSource = (*OpeningSource)(nil)
)

// PublishedControl is one message the Service published.
type PublishedControl struct {
	SessionID uuid.UUID
	Msg       session.ControlMessage
}

// ControlPublisher records the control messages of session transitions.
type ControlPublisher struct {
	FailInjector
	mu   sync.Mutex
	sent []PublishedControl
}

// Publish implements session.ControlPublisher.
func (c *ControlPublisher) Publish(_ context.Context, id uuid.UUID, msg session.ControlMessage) error {
	if err := c.Take("Publish"); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, PublishedControl{SessionID: id, Msg: msg})
	return nil
}

// Sent returns the messages published so far.
func (c *ControlPublisher) Sent() []PublishedControl {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]PublishedControl(nil), c.sent...)
}

// OpeningSource answers PublishedPromptVersionID with ID (nil: no PUBLISHED version).
type OpeningSource struct {
	FailInjector
	mu sync.Mutex
	ID *uuid.UUID
}

// Set changes the answer.
func (o *OpeningSource) Set(id *uuid.UUID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ID = id
}

// PublishedPromptVersionID implements session.OpeningPromptSource.
func (o *OpeningSource) PublishedPromptVersionID(_ context.Context, _ string) (*uuid.UUID, error) {
	if err := o.Take("PublishedPromptVersionID"); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ID, nil
}
