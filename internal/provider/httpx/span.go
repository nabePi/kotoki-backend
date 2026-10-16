package httpx

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Span outcomes (attribute provider.outcome).
const (
	OutcomeSuccess   = "success"
	OutcomeError     = "error"
	OutcomeCancelled = "cancelled"
	OutcomeTruncated = "truncated"
)

// Span is the OTel span of one adapter call. The router (M4) has no spans; each vendor adapter
// opens one with StartSpan and closes it exactly once with End.
type Span struct {
	span trace.Span
	once sync.Once
}

// StartSpan opens span "provider.<capability>" with provider.name and provider.model. Only
// vendor/model identifiers go in; user text, prompts and audio never do (§8.4.0).
func StartSpan(ctx context.Context, c provider.Capability, n provider.Name, model string) (context.Context, *Span) {
	ctx, sp := otel.Start(ctx, "provider."+string(c),
		attribute.String("provider.name", string(n)),
		attribute.String("provider.model", model),
	)
	return ctx, &Span{span: sp}
}

// End finishes the span with provider.outcome and, when err is non-nil, provider.error_kind =
// provider.KindOf(err). Only outcome "error" marks the span as failed; a cancelled or
// truncated stream is not a failure. Later calls are no-ops.
func (s *Span) End(outcome string, err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.span.SetAttributes(attribute.String("provider.outcome", outcome)) // fixed vocabulary, nothing to redact
		if err != nil {
			s.span.SetAttributes(attribute.String("provider.error_kind", string(provider.KindOf(err))))
		}
		var failed error
		if outcome == OutcomeError {
			failed = err
		}
		otel.End(s.span, &failed)
	})
}
