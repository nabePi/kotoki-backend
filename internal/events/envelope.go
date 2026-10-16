package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Envelope is the body of every AMQP message (spec 9.4). All fields are required except tracestate.
type Envelope struct {
	EventID       uuid.UUID       `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  int             `json:"event_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Producer      string          `json:"producer"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   uuid.UUID       `json:"aggregate_id"`
	UserID        uuid.UUID       `json:"user_id"`
	Traceparent   string          `json:"traceparent"`
	Tracestate    *string         `json:"tracestate"`
	Payload       json.RawMessage `json:"payload"`
}

// MarshalJSON writes occurred_at as RFC 3339 UTC with exactly three fractional digits (spec 9.4).
func (e Envelope) MarshalJSON() ([]byte, error) {
	type plain Envelope
	return json.Marshal(struct {
		plain
		OccurredAt string `json:"occurred_at"`
	}{plain: plain(e), OccurredAt: millis(e.OccurredAt)})
}

// Envelope builds the message body of the event; traceparent and tracestate come from the trace
// context stored with the outbox row. OccurredAt is truncated to milliseconds in UTC.
func (e Event) Envelope(traceparent string, tracestate *string) (Envelope, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode %s payload: %w", e.EventType, err)
	}
	return Envelope{
		EventID: e.EventID, EventType: e.EventType, EventVersion: e.EventVersion,
		OccurredAt: e.OccurredAt.UTC().Truncate(time.Millisecond), Producer: e.Producer,
		AggregateType: e.AggregateType, AggregateID: e.AggregateID, UserID: e.UserID,
		Traceparent: traceparent, Tracestate: tracestate, Payload: payload,
	}, nil
}

// DecodeEnvelope parses a message body. Unknown fields are ignored (spec 9.4 additive versioning);
// event_id, event_type, event_version >= 1 and payload are mandatory.
func DecodeEnvelope(b []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	switch {
	case env.EventID == uuid.Nil:
		return Envelope{}, errors.New("decode envelope: event_id missing")
	case env.EventType == "":
		return Envelope{}, errors.New("decode envelope: event_type missing")
	case env.EventVersion < 1:
		return Envelope{}, fmt.Errorf("decode envelope: event_version %d < 1", env.EventVersion)
	case len(env.Payload) == 0 || string(env.Payload) == "null":
		return Envelope{}, errors.New("decode envelope: payload missing")
	}
	return env, nil
}

// DecodePayload decodes the payload of env into T; a failure is a PermanentError DECODE_ERROR, since
// redelivering the same bytes cannot succeed.
func DecodePayload[T any](env Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		return v, Permanent("DECODE_ERROR", err)
	}
	return v, nil
}
