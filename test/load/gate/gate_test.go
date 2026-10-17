package gate

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSpecs_MatchSection1253(t *testing.T) {
	cases := []struct {
		name string
		vus  int
		ramp time.Duration
		hold time.Duration
	}{
		{"g1", 100, 5 * time.Minute, 30 * time.Minute},
		{"g2", 250, 5 * time.Minute, 30 * time.Minute},
		{"g3", 500, 10 * time.Minute, 30 * time.Minute},
		{"g4a", 1000, 15 * time.Minute, 60 * time.Minute},
		{"g4b", 1000, 15 * time.Minute, 60 * time.Minute},
		{"soak", 500, 10 * time.Minute, 4 * time.Hour},
		{"real", 20, 2 * time.Minute, 15 * time.Minute},
		{"g1-local", 100, time.Minute, 5 * time.Minute},
	}
	require.Len(t, Specs, len(cases))
	for _, c := range cases {
		s, ok := Specs[c.name]
		require.True(t, ok, c.name)
		require.Equal(t, c.name, s.Name)
		require.Equal(t, c.vus, s.VUs, c.name)
		require.Equal(t, c.ramp, s.Ramp, c.name)
		require.Equal(t, c.hold, s.Hold, c.name)
		wantMode := "fake"
		if c.name == "real" {
			wantMode = "real"
		}
		require.Equal(t, wantMode, s.Mode, c.name)
	}
}

func TestSpecs_SkipsAreKnownAndExplained(t *testing.T) {
	known := map[string]bool{}
	for _, c := range criteria {
		known[c.ID] = true
	}
	for name, s := range Specs {
		for id, why := range s.Skip {
			require.True(t, known[id], "%s skips unknown %s", name, id)
			require.NotEmpty(t, strings.TrimSpace(why), "%s skip %s needs a reason", name, id)
		}
	}
	require.ElementsMatch(t, []string{"K10", "K11", "K12", "K15"}, keys(Specs["g1-local"].Skip))
	require.Equal(t, []string{"K2"}, keys(Specs["real"].Skip))
	for _, n := range []string{"g1", "g2", "g3", "g4a", "g4b", "soak"} {
		require.Empty(t, Specs[n].Skip, n)
	}
	require.True(t, Specs["g4a"].PodBalance)
	for n, s := range Specs {
		if n != "g4a" {
			require.False(t, s.PodBalance, n)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// good returns an observation that passes every criterion for every gate (including the g4a pod balance).
func good() Observation {
	return Observation{
		"turn_p95_s": 1.0, "server_overhead_p95_s": 0.05, "bargein_stop_p95_s": 0.1, "client_bargein_p95_s": 0.4,
		"rest_p95_s": 0.1, "session_create_p95_s": 0.2, "ws_connect_ok_ratio": 1, "session_failed_server_error_ratio": 0,
		"resume_ok_ratio": 1, "slow_consumer_total": 0, "protocol_violations_total": 0,
		"realtime_cpu_ratio_max": 0.5, "realtime_mem_ratio_max": 0.5, "realtime_restarts": 0,
		"pg_acquire_wait_p99_s": 0.01, "pg_cpu_ratio": 0.3, "redis_cpu_ratio": 0.2, "redis_errors": 0,
		"outbox_lag_p99_s": 1, "dlq_depth": 0, "sessions_without_evaluation": 0, "verify_violations": 0,
		"soak_rss_drift": 0.01, "soak_goroutine_drift": 0.01, "sessions_per_pod_min": 98, "sessions_per_pod_max": 100,
	}
}

func byID(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

func TestEvaluate_GoodPassesEverywhere(t *testing.T) {
	for name, s := range Specs {
		rs := Evaluate(s, good())
		require.Len(t, rs, 15, name)
		require.True(t, AllPass(rs), name)
		for i, r := range rs {
			require.Equal(t, fmt.Sprintf("K%d", i+1), r.ID, name)
			require.NotEmpty(t, r.Name)
		}
	}
}

func TestEvaluate_Boundaries(t *testing.T) {
	cases := []struct {
		id, key  string
		pass     float64
		fail     float64
		failNote string
	}{
		{"K1", "turn_p95_s", 1.5, 1.501, ""},
		{"K2", "server_overhead_p95_s", 0.150, 0.151, ""},
		{"K3", "bargein_stop_p95_s", 0.150, 0.151, ""},
		{"K4", "client_bargein_p95_s", 0.6, 0.601, ""},
		{"K5", "rest_p95_s", 0.2, 0.201, ""},
		{"K5", "session_create_p95_s", 0.3, 0.301, ""},
		{"K6", "ws_connect_ok_ratio", 0.999, 0.9989, ""},
		{"K7", "session_failed_server_error_ratio", 0.001, 0.0011, ""},
		{"K8", "resume_ok_ratio", 0.99, 0.9899, ""},
		{"K9", "slow_consumer_total", 0, 1, ""},
		{"K9", "protocol_violations_total", 0, 1, ""},
		{"K10", "realtime_cpu_ratio_max", 0.79, 0.80, ""},
		{"K10", "realtime_mem_ratio_max", 0.79, 0.80, ""},
		{"K10", "realtime_restarts", 0, 1, ""},
		{"K11", "pg_acquire_wait_p99_s", 0.0499, 0.05, ""},
		{"K11", "pg_cpu_ratio", 0.59, 0.6, ""},
		{"K12", "redis_cpu_ratio", 0.49, 0.5, ""},
		{"K12", "redis_errors", 0, 1, ""},
		{"K13", "outbox_lag_p99_s", 4.99, 5, ""},
		{"K13", "dlq_depth", 0, 1, ""},
		{"K13", "sessions_without_evaluation", 0, 1, ""},
		{"K14", "verify_violations", 0, 1, ""},
		{"K15", "soak_rss_drift", 0.099, 0.10, ""},
		{"K15", "soak_goroutine_drift", 0.099, 0.10, ""},
	}
	g1 := Specs["g1"]
	for _, c := range cases {
		t.Run(c.id+"/"+c.key, func(t *testing.T) {
			o := good()
			o[c.key] = c.pass
			rs := Evaluate(g1, o)
			require.True(t, byID(rs)[c.id].Pass, "pass value %v", c.pass)
			require.True(t, AllPass(rs))

			o[c.key] = c.fail
			rs = Evaluate(g1, o)
			m := byID(rs)
			require.False(t, m[c.id].Pass, "fail value %v", c.fail)
			require.Equal(t, c.fail, m[c.id].Value)
			require.NotEmpty(t, m[c.id].Threshold)
			require.False(t, AllPass(rs))
			for id, r := range m { // exactly one criterion reacts
				if id != c.id {
					require.True(t, r.Pass, id)
				}
			}
		})
	}
}

func TestEvaluate_NaNFails(t *testing.T) {
	o := good()
	o["turn_p95_s"] = math.NaN()
	r := byID(Evaluate(Specs["g1"], o))["K1"]
	require.False(t, r.Pass)
	require.Equal(t, "no data", r.Reason)
}

func TestEvaluate_MissingDataFails(t *testing.T) {
	rs := Evaluate(Specs["g1"], Observation{})
	require.Len(t, rs, 15)
	for _, r := range rs {
		require.False(t, r.Pass, r.ID)
		require.False(t, r.Skipped, r.ID)
		require.Equal(t, "no data", r.Reason, r.ID)
	}
	require.False(t, AllPass(rs))
	require.False(t, AllPass(nil))

	rs = Evaluate(Specs["g1-local"], nil)
	skipped := map[string]bool{"K10": true, "K11": true, "K12": true, "K15": true}
	for _, r := range rs {
		if skipped[r.ID] {
			require.True(t, r.Skipped, r.ID)
			require.NotEmpty(t, r.Reason, r.ID)
		} else {
			require.False(t, r.Skipped, r.ID)
			require.False(t, r.Pass, r.ID)
			require.Equal(t, "no data", r.Reason, r.ID)
		}
	}
	require.False(t, AllPass(rs))
}

func TestEvaluate_OneMissingKeyOfManyFails(t *testing.T) {
	for _, k := range []string{"rest_p95_s", "session_create_p95_s", "realtime_restarts", "dlq_depth", "soak_goroutine_drift"} {
		o := good()
		delete(o, k)
		require.False(t, AllPass(Evaluate(Specs["g1"], o)), k)
	}
}

func TestEvaluate_SkipDoesNotMaskOthers(t *testing.T) {
	o := good()
	delete(o, "pg_cpu_ratio")
	delete(o, "redis_errors")
	rs := Evaluate(Specs["g1-local"], o)
	require.True(t, AllPass(rs))
	o["turn_p95_s"] = 9
	require.False(t, AllPass(Evaluate(Specs["g1-local"], o)))
	// a skipped criterion is not evaluated even when its data is bad
	o = good()
	o["pg_cpu_ratio"] = 0.99
	require.True(t, AllPass(Evaluate(Specs["g1-local"], o)))
	require.False(t, AllPass(Evaluate(Specs["g1"], o)))
}

func TestEvaluate_RealSkipsK2(t *testing.T) {
	o := good()
	delete(o, "server_overhead_p95_s")
	rs := Evaluate(Specs["real"], o)
	r := byID(rs)["K2"]
	require.True(t, r.Skipped)
	require.False(t, r.Pass)
	require.True(t, AllPass(rs))
	require.False(t, AllPass(Evaluate(Specs["g1"], o)))
	o["server_overhead_p95_s"] = 5
	require.True(t, AllPass(Evaluate(Specs["real"], o)))
}

func TestEvaluate_G4aPodBalance(t *testing.T) {
	g4a := Specs["g4a"]
	for _, c := range []struct {
		key  string
		v    float64
		pass bool
	}{
		{"sessions_per_pod_min", 95, true}, {"sessions_per_pod_min", 94.9, false},
		{"sessions_per_pod_max", 100, true}, {"sessions_per_pod_max", 100.1, false},
	} {
		o := good()
		o[c.key] = c.v
		rs := Evaluate(g4a, o)
		require.Equal(t, c.pass, byID(rs)["K10"].Pass, "%s=%v", c.key, c.v)
		require.Equal(t, c.pass, AllPass(rs))
		// other gates ignore pod balance
		require.True(t, AllPass(Evaluate(Specs["g4b"], o)))
	}
	o := good()
	delete(o, "sessions_per_pod_min")
	r := byID(Evaluate(g4a, o))["K10"]
	require.False(t, r.Pass)
	require.Equal(t, "no data", r.Reason)
	require.True(t, AllPass(Evaluate(Specs["g4b"], o)))
}

func TestSoakDrift(t *testing.T) {
	require.InDelta(t, 0.09, SoakDrift([]float64{100, 100}, []float64{109, 109}), 1e-12)
	require.InDelta(t, 0.09, SoakDrift([]float64{100, 100}, []float64{91, 91}), 1e-12)
	require.Equal(t, 0.0, SoakDrift([]float64{50}, []float64{50}))
	require.True(t, math.IsNaN(SoakDrift([]float64{100}, nil)))
	require.True(t, math.IsNaN(SoakDrift(nil, []float64{100})))
	require.True(t, math.IsNaN(SoakDrift([]float64{0, 0}, []float64{1})))

	// NaN drift (no samples) makes K15 fail
	o := good()
	o["soak_rss_drift"] = SoakDrift([]float64{100}, nil)
	require.False(t, byID(Evaluate(Specs["soak"], o))["K15"].Pass)
}

func TestRenderMarkdown_ContainsEveryCriterion(t *testing.T) {
	o := good()
	o["turn_p95_s"] = 2
	rs := Evaluate(Specs["g1-local"], o)
	md := RenderMarkdown(Specs["g1-local"], rs, map[string]string{"date": "2026-10-17", "commit": "abc|def"})
	for _, c := range criteria {
		require.Contains(t, md, c.ID)
		require.Contains(t, md, c.Name)
	}
	require.Contains(t, md, "2026-10-17")
	require.Contains(t, md, "g1-local")
	require.Contains(t, md, "FAIL")
	require.Contains(t, md, "SKIP")
	require.Contains(t, md, "PASS")
	require.Contains(t, md, "tanpa kube/PG/Redis exporter lokal")
	require.Contains(t, md, `abc\|def`)
	require.Contains(t, md, "GAGAL")

	md = RenderMarkdown(Specs["g1"], Evaluate(Specs["g1"], good()), nil)
	require.Contains(t, md, "LULUS")
	require.NotContains(t, md, "GAGAL")
}

func TestKeys_AreExactlyTheObservationKeys(t *testing.T) {
	want := []string{
		"turn_p95_s", "server_overhead_p95_s", "bargein_stop_p95_s", "client_bargein_p95_s", "rest_p95_s",
		"session_create_p95_s", "ws_connect_ok_ratio", "session_failed_server_error_ratio", "resume_ok_ratio",
		"slow_consumer_total", "protocol_violations_total", "realtime_cpu_ratio_max", "realtime_mem_ratio_max",
		"realtime_restarts", "pg_acquire_wait_p99_s", "pg_cpu_ratio", "redis_cpu_ratio", "redis_errors",
		"outbox_lag_p99_s", "dlq_depth", "sessions_without_evaluation", "verify_violations", "soak_rss_drift",
		"soak_goroutine_drift", "sessions_per_pod_min", "sessions_per_pod_max",
	}
	require.ElementsMatch(t, want, Keys())
	require.Len(t, Keys(), len(want), "no duplicates")
	require.Len(t, good(), len(want))
}
