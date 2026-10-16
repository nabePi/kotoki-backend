package bench

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Suites (the --suite values; "all" runs them in this order).
const (
	SuiteSTT         = "stt"
	SuiteLLM         = "llm"
	SuiteLLMJSON     = "llm-json"
	SuiteTTS         = "tts"
	SuiteE2E         = "e2e"
	SuiteConcurrency = "concurrency"
	// SuiteHuman and SuiteCost carry data that is not measured by a run: the averaged human
	// ratings and the priced cost per minute. They are written to results.jsonl too, so that
	// summary.csv can be rebuilt from results.jsonl alone.
	SuiteHuman = "human"
	SuiteCost  = "cost"
)

// AllSuites is the order of `--suite all`.
var AllSuites = []string{SuiteSTT, SuiteLLM, SuiteLLMJSON, SuiteTTS, SuiteE2E, SuiteConcurrency}

// CapE2E labels the rows of a composed STT, LLM and TTS stack.
const CapE2E provider.Capability = "E2E"

// Result is one attempt (one line of results.jsonl). It carries ids and numbers only: never the
// reference text, the transcript, a prompt, an answer or a key.
type Result struct {
	Suite      string             `json:"suite"`
	Capability string             `json:"capability"`
	Provider   string             `json:"provider"` // E2E: "STT+LLM+TTS"
	Model      string             `json:"model"`    // E2E: "stt+llm+tts"
	Voice      string             `json:"voice,omitempty"`
	Role       string             `json:"role,omitempty"`
	Set        string             `json:"set,omitempty"`
	ItemID     string             `json:"item_id,omitempty"`
	Attempt    int                `json:"attempt"`
	Stage      string             `json:"stage,omitempty"` // E2E: the stage that failed
	Values     map[string]float64 `json:"values,omitempty"`
	Flags      map[string]bool    `json:"flags,omitempty"`
	ErrorKind  string             `json:"error_kind,omitempty"` // a vendor failure: the attempt has no measurements
}

func newResult(suite string, c Candidate, it Item, attempt int) Result {
	return Result{
		Suite: suite, Capability: string(c.Capability), Provider: string(c.Provider), Model: c.Model, Voice: c.Voice, Role: c.Role,
		Set: it.Set, ItemID: it.ID, Attempt: attempt, Values: map[string]float64{}, Flags: map[string]bool{},
	}
}

// failed turns r into an error row: the measurements are dropped, the kind stays.
func (r Result) failed(err error) Result {
	r.Values, r.Flags = nil, nil
	r.ErrorKind = string(provider.KindOf(err))
	return r
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Option tunes a runner.
type Option func(*runOpts)

type runOpts struct {
	pacing       bool
	timeout      time.Duration
	maxTokens    int
	temperature  float64
	system       string
	turnEndMs    int
	jsonMaxToken int
}

func newRunOpts(opts []Option) runOpts {
	o := runOpts{timeout: 30 * time.Second, maxTokens: 300, temperature: 0.8, jsonMaxToken: 4000}
	for _, f := range opts {
		f(&o)
	}
	return o
}

// WithPacing sends STT audio in real time, one 20 ms frame per 20 ms.
func WithPacing(on bool) Option { return func(o *runOpts) { o.pacing = on } }

// WithTimeout bounds one attempt (STT: until the FINAL of Finalize; LLM/TTS/JSON: the whole call).
func WithTimeout(d time.Duration) Option { return func(o *runOpts) { o.timeout = d } }

// WithLLMParams sets max output tokens and temperature of the conversation calls (the values of
// LLM_MAX_OUTPUT_TOKENS and LLM_TEMPERATURE).
func WithLLMParams(maxTokens int, temperature float64) Option {
	return func(o *runOpts) { o.maxTokens, o.temperature = maxTokens, temperature }
}

// WithSystemPrompt sets the system message of the e2e LLM stage.
func WithSystemPrompt(s string) Option { return func(o *runOpts) { o.system = s } }

// WithTurnEndMs is the turn-end delay added to the e2e latency (no VAD or turn engine runs).
func WithTurnEndMs(n int) Option { return func(o *runOpts) { o.turnEndMs = n } }

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sentenceEnd reports the byte index just after the first 。！？ in s, or -1.
func sentenceEnd(s string) int {
	i := strings.IndexAny(s, "。！？")
	if i < 0 {
		return -1
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return i + size
}
