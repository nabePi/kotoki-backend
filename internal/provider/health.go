package provider

import (
	"sort"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const (
	healthBuckets     = 60
	maxLatencySamples = 512 // per bucket; enough for p95 at realtime rates
)

type healthKey struct {
	p Name
	c Capability
}

type healthBucket struct {
	sec               int64
	requests          int
	successes         int
	timeouts          int
	rateLimited       int
	streamDisconnects int
	fallbacksFrom     int
	failures          map[ErrorKind]int
	latencies         []time.Duration
}

type healthEntry struct {
	buckets     [healthBuckets]healthBucket
	active      int
	lastErrKind ErrorKind
	lastErrAt   time.Time
}

// HealthTracker keeps a rolling 60 s window (60 buckets of 1 s) per (vendor, capability).
// It is observation only: it never changes routing and never affects readiness.
type HealthTracker struct {
	clk     clock.Clock
	mu      sync.Mutex
	entries map[healthKey]*healthEntry
}

// NewHealthTracker returns an empty tracker.
func NewHealthTracker(clk clock.Clock) *HealthTracker {
	return &HealthTracker{clk: clk, entries: map[healthKey]*healthEntry{}}
}

func (h *HealthTracker) entry(p Name, c Capability) *healthEntry {
	k := healthKey{p, c}
	e := h.entries[k]
	if e == nil {
		e = &healthEntry{}
		h.entries[k] = e
	}
	return e
}

// bucket returns the current second's bucket, resetting it when it holds an older second.
func (h *HealthTracker) bucket(e *healthEntry) *healthBucket {
	now := h.clk.Now()
	sec := now.Unix()
	b := &e.buckets[sec%healthBuckets]
	if b.sec != sec {
		*b = healthBucket{sec: sec}
	}
	return b
}

// Record notes one finished call. A nil err is a success; CANCELLED is neither a success
// nor a failure.
func (h *HealthTracker) Record(p Name, c Capability, err error, latency time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(p, c)
	b := h.bucket(e)
	b.requests++
	if err == nil {
		b.successes++
		if len(b.latencies) < maxLatencySamples {
			b.latencies = append(b.latencies, latency)
		}
		return
	}
	kind := KindOf(err)
	if kind == KindCancelled {
		return
	}
	if b.failures == nil {
		b.failures = map[ErrorKind]int{}
	}
	b.failures[kind]++
	switch kind {
	case KindTimeout:
		b.timeouts++
	case KindRateLimited:
		b.rateLimited++
	}
	e.lastErrKind, e.lastErrAt = kind, h.clk.Now()
}

// IncActive and DecActive track in-flight streams/requests.
func (h *HealthTracker) IncActive(p Name, c Capability) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entry(p, c).active++
}

// DecActive is the counterpart of IncActive.
func (h *HealthTracker) DecActive(p Name, c Capability) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entry(p, c); e.active > 0 {
		e.active--
	}
}

// Fallback notes that a TTS call left `from` for the character's fallback voice.
func (h *HealthTracker) Fallback(from Name, c Capability) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bucket(h.entry(from, c)).fallbacksFrom++
}

// StreamDisconnect notes a stream that broke after it started.
func (h *HealthTracker) StreamDisconnect(p Name, c Capability) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bucket(h.entry(p, c)).streamDisconnects++
}

// Window60s is the aggregated rolling window.
type Window60s struct {
	Requests          int               `json:"requests"`
	Successes         int               `json:"successes"`
	Failures          map[ErrorKind]int `json:"failures"`
	P95LatencyMs      *int64            `json:"p95_latency_ms"`
	Timeouts          int               `json:"timeouts,omitempty"`
	RateLimited       int               `json:"rate_limited,omitempty"`
	StreamDisconnects int               `json:"stream_disconnects,omitempty"`
	FallbacksFrom     int               `json:"fallbacks_from,omitempty"`
}

// LastError is the most recent counted failure.
type LastError struct {
	Kind ErrorKind `json:"kind"`
	At   time.Time `json:"at"`
}

// ProviderHealth is one row of the debug snapshot (spec 8.5.5).
type ProviderHealth struct { //nolint:revive // name fixed by the plan (Snapshot() []ProviderHealth)
	Provider         Name       `json:"provider"`
	Capability       Capability `json:"capability"`
	Circuit          string     `json:"circuit"`
	Active           int        `json:"active"`
	ConcurrencyLimit int        `json:"concurrency_limit"`
	Window           Window60s  `json:"window_60s"`
	LastError        *LastError `json:"last_error,omitempty"`
}

// String renders the state as in the debug endpoint.
func (s BreakerState) String() string {
	switch s {
	case BreakerOpen:
		return "OPEN"
	case BreakerHalfOpen:
		return "HALF_OPEN"
	default:
		return "CLOSED"
	}
}

// Snapshot returns one row per tracked (vendor, capability), sorted by provider then capability.
func (h *HealthTracker) Snapshot(limits func(Name, Capability) int, circuits func(Name, Capability) BreakerState) []ProviderHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	nowSec := h.clk.Now().Unix()
	out := make([]ProviderHealth, 0, len(h.entries))
	for k, e := range h.entries {
		row := ProviderHealth{
			Provider: k.p, Capability: k.c, Active: e.active,
			ConcurrencyLimit: limits(k.p, k.c), Circuit: circuits(k.p, k.c).String(),
			Window: Window60s{Failures: map[ErrorKind]int{}},
		}
		var lat []time.Duration
		for i := range e.buckets {
			b := &e.buckets[i]
			if b.sec == 0 || b.sec <= nowSec-healthBuckets || b.sec > nowSec {
				continue
			}
			row.Window.Requests += b.requests
			row.Window.Successes += b.successes
			row.Window.Timeouts += b.timeouts
			row.Window.RateLimited += b.rateLimited
			row.Window.StreamDisconnects += b.streamDisconnects
			row.Window.FallbacksFrom += b.fallbacksFrom
			for kind, n := range b.failures {
				row.Window.Failures[kind] += n
			}
			lat = append(lat, b.latencies...)
		}
		if len(lat) > 0 {
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			p := lat[(len(lat)*95+99)/100-1].Milliseconds()
			row.Window.P95LatencyMs = &p
		}
		if e.lastErrKind != "" {
			row.LastError = &LastError{Kind: e.lastErrKind, At: e.lastErrAt.UTC()}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Capability < out[j].Capability
	})
	return out
}
