package provider

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func exerciseAll(m *Metrics) {
	m.ObserveCall(OpenAI, CapLLM, "gpt-4.1", "", "error", KindRateLimited, "rate_limit_exceeded")
	m.ObserveLatency(OpenAI, CapLLM, "gpt-4.1", "first_token", 300*time.Millisecond)
	m.ActiveInc(OpenAI, CapLLM)
	m.ConcurrencyWait(OpenAI, CapLLM, time.Millisecond)
	m.ConcurrencyRejected(OpenAI, CapLLM)
	m.ObserveFallback(ElevenLabs, Cartesia, "TIMEOUT")
	m.CircuitTransition(OpenAI, CapLLM, BreakerOpen)
	m.StreamDisconnect(OpenAI, CapSTT)
	m.RateLimitRemaining(OpenAI, CapLLM, "requests", 10)
	m.STTReplay(OpenAI, 1.5)
	m.STTRotation(OpenAI)
	m.TTSEpochRestart(ElevenLabs, Cartesia)
	m.Usage(OpenAI, CapLLM, "gpt-4.1", "input_tokens", 100)
	m.CostUSD(OpenAI, CapLLM, "gpt-4.1", 0.01)
	m.ObserveSessionCost(0.12, 0.02)
}

func TestMetricsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	exerciseAll(m)
	fams, err := reg.Gather()
	require.NoError(t, err)
	got := map[string]bool{}
	for _, f := range fams {
		got[f.GetName()] = true
	}
	for _, name := range []string{
		"kotoki_provider_requests_total", "kotoki_provider_errors_total", "kotoki_provider_latency_seconds", "kotoki_provider_active",
		"kotoki_provider_concurrency_wait_seconds", "kotoki_provider_concurrency_rejected_total", "kotoki_provider_fallback_total",
		"kotoki_provider_circuit_state", "kotoki_provider_circuit_transitions_total", "kotoki_provider_stream_disconnects_total",
		"kotoki_provider_ratelimit_remaining", "kotoki_stt_replay_seconds", "kotoki_stt_rotations_total", "kotoki_tts_epoch_restarts_total",
		"kotoki_provider_usage_total", "kotoki_provider_cost_usd_total", "kotoki_session_cost_usd", "kotoki_session_cost_per_minute_usd",
	} {
		require.True(t, got[name], "missing metric %s", name)
	}
}

func TestMetricsNoHighCardinalityLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	exerciseAll(NewMetrics(reg))
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		for _, mm := range f.GetMetric() {
			for _, l := range mm.GetLabel() {
				require.NotContains(t, []string{"session_id", "user_id", "request_id"}, l.GetName(), f.GetName())
			}
		}
	}
}

func TestSessionCostNoLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.ObserveSessionCost(0.12, 0.02)
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() == "kotoki_session_cost_usd" {
			require.Len(t, f.GetMetric(), 1)
			require.Empty(t, f.GetMetric()[0].GetLabel())
			require.EqualValues(t, 1, f.GetMetric()[0].GetHistogram().GetSampleCount())
			return
		}
	}
	t.Fatal("kotoki_session_cost_usd missing")
}

func TestUnknownVendorCodeBecomesOther(t *testing.T) {
	m := NewMetrics(nil)
	m.ObserveCall(OpenAI, CapLLM, "m", "", "error", KindUnavailable, "some_weird_vendor_code_123")
	m.ObserveCall(OpenAI, CapLLM, "m", "", "error", KindRateLimited, "insufficient_quota")
	require.Equal(t, 1.0, testutil.ToFloat64(m.errors.WithLabelValues("OPENAI", "LLM", "UNAVAILABLE", "other")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.errors.WithLabelValues("OPENAI", "LLM", "RATE_LIMITED", "insufficient_quota")))
}

func TestFallbackReasonLabelValues(t *testing.T) {
	m := NewMetrics(nil)
	m.ObserveFallback(ElevenLabs, Cartesia, "CIRCUIT_OPEN")
	m.ObserveFallback(ElevenLabs, Cartesia, string(KindTimeout))
	require.Equal(t, 1.0, testutil.ToFloat64(m.fallback.WithLabelValues("TTS", "ELEVENLABS", "CARTESIA", "CIRCUIT_OPEN")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.fallback.WithLabelValues("TTS", "ELEVENLABS", "CARTESIA", "TIMEOUT")))
}

func TestObserveLLMFirstSentenceUsesDefault(t *testing.T) {
	ObserveLLMFirstSentence(OpenAI, "m", time.Second) // no default yet: must not panic
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	SetDefaultMetrics(m)
	t.Cleanup(func() { SetDefaultMetrics(nil) })
	ObserveLLMFirstSentence(OpenAI, "gpt-4.1", 700*time.Millisecond)
	fams, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, f := range fams {
		if f.GetName() == "kotoki_provider_latency_seconds" {
			for _, l := range f.GetMetric()[0].GetLabel() {
				if l.GetName() == "phase" {
					require.Equal(t, "first_sentence", l.GetValue())
					found = true
				}
			}
		}
	}
	require.True(t, found)
}

func TestNilMetricsSafe(_ *testing.T) {
	var m *Metrics
	exerciseAll(m) // every helper tolerates a nil receiver
}
