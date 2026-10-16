package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const maxErrorBody = 64 << 10

// Rate-limit headers read when present (not documented by DeepSeek, see Config.Metrics).
const (
	headerRemainingRequests = "x-ratelimit-remaining-requests"
	headerRemainingTokens   = "x-ratelimit-remaining-tokens" //nolint:gosec // header name, not a credential
)

// Vendor codes that change the normalised kind. DeepSeek documents status codes only (error
// body shape is the OpenAI-compatible one); the content-filter codes mirror OpenAI and are
// defensive (spec 8.4.4: content_filter normally arrives as finish_reason).
const (
	codeContentFilter   = "content_filter"
	codeContentPolicy   = "content_policy_violation"
	statusInsufficient  = 402 // "Insufficient Balance" (https://api-docs.deepseek.com/quick_start/error_codes)
	errMessageStreamCut = "stream ended before [DONE]"
)

// client is the HTTP plumbing shared by the DeepSeek capabilities.
type client struct {
	cfg Config
	hc  *http.Client
	log *slog.Logger
}

func newClient(cfg Config, hc *http.Client, log *slog.Logger) (*client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("deepseek: APIKey is empty")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("deepseek: BaseURL is empty")
	}
	if _, err := thinkingValue(cfg.Thinking); err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &client{cfg: cfg, hc: hc, log: log}, nil
}

// post sends a JSON POST and returns the 2xx response (body open) or a classified *provider.Error.
func (c *client) post(ctx context.Context, cp provider.Capability, path string, body any, stream bool) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, provider.NewError(provider.DeepSeek, cp, provider.KindInvalidRequest, "request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, provider.NewError(provider.DeepSeek, cp, provider.KindInvalidRequest, "request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		e := httpx.ClassifyError(provider.DeepSeek, cp, err)
		e.Message = c.scrub(e.Message)
		return nil, e
	}
	c.publishRateLimit(cp, resp.Header)
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		e := classify(cp, resp, b)
		e.Message = c.scrub(e.Message) // httpx.Redact only knows key shapes; drop our exact key too
		e.VendorCode = c.scrub(e.VendorCode)
		return nil, e
	}
	return resp, nil
}

func (c *client) scrub(s string) string { return strings.ReplaceAll(s, c.cfg.APIKey, "[redacted]") }

// classify maps a non-2xx response. On top of httpx.ClassifyHTTP: 402 (insufficient balance) is
// AUTH and never retried; 400 with a content-filter code is CONTENT_FILTERED, never retried.
func classify(cp provider.Capability, resp *http.Response, body []byte) *provider.Error {
	e := httpx.ClassifyHTTP(provider.DeepSeek, cp, resp, body)
	switch {
	case resp.StatusCode == statusInsufficient:
		e.Kind = provider.KindAuth
		e.Retryable = false
	case e.Kind == provider.KindInvalidRequest && (e.VendorCode == codeContentFilter || e.VendorCode == codeContentPolicy):
		e.Kind = provider.KindContentFiltered
		e.Retryable = false
	}
	return e
}

// publishRateLimit exports the remaining-requests/-tokens headers when present.
func (c *client) publishRateLimit(cp provider.Capability, h http.Header) {
	if c.cfg.Metrics == nil {
		return
	}
	for kind, name := range map[string]string{"requests": headerRemainingRequests, "tokens": headerRemainingTokens} {
		if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(name)), 64); err == nil {
			c.cfg.Metrics.RateLimitRemaining(provider.DeepSeek, cp, kind, v)
		}
	}
}

// logFailure logs a failed call: identifiers and classification only, never keys, text or audio.
func (c *client) logFailure(cp provider.Capability, model, requestID string, err error) {
	if err == nil || provider.KindOf(err) == provider.KindCancelled {
		return
	}
	attrs := []any{"provider", string(provider.DeepSeek), "capability", string(cp), "model", model,
		"request_id", requestID, "kind", string(provider.KindOf(err))}
	var pe *provider.Error
	if errors.As(err, &pe) {
		attrs = append(attrs, "http_status", pe.HTTPStatus, "vendor_code", pe.VendorCode)
	}
	c.log.Warn("provider call failed", attrs...)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func toChatMessages(ms []provider.Message) []chatMessage {
	out := make([]chatMessage, len(ms))
	for i, m := range ms {
		out[i] = chatMessage{Role: string(m.Role), Content: m.Content}
	}
	return out
}

type thinkingParam struct {
	Type string `json:"type"`
}

// chatUsage is the usage object of DeepSeek responses.
type chatUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

func (u *chatUsage) tokenUsage() provider.TokenUsage {
	return provider.TokenUsage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, CachedInputTokens: u.PromptCacheHitTokens}
}

// estimateTokens is the 1.0 token per rune estimator of spec 8.7.3 over every message.
func estimateTokens(ms []provider.Message) int {
	n := 0
	for _, m := range ms {
		n += utf8.RuneCountInString(m.Content)
	}
	return n
}
