// Package fake implements deterministic fake providers for every capability (spec 8.9,
// D-007): local dev (PROVIDER_MODE=fake), tests and load tests. It never imports go-redis
// or httpx; runtime fault injection arrives through the FaultSource port.
package fake

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Config holds the capability-independent FAKE_* settings (spec 8.9, §13.2.9).
type Config struct {
	Seed               int64         `env:"FAKE_SEED" envDefault:"42"`
	STTMode            string        `env:"FAKE_STT_MODE"` // "", fixture or energy; "" = fixture when STTFixtureDir is set
	STTFixtureDir      string        `env:"FAKE_STT_FIXTURE_DIR"`
	STTMatchThreshold  float64       `env:"FAKE_STT_MATCH_THRESHOLD" envDefault:"0.8"`
	STTEnergyDBFS      float64       `env:"FAKE_STT_ENERGY_DBFS" envDefault:"-40"`
	STTPhrases         []string      `env:"FAKE_STT_PHRASES" envSeparator:"|" envDefault:"はい、そうです。|えっと、週末は友達と映画を見ました。|日本語の勉強は楽しいです。|すみません、もう一度お願いします。|昨日は雨でしたね。"`
	LLMTokenIntervalMS int           `env:"FAKE_LLM_TOKEN_INTERVAL_MS" envDefault:"30"`
	LLMTemplatesFile   string        `env:"FAKE_LLM_TEMPLATES_FILE"`
	TTSMsPerChar       int           `env:"FAKE_TTS_MS_PER_CHAR" envDefault:"120"`
	TTSChunkIntervalMS int           `env:"FAKE_TTS_CHUNK_INTERVAL_MS" envDefault:"5"`
	TTSWaveform        string        `env:"FAKE_TTS_WAVEFORM" envDefault:"tone"` // tone | silence
	EvalFixtureDir     string        `env:"FAKE_EVAL_FIXTURE_DIR"`
	FaultsRedisKey     string        `env:"FAKE_FAULTS_REDIS_KEY" envDefault:"kotoki:fake:faults"`
	FaultsPollInterval time.Duration `env:"FAKE_FAULTS_POLL_INTERVAL" envDefault:"1s"`
}

// Failure phases of CapConfig.FailurePhase.
const (
	PhaseStart       = "start"        // synchronous error
	PhaseBeforeFirst = "before_first" // error before the first event/chunk
	PhaseMid         = "mid"          // after FailAfter events/chunks (STT: seconds of stream)
	PhaseHang        = "hang"         // send nothing until ctx ends (tests timeouts)
)

// CapConfig is the per-instance, per-capability behaviour (FAKE_<INST>_<CAP>_*).
type CapConfig struct {
	LatencyMS    int
	JitterMS     int
	FailureRate  float64
	FailEveryN   int
	FailureKind  provider.ErrorKind
	FailurePhase string
	FailAfter    int
	HangAfter    int // events/chunks delivered before a "hang" phase starts (0 = hang at once)
	ConnectMS    int // STT only
}

// Deps are the collaborators shared by every fake.
type Deps struct {
	Clock  clock.Clock
	Faults FaultSource
	Log    *slog.Logger
	Seed   int64
}

// WithDefaults fills the zero-valued collaborators (system clock, no faults, discard logger).
func (d Deps) WithDefaults() Deps {
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.Faults == nil {
		d.Faults = NoFaults
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return d
}

func defaultLatencyMS(c provider.Capability) int {
	switch c {
	case provider.CapSTT:
		return 150
	case provider.CapLLM:
		return 300
	case provider.CapTTS:
		return 150
	case provider.CapEvaluation:
		return 500
	case provider.CapEmbedding:
		return 20
	}
	return 0
}

var validKinds = map[provider.ErrorKind]bool{
	provider.KindTimeout: true, provider.KindRateLimited: true, provider.KindAuth: true, provider.KindInvalidRequest: true,
	provider.KindUnavailable: true, provider.KindContentFiltered: true, provider.KindInvalidResponse: true, provider.KindCancelled: true,
}

func instLabel(inst provider.Name) (string, error) {
	switch inst {
	case provider.FakePrimary:
		return "PRIMARY", nil
	case provider.FakeFallback:
		return "FALLBACK", nil
	}
	return "", fmt.Errorf("fake: unknown instance %q", inst)
}

// LoadCapConfig reads FAKE_<PRIMARY|FALLBACK>_<CAP>_* through lookup. FALLBACK exists only
// for TTS (the character's voice.fallback slot, D-015). Invalid values name the variable.
func LoadCapConfig(lookup func(string) (string, bool), inst provider.Name, c provider.Capability) (CapConfig, error) {
	label, err := instLabel(inst)
	if err != nil {
		return CapConfig{}, err
	}
	if inst == provider.FakeFallback && c != provider.CapTTS {
		return CapConfig{}, fmt.Errorf("fake: FAKE_FALLBACK exists only for TTS, not %s", c)
	}
	prefix := "FAKE_" + label + "_" + string(c) + "_"
	cfg := CapConfig{LatencyMS: defaultLatencyMS(c), FailureKind: provider.KindUnavailable, FailurePhase: PhaseStart, FailAfter: 3}
	if c == provider.CapSTT {
		cfg.ConnectMS = 50
	}
	getInt := func(suffix string, dst *int, lo int) error {
		name := prefix + suffix
		v, ok := lookup(name)
		if !ok || v == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < lo {
			return fmt.Errorf("%s must be an integer >= %d, got %q", name, lo, v)
		}
		*dst = n
		return nil
	}
	for _, f := range []struct {
		suffix string
		dst    *int
	}{{"LATENCY_MS", &cfg.LatencyMS}, {"JITTER_MS", &cfg.JitterMS}, {"FAIL_EVERY_N", &cfg.FailEveryN}, {"FAIL_AFTER", &cfg.FailAfter}, {"HANG_AFTER", &cfg.HangAfter}} {
		if err := getInt(f.suffix, f.dst, 0); err != nil {
			return CapConfig{}, err
		}
	}
	if c == provider.CapSTT {
		if err := getInt("CONNECT_MS", &cfg.ConnectMS, 0); err != nil {
			return CapConfig{}, err
		}
	}
	if v, ok := lookup(prefix + "FAILURE_RATE"); ok && v != "" {
		r, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || r < 0 || r > 1 {
			return CapConfig{}, fmt.Errorf("%sFAILURE_RATE must be a number in 0..1, got %q", prefix, v)
		}
		cfg.FailureRate = r
	}
	if v, ok := lookup(prefix + "FAILURE_KIND"); ok && v != "" {
		k := provider.ErrorKind(strings.ToUpper(strings.TrimSpace(v)))
		if !validKinds[k] {
			return CapConfig{}, fmt.Errorf("%sFAILURE_KIND must be a provider error kind, got %q", prefix, v)
		}
		cfg.FailureKind = k
	}
	if v, ok := lookup(prefix + "FAILURE_PHASE"); ok && v != "" {
		switch p := strings.ToLower(strings.TrimSpace(v)); p {
		case PhaseStart, PhaseBeforeFirst, PhaseMid, PhaseHang:
			cfg.FailurePhase = p
		default:
			return CapConfig{}, fmt.Errorf("%sFAILURE_PHASE must be start, before_first, mid or hang, got %q", prefix, v)
		}
	}
	return cfg, nil
}
