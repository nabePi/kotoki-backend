// Package promtext is a small parser for the Prometheus text exposition format and the few
// aggregations the load harness needs (sums, and histogram quantiles over a delta between two
// snapshots). It is hand written on purpose (no expfmt dependency) and uses no wall clock: the
// window of a delta is whatever two snapshots the caller took.
package promtext

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Sample is one exposed series value. Labels is never nil.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Samples is a scrape.
type Samples []Sample

// Parse reads the text exposition format. "# HELP" / "# TYPE" and other comments are ignored; a
// trailing timestamp is accepted and ignored. A malformed line is an error that names the line: a
// half-parsed scrape would silently turn into "metric absent".
func Parse(r io.Reader) (Samples, error) {
	br := bufio.NewReader(r)
	var out Samples
	for n := 1; ; n++ {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			s, perr := parseLine(t)
			if perr != nil {
				return nil, fmt.Errorf("promtext: line %d: %w", n, perr)
			}
			out = append(out, s)
		}
		if err == io.EOF {
			return out, nil
		}
	}
}

func isNameStart(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func parseLine(line string) (Sample, error) {
	i := 0
	for i < len(line) && isNameChar(line[i]) {
		i++
	}
	if i == 0 || !isNameStart(line[0]) {
		return Sample{}, fmt.Errorf("bad metric name in %q", trunc(line))
	}
	s := Sample{Name: line[:i], Labels: map[string]string{}}
	rest := line[i:]
	if strings.HasPrefix(rest, "{") {
		var err error
		rest, err = parseLabels(rest[1:], s.Labels)
		if err != nil {
			return Sample{}, err
		}
	}
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return Sample{}, fmt.Errorf("unexpected %q after metric name or labels", trunc(rest))
	}
	f := strings.Fields(rest)
	switch len(f) {
	case 1, 2:
	default:
		return Sample{}, fmt.Errorf("want a value and an optional timestamp, got %d fields", len(f))
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return Sample{}, fmt.Errorf("bad value %q", f[0])
	}
	if len(f) == 2 {
		if _, err := strconv.ParseInt(f[1], 10, 64); err != nil {
			return Sample{}, fmt.Errorf("bad timestamp %q", f[1])
		}
	}
	s.Value = v
	return s, nil
}

// parseLabels parses the part after '{' up to and including '}', returning what follows it.
func parseLabels(in string, into map[string]string) (string, error) {
	i := 0
	skip := func() {
		for i < len(in) && (in[i] == ' ' || in[i] == '\t') {
			i++
		}
	}
	for {
		skip()
		if i >= len(in) {
			return "", fmt.Errorf("unterminated label set")
		}
		if in[i] == '}' {
			return in[i+1:], nil
		}
		start := i
		for i < len(in) && isNameChar(in[i]) {
			i++
		}
		if i == start || !isNameStart(in[start]) {
			return "", fmt.Errorf("bad label name near %q", trunc(in[start:]))
		}
		name := in[start:i]
		skip()
		if i >= len(in) || in[i] != '=' {
			return "", fmt.Errorf("label %q has no '='", name)
		}
		i++
		skip()
		if i >= len(in) || in[i] != '"' {
			return "", fmt.Errorf("label %q value is not quoted", name)
		}
		i++
		var val strings.Builder
		closed := false
		for i < len(in) {
			c := in[i]
			if c == '"' {
				closed = true
				i++
				break
			}
			if c == '\\' {
				if i+1 >= len(in) {
					break
				}
				switch in[i+1] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					return "", fmt.Errorf("bad escape \\%c in label %q", in[i+1], name)
				}
				i += 2
				continue
			}
			val.WriteByte(c)
			i++
		}
		if !closed {
			return "", fmt.Errorf("unterminated value of label %q", name)
		}
		into[name] = val.String()
		skip()
		if i < len(in) && in[i] == ',' {
			i++
			continue
		}
		if i < len(in) && in[i] == '}' {
			continue
		}
		return "", fmt.Errorf("expected ',' or '}' after label %q", name)
	}
}

func trunc(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

func matches(s Sample, name string, match map[string]string) bool {
	if s.Name != name {
		return false
	}
	for k, v := range match {
		got, ok := s.Labels[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// Sum adds the values of every series called name whose labels equal all of match (nil matches all).
func (ss Samples) Sum(name string, match map[string]string) float64 {
	var sum float64
	for _, s := range ss {
		if matches(s, name, match) {
			sum += s.Value
		}
	}
	return sum
}

// SumBy is Sum grouped by the value of label; series without that label are grouped under "".
func (ss Samples) SumBy(name string, match map[string]string, label string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range ss {
		if matches(s, name, match) {
			out[s.Labels[label]] += s.Value
		}
	}
	return out
}

// Filter returns the samples for which keep is true, leaving the receiver untouched.
func (ss Samples) Filter(keep func(Sample) bool) Samples {
	var out Samples
	for _, s := range ss {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// HasPrefix reports whether any series name starts with prefix.
func (ss Samples) HasPrefix(prefix string) bool {
	for _, s := range ss {
		if strings.HasPrefix(s.Name, prefix) {
			return true
		}
	}
	return false
}

// Quantile is histogram_quantile(q, sum by (le) (increase(name_bucket{match}))) for the window
// between two snapshots: bucket counts are summed across instances, before is subtracted from after,
// and the quantile is interpolated linearly inside the bucket like Prometheus does (a rank that
// falls in the +Inf bucket answers with the highest finite bound). before may be nil.
//
// ok is false when there is nothing to answer from: q outside [0,1], no +Inf bucket, no
// observations in the window, a malformed le, or a bucket that went down between the snapshots (a
// counter reset, e.g. a pod restart, whose delta cannot be trusted).
func Quantile(q float64, before, after Samples, name string, match map[string]string) (float64, bool) {
	if math.IsNaN(q) || q < 0 || q > 1 {
		return 0, false
	}
	bucket := name + "_bucket"
	delta := map[float64]float64{}
	add := func(ss Samples, sign float64) bool {
		for _, s := range ss {
			if !matches(s, bucket, match) {
				continue
			}
			raw, ok := s.Labels["le"]
			if !ok {
				return false
			}
			le, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(le) {
				return false
			}
			delta[le] += sign * s.Value
		}
		return true
	}
	if !add(after, 1) || !add(before, -1) {
		return 0, false
	}
	les := make([]float64, 0, len(delta))
	for le, d := range delta {
		if d < 0 {
			return 0, false
		}
		les = append(les, le)
	}
	sort.Float64s(les)
	if len(les) == 0 || !math.IsInf(les[len(les)-1], 1) {
		return 0, false
	}
	total := delta[les[len(les)-1]]
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	prevBound, prevCount := 0.0, 0.0
	for i, le := range les {
		count := delta[le]
		if count < rank {
			prevBound, prevCount = le, count
			continue
		}
		if math.IsInf(le, 1) {
			if i == 0 {
				return 0, false
			}
			return les[i-1], true
		}
		if count == prevCount {
			return le, true
		}
		return prevBound + (le-prevBound)*(rank-prevCount)/(count-prevCount), true
	}
	return 0, false
}
