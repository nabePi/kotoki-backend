package evaluation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	// maxLLMCalls is the per-delivery ceiling: the main call plus one repair (D-017).
	maxLLMCalls = 2
	// repairPreviousMaxRunes bounds the previous output echoed in the repair message.
	repairPreviousMaxRunes = 8000
	maxRepairProblems      = 20
)

// LLMInput is what one evaluation needs: the prompt version in use, its rendered messages and the
// user turns the findings are checked against.
type LLMInput struct {
	SessionID uuid.UUID
	PV        content.PromptVersion
	Rendered  prompt.Rendered
	Turns     []TurnRef
}

// LLMResult is a validated evaluation: Findings are filtered, Provider/Model come from the call
// that passed the validator and the token counts sum all calls.
type LLMResult struct {
	Output                    EvalOutput
	Findings                  []Finding
	Provider                  provider.Name
	Model                     string
	CallCount                 int
	InputTokens, OutputTokens int
	Dropped                   map[string]int
}

// LLMEvaluator runs the main call and at most one repair against the active vendor (no vendor
// fallback). Provider errors are returned untouched so the delivery retry policy decides.
type LLMEvaluator struct {
	ev            provider.EvaluationProvider
	schemas       *SchemaCache
	metrics       *Metrics
	minConfidence float64
}

// NewLLMEvaluator returns an evaluator over ev (AsyncRouter.Evaluation()). m may be nil.
func NewLLMEvaluator(ev provider.EvaluationProvider, schemas *SchemaCache, m *Metrics, minConfidence float64) *LLMEvaluator {
	return &LLMEvaluator{ev: ev, schemas: schemas, metrics: m, minConfidence: minConfidence}
}

// BuildStructuredRequest builds the request of one call; llm_params in the prompt version win over
// the code defaults (max_output_tokens 4000, temperature 0, seed 42).
func BuildStructuredRequest(sessionID uuid.UUID, pv content.PromptVersion, msgs []provider.Message) provider.StructuredRequest {
	p := pv.LLMParams
	seed := p.SeedOr(42)
	return provider.StructuredRequest{
		Task:            "EVALUATION",
		Messages:        msgs,
		SchemaName:      fmt.Sprintf("kotoki_evaluation_rubric_v%d", pv.Version),
		JSONSchema:      pv.OutputSchema,
		MaxOutputTokens: p.MaxOutputTokensOr(4000),
		Temperature:     p.TemperatureOr(0),
		Seed:            &seed,
		RequestID:       "EVALUATION:" + sessionID.String(),
	}
}

// RepairMessages appends the rejected output (cut to 8,000 characters; omitted when there is none)
// and the validator errors to base.
func RepairMessages(base []provider.Message, previous string, problems []string) []provider.Message {
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

func baseMessages(r prompt.Rendered) []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleSystem, Content: r.System}}
	if r.User != "" {
		msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: r.User})
	}
	return msgs
}

// Evaluate runs the call sequence (spec 10.6.1): call 1; when its output is rejected (including
// INVALID_RESPONSE) call 2 with repair messages; a second rejection is ErrInvalidModelOutput. Any
// other provider error is returned as is after one call.
func (e *LLMEvaluator) Evaluate(ctx context.Context, in LLMInput) (LLMResult, error) {
	schema, err := e.schemas.Get(in.PV.ID, in.PV.OutputSchema)
	if err != nil {
		return LLMResult{}, err
	}
	base := baseMessages(in.Rendered)
	msgs := base
	res := LLMResult{}
	var lastProblems []string

	for call := 1; call <= maxLLMCalls; call++ {
		out, err := e.ev.GenerateJSON(ctx, BuildStructuredRequest(in.SessionID, in.PV, msgs))
		res.CallCount++
		previous := ""
		var problems []string
		var filtered FilterResult
		var parsed EvalOutput

		switch {
		case err != nil && provider.KindOf(err) == provider.KindInvalidResponse:
			e.metrics.LLMCall(string(e.ev.Name()), "invalid")
			problems = []string{"the response was empty, truncated or not valid JSON"}
		case err != nil:
			e.metrics.LLMCall(string(e.ev.Name()), "error")
			return LLMResult{}, err
		default:
			res.InputTokens += out.Usage.InputTokens
			res.OutputTokens += out.Usage.OutputTokens
			previous = string(out.JSON)
			parsed, filtered, problems = e.validate(schema, in, out.JSON)
			if len(problems) > 0 {
				e.metrics.LLMCall(string(out.Provider), "invalid")
			}
		}

		if len(problems) == 0 {
			e.metrics.LLMCall(string(out.Provider), "valid")
			res.Output, res.Findings, res.Dropped = parsed, filtered.Kept, filtered.Dropped
			res.Provider, res.Model = out.Provider, out.Model
			return res, nil
		}
		lastProblems = problems
		msgs = RepairMessages(base, previous, problems)
	}
	return LLMResult{}, &OutputError{Problems: lastProblems}
}

// validate applies schema, language and finding rules to raw; it returns the problems that make
// the output invalid (none when accepted).
func (e *LLMEvaluator) validate(schema *jsonschema.Schema, in LLMInput, raw []byte) (EvalOutput, FilterResult, []string) {
	out, err := ParseOutput(schema, raw)
	if err != nil {
		var oe *OutputError
		if errors.As(err, &oe) {
			return EvalOutput{}, FilterResult{}, capProblems(oe.Problems)
		}
		return EvalOutput{}, FilterResult{}, []string{err.Error()}
	}
	if p := CheckLanguage(out); len(p) > 0 {
		return EvalOutput{}, FilterResult{}, capProblems(p)
	}
	fr := FilterFindings(out.Findings, in.Turns, e.minConfidence)
	for reason, n := range fr.Dropped {
		for range n {
			e.metrics.FindingDropped(reason)
		}
	}
	if fr.TooManyRejected() {
		return EvalOutput{}, fr, []string{tooManyRejectedProblem(fr)}
	}
	return out, fr, nil
}

func capProblems(p []string) []string {
	if len(p) > maxRepairProblems {
		return p[:maxRepairProblems]
	}
	return p
}

// tooManyRejectedProblem describes the rejection without quoting any finding text.
func tooManyRejectedProblem(fr FilterResult) string {
	reasons := make([]string, 0, len(fr.Dropped))
	for r, n := range fr.Dropped {
		reasons = append(reasons, fmt.Sprintf("%s=%d", r, n))
	}
	sort.Strings(reasons)
	return fmt.Sprintf("findings: %d of %d findings violate the rules (%s); each finding must follow its type rules and quote the user's words exactly",
		fr.Rejected, fr.Total, strings.Join(reasons, ", "))
}
