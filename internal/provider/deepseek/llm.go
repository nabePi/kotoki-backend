package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

// LLM streams Chat Completions-compatible responses (spec 8.4.4).
type LLM struct{ c *client }

var _ provider.LLMProvider = (*LLM)(nil)

// NewLLM builds the LLM adapter; APIKey and BaseURL are required.
func NewLLM(cfg Config, hc *http.Client, log *slog.Logger) (*LLM, error) {
	c, err := newClient(cfg, hc, log)
	if err != nil {
		return nil, err
	}
	return &LLM{c: c}, nil
}

// Name is OPENAI.
func (*LLM) Name() provider.Name { return provider.DeepSeek }

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatRequest is the DeepSeek body: max_tokens (not max_completion_tokens) and thinking.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   float64        `json:"temperature"`
	Stop          []string       `json:"stop,omitempty"`
	Thinking      thinkingParam  `json:"thinking"`
}

// Stream returns once the vendor answered 200; failures before that are returned as *provider.Error.
func (l *LLM) Stream(ctx context.Context, req provider.LLMRequest) (provider.LLMStream, error) {
	thinking, _ := thinkingValue(l.c.cfg.Thinking) // validated by newClient
	body := chatRequest{
		Model: req.Model, Messages: toChatMessages(req.Messages), Stream: true,
		StreamOptions: &streamOptions{IncludeUsage: true}, MaxTokens: req.MaxTokens,
		Temperature: req.Temperature, Stop: req.Stop, Thinking: thinkingParam{Type: thinking},
	}
	ctx, span := httpx.StartSpan(ctx, provider.CapLLM, provider.DeepSeek, req.Model)
	sctx, cancel := context.WithCancel(ctx)
	resp, err := l.c.post(sctx, provider.CapLLM, "/chat/completions", body, true) //nolint:bodyclose // closed by run
	if err != nil {
		cancel()
		span.End(outcomeOf(err), err)
		l.c.logFailure(provider.CapLLM, req.Model, req.RequestID, err)
		return nil, err
	}
	st := &llmStream{ch: make(chan provider.LLMEvent, 128), cancel: cancel}
	go l.run(sctx, resp, st, req, span)
	return st, nil
}

func outcomeOf(err error) string {
	if provider.KindOf(err) == provider.KindCancelled {
		return httpx.OutcomeCancelled
	}
	return httpx.OutcomeError
}

type llmStream struct {
	ch     chan provider.LLMEvent
	cancel context.CancelFunc
	once   sync.Once
}

func (s *llmStream) Events() <-chan provider.LLMEvent { return s.ch }

// Close cancels the request; idempotent.
func (s *llmStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (l *LLM) run(ctx context.Context, resp *http.Response, st *llmStream, req provider.LLMRequest, span *httpx.Span) {
	defer close(st.ch)
	defer st.cancel()
	defer func() { _ = resp.Body.Close() }()

	var (
		outRunes int
		finish   = provider.FinishStop
		usage    *provider.TokenUsage
	)
	event := func(ev provider.LLMEvent) provider.LLMEvent {
		ev.Provider, ev.Model, ev.ReceivedAt = provider.DeepSeek, req.Model, time.Now()
		return ev
	}
	estimate := func() *provider.TokenUsage {
		return &provider.TokenUsage{InputTokens: estimateTokens(req.Messages), OutputTokens: outRunes, Estimated: true}
	}
	cancelled := func() {
		select { // non-blocking: the consumer may already be gone
		case st.ch <- event(provider.LLMEvent{Type: provider.LLMDone, FinishReason: provider.FinishCancelled, Usage: estimate()}):
		default:
		}
		span.End(httpx.OutcomeCancelled, nil)
	}
	send := func(ev provider.LLMEvent) bool {
		if ctx.Err() != nil {
			return false
		}
		select {
		case st.ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	fail := func(e *provider.Error) {
		l.c.logFailure(provider.CapLLM, req.Model, req.RequestID, e)
		if send(event(provider.LLMEvent{Type: provider.LLMError, Err: e})) {
			span.End(httpx.OutcomeError, e)
			return
		}
		cancelled()
	}

	sse := httpx.NewSSEReader(resp.Body)
	for {
		_, data, err := sse.Next()
		if ctx.Err() != nil {
			cancelled()
			return
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				e := provider.NewError(provider.DeepSeek, provider.CapLLM, provider.KindUnavailable, errMessageStreamCut)
				e.Retryable = true
				fail(e)
				return
			}
			fail(httpx.ClassifyError(provider.DeepSeek, provider.CapLLM, err))
			return
		}
		if data == "[DONE]" {
			u := usage
			if u == nil {
				u = estimate()
			}
			if send(event(provider.LLMEvent{Type: provider.LLMDone, FinishReason: finish, Usage: u})) {
				span.End(httpx.OutcomeSuccess, nil)
			} else {
				cancelled()
			}
			return
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			fail(httpx.ClassifyBody(provider.DeepSeek, provider.CapLLM, err))
			return
		}
		if ch.Error != nil {
			e := provider.NewError(provider.DeepSeek, provider.CapLLM, provider.KindUnavailable, ch.Error.Message)
			e.Retryable = true
			if code, ok := ch.Error.Code.(string); ok && code != "" {
				e.VendorCode = code
			} else {
				e.VendorCode = ch.Error.Type
			}
			fail(e)
			return
		}
		if ch.Usage != nil {
			u := ch.Usage.tokenUsage()
			usage = &u
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" {
				outRunes += utf8.RuneCountInString(c.Delta.Content)
				if !send(event(provider.LLMEvent{Type: provider.LLMDelta, Text: c.Delta.Content})) {
					cancelled()
					return
				}
			}
			if c.FinishReason != nil {
				if vendorStopped(*c.FinishReason) {
					e := provider.NewError(provider.DeepSeek, provider.CapLLM, provider.KindUnavailable, "vendor stopped generation: "+*c.FinishReason)
					e.Retryable = true
					e.VendorCode = *c.FinishReason
					fail(e)
					return
				}
				finish = mapFinish(*c.FinishReason)
			}
		}
	}
}

// vendorStopped reports finish reasons where the vendor aborted generation (spec 8.4.4): they
// are errors, not a normal end.
func vendorStopped(s string) bool { return s == "insufficient_system_resource" || s == "aborted" }

func mapFinish(s string) provider.FinishReason {
	switch s {
	case "length":
		return provider.FinishMaxTokens
	case "content_filter":
		return provider.FinishContentFiltered
	default:
		return provider.FinishStop
	}
}
