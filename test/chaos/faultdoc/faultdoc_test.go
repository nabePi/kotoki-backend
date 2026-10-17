package faultdoc_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
)

// TestBuild_MatchesSpecExamples builds the three entries of spec 13.13.1 and compares the decoded
// document with the literal of the spec.
func TestBuild_MatchesSpecExamples(t *testing.T) {
	until := time.Date(2026, 10, 12, 17, 15, 0, 0, time.FixedZone("WIB", 7*3600)) // 10:15Z
	b, err := faultdoc.Build(until, map[string]faultdoc.Fault{
		"STT.FAKE_PRIMARY": {Mode: "timeout", Rate: 1.0, AfterMS: 2000},
		"LLM.FAKE_PRIMARY": {Mode: "error", Rate: 0.5, Status: 503},
		"TTS.FAKE_PRIMARY": {Mode: "fail_after_first_audio", Rate: 1.0},
	})
	require.NoError(t, err)
	const spec = `{
	  "STT.FAKE_PRIMARY": {"mode": "timeout", "rate": 1.0, "after_ms": 2000},
	  "LLM.FAKE_PRIMARY": {"mode": "error", "rate": 0.5, "status": 503},
	  "TTS.FAKE_PRIMARY": {"mode": "fail_after_first_audio", "rate": 1.0},
	  "until": "2026-10-12T10:15:00Z"
	}`
	var got, want map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.NoError(t, json.Unmarshal([]byte(spec), &want))
	require.Equal(t, want, got)
}

func TestBuild_KeepsZeroRateAndOptionalFields(t *testing.T) {
	b, err := faultdoc.Build(time.Time{}, map[string]faultdoc.Fault{
		"EVALUATION.FAKE_PRIMARY": {Mode: "slow", Rate: 0, DelayMS: 20000},
		"TTS.FAKE_FALLBACK":       {Mode: "error", Rate: 1, Kind: "UNAVAILABLE"},
	})
	require.NoError(t, err)
	var got map[string]map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, map[string]any{"mode": "slow", "rate": 0.0, "delay_ms": 20000.0}, got["EVALUATION.FAKE_PRIMARY"])
	require.Equal(t, map[string]any{"mode": "error", "rate": 1.0, "kind": "UNAVAILABLE"}, got["TTS.FAKE_FALLBACK"])
	require.NotContains(t, got, "until", "a zero until leaves the document without expiry")
}

func TestBuild_Rejects(t *testing.T) {
	ok := faultdoc.Fault{Mode: "error", Rate: 1}
	cases := map[string]map[string]faultdoc.Fault{
		"vendor name as instance":       {"STT.ELEVENLABS": ok},
		"lowercase capability":          {"stt.FAKE_PRIMARY": ok},
		"unknown capability":            {"VIDEO.FAKE_PRIMARY": ok},
		"no instance":                   {"STT": ok},
		"fail_after_first_audio on STT": {"STT.FAKE_PRIMARY": {Mode: "fail_after_first_audio", Rate: 1}},
		"disconnect_mid_stream on TTS":  {"TTS.FAKE_PRIMARY": {Mode: "disconnect_mid_stream", Rate: 1}},
		"unknown mode":                  {"STT.FAKE_PRIMARY": {Mode: "explode", Rate: 1}},
		"empty mode":                    {"STT.FAKE_PRIMARY": {Rate: 1}},
		"rate above 1":                  {"STT.FAKE_PRIMARY": {Mode: "error", Rate: 1.5}},
		"negative rate":                 {"STT.FAKE_PRIMARY": {Mode: "error", Rate: -0.1}},
		// the fake provider ignores FAKE_FALLBACK outside TTS (spec 8.9.6): a fault that does nothing
		// would make a chaos run pass for the wrong reason
		"FAKE_FALLBACK outside TTS": {"LLM.FAKE_FALLBACK": ok},
		"no faults":                 {},
	}
	for name, faults := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := faultdoc.Build(time.Now(), faults)
			require.Error(t, err)
		})
	}
}

func TestRedisKey(t *testing.T) {
	require.Equal(t, "kotoki:fake:faults", faultdoc.RedisKey)
}
