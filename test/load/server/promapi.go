package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/nabePi/kotoki-backend/test/load/gate"
)

// Querier runs one instant PromQL query that yields a single number.
type Querier interface {
	Query(ctx context.Context, promql string) (float64, error)
}

// PromAPI queries the Prometheus HTTP API (/api/v1/query).
type PromAPI struct {
	BaseURL string
	Client  *http.Client // nil: a client with a 30 s timeout
}

const maxAPIBytes = 1 << 20

// Query implements Querier. An empty result, a NaN result, several series or an error status are
// errors: the caller must treat them as "no data".
func (p PromAPI) Query(ctx context.Context, promql string) (float64, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint, err := url.JoinPath(p.BaseURL, "api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("server: prometheus url: %w", err)
	}
	form := url.Values{"query": {promql}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+form.Encode(), nil)
	if err != nil {
		return 0, fmt.Errorf("server: prometheus request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("server: prometheus query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if err != nil {
		return 0, fmt.Errorf("server: prometheus read: %w", err)
	}
	var out struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("server: prometheus status %d", resp.StatusCode)
		}
		return 0, fmt.Errorf("server: prometheus decode: %w", err)
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("server: prometheus %s: %s (status %d)", out.ErrorType, out.Error, resp.StatusCode)
	}
	var raw string
	switch out.Data.ResultType {
	case "vector":
		var vec []struct {
			Value [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(out.Data.Result, &vec); err != nil {
			return 0, fmt.Errorf("server: prometheus decode vector: %w", err)
		}
		switch len(vec) {
		case 0:
			return 0, errors.New("server: prometheus: no data")
		case 1:
		default:
			return 0, fmt.Errorf("server: prometheus: expected 1 series, got %d series", len(vec))
		}
		if err := json.Unmarshal(vec[0].Value[1], &raw); err != nil {
			return 0, fmt.Errorf("server: prometheus decode sample: %w", err)
		}
	case "scalar":
		var pair [2]json.RawMessage
		if err := json.Unmarshal(out.Data.Result, &pair); err != nil {
			return 0, fmt.Errorf("server: prometheus decode scalar: %w", err)
		}
		if err := json.Unmarshal(pair[1], &raw); err != nil {
			return 0, fmt.Errorf("server: prometheus decode sample: %w", err)
		}
	default:
		return 0, fmt.Errorf("server: prometheus: unsupported result type %q", out.Data.ResultType)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("server: prometheus: bad sample value %q", raw)
	}
	if math.IsNaN(v) {
		return 0, errors.New("server: prometheus: no data (NaN)")
	}
	return v, nil
}

func seconds(window time.Duration) string {
	s := int64(window / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10) + "s"
}

const (
	rtSel  = `namespace="kotoki",pod=~"kotoki-realtime-.*",container="realtime"`
	rtOnly = `(count(kotoki_realtime_session_capacity) > 0)`
)

// PromQL returns, per Observation key, the instant query that answers it over the trailing window
// (the whole measured hold, evaluated at the end of the run). Keys that Prometheus cannot answer
// are absent: client_bargein_p95_s and resume_ok_ratio (generator), verify_violations and
// sessions_without_evaluation (kotoki-verify), soak_*_drift (SoakDrift), and pg_cpu_ratio, whose
// source is the managed-database metric of the environment (MANUAL GATE, no exporter in the repo).
// Counters that only get a series on failure are guarded with `or vector(0)` and tied to realtime
// pods existing, so an empty Prometheus is "no data" and a healthy system is 0.
func PromQL(window time.Duration) map[string]string {
	w := "[" + seconds(window) + "]"
	hq := func(q, name, sel string) string {
		return fmt.Sprintf(`histogram_quantile(%s, sum by (le) (increase(%s_bucket%s%s)))`, q, name, sel, w)
	}
	healthyZero := func(expr string) string { return fmt.Sprintf(`(%s or vector(0)) and on() %s`, expr, rtOnly) }
	inc := func(name, sel string) string { return fmt.Sprintf(`sum(increase(%s%s%s))`, name, sel, w) }
	return map[string]string{
		"turn_p95_s":            hq("0.95", mTurn, `{kind="reply"}`),
		"server_overhead_p95_s": hq("0.95", mOverhead, ""),
		"bargein_stop_p95_s":    hq("0.95", mBargeStop, ""),
		"rest_p95_s":            hq("0.95", mHTTP, `{service="kotoki-api",route!~"/api/v1/realtime.*"}`),
		"session_create_p95_s":  hq("0.95", mHTTP, `{service="kotoki-api",route="/api/v1/sessions",method="POST"}`),
		"ws_connect_ok_ratio": inc(mConnections, `{result="accepted"}`) + " / " +
			inc(mConnections, `{result!~"rejected_ratelimit|rejected_ticket|rejected_draining"}`),
		"session_failed_server_error_ratio": "(" + inc(mEnded, `{end_reason="SERVER_ERROR"}`) + " or vector(0)) / " + inc(mEnded, ""),
		"slow_consumer_total":               inc(mSlow, ""),
		"protocol_violations_total":         healthyZero(inc(mViolations, "")),
		"realtime_cpu_ratio_max": fmt.Sprintf(`max(sum by (pod) (rate(container_cpu_usage_seconds_total{%s}%s)) / sum by (pod) (kube_pod_container_resource_limits{%s,resource="cpu"}))`,
			rtSel, w, rtSel),
		"realtime_mem_ratio_max": fmt.Sprintf(`max(max by (pod) (max_over_time(container_memory_working_set_bytes{%s}%s)) / max by (pod) (kube_pod_container_resource_limits{%s,resource="memory"}))`,
			rtSel, w, rtSel),
		"realtime_restarts":     fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{%s}%s))`, rtSel, w),
		"pg_acquire_wait_p99_s": hq("0.99", "kotoki_db_pool_acquire_wait_seconds", ""),
		"redis_cpu_ratio":       fmt.Sprintf(`max(rate(redis_cpu_sys_seconds_total%s) + rate(redis_cpu_user_seconds_total%s))`, w, w),
		"redis_errors":          healthyZero(inc("kotoki_redis_command_errors_total", "")),
		"outbox_lag_p99_s":      fmt.Sprintf(`quantile_over_time(0.99, max(kotoki_outbox_oldest_pending_age_seconds)[%s:15s])`, seconds(window)),
		"dlq_depth":             `sum(kotoki_rabbitmq_queue_messages{queue=~".*[.]dlq"})`,
		"sessions_per_pod_min":  `min(sum by (pod) (kotoki_realtime_sessions_active{namespace="kotoki"}))`,
		"sessions_per_pod_max":  `max(sum by (pod) (kotoki_realtime_sessions_active{namespace="kotoki"}))`,
	}
}

// FromProm runs every PromQL query of the window and builds an Observation from the answers. A query
// that fails or returns no data leaves its key out (the gate then fails it as "no data") and adds an
// error naming the key; the other keys are still filled.
func FromProm(ctx context.Context, q Querier, window time.Duration) (gate.Observation, []error) {
	exprs := PromQL(window)
	keys := make([]string, 0, len(exprs))
	for k := range exprs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := gate.Observation{}
	var errs []error
	for _, k := range keys {
		v, err := q.Query(ctx, exprs[k])
		if err == nil && math.IsNaN(v) {
			err = errors.New("no data (NaN)")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
			continue
		}
		o[k] = v
	}
	return o, errs
}
