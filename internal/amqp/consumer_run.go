package amqp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	amqp091 "github.com/rabbitmq/amqp091-go"
)

// lifecycle is the shutdown state shared by Run and Shutdown. It is created lazily so the zero
// Consumer (see NewConsumerForTest) stays usable.
type lifecycle struct {
	stop       chan struct{} // closed by Shutdown: stop taking deliveries
	done       chan struct{} // closed when Run has returned (all in-flight handlers finished)
	rootCtx    context.Context
	rootCancel context.CancelFunc
	stopOnce   sync.Once
	started    atomic.Bool
}

func (c *Consumer) life() *lifecycle {
	c.lifeOnce.Do(func() {
		root, cancel := context.WithCancel(context.Background())
		c.lc = &lifecycle{stop: make(chan struct{}), done: make(chan struct{}), rootCtx: root, rootCancel: cancel}
	})
	return c.lc
}

// prefetchOf is the per-pod prefetch of queue (spec 9.6.2); an unknown queue gets 1.
func prefetchOf(queue string) int {
	for _, q := range Queues {
		if q.Name == queue {
			return q.Prefetch
		}
	}
	return 1
}

// Run consumes the handler's queue until ctx is done or Shutdown is called, reconnecting with
// Backoff when the connection or channel drops (spec 9.7.6). It returns once no handler is in
// flight. Handlers run under a root context that only Shutdown cancels (after its grace), so
// cancelling ctx alone stops taking deliveries but lets in-flight handlers finish within their own
// timeout. Run may be called once.
func (c *Consumer) Run(ctx context.Context) error {
	lc := c.life()
	lc.started.Store(true)
	defer close(lc.done)

	for attempt := 0; ; {
		if c.halted(ctx) {
			return nil
		}
		consuming, err := c.consumeOnce(ctx, lc)
		if c.halted(ctx) {
			return nil
		}
		if consuming {
			attempt = 0
		}
		if err != nil {
			c.log.Warn("consumer stopped, will reconnect", "queue", c.h.Queue(), "error", err)
		}
		if rerr := c.conn.Reconnect(ctx); rerr != nil {
			if c.halted(ctx) {
				return nil
			}
			return fmt.Errorf("amqp: consumer %s reconnect: %w", c.h.Queue(), rerr)
		}
		if err != nil || !consuming {
			// the connection is healthy but the channel/consume failed: do not spin
			select {
			case <-ctx.Done():
			case <-lc.stop:
			case <-time.After(Backoff(attempt, rand.Float64)):
			}
			attempt++
		}
	}
}

func (c *Consumer) halted(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-c.life().stop:
		return true
	default:
		return false
	}
}

// consumeOnce opens a channel, sets Qos and consumes with prefetch workers until the consumption
// is halted (ctx, Shutdown) or the channel/connection dies. It returns after every worker has
// finished. consuming reports whether Consume succeeded.
func (c *Consumer) consumeOnce(ctx context.Context, lc *lifecycle) (consuming bool, err error) {
	queue := c.h.Queue()
	prefetch := prefetchOf(queue)
	connClosed := c.conn.Closed()
	ch, err := c.conn.Channel()
	if err != nil {
		return false, err
	}
	defer func() { _ = ch.Close() }()
	chClosed := ch.NotifyClose(make(chan *amqp091.Error, 1))
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return false, fmt.Errorf("qos: %w", err)
	}
	tag := queue + "-" + uuid.NewString()
	deliveries, err := ch.Consume(queue, tag, false, false, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consume: %w", err)
	}

	var halting atomic.Bool
	var wg sync.WaitGroup
	for range prefetch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range deliveries {
				if halting.Load() {
					// taken from the broker but not started: give it back, it is not a retry
					if nerr := d.Nack(false, true); nerr != nil {
						c.log.Warn("nack requeue of unstarted delivery failed", "queue", queue, "error", nerr)
					}
					c.m.ConsumerMessages.WithLabelValues(queue, resultRequeue).Inc()
					continue
				}
				c.process(lc.rootCtx, d)
			}
		}()
	}

	var cause error
	select {
	case <-ctx.Done():
	case <-lc.stop:
	case <-connClosed:
		cause = ErrBrokerUnavailable
	case e := <-chClosed:
		cause = ErrBrokerUnavailable
		if e != nil {
			cause = fmt.Errorf("%w: %w", ErrBrokerUnavailable, e)
		}
	}
	halting.Store(true)
	// basic.cancel: the broker stops sending and the deliveries channel closes once the already
	// sent ones were handed over; a dead channel just fails here and closes it the same way.
	if cerr := ch.Cancel(tag, false); cerr != nil && cause == nil {
		cause = cerr
	}
	wg.Wait()
	return true, cause
}

// Shutdown is spec 9.7.7 steps 3 to 5: stop taking deliveries (basic.cancel; deliveries already
// taken but not started are nacked with requeue), wait up to grace for in-flight handlers, then
// cancel their root context so they return context.Canceled (nack requeue, no OnFailure, no retry
// count). It returns when Run has finished, or with ctx's error if ctx ends first.
func (c *Consumer) Shutdown(ctx context.Context, grace time.Duration) error {
	lc := c.life()
	lc.stopOnce.Do(func() { close(lc.stop) })
	if !lc.started.Load() {
		lc.rootCancel()
		return nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-lc.done:
		lc.rootCancel()
		return nil
	case <-timer.C:
		lc.rootCancel()
	case <-ctx.Done():
		lc.rootCancel()
		return ctx.Err()
	}
	select {
	case <-lc.done:
		return nil
	case <-ctx.Done():
		return errors.Join(ctx.Err(), errors.New("amqp: handlers still running after cancel"))
	}
}
