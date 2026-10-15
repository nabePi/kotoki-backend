package conversation

import (
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

// specMetrics is the table of spec §7.14 (name without prefix → label names), plus the two
// runtime counters outside the prefix. The test pins both the set and the labels.
var specMetrics = map[string][]string{
	"sessions_active":                   {"state"},
	"session_capacity":                  nil,
	"draining":                          nil,
	"connections_total":                 {"result"},
	"ws_close_total":                    {"code"},
	"turn_end_to_first_audio_seconds":   {"kind", "used_fallback"},
	"stt_final_seconds":                 {"provider"},
	"llm_first_token_seconds":           {"provider", "model"},
	"llm_first_sentence_seconds":        {"provider", "model"},
	"tts_first_audio_seconds":           {"provider", "model"},
	"server_overhead_seconds":           nil,
	"bargein_stop_seconds":              nil,
	"bargein_total":                     {"source"},
	"turns_total":                       {"status"},
	"turn_end_reason_total":             {"reason"},
	"turn_resumed_total":                nil,
	"generation_cancelled_total":        {"reason"},
	"generation_goroutine_exit_seconds": nil,
	"ai_sentence_limit_total":           {"kind"},
	"llm_truncated_total":               nil,
	"stt_discarded_commits_total":       {"boundary"},
	"difficulty_adjustments_total":      nil,
	"provider_fallback_total":           {"capability", "from", "to"},
	"audio_frames_in_total":             {"result"},
	"audio_frames_out_total":            {"result"},
	"frame_processing_seconds":          {"stage"},
	"outbound_queue_depth":              {"queue"},
	"slow_consumer_total":               nil,
	"protocol_violations_total":         {"kind"},
	"reconnect_total":                   {"result", "replay"},
	"reconnect_gap_seconds":             nil,
	"fence_lost_total":                  nil,
	"prompt_tokens":                     {"component"},
	"prompt_token_estimate_ratio":       nil,
	"rolling_summary_total":             {"result"},
	"playback_finished_missing_total":   nil,
	"drain_duration_seconds":            nil,
}

// outsidePrefix are the runtime counters whose names belong to other chapters (§7.14 note).
var outsidePrefix = map[string][]string{
	"kotoki_llm_content_filtered_total":     {"provider"},
	"kotoki_content_opening_fallback_total": nil,
}

// extraMetrics are names §7.14 does not list: outbound_transient_dropped_total is required by
// §7.12.3 without a name (deviation of Task 10).
var extraMetrics = map[string][]string{"outbound_transient_dropped_total": {"reason"}}

var (
	descName   = regexp.MustCompile(`fqName: "([^"]+)"`)
	descLabels = regexp.MustCompile(`variableLabels: \{([^}]*)\}`)
)

// described returns fqName → label names of every collector, counting each name's occurrences.
func described(t *testing.T, m *Metrics) (map[string][]string, map[string]int) {
	t.Helper()
	labels, seen := map[string][]string{}, map[string]int{}
	for _, c := range m.collectors() {
		ch := make(chan *prometheus.Desc, 8)
		go func() { c.Describe(ch); close(ch) }()
		for d := range ch {
			s := d.String()
			name := descName.FindStringSubmatch(s)
			require.Len(t, name, 2, s)
			seen[name[1]]++
			var ls []string
			if l := descLabels.FindStringSubmatch(s); len(l) == 2 && strings.TrimSpace(l[1]) != "" {
				for _, x := range strings.Split(l[1], ",") {
					ls = append(ls, strings.TrimSpace(x))
				}
			}
			labels[name[1]] = ls
		}
	}
	return labels, seen
}

func TestMetricsNamesAndBuckets(t *testing.T) {
	m := NewMetrics(nil)
	labels, seen := described(t, m)

	want := map[string][]string{}
	for n, l := range specMetrics {
		want[metricPrefix+n] = l
	}
	for n, l := range outsidePrefix {
		want[n] = l
	}
	for n, l := range extraMetrics {
		want[metricPrefix+n] = l
	}
	require.Equal(t, want, labels, "the registered set is exactly §7.14 (plus the documented extras)")
	for n, c := range seen {
		require.Equal(t, 1, c, "%s is registered exactly once", n)
	}
	for n, ls := range labels {
		require.NotContains(t, ls, "session_id", n)
		require.NotContains(t, ls, "user_id", n)
	}

	// Registering twice on one registry must collide: each name is owned by NewMetrics alone.
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	require.Panics(t, func() { NewMetrics(reg) })

	// Buckets: TURN_BUCKETS for latency, BARGE_BUCKETS for barge-in and goroutine exit (§7.14).
	reg = prometheus.NewRegistry()
	m = NewMetrics(reg)
	d := 120 * time.Millisecond
	m.TurnEndToFirstAudio("reply", false, d)
	m.STTFinal("FAKE_PRIMARY", d)
	m.LLMFirstToken("FAKE_PRIMARY", "m", d)
	m.LLMFirstSentence("FAKE_PRIMARY", "m", d)
	m.TTSFirstAudio("FAKE_PRIMARY", "m", d)
	m.ServerOverhead(d)
	m.Reconnect("same_pod", "complete", d)
	m.BargeInStop(d)
	m.GenerationExit(d)
	m.FrameProcessing(StageVAD, 200*time.Microsecond)
	m.PromptTokens("system", 300)
	m.OutboundDepth(1, 2)
	got := bucketBounds(t, reg)
	for _, n := range []string{"turn_end_to_first_audio_seconds", "stt_final_seconds", "llm_first_token_seconds",
		"llm_first_sentence_seconds", "tts_first_audio_seconds", "server_overhead_seconds", "reconnect_gap_seconds"} {
		require.Equal(t, metrics.TurnBuckets, got[metricPrefix+n], n)
	}
	for _, n := range []string{"bargein_stop_seconds", "generation_goroutine_exit_seconds"} {
		require.Equal(t, metrics.BargeBuckets, got[metricPrefix+n], n)
	}
	require.Equal(t, frameProcessingBuckets, got[metricPrefix+"frame_processing_seconds"])
	require.Equal(t, []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02}, frameProcessingBuckets)
	require.Equal(t, []float64{50, 100, 200, 400, 800, 1600, 3200, 6400}, got[metricPrefix+"prompt_tokens"])
}

// scrape renders reg in the Prometheus text format.
func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

var bucketLine = regexp.MustCompile(`^(\w+)_bucket\{[^}]*le="([^"]+)"\} `)

// bucketBounds returns the finite upper bounds of every histogram of reg that has samples (the
// bounds are identical for every label set of one histogram, so each is listed once).
func bucketBounds(t *testing.T, reg *prometheus.Registry) map[string][]float64 {
	t.Helper()
	out := map[string][]float64{}
	seen := map[string]bool{}
	for _, line := range strings.Split(scrape(t, reg), "\n") {
		m := bucketLine.FindStringSubmatch(line)
		if m == nil || m[2] == "+Inf" || seen[m[1]+"|"+m[2]] {
			continue
		}
		seen[m[1]+"|"+m[2]] = true
		f, err := strconv.ParseFloat(m[2], 64)
		require.NoError(t, err)
		out[m[1]] = append(out[m[1]], f)
	}
	return out
}

func TestMetricsNilSafe(*testing.T) {
	var m *Metrics
	m.SessionState("", SessionStarting)
	m.SetSessionCapacity(100)
	m.SetDraining(true)
	m.Connection(ConnAccepted)
	m.WSClose(1000)
	m.Reconnect("same_pod", "complete", time.Second)
	m.DrainDuration(time.Second)
	m.TurnEndToFirstAudio("reply", true, time.Second)
	m.STTFinal("P", time.Second)
	m.LLMFirstToken("P", "m", time.Second)
	m.LLMFirstSentence("P", "m", time.Second)
	m.TTSFirstAudio("P", "m", time.Second)
	m.ServerOverhead(time.Second)
	m.BargeInStop(time.Second)
	m.BargeIn("server")
	m.Turn("COMPLETED")
	m.TurnEndReason("silence_800")
	m.TurnResumed()
	m.GenerationCancelled("BARGE_IN")
	m.GenerationExit(time.Second)
	m.STTDiscarded("listening")
	m.ProviderFallback("TTS", "A", "B")
	m.PlaybackFinishedMissing()
	m.RollingSummary("ok")
	m.FrameProcessing(StageOpusDecode, time.Millisecond)
	m.OutboundDepth(1, 1)
	m.PromptTokens("system", 1)
	m.PromptEstimateRatio(10, 5)
	m.LLMContentFiltered("P")
	m.OpeningFallback()
}

func TestSessionStateGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.SessionState("", SessionStarting)
	m.SessionState(SessionStarting, SessionActive)
	m.SessionState("", SessionStarting)
	m.SessionState(SessionActive, "")
	out := scrape(t, reg)
	require.Contains(t, out, `kotoki_realtime_sessions_active{state="starting"} 1`)
	require.Contains(t, out, `kotoki_realtime_sessions_active{state="active"} 0`)
}
