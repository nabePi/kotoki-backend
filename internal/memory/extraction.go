package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInvalidOutput marks an LLM output that is not JSON, violates the output schema or does not
// decode into ExtractionOutput; the worker may repair once and then dead-letters the message.
var ErrInvalidOutput = errors.New("memory: invalid extraction output")

// RawCandidate is one memory exactly as the LLM returned it (spec 11.5.2 schema).
type RawCandidate struct {
	Category       string  `json:"category"`
	Key            string  `json:"key"`
	ContentJA      string  `json:"content_ja"`
	DisplayTextID  string  `json:"display_text_id"`
	EvidenceType   string  `json:"evidence_type"`
	Confidence     float64 `json:"confidence"`
	Importance     float64 `json:"importance"`
	IsTemporary    bool    `json:"is_temporary"`
	ValidDays      *int    `json:"valid_days"`
	SourceTurnSeqs []int   `json:"source_turn_seqs"`
	Sensitive      bool    `json:"sensitive"`
}

// RawEpisode is the episode summary of the LLM output.
type RawEpisode struct {
	ContentJA      string  `json:"content_ja"`
	DisplayTextID  string  `json:"display_text_id"`
	Confidence     float64 `json:"confidence"`
	Importance     float64 `json:"importance"`
	SourceTurnSeqs []int   `json:"source_turn_seqs"`
}

// ExtractionOutput is the decoded LLM output; Episode is nil when the LLM returned null.
type ExtractionOutput struct {
	Memories []RawCandidate `json:"memories"`
	Episode  *RawEpisode    `json:"episode"`
}

// ParseOutput validates raw against the JSON Schema (draft 2020-12) of the prompt version and
// decodes it strictly (unknown fields are errors; trailing data fails the JSON parse). Every failure wraps
// ErrInvalidOutput; messages name schema locations, never the model's text.
func ParseOutput(raw []byte, schema json.RawMessage) (ExtractionOutput, error) {
	compiled, err := compileSchema(schema)
	if err != nil {
		return ExtractionOutput{}, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return ExtractionOutput{}, fmt.Errorf("%w: not JSON", ErrInvalidOutput)
	}
	if err := compiled.Validate(inst); err != nil {
		return ExtractionOutput{}, fmt.Errorf("%w: schema violation at %s", ErrInvalidOutput, violationPaths(err))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out ExtractionOutput
	if err := dec.Decode(&out); err != nil {
		return ExtractionOutput{}, fmt.Errorf("%w: decode failed", ErrInvalidOutput)
	}
	return out, nil
}

// compileSchema failures are configuration errors, not model errors: they do not wrap
// ErrInvalidOutput.
func compileSchema(schema json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("memory: output_schema is not JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("memory_output_schema.json", doc); err != nil {
		return nil, fmt.Errorf("memory: output_schema: %w", err)
	}
	s, err := c.Compile("memory_output_schema.json")
	if err != nil {
		return nil, fmt.Errorf("memory: output_schema: %w", err)
	}
	return s, nil
}

// violationPaths lists the instance locations of the leaf violations (never the values).
func violationPaths(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return "/"
	}
	var paths []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			paths = append(paths, "/"+strings.Join(e.InstanceLocation, "/"))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(paths) > 5 {
		paths = paths[:5]
	}
	return strings.Join(paths, ", ")
}
