package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/load/gate"
	"github.com/nabePi/kotoki-backend/test/load/hist"
	"github.com/nabePi/kotoki-backend/test/load/promtext"
	"github.com/nabePi/kotoki-backend/test/tools/metricref"
)

const metricsFile = "../../../deploy/k8s/hack/metrics.txt"

var testLes = []string{"0.05", "0.1", "0.2", "0.3", "1", "+Inf"}

// histText renders a cumulative histogram from per-bucket counts (parallel to testLes).
func histText(name, labels string, per []float64) string {
	var b strings.Builder
	cum := 0.0
	for i, le := range testLes {
		cum += per[i]
		sep := ""
		if labels != "" {
			sep = labels + ","
		}
		fmt.Fprintf(&b, "%s_bucket{%sle=%q} %s\n", name, sep, le, strconv.FormatFloat(cum, 'f', -1, 64))
	}
	return b.String()
}

func parse(t *testing.T, s string) promtext.Samples {
	t.Helper()
	ss, err := promtext.Parse(strings.NewReader(s))
	require.NoError(t, err)
	return ss
}

// snapshot builds one scrape. mult scales the counters so that before (mult 1) and after (mult 3)
// differ by a known delta of 2x the base values.
func snapshot(t *testing.T, mult float64) promtext.Samples {
	var b strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	// all 100 turn observations of a reply in (0.1, 0.2]; opening turns would be 10 s if (wrongly) included.
	b.WriteString(histText("kotoki_realtime_turn_end_to_first_audio_seconds", `kind="reply",used_fallback="false"`, []float64{0, 0, 100 * mult, 0, 0, 0}))
	b.WriteString(histText("kotoki_realtime_turn_end_to_first_audio_seconds", `kind="opening",used_fallback="false"`, []float64{0, 0, 0, 0, 0, 100 * mult}))
	b.WriteString(histText("kotoki_realtime_server_overhead_seconds", "", []float64{100 * mult, 0, 0, 0, 0, 0}))
	b.WriteString(histText("kotoki_realtime_bargein_stop_seconds", "", []float64{0, 0, 100 * mult, 0, 0, 0}))
	// REST: GET /me in (0.05,0.1], POST /sessions in (0.1,0.2]; realtime route and other service are noise.
	b.WriteString(histText("kotoki_http_request_duration_seconds", `service="kotoki-api",method="GET",route="/api/v1/me"`, []float64{0, 100 * mult, 0, 0, 0, 0}))
	b.WriteString(histText("kotoki_http_request_duration_seconds", `service="kotoki-api",method="POST",route="/api/v1/sessions"`, []float64{0, 0, 100 * mult, 0, 0, 0}))
	b.WriteString(histText("kotoki_http_request_duration_seconds", `service="kotoki-api",method="GET",route="/api/v1/realtime"`, []float64{0, 0, 0, 0, 0, 500 * mult}))
	b.WriteString(histText("kotoki_http_request_duration_seconds", `service="kotoki-realtime",method="GET",route="/api/v1/me"`, []float64{0, 0, 0, 0, 0, 500 * mult}))
	// connections over the delta: 900 accepted, 6 real rejections; ratelimit/ticket/draining must not count
	for res, n := range map[string]float64{"accepted": 450, "rejected_capacity": 1, "rejected_unavailable": 1, "rejected_state": 0.5, "rejected_origin": 0.5,
		"rejected_ratelimit": 25, "rejected_ticket": 3, "rejected_draining": 2} {
		f("kotoki_realtime_connections_total{result=%q} %g\n", res, n*mult)
	}
	// sessions ended: 1000 over the delta, 2 SERVER_ERROR (across two services/statuses)
	f("kotoki_sessions_ended_total{service=\"kotoki-realtime\",end_reason=\"SERVER_ERROR\",status=\"FAILED\"} %g\n", 0.5*mult)
	f("kotoki_sessions_ended_total{service=\"kotoki-worker\",end_reason=\"SERVER_ERROR\",status=\"FAILED\"} %g\n", 0.5*mult)
	f("kotoki_sessions_ended_total{service=\"kotoki-realtime\",end_reason=\"DURATION_ELAPSED\",status=\"COMPLETED\"} %g\n", 499*mult)
	f("kotoki_sessions_ended_total{service=\"kotoki-realtime\",end_reason=\"USER_ENDED\",status=\"COMPLETED\"} %g\n", 0*mult)
	f("kotoki_sessions_ended_total{service=\"kotoki-realtime\",end_reason=\"USER_ENDED\",status=\"CANCELLED\"} %g\n", 0*mult)
	f("kotoki_sessions_ended_total{service=\"kotoki-api\",end_reason=\"USER_CANCELLED\",status=\"CANCELLED\"} %g\n", 0*mult)
	f("kotoki_sessions_ended_total{service=\"kotoki-api\",end_reason=\"OTHER\",status=\"COMPLETED\"} %g\n", 0*mult)
	f("kotoki_realtime_slow_consumer_total %g\n", 1+1*mult)                             // delta 2
	f("kotoki_realtime_protocol_violations_total{kind=\"bad_frame\"} %g\n", 3+0.5*mult) // delta 1
	f("kotoki_realtime_protocol_violations_total{kind=\"bad_json\"} %g\n", 0*mult)      // delta 0
	f("kotoki_rabbitmq_queue_messages{queue=\"evaluation.dlq\"} %g\n", 2.0)
	f("kotoki_rabbitmq_queue_messages{queue=\"memory.dlq\"} %g\n", 1.0)
	f("kotoki_rabbitmq_queue_messages{queue=\"evaluation\"} %g\n", 50.0)
	f("kotoki_realtime_sessions_active{state=\"ACTIVE\"} 90\n")
	return parse(t, b.String())
}

func TestFromScrape_MapsEveryKey(t *testing.T) {
	before, after := snapshot(t, 1), snapshot(t, 3)
	agg := ClientAgg{
		ClientBargeIn: histOf(0.5),
		Resume:        histOf(2),
		ConnectOK:     98, ConnectTotal: 100,
		ResumeOK: 9, ResumeTotal: 10,
		OutboxLagSamples: seq(1, 100),
	}
	o := FromScrape(before, after, agg)

	want := map[string]float64{
		"turn_p95_s":                        0.1 + 0.1*0.95,               // reply only, delta 200 in (0.1,0.2]
		"server_overhead_p95_s":             0.05 * 0.95,                  // first bucket, lower bound 0
		"bargein_stop_p95_s":                0.1 + 0.1*0.95,               // (0.1,0.2]
		"rest_p95_s":                        0.1 + 0.1*(0.95*400-200)/200, // /me 200 in (0.05,0.1], POST 200 in (0.1,0.2]; realtime route and service excluded
		"session_create_p95_s":              0.1 + 0.1*0.95,
		"ws_connect_ok_ratio":               900.0 / 906.0, // ratelimit/ticket/draining are not failures of the system
		"session_failed_server_error_ratio": 2.0 / 1000.0,
		"slow_consumer_total":               2,
		"protocol_violations_total":         1,
		"dlq_depth":                         3,
		"outbox_lag_p99_s":                  99,
		"resume_ok_ratio":                   0.9,
	}
	for k, v := range want {
		got, ok := o[k]
		require.True(t, ok, "missing %s", k)
		require.InDelta(t, v, got, 1e-9, k)
	}
	require.InEpsilon(t, 0.5, o["client_bargein_p95_s"], 0.01) // histogram relative error
	// Not measurable from a scrape: absent, never 0 (K10-K12, K14, K15 and the pod balance come from elsewhere).
	for _, k := range []string{"realtime_cpu_ratio_max", "realtime_mem_ratio_max", "realtime_restarts", "pg_acquire_wait_p99_s", "pg_cpu_ratio",
		"redis_cpu_ratio", "redis_errors", "sessions_without_evaluation", "verify_violations", "soak_rss_drift", "soak_goroutine_drift",
		"sessions_per_pod_min", "sessions_per_pod_max"} {
		_, ok := o[k]
		require.False(t, ok, "%s must be absent", k)
	}
	known := map[string]bool{}
	for _, k := range gate.Keys() {
		known[k] = true
	}
	for k := range o {
		require.True(t, known[k], "FromScrape emits %q, which no gate criterion reads", k)
	}
	require.Len(t, o, len(want)+1)
}

func histOf(vs ...float64) *hist.Histogram {
	h := hist.New()
	for _, v := range vs {
		h.Record(v)
	}
	return h
}

func seq(from, to int) []float64 {
	var out []float64
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}

func TestFromScrape_NoDataIsAbsentNotZero(t *testing.T) {
	same := snapshot(t, 1)
	o := FromScrape(same, same, ClientAgg{})
	for _, k := range []string{"turn_p95_s", "server_overhead_p95_s", "bargein_stop_p95_s", "rest_p95_s", "session_create_p95_s",
		"ws_connect_ok_ratio", "session_failed_server_error_ratio", "client_bargein_p95_s", "resume_ok_ratio", "outbox_lag_p99_s"} {
		_, ok := o[k]
		require.False(t, ok, "%s must be absent when nothing was observed", k)
	}
	rs := gate.Evaluate(gate.Specs["g1"], o)
	for _, r := range rs {
		if r.ID == "K1" || r.ID == "K2" || r.ID == "K3" || r.ID == "K4" || r.ID == "K5" || r.ID == "K6" || r.ID == "K7" || r.ID == "K8" {
			require.False(t, r.Pass, r.ID)
			require.Equal(t, "no data", r.Reason, r.ID)
		}
	}
	// nothing scraped at all: no key, K9 and K13 included
	empty := FromScrape(nil, nil, ClientAgg{})
	require.Empty(t, empty)
	// empty client histograms and zero totals are "no data" too
	o = FromScrape(nil, nil, ClientAgg{ClientBargeIn: hist.New(), Resume: hist.New()})
	require.Empty(t, o)
}

func TestFromScrape_RealtimeScrapedWithoutViolationSeriesIsZero(t *testing.T) {
	// kotoki_realtime_protocol_violations_total is a CounterVec: it has no series until a violation
	// happens. A healthy realtime pod is therefore "0", but only if realtime was really scraped.
	after := parse(t, "kotoki_realtime_session_capacity 100\nkotoki_realtime_slow_consumer_total 0\n")
	o := FromScrape(nil, after, ClientAgg{})
	require.Equal(t, 0.0, o["protocol_violations_total"])
	require.Equal(t, 0.0, o["slow_consumer_total"])
	o = FromScrape(nil, parse(t, "kotoki_http_requests_total 1\n"), ClientAgg{})
	_, ok := o["protocol_violations_total"]
	require.False(t, ok)
	_, ok = o["slow_consumer_total"]
	require.False(t, ok)
}

func TestFromScrape_CounterResetIsNoData(t *testing.T) {
	before := parse(t, "kotoki_realtime_session_capacity 1\nkotoki_realtime_slow_consumer_total 10\n"+
		`kotoki_realtime_connections_total{result="accepted"} 500`+"\n"+
		`kotoki_sessions_ended_total{service="s",end_reason="SERVER_ERROR",status="FAILED"} 9`+"\n")
	after := parse(t, "kotoki_realtime_session_capacity 1\nkotoki_realtime_slow_consumer_total 2\n"+
		`kotoki_realtime_connections_total{result="accepted"} 100`+"\n"+
		`kotoki_sessions_ended_total{service="s",end_reason="OK",status="COMPLETED"} 50`+"\n")
	o := FromScrape(before, after, ClientAgg{})
	for _, k := range []string{"slow_consumer_total", "ws_connect_ok_ratio", "session_failed_server_error_ratio"} {
		_, ok := o[k]
		require.False(t, ok, k)
	}
}

func TestFromScrape_ConnectRatioExcludesNonFailures(t *testing.T) {
	after := parse(t, `kotoki_realtime_connections_total{result="accepted"} 99
kotoki_realtime_connections_total{result="rejected_capacity"} 1
kotoki_realtime_connections_total{result="rejected_ratelimit"} 1000
kotoki_realtime_connections_total{result="rejected_ticket"} 1000
kotoki_realtime_connections_total{result="rejected_draining"} 1000
`)
	o := FromScrape(nil, after, ClientAgg{})
	require.InDelta(t, 0.99, o["ws_connect_ok_ratio"], 1e-12)
	// only excluded results: nothing to divide
	o = FromScrape(nil, parse(t, `kotoki_realtime_connections_total{result="rejected_ratelimit"} 5`), ClientAgg{})
	_, ok := o["ws_connect_ok_ratio"]
	require.False(t, ok)
}

func TestFromScrape_DLQ(t *testing.T) {
	// DLQ gauges are read from "after" only (a gauge); the plain queues do not count
	o := FromScrape(nil, parse(t, "kotoki_rabbitmq_queue_messages{queue=\"a.dlq\"} 0\nkotoki_rabbitmq_queue_messages{queue=\"a\"} 9\n"), ClientAgg{})
	require.Equal(t, 0.0, o["dlq_depth"])
	// queue gauges without any dlq queue: unknown, not 0
	o = FromScrape(nil, parse(t, "kotoki_rabbitmq_queue_messages{queue=\"a\"} 9\n"), ClientAgg{})
	_, ok := o["dlq_depth"]
	require.False(t, ok)
}

func TestFromScrape_ClientAggregates(t *testing.T) {
	o := FromScrape(nil, nil, ClientAgg{ClientBargeIn: histOf(0.2, 0.3, 0.4), ResumeOK: 99, ResumeTotal: 100, OutboxLagSamples: []float64{5, 1, 3, 2, 4}})
	require.InEpsilon(t, 0.4, o["client_bargein_p95_s"], 0.01)
	require.Equal(t, 0.99, o["resume_ok_ratio"])
	require.Equal(t, 5.0, o["outbox_lag_p99_s"], "nearest rank on a sorted copy")
	agg := ClientAgg{OutboxLagSamples: []float64{5, 1}}
	_ = FromScrape(nil, nil, agg)
	require.Equal(t, []float64{5, 1}, agg.OutboxLagSamples, "input slice is not reordered")
}

func TestPromAPI_Query(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		switch gotQuery {
		case "ok":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000.5,"1.25"]}]}}`))
		case "scalar":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000.5,"7"]}}`))
		case "err":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
		case "empty":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case "nan":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`))
		case "many":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"a":"1"},"value":[1,"1"]},{"metric":{"a":"2"},"value":[1,"2"]}]}}`))
		case "html":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`<html>bad gateway</html>`))
		case "garbage":
			_, _ = w.Write([]byte(`not json`))
		case "badval":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`))
		case "matrix":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}
	}))
	defer srv.Close()
	p := PromAPI{BaseURL: srv.URL + "/"}
	ctx := context.Background()

	v, err := p.Query(ctx, "ok")
	require.NoError(t, err)
	require.Equal(t, 1.25, v)
	require.Equal(t, "/api/v1/query", gotPath)
	v, err = p.Query(ctx, "scalar")
	require.NoError(t, err)
	require.Equal(t, 7.0, v)

	for q, sub := range map[string]string{"err": "parse error", "empty": "no data", "nan": "no data", "many": "2 series", "html": "502",
		"garbage": "decode", "badval": "abc", "matrix": "matrix"} {
		_, err = p.Query(ctx, q)
		require.Error(t, err, q)
		require.Contains(t, err.Error(), sub, q)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = p.Query(cctx, "ok")
	require.Error(t, err)
	_, err = PromAPI{BaseURL: "http://127.0.0.1:1"}.Query(ctx, "ok")
	require.Error(t, err)
	_, err = PromAPI{BaseURL: "://bad"}.Query(ctx, "ok")
	require.Error(t, err)
}

func TestScraper_Snapshot(t *testing.T) {
	mk := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/metrics", r.URL.Path)
			_, _ = w.Write([]byte(body))
		}))
	}
	a, b := mk("m 1\n"), mk("m 2\nn 5\n")
	defer a.Close()
	defer b.Close()
	ss, err := Scraper{URLs: []string{a.URL + "/metrics", b.URL + "/metrics"}}.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3.0, ss.Sum("m", nil))
	require.Equal(t, 5.0, ss.Sum("n", nil))

	var _ Source = Scraper{}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("m 1\n")) // a parseable body must not rescue a failing status
	}))
	defer bad.Close()
	_, err = Scraper{URLs: []string{a.URL + "/metrics", bad.URL + "/metrics"}}.Snapshot(context.Background())
	require.Error(t, err, "one failing URL must fail the snapshot, not be skipped")
	require.Contains(t, err.Error(), bad.URL)

	garbage := mk("this is not exposition\n")
	defer garbage.Close()
	_, err = Scraper{URLs: []string{garbage.URL + "/metrics"}}.Snapshot(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), garbage.URL)

	_, err = Scraper{}.Snapshot(context.Background())
	require.Error(t, err, "no URLs is a configuration error, not an empty scrape")
	_, err = Scraper{URLs: []string{"http://127.0.0.1:1/metrics"}}.Snapshot(context.Background())
	require.Error(t, err)
	_, err = Scraper{URLs: []string{"://bad"}}.Snapshot(context.Background())
	require.Error(t, err)

	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = Scraper{URLs: []string{slow.URL + "/metrics"}}.Snapshot(ctx)
	require.Error(t, err)
}

func TestPromQL_Window(t *testing.T) {
	q := PromQL(30 * time.Minute)
	require.Contains(t, q["turn_p95_s"], `histogram_quantile(0.95, sum by (le) (increase(kotoki_realtime_turn_end_to_first_audio_seconds_bucket{kind="reply"}[1800s])))`)
	for k, e := range q {
		require.NotContains(t, e, "<window>", k)
		require.Equal(t, strings.Count(e, "["), strings.Count(e, "]"), k)
		require.Equal(t, strings.Count(e, "("), strings.Count(e, ")"), k)
		require.Equal(t, strings.Count(e, "{"), strings.Count(e, "}"), k)
	}
	require.Contains(t, PromQL(90 * time.Second)["turn_p95_s"], "[90s]")
	require.Contains(t, PromQL(0)["turn_p95_s"], "[1s]", "a non-positive window is clamped, never emitted as [0s]")
}

func TestPromQL_ReferencesKnownMetrics(t *testing.T) {
	known, err := metricref.LoadAllowed(metricsFile)
	require.NoError(t, err)
	member := map[string]bool{} // LoadAllowed maps name -> optional; Resolve wants membership
	for name := range known {
		member[name] = true
	}
	gateKeys := map[string]bool{}
	for _, k := range gate.Keys() {
		gateKeys[k] = true
	}
	q := PromQL(10 * time.Minute)
	require.NotEmpty(t, q)
	for key, expr := range q {
		require.True(t, gateKeys[key], "PromQL key %q is not a gate observation key", key)
		for _, tok := range metricref.Extract(expr) {
			_, ok := metricref.Resolve(tok, member)
			require.True(t, ok, "%s: %s is not in metrics.txt", key, tok)
		}
	}
	for _, name := range ScrapeMetrics {
		_, ok := known[name]
		require.True(t, ok, "scrape metric %s is not in metrics.txt", name)
	}
	// the scrape path and the PromQL path cover the same server-side keys of K1-K9 and K13
	for _, k := range []string{"turn_p95_s", "server_overhead_p95_s", "bargein_stop_p95_s", "rest_p95_s", "session_create_p95_s",
		"ws_connect_ok_ratio", "session_failed_server_error_ratio", "slow_consumer_total", "protocol_violations_total",
		"outbox_lag_p99_s", "dlq_depth", "realtime_cpu_ratio_max", "realtime_mem_ratio_max", "realtime_restarts",
		"pg_acquire_wait_p99_s", "redis_cpu_ratio", "redis_errors", "sessions_per_pod_min", "sessions_per_pod_max"} {
		require.Contains(t, q, k)
	}
	for _, k := range []string{"client_bargein_p95_s", "resume_ok_ratio", "verify_violations", "sessions_without_evaluation", "soak_rss_drift", "soak_goroutine_drift"} {
		require.NotContains(t, q, k, "%s is measured by the client/verify, not by Prometheus", k)
	}
}

type fakeQuerier struct {
	vals  map[string]float64
	errs  map[string]error
	calls []string
}

func (f *fakeQuerier) Query(_ context.Context, q string) (float64, error) {
	f.calls = append(f.calls, q)
	if err := f.errs[q]; err != nil {
		return 0, err
	}
	v, ok := f.vals[q]
	if !ok {
		return 0, errors.New("no data")
	}
	return v, nil
}

func TestFromProm(t *testing.T) {
	win := 20 * time.Minute
	exprs := PromQL(win)
	f := &fakeQuerier{vals: map[string]float64{}, errs: map[string]error{}}
	for k, e := range exprs {
		f.vals[e] = float64(len(k))
	}
	f.errs[exprs["dlq_depth"]] = errors.New("boom")
	delete(f.vals, exprs["redis_cpu_ratio"]) // fake answers "no data"
	f.vals[exprs["turn_p95_s"]] = math.NaN() // NaN from Prometheus is not data either

	o, errs := FromProm(context.Background(), f, win)
	require.Len(t, f.calls, len(exprs))
	require.Len(t, errs, 3)
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	sort.Strings(msgs)
	require.Contains(t, msgs[0], "dlq_depth")
	require.Contains(t, msgs[0], "boom")
	require.Contains(t, msgs[1], "redis_cpu_ratio")
	require.Contains(t, msgs[2], "turn_p95_s")
	for _, k := range []string{"dlq_depth", "redis_cpu_ratio", "turn_p95_s"} {
		_, ok := o[k]
		require.False(t, ok, "%s must be absent, not 0", k)
	}
	require.Equal(t, float64(len("rest_p95_s")), o["rest_p95_s"])
	require.Len(t, o, len(exprs)-3)
	// the gate sees the missing keys as "no data"
	r := gate.Evaluate(gate.Specs["g1"], o)[0]
	require.Equal(t, "K1", r.ID)
	require.False(t, r.Pass)
	require.Equal(t, "no data", r.Reason)
}

func TestPromQL_GuardsHealthyZeroSeries(t *testing.T) {
	// counters without series until a failure ("?" in metrics.txt) must read 0 only while realtime is
	// scraped, never when Prometheus has nothing at all.
	q := PromQL(time.Minute)
	for _, k := range []string{"protocol_violations_total", "redis_errors", "session_failed_server_error_ratio"} {
		require.Contains(t, q[k], "or vector(0)", k)
	}
	require.Contains(t, q["protocol_violations_total"], "kotoki_realtime_session_capacity")
	require.Contains(t, q["redis_errors"], "kotoki_realtime_session_capacity")
	vz := `(sum(increase(kotoki_realtime_protocol_violations_total[60s])) or vector(0)) and on() (count(kotoki_realtime_session_capacity) > 0)`
	require.Equal(t, vz, q["protocol_violations_total"])
	require.Equal(t, strings.Replace(vz, "kotoki_realtime_protocol_violations_total", "kotoki_redis_command_errors_total", 1), q["redis_errors"])
	require.Equal(t, `sum(kotoki_rabbitmq_queue_messages{queue=~".*[.]dlq"})`, q["dlq_depth"])
	// the denominator of the ratio stays unguarded so an empty system is "no data"
	require.NotContains(t, strings.SplitN(q["session_failed_server_error_ratio"], ") / ", 2)[1], "vector(0)")
}
