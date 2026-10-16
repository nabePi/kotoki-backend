package config

import "time"

// WorkerEvaluation holds the post-session evaluation settings (spec 10.13).
type WorkerEvaluation struct {
	MinUserTurns         int           `env:"EVAL_MIN_USER_TURNS"`
	MinUserSpeechSeconds int           `env:"EVAL_MIN_USER_SPEECH_SECONDS"`
	MinActiveSeconds     int           `env:"EVAL_MIN_ACTIVE_SECONDS"`
	MinFindingConfidence float64       `env:"EVAL_MIN_FINDING_CONFIDENCE"`
	TranscriptMaxChars   int           `env:"EVAL_TRANSCRIPT_MAX_CHARS"`
	HandlerTimeout       time.Duration `env:"EVAL_HANDLER_TIMEOUT"`
}

// WorkerSummary holds the session summary settings (spec 9.9.6).
type WorkerSummary struct {
	MaxChars int `env:"SUMMARY_MAX_CHARS"`
}

func defaultWorkerEvaluation() WorkerEvaluation {
	return WorkerEvaluation{
		MinUserTurns: 3, MinUserSpeechSeconds: 20, MinActiveSeconds: 60,
		MinFindingConfidence: 0.6, TranscriptMaxChars: 60000, HandlerTimeout: 200 * time.Second,
	}
}

func defaultWorkerSummary() WorkerSummary { return WorkerSummary{MaxChars: 300} }

func (e WorkerEvaluation) validate(v *validator) {
	for _, c := range []struct {
		name string
		val  int
	}{
		{"EVAL_MIN_USER_TURNS", e.MinUserTurns},
		{"EVAL_MIN_USER_SPEECH_SECONDS", e.MinUserSpeechSeconds},
		{"EVAL_MIN_ACTIVE_SECONDS", e.MinActiveSeconds},
	} {
		if c.val < 0 {
			v.fail(c.name, "must be >= 0, got %d", c.val)
		}
	}
	if e.MinFindingConfidence < 0 || e.MinFindingConfidence > 1 {
		v.fail("EVAL_MIN_FINDING_CONFIDENCE", "must be within 0–1, got %v", e.MinFindingConfidence)
	}
	if e.TranscriptMaxChars <= 0 {
		v.fail("EVAL_TRANSCRIPT_MAX_CHARS", "must be > 0, got %d", e.TranscriptMaxChars)
	}
	v.positive("EVAL_HANDLER_TIMEOUT", e.HandlerTimeout)
}

func (s WorkerSummary) validate(v *validator) {
	if s.MaxChars <= 0 {
		v.fail("SUMMARY_MAX_CHARS", "must be > 0, got %d", s.MaxChars)
	}
}
