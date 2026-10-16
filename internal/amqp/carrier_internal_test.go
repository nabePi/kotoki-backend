package amqp

import (
	"testing"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
)

func TestCarrier_GetSetKeys(t *testing.T) {
	h := amqp091.Table{"traceparent": "tp", "x-event-version": int32(1)}
	c := amqpHeaderCarrier(h)
	assert.Equal(t, "tp", c.Get("traceparent"))
	assert.Empty(t, c.Get("x-event-version"), "non-string header is not a trace value")
	assert.Empty(t, c.Get("missing"))
	c.Set("tracestate", "a=b")
	assert.Equal(t, "a=b", h["tracestate"], "Set writes through to the table")
	assert.ElementsMatch(t, []string{"traceparent", "x-event-version", "tracestate"}, c.Keys())
}
