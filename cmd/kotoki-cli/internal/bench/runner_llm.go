package bench

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// llmOutcome is what one streamed answer measured.
type llmOutcome struct {
	text            string // up to and including the first sentence when stopAtSentence
	ttftMs          float64
	firstSentenceMs float64
	totalMs         float64
	sentenceEnded   bool // a 。！？ closed the first sentence (else the whole answer is the sentence)
	sentence        string
}

// llmOnce streams one answer. With stopAtSentence the stream is closed as soon as the first sentence
// is complete, as the runtime hands that sentence to TTS (used by the e2e stack).
func llmOnce(ctx context.Context, p provider.LLMProvider, req provider.LLMRequest, clk clock.Clock, timeout time.Duration, stopAtSentence bool) (llmOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t0 := clk.Now()
	st, err := p.Stream(ctx, req)
	if err != nil {
		return llmOutcome{}, err
	}
	defer func() { _ = st.Close() }()
	var out llmOutcome
	var b strings.Builder
	gotFirst := false
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case ev, ok := <-st.Events():
			if !ok {
				return out, provider.NewError(p.Name(), provider.CapLLM, provider.KindUnavailable, "stream closed without DONE")
			}
			switch ev.Type {
			case provider.LLMDelta:
				if !gotFirst {
					gotFirst, out.ttftMs = true, ms(clk.Now().Sub(t0))
				}
				b.WriteString(ev.Text)
				if i := sentenceEnd(b.String()); i >= 0 && !out.sentenceEnded {
					out.sentenceEnded, out.firstSentenceMs = true, ms(clk.Now().Sub(t0))
					out.sentence = b.String()[:i]
					if stopAtSentence {
						out.text, out.totalMs = b.String(), out.firstSentenceMs
						return out, nil
					}
				}
			case provider.LLMDone:
				out.text, out.totalMs = b.String(), ms(clk.Now().Sub(t0))
				if !out.sentenceEnded { // no 。！？ at all: the whole answer is the first sentence
					out.firstSentenceMs, out.sentence = out.totalMs, out.text
				}
				if !gotFirst {
					out.ttftMs = out.totalMs
				}
				return out, nil
			case provider.LLMError:
				if ev.Err != nil {
					return out, ev.Err
				}
				return out, provider.NewError(p.Name(), provider.CapLLM, provider.KindUnavailable, "llm error event without detail")
			}
		}
	}
}

// RunLLM measures the LLM candidate on every item (Messages = the prompt), `repeat` times each:
// time to first token, time to the first sentence (to 。！？; the whole answer when it has none),
// total time, answer length in runes and whether the answer is plain Japanese (FormatOK). The answer
// itself is never recorded. A vendor error becomes an error row.
func RunLLM(ctx context.Context, p provider.LLMProvider, c Candidate, items []Item, repeat int, clk clock.Clock, opts ...Option) []Result {
	o := newRunOpts(opts)
	var rs []Result
	for _, it := range items {
		for n := 1; n <= repeat; n++ {
			if ctx.Err() != nil {
				return rs
			}
			r := newResult(SuiteLLM, c, it, n)
			out, err := llmOnce(ctx, p, provider.LLMRequest{
				Model: c.Model, Messages: it.Messages, MaxTokens: o.maxTokens, Temperature: o.temperature,
				RequestID: fmt.Sprintf("bench:%s:%d", it.ID, n),
			}, clk, o.timeout, false)
			if err != nil {
				rs = append(rs, r.failed(err))
				continue
			}
			r.Values["ttft_ms"] = out.ttftMs
			r.Values["first_sentence_ms"] = out.firstSentenceMs
			r.Values["total_ms"] = out.totalMs
			r.Values["runes"] = float64(utf8.RuneCountInString(out.text))
			r.Flags["format_ok"] = FormatOK(out.text)
			r.Flags["no_sentence_end"] = !out.sentenceEnded
			rs = append(rs, r)
		}
	}
	return rs
}

// JSONSpec describes the structured call of one LLM-json item.
type JSONSpec struct {
	Task       string // "EVALUATION" | "MEMORY_EXTRACTION"
	SchemaName string
	Schema     []byte // JSON Schema for vendors that enforce it
}

const repairPreviousMaxRunes = 8000

// repairMessages mirrors the repair call of M7/M8 (spec 10.6.1): the rejected output (cut, omitted
// when there is none) and the validator problems are appended to the prompt.
func repairMessages(base []provider.Message, previous string, problems []string) []provider.Message {
	out := make([]provider.Message, 0, len(base)+2)
	out = append(out, base...)
	if previous != "" {
		if r := []rune(previous); len(r) > repairPreviousMaxRunes {
			previous = string(r[:repairPreviousMaxRunes])
		}
		out = append(out, provider.Message{Role: provider.RoleAssistant, Content: previous})
	}
	var b strings.Builder
	b.WriteString("Your previous output was rejected by the validator. Errors:\n")
	for _, p := range problems {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("Return the complete corrected JSON object only. Follow the schema and all rules exactly.")
	return append(out, provider.Message{Role: provider.RoleUser, Content: b.String()})
}

// RunLLMJSON runs the LLM-json suite: per item and attempt at most TWO calls to GenerateJSON, the
// same budget as M7/M8 (call, then one repair). resolve gives the task, schema name and schema of the
// item; validate is the code-side validator of that schema (nil = valid). Per attempt it records
// first_pass_ok (valid on the first call), after_repair_ok (valid in at most two calls), calls, and
// invalid_response_first (the vendor itself reported an empty/truncated/invalid answer on call 1).
// A non-INVALID_RESPONSE vendor error becomes an error row. Neither the prompt nor the JSON is recorded.
func RunLLMJSON(ctx context.Context, p provider.EvaluationProvider, c Candidate, items []Item, resolve func(Item) (JSONSpec, error),
	validate func(schemaName string, raw []byte) error, repeat int, clk clock.Clock, opts ...Option) []Result {
	o := newRunOpts(opts)
	var rs []Result
	for _, it := range items {
		spec, serr := resolve(it)
		for n := 1; n <= repeat; n++ {
			if ctx.Err() != nil {
				return rs
			}
			r := newResult(SuiteLLMJSON, c, it, n)
			if serr != nil {
				r.Values, r.Flags, r.ErrorKind = nil, nil, "DATASET"
				rs = append(rs, r)
				continue
			}
			rs = append(rs, jsonAttempt(ctx, p, c, it, spec, validate, n, clk, o, r))
		}
	}
	return rs
}

func jsonAttempt(ctx context.Context, p provider.EvaluationProvider, c Candidate, it Item, spec JSONSpec,
	validate func(string, []byte) error, n int, clk clock.Clock, o runOpts, r Result) Result {
	msgs := it.Messages
	t0 := clk.Now()
	r.Flags["first_pass_ok"], r.Flags["after_repair_ok"] = false, false
	for call := 1; call <= 2; call++ {
		cctx, cancel := context.WithTimeout(ctx, o.timeout)
		res, err := p.GenerateJSON(cctx, provider.StructuredRequest{
			Task: spec.Task, Model: c.Model, Messages: msgs, SchemaName: spec.SchemaName, JSONSchema: spec.Schema,
			MaxOutputTokens: o.jsonMaxToken, Temperature: 0, RequestID: fmt.Sprintf("bench:%s:%d:%d", it.ID, n, call),
		})
		cancel()
		previous := ""
		var problems []string
		switch {
		case err != nil && provider.KindOf(err) == provider.KindInvalidResponse:
			if call == 1 {
				r.Flags["invalid_response_first"] = true
			}
			problems = []string{"the response was empty, truncated or not valid JSON"}
		case err != nil:
			return r.failed(err)
		default:
			previous = string(res.JSON)
			if verr := validate(spec.SchemaName, res.JSON); verr != nil {
				problems = strings.Split(verr.Error(), "\n")
			}
		}
		r.Values["calls"] = float64(call)
		if len(problems) == 0 {
			r.Flags["first_pass_ok"] = call == 1
			r.Flags["after_repair_ok"] = true
			break
		}
		msgs = repairMessages(it.Messages, previous, problems)
	}
	r.Values["total_ms"] = ms(clk.Now().Sub(t0))
	return r
}
