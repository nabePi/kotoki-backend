package metrics_test

import (
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
)

func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuckets(t *testing.T) {
	cases := map[string][2][]float64{
		"http":  {metrics.HTTPBuckets, {0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5}},
		"turn":  {metrics.TurnBuckets, {0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.8, 1, 1.25, 1.5, 2, 3, 5, 10}},
		"barge": {metrics.BargeBuckets, {0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 0.6, 1}},
		"async": {metrics.AsyncBuckets, {0.1, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600}},
	}
	for n, c := range cases {
		if !reflect.DeepEqual(c[0], c[1]) {
			t.Errorf("%s buckets = %v, want %v", n, c[0], c[1])
		}
	}
}

func TestRegistryHasRuntimeMetrics(t *testing.T) {
	out := scrape(t, metrics.NewRegistry())
	for _, s := range []string{"go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %s", s)
		}
	}
}

func TestDBMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	db := metrics.NewDB(reg, func() metrics.PoolStat { return metrics.PoolStat{Acquired: 3, Idle: 2, Max: 10} })
	db.ObserveQuery("InsertSession", 30*time.Millisecond)
	db.ObserveAcquireWait(2 * time.Millisecond)
	out := scrape(t, reg)
	for _, s := range []string{
		"kotoki_db_pool_acquired_conns 3",
		"kotoki_db_pool_idle_conns 2",
		"kotoki_db_pool_max_conns 10",
		`kotoki_db_query_duration_seconds_bucket{query="InsertSession",le="0.05"} 1`,
		`kotoki_db_pool_acquire_wait_seconds_bucket{le="0.005"} 1`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
}

func TestRedisMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	r := metrics.NewRedis(reg)
	r.CommandError("get")
	r.CommandError("get")
	r.PoolTimeout()
	out := scrape(t, reg)
	for _, s := range []string{
		`kotoki_redis_command_errors_total{op="get"} 2`,
		"kotoki_redis_pool_timeouts_total 1",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q", s)
		}
	}
}

func TestNoForbiddenLabels(t *testing.T) {
	reg := metrics.NewRegistry()
	db := metrics.NewDB(reg, func() metrics.PoolStat { return metrics.PoolStat{} })
	db.ObserveQuery("q", time.Millisecond)
	db.ObserveAcquireWait(time.Millisecond)
	r := metrics.NewRedis(reg)
	r.CommandError("get")
	r.PoolTimeout()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "user_id", "session_id", "turn_id", "email":
					t.Errorf("forbidden label %s on %s", l.GetName(), mf.GetName())
				}
			}
		}
	}
}
