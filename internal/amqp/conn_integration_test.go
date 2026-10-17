//go:build integration

package amqp_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func dial(t *testing.T, url string) *amqp.Conn {
	t.Helper()
	c, err := amqp.Dial(context.Background(), url, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func msg() amqp091.Publishing {
	return amqp091.Publishing{DeliveryMode: amqp091.Persistent, Body: []byte(`{}`)}
}

func TestConfirmPublisher_PublishConfirmed_Delivers(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	pub := amqp.NewConfirmPublisher(dial(t, url), 5*time.Second)

	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg()))

	assert.Equal(t, 1, adm.QueueMessages(events.QueueLearner), "confirmed means already routed and persisted")
	assert.Equal(t, 0, adm.QueueMessages(events.QueueEvaluation))
	assert.True(t, pub.IsOpen())
}

func TestConfirmPublisher_Send_BatchThenWait(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	pub := amqp.NewConfirmPublisher(dial(t, url), 5*time.Second)

	var cs []amqp.Confirmation
	for range 5 {
		c, err := pub.Send(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg())
		require.NoError(t, err)
		cs = append(cs, c)
	}
	for _, c := range cs {
		acked, err := c.Wait(context.Background())
		require.NoError(t, err)
		assert.True(t, acked)
	}
	assert.Equal(t, 5, adm.QueueMessages(events.QueueLearner))
}

func TestConfirmPublisher_Nack_ErrNacked(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	conn := dial(t, url)
	ch, err := conn.Channel()
	require.NoError(t, err)
	_, err = ch.QueueDeclare("tiny", true, false, false, false, amqp091.Table{
		"x-queue-type": "classic", "x-max-length": int64(1), "x-overflow": "reject-publish"})
	require.NoError(t, err)
	_ = ch.Close()
	pub := amqp.NewConfirmPublisher(conn, 5*time.Second)

	require.NoError(t, pub.PublishConfirmed(context.Background(), "", "tiny", msg()))
	err = pub.PublishConfirmed(context.Background(), "", "tiny", msg())
	assert.ErrorIs(t, err, amqp.ErrNacked, "a full reject-publish queue nacks (classic: quorum limits are not exact)")
}

func TestConn_ReconnectsAfterForcedClose(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	conn := dial(t, url)
	pub := amqp.NewConfirmPublisher(conn, 5*time.Second)
	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg()))
	require.True(t, conn.Healthy())
	closed := conn.Closed()

	adm.CloseAllConnections()

	require.Eventually(t, func() bool { return !conn.Healthy() }, 5*time.Second, 50*time.Millisecond)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Closed() not signalled")
	}
	assert.False(t, pub.IsOpen())
	err := pub.PublishConfirmed(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg())
	require.ErrorIs(t, err, amqp.ErrBrokerUnavailable)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, conn.Reconnect(ctx))
	assert.True(t, conn.Healthy())
	assert.Len(t, adm.Queues(), 25, "topology still there")
	select {
	case <-conn.Closed():
		t.Fatal("Closed() of the new connection must be open")
	default:
	}

	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg()))
	assert.Equal(t, 2, adm.QueueMessages(events.QueueLearner))
}

// The outbox publisher gates every batch on IsOpen and never calls Send while the broker is down,
// so a channel that died with the connection is still held when the connection is back. IsOpen
// must then be true (Send recreates the channel); the F8 chaos run showed the publisher stuck
// forever, spinning on ErrBrokerUnavailable, when IsOpen stayed false for a dead channel.
func TestConfirmPublisher_IsOpenAfterReconnect_WithoutSendDuringOutage(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	conn := dial(t, url)
	pub := amqp.NewConfirmPublisher(conn, 5*time.Second)
	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg()))

	adm.CloseAllConnections()
	require.Eventually(t, func() bool { return !conn.Healthy() }, 5*time.Second, 50*time.Millisecond)
	assert.False(t, pub.IsOpen(), "no connection, nothing to publish on")
	// deliberately no Send here: that is what the outbox publisher does during an outage

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, conn.Reconnect(ctx))

	assert.True(t, pub.IsOpen(), "healthy connection: the dead channel is recreated by the next Send")
	require.NoError(t, pub.PublishConfirmed(context.Background(), amqp.ExchangeEvents, "evaluation.completed", msg()))
	assert.Equal(t, 2, adm.QueueMessages(events.QueueLearner))
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Several goroutines (the consumers and the keep-alive of one worker share a Conn) may Reconnect
// at once; exactly one of them may dial, the others must find the new connection healthy.
func TestConn_ConcurrentReconnect_DialsOnce(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	var logs syncBuf
	conn, err := amqp.Dial(context.Background(), url, "test", slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	adm.CloseAllConnections()
	require.Eventually(t, func() bool { return !conn.Healthy() }, 5*time.Second, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, conn.Reconnect(ctx))
		}()
	}
	wg.Wait()
	assert.True(t, conn.Healthy())
	assert.Equal(t, 1, strings.Count(logs.String(), "amqp connection established"), "one dial for eight callers")
}

func TestConn_Reconnect_WaiterHonoursContext(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	conn := dial(t, url)
	adm.CloseAllConnections()
	require.Eventually(t, func() bool { return !conn.Healthy() }, 5*time.Second, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Error(t, conn.Reconnect(ctx), "a cancelled caller never dials")
}

func TestConn_ClosedForGood_ReconnectFails(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	conn := dial(t, url)
	require.NoError(t, conn.Close())
	assert.False(t, conn.Healthy())
	assert.ErrorIs(t, conn.Reconnect(context.Background()), amqp.ErrBrokerUnavailable, "closed for good")
}

func TestConfirmPublisher_ClosedConn_ErrBrokerUnavailable(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	conn := dial(t, url)
	pub := amqp.NewConfirmPublisher(conn, time.Second)
	require.NoError(t, conn.Close())

	assert.False(t, pub.IsOpen())
	assert.NotPanics(t, func() {
		_, err := pub.Send(context.Background(), amqp.ExchangeEvents, "session.completed", msg())
		assert.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
	})
	_, err := conn.Channel()
	assert.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
}
