package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Fault modes (spec 8.9.6).
const (
	ModeError               = "error"
	ModeTimeout             = "timeout"
	ModeSlow                = "slow"
	ModeFailAfterFirstAudio = "fail_after_first_audio"
	ModeDisconnectMidStream = "disconnect_mid_stream"
)

// staleWindow is how long the last snapshot survives while the fault source errors, so a
// Redis outage cannot leave a fault stuck forever.
const staleWindow = 10 * time.Second

// Fault is one runtime fault for a (capability, instance).
type Fault struct {
	Mode    string
	Rate    float64
	AfterMs int64
	Status  int
	Kind    provider.ErrorKind
	DelayMs int
}

// FaultSet is an immutable set of runtime faults keyed by capability and instance.
type FaultSet struct{ m map[string]*Fault }

func faultKey(c provider.Capability, inst provider.Name) string {
	return string(c) + "." + string(inst)
}

// Lookup returns the fault for (c, inst); a nil set has none.
func (s *FaultSet) Lookup(c provider.Capability, inst provider.Name) (*Fault, bool) {
	if s == nil {
		return nil, false
	}
	f, ok := s.m[faultKey(c, inst)]
	return f, ok
}

// Len is the number of active faults.
func (s *FaultSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.m)
}

var emptySet = &FaultSet{}

// FaultSource supplies the current fault snapshot without I/O on the hot path.
type FaultSource interface{ Current() *FaultSet }

type noFaults struct{}

func (noFaults) Current() *FaultSet { return emptySet }

// NoFaults is the FaultSource used when nothing is wired.
var NoFaults FaultSource = noFaults{}

type faultJSON struct {
	Mode    string             `json:"mode"`
	Rate    *float64           `json:"rate"`
	AfterMs int64              `json:"after_ms"`
	Status  int                `json:"status"`
	Kind    provider.ErrorKind `json:"kind"`
	DelayMs int                `json:"delay_ms"`
}

func modeAllowed(mode string, c provider.Capability) bool {
	switch mode {
	case ModeError, ModeTimeout, ModeSlow:
		return true
	case ModeFailAfterFirstAudio:
		return c == provider.CapTTS
	case ModeDisconnectMidStream:
		return c == provider.CapSTT
	}
	return false
}

func parseKey(key string) (provider.Capability, provider.Name, bool) {
	cs, is, ok := strings.Cut(key, ".")
	if !ok {
		return "", "", false
	}
	c, inst := provider.Capability(cs), provider.Name(is)
	switch c {
	case provider.CapSTT, provider.CapLLM, provider.CapTTS, provider.CapEvaluation, provider.CapEmbedding:
	default:
		return "", "", false
	}
	switch {
	case inst == provider.FakePrimary:
	case inst == provider.FakeFallback && c == provider.CapTTS:
	default:
		return "", "", false
	}
	return c, inst, true
}

// ParseFaultSet parses the Redis fault document. Bad JSON, an expired "until", unknown keys
// or invalid entries never fail: they are skipped (with warn) so a typo cannot take the
// service down.
func ParseFaultSet(b []byte, now time.Time, warn func(string)) *FaultSet {
	if warn == nil {
		warn = func(string) {}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		warn("fake faults: invalid JSON document ignored")
		return emptySet
	}
	if raw, ok := doc["until"]; ok {
		var until string
		t, err := time.Time{}, json.Unmarshal(raw, &until)
		if err == nil {
			t, err = time.Parse(time.RFC3339, until)
		}
		if err != nil {
			warn("fake faults: invalid until, document ignored")
			return emptySet
		}
		if !now.Before(t) {
			return emptySet
		}
		delete(doc, "until")
	}
	set := &FaultSet{m: map[string]*Fault{}}
	for key, raw := range doc {
		c, inst, ok := parseKey(key)
		if !ok {
			warn(fmt.Sprintf("fake faults: unknown key %q ignored", key))
			continue
		}
		var fj faultJSON
		if err := json.Unmarshal(raw, &fj); err != nil {
			warn(fmt.Sprintf("fake faults: invalid entry %q ignored", key))
			continue
		}
		if !modeAllowed(fj.Mode, c) {
			warn(fmt.Sprintf("fake faults: mode %q is not valid for %s, entry %q ignored", fj.Mode, c, key))
			continue
		}
		rate := 1.0
		if fj.Rate != nil {
			rate = *fj.Rate
		}
		if rate < 0 || rate > 1 {
			warn(fmt.Sprintf("fake faults: rate out of range, entry %q ignored", key))
			continue
		}
		f := &Fault{Mode: fj.Mode, Rate: rate, AfterMs: fj.AfterMs, Status: fj.Status, DelayMs: fj.DelayMs}
		switch {
		case fj.Kind != "":
			if !validKinds[fj.Kind] {
				warn(fmt.Sprintf("fake faults: unknown kind, entry %q ignored", key))
				continue
			}
			f.Kind = fj.Kind
		case fj.Status != 0:
			f.Kind = provider.ClassifyHTTPStatus(fj.Status)
		default:
			f.Kind = provider.ClassifyHTTPStatus(503)
		}
		set.m[faultKey(c, inst)] = f
	}
	return set
}

// failPlan tells a fake how to misbehave for one request.
type failPlan struct {
	Mode         string // "" for env config, else the runtime fault mode
	Kind         provider.ErrorKind
	Phase        string
	After        int   // events/chunks (STT: seconds) before a mid failure
	AfterMs      int64 // STT audio position for runtime timeout/disconnect faults
	ExtraDelayMs int   // "slow" fault: added latency (not a failure)
}

// decider makes the deterministic fail/succeed decision per request (spec 8.9.1).
type decider struct {
	seed    int64
	inst    provider.Name
	cap     provider.Capability
	cfg     CapConfig
	counter atomic.Uint64
}

func newDecider(seed int64, inst provider.Name, c provider.Capability, cfg CapConfig) *decider {
	return &decider{seed: seed, inst: inst, cap: c, cfg: cfg}
}

func (d *decider) hashFraction(counter uint64) float64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s|%s|%d", d.seed, d.inst, d.cap, counter)
	return float64(mix64(h.Sum64())>>11) / (1 << 53) // exact value in [0, 1)
}

// mix64 is the splitmix64 finalizer. FNV-1a alone barely moves the high bits when only the
// last digits of the input change (consecutive counters), which would skew the rate.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// next decides the fate of the next request. A runtime fault, when present, replaces the
// env FAILURE_RATE/FAIL_EVERY_N for this pair.
func (d *decider) next(f *Fault) (fail bool, plan failPlan) {
	n := d.counter.Add(1)
	if f != nil {
		if f.Mode == ModeSlow {
			return false, failPlan{Mode: f.Mode, ExtraDelayMs: f.DelayMs}
		}
		if d.hashFraction(n) >= f.Rate {
			return false, failPlan{}
		}
		p := failPlan{Mode: f.Mode, Kind: f.Kind, AfterMs: f.AfterMs, Phase: PhaseStart}
		switch f.Mode {
		case ModeTimeout:
			p.Phase = PhaseHang
		case ModeFailAfterFirstAudio:
			p.Phase, p.After, p.Kind = PhaseMid, 1, provider.KindUnavailable
		case ModeDisconnectMidStream:
			p.Phase, p.Kind = PhaseMid, provider.KindUnavailable
		}
		return true, p
	}
	switch {
	case d.cfg.FailEveryN > 0:
		fail = n%uint64(d.cfg.FailEveryN) == 0
	case d.cfg.FailureRate > 0:
		fail = d.hashFraction(n) < d.cfg.FailureRate
	}
	if !fail {
		return false, failPlan{}
	}
	return true, failPlan{Kind: d.cfg.FailureKind, Phase: d.cfg.FailurePhase, After: d.cfg.FailAfter}
}

// Poller refreshes the fault snapshot from a fetch function (the Redis GET lives in
// internal/redisstore). Provider calls only read the snapshot.
type Poller struct {
	fetch    func(ctx context.Context) ([]byte, bool, error)
	interval time.Duration
	clk      clock.Clock
	log      *slog.Logger

	cur atomic.Pointer[FaultSet]

	mu       sync.Mutex
	lastOK   time.Time
	warnedAt map[string]time.Time
}

// NewPoller builds a Poller; the snapshot starts empty.
func NewPoller(fetch func(ctx context.Context) ([]byte, bool, error), interval time.Duration, clk clock.Clock, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if interval <= 0 {
		interval = time.Second
	}
	p := &Poller{fetch: fetch, interval: interval, clk: clk, log: log, warnedAt: map[string]time.Time{}}
	p.cur.Store(emptySet)
	return p
}

// Current implements FaultSource.
func (p *Poller) Current() *FaultSet { return p.cur.Load() }

func (p *Poller) warn(msg string) {
	p.mu.Lock()
	now := p.clk.Now()
	last, seen := p.warnedAt[msg]
	if seen && now.Sub(last) < time.Minute {
		p.mu.Unlock()
		return
	}
	p.warnedAt[msg] = now
	p.mu.Unlock()
	p.log.Warn(msg)
}

// PollOnce fetches and swaps the snapshot. On a fetch error the last snapshot is kept for
// at most 10 s since the last success, then cleared.
func (p *Poller) PollOnce(ctx context.Context) {
	b, found, err := p.fetch(ctx)
	now := p.clk.Now()
	if err != nil {
		p.mu.Lock()
		stale := p.lastOK.IsZero() || now.Sub(p.lastOK) > staleWindow
		p.mu.Unlock()
		if stale {
			p.cur.Store(emptySet)
		}
		p.warn("fake faults: fetch failed")
		return
	}
	p.mu.Lock()
	p.lastOK = now
	p.mu.Unlock()
	if !found {
		p.cur.Store(emptySet)
		return
	}
	p.cur.Store(ParseFaultSet(b, now, p.warn))
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	p.PollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollOnce(ctx)
		}
	}
}
