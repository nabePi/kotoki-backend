package events

// Delivery is one message handed to a consumer handler (spec 9.7.1).
type Delivery struct {
	Envelope    Envelope
	Queue       string
	RetryCount  int  // header x-retry-count; 0 when absent
	Redelivered bool // AMQP redelivered flag
}
