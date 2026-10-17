// Package live holds the parts of the chaos harness that touch a running local stack: the sampler
// that stands in for Prometheus (the compose stack has none), the kotoki-load process and
// kotoki-verify.
package live

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/test/load/promtext"
	"github.com/nabePi/kotoki-backend/test/load/server"
)

// ErrNoData is returned when an expression matches no series. A chaos assertion must fail on it,
// never pass: the only way to read "nothing happened" as 0 is the explicit `or vector(0)`.
var ErrNoData = errors.New("no data")

// ErrUnsupported is returned for an expression outside the small PromQL subset the sampler
// evaluates. The table uses only that subset for LocalOK scenarios; the others are evaluated by a
// real Prometheus.
var ErrUnsupported = errors.New("unsupported expression")

// Sampler scrapes /metrics endpoints periodically and keeps the history, so that the compose
// stack can answer the few PromQL shapes of the chaos table without a Prometheus:
//
//	sum|max|min( increase|max_over_time|min_over_time( name{a="b",c!="d"}[30m] ) ) [or vector(0)]
//	sum|max|min( name{...} ) [or vector(0)]
//
// Series stay apart per source URL; the function is computed per series and then aggregated, like
// PromQL. increase is the sum of the non-negative steps (a drop is a counter reset), and a series
// that first shows up after the window start counts from 0. The window is measured back from the
// newest snapshot and is clipped to the history there is.
type Sampler struct {
	urls  []string
	every time.Duration

	mu   sync.Mutex
	srcs map[string]*source
	errs []string
	wg   sync.WaitGroup

	// Now is the clock of Start (default time.Now); a seam for tests.
	Now func() time.Time
}

type source struct {
	times  []time.Time
	series map[string]*serie
}

type serie struct {
	name   string
	labels map[string]string
	pts    []point
}

type point struct {
	idx int // index into source.times
	v   float64
}

// NewSampler returns a sampler of urls (full /metrics URLs) with the given period.
func NewSampler(urls []string, every time.Duration) *Sampler {
	return &Sampler{urls: urls, every: every, srcs: map[string]*source{}, Now: time.Now}
}

// Observe records one scrape of source at time t.
func (s *Sampler) Observe(t time.Time, src string, ss promtext.Samples) {
	s.mu.Lock()
	defer s.mu.Unlock()
	so := s.srcs[src]
	if so == nil {
		so = &source{series: map[string]*serie{}}
		s.srcs[src] = so
	}
	so.times = append(so.times, t)
	idx := len(so.times) - 1
	for _, sm := range ss {
		key := sm.Name + "{" + labelKey(sm.Labels) + "}"
		se := so.series[key]
		if se == nil {
			se = &serie{name: sm.Name, labels: sm.Labels}
			so.series[key] = se
		}
		se.pts = append(se.pts, point{idx: idx, v: sm.Value})
	}
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + strconv.Quote(l[k]) + ",")
	}
	return b.String()
}

// Prime scrapes every URL once and records it; it fails when any of them cannot be read. The run
// must not start without a baseline.
func (s *Sampler) Prime(ctx context.Context) error {
	var errs []error
	for _, u := range s.urls {
		if err := s.scrape(ctx, u); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Sampler) scrape(ctx context.Context, u string) error {
	ss, err := server.Scraper{URLs: []string{u}}.Snapshot(ctx)
	if err != nil {
		return err
	}
	s.Observe(s.Now(), u, ss)
	return nil
}

// Start samples every period until ctx ends; Wait blocks until the goroutine is gone. A URL that
// fails once is recorded in Errors and tried again next time.
func (s *Sampler) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			for _, u := range s.urls {
				if err := s.scrape(ctx, u); err != nil && ctx.Err() == nil {
					s.addErr(err)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.every):
			}
		}
	}()
}

// Wait blocks until the sampling goroutine has stopped.
func (s *Sampler) Wait() { s.wg.Wait() }

func (s *Sampler) addErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) < 50 {
		s.errs = append(s.errs, err.Error())
	}
}

// Errors returns the scrape failures seen so far (at most 50).
func (s *Sampler) Errors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.errs...)
}

const sel = `([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?`

var (
	exprRE = regexp.MustCompile(`^(sum|max|min)\(\s*(?:(increase|max_over_time|min_over_time)\(\s*` + sel +
		`\[(\d+)(s|m|h)\]\s*\)|` + sel + `)\s*\)(\s+or\s+vector\(0\))?$`)
	matcherRE = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)\s*(!=|=)\s*"([^"]*)"$`)
)

type matcher struct {
	name, val string
	neq       bool
}

func parseMatchers(raw string) ([]matcher, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "{"), "}"))
	if raw == "" {
		return nil, nil
	}
	var out []matcher
	for _, part := range strings.Split(raw, ",") {
		m := matcherRE.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			return nil, fmt.Errorf("%w: matcher %q (only = and != with a quoted value)", ErrUnsupported, part)
		}
		out = append(out, matcher{name: m[1], val: m[3], neq: m[2] == "!="})
	}
	return out, nil
}

func (m matcher) ok(labels map[string]string) bool {
	return (labels[m.name] == m.val) != m.neq
}

// Query evaluates expr against the history.
func (s *Sampler) Query(_ context.Context, expr string) (float64, error) {
	m := exprRE.FindStringSubmatch(strings.TrimSpace(expr))
	if m == nil {
		return 0, fmt.Errorf("%w: %q", ErrUnsupported, expr)
	}
	agg, fn := m[1], m[2]
	name, rawMatch := m[3], m[4]
	var window time.Duration
	if fn != "" {
		n, _ := strconv.Atoi(m[5])
		window = time.Duration(n) * map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[m[6]]
	} else {
		name, rawMatch = m[7], m[8]
	}
	orZero := m[9] != ""
	matchers, err := parseMatchers(rawMatch)
	if err != nil {
		return 0, err
	}

	vals := s.evaluate(fn, name, matchers, window)
	if len(vals) == 0 {
		if orZero {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: %s", ErrNoData, expr)
	}
	out := vals[0]
	for _, v := range vals[1:] {
		switch agg {
		case "sum":
			out += v
		case "max":
			out = max(out, v)
		case "min":
			out = min(out, v)
		}
	}
	return out, nil
}

func (s *Sampler) evaluate(fn, name string, ms []matcher, window time.Duration) []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var now time.Time
	for _, so := range s.srcs {
		if n := len(so.times); n > 0 && so.times[n-1].After(now) {
			now = so.times[n-1]
		}
	}
	var out []float64
	for _, so := range s.srcs {
		if len(so.times) == 0 {
			continue
		}
		start := 0
		if fn != "" {
			start = sort.Search(len(so.times), func(i int) bool { return !so.times[i].Before(now.Add(-window)) })
			if start >= len(so.times) {
				start = len(so.times) - 1
			}
		}
		last := len(so.times) - 1
	series:
		for _, se := range so.series {
			if se.name != name {
				continue
			}
			for _, m := range ms {
				if !m.ok(se.labels) {
					continue series
				}
			}
			switch fn {
			case "":
				if n := len(se.pts); n > 0 && se.pts[n-1].idx == last {
					out = append(out, se.pts[n-1].v)
				}
			case "increase":
				if v, ok := increase(se.pts, start); ok {
					out = append(out, v)
				}
			case "max_over_time", "min_over_time":
				first := true
				var best float64
				for _, p := range se.pts {
					if p.idx < start {
						continue
					}
					switch {
					case first, fn == "max_over_time" && p.v > best, fn == "min_over_time" && p.v < best:
						best, first = p.v, false
					}
				}
				if !first {
					out = append(out, best)
				}
			}
		}
	}
	return out
}

// increase sums the non-negative steps of the points inside the window; a series that is not in
// the first snapshot of the window starts from 0.
func increase(pts []point, start int) (float64, bool) {
	var total, prev float64
	seen := false
	for _, p := range pts {
		if p.idx < start {
			continue
		}
		if !seen {
			seen = true
			if p.idx == start {
				prev = p.v
				continue
			}
			prev = 0
		}
		if p.v >= prev {
			total += p.v - prev
		} else {
			total += p.v // counter reset
		}
		prev = p.v
	}
	return total, seen
}
