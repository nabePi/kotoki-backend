package bench

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// modelsFile is bench/models.yaml: the candidates per capability (spec 8.10.1).
type modelsFile struct {
	Candidates []struct {
		Capability string   `yaml:"capability"`
		Provider   string   `yaml:"provider"`
		Model      string   `yaml:"model"`
		Voice      string   `yaml:"voice"`
		Role       string   `yaml:"role"`
		Characters []string `yaml:"characters"`
	} `yaml:"candidates"`
}

// vendorsOf lists the vendors a capability may have as a candidate (D-017); FAKE_PRIMARY is for the
// dry run with the fakes.
var vendorsOf = map[provider.Capability][]provider.Name{
	provider.CapSTT: {provider.OpenAI, provider.ElevenLabs, provider.Cartesia, provider.FakePrimary},
	provider.CapLLM: {provider.OpenAI, provider.DeepSeek, provider.FakePrimary},
	provider.CapTTS: {provider.ElevenLabs, provider.Cartesia, provider.FakePrimary},
}

// LoadModels parses bench/models.yaml (unknown fields rejected). capability and provider are
// UPPERCASE; a vendor outside the D-017 set for the capability is rejected; model is required; TTS
// needs voice and role (PRIMARY|FALLBACK), STT and LLM take neither voice nor role; a duplicate
// candidate is rejected. Model names are taken as written, never guessed.
func LoadModels(path string) ([]Candidate, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-chosen file (--models-file)
	if err != nil {
		return nil, fmt.Errorf("bench: models: %w", err)
	}
	var f modelsFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("bench: models: %w", err)
	}
	var out []Candidate
	seen := map[string]bool{}
	for i, e := range f.Candidates {
		c := Candidate{Capability: provider.Capability(e.Capability), Provider: provider.Name(e.Provider), Model: e.Model, Voice: e.Voice, Role: e.Role, Characters: e.Characters}
		at := fmt.Sprintf("bench: models: candidate %d", i+1)
		vendors, ok := vendorsOf[c.Capability]
		if !ok {
			return nil, fmt.Errorf("%s: capability %q is not STT|LLM|TTS (UPPERCASE)", at, e.Capability)
		}
		if !slices.Contains(vendors, c.Provider) {
			return nil, fmt.Errorf("%s: provider %q is not a vendor of %s (%s; UPPERCASE)", at, e.Provider, c.Capability, joinNames(vendors))
		}
		if strings.TrimSpace(c.Model) == "" {
			return nil, fmt.Errorf("%s: model is required", at)
		}
		if c.Capability == provider.CapTTS {
			if c.Voice == "" {
				return nil, fmt.Errorf("%s: voice is required for TTS", at)
			}
			if c.Role != RolePrimary && c.Role != RoleFallback {
				return nil, fmt.Errorf("%s: role %q is not PRIMARY|FALLBACK", at, c.Role)
			}
			for _, ch := range c.Characters {
				if ch == "" || ch != strings.ToUpper(ch) {
					return nil, fmt.Errorf("%s: character code %q must be UPPERCASE", at, ch)
				}
			}
		} else if c.Voice != "" || c.Role != "" || len(c.Characters) > 0 {
			return nil, fmt.Errorf("%s: voice, role and characters belong to TTS only", at)
		}
		key := string(c.Capability) + "|" + c.Key() + "|" + c.Role
		if seen[key] {
			return nil, fmt.Errorf("%s: duplicate candidate %s", at, c.Key())
		}
		seen[key] = true
		out = append(out, c)
	}
	return out, nil
}

func joinNames(ns []provider.Name) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = string(n)
	}
	return strings.Join(s, "|")
}
