package evaluation

import (
	"context"
	"time"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// Consumer is the queue handler of evaluation.session_completed. It satisfies amqp.Handler
// (spec 9.7.1) structurally, so this package imports events only, never amqp.
type Consumer struct {
	s   *Service
	cfg Config
}

// NewConsumer returns the handler of s; cfg supplies the handler timeout.
func NewConsumer(s *Service, cfg Config) *Consumer { return &Consumer{s: s, cfg: cfg} }

// Queue is the queue name and the consumer name in processed_events.
func (c *Consumer) Queue() string { return events.QueueEvaluation }

// SupportedEvents maps each handled event type to the highest version understood.
func (c *Consumer) SupportedEvents() map[string]int {
	return map[string]int{events.SessionCompleted: 1}
}

// Timeout bounds one Handle call.
func (c *Consumer) Timeout() time.Duration { return c.cfg.HandlerTimeout }

// Handle decodes the session.completed payload (DECODE_ERROR is permanent) and evaluates the
// session.
func (c *Consumer) Handle(ctx context.Context, d events.Delivery) error {
	p, err := events.DecodePayload[events.SessionCompletedV1](d.Envelope)
	if err != nil {
		return err
	}
	return c.s.Process(ctx, d.Envelope, p)
}

// OnFailure records the failure before a retry (final false) or dead-lettering (final true).
func (c *Consumer) OnFailure(ctx context.Context, d events.Delivery, cause error, final bool) {
	c.s.RecordFailure(ctx, d.Envelope, cause, final)
}
