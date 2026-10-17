package amqp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

// Publisher confirm outcomes.
var (
	ErrNacked         = errors.New("amqp: publish nacked by broker")
	ErrConfirmTimeout = errors.New("amqp: publish confirm timeout")
)

// Confirmation is the pending broker verdict of one published message.
type Confirmation interface {
	// Wait blocks for the verdict. acked=false with a nil error means the broker nacked.
	// It returns ErrConfirmTimeout when the verdict does not arrive in time and
	// ErrBrokerUnavailable when the channel died first.
	Wait(ctx context.Context) (acked bool, err error)
}

// ConfirmPublisher publishes on a channel in confirm mode and recreates the channel when it dies.
type ConfirmPublisher struct {
	conn    *Conn
	timeout time.Duration

	mu sync.Mutex
	ch *amqp091.Channel
}

// NewConfirmPublisher returns a publisher whose confirmations time out after confirmTimeout.
func NewConfirmPublisher(conn *Conn, confirmTimeout time.Duration) *ConfirmPublisher {
	return &ConfirmPublisher{conn: conn, timeout: confirmTimeout}
}

func (p *ConfirmPublisher) channel() (*amqp091.Channel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	p.ch = nil
	ch, err := p.conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("%w: %w", ErrBrokerUnavailable, err)
	}
	p.ch = ch
	return ch, nil
}

// IsOpen reports whether a publish can be attempted: the connection is healthy. A missing or dead
// channel does not count against it, because Send recreates the channel (channel()). A dead channel
// must not make IsOpen false: the outbox publisher gates each batch on IsOpen and calls Send only
// when it is true, so after a broker restart (the channel died with the old connection, nobody
// called Send in between) the publisher would never get the chance to recreate it.
func (p *ConfirmPublisher) IsOpen() bool {
	return p.conn.Healthy()
}

// Send publishes msg and returns a Confirmation to wait on. Several Sends can be in flight
// before their confirmations are awaited (the outbox publishes a batch, then waits).
func (p *ConfirmPublisher) Send(ctx context.Context, exchange, key string, msg amqp091.Publishing) (Confirmation, error) {
	ch, err := p.channel()
	if err != nil {
		return nil, err
	}
	dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, false, false, msg)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %w", ErrBrokerUnavailable, err)
	}
	return &confirmation{dc: dc, ch: ch, timeout: p.timeout}, nil
}

// PublishConfirmed sends msg and waits for the verdict: nack gives ErrNacked, no verdict in time
// gives ErrConfirmTimeout.
func (p *ConfirmPublisher) PublishConfirmed(ctx context.Context, exchange, key string, msg amqp091.Publishing) error {
	c, err := p.Send(ctx, exchange, key, msg)
	if err != nil {
		return err
	}
	acked, err := c.Wait(ctx)
	if err != nil {
		return err
	}
	if !acked {
		return ErrNacked
	}
	return nil
}

type confirmation struct {
	dc      *amqp091.DeferredConfirmation
	ch      *amqp091.Channel
	timeout time.Duration
}

func (c *confirmation) Wait(ctx context.Context) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	acked, err := c.dc.WaitContext(wctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, ErrConfirmTimeout
	}
	if !acked && c.ch.IsClosed() {
		// a channel that dies fails its pending confirmations; that is not a broker nack
		return false, ErrBrokerUnavailable
	}
	return acked, nil
}
