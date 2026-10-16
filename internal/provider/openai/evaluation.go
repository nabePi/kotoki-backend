package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

// defaultEvalTemperature applies when StructuredRequest.Temperature is negative (spec 8.2.5).
const defaultEvalTemperature = 0.2

var schemaNameRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Evaluation generates structured JSON with strict json_schema output (spec 8.4.8).
type Evaluation struct{ c *client }

var _ provider.EvaluationProvider = (*Evaluation)(nil)

// NewEvaluation builds the evaluation adapter; APIKey and BaseURL are required.
func NewEvaluation(cfg Config, hc *http.Client, log *slog.Logger) (*Evaluation, error) {
	c, err := newClient(cfg, hc, log)
	if err != nil {
		return nil, err
	}
	return &Evaluation{c: c}, nil
}

// Name is OPENAI.
func (*Evaluation) Name() provider.Name { return provider.OpenAI }

type jsonSchemaFormat struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type responseFormat struct {
	Type       string           `json:"type"`
	JSONSchema jsonSchemaFormat `json:"json_schema"`
}

type evalRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Temperature         float64        `json:"temperature"`
	Seed                *int64         `json:"seed,omitempty"`
	ResponseFormat      responseFormat `json:"response_format"`
}

type evalResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func invalidRequest(cp provider.Capability, msg string) *provider.Error {
	return provider.NewError(provider.OpenAI, cp, provider.KindInvalidRequest, msg)
}

func invalidResponse(msg string) *provider.Error {
	e := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindInvalidResponse, msg)
	e.Retryable = true
	return e
}

// GenerateJSON is non-streaming. A refusal is CONTENT_FILTERED (not retried); truncation by
// length, empty content and invalid JSON are INVALID_RESPONSE (retryable).
func (e *Evaluation) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	ctx, span := httpx.StartSpan(ctx, provider.CapEvaluation, provider.OpenAI, req.Model)
	res, err := e.generate(ctx, req)
	if err != nil {
		span.End(outcomeOf(err), err)
		e.c.logFailure(provider.CapEvaluation, req.Model, req.RequestID, err)
		return provider.StructuredResult{}, err
	}
	span.End(httpx.OutcomeSuccess, nil)
	return res, nil
}

func (e *Evaluation) generate(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	switch {
	case !schemaNameRE.MatchString(req.SchemaName):
		return provider.StructuredResult{}, invalidRequest(provider.CapEvaluation, "schema name must match [a-z0-9_]{1,64}")
	case !json.Valid(req.JSONSchema):
		return provider.StructuredResult{}, invalidRequest(provider.CapEvaluation, "JSON schema is not valid JSON")
	case len(req.Messages) == 0:
		return provider.StructuredResult{}, invalidRequest(provider.CapEvaluation, "no messages")
	}
	temp := req.Temperature
	if temp < 0 {
		temp = defaultEvalTemperature
	}
	body := evalRequest{
		Model: req.Model, Messages: toChatMessages(req.Messages), MaxCompletionTokens: req.MaxOutputTokens,
		Temperature: temp, Seed: req.Seed,
		ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: jsonSchemaFormat{Name: req.SchemaName, Strict: true, Schema: req.JSONSchema}},
	}
	start := time.Now()
	resp, err := e.c.post(ctx, provider.CapEvaluation, "/chat/completions", body, false)
	if err != nil {
		return provider.StructuredResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out evalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if ctx.Err() != nil {
			return provider.StructuredResult{}, httpx.ClassifyError(provider.OpenAI, provider.CapEvaluation, ctx.Err())
		}
		return provider.StructuredResult{}, httpx.ClassifyBody(provider.OpenAI, provider.CapEvaluation, err)
	}
	if len(out.Choices) == 0 {
		return provider.StructuredResult{}, invalidResponse(errMessageNoChoices)
	}
	ch := out.Choices[0]
	if ch.Message.Refusal != nil && *ch.Message.Refusal != "" {
		return provider.StructuredResult{}, provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindContentFiltered, "model refused the request")
	}
	switch ch.FinishReason {
	case "content_filter":
		return provider.StructuredResult{}, provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindContentFiltered, "response blocked by content filter")
	case "length":
		return provider.StructuredResult{}, invalidResponse("output truncated at max tokens")
	}
	if ch.Message.Content == nil || *ch.Message.Content == "" {
		return provider.StructuredResult{}, invalidResponse(errMessageEmptyReply)
	}
	raw := json.RawMessage(*ch.Message.Content)
	if !json.Valid(raw) {
		return provider.StructuredResult{}, invalidResponse("content is not valid JSON")
	}
	res := provider.StructuredResult{
		JSON: raw, FinishReason: provider.FinishStop, Provider: provider.OpenAI, Model: req.Model, Latency: time.Since(start),
	}
	if out.Usage != nil {
		res.Usage = out.Usage.tokenUsage()
	}
	return res, nil
}
