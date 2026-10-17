package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// OutboxEvent is an event_outbox row as far as the RabbitMQ management API needs it.
type OutboxEvent struct {
	EventID, EventType                 string
	EventVersion                       int
	AggregateType, AggregateID, UserID string
	Producer                           string
	Payload                            json.RawMessage
	OccurredAt                         time.Time
}

// envelope is the JSON body of a message (events.Envelope, spec 9.3.3), written out here so that
// the harness stays independent of the feature packages.
type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  int             `json:"event_version"`
	OccurredAt    string          `json:"occurred_at"`
	Producer      string          `json:"producer"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	UserID        string          `json:"user_id"`
	Traceparent   string          `json:"traceparent"`
	Tracestate    *string         `json:"tracestate"`
	Payload       json.RawMessage `json:"payload"`
}

// PublishBody builds the body of POST /api/exchanges/<vhost>/kotoki.events/publish that publishes e
// again: same message id, same envelope, same routing key.
func PublishBody(e OutboxEvent) (string, error) {
	env, err := json.Marshal(envelope{
		EventID: e.EventID, EventType: e.EventType, EventVersion: e.EventVersion,
		OccurredAt: e.OccurredAt.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00"),
		Producer:   e.Producer, AggregateType: e.AggregateType, AggregateID: e.AggregateID, UserID: e.UserID,
		Payload: e.Payload,
	})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"properties": map[string]any{
			"message_id": e.EventID, "type": e.EventType, "content_type": "application/json",
			"delivery_mode": 2, "app_id": "kotoki", "headers": map[string]any{"x-event-version": e.EventVersion},
		},
		"routing_key": e.EventType, "payload": string(env), "payload_encoding": "string",
	})
	return string(body), err
}

// Republisher resolves the token of F13 into the publish body of a session.completed event that
// was published after Since, so the consumers see the same event a second time.
type Republisher struct {
	Find  func(ctx context.Context, since time.Time) (OutboxEvent, error)
	Since time.Time
}

// Resolve implements the Resolve hook of driver.Compose.
func (r Republisher) Resolve(ctx context.Context, token string) (string, error) {
	if token != scenario.F13Token {
		return "", fmt.Errorf("republisher: unknown token %q", token)
	}
	e, err := r.Find(ctx, r.Since)
	if err != nil {
		return "", err
	}
	return PublishBody(e)
}

// FindPublished returns a session.completed row of event_outbox that was published at least 20
// seconds ago (the consumers have it) and after since: the newest.
func FindPublished(dsn string) func(ctx context.Context, since time.Time) (OutboxEvent, error) {
	return func(ctx context.Context, since time.Time) (OutboxEvent, error) {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return OutboxEvent{}, errors.New("republisher: connect to the database failed")
		}
		defer pool.Close()
		var e OutboxEvent
		var payload []byte
		err = pool.QueryRow(ctx, `
SELECT event_id::text, event_type, event_version, aggregate_type, aggregate_id::text, user_id::text, producer, payload, occurred_at
  FROM event_outbox
 WHERE event_type = 'session.completed' AND status = 'PUBLISHED' AND created_at >= $1 AND published_at < now() - interval '20 seconds'
 ORDER BY published_at DESC LIMIT 1`, since).Scan(&e.EventID, &e.EventType, &e.EventVersion, &e.AggregateType,
			&e.AggregateID, &e.UserID, &e.Producer, &payload, &e.OccurredAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return OutboxEvent{}, errors.New("republisher: no session.completed event was published (and 20 s old) since the run started")
		}
		if err != nil {
			return OutboxEvent{}, fmt.Errorf("republisher: %w", err)
		}
		e.Payload = payload
		return e, nil
	}
}
