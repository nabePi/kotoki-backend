package amqp

import (
	"context"
	"log/slog"

	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// Process exposes Consumer.process to the external tests.
func (c *Consumer) Process(root context.Context, msg amqp091.Delivery) { c.process(root, msg) }

// NewConsumerForTest builds a Consumer without a Conn.
func NewConsumerForTest(h Handler, pub Publisher, processed ProcessedChecker, m *Metrics, log *slog.Logger, clk clock.Clock) *Consumer {
	return &Consumer{h: h, pub: pub, processed: processed, m: m, log: log, clk: clk}
}
