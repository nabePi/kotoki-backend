package provider_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func TestErrorTaxonomyTable(t *testing.T) {
	cases := []struct {
		kind          provider.ErrorKind
		retry, fb, br bool
	}{
		{provider.KindTimeout, true, true, true}, {provider.KindRateLimited, true, true, true},
		{provider.KindAuth, false, true, true}, {provider.KindInvalidRequest, false, true, false},
		{provider.KindUnavailable, true, true, true}, {provider.KindContentFiltered, false, false, false},
		{provider.KindInvalidResponse, true, true, true}, {provider.KindCancelled, false, false, false},
	}
	for _, c := range cases {
		e := &provider.Error{Kind: c.kind, Retryable: c.retry}
		require.Equal(t, c.fb, provider.FallbackEligible(e), c.kind)
		require.Equal(t, c.br, provider.CountsForBreaker(e), c.kind)
		require.Equal(t, c.retry, provider.IsRetryable(e), c.kind)
	}
}

func TestInvalidRequest404CountsForBreaker(t *testing.T) {
	require.True(t, provider.CountsForBreaker(&provider.Error{Kind: provider.KindInvalidRequest, HTTPStatus: 404}))
	require.False(t, provider.CountsForBreaker(&provider.Error{Kind: provider.KindInvalidRequest, HTTPStatus: 400}))
}

func TestRateLimitedLocalConcurrencyNotCounted(t *testing.T) {
	require.False(t, provider.CountsForBreaker(&provider.Error{Kind: provider.KindRateLimited, VendorCode: "local_concurrency"}))
	require.True(t, provider.CountsForBreaker(&provider.Error{Kind: provider.KindRateLimited, VendorCode: "rate_limit_exceeded"}))
}

func TestRateLimitedQuotaNotRetryable(t *testing.T) {
	require.False(t, provider.IsRetryable(&provider.Error{Kind: provider.KindRateLimited, VendorCode: "insufficient_quota", Retryable: false}))
}

func TestKindOf(t *testing.T) {
	require.Equal(t, provider.KindCancelled, provider.KindOf(context.Canceled))
	require.Equal(t, provider.KindTimeout, provider.KindOf(context.DeadlineExceeded))
	require.Equal(t, provider.KindUnavailable, provider.KindOf(errors.New("boom")))
	wrapped := fmt.Errorf("ctx: %w", &provider.Error{Kind: provider.KindAuth})
	require.Equal(t, provider.KindAuth, provider.KindOf(wrapped))
	require.True(t, provider.IsRetryable(context.DeadlineExceeded), "a bare timeout is retryable")
	require.False(t, provider.IsRetryable(context.Canceled))
	require.False(t, provider.IsRetryable(nil))
}

func TestClassifyHTTPStatus(t *testing.T) {
	want := map[int]provider.ErrorKind{
		408: provider.KindTimeout, 504: provider.KindTimeout, 429: provider.KindRateLimited,
		401: provider.KindAuth, 403: provider.KindAuth,
		400: provider.KindInvalidRequest, 404: provider.KindInvalidRequest, 409: provider.KindInvalidRequest,
		413: provider.KindInvalidRequest, 422: provider.KindInvalidRequest,
		500: provider.KindUnavailable, 502: provider.KindUnavailable, 503: provider.KindUnavailable, 529: provider.KindUnavailable,
	}
	for status, kind := range want {
		require.Equal(t, kind, provider.ClassifyHTTPStatus(status), status)
	}
}

func TestErrorString(t *testing.T) {
	e := &provider.Error{Provider: provider.OpenAI, Capability: provider.CapLLM, Kind: provider.KindRateLimited,
		HTTPStatus: 429, VendorCode: "rate_limit_exceeded", Message: "msg"}
	require.Equal(t, "provider OPENAI/LLM: RATE_LIMITED (429 rate_limit_exceeded): msg", e.Error())
}

func TestErrorMessageTruncated512(t *testing.T) {
	e := provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindUnavailable, strings.Repeat("x", 2000))
	require.Len(t, e.Message, 512)
	cut := provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindUnavailable, strings.Repeat("あ", 300))
	require.LessOrEqual(t, len(cut.Message), 512)
	require.True(t, strings.ToValidUTF8(cut.Message, "") == cut.Message, "truncation must not split a rune")
}

func TestUnwrap(t *testing.T) {
	cause := errors.New("root")
	require.ErrorIs(t, &provider.Error{Cause: cause}, cause)
}

func TestCircuitOpenError(t *testing.T) {
	e := provider.NewCircuitOpen(provider.CapSTT)
	require.Equal(t, provider.KindUnavailable, e.Kind)
	require.Equal(t, "circuit_open", e.VendorCode)
	require.True(t, e.Retryable)
	require.Equal(t, provider.CapSTT, e.Capability)
}
