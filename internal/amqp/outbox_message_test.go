package amqp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func tracerForTest() trace.Tracer { return otel.GetTracerProvider().Tracer("test") }

const storedTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func outboxRow() amqp.OutboxRow {
	tp := storedTraceparent
	return amqp.OutboxRow{
		EventID:       uuid.MustParse("0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c"),
		EventType:     events.SessionCompleted,
		EventVersion:  1,
		AggregateType: events.AggregateSession,
		AggregateID:   uuid.MustParse("0192f1a0-1111-7aaa-8bbb-cccccccccccc"),
		UserID:        uuid.MustParse("0192e000-2222-7ddd-8eee-ffffffffffff"),
		Producer:      events.ProducerRealtime,
		Payload:       json.RawMessage(`{"session_id":"0192f1a0-1111-7aaa-8bbb-cccccccccccc","n":1}`),
		Traceparent:   &tp,
		OccurredAt:    time.Date(2026, 10, 12, 6, 30, 15, 123_000_000, time.UTC),
	}
}

func TestBuildMessage_Properties(t *testing.T) {
	testutil.NewTraceRecorder(t)
	row := outboxRow()

	msg, end, err := amqp.BuildMessage(context.Background(), row)
	require.NoError(t, err)
	defer end()

	assert.Equal(t, "application/json", msg.ContentType)
	assert.Equal(t, amqp091.Persistent, msg.DeliveryMode)
	assert.Equal(t, row.EventID.String(), msg.MessageId)
	assert.Equal(t, events.SessionCompleted, msg.Type)
	assert.True(t, row.OccurredAt.Equal(msg.Timestamp))
	assert.Equal(t, "kotoki", msg.AppId)
	assert.Equal(t, int32(1), msg.Headers["x-event-version"])
	assert.IsType(t, int32(0), msg.Headers["x-event-version"])
}

func TestBuildMessage_VersionHeaderFollowsRow(t *testing.T) {
	testutil.NewTraceRecorder(t)
	row := outboxRow()
	row.EventVersion = 3
	msg, end, err := amqp.BuildMessage(context.Background(), row)
	require.NoError(t, err)
	defer end()
	assert.Equal(t, int32(3), msg.Headers["x-event-version"])
	env, err := events.DecodeEnvelope(msg.Body)
	require.NoError(t, err)
	assert.Equal(t, 3, env.EventVersion)
}

func TestBuildMessage_BodyIsEnvelope(t *testing.T) {
	testutil.NewTraceRecorder(t)
	row := outboxRow()

	msg, end, err := amqp.BuildMessage(context.Background(), row)
	require.NoError(t, err)
	defer end()

	env, err := events.DecodeEnvelope(msg.Body)
	require.NoError(t, err)
	assert.Equal(t, row.EventID, env.EventID)
	assert.Equal(t, row.EventType, env.EventType)
	assert.Equal(t, row.EventVersion, env.EventVersion)
	assert.Equal(t, row.Producer, env.Producer)
	assert.Equal(t, row.AggregateType, env.AggregateType)
	assert.Equal(t, row.AggregateID, env.AggregateID)
	assert.Equal(t, row.UserID, env.UserID)
	assert.True(t, row.OccurredAt.Equal(env.OccurredAt))
	assert.Equal(t, string(row.Payload), string(env.Payload), "payload bytes pass through untouched")
}

func TestBuildMessage_ParentFromStoredTraceparent(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	row := outboxRow()
	ts := "vendor=abc"
	row.Tracestate = &ts

	msg, end, err := amqp.BuildMessage(context.Background(), row)
	require.NoError(t, err)
	end()

	span := rec.Find("kotoki.events publish")
	assert.Equal(t, trace.SpanKindProducer, span.SpanKind)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.SpanContext.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", span.Parent.SpanID().String())
	assert.NotEqual(t, span.Parent.SpanID(), span.SpanContext.SpanID())

	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-" + span.SpanContext.SpanID().String() + "-01"
	assert.Equal(t, want, msg.Headers["traceparent"], "header carries the publish span, not the stored parent")
	env, err := events.DecodeEnvelope(msg.Body)
	require.NoError(t, err)
	assert.Equal(t, want, env.Traceparent)
	assert.Equal(t, "vendor=abc", msg.Headers["tracestate"])
	require.NotNil(t, env.Tracestate)
	assert.Equal(t, "vendor=abc", *env.Tracestate)
}

func TestBuildMessage_NullTraceparent_NewRootAndNoTracestateHeader(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	row := outboxRow()
	row.Traceparent, row.Tracestate = nil, nil

	msg, end, err := amqp.BuildMessage(context.Background(), row)
	require.NoError(t, err)
	end()

	span := rec.Find("kotoki.events publish")
	assert.False(t, span.Parent.IsValid(), "no stored context: new root span")
	assert.NotEqual(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.SpanContext.TraceID().String())
	want := "00-" + span.SpanContext.TraceID().String() + "-" + span.SpanContext.SpanID().String() + "-01"
	assert.Equal(t, want, msg.Headers["traceparent"])
	assert.NotContains(t, msg.Headers, "tracestate")
	env, err := events.DecodeEnvelope(msg.Body)
	require.NoError(t, err)
	assert.Equal(t, want, env.Traceparent)
	assert.Nil(t, env.Tracestate)
}

func TestBuildMessage_InvalidStoredTraceparent_NewRoot(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	row := outboxRow()
	bad := "garbage"
	row.Traceparent = &bad

	_, end, err := amqp.BuildMessage(context.Background(), row)
	require.NoError(t, err)
	end()
	assert.False(t, rec.Find("kotoki.events publish").Parent.IsValid())
}

func TestBuildMessage_ParentIsNotTheCallerContextSpan(t *testing.T) {
	// The stored context, not whatever span the publisher loop happens to run in, is the parent.
	rec := testutil.NewTraceRecorder(t)
	ctx, loop := tracerForTest().Start(context.Background(), "loop")
	defer loop.End()

	_, end, err := amqp.BuildMessage(ctx, outboxRow())
	require.NoError(t, err)
	end()
	assert.Equal(t, "00f067aa0ba902b7", rec.Find("kotoki.events publish").Parent.SpanID().String())
}

func TestBuildMessage_NullTraceparent_IgnoresCallerSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	ctx, loop := tracerForTest().Start(context.Background(), "loop")
	defer loop.End()
	row := outboxRow()
	row.Traceparent = nil

	_, end, err := amqp.BuildMessage(ctx, row)
	require.NoError(t, err)
	end()
	span := rec.Find("kotoki.events publish")
	assert.False(t, span.Parent.IsValid(), "spec 9.7.5: NULL columns mean a new root, not the loop's span")
	assert.NotEqual(t, loop.SpanContext().TraceID(), span.SpanContext.TraceID())
}
