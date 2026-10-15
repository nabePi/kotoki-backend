package fakes

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ conversation.SessionEventWriter = (*EventWriter)(nil)

// WrittenEvent is one event the runtime appended.
type WrittenEvent struct {
	Fence     int64
	SessionID uuid.UUID
	Type      string
	TurnID    *uuid.UUID
	Payload   json.RawMessage // the payload as JSON, as the database would store it
}

// EventWriter records runtime events. SetFence arms the fence guard (0: off); FailNext("Append",
// err) fails the next append.
type EventWriter struct {
	FailInjector
	mu    sync.Mutex
	fence int64
	// FenceOf, when set, is the fence of the session in the database (SessionRuntime.Fence).
	FenceOf  func(sid uuid.UUID) int64
	events   []WrittenEvent
	attempts []WriteAttempt
}

// Attempts returns every Append call so far in order (Method is the event type), the refused
// ones included.
func (w *EventWriter) Attempts() []WriteAttempt {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]WriteAttempt(nil), w.attempts...)
}

// NewEventWriter returns an empty writer.
func NewEventWriter() *EventWriter { return &EventWriter{} }

// SetFence makes appends with any other fence fail with session.ErrFenceLost (0 disables).
func (w *EventWriter) SetFence(f int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fence = f
}

// Append implements conversation.SessionEventWriter.
func (w *EventWriter) Append(_ context.Context, fence int64, sid uuid.UUID, eventType string, turnID *uuid.UUID, payload any) error {
	taken := w.Take("Append")
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if taken == nil && w.fence != 0 && w.fence != fence {
		taken = session.ErrFenceLost
	}
	if taken == nil && w.FenceOf != nil {
		if cur := w.FenceOf(sid); cur != 0 && cur != fence {
			taken = session.ErrFenceLost
		}
	}
	w.attempts = append(w.attempts, WriteAttempt{Method: eventType, Fence: fence, OK: taken == nil})
	if taken != nil {
		return taken
	}
	w.events = append(w.events, WrittenEvent{Fence: fence, SessionID: sid, Type: eventType, TurnID: turnID, Payload: raw})
	return nil
}

// Events returns the events appended so far.
func (w *EventWriter) Events() []WrittenEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]WrittenEvent(nil), w.events...)
}

// Types returns the event types appended so far, in order.
func (w *EventWriter) Types() []string {
	var out []string
	for _, e := range w.Events() {
		out = append(out, e.Type)
	}
	return out
}
