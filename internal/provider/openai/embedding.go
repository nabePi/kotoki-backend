package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const (
	embeddingDims     = 1536
	maxEmbeddingTexts = 64
)

// Embedding embeds text with the OpenAI embeddings endpoint (spec 8.4.10).
type Embedding struct{ c *client }

var _ provider.EmbeddingProvider = (*Embedding)(nil)

// NewEmbedding builds the embedding adapter; APIKey and BaseURL are required.
func NewEmbedding(cfg Config, hc *http.Client, log *slog.Logger) (*Embedding, error) {
	c, err := newClient(cfg, hc, log)
	if err != nil {
		return nil, err
	}
	return &Embedding{c: c}, nil
}

// Name is OPENAI.
func (*Embedding) Name() provider.Name { return provider.OpenAI }

type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// Embed returns one 1536-dim vector per text, in request order. The dimensions parameter is
// never sent (the model default is 1536).
func (e *Embedding) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	ctx, span := httpx.StartSpan(ctx, provider.CapEmbedding, provider.OpenAI, req.Model)
	res, err := e.embed(ctx, req)
	if err != nil {
		span.End(outcomeOf(err), err)
		e.c.logFailure(provider.CapEmbedding, req.Model, req.RequestID, err)
		return provider.EmbeddingResult{}, err
	}
	span.End(httpx.OutcomeSuccess, nil)
	return res, nil
}

func (e *Embedding) embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	switch {
	case len(req.Texts) == 0:
		return provider.EmbeddingResult{}, invalidRequest(provider.CapEmbedding, "no texts")
	case len(req.Texts) > maxEmbeddingTexts:
		return provider.EmbeddingResult{}, invalidRequest(provider.CapEmbedding, "at most 64 texts per request")
	case req.Dimensions != 0 && req.Dimensions != embeddingDims:
		return provider.EmbeddingResult{}, invalidRequest(provider.CapEmbedding, "only 1536 dimensions are supported")
	}
	resp, err := e.c.post(ctx, provider.CapEmbedding, "/embeddings",
		embedRequest{Model: req.Model, Input: req.Texts, EncodingFormat: "float"}, false)
	if err != nil {
		return provider.EmbeddingResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if ctx.Err() != nil {
			return provider.EmbeddingResult{}, httpx.ClassifyError(provider.OpenAI, provider.CapEmbedding, ctx.Err())
		}
		return provider.EmbeddingResult{}, httpx.ClassifyBody(provider.OpenAI, provider.CapEmbedding, err)
	}
	bad := func(msg string) error {
		pe := httpx.ClassifyBody(provider.OpenAI, provider.CapEmbedding, nil)
		pe.Message = msg
		return pe
	}
	if len(out.Data) != len(req.Texts) {
		return provider.EmbeddingResult{}, bad("embedding count does not match the input count")
	}
	vectors := make([][]float32, len(req.Texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vectors) || vectors[d.Index] != nil {
			return provider.EmbeddingResult{}, bad("embedding index out of range or repeated")
		}
		if len(d.Embedding) != embeddingDims {
			return provider.EmbeddingResult{}, bad("embedding has the wrong dimension")
		}
		vectors[d.Index] = d.Embedding
	}
	res := provider.EmbeddingResult{Vectors: vectors, Provider: provider.OpenAI, Model: req.Model}
	if out.Usage != nil {
		res.Usage.InputTokens = out.Usage.PromptTokens
	}
	return res, nil
}
