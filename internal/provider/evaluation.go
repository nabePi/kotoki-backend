package provider

import (
	"context"
	"encoding/json"
	"time"
)

// StructuredRequest is a non-streaming JSON generation (evaluation, memory extraction).
type StructuredRequest struct {
	Task            string // "EVALUATION" | "MEMORY_EXTRACTION"
	Model           string // filled by the router
	Messages        []Message
	SchemaName      string          // [a-z0-9_]{1,64}
	JSONSchema      json.RawMessage // draft 2020-12 subset; vendors that do not enforce it leave validation to the caller
	MaxOutputTokens int             // >0
	Temperature     float64         // negative → 0.2; 0 is sent as is
	Seed            *int64
	RequestID       string // "{task}:{session_id}"
}

// StructuredResult is a validated-JSON response; schema validation is the caller's job.
type StructuredResult struct {
	JSON         json.RawMessage
	Usage        TokenUsage
	FinishReason FinishReason
	Provider     Name
	Model        string
	Latency      time.Duration
}

// EvaluationProvider generates structured JSON.
type EvaluationProvider interface {
	Name() Name
	// GenerateJSON: MAX_TOKENS or invalid JSON → *Error{Kind: KindInvalidResponse, Retryable: true}.
	GenerateJSON(ctx context.Context, req StructuredRequest) (StructuredResult, error)
}
