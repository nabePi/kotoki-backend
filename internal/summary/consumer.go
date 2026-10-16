package summary

import (
	"context"
	"time"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// handlerTimeout bounds one Handle call (spec 9.9.1: summary 60 s).
const handlerTimeout = 60 * time.Second

// Consumer is the queue handler of summary.session_completed. It satisfies amqp.Handler (spec
// 9.7.1) structurally, so this package imports events only, never amqp.
type Consumer struct {
	s *Service
}

// NewConsumer returns the handler of s.
func NewConsumer(s *Service) *Consumer { return &Consumer{s: s} }

// Queue is the queue name and the consumer name in processed_events.
func (c *Consumer) Queue() string { return events.QueueSummary }

// SupportedEvents maps each handled event type to the highest version understood.
func (c *Consumer) SupportedEvents() map[string]int {
	return map[string]int{events.SessionCompleted: 1}
}

// Timeout bounds one Handle call.
func (c *Consumer) Timeout() time.Duration { return handlerTimeout }

// Handle decodes the session.completed payload (DECODE_ERROR is permanent) and writes the summary.
func (c *Consumer) Handle(ctx context.Context, d events.Delivery) error {
	p, err := events.DecodePayload[events.SessionCompletedV1](d.Envelope)
	if err != nil {
		return err
	}
	return c.s.Process(ctx, d.Envelope, p)
}

// OnFailure logs the failure and counts it; summary keeps no status in the database.
func (c *Consumer) OnFailure(ctx context.Context, d events.Delivery, cause error, final bool) {
	c.s.RecordFailure(ctx, d.Envelope, cause, final)
}
