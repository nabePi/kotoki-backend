// Package faultdoc builds the fault document of the fake provider (spec 13.13.1, 8.9.6): the JSON
// that chaos scenarios SET under RedisKey. Building validates it the way the fake provider's own
// parser does, because that parser silently skips what it does not understand and a skipped fault
// would turn a chaos run into a pass for the wrong reason.
package faultdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RedisKey is the default FAKE_FAULTS_REDIS_KEY.
const RedisKey = "kotoki:fake:faults"

// Fault is one entry of the document. Mode and Rate are always written (a rate of 0 is a valid,
// explicit value); the others only when set.
type Fault struct {
	Mode    string  `json:"mode"`
	Rate    float64 `json:"rate"`
	AfterMS int     `json:"after_ms,omitempty"`
	DelayMS int     `json:"delay_ms,omitempty"`
	Status  int     `json:"status,omitempty"`
	Kind    string  `json:"kind,omitempty"`
}

var keyRE = regexp.MustCompile(`^([A-Z]+)\.(FAKE_PRIMARY|FAKE_FALLBACK)$`)

var capabilities = map[string]bool{"STT": true, "LLM": true, "TTS": true, "EVALUATION": true, "EMBEDDING": true}

var modes = map[string]bool{"error": true, "timeout": true, "slow": true, "fail_after_first_audio": true, "disconnect_mid_stream": true}

// Build returns the JSON document for faults. A zero until leaves the document without expiry;
// otherwise it is written in UTC (RFC 3339) and the fake provider ignores the whole document after
// it, the safety net of a chaos run whose revert never happens.
func Build(until time.Time, faults map[string]Fault) ([]byte, error) {
	if len(faults) == 0 {
		return nil, errors.New("faultdoc: no faults")
	}
	doc := make(map[string]any, len(faults)+1)
	for key, f := range faults {
		if err := validate(key, f); err != nil {
			return nil, err
		}
		doc[key] = f
	}
	if !until.IsZero() {
		doc["until"] = until.UTC().Format(time.RFC3339)
	}
	return json.Marshal(doc)
}

func validate(key string, f Fault) error {
	m := keyRE.FindStringSubmatch(key)
	if m == nil {
		return fmt.Errorf("faultdoc: key %q is not <CAPABILITY>.FAKE_PRIMARY|FAKE_FALLBACK in upper case", key)
	}
	capName, inst := m[1], m[2]
	if !capabilities[capName] {
		return fmt.Errorf("faultdoc: key %q: unknown capability %q", key, capName)
	}
	if inst == "FAKE_FALLBACK" && capName != "TTS" {
		return fmt.Errorf("faultdoc: key %q: only TTS has a FAKE_FALLBACK instance", key)
	}
	if !modes[f.Mode] {
		return fmt.Errorf("faultdoc: key %q: unknown mode %q", key, f.Mode)
	}
	if f.Mode == "fail_after_first_audio" && capName != "TTS" {
		return fmt.Errorf("faultdoc: key %q: %s only applies to TTS", key, f.Mode)
	}
	if f.Mode == "disconnect_mid_stream" && capName != "STT" {
		return fmt.Errorf("faultdoc: key %q: %s only applies to STT", key, f.Mode)
	}
	if f.Rate < 0 || f.Rate > 1 {
		return fmt.Errorf("faultdoc: key %q: rate %v outside [0,1]", key, f.Rate)
	}
	if f.AfterMS < 0 || f.DelayMS < 0 || f.Status < 0 {
		return fmt.Errorf("faultdoc: key %q: negative after_ms, delay_ms or status", key)
	}
	if f.Kind != "" && f.Kind != strings.ToUpper(f.Kind) {
		return fmt.Errorf("faultdoc: key %q: kind %q must be upper case", key, f.Kind)
	}
	return nil
}
