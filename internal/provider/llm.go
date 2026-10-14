package provider

import (
	"context"
	"time"
)

// LLMRequest is one streaming completion. Model is filled by the router.
type LLMRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int // >0; 0 → LLM_MAX_OUTPUT_TOKENS
	Temperature float64
	Stop        []string // at most 4
	RequestID   string   // "{session_id}:{generation}"
}

// LLMProvider streams completions.
type LLMProvider interface {
	Name() Name
	// Stream returns once the vendor accepted the request (not at the first token).
	Stream(ctx context.Context, req LLMRequest) (LLMStream, error)
}

// LLMStream delivers events; the channel is buffered (128) and closed after DONE or ERROR.
type LLMStream interface {
	Events() <-chan LLMEvent
	Close() error // same as cancel; idempotent
}

// LLMEventType is the kind of LLMEvent.
type LLMEventType string

// LLM event types.
const (
	LLMDelta LLMEventType = "DELTA"
	LLMDone  LLMEventType = "DONE"
	LLMError LLMEventType = "ERROR"
)

// FinishReason says why a stream ended.
type FinishReason string

// Finish reasons.
const (
	FinishStop            FinishReason = "STOP"
	FinishMaxTokens       FinishReason = "MAX_TOKENS"
	FinishContentFiltered FinishReason = "CONTENT_FILTERED"
	FinishCancelled       FinishReason = "CANCELLED"
	// FinishTruncated comes only from the router Generation (8.5.7), never from an
	// adapter: the LLM total timeout or the session deadline hit after MarkAudioSent (S-7).
	FinishTruncated FinishReason = "TRUNCATED"
)

// LLMEvent is one stream event.
type LLMEvent struct {
	Type         LLMEventType
	Text         string       // DELTA
	FinishReason FinishReason // DONE
	Usage        *TokenUsage  // DONE, always non-nil (real or estimated)
	Provider     Name
	Model        string
	ReceivedAt   time.Time
	Err          *Error // ERROR
}
