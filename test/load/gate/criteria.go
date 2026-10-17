package gate

import (
	"fmt"
	"strings"
)

type cmp int

const (
	le cmp = iota // v <= limit
	lt            // v <  limit
	ge            // v >= limit
	eq            // v == 0
)

type check struct {
	Key   string
	Cmp   cmp
	Limit float64
}

func (c check) ok(v float64) bool {
	switch c.Cmp {
	case le:
		return v <= c.Limit
	case lt:
		return v < c.Limit
	case ge:
		return v >= c.Limit
	default:
		return v == c.Limit
	}
}

func (c check) String() string {
	op := map[cmp]string{le: "<=", lt: "<", ge: ">=", eq: "=="}[c.Cmp]
	return fmt.Sprintf("%s %s %g", c.Key, op, c.Limit)
}

func describe(cs []check) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.String()
	}
	return strings.Join(parts, "; ")
}

type criterion struct {
	ID, Name string
	Checks   []check
	// PodChecks are added only for gates with Spec.PodBalance (G4a).
	PodChecks []check
}

// criteria is spec 13.12.4, in order. Thresholds follow the spec wording: "<=" / ">=" where it says
// "<=" / ">=", strict "<" where it says "<".
var criteria = []criterion{
	{ID: "K1", Name: "Turn end to first audio, server P95 (D-100)", Checks: []check{{"turn_p95_s", le, 1.5}}},
	{ID: "K2", Name: "Server overhead P95 (fake)", Checks: []check{{"server_overhead_p95_s", le, 0.150}}},
	{ID: "K3", Name: "Barge-in stop, server P95 (D-101)", Checks: []check{{"bargein_stop_p95_s", le, 0.150}}},
	{ID: "K4", Name: "Barge-in, client P95 (D-101)", Checks: []check{{"client_bargein_p95_s", le, 0.6}}},
	{ID: "K5", Name: "REST P95 and POST /sessions P95 (D-102)", Checks: []check{{"rest_p95_s", le, 0.2}, {"session_create_p95_s", le, 0.3}}},
	{ID: "K6", Name: "WebSocket connect success", Checks: []check{{"ws_connect_ok_ratio", ge, 0.999}}},
	{ID: "K7", Name: "Sessions FAILED (SERVER_ERROR)", Checks: []check{{"session_failed_server_error_ratio", le, 0.001}}},
	{ID: "K8", Name: "Resume success", Checks: []check{{"resume_ok_ratio", ge, 0.99}}},
	{ID: "K9", Name: "Slow consumer and protocol violations", Checks: []check{{"slow_consumer_total", eq, 0}, {"protocol_violations_total", eq, 0}}},
	{ID: "K10", Name: "Realtime resources (CPU, memory, restarts)",
		Checks:    []check{{"realtime_cpu_ratio_max", lt, 0.8}, {"realtime_mem_ratio_max", lt, 0.8}, {"realtime_restarts", eq, 0}},
		PodChecks: []check{{"sessions_per_pod_min", ge, 95}, {"sessions_per_pod_max", le, 100}}},
	{ID: "K11", Name: "PostgreSQL (acquire wait P99, CPU)", Checks: []check{{"pg_acquire_wait_p99_s", lt, 0.05}, {"pg_cpu_ratio", lt, 0.6}}},
	{ID: "K12", Name: "Redis (CPU, command errors)", Checks: []check{{"redis_cpu_ratio", lt, 0.5}, {"redis_errors", eq, 0}}},
	{ID: "K13", Name: "Async (outbox lag P99, evaluations, DLQ)",
		Checks: []check{{"outbox_lag_p99_s", lt, 5}, {"dlq_depth", eq, 0}, {"sessions_without_evaluation", eq, 0}}},
	{ID: "K14", Name: "Integrity (kotoki-verify)", Checks: []check{{"verify_violations", eq, 0}}},
	{ID: "K15", Name: "Soak drift (RSS, goroutines)", Checks: []check{{"soak_rss_drift", lt, 0.10}, {"soak_goroutine_drift", lt, 0.10}}},
}

// Keys lists every observation key the criteria read (each once, in criteria order). Collectors use
// it to prove that the keys they emit are keys the gate understands.
func Keys() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range criteria {
		for _, ck := range append(append([]check(nil), c.Checks...), c.PodChecks...) {
			if !seen[ck.Key] {
				seen[ck.Key] = true
				out = append(out, ck.Key)
			}
		}
	}
	return out
}
