package bench

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var schemaNameRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// SchemaDir resolves and validates the LLM-json items from a directory of JSON Schema files
// (`<schema_name>.json`). An item names its task and schema by tags, `task:EVALUATION` and
// `schema:kotoki_evaluation_rubric_v1`. This is the code-side validation of the structured output:
// valid JSON that satisfies the schema. Problems are reported as instance paths, never values (the
// output contains the learner's words).
type SchemaDir struct {
	dir string
	mu  sync.Mutex
	raw map[string][]byte
	sch map[string]*jsonschema.Schema
}

// NewSchemaDir returns a resolver over dir.
func NewSchemaDir(dir string) *SchemaDir {
	return &SchemaDir{dir: dir, raw: map[string][]byte{}, sch: map[string]*jsonschema.Schema{}}
}

func tagValue(it Item, prefix string) string {
	for _, t := range it.Tags {
		if v, ok := strings.CutPrefix(t, prefix); ok {
			return v
		}
	}
	return ""
}

func (d *SchemaDir) load(name string) (*jsonschema.Schema, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sch[name]; ok {
		return s, d.raw[name], nil
	}
	if !schemaNameRE.MatchString(name) {
		return nil, nil, fmt.Errorf("schema name %q is not [a-z0-9_]{1,64}", name)
	}
	b, err := os.ReadFile(filepath.Join(d.dir, name+".json")) //nolint:gosec // name is restricted to [a-z0-9_]
	if err != nil {
		return nil, nil, fmt.Errorf("schema %s: %w", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, nil, fmt.Errorf("schema %s is not JSON: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, nil, fmt.Errorf("schema %s: %w", name, err)
	}
	s, err := c.Compile("schema.json")
	if err != nil {
		return nil, nil, fmt.Errorf("schema %s: %w", name, err)
	}
	d.sch[name], d.raw[name] = s, b
	return s, b, nil
}

// Resolve is the `resolve` argument of RunLLMJSON.
func (d *SchemaDir) Resolve(it Item) (JSONSpec, error) {
	task, name := tagValue(it, "task:"), tagValue(it, "schema:")
	if task != "EVALUATION" && task != "MEMORY_EXTRACTION" {
		return JSONSpec{}, fmt.Errorf("item %s: tag task:EVALUATION or task:MEMORY_EXTRACTION is required", it.ID)
	}
	if name == "" {
		return JSONSpec{}, fmt.Errorf("item %s: tag schema:<name> is required", it.ID)
	}
	_, raw, err := d.load(name)
	if err != nil {
		return JSONSpec{}, fmt.Errorf("item %s: %w", it.ID, err)
	}
	return JSONSpec{Task: task, SchemaName: name, Schema: raw}, nil
}

// Validate is the `validate` argument of RunLLMJSON; one problem per line.
func (d *SchemaDir) Validate(name string, raw []byte) error {
	s, _, err := d.load(name)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return errors.New("output is not valid JSON")
	}
	verr := s.Validate(inst)
	if verr == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return errors.New("output does not satisfy the schema")
	}
	var paths []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			paths = append(paths, "/"+strings.Join(e.InstanceLocation, "/")+": violates the schema")
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(paths) > 8 {
		paths = paths[:8]
	}
	if len(paths) == 0 {
		paths = []string{"output does not satisfy the schema"}
	}
	return errors.New(strings.Join(paths, "\n"))
}
