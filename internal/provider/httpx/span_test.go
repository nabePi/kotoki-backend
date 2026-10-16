package httpx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func spanAttrs(t *testing.T, rec *testutil.TraceRecorder, name string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range rec.Find(name).Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func TestSpanAttributes(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := httpx.StartSpan(context.Background(), provider.CapLLM, provider.OpenAI, "model-x")
	sp.End(httpx.OutcomeError, &provider.Error{Kind: provider.KindRateLimited})
	attrs := spanAttrs(t, rec, "provider.LLM")
	require.Equal(t, map[string]string{
		"provider.name": "OPENAI", "provider.model": "model-x",
		"provider.outcome": "error", "provider.error_kind": "RATE_LIMITED",
	}, attrs)
	require.Equal(t, codes.Error, rec.Find("provider.LLM").Status.Code)
}

func TestSpanSuccessHasNoErrorKind(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := httpx.StartSpan(context.Background(), provider.CapTTS, provider.ElevenLabs, "m")
	sp.End(httpx.OutcomeSuccess, nil)
	attrs := spanAttrs(t, rec, "provider.TTS")
	require.Equal(t, "success", attrs["provider.outcome"])
	require.NotContains(t, attrs, "provider.error_kind")
	require.NotEqual(t, codes.Error, rec.Find("provider.TTS").Status.Code)
}

func TestSpanCancelledIsNotAFailure(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := httpx.StartSpan(context.Background(), provider.CapSTT, provider.Cartesia, "m")
	sp.End(httpx.OutcomeCancelled, context.Canceled)
	attrs := spanAttrs(t, rec, "provider.STT")
	require.Equal(t, "cancelled", attrs["provider.outcome"])
	require.Equal(t, "CANCELLED", attrs["provider.error_kind"])
	require.NotEqual(t, codes.Error, rec.Find("provider.STT").Status.Code)
}

func TestSpanErrorKindFromPlainError(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := httpx.StartSpan(context.Background(), provider.CapEvaluation, provider.DeepSeek, "m")
	sp.End(httpx.OutcomeError, errors.New("boom"))
	require.Equal(t, "UNAVAILABLE", spanAttrs(t, rec, "provider.EVALUATION")["provider.error_kind"])
}

func TestSpanEndIsIdempotentAndNilSafe(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	_, sp := httpx.StartSpan(context.Background(), provider.CapLLM, provider.OpenAI, "m")
	sp.End(httpx.OutcomeTruncated, nil)
	sp.End(httpx.OutcomeError, errors.New("late"))
	require.Equal(t, "truncated", spanAttrs(t, rec, "provider.LLM")["provider.outcome"])
	require.Len(t, rec.Spans(), 1)
	var nilSpan *httpx.Span
	require.NotPanics(t, func() { nilSpan.End(httpx.OutcomeSuccess, nil) })
}
