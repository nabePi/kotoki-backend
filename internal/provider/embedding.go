package provider

import "context"

// EmbeddingRequest embeds 1..64 texts.
type EmbeddingRequest struct {
	Texts      []string
	Model      string // filled by the router
	Dimensions int    // always 1536 (D-040)
	RequestID  string
}

// EmbeddingResult keeps the order of Texts.
type EmbeddingResult struct {
	Vectors  [][]float32
	Usage    TokenUsage // OutputTokens = 0
	Provider Name
	Model    string
}

// EmbeddingProvider embeds text.
type EmbeddingProvider interface {
	Name() Name
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResult, error)
}
