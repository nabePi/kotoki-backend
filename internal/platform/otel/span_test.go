package otel

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func attrMap(s tracetest.SpanStub) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func TestEndInternalError(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := Start(context.Background(), "balance.Settle")
	err := errs.Wrap(errors.New("pgx boom"), errs.CodeInternalError, "x")
	End(sp, &err)

	s := rec.Find("balance.Settle")
	assert.Equal(t, codes.Error, s.Status.Code)
	assert.Equal(t, "INTERNAL_ERROR", s.Status.Description)
	var stack string
	for _, e := range s.Events {
		if e.Name == "exception" {
			for _, kv := range e.Attributes {
				if kv.Key == "exception.stacktrace" {
					stack = kv.Value.AsString()
				}
			}
		}
	}
	require.NotEmpty(t, stack)
	m := attrMap(s)
	assert.Equal(t, "internal/platform/otel/span_test.go", m["code.file.path"].AsString())
	assert.Greater(t, m["code.line.number"].AsInt64(), int64(0))
	assert.Contains(t, m["code.function.name"].AsString(), "TestEndInternalError")
	assert.Equal(t, "INTERNAL_ERROR", m["kotoki.error_code"].AsString())
}

func TestEndBusinessError(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := Start(context.Background(), "b")
	err := errs.New(errs.CodeBalanceInsufficient, "low")
	End(sp, &err)

	s := rec.Find("b")
	assert.NotEqual(t, codes.Error, s.Status.Code)
	assert.Empty(t, s.Events)
	m := attrMap(s)
	assert.Equal(t, "BALANCE_INSUFFICIENT", m["kotoki.error_code"].AsString())
	assert.Equal(t, "internal/platform/otel/span_test.go", m["code.file.path"].AsString())
	assert.Contains(t, m, "code.line.number")
	assert.Contains(t, m, "code.function.name")
}

func TestEndPlainErrorIsInternal(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := Start(context.Background(), "p")
	err := errors.New("plain")
	End(sp, &err)

	s := rec.Find("p")
	assert.Equal(t, codes.Error, s.Status.Code)
	assert.NotContains(t, attrMap(s), "code.file.path")
}

func TestEndNilError(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := Start(context.Background(), "n")
	var err error
	End(sp, &err)

	s := rec.Find("n")
	assert.NotEqual(t, codes.Error, s.Status.Code)
	assert.NotContains(t, attrMap(s), "kotoki.error_code")
}

func TestEndNilErrp(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := Start(context.Background(), "z")
	End(sp, nil)
	assert.NotEqual(t, codes.Error, rec.Find("z").Status.Code)
}

func TestParentChild(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	ctx, parent := Start(context.Background(), "parent")
	_, child := Start(ctx, "child")
	child.End()
	parent.End()

	p, c := rec.Find("parent"), rec.Find("child")
	assert.Equal(t, p.SpanContext.SpanID(), c.Parent.SpanID())
	assert.Equal(t, p.SpanContext.TraceID(), c.SpanContext.TraceID())
}

func TestAttrRedaction(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := Start(context.Background(), "x",
		attribute.String("text", "halo"),
		attribute.String("user.id", "u1"),
		attribute.Int("password", 5))
	sp.End()

	m := attrMap(rec.Find("x"))
	assert.Equal(t, "[REDACTED]", m["text"].AsString())
	assert.Equal(t, "u1", m["user.id"].AsString())
	assert.Equal(t, "[REDACTED]", m["password"].AsString())
}
