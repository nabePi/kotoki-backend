package amqp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/events"
)

const (
	appID           = "kotoki"
	lastErrorText   = "broker nack or confirm timeout"
	publishSpanName = "kotoki.events publish"
	tracerName      = "github.com/nabePi/kotoki-backend/internal/amqp"
)

// OutboxRow is one event_outbox row as the publisher reads it (spec 9.3.2).
type OutboxRow struct {
	EventID       uuid.UUID
	EventType     string
	EventVersion  int
	AggregateType string
	AggregateID   uuid.UUID
	UserID        uuid.UUID
	Producer      string
	Payload       json.RawMessage
	Traceparent   *string
	Tracestate    *string
	OccurredAt    time.Time
	Attempts      int
}

// BuildMessage turns an outbox row into the AMQP message of spec 9.3.3. It starts the PRODUCER span
// "kotoki.events publish" whose parent is the trace context stored with the row (no stored context:
// a new root span) and injects that span into the headers and the envelope. end finishes the span;
// call it once the confirm verdict is known.
func BuildMessage(ctx context.Context, row OutboxRow) (amqp091.Publishing, func(), error) {
	// The stored context is the parent, never a span that happens to be active in ctx.
	carrier := propagation.MapCarrier{}
	if row.Traceparent != nil {
		carrier.Set("traceparent", *row.Traceparent)
	}
	if row.Tracestate != nil {
		carrier.Set("tracestate", *row.Tracestate)
	}
	parent := propagation.TraceContext{}.Extract(trace.ContextWithSpanContext(ctx, trace.SpanContext{}), carrier)
	spanCtx, span := otel.GetTracerProvider().Tracer(tracerName).Start(parent, publishSpanName,
		trace.WithSpanKind(trace.SpanKindProducer))
	end := func() { span.End() }

	headers := amqp091.Table{"x-event-version": int32(row.EventVersion)} //nolint:gosec // small constant version
	propagation.TraceContext{}.Inject(spanCtx, amqpHeaderCarrier(headers))
	traceparent, _ := headers["traceparent"].(string)
	var tracestate *string
	if v, ok := headers["tracestate"].(string); ok && v != "" {
		tracestate = &v
	}

	env := events.Envelope{
		EventID: row.EventID, EventType: row.EventType, EventVersion: row.EventVersion,
		OccurredAt: row.OccurredAt.UTC().Truncate(time.Millisecond), Producer: row.Producer,
		AggregateType: row.AggregateType, AggregateID: row.AggregateID, UserID: row.UserID,
		Traceparent: traceparent, Tracestate: tracestate, Payload: row.Payload,
	}
	body, err := json.Marshal(env)
	if err != nil {
		end()
		return amqp091.Publishing{}, nil, fmt.Errorf("amqp: encode envelope %s: %w", row.EventID, err)
	}
	return amqp091.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp091.Persistent,
		MessageId:    row.EventID.String(),
		Type:         row.EventType,
		Timestamp:    row.OccurredAt,
		AppId:        appID,
		Headers:      headers,
		Body:         body,
	}, end, nil
}

// BatchPublisher is what the outbox publisher needs from the broker side; *ConfirmPublisher
// satisfies it.
type BatchPublisher interface {
	IsOpen() bool
	Send(ctx context.Context, exchange, key string, msg amqp091.Publishing) (Confirmation, error)
}

// PublisherConfig is config.Worker.Outbox.
type PublisherConfig struct {
	PollInterval, ConfirmTimeout, TxTimeout time.Duration
	BatchSize, MaxAttempts                  int
}

// OutboxPublisher moves event_outbox rows to RabbitMQ (spec 9.3.2). It may run in several replicas:
// FOR UPDATE SKIP LOCKED keeps their batches disjoint.
type OutboxPublisher struct {
	// Sleep waits for d or until ctx ends; WaitReconnect blocks until the broker may be back. Both
	// are seams for tests and wiring (wiring points WaitReconnect at Conn.Reconnect); nil WaitReconnect
	// sleeps with Backoff.
	Sleep         func(ctx context.Context, d time.Duration)
	WaitReconnect func(ctx context.Context)

	pool *pgxpool.Pool
	pub  BatchPublisher
	cfg  PublisherConfig
	m    *Metrics
	log  *slog.Logger
}

// NewOutboxPublisher returns a publisher; m may be shared with the consumer side.
func NewOutboxPublisher(pool *pgxpool.Pool, pub BatchPublisher, cfg PublisherConfig, m *Metrics, log *slog.Logger) *OutboxPublisher {
	return &OutboxPublisher{Sleep: sleepCtx, pool: pool, pub: pub, cfg: cfg, m: m, log: log}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

const selectReadySQL = `
	SELECT event_id, event_type, event_version, aggregate_type, aggregate_id, user_id,
	       producer, payload, traceparent, tracestate, occurred_at, attempts
	FROM event_outbox
	WHERE status = 'PENDING' AND available_at <= now()
	ORDER BY available_at, event_id
	LIMIT $1
	FOR UPDATE SKIP LOCKED`

const markPublishedSQL = `
	UPDATE event_outbox SET status = 'PUBLISHED', published_at = now()
	WHERE event_id = ANY($1)`

const markFailedSQL = `
	UPDATE event_outbox
	SET attempts     = attempts + 1,
	    last_error   = $2,
	    available_at = now() + make_interval(secs => LEAST(power(2, attempts + 1), 300)),
	    status       = CASE WHEN attempts + 1 >= $3 THEN 'FAILED' ELSE 'PENDING' END
	WHERE event_id = ANY($1)`

type failedRow struct {
	id        uuid.UUID
	eventType string
	reason    string
}

// PublishBatch publishes one batch inside a single transaction and returns the number of rows it
// claimed. Row locks are held until commit, so any failure before the commit (broker unavailable,
// channel lost, publish error) rolls the whole batch back and the next poll publishes it again;
// consumers are idempotent, so the duplicates are harmless. attempts rises only for rows the broker
// nacked or did not confirm in time, never because the broker was unreachable.
//
// The batch is bounded by the tx timeout but not by cancellation of ctx: a batch in flight when the
// worker shuts down finishes first (spec 9.7.7 step 6).
func (p *OutboxPublisher) PublishBatch(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.TxTimeout)
	defer cancel()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("amqp: outbox begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := p.selectReady(ctx, tx)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if !p.pub.IsOpen() {
		return 0, ErrBrokerUnavailable
	}

	var ends []func()
	defer func() {
		for _, end := range ends {
			end()
		}
	}()
	confirms := make([]Confirmation, len(rows))
	for i, r := range rows {
		msg, end, err := BuildMessage(ctx, r)
		if err != nil {
			return 0, err
		}
		ends = append(ends, end)
		c, err := p.pub.Send(ctx, ExchangeEvents, r.EventType, msg)
		if err != nil {
			return 0, err
		}
		confirms[i] = c
	}

	var ok []uuid.UUID
	var failed []failedRow
	var okTypes []string
	for i, c := range confirms {
		acked, reason, err := p.awaitConfirm(ctx, c)
		if err != nil {
			return 0, err
		}
		r := rows[i]
		if acked {
			ok = append(ok, r.EventID)
			okTypes = append(okTypes, r.EventType)
			continue
		}
		failed = append(failed, failedRow{id: r.EventID, eventType: r.EventType, reason: reason})
	}
	for _, end := range ends {
		end()
	}

	if len(ok) > 0 {
		if _, err := tx.Exec(ctx, markPublishedSQL, ok); err != nil {
			return 0, fmt.Errorf("amqp: outbox mark published: %w", err)
		}
	}
	if len(failed) > 0 {
		ids := make([]uuid.UUID, len(failed))
		for i, f := range failed {
			ids[i] = f.id
		}
		if _, err := tx.Exec(ctx, markFailedSQL, ids, lastErrorText, p.cfg.MaxAttempts); err != nil {
			return 0, fmt.Errorf("amqp: outbox mark failed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("amqp: outbox commit: %w", err)
	}

	// Counted only once the verdicts are durable.
	for _, t := range okTypes {
		p.m.OutboxPublished.WithLabelValues(t).Inc()
	}
	for _, f := range failed {
		p.m.OutboxFailures.WithLabelValues(f.eventType, f.reason).Inc()
		p.log.Warn("outbox publish not confirmed", "event_id", f.id, "event_type", f.eventType, "reason", f.reason)
	}
	return len(rows), nil
}

func (p *OutboxPublisher) selectReady(ctx context.Context, tx pgx.Tx) ([]OutboxRow, error) {
	rs, err := tx.Query(ctx, selectReadySQL, p.cfg.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("amqp: outbox select: %w", err)
	}
	defer rs.Close()
	var out []OutboxRow
	for rs.Next() {
		var r OutboxRow
		var payload []byte
		var version, attempts int32
		if err := rs.Scan(&r.EventID, &r.EventType, &version, &r.AggregateType, &r.AggregateID, &r.UserID,
			&r.Producer, &payload, &r.Traceparent, &r.Tracestate, &r.OccurredAt, &attempts); err != nil {
			return nil, fmt.Errorf("amqp: outbox scan: %w", err)
		}
		r.EventVersion, r.Attempts, r.Payload = int(version), int(attempts), payload
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("amqp: outbox select: %w", err)
	}
	return out, nil
}

// awaitConfirm returns the broker verdict. A nack or a missing verdict within ConfirmTimeout is a
// publish failure (acked=false, reason nack|timeout); a channel that died, or a cancelled/expired
// transaction, is an error that rolls the batch back instead.
func (p *OutboxPublisher) awaitConfirm(ctx context.Context, c Confirmation) (acked bool, reason string, err error) {
	wctx, cancel := context.WithTimeout(ctx, p.cfg.ConfirmTimeout)
	defer cancel()
	acked, err = c.Wait(wctx)
	switch {
	case err == nil && acked:
		return true, "", nil
	case err == nil:
		return false, "nack", nil
	case errors.Is(err, ErrConfirmTimeout):
		return false, "timeout", nil
	case ctx.Err() == nil && wctx.Err() != nil:
		return false, "timeout", nil // our own per-confirmation deadline, not the transaction's
	default:
		return false, "", err
	}
}

// Run publishes until ctx is cancelled and then returns nil. A full batch is followed immediately
// by the next one; otherwise it sleeps PollInterval. While the broker is unavailable it waits for a
// reconnect without touching any row.
func (p *OutboxPublisher) Run(ctx context.Context) error {
	unavailable := 0
	for ctx.Err() == nil {
		n, err := p.PublishBatch(ctx)
		switch {
		case errors.Is(err, ErrBrokerUnavailable):
			p.log.Warn("outbox publisher: broker unavailable", "consecutive", unavailable+1)
			p.waitReconnect(ctx, unavailable)
			unavailable++
			// WaitReconnect returns at once while the connection is healthy. If the publisher still
			// reports the broker unavailable after a few such rounds, back off instead of spinning
			// (a spin once wrote 2.6 million log lines in 14 minutes, see M10-T23).
			if p.WaitReconnect != nil && unavailable >= reconnectSpinGuard {
				p.Sleep(ctx, Backoff(unavailable-reconnectSpinGuard, rand.Float64))
			}
			continue
		case err != nil:
			p.log.Error("outbox publish batch failed", "err", err)
			p.Sleep(ctx, p.cfg.PollInterval)
		case n >= p.cfg.BatchSize:
			// backlog: take the next batch at once
		default:
			p.Sleep(ctx, p.cfg.PollInterval)
		}
		unavailable = 0
	}
	return nil
}

// reconnectSpinGuard is the number of consecutive unavailable rounds after which Run adds a
// backoff on top of WaitReconnect.
const reconnectSpinGuard = 3

func (p *OutboxPublisher) waitReconnect(ctx context.Context, attempt int) {
	if p.WaitReconnect != nil {
		p.WaitReconnect(ctx)
		return
	}
	p.Sleep(ctx, Backoff(attempt, rand.Float64))
}
