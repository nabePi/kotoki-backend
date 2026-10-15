package conversation

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// fencedEventAppender is the part of session.EventRepository the writer uses.
type fencedEventAppender interface {
	AppendFenced(ctx context.Context, fence int64, e session.Event) error
}

type sessionEventWriter struct {
	repo fencedEventAppender
	ids  idgen.Generator
	clk  clock.Clock
}

// NewSessionEventWriter adapts session.EventRepository to SessionEventWriter: actor REALTIME,
// from/to status NULL (runtime events are not transitions), payload JSON without transcript text.
func NewSessionEventWriter(repo interface {
	AppendFenced(ctx context.Context, fence int64, e session.Event) error
}, ids idgen.Generator, clk clock.Clock) SessionEventWriter {
	return &sessionEventWriter{repo: repo, ids: ids, clk: clk}
}

// Append implements SessionEventWriter. A nil payload is stored as {}.
func (w *sessionEventWriter) Append(ctx context.Context, fence int64, sid uuid.UUID, eventType string, turnID *uuid.UUID, payload any) error {
	raw := json.RawMessage("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternalError, "marshal session event payload")
		}
		raw = b
	}
	return w.repo.AppendFenced(ctx, fence, session.Event{
		ID: w.ids.New(), SessionID: sid, Type: session.EventType(eventType), Actor: session.ActorRealtime,
		TurnID: turnID, Payload: raw, CreatedAt: w.clk.Now(),
	})
}
