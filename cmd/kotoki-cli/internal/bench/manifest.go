// Package bench is the core of `kotoki-cli provider-bench` (spec 8.10): the dataset manifest, the
// metrics (CER, percentiles, format check), the hard gates of 8.10.4, the cost model of 8.10.3, the
// recommendation of STT, LLM and per-character TTS voices (recommendation only: nothing here picks a
// vendor or writes an *_ENABLED switch for the product, D-017) and the suite runners. It depends on
// the provider ports, never on a vendor package, so it is tested with the fakes.
package bench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Dataset sets (spec 8.10.2).
const (
	SetSTTNative    = "STT-native"
	SetSTTLearner   = "STT-learner"
	SetSTTNoise     = "STT-noise"
	SetLLMContext   = "LLM-context"
	SetLLMJSON      = "LLM-json"
	SetTTSSentences = "TTS-sentences"
)

// Speakers of the STT sets.
const (
	SpeakerNative  = "native"
	SpeakerLearner = "learner"
)

var validSets = []string{SetSTTNative, SetSTTLearner, SetSTTNoise, SetLLMContext, SetLLMJSON, SetTTSSentences}

// Item is one manifest line (spec 8.10.2). Audio lives in a private bucket, never in the repo;
// ReferenceText is the transcript of an STT item and the sentence of a TTS item. Messages is the
// prompt of the LLM sets.
type Item struct {
	ID            string             `json:"id"`
	Set           string             `json:"set"`
	AudioURI      string             `json:"audio_uri,omitempty"`
	ReferenceText string             `json:"reference_text,omitempty"`
	Speaker       string             `json:"speaker,omitempty"`
	DurationMs    int64              `json:"duration_ms,omitempty"`
	Tags          []string           `json:"tags,omitempty"`
	Messages      []provider.Message `json:"messages,omitempty"`
}

// IsSTT reports whether the item belongs to one of the STT sets.
func (it Item) IsSTT() bool {
	return it.Set == SetSTTNative || it.Set == SetSTTLearner || it.Set == SetSTTNoise
}

// LoadManifest reads a JSONL manifest. It rejects an unknown set, a speaker outside native|learner
// (an STT item must name one, and STT-native/STT-learner must match their set), a duplicate or empty
// id, an unknown field, and an item without what its set needs (audio and transcript for STT,
// messages for the LLM sets, the sentence for TTS). Blank lines are skipped.
func LoadManifest(path string) ([]Item, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-chosen manifest path (--dataset)
	if err != nil {
		return nil, fmt.Errorf("bench: manifest: %w", err)
	}
	var items []Item
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var it Item
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&it); err != nil {
			return nil, fmt.Errorf("bench: manifest line %d: %w", line, err)
		}
		if err := it.validate(); err != nil {
			return nil, fmt.Errorf("bench: manifest line %d (%q): %w", line, it.ID, err)
		}
		if first, dup := seen[it.ID]; dup {
			return nil, fmt.Errorf("bench: manifest line %d: duplicate id %q (first on line %d)", line, it.ID, first)
		}
		seen[it.ID] = line
		items = append(items, it)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("bench: manifest: %w", err)
	}
	return items, nil
}

func (it Item) validate() error {
	if strings.TrimSpace(it.ID) == "" {
		return fmt.Errorf("empty id")
	}
	if !slices.Contains(validSets, it.Set) {
		return fmt.Errorf("set %q is not one of %s", it.Set, strings.Join(validSets, "|"))
	}
	if it.Speaker != "" && it.Speaker != SpeakerNative && it.Speaker != SpeakerLearner {
		return fmt.Errorf("speaker %q is not native|learner", it.Speaker)
	}
	switch it.Set {
	case SetSTTNative, SetSTTLearner, SetSTTNoise:
		if it.Speaker == "" {
			return fmt.Errorf("set %s needs a speaker (native|learner)", it.Set)
		}
		if (it.Set == SetSTTNative && it.Speaker != SpeakerNative) || (it.Set == SetSTTLearner && it.Speaker != SpeakerLearner) {
			return fmt.Errorf("set %s does not take speaker %q", it.Set, it.Speaker)
		}
		if it.AudioURI == "" || it.ReferenceText == "" {
			return fmt.Errorf("set %s needs audio_uri and reference_text", it.Set)
		}
	case SetLLMContext, SetLLMJSON:
		if len(it.Messages) == 0 {
			return fmt.Errorf("set %s needs messages", it.Set)
		}
	case SetTTSSentences:
		if it.ReferenceText == "" {
			return fmt.Errorf("set %s needs reference_text (the sentence)", it.Set)
		}
	}
	return nil
}

// ItemsOf returns the items of the given sets, in manifest order.
func ItemsOf(items []Item, sets ...string) []Item {
	var out []Item
	for _, it := range items {
		if slices.Contains(sets, it.Set) {
			out = append(out, it)
		}
	}
	return out
}
