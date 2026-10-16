package prompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"
)

// Params are the LLM parameters of a prompt version (prompt_versions.llm_params). A value from
// the database wins over the code default (S-1).
type Params struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	Seed            *int64   `json:"seed,omitempty"`
}

// TemperatureOr returns the stored temperature or def.
func (p Params) TemperatureOr(def float64) float64 {
	if p.Temperature != nil {
		return *p.Temperature
	}
	return def
}

// MaxOutputTokensOr returns the stored limit or def.
func (p Params) MaxOutputTokensOr(def int) int {
	if p.MaxOutputTokens != nil {
		return *p.MaxOutputTokens
	}
	return def
}

// SeedOr returns the stored seed or def.
func (p Params) SeedOr(def int64) int64 {
	if p.Seed != nil {
		return *p.Seed
	}
	return def
}

// Template is a prompt version as the renderer needs it (content.PromptVersion.Template()
// builds it, so prompt never imports content). User == "" means no user message.
type Template struct {
	Key          string
	Version      int
	System, User string
	Params       Params
	OutputSchema json.RawMessage
}

// Rendered is the result: a system message and, when the template has one, a user message.
type Rendered struct {
	System string
	User   string // "" when the template has no user part
}

func parse(name, text string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Funcs(funcMap).Parse(text)
}

func execute(t *template.Template, data any) (string, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Render executes the system (and user) template against data. data must be a typed struct:
// a misspelled or snake_case placeholder fails instead of rendering empty. Errors name the
// template and the offending placeholder, never data values.
func Render(t Template, data any) (Rendered, error) {
	id := fmt.Sprintf("%s v%d", t.Key, t.Version)
	sys, err := parse(id+" system", t.System)
	if err != nil {
		return Rendered{}, fmt.Errorf("prompt: parse %s system_template: %w", id, err)
	}
	out := Rendered{}
	if out.System, err = execute(sys, data); err != nil {
		return Rendered{}, fmt.Errorf("prompt: render %s system_template: %w", id, err)
	}
	if t.User == "" {
		return out, nil
	}
	user, err := parse(id+" user", t.User)
	if err != nil {
		return Rendered{}, fmt.Errorf("prompt: parse %s user_template: %w", id, err)
	}
	if out.User, err = execute(user, data); err != nil {
		return Rendered{}, fmt.Errorf("prompt: render %s user_template: %w", id, err)
	}
	return out, nil
}
