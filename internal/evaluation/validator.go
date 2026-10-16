package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// ErrInvalidModelOutput marks an LLM output that violates the output schema, the language rule or
// the finding rules; the caller may repair once and then fails with INVALID_MODEL_OUTPUT.
var ErrInvalidModelOutput = errors.New("evaluation: invalid model output")

// OutputError lists what is wrong with an LLM output; errors.Is(err, ErrInvalidModelOutput) holds.
type OutputError struct{ Problems []string }

// Error implements error.
func (e *OutputError) Error() string {
	return ErrInvalidModelOutput.Error() + ": " + strings.Join(e.Problems, "; ")
}

// Unwrap makes errors.Is(err, ErrInvalidModelOutput) true.
func (e *OutputError) Unwrap() error { return ErrInvalidModelOutput }

// SchemaCache compiles output schemas once per prompt_version_id.
type SchemaCache struct {
	mu sync.Mutex
	m  map[uuid.UUID]*jsonschema.Schema
}

// NewSchemaCache returns an empty cache.
func NewSchemaCache() *SchemaCache { return &SchemaCache{m: map[uuid.UUID]*jsonschema.Schema{}} }

// Get returns the compiled schema of prompt version id, compiling it on first use.
func (c *SchemaCache) Get(id uuid.UUID, schema json.RawMessage) (*jsonschema.Schema, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.m[id]; ok {
		return s, nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("evaluation: parse output schema: %w", err)
	}
	comp := jsonschema.NewCompiler()
	comp.DefaultDraft(jsonschema.Draft2020)
	if err := comp.AddResource("output_schema.json", doc); err != nil {
		return nil, fmt.Errorf("evaluation: add output schema: %w", err)
	}
	s, err := comp.Compile("output_schema.json")
	if err != nil {
		return nil, fmt.Errorf("evaluation: compile output schema: %w", err)
	}
	c.m[id] = s
	return s, nil
}

// ParseOutput validates raw against schema and decodes it. Non-integer scores are rounded
// (math.Round) before validation; unknown properties are rejected by the schema and again by the
// strict decoder. Every failure is an *OutputError.
func ParseOutput(schema *jsonschema.Schema, raw json.RawMessage) (EvalOutput, error) {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return EvalOutput{}, &OutputError{Problems: []string{"output is not valid JSON"}}
	}
	normalizeNumbers(inst)
	if err := schema.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return EvalOutput{}, &OutputError{Problems: schemaProblems(ve)}
		}
		return EvalOutput{}, &OutputError{Problems: []string{err.Error()}}
	}
	b, err := json.Marshal(inst)
	if err != nil {
		return EvalOutput{}, &OutputError{Problems: []string{err.Error()}}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var out EvalOutput
	if err := dec.Decode(&out); err != nil {
		return EvalOutput{}, &OutputError{Problems: []string{err.Error()}}
	}
	return out, nil
}

// normalizeNumbers rounds scores/signals scores and turns integral turn_seq numbers (e.g. 3.0) into
// plain integers so the strict decoder accepts them.
func normalizeNumbers(inst any) {
	root, ok := inst.(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"scores", "signals"} {
		group, _ := root[key].(map[string]any)
		for _, v := range group {
			if m, ok := v.(map[string]any); ok {
				roundField(m, "score", true)
			}
		}
	}
	findings, _ := root["findings"].([]any)
	for _, f := range findings {
		if m, ok := f.(map[string]any); ok {
			roundField(m, "turn_seq", false)
		}
	}
}

func roundField(m map[string]any, key string, round bool) {
	n, ok := m[key].(json.Number)
	if !ok {
		return
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return
	}
	if round {
		f = math.Round(f)
	} else if f != math.Trunc(f) {
		return
	}
	m[key] = json.Number(strconv.FormatFloat(f, 'f', 0, 64))
}

func schemaProblems(ve *jsonschema.ValidationError) []string {
	p := message.NewPrinter(language.English)
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			out = append(out, loc+": "+e.ErrorKind.LocalizedString(p))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) == 0 {
		out = []string{ve.Error()}
	}
	return out
}

// CheckLanguage returns the language problems of out: summary, the five comments and every finding
// explanation must be Indonesian (spec 10.7). A text is valid when, after removing 「…」 segments and
// Han/Hiragana/Katakana runes, it has at least one Latin letter and at least half of the remaining
// runes are Latin letters, digits, whitespace or punctuation.
func CheckLanguage(out EvalOutput) []string {
	var problems []string
	check := func(field, text string) {
		if !isIndonesian(text) {
			problems = append(problems, field+": not written in Indonesian")
		}
	}
	check("summary", out.Summary)
	for _, d := range LLMDimensions {
		check("scores."+string(d)+".comment", out.Dimensions()[d].Comment)
	}
	for i, f := range out.Findings {
		check(fmt.Sprintf("findings[%d].explanation", i), f.Explanation)
	}
	return problems
}

func isJapaneseRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) || r == 'ー'
}

func isIndonesian(s string) bool {
	var rest []rune
	quoted := false
	for _, r := range s {
		switch {
		case r == '「':
			quoted = true
		case r == '」':
			quoted = false
		case quoted || isJapaneseRune(r):
		default:
			rest = append(rest, r)
		}
	}
	latin, ok := 0, 0
	for _, r := range rest {
		switch {
		case unicode.Is(unicode.Latin, r) && unicode.IsLetter(r):
			latin++
			ok++
		case unicode.IsDigit(r) || unicode.IsSpace(r) || unicode.IsPunct(r):
			ok++
		}
	}
	return latin >= 1 && ok*2 >= len(rest)
}
