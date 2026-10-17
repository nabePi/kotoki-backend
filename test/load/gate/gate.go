// Package gate holds the load-test gates of spec 13.12.3 and the evaluation of criteria K1-K15
// (spec 13.12.4). A gate never passes on missing data: a criterion whose observation is absent (or
// NaN) fails with Reason "no data" unless the gate explicitly skips it, with a stated reason.
package gate

import (
	"math"
	"time"
)

// Spec is one load gate.
type Spec struct {
	Name string
	VUs  int
	Ramp time.Duration
	Hold time.Duration
	// Skip maps a criterion id (K1..K15) to the reason it does not apply to this gate.
	Skip map[string]string
	// Mode is the provider mode of the run: "fake" or "real".
	Mode string
	// PodBalance adds the G4a check to K10: every realtime pod holds about 100 sessions.
	PodBalance bool
}

// Specs are the gates by name. Values come from spec 13.12.3; g1-local is the compose variant of G1
// (Task 19), which has no kube, PG or Redis exporters.
var Specs = map[string]Spec{
	"g1":   {Name: "g1", VUs: 100, Ramp: 5 * time.Minute, Hold: 30 * time.Minute, Mode: "fake"},
	"g2":   {Name: "g2", VUs: 250, Ramp: 5 * time.Minute, Hold: 30 * time.Minute, Mode: "fake"},
	"g3":   {Name: "g3", VUs: 500, Ramp: 10 * time.Minute, Hold: 30 * time.Minute, Mode: "fake"},
	"g4a":  {Name: "g4a", VUs: 1000, Ramp: 15 * time.Minute, Hold: 60 * time.Minute, Mode: "fake", PodBalance: true},
	"g4b":  {Name: "g4b", VUs: 1000, Ramp: 15 * time.Minute, Hold: 60 * time.Minute, Mode: "fake"},
	"soak": {Name: "soak", VUs: 500, Ramp: 10 * time.Minute, Hold: 4 * time.Hour, Mode: "fake"},
	"real": {Name: "real", VUs: 20, Ramp: 2 * time.Minute, Hold: 15 * time.Minute, Mode: "real",
		Skip: map[string]string{"K2": "server_overhead is a fake-provider metric (spec 13.12.4)"}},
	"g1-local": {Name: "g1-local", VUs: 100, Ramp: time.Minute, Hold: 5 * time.Minute, Mode: "fake",
		Skip: map[string]string{
			"K10": "tanpa kube/PG/Redis exporter lokal",
			"K11": "tanpa kube/PG/Redis exporter lokal",
			"K12": "tanpa kube/PG/Redis exporter lokal",
			"K15": "tanpa kube/PG/Redis exporter lokal",
		}},
}

// Observation maps an observation key (see criteria.go) to its measured value. An absent key means
// "not measured", which is different from 0.
type Observation map[string]float64

// Result is the verdict for one criterion.
type Result struct {
	ID, Name  string
	Pass      bool
	Skipped   bool
	Value     float64 // the first failing value, else the first measured one
	Threshold string
	Reason    string
}

// Evaluate checks K1..K15 in order against o.
func Evaluate(s Spec, o Observation) []Result {
	out := make([]Result, 0, len(criteria))
	for _, c := range criteria {
		r := Result{ID: c.ID, Name: c.Name}
		checks := c.Checks
		if s.PodBalance {
			checks = append(append([]check(nil), checks...), c.PodChecks...)
		}
		r.Threshold = describe(checks)
		if why, skip := s.Skip[c.ID]; skip {
			r.Skipped, r.Reason = true, why
			out = append(out, r)
			continue
		}
		r.Pass, r.Value, r.Reason = run(checks, o)
		out = append(out, r)
	}
	return out
}

// AllPass is true when there is at least one result and every one passed or was skipped.
func AllPass(rs []Result) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if !r.Pass && !r.Skipped {
			return false
		}
	}
	return true
}

func run(checks []check, o Observation) (pass bool, value float64, reason string) {
	pass, value = true, math.NaN()
	firstSet := false
	for _, ck := range checks {
		v, ok := o[ck.Key]
		if !ok || math.IsNaN(v) {
			// missing data fails the criterion, whatever the other keys say
			return false, math.NaN(), "no data"
		}
		if !firstSet {
			value, firstSet = v, true
		}
		if !ck.ok(v) {
			if pass {
				value = v // report the first failing value
			}
			pass = false
		}
	}
	return pass, value, ""
}
