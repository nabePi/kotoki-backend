// Package replay scripts the STT side of realtime tests: a Timeline (the §14.5
// <fixture>.partials.json format) played by ScriptedSTT against the audio time it is fed.
// It is cgo-free; reading fixture audio is internal/testutil/audiofx's job.
package replay

import (
	"encoding/json"
	"fmt"
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

// LoadTimeline reads a timeline JSON file (array of {at_ms, text, final}).
func LoadTimeline(path string) (Timeline, error) {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		return nil, fmt.Errorf("replay: timeline: %w", err)
	}
	var tl Timeline
	if err := json.Unmarshal(b, &tl); err != nil {
		return nil, fmt.Errorf("replay: timeline %s: %w", path, err)
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
		if s.AtMs < prev {
			return fmt.Errorf("step %d: at_ms %d before %d", i, s.AtMs, prev)
		}
		prev = s.AtMs
	}
	return nil
}
