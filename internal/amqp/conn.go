package amqp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

const (
	dialTimeout  = 10 * time.Second
	heartbeat    = 10 * time.Second
	maxBackoff   = 30 * time.Second
	jitterSpread = 0.2
)

// ErrBrokerUnavailable means there is no usable connection or channel right now. It is not the
// broker's verdict on a message, so the outbox must not count it as a publish attempt.
var ErrBrokerUnavailable = errors.New("amqp: broker unavailable")

// Backoff is the reconnect delay: min(2^attempt s, 30 s) x (1 + jitter), jitter in [-20%, +20%]
// taken from rnd() in [0,1). A negative attempt behaves as 0.
func Backoff(attempt int, rnd func() float64) time.Duration {
	attempt = min(max(attempt, 0), 6) // 2^6 s is already past the cap; avoids overflow
	base := min(time.Duration(1<<attempt)*time.Second, maxBackoff)
	factor := 1 + (rnd()*2*jitterSpread - jitterSpread)
	return time.Duration(math.Round(float64(base) * factor))
}

// Conn is one AMQP connection that can be re-established. Publisher and consumer use separate Conns.
type Conn struct {
	url  string
	name string
	log  *slog.Logger

	// redial serialises Reconnect (a 1-slot semaphore so a waiter can give up with its ctx): the
	// callers that share a Conn (consumers, the keep-alive) must not each dial a connection.
	redial chan struct{}

	mu     sync.Mutex
	conn   *amqp091.Connection
	closed chan struct{} // closed when the current connection drops
	done   bool          // Close was called
}

// Dial connects (one attempt, bounded by ctx and a dial timeout) and declares the topology.
func Dial(ctx context.Context, url, name string, log *slog.Logger) (*Conn, error) {
	c := &Conn{url: url, name: name, log: log, redial: make(chan struct{}, 1)}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Conn) connect(ctx context.Context) error {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := amqp091.DialConfig(c.url, amqp091.Config{
		Heartbeat:  heartbeat,
		Locale:     "en_US",
		Properties: amqp091.Table{"connection_name": c.name},
		Dial: func(network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
	})
	if err != nil {
		return fmt.Errorf("amqp: dial %s: %w", c.name, err)
	}
	ch, err := conn.Channel()
	if err == nil {
		err = DeclareTopology(ch)
		_ = ch.Close()
	}
	if err != nil {
		_ = conn.Close()
		return err
	}

	closed := make(chan struct{})
	notify := conn.NotifyClose(make(chan *amqp091.Error, 1))
	go func() {
		if e := <-notify; e != nil {
			c.log.Warn("amqp connection lost", "conn", c.name, "err", e)
		}
		close(closed)
	}()

	c.mu.Lock()
	c.conn, c.closed = conn, closed
	c.mu.Unlock()
	return nil
}

// Channel opens a channel on the current connection.
func (c *Conn) Channel() (*amqp091.Channel, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || conn.IsClosed() {
		return nil, ErrBrokerUnavailable
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBrokerUnavailable, err)
	}
	return ch, nil
}

// Healthy reports whether the current connection is open.
func (c *Conn) Healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.done && c.conn != nil && !c.conn.IsClosed()
}

// Closed returns a channel that is closed when the current connection drops. After a successful
// Reconnect, call Closed again for the new connection.
func (c *Conn) Closed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Reconnect re-establishes the connection, retrying with Backoff until it succeeds or ctx ends.
// It returns nil at once when the connection is already healthy, also when another caller just
// re-established it (concurrent callers dial once).
func (c *Conn) Reconnect(ctx context.Context) error {
	if c.Healthy() {
		return nil
	}
	select {
	case c.redial <- struct{}{}:
		defer func() { <-c.redial }()
	case <-ctx.Done():
		return ctx.Err()
	}
	for attempt := 0; ; attempt++ {
		if c.Healthy() {
			return nil
		}
		c.mu.Lock()
		done, old := c.done, c.conn
		c.mu.Unlock()
		if done {
			return ErrBrokerUnavailable
		}
		if old != nil {
			_ = old.Close()
		}
		err := c.connect(ctx)
		if err == nil {
			c.log.Info("amqp connection established", "conn", c.name, "attempt", attempt+1)
			return nil
		}
		delay := Backoff(attempt, rand.Float64)
		c.log.Warn("amqp reconnect failed", "conn", c.name, "attempt", attempt+1, "retry_in", delay, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// Close closes the connection for good; Reconnect afterwards fails.
func (c *Conn) Close() error {
	c.mu.Lock()
	c.done = true
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || conn.IsClosed() {
		return nil
	}
	return conn.Close()
}
