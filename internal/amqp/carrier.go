package amqp

import amqp091 "github.com/rabbitmq/amqp091-go"

// amqpHeaderCarrier adapts AMQP headers to propagation.TextMapCarrier (spec 9.7.5). Only string
// headers are trace values; Set writes through to the underlying table.
type amqpHeaderCarrier amqp091.Table

func (c amqpHeaderCarrier) Get(k string) string {
	if v, ok := c[k].(string); ok {
		return v
	}
	return ""
}

func (c amqpHeaderCarrier) Set(k, v string) { c[k] = v }

func (c amqpHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
