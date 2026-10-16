package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestWorkerEvalDefaults(t *testing.T) {
	w, err := loadWorker(testutil.MinimalEnv(t))
	require.NoError(t, err)
	require.Equal(t, WorkerEvaluation{
		MinUserTurns: 3, MinUserSpeechSeconds: 20, MinActiveSeconds: 60,
		MinFindingConfidence: 0.6, TranscriptMaxChars: 60000, HandlerTimeout: 200 * time.Second,
	}, w.Evaluation)
	require.Equal(t, WorkerSummary{MaxChars: 300}, w.Summary)
}

func TestWorkerEvalOverrides(t *testing.T) {
	w, err := loadWorker(testutil.MinimalEnv(t, map[string]string{
		"EVAL_MIN_USER_TURNS": "5", "EVAL_MIN_USER_SPEECH_SECONDS": "30", "EVAL_MIN_ACTIVE_SECONDS": "90",
		"EVAL_MIN_FINDING_CONFIDENCE": "0.75", "EVAL_TRANSCRIPT_MAX_CHARS": "1000", "EVAL_HANDLER_TIMEOUT": "4m",
		"SUMMARY_MAX_CHARS": "200",
	}))
	require.NoError(t, err)
	require.Equal(t, 5, w.Evaluation.MinUserTurns)
	require.Equal(t, 30, w.Evaluation.MinUserSpeechSeconds)
	require.Equal(t, 90, w.Evaluation.MinActiveSeconds)
	require.Equal(t, 0.75, w.Evaluation.MinFindingConfidence)
	require.Equal(t, 1000, w.Evaluation.TranscriptMaxChars)
	require.Equal(t, 4*time.Minute, w.Evaluation.HandlerTimeout)
	require.Equal(t, 200, w.Summary.MaxChars)
}

func TestWorkerEvalValidation(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"EVAL_MIN_USER_TURNS":         {"EVAL_MIN_USER_TURNS": "-1"},
		"EVAL_MIN_ACTIVE_SECONDS":     {"EVAL_MIN_ACTIVE_SECONDS": "-5"},
		"EVAL_MIN_FINDING_CONFIDENCE": {"EVAL_MIN_FINDING_CONFIDENCE": "1.5"},
		"EVAL_TRANSCRIPT_MAX_CHARS":   {"EVAL_TRANSCRIPT_MAX_CHARS": "0"},
		"EVAL_HANDLER_TIMEOUT":        {"EVAL_HANDLER_TIMEOUT": "0s"},
		"SUMMARY_MAX_CHARS":           {"SUMMARY_MAX_CHARS": "0"},
	} {
		_, err := loadWorker(testutil.MinimalEnv(t, env))
		require.ErrorContains(t, err, name)
	}
}
