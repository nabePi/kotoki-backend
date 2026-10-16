package deepseek

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

// Evaluation generates JSON with DeepSeek JSON mode (spec 8.4.9). The vendor only guarantees
// syntactically valid JSON (response_format json_object; there is no json_schema/strict), so the
// schema is injected as an instruction and NOT enforced here: GenerateJSON returns any valid JSON
// as is, and the caller (M7/M8) MUST validate it against the schema and run the repair path.
// Empty content, truncation and invalid JSON are INVALID_RESPONSE (retryable). One attempt per
// call; no retry or vendor fallback inside the adapter.
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

// Name is DEEPSEEK.
func (*Evaluation) Name() provider.Name { return provider.DeepSeek }

type jsonObjectFormat struct {
	Type string `json:"type"`
}

type evalRequest struct {
	Model          string           `json:"model"`
	Messages       []chatMessage    `json:"messages"`
	MaxTokens      int              `json:"max_tokens,omitempty"`
	Temperature    float64          `json:"temperature"`
	ResponseFormat jsonObjectFormat `json:"response_format"`
	Thinking       thinkingParam    `json:"thinking"`
}

type evalResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func invalidRequest(msg string) *provider.Error {
	return provider.NewError(provider.DeepSeek, provider.CapEvaluation, provider.KindInvalidRequest, msg)
}

func invalidResponse(msg string) *provider.Error {
	e := provider.NewError(provider.DeepSeek, provider.CapEvaluation, provider.KindInvalidResponse, msg)
	e.Retryable = true
	return e
}

// withSchemaInstruction inserts the instruction message at the end of the leading system block
// (or first when there is none); the other messages keep their order.
func withSchemaInstruction(ms []provider.Message, instruction string) []provider.Message {
	n := 0
	for n < len(ms) && ms[n].Role == provider.RoleSystem {
		n++
	}
	out := make([]provider.Message, 0, len(ms)+1)
	out = append(out, ms[:n]...)
	out = append(out, provider.Message{Role: provider.RoleSystem, Content: instruction})
	return append(out, ms[n:]...)
}

// GenerateJSON is non-streaming. content_filter is CONTENT_FILTERED (not retried); truncation by
// length, empty content and invalid JSON are INVALID_RESPONSE (retryable).
func (e *Evaluation) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	ctx, span := httpx.StartSpan(ctx, provider.CapEvaluation, provider.DeepSeek, req.Model)
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
		return provider.StructuredResult{}, invalidRequest("schema name must match [a-z0-9_]{1,64}")
	case !json.Valid(req.JSONSchema):
		return provider.StructuredResult{}, invalidRequest("JSON schema is not valid JSON")
	case len(req.Messages) == 0:
		return provider.StructuredResult{}, invalidRequest("no messages")
	}
	instruction, err := schemaInstruction(req.JSONSchema, req.SchemaName)
	if err != nil {
		return provider.StructuredResult{}, invalidRequest("JSON schema must be a JSON object")
	}
	temp := req.Temperature
	if temp < 0 {
		temp = defaultEvalTemperature
	}
	thinking, _ := thinkingValue(e.c.cfg.EvaluationThinking) // validated by newClient
	// StructuredRequest.Seed is not sent: the DeepSeek API reference documents no seed parameter.
	body := evalRequest{
		Model: req.Model, Messages: toChatMessages(withSchemaInstruction(req.Messages, instruction)),
		MaxTokens: req.MaxOutputTokens, Temperature: temp,
		ResponseFormat: jsonObjectFormat{Type: "json_object"}, Thinking: thinkingParam{Type: thinking},
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
			return provider.StructuredResult{}, httpx.ClassifyError(provider.DeepSeek, provider.CapEvaluation, ctx.Err())
		}
		return provider.StructuredResult{}, httpx.ClassifyBody(provider.DeepSeek, provider.CapEvaluation, err)
	}
	if len(out.Choices) == 0 {
		return provider.StructuredResult{}, invalidResponse(errMessageNoChoices)
	}
	ch := out.Choices[0]
	switch {
	case ch.FinishReason == "content_filter":
		return provider.StructuredResult{}, provider.NewError(provider.DeepSeek, provider.CapEvaluation, provider.KindContentFiltered, "response blocked by content filter")
	case ch.FinishReason == "length":
		return provider.StructuredResult{}, invalidResponse("output truncated at max tokens")
	case vendorStopped(ch.FinishReason):
		ve := provider.NewError(provider.DeepSeek, provider.CapEvaluation, provider.KindUnavailable, "vendor stopped generation: "+ch.FinishReason)
		ve.Retryable = true
		ve.VendorCode = ch.FinishReason
		return provider.StructuredResult{}, ve
	}
	// Documented: JSON mode may occasionally return empty content.
	if ch.Message.Content == nil || *ch.Message.Content == "" {
		return provider.StructuredResult{}, invalidResponse(errMessageEmptyReply)
	}
	raw := json.RawMessage(*ch.Message.Content)
	if !json.Valid(raw) {
		return provider.StructuredResult{}, invalidResponse("content is not valid JSON")
	}
	res := provider.StructuredResult{
		JSON: raw, FinishReason: provider.FinishStop, Provider: provider.DeepSeek, Model: req.Model, Latency: time.Since(start),
	}
	if out.Usage != nil {
		res.Usage = out.Usage.tokenUsage()
	}
	return res, nil
}
