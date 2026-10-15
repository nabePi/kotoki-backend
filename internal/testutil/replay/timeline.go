// Package replay scripts the STT side of realtime tests: a Timeline (the §14.5
// <fixture>.partials.json format) played by ScriptedSTT against the audio time it is fed.
// It is cgo-free; reading fixture audio is internal/testutil/audiofx's job.
package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Step is one scripted STT event, due once the total audio sent reaches AtMs: a PARTIAL with
// Text (the whole running segment), or for a Final step a FINAL that closes the segment.
type Step struct {
	AtMs  int64  `json:"at_ms"`
	Text  string `json:"text"`
	Final bool   `json:"final,omitempty"`
}

// Timeline is a list of steps with non-decreasing AtMs.
type Timeline []Step

// LoadTimeline reads a timeline JSON file: one array of {at_ms, text, final}; unknown keys,
// trailing data, a negative or backwards at_ms and an empty partial text are rejected (an
// empty FINAL is allowed).
func LoadTimeline(path string) (Timeline, error) {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		return nil, fmt.Errorf("replay: timeline: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var tl Timeline
	if err := dec.Decode(&tl); err != nil {
		return nil, fmt.Errorf("replay: timeline %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("replay: timeline %s: data after the array", path)
	}
	if err := tl.validate(); err != nil {
		return nil, fmt.Errorf("replay: timeline %s: %w", path, err)
	}
	return tl, nil
}

func (tl Timeline) validate() error {
	var prev int64
	for i, s := range tl {
		if s.AtMs < 0 {
			return fmt.Errorf("step %d: at_ms %d < 0", i, s.AtMs)
		}
		if s.Text == "" && !s.Final {
			return fmt.Errorf("step %d: empty text on a partial step", i)
		}
		if s.AtMs < prev {
			return fmt.Errorf("step %d: at_ms %d before %d", i, s.AtMs, prev)
		}
		prev = s.AtMs
	}
	return nil
}
