package amqp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const testQueue = "Q"

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// fakeAck records the broker verdict of one delivery.
type fakeAck struct {
	mu    sync.Mutex
	calls []string
}

func (a *fakeAck) rec(s string) error {
	a.mu.Lock()
	a.calls = append(a.calls, s)
	a.mu.Unlock()
	return nil
}
func (a *fakeAck) Ack(uint64, bool) error {
	return a.rec("ack")
}
func (a *fakeAck) Nack(_ uint64, _, requeue bool) error {
	return a.rec(fmt.Sprintf("nack requeue=%v", requeue))
}
func (a *fakeAck) Reject(uint64, bool) error { return a.rec("reject") }
func (a *fakeAck) verdicts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

type pubRecord struct {
	exchange, key string
	msg           amqp091.Publishing
}

type recPub struct {
	mu   sync.Mutex
	sent []pubRecord
	fail map[string]error // by exchange
}

func (p *recPub) PublishConfirmed(_ context.Context, exchange, key string, msg amqp091.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail[exchange]; err != nil {
		return err
	}
	p.sent = append(p.sent, pubRecord{exchange, key, msg})
	return nil
}

func (p *recPub) all() []pubRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pubRecord(nil), p.sent...)
}

type failureCall struct {
	cause error
	final bool
}

type fakeHandler struct {
	handle   func(ctx context.Context, d events.Delivery) error
	timeout  time.Duration
	supports map[string]int

	mu       sync.Mutex
	handled  []events.Delivery
	failures []failureCall
}

func (h *fakeHandler) Queue() string { return testQueue }
func (h *fakeHandler) SupportedEvents() map[string]int {
	if h.supports != nil {
		return h.supports
	}
	return map[string]int{events.SessionCompleted: 1}
}
func (h *fakeHandler) Timeout() time.Duration {
	if h.timeout > 0 {
		return h.timeout
	}
	return 5 * time.Second
}
func (h *fakeHandler) Handle(ctx context.Context, d events.Delivery) error {
	h.mu.Lock()
	h.handled = append(h.handled, d)
	h.mu.Unlock()
	if h.handle == nil {
		return nil
	}
	return h.handle(ctx, d)
}
func (h *fakeHandler) OnFailure(_ context.Context, _ events.Delivery, cause error, final bool) {
	h.mu.Lock()
	h.failures = append(h.failures, failureCall{cause, final})
	h.mu.Unlock()
}
func (h *fakeHandler) handledCount() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.handled) }
func (h *fakeHandler) failureCalls() []failureCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]failureCall(nil), h.failures...)
}

type fakeProcessed struct {
	done bool
	err  error
}

func (p fakeProcessed) IsProcessed(context.Context, string, uuid.UUID) (bool, error) {
	return p.done, p.err
}

type env struct {
	c    *amqp.Consumer
	h    *fakeHandler
	pub  *recPub
	m    *amqp.Metrics
	clk  *clock.Fake
	root context.Context
}

func newEnv(t *testing.T, h *fakeHandler, proc fakeProcessed) *env {
	t.Helper()
	pub := &recPub{fail: map[string]error{}}
	m := amqp.NewMetrics(prometheus.NewRegistry())
	clk := clock.NewFake(t0)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &env{c: amqp.NewConsumerForTest(h, pub, proc, m, log, clk), h: h, pub: pub, m: m, clk: clk, root: context.Background()}
}

func (e *env) result(result string) float64 {
	return promtest.ToFloat64(e.m.ConsumerMessages.WithLabelValues(testQueue, result))
}

func envelopeBody(t *testing.T, typ string, version int, traceparent string) []byte {
	t.Helper()
	b, err := json.Marshal(events.Envelope{
		EventID: uuid.MustParse("0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c"), EventType: typ, EventVersion: version,
		OccurredAt: t0, Producer: events.ProducerRealtime, AggregateType: events.AggregateSession,
		AggregateID: uuid.MustParse("0192f1a0-1111-7aaa-8bbb-cccccccccccc"),
		UserID:      uuid.MustParse("0192e000-2222-7ddd-8eee-ffffffffffff"),
		Traceparent: traceparent, Payload: json.RawMessage(`{"session_id":"x","n":1}`),
	})
	require.NoError(t, err)
	return b
}

func validBody(t *testing.T) []byte { return envelopeBody(t, events.SessionCompleted, 1, "") }

func delivery(body []byte, headers amqp091.Table) (amqp091.Delivery, *fakeAck) {
	ack := &fakeAck{}
	return amqp091.Delivery{
		Acknowledger: ack, DeliveryTag: 7, Body: body, Headers: headers, ContentType: "application/json",
		MessageId: "0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c", Type: events.SessionCompleted, AppId: "kotoki", Timestamp: t0,
	}, ack
}

func failWith(err error) func(context.Context, events.Delivery) error {
	return func(context.Context, events.Delivery) error { return err }
}

func TestProcess_Success_Ack(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Empty(t, e.pub.all())
	assert.Equal(t, 1, e.h.handledCount())
	assert.Empty(t, e.h.failureCalls())
	assert.Equal(t, 1.0, e.result("ack"))
	d := e.h.handled[0]
	assert.Equal(t, testQueue, d.Queue)
	assert.Equal(t, 0, d.RetryCount)
	assert.False(t, d.Redelivered)
	assert.Equal(t, events.SessionCompleted, d.Envelope.EventType)
	assert.Equal(t, 0.0, promtest.ToFloat64(e.m.ConsumerInflight.WithLabelValues(testQueue)), "inflight returns to 0")
}

func TestProcess_DeliveryCarriesRetryCountAndRedelivered(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, _ := delivery(validBody(t), amqp091.Table{"x-retry-count": int32(2)})
	msg.Redelivered = true

	e.c.Process(e.root, msg)

	require.Equal(t, 1, e.h.handledCount())
	assert.Equal(t, 2, e.h.handled[0].RetryCount)
	assert.True(t, e.h.handled[0].Redelivered)
}

func TestProcess_Duplicate_PrecheckAcksWithoutHandle(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{done: true})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Equal(t, 0, e.h.handledCount(), "an already processed event never reaches the handler")
	assert.Empty(t, e.pub.all())
	assert.Equal(t, 1.0, e.result("duplicate"))
	assert.Equal(t, 0.0, e.result("ack"))
}

func TestProcess_PrecheckError_StillHandles(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{err: errors.New("db down")})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, 1, e.h.handledCount(), "the pre-check is an optimisation: MarkProcessed is the guard")
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

func TestProcess_ErrAlreadyProcessed_Acks(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(fmt.Errorf("tx: %w", events.ErrAlreadyProcessed))}, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Empty(t, e.pub.all())
	assert.Empty(t, e.h.failureCalls())
	assert.Equal(t, 1.0, e.result("ack"))
}

func TestProcess_SkippedDeletedUser_AcksWithDedicatedMetric(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(events.ErrSkippedDeletedUser)}, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Empty(t, e.pub.all(), "no retry and no DLQ")
	assert.Empty(t, e.h.failureCalls(), "OnFailure is not called")
	assert.Equal(t, 1.0, e.result("skipped_deleted_user"))
	assert.Equal(t, 0.0, e.result("ack"))
}

func TestProcess_RetryLadder_HeadersAndRoutingKeys(t *testing.T) {
	cases := []struct {
		name    string
		headers amqp091.Table
		key     string
		count   int32
	}{
		{"first failure, header absent", nil, "Q.retry.5s", 1},
		{"second failure", amqp091.Table{"x-retry-count": int32(1)}, "Q.retry.30s", 2},
		{"third failure", amqp091.Table{"x-retry-count": int32(2)}, "Q.retry.2m", 3},
		{"fourth failure", amqp091.Table{"x-retry-count": int32(3)}, "Q.retry.10m", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
			body := validBody(t)
			before := cloneTable(c.headers)
			msg, ack := delivery(body, c.headers)

			e.c.Process(e.root, msg)

			sent := e.pub.all()
			require.Len(t, sent, 1)
			assert.Equal(t, "kotoki.retry", sent[0].exchange)
			assert.Equal(t, c.key, sent[0].key)
			h := sent[0].msg.Headers
			assert.Equal(t, c.count, h["x-retry-count"])
			assert.IsType(t, int32(0), h["x-retry-count"])
			assert.Equal(t, testQueue, h["x-original-queue"])
			assert.Equal(t, "boom", h["x-last-error"])
			assert.Equal(t, t0.Format(time.RFC3339Nano), h["x-first-failed-at"])
			assert.NotContains(t, h, "x-error-class", "retry is not a dead letter")
			assert.NotContains(t, h, "x-dead-at")
			assert.Equal(t, body, sent[0].msg.Body, "the body is never rewritten")
			assert.Equal(t, before, c.headers, "the delivery's own headers are not mutated")
			assert.Equal(t, []string{"ack"}, ack.verdicts(), "publish a copy, then ack the original")
			calls := e.h.failureCalls()
			require.Len(t, calls, 1)
			assert.False(t, calls[0].final)
			assert.Equal(t, 1.0, e.result("retry"))
			assert.Equal(t, 0.0, e.result("dead"))
		})
	}
}

func TestProcess_RetryLadder_FullClimbThenDLQ(t *testing.T) {
	// Feed each republished copy back in, as the broker does after the retry queue TTL.
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
	var keys []string
	var headers amqp091.Table
	for i := 0; i < 5; i++ {
		msg, _ := delivery(validBody(t), headers)
		e.c.Process(e.root, msg)
		sent := e.pub.all()
		last := sent[len(sent)-1]
		keys = append(keys, last.exchange+"/"+last.key)
		headers = last.msg.Headers
		e.clk.Advance(time.Minute)
	}
	assert.Equal(t, []string{"kotoki.retry/Q.retry.5s", "kotoki.retry/Q.retry.30s", "kotoki.retry/Q.retry.2m",
		"kotoki.retry/Q.retry.10m", "kotoki.dlx/Q"}, keys, "5 attempts: four retries, then the DLQ")
	assert.Equal(t, 5, e.h.handledCount())
	assert.Equal(t, t0.Format(time.RFC3339Nano), headers["x-first-failed-at"], "first failure time survives the whole climb")
}

func TestProcess_FirstFailedAt_NotOverwritten(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
	first := "2026-10-14T08:00:00Z"
	msg, _ := delivery(validBody(t), amqp091.Table{"x-retry-count": int32(1), "x-first-failed-at": first})

	e.c.Process(e.root, msg)

	require.Len(t, e.pub.all(), 1)
	assert.Equal(t, first, e.pub.all()[0].msg.Headers["x-first-failed-at"])
}

func TestProcess_LastError_TruncatedTo512(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New(strings.Repeat("e", 2000)))}, fakeProcessed{})
	msg, _ := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	require.Len(t, e.pub.all(), 1)
	assert.Len(t, e.pub.all()[0].msg.Headers["x-last-error"], 512)
}

func TestProcess_RetriesExhausted_GoesToDLQ(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
	body := validBody(t)
	msg, ack := delivery(body, amqp091.Table{"x-retry-count": int32(4), "x-first-failed-at": "2026-10-14T08:00:00Z"})

	e.c.Process(e.root, msg)

	sent := e.pub.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "kotoki.dlx", sent[0].exchange)
	assert.Equal(t, testQueue, sent[0].key)
	h := sent[0].msg.Headers
	assert.Equal(t, "RETRIES_EXHAUSTED", h["x-error-class"])
	assert.Equal(t, t0.Format(time.RFC3339Nano), h["x-dead-at"])
	assert.Equal(t, testQueue, h["x-original-queue"])
	assert.Equal(t, "boom", h["x-last-error"])
	assert.Equal(t, int32(4), h["x-retry-count"], "the DLQ copy keeps its retry count")
	assert.Equal(t, body, sent[0].msg.Body)
	assert.Equal(t, []string{"ack"}, ack.verdicts())
	calls := e.h.failureCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].final)
	assert.Equal(t, 1.0, e.result("dead"))
}

func TestProcess_Permanent_DirectToDLQ_NoRetry(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(events.Permanent("ENTITY_NOT_FOUND", errors.New("no session")))}, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil) // retry count 0: a retryable error would climb to 5s

	e.c.Process(e.root, msg)

	sent := e.pub.all()
	require.Len(t, sent, 1, "exactly one publish")
	assert.Equal(t, "kotoki.dlx", sent[0].exchange)
	assert.Equal(t, testQueue, sent[0].key)
	assert.Equal(t, "ENTITY_NOT_FOUND", sent[0].msg.Headers["x-error-class"])
	assert.NotContains(t, sent[0].msg.Headers, "x-retry-count", "never entered the ladder")
	assert.Equal(t, 1, e.h.handledCount(), "handled once, not retried")
	assert.Equal(t, []string{"ack"}, ack.verdicts())
	calls := e.h.failureCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].final)
	assert.Equal(t, 1.0, e.result("dead"))
	assert.Equal(t, 0.0, e.result("retry"))
}

func TestProcess_DecodeError_DLQ_DECODE_ERROR(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, ack := delivery([]byte(`{not json`), nil)

	e.c.Process(e.root, msg)

	sent := e.pub.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "kotoki.dlx", sent[0].exchange)
	assert.Equal(t, testQueue, sent[0].key)
	assert.Equal(t, "DECODE_ERROR", sent[0].msg.Headers["x-error-class"])
	assert.Equal(t, []byte(`{not json`), sent[0].msg.Body)
	assert.Equal(t, 0, e.h.handledCount())
	assert.Empty(t, e.h.failureCalls(), "no Delivery exists to hand to OnFailure")
	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Equal(t, 1.0, e.result("dead"))
}

func TestProcess_MissingEnvelopeField_DLQ(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, _ := delivery([]byte(`{"event_type":"session.completed","event_version":1,"payload":{"a":1}}`), nil)

	e.c.Process(e.root, msg)

	require.Len(t, e.pub.all(), 1)
	assert.Equal(t, "DECODE_ERROR", e.pub.all()[0].msg.Headers["x-error-class"])
}

func TestProcess_UnsupportedVersion_DLQ(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{}) // supports session.completed v1
	msg, ack := delivery(envelopeBody(t, events.SessionCompleted, 2, ""), nil)

	e.c.Process(e.root, msg)

	sent := e.pub.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "kotoki.dlx", sent[0].exchange)
	assert.Equal(t, "UNSUPPORTED_EVENT_VERSION", sent[0].msg.Headers["x-error-class"])
	assert.Equal(t, 0, e.h.handledCount())
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

func TestProcess_OlderVersionStillHandled(t *testing.T) {
	e := newEnv(t, &fakeHandler{supports: map[string]int{events.SessionCompleted: 3}}, fakeProcessed{})
	msg, ack := delivery(envelopeBody(t, events.SessionCompleted, 2, ""), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, 1, e.h.handledCount(), "SupportedEvents is a maximum version")
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

func TestProcess_UnknownEventType_DLQ(t *testing.T) {
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, ack := delivery(envelopeBody(t, "billing.invoice_paid", 1, ""), nil)

	e.c.Process(e.root, msg)

	sent := e.pub.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "kotoki.dlx", sent[0].exchange)
	assert.Equal(t, "DECODE_ERROR", sent[0].msg.Headers["x-error-class"])
	assert.Equal(t, 0, e.h.handledCount())
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

func TestProcess_HandlerPanic_Retries(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: func(context.Context, events.Delivery) error { panic("kaboom") }}, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil)

	require.NotPanics(t, func() { e.c.Process(e.root, msg) })

	sent := e.pub.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "kotoki.retry", sent[0].exchange)
	assert.Equal(t, "Q.retry.5s", sent[0].key)
	assert.Contains(t, sent[0].msg.Headers["x-last-error"], "kaboom")
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

func TestProcess_OnFailurePanic_DoesNotChangeRouting(t *testing.T) {
	h := &panickyFailureHandler{fakeHandler{handle: failWith(errors.New("boom"))}}
	pub := &recPub{fail: map[string]error{}}
	c := amqp.NewConsumerForTest(h, pub, fakeProcessed{}, amqp.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), clock.NewFake(t0))
	msg, ack := delivery(validBody(t), nil)

	require.NotPanics(t, func() { c.Process(context.Background(), msg) })

	require.Len(t, pub.all(), 1)
	assert.Equal(t, "Q.retry.5s", pub.all()[0].key)
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

type panickyFailureHandler struct{ fakeHandler }

func (h *panickyFailureHandler) OnFailure(context.Context, events.Delivery, error, bool) {
	panic("oops")
}

func TestProcess_HandlerTimeout_Retries(t *testing.T) {
	h := &fakeHandler{timeout: 10 * time.Millisecond, handle: func(ctx context.Context, _ events.Delivery) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	e := newEnv(t, h, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	sent := e.pub.all()
	require.Len(t, sent, 1)
	assert.Equal(t, "kotoki.retry", sent[0].exchange, "a timeout is retryable, not a shutdown requeue")
	assert.Contains(t, sent[0].msg.Headers["x-last-error"], "deadline exceeded")
	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Equal(t, 1.0, e.result("retry"))
}

func TestProcess_RetryPublishFails_NacksRequeue(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
	e.pub.fail["kotoki.retry"] = amqp.ErrConfirmTimeout
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"nack requeue=true"}, ack.verdicts(), "never ack a message whose retry copy was not confirmed")
	assert.Empty(t, e.pub.all())
	assert.Equal(t, 0.0, e.result("retry"))
}

func TestProcess_DeadPublishFails_NacksWithoutRequeue(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(events.Permanent("INVARIANT_VIOLATION", errors.New("x")))}, fakeProcessed{})
	e.pub.fail["kotoki.dlx"] = amqp.ErrNacked
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"nack requeue=false"}, ack.verdicts(), "queue-level DLX takes over")
	assert.Empty(t, e.pub.all())
}

func TestProcess_RetriesExhausted_DeadPublishFails_NacksWithoutRequeue(t *testing.T) {
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
	e.pub.fail["kotoki.dlx"] = amqp.ErrNacked
	msg, ack := delivery(validBody(t), amqp091.Table{"x-retry-count": int32(4)})

	e.c.Process(e.root, msg)

	assert.Equal(t, []string{"nack requeue=false"}, ack.verdicts())
}

func TestProcess_ShuttingDown_Requeue_NoOnFailure(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	h := &fakeHandler{handle: func(ctx context.Context, _ events.Delivery) error {
		cancelRoot() // grace expired while the handler runs
		<-ctx.Done()
		return ctx.Err()
	}}
	e := newEnv(t, h, fakeProcessed{})
	msg, ack := delivery(validBody(t), amqp091.Table{"x-retry-count": int32(1)})

	e.c.Process(root, msg)

	assert.Equal(t, []string{"nack requeue=true"}, ack.verdicts())
	assert.Empty(t, e.pub.all(), "a shutdown never climbs the ladder: no retry copy, no dead letter")
	assert.Empty(t, e.h.failureCalls(), "OnFailure is not called on shutdown")
	assert.Equal(t, 1.0, e.result("requeue"))
	assert.Equal(t, 0.0, e.result("retry"))
	assert.Equal(t, 0.0, e.result("dead"))
}

func TestProcess_ShuttingDown_PermanentStillDead(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	cancelRoot()
	e := newEnv(t, &fakeHandler{handle: failWith(events.Permanent("ENTITY_NOT_FOUND", errors.New("x")))}, fakeProcessed{})
	msg, ack := delivery(validBody(t), nil)

	e.c.Process(root, msg)

	require.Len(t, e.pub.all(), 1)
	assert.Equal(t, "kotoki.dlx", e.pub.all()[0].exchange)
	assert.Equal(t, []string{"ack"}, ack.verdicts())
}

func TestProcess_PropagatesTraceContext(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	pctx, parent := otel.GetTracerProvider().Tracer("test").Start(context.Background(), "publisher")
	headers := amqp091.Table{"x-retry-count": int32(2)}
	otel.GetTextMapPropagator().Inject(pctx, carrierOf(headers))
	parent.End()

	var handlerSC trace.SpanContext
	h := &fakeHandler{handle: func(ctx context.Context, _ events.Delivery) error {
		handlerSC = trace.SpanContextFromContext(ctx)
		return nil
	}}
	e := newEnv(t, h, fakeProcessed{})
	msg, _ := delivery(validBody(t), headers)

	e.c.Process(e.root, msg)

	span := rec.Find("Q process")
	assert.Equal(t, trace.SpanKindConsumer, span.SpanKind)
	assert.Equal(t, parent.SpanContext().TraceID(), span.SpanContext.TraceID())
	assert.Equal(t, parent.SpanContext().SpanID(), span.Parent.SpanID(), "child of the publisher span")
	assert.Equal(t, span.SpanContext.SpanID(), handlerSC.SpanID(), "the handler context carries the consumer span")
	assert.Equal(t, parent.SpanContext().TraceID(), handlerSC.TraceID())
	attrs := attrMap(span.Attributes)
	assert.Equal(t, "rabbitmq", attrs["messaging.system"])
	assert.Equal(t, "Q", attrs["messaging.destination.name"])
	assert.Equal(t, events.SessionCompleted, attrs["messaging.rabbitmq.destination.routing_key"])
	assert.Equal(t, "0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c", attrs["messaging.message.id"])
	assert.Equal(t, int64(2), attrs["kotoki.retry_count"])
	assert.Equal(t, "0192f1a0-1111-7aaa-8bbb-cccccccccccc", attrs["kotoki.session_id"])
	assert.Equal(t, "0192e000-2222-7ddd-8eee-ffffffffffff", attrs["kotoki.user_id"])
}

func TestProcess_TraceContext_FallsBackToEnvelopeField(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, _ := delivery(envelopeBody(t, events.SessionCompleted, 1, tp), nil) // no traceparent header

	e.c.Process(e.root, msg)

	span := rec.Find("Q process")
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.SpanContext.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", span.Parent.SpanID().String())
}

func TestProcess_HeaderTraceWinsOverEnvelopeField(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	const hdr = "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"
	const envTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	e := newEnv(t, &fakeHandler{}, fakeProcessed{})
	msg, _ := delivery(envelopeBody(t, events.SessionCompleted, 1, envTP), amqp091.Table{"traceparent": hdr})

	e.c.Process(e.root, msg)

	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", rec.Find("Q process").SpanContext.TraceID().String())
}

func TestProcess_RetryCopyCarriesConsumerSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	e := newEnv(t, &fakeHandler{handle: failWith(errors.New("boom"))}, fakeProcessed{})
	msg, _ := delivery(validBody(t), amqp091.Table{"traceparent": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"})

	e.c.Process(e.root, msg)

	span := rec.Find("Q process")
	tp, _ := e.pub.all()[0].msg.Headers["traceparent"].(string)
	assert.Contains(t, tp, span.SpanContext.TraceID().String(), "the next attempt continues the same trace")
	assert.Contains(t, tp, span.SpanContext.SpanID().String(), "as a child of this attempt")
}

func TestProcess_BodyNeverModified(t *testing.T) {
	bodies := map[string]struct {
		handle func(context.Context, events.Delivery) error
		hdr    amqp091.Table
	}{
		"retry":     {failWith(errors.New("boom")), nil},
		"dead":      {failWith(events.Permanent("INVARIANT_VIOLATION", errors.New("x"))), nil},
		"exhausted": {failWith(errors.New("boom")), amqp091.Table{"x-retry-count": int32(4)}},
	}
	for name, c := range bodies {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, &fakeHandler{handle: c.handle}, fakeProcessed{})
			body := validBody(t)
			orig := append([]byte(nil), body...)
			msg, _ := delivery(body, c.hdr)

			e.c.Process(e.root, msg)

			require.Len(t, e.pub.all(), 1)
			assert.Equal(t, orig, e.pub.all()[0].msg.Body)
			assert.Equal(t, orig, body)
			p := e.pub.all()[0].msg
			assert.Equal(t, "application/json", p.ContentType)
			assert.Equal(t, amqp091.Persistent, p.DeliveryMode)
			assert.Equal(t, msg.MessageId, p.MessageId)
			assert.Equal(t, msg.Type, p.Type)
			assert.Equal(t, "kotoki", p.AppId)
			assert.True(t, msg.Timestamp.Equal(p.Timestamp))
		})
	}
}

// Forced redelivery of an event that already took effect: the second copy must not repeat the
// effect. The handler here models the contract (MarkProcessed inside the effect tx).
func TestProcess_ForcedRedelivery_SingleEffect(t *testing.T) {
	var mu sync.Mutex
	marked := map[uuid.UUID]bool{}
	effects := 0
	h := &fakeHandler{handle: func(_ context.Context, d events.Delivery) error {
		mu.Lock()
		defer mu.Unlock()
		if marked[d.Envelope.EventID] {
			return events.ErrAlreadyProcessed
		}
		marked[d.Envelope.EventID] = true
		effects++
		return nil
	}}
	// Without the pre-check (concurrent copies race past it) MarkProcessed is the guard.
	e := newEnv(t, h, fakeProcessed{})
	body := validBody(t)
	var acks []*fakeAck
	for i := 0; i < 3; i++ {
		msg, ack := delivery(body, nil)
		msg.Redelivered = i > 0
		acks = append(acks, ack)
		e.c.Process(e.root, msg)
	}
	assert.Equal(t, 1, effects, "three deliveries, one effect")
	for _, a := range acks {
		assert.Equal(t, []string{"ack"}, a.verdicts())
	}
	assert.Empty(t, e.pub.all())
	assert.Equal(t, 3.0, e.result("ack"))

	// With the pre-check seeing the committed row, the handler is not even called.
	e2 := newEnv(t, h, fakeProcessed{done: true})
	msg, ack := delivery(body, nil)
	e2.c.Process(e2.root, msg)
	assert.Equal(t, 1, effects)
	assert.Equal(t, []string{"ack"}, ack.verdicts())
	assert.Equal(t, 1.0, e2.result("duplicate"))
}

func TestProcess_InflightGaugeCoversHandler(t *testing.T) {
	var e *env
	var during float64
	h := &fakeHandler{handle: func(context.Context, events.Delivery) error {
		during = promtest.ToFloat64(e.m.ConsumerInflight.WithLabelValues(testQueue))
		return nil
	}}
	e = newEnv(t, h, fakeProcessed{})
	msg, _ := delivery(validBody(t), nil)

	e.c.Process(e.root, msg)

	assert.Equal(t, 1.0, during)
	assert.Equal(t, 0.0, promtest.ToFloat64(e.m.ConsumerInflight.WithLabelValues(testQueue)))
	assert.Equal(t, 1, promtest.CollectAndCount(e.m.ConsumerProcessing), "one observation series per queue")
}

func cloneTable(t amqp091.Table) amqp091.Table {
	if t == nil {
		return nil
	}
	out := amqp091.Table{}
	for k, v := range t {
		out[k] = v
	}
	return out
}

// carrierOf adapts a header table for propagation.Inject in tests (string values only).
type carrierOf amqp091.Table

func (c carrierOf) Get(k string) string { s, _ := c[k].(string); return s }
func (c carrierOf) Set(k, v string)     { c[k] = v }
func (c carrierOf) Keys() []string {
	var ks []string
	for k := range c {
		ks = append(ks, k)
	}
	return ks
}

func attrMap(kvs []attribute.KeyValue) map[string]any {
	m := map[string]any{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.AsInterface()
	}
	return m
}
