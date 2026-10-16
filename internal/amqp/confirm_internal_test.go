package amqp

import (
	"context"
	"testing"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
)

func TestConfirmation_Wait_TimeoutAndCallerCancel(t *testing.T) {
	// a zero DeferredConfirmation never resolves, like a broker that never answers
	c := &confirmation{dc: &amqp091.DeferredConfirmation{}, timeout: 20 * time.Millisecond}

	start := time.Now()
	acked, err := c.Wait(context.Background())
	assert.False(t, acked)
	assert.ErrorIs(t, err, ErrConfirmTimeout)
	assert.Less(t, time.Since(start), 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.timeout = time.Minute
	_, err = c.Wait(ctx)
	assert.ErrorIs(t, err, context.Canceled, "caller cancel is not a confirm timeout")
	assert.NotErrorIs(t, err, ErrConfirmTimeout)
}
