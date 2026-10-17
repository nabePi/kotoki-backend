package server

import (
	"math"
	"sort"
	"strings"

	"github.com/nabePi/kotoki-backend/test/load/gate"
	"github.com/nabePi/kotoki-backend/test/load/hist"
	"github.com/nabePi/kotoki-backend/test/load/promtext"
)

// Metric names read by FromScrape. TestPromQL_ReferencesKnownMetrics keeps them in metrics.txt.
const (
	mTurn        = "kotoki_realtime_turn_end_to_first_audio_seconds"
	mOverhead    = "kotoki_realtime_server_overhead_seconds"
	mBargeStop   = "kotoki_realtime_bargein_stop_seconds"
	mHTTP        = "kotoki_http_request_duration_seconds"
	mConnections = "kotoki_realtime_connections_total"
	mEnded       = "kotoki_sessions_ended_total"
	mSlow        = "kotoki_realtime_slow_consumer_total"
	mViolations  = "kotoki_realtime_protocol_violations_total"
	mQueue       = "kotoki_rabbitmq_queue_messages"
)

// ScrapeMetrics lists the metric names FromScrape reads.
var ScrapeMetrics = []string{mTurn, mOverhead, mBargeStop, mHTTP, mConnections, mEnded, mSlow, mViolations, mQueue}

const (
	apiService     = "kotoki-api"
	realtimeRoute  = "/api/v1/realtime"
	sessionsRoute  = "/api/v1/sessions"
	realtimePrefix = "kotoki_realtime_"
)

// ClientAgg is what the generator measured itself. Histograms hold seconds.
type ClientAgg struct {
	ClientBargeIn, Resume   *hist.Histogram
	ConnectOK, ConnectTotal int
	ResumeOK, ResumeTotal   int
	OutboxLagSamples        []float64 // seconds, sampled periodically during the run
}

// FromScrape converts two scrapes (start and end of the measured window; before may be nil) and
// the client aggregate into an Observation. Only keys that could be measured are set:
//
//   - histograms need at least one observation inside the window;
//   - counters need a trustworthy delta (no series went down, which would be a restart);
//   - K10-K12 (resources, PG, Redis), K14, K15 and the pod balance are not scrape data and stay
//     absent; they come from Prometheus (FromProm), kotoki-verify and SoakDrift.
//
// Counter deltas are taken per label set, so the instance set must not change between the two
// scrapes (use PromAPI for runs where pods come and go).
func FromScrape(before, after promtext.Samples, c ClientAgg) gate.Observation {
	o := gate.Observation{}
	setQ := func(key, name string, match map[string]string, b, a promtext.Samples) {
		if v, ok := promtext.Quantile(0.95, b, a, name, match); ok {
			o[key] = v
		}
	}
	setQ("turn_p95_s", mTurn, map[string]string{"kind": "reply"}, before, after)
	setQ("server_overhead_p95_s", mOverhead, nil, before, after)
	setQ("bargein_stop_p95_s", mBargeStop, nil, before, after)

	restOnly := func(s promtext.Sample) bool {
		return s.Labels["service"] == apiService && !strings.HasPrefix(s.Labels["route"], realtimeRoute)
	}
	if v, ok := promtext.Quantile(0.95, before.Filter(restOnly), after.Filter(restOnly), mHTTP, nil); ok {
		o["rest_p95_s"] = v
	}
	setQ("session_create_p95_s", mHTTP, map[string]string{"service": apiService, "route": sessionsRoute, "method": "POST"}, before, after)

	if ds, ok := deltas(before, after, mConnections); ok {
		var accepted, counted float64
		for _, d := range ds {
			switch r := d.labels["result"]; r {
			case "rejected_ratelimit", "rejected_ticket", "rejected_draining":
				// not a failure of the system under test: client rate limit, bad/used ticket, pod draining
			default:
				counted += d.value
				if r == "accepted" {
					accepted += d.value
				}
			}
		}
		if counted > 0 {
			o["ws_connect_ok_ratio"] = accepted / counted
		}
	}
	if ds, ok := deltas(before, after, mEnded); ok {
		var total, failed float64
		for _, d := range ds {
			total += d.value
			if d.labels["end_reason"] == "SERVER_ERROR" {
				failed += d.value
			}
		}
		if total > 0 {
			o["session_failed_server_error_ratio"] = failed / total
		}
	}

	// slow_consumer and protocol_violations: a counter (vec) with no series is a healthy 0, but only
	// when realtime pods were really scraped.
	if after.HasPrefix(realtimePrefix) {
		if v, ok := deltaTotal(before, after, mSlow); ok {
			o["slow_consumer_total"] = v
		}
		if v, ok := deltaTotal(before, after, mViolations); ok {
			o["protocol_violations_total"] = v
		}
	}

	dlq := after.Filter(func(s promtext.Sample) bool { return s.Name == mQueue && strings.HasSuffix(s.Labels["queue"], ".dlq") })
	if len(dlq) > 0 {
		o["dlq_depth"] = dlq.Sum(mQueue, nil)
	}
	if v, ok := nearestRank(c.OutboxLagSamples, 0.99); ok {
		o["outbox_lag_p99_s"] = v
	}

	if c.ClientBargeIn != nil && c.ClientBargeIn.Count() > 0 {
		o["client_bargein_p95_s"] = c.ClientBargeIn.Quantile(0.95)
	}
	if c.ResumeTotal > 0 {
		o["resume_ok_ratio"] = float64(c.ResumeOK) / float64(c.ResumeTotal)
	}
	return o
}

type seriesDelta struct {
	labels map[string]string
	value  float64
}

// deltas returns, per distinct label set of name, after minus before (no series at all gives an empty
// slice). ok is false when a delta is negative or NaN: a counter reset or a vanished series, whose
// delta cannot be trusted.
func deltas(before, after promtext.Samples, name string) (out []seriesDelta, ok bool) {
	key := func(l map[string]string) string {
		ks := make([]string, 0, len(l))
		for k, v := range l {
			ks = append(ks, k+"\x00"+v)
		}
		sort.Strings(ks)
		return strings.Join(ks, "\x01")
	}
	type acc struct {
		labels map[string]string
		d      float64
	}
	m := map[string]*acc{}
	add := func(ss promtext.Samples, sign float64) {
		for _, s := range ss {
			if s.Name != name {
				continue
			}
			k := key(s.Labels)
			if m[k] == nil {
				m[k] = &acc{labels: s.Labels}
			}
			m[k].d += sign * s.Value
		}
	}
	add(before, -1)
	add(after, 1)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !(m[k].d >= 0) { // also catches NaN
			return nil, false
		}
		out = append(out, seriesDelta{labels: m[k].labels, value: m[k].d})
	}
	return out, true
}

// deltaTotal is the sum of the deltas of every series of name; a metric without series reads 0.
func deltaTotal(before, after promtext.Samples, name string) (float64, bool) {
	ds, ok := deltas(before, after, name)
	if !ok {
		return 0, false
	}
	var sum float64
	for _, d := range ds {
		sum += d.value
	}
	return sum, true
}

// nearestRank is the q-quantile of xs by the nearest-rank method on a sorted copy.
func nearestRank(xs []float64, q float64) (float64, bool) {
	if len(xs) == 0 {
		return 0, false
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := int(math.Ceil(q * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	return s[rank-1], true
}
