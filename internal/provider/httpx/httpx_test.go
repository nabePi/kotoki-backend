package httpx_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

func resp(status int, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h}
}

func TestClassifyHTTPRetryAfter(t *testing.T) {
	r := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"7"}}}
	e := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, r, []byte(`{"error":{"code":"rate_limit_exceeded"}}`))
	require.Equal(t, provider.KindRateLimited, e.Kind)
	require.Equal(t, 7*time.Second, e.RetryAfter)
	require.True(t, e.Retryable)
	require.Equal(t, "rate_limit_exceeded", e.VendorCode)
	require.Equal(t, 429, e.HTTPStatus)
	require.Equal(t, provider.OpenAI, e.Provider)
}

func TestClassifyHTTPRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{"Date": {"Mon, 02 Jan 2006 15:04:05 GMT"}, "Retry-After": {"Mon, 02 Jan 2006 15:04:35 GMT"}}
	e := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(429, h), nil)
	require.Equal(t, 30*time.Second, e.RetryAfter, "relative to the response's own Date header")

	noDate := http.Header{"Retry-After": {"Mon, 02 Jan 2006 15:04:35 GMT"}}
	require.Zero(t, httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(429, noDate), nil).RetryAfter)
	bad := http.Header{"Retry-After": {"soon"}}
	require.Zero(t, httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(429, bad), nil).RetryAfter)
}

func TestClassifyHTTPTable(t *testing.T) {
	cases := []struct {
		status int
		kind   provider.ErrorKind
		retry  bool
	}{
		{408, provider.KindTimeout, true}, {504, provider.KindTimeout, true},
		{429, provider.KindRateLimited, true},
		{401, provider.KindAuth, false}, {403, provider.KindAuth, false},
		{400, provider.KindInvalidRequest, false}, {404, provider.KindInvalidRequest, false}, {409, provider.KindInvalidRequest, false},
		{413, provider.KindInvalidRequest, false}, {422, provider.KindInvalidRequest, false},
		{500, provider.KindUnavailable, true}, {502, provider.KindUnavailable, true}, {503, provider.KindUnavailable, true}, {529, provider.KindUnavailable, true},
	}
	for _, c := range cases {
		e := httpx.ClassifyHTTP(provider.DeepSeek, provider.CapLLM, resp(c.status, nil), nil)
		require.Equal(t, c.kind, e.Kind, c.status)
		require.Equal(t, c.retry, e.Retryable, c.status)
		require.Equal(t, c.status, e.HTTPStatus)
	}
}

func TestClassifyHTTPQuotaIsNotRetryable(t *testing.T) {
	e := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(429, nil), []byte(`{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`))
	require.Equal(t, provider.KindRateLimited, e.Kind)
	require.False(t, e.Retryable)
	require.Equal(t, "insufficient_quota", e.VendorCode)
	require.Equal(t, "You exceeded your current quota", e.Message)
}

func TestClassifyHTTPRedactsSecretsAndCuts(t *testing.T) {
	body := `{"error":{"message":"Incorrect API key provided: sk-proj-abcdef1234567890. Authorization: Bearer abc.def-ghi"}}`
	e := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(401, nil), []byte(body))
	require.NotContains(t, e.Message, "sk-proj")
	require.NotContains(t, e.Message, "abc.def")
	require.Contains(t, e.Message, "[redacted]")

	long := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(500, nil), []byte(`{"message":"`+strings.Repeat("あ", 400)+`"}`))
	require.LessOrEqual(t, len(long.Message), 512)
	require.True(t, strings.ToValidUTF8(long.Message, "") == long.Message)
}

func TestClassifyHTTPNeverEchoesARawBody(t *testing.T) {
	e := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp(400, nil), []byte("the user said: SECRET-PRIVATE-TEXT"))
	require.NotContains(t, e.Message, "SECRET-PRIVATE-TEXT")
	require.NotContains(t, e.Error(), "SECRET-PRIVATE-TEXT")
}

func TestRedact(t *testing.T) {
	for _, in := range []string{"key sk_live_ABCDEFGH12345", "key sk-live_ABCDEFGH12345", "Bearer tok_12345.abc", "api_key=SECRETVALUE123", `"xi-api-key": "abcdef123456"`} {
		out := httpx.Redact(in)
		require.Contains(t, out, "[redacted]", in)
		require.NotContains(t, out, "ABCDEFGH12345")
		require.NotContains(t, out, "SECRETVALUE123")
		require.NotContains(t, out, "abcdef123456")
	}
	require.Equal(t, "plain message", httpx.Redact("plain message"))
}

func TestClassifyBody(t *testing.T) {
	e := httpx.ClassifyBody(provider.OpenAI, provider.CapEvaluation, errors.New("invalid character"))
	require.Equal(t, provider.KindInvalidResponse, e.Kind)
	require.True(t, e.Retryable)
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyError(t *testing.T) {
	p, c := provider.ElevenLabs, provider.CapTTS
	require.Equal(t, provider.KindCancelled, httpx.ClassifyError(p, c, context.Canceled).Kind)
	require.False(t, httpx.ClassifyError(p, c, context.Canceled).Retryable)
	require.Equal(t, provider.KindTimeout, httpx.ClassifyError(p, c, context.DeadlineExceeded).Kind)
	require.Equal(t, provider.KindTimeout, httpx.ClassifyError(p, c, &net.OpError{Op: "read", Err: timeoutErr{}}).Kind)
	require.Equal(t, provider.KindUnavailable, httpx.ClassifyError(p, c, &net.DNSError{Err: "no such host", Name: "x"}).Kind)
	require.Equal(t, provider.KindUnavailable, httpx.ClassifyError(p, c, errors.New("connection reset by peer")).Kind)
	require.True(t, httpx.ClassifyError(p, c, errors.New("tls: handshake failure")).Retryable)

	existing := &provider.Error{Kind: provider.KindAuth}
	require.Same(t, existing, httpx.ClassifyError(p, c, existing), "an already classified error passes through")
}

func TestClassifyWSClose(t *testing.T) {
	for _, code := range []int{1006, 1011, 1012, 1013} {
		e := httpx.ClassifyWSClose(provider.Cartesia, provider.CapSTT, code)
		require.Equal(t, provider.KindUnavailable, e.Kind, code)
		require.True(t, e.Retryable)
	}
}

func TestNewClientTransport(t *testing.T) {
	c := httpx.NewClient()
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.True(t, tr.ForceAttemptHTTP2)
	require.Equal(t, 256, tr.MaxIdleConnsPerHost)
	require.Equal(t, 90*time.Second, tr.IdleConnTimeout)
	require.Equal(t, 3*time.Second, tr.TLSHandshakeTimeout)
	require.Zero(t, tr.ResponseHeaderTimeout)
	require.Zero(t, c.Timeout, "streams are bounded by the caller's context")
}

func TestSSEReader(t *testing.T) {
	r := httpx.NewSSEReader(strings.NewReader(": ping\n\ndata: {\"a\":1}\n\nevent: x\ndata: l1\ndata: l2\n\ndata: [DONE]\n\n"))
	ev, d, err := r.Next()
	require.NoError(t, err)
	require.Empty(t, ev)
	require.Equal(t, `{"a":1}`, d)
	ev, d, err = r.Next()
	require.NoError(t, err)
	require.Equal(t, "x", ev)
	require.Equal(t, "l1\nl2", d)
	_, d, err = r.Next()
	require.NoError(t, err)
	require.Equal(t, "[DONE]", d, "returned as is")
	_, _, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestSSEReaderEdgeCases(t *testing.T) {
	r := httpx.NewSSEReader(strings.NewReader("data:no-space\r\n\r\nid: 1\nretry: 100\ndata: x\n\ndata: last"))
	_, d, err := r.Next()
	require.NoError(t, err)
	require.Equal(t, "no-space", d, "CRLF and a missing space after the colon")
	_, d, err = r.Next()
	require.NoError(t, err)
	require.Equal(t, "x", d, "id and retry fields are ignored")
	_, d, err = r.Next()
	require.NoError(t, err)
	require.Equal(t, "last", d, "an unterminated final event is still delivered")
	_, _, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestSSEReaderLongLine(t *testing.T) {
	big := strings.Repeat("a", 200_000)
	r := httpx.NewSSEReader(strings.NewReader("data: " + big + "\n\n"))
	_, d, err := r.Next()
	require.NoError(t, err)
	require.Len(t, d, 200_000)
}
