// Package pricing loads the vendor price table (embedded YAML, overridable by file) and
// estimates USD cost. It implements provider.PriceTable; all prices are 0 until the
// benchmark task fills them from the vendors' official pricing pages.
package pricing

import (
	"bytes"
	_ "embed" // prices.yaml
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

//go:embed prices.yaml
var embedded []byte

const wildcard = "*"

type entry struct {
	Provider        string   `yaml:"provider"`
	Capability      string   `yaml:"capability"`
	Model           string   `yaml:"model"`
	InputPer1M      float64  `yaml:"input_per_1m_tokens"`
	InputCachePer1M *float64 `yaml:"input_cache_hit_per_1m_tokens"`
	OutputPer1M     float64  `yaml:"output_per_1m_tokens"`
	PerAudioHour    *float64 `yaml:"per_audio_hour"`
	PerMillionChars *float64 `yaml:"per_1m_characters"`
}

type file struct {
	Version  string  `yaml:"version"`
	Currency string  `yaml:"currency"`
	Entries  []entry `yaml:"entries"`
}

type key struct {
	p     provider.Name
	c     provider.Capability
	model string
}

// Table is an immutable price table.
type Table struct {
	version string
	byKey   map[key]entry
}

// Load reads the price table at path; "" loads the embedded one. Unknown fields,
// lowercase provider/capability, empty models, negative prices and duplicates are errors.
func Load(path string) (*Table, error) {
	raw := embedded
	if path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // operator-provided ConfigMap path (PROVIDER_PRICES_FILE)
		if err != nil {
			return nil, fmt.Errorf("pricing: read %s: %w", path, err)
		}
		raw = b
	}
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("pricing: parse: %w", err)
	}
	t := &Table{version: f.Version, byKey: make(map[key]entry, len(f.Entries))}
	for i, e := range f.Entries {
		if e.Provider != strings.ToUpper(e.Provider) || e.Provider == "" {
			return nil, fmt.Errorf("pricing: entry %d: provider %q must be UPPERCASE", i, e.Provider)
		}
		if e.Capability != strings.ToUpper(e.Capability) || e.Capability == "" {
			return nil, fmt.Errorf("pricing: entry %d: capability %q must be UPPERCASE", i, e.Capability)
		}
		if e.Model == "" {
			return nil, fmt.Errorf("pricing: entry %d: model must not be empty", i)
		}
		for _, p := range []float64{e.InputPer1M, e.OutputPer1M, deref(e.InputCachePer1M), deref(e.PerAudioHour), deref(e.PerMillionChars)} {
			if p < 0 {
				return nil, fmt.Errorf("pricing: entry %d (%s/%s/%s): negative price", i, e.Provider, e.Capability, e.Model)
			}
		}
		k := key{provider.Name(e.Provider), provider.Capability(e.Capability), e.Model}
		if _, dup := t.byKey[k]; dup {
			return nil, fmt.Errorf("pricing: duplicate entry %s/%s/%s", e.Provider, e.Capability, e.Model)
		}
		t.byKey[k] = e
	}
	if len(t.byKey) == 0 {
		return nil, errors.New("pricing: no entries")
	}
	return t, nil
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// Version is the table's version string.
func (t *Table) Version() string { return t.version }

// find prefers the exact model, then the vendor/capability wildcard.
func (t *Table) find(p provider.Name, c provider.Capability, model string) (entry, bool) {
	if e, ok := t.byKey[key{p, c, model}]; ok {
		return e, true
	}
	e, ok := t.byKey[key{p, c, wildcard}]
	return e, ok
}

// LLM prices a token-billed call (LLM, EVALUATION, EMBEDDING). Cached input tokens use
// the cache-hit price when the entry has one.
func (t *Table) LLM(p provider.Name, c provider.Capability, model string, u provider.TokenUsage) (float64, bool) {
	e, ok := t.find(p, c, model)
	if !ok {
		return 0, false
	}
	cached := min(max(u.CachedInputTokens, 0), u.InputTokens)
	plain := u.InputTokens - cached
	usd := float64(plain) / 1e6 * e.InputPer1M
	if e.InputCachePer1M != nil {
		usd += float64(cached) / 1e6 * *e.InputCachePer1M
	} else {
		usd += float64(cached) / 1e6 * e.InputPer1M
	}
	usd += float64(u.OutputTokens) / 1e6 * e.OutputPer1M
	return usd, true
}

// TTS prices a synthesis by characters; false when the entry is not priced per character.
func (t *Table) TTS(p provider.Name, model string, characters int) (float64, bool) {
	e, ok := t.find(p, provider.CapTTS, model)
	if !ok || e.PerMillionChars == nil {
		return 0, false
	}
	return float64(characters) / 1e6 * *e.PerMillionChars, true
}

// Audio prices audio time (STT, or TTS billed per audio hour).
func (t *Table) Audio(p provider.Name, c provider.Capability, model string, audioMs int64) (float64, bool) {
	e, ok := t.find(p, c, model)
	if !ok || e.PerAudioHour == nil {
		return 0, false
	}
	return float64(audioMs) / 3.6e6 * *e.PerAudioHour, true
}

func (e entry) free() bool {
	return e.InputPer1M == 0 && e.OutputPer1M == 0 && deref(e.InputCachePer1M) == 0 &&
		deref(e.PerAudioHour) == 0 && deref(e.PerMillionChars) == 0
}

// Missing returns the configured combinations that have no entry or a price of 0, for the
// startup WARN "provider price missing".
func (t *Table) Missing(configured []provider.ModelRef) []provider.ModelRef {
	var out []provider.ModelRef
	for _, m := range configured {
		if e, ok := t.find(m.Provider, m.Capability, m.Model); !ok || e.free() {
			out = append(out, m)
		}
	}
	return out
}
