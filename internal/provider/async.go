package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxRetryAfter     = 30 * time.Second
	defaultEvalTemp   = 0.2
	embeddingDimsWant = 1536
)

var (
	// jsonBackoff is the wait before attempts 2 and 3 of GenerateJSON and CompleteText.
	jsonBackoff = []time.Duration{time.Second, 4 * time.Second}
	// embedBackoff is the wait before attempts 2 and 3 of Embed.
	embedBackoff = []time.Duration{500 * time.Millisecond, 2 * time.Second}
)

var (
	_ RealtimeRouter = (*Router)(nil)
	_ AsyncRouter    = (*Router)(nil)
)

// asyncCall describes one logical async call to the single active vendor.
type asyncCall struct {
	vendor   Name
	cap      Capability
	model    string
	task     string
	attempts int
	backoff  []time.Duration
	timeout  time.Duration
}

// doAsync runs call against the active vendor with retries on the SAME vendor (no vendor
// fallback, D-017): breaker and semaphore per attempt, Retry-After (≤ 30 s) before the
// backoff, non-retryable and cancelled errors end it at once. call returns the usage to meter.
func (r *Router) doAsync(ctx context.Context, ac asyncCall, call func(ctx context.Context) (TokenUsage, error)) error {
	var last error
	invalid := 0 // INVALID_RESPONSE is retried once only (spec 8.3)
	for a := 1; a <= ac.attempts; a++ {
		tk, release, aerr := r.admitVendor(ctx, ac.vendor, ac.cap)
		if aerr != nil {
			return aerr // circuit_open and local_concurrency are returned as they are
		}
		start := r.clock.Now()
		actx, cancel := context.WithTimeout(ctx, ac.timeout)
		usage, err := call(actx)
		if err != nil {
			err = asError(ac.vendor, ac.cap, err) // ctx errors become TIMEOUT/CANCELLED, others UNAVAILABLE
		}
		cancel()
		release()
		tk.Done(err)
		r.settleAsync(ac, usage, err, r.clock.Now().Sub(start))
		if err == nil {
			return nil
		}
		last = err
		if KindOf(err) == KindCancelled || !IsRetryable(err) || a == ac.attempts {
			return err
		}
		if KindOf(err) == KindInvalidResponse {
			invalid++
			if invalid > 1 {
				return err
			}
		}
		if serr := r.sleep(ctx, r.retryDelay(err, ac.backoff, a)); serr != nil {
			return ctxError(ac.vendor, ac.cap, serr)
		}
	}
	return last
}

func (r *Router) retryDelay(err error, backoff []time.Duration, attempt int) time.Duration {
	var pe *Error
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		return min(pe.RetryAfter, maxRetryAfter)
	}
	i := min(attempt-1, len(backoff)-1)
	return r.jitter(backoff[i])
}

// settleAsync records metrics, cost and health of one attempt.
func (r *Router) settleAsync(ac asyncCall, usage TokenUsage, err error, d time.Duration) {
	outcome, kind, code := "success", ErrorKind(""), ""
	switch KindOf(err) {
	case KindCancelled:
		outcome = "cancelled"
	default:
		if err != nil {
			outcome = "error"
			pe := asError(ac.vendor, ac.cap, err)
			kind, code = pe.Kind, pe.VendorCode
		}
	}
	r.metrics.ObserveCall(ac.vendor, ac.cap, ac.model, ac.task, outcome, kind, code)
	r.metrics.ObserveLatency(ac.vendor, ac.cap, ac.model, "total", d)
	r.metrics.Usage(ac.vendor, ac.cap, ac.model, "input_tokens", float64(usage.InputTokens))
	r.metrics.Usage(ac.vendor, ac.cap, ac.model, "output_tokens", float64(usage.OutputTokens))
	r.metrics.Usage(ac.vendor, ac.cap, ac.model, "requests", 1)
	if r.prices != nil && err == nil {
		if usd, ok := r.prices.LLM(ac.vendor, ac.cap, ac.model, usage); ok {
			r.metrics.CostUSD(ac.vendor, ac.cap, ac.model, usd)
		}
	}
	r.health.Record(ac.vendor, ac.cap, err, d)
	r.log.Debug("provider call", "provider", string(ac.vendor), "capability", string(ac.cap), "model", ac.model, "task", ac.task,
		"outcome", outcome, "kind", string(kind), "latency_total_ms", d.Milliseconds(), "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
}

// GenerateJSON retries on the active evaluation vendor: up to 3 attempts with backoff 1 s, 4 s.
func (r *Router) GenerateJSON(ctx context.Context, req StructuredRequest) (StructuredResult, error) {
	return r.generateJSON(ctx, req, 3)
}

func (r *Router) generateJSON(ctx context.Context, req StructuredRequest, attempts int) (StructuredResult, error) {
	req.Model = r.evaluationModel()
	if req.Temperature < 0 {
		req.Temperature = defaultEvalTemp
	}
	ac := asyncCall{vendor: r.eval.Name(), cap: CapEvaluation, model: req.Model, task: req.Task, attempts: attempts,
		backoff: jsonBackoff, timeout: r.cfg.TimeoutEvaluation}
	var res StructuredResult
	start := r.clock.Now()
	err := r.doAsync(ctx, ac, func(actx context.Context) (TokenUsage, error) {
		out, err := r.eval.GenerateJSON(actx, req)
		if err != nil {
			return TokenUsage{}, err
		}
		if !json.Valid(out.JSON) {
			return out.Usage, &Error{Provider: ac.vendor, Capability: CapEvaluation, Kind: KindInvalidResponse, Retryable: true, Message: "response is not valid JSON"}
		}
		res = out
		return out.Usage, nil
	})
	if err != nil {
		return StructuredResult{}, err
	}
	res.Latency = r.clock.Now().Sub(start)
	if res.Provider == "" {
		res.Provider = ac.vendor
	}
	if res.Model == "" {
		res.Model = req.Model
	}
	return res, nil
}

// evaluationOnce is the router-wrapped provider.EvaluationProvider handed to callers that run
// their own call sequence (Bab 10): exactly one attempt per call.
type evaluationOnce struct{ r *Router }

var _ EvaluationProvider = evaluationOnce{}

func (e evaluationOnce) Name() Name { return e.r.eval.Name() }

func (e evaluationOnce) GenerateJSON(ctx context.Context, req StructuredRequest) (StructuredResult, error) {
	return e.r.generateJSON(ctx, req, 1)
}

// Evaluation returns the active evaluation provider wrapped with breaker, semaphore,
// timeout and metrics; it never retries.
func (r *Router) Evaluation() EvaluationProvider { return evaluationOnce{r} }

// CompleteText collects a plain-text completion from the active LLM vendor. Workers make up to
// 3 attempts, the realtime process one (rolling summary); the first-token timer does not apply.
func (r *Router) CompleteText(ctx context.Context, req TextRequest) (TextResult, error) {
	attempts := 1
	if r.role == RoleWorker {
		attempts = 3
	}
	model := r.textModel(req)
	ac := asyncCall{vendor: r.llm.Name(), cap: CapLLM, model: model, task: req.Task, attempts: attempts,
		backoff: jsonBackoff, timeout: r.cfg.TimeoutLLMTotal}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = r.cfg.LLMMaxOutputTokens
	}
	temp := req.Temperature
	if temp < 0 {
		temp = r.cfg.LLMTemperature
	}
	var res TextResult
	start := r.clock.Now()
	err := r.doAsync(ctx, ac, func(actx context.Context) (TokenUsage, error) {
		text, finish, usage, err := r.collectLLM(actx, ac.vendor, LLMRequest{Model: model, Messages: req.Messages, MaxTokens: maxTokens,
			Temperature: temp, RequestID: req.RequestID})
		if err != nil {
			return usage, err
		}
		res = TextResult{Text: text, Usage: usage, FinishReason: finish, Provider: ac.vendor, Model: model}
		return usage, nil
	})
	if err != nil {
		return TextResult{}, err
	}
	res.Latency = r.clock.Now().Sub(start)
	return res, nil
}

// collectLLM streams a completion and gathers the text.
func (r *Router) collectLLM(ctx context.Context, p Name, req LLMRequest) (string, FinishReason, TokenUsage, error) {
	stream, err := r.llm.Stream(ctx, req)
	if err != nil {
		return "", "", TokenUsage{}, asError(p, CapLLM, err)
	}
	defer func() { _ = stream.Close() }()
	var b strings.Builder
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				return "", "", TokenUsage{}, NewError(p, CapLLM, KindUnavailable, "stream closed without a terminal event")
			}
			switch ev.Type {
			case LLMDelta:
				b.WriteString(ev.Text)
			case LLMError:
				return "", "", TokenUsage{}, asError(p, CapLLM, ev.Err)
			case LLMDone:
				usage := TokenUsage{InputTokens: messageRunes(req.Messages), OutputTokens: utf8.RuneCountInString(b.String()), Estimated: true}
				if ev.Usage != nil {
					usage = *ev.Usage
				}
				switch ev.FinishReason {
				case FinishContentFiltered:
					return "", "", usage, NewError(p, CapLLM, KindContentFiltered, "response blocked by the content filter")
				case FinishCancelled:
					return "", "", usage, ctxError(p, CapLLM, context.Canceled)
				}
				return b.String(), ev.FinishReason, usage, nil
			}
		case <-ctx.Done():
			return "", "", TokenUsage{}, ctxError(p, CapLLM, ctx.Err())
		}
	}
}

// Embed embeds texts on the OpenAI embedding vendor: up to 3 attempts, backoff 500 ms, 2 s.
func (r *Router) Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResult, error) {
	req.Model = r.cfg.OpenAIEmbeddingModel
	if req.Dimensions == 0 {
		req.Dimensions = embeddingDimsWant
	}
	ac := asyncCall{vendor: r.embed.Name(), cap: CapEmbedding, model: req.Model, attempts: 3, backoff: embedBackoff, timeout: r.cfg.TimeoutEmbedding}
	var res EmbeddingResult
	err := r.doAsync(ctx, ac, func(actx context.Context) (TokenUsage, error) {
		out, err := r.embed.Embed(actx, req)
		if err != nil {
			return TokenUsage{}, err
		}
		res = out
		return out.Usage, nil
	})
	if err != nil {
		return EmbeddingResult{}, err
	}
	if res.Provider == "" {
		res.Provider = ac.vendor
	}
	if res.Model == "" {
		res.Model = req.Model
	}
	return res, nil
}
