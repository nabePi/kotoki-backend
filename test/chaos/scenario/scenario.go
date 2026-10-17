// Package scenario holds the failure-injection scenarios F1-F16 of spec 13.13.2 as data (Table) and
// the runner that plays one: load, inject at T+5 m, revert at T+15 m, kotoki-verify, assertions.
// The runner knows nothing about Redis, docker or kubectl: those are Drivers (test/chaos/driver).
package scenario

import (
	"context"
	"errors"
	"time"

	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
)

// Env is where a scenario runs.
type Env string

// The two environments: the local docker compose stack and a Kubernetes cluster (staging).
const (
	Compose Env = "compose"
	Kube    Env = "kube"
)

// Action kinds.
const (
	// KindFault sets (Fault non-empty) or clears (Fault empty) the fake provider fault document.
	KindFault = "fault"
	// KindCmd runs Argv in the environment (docker compose, kubectl, curl).
	KindCmd = "cmd"
	// KindReset empties the session-start budgets of the rate limiter (Argv[0] is "ratelimits":
	// session_create_user, ticket_user, ws_connect_ip and ws_attach_user). Sessions that fail within
	// seconds (that is what a fault does) use them up; a user without budget either makes no request at
	// all or is refused at the WebSocket and left with a RESERVED session that blocks every new one
	// (409 for minutes), so a circuit breaker never gets the call that would half-open it and the
	// recovery cannot be seen. kotoki-chaos runs it at the start and every few seconds; scenarios do not
	// list it. It is an operation on the test environment, not a fault.
	KindReset = "reset"
	// KindProbe runs a Go-native probe of the harness (Argv[0] names it): a client that does
	// something no command line does, and fails when what it observes is wrong.
	KindProbe = "probe"
)

// ResetRateLimits is the Argv[0] of the KindReset action that empties the session-start budgets.
const ResetRateLimits = "ratelimits"

// Action is one step of an inject or revert.
type Action struct {
	Kind  string
	Fault map[string]faultdoc.Fault
	Argv  []string
}

// Assertion kinds.
const (
	// AssertProm compares the value of the PromQL expression Expr with Want using Cmp.
	AssertProm = "prom"
	// AssertVerify compares the number of kotoki-verify violations with Want (default: 0).
	AssertVerify = "verify"
	// AssertManual cannot be checked by the runner; a scenario with one never passes by itself.
	AssertManual = "manual"
)

// Assertion is one expectation of a scenario. Cmp is one of > >= < <= == !=.
type Assertion struct {
	Kind string
	Name string
	Expr string
	Cmp  string
	Want float64
	// Only restricts the assertion to one environment (empty: both). The alert checks need the
	// Prometheus of a cluster and have no counterpart on the local stack.
	Only Env
}

// Scenario is one row of the table of spec 13.13.2.
type Scenario struct {
	ID, Title string
	Inject    map[Env][]Action
	Revert    map[Env][]Action
	Expect    []Assertion
	// Fixed, when set, replaces the spec timeline (inject T+5 m, revert T+15 m) and ignores Scale: the
	// scenarios whose point depends on real durations (F13 needs a published event, F15 a 300 s
	// session) cannot be compressed.
	Fixed *Window
	// LocalOK: the scenario can be played against docker compose. The others are MANUAL GATE
	// (Task 24): they need a cluster.
	LocalOK bool
}

// Window is a fixed timeline: inject and revert after the load started, then Tail more.
type Window struct{ InjectAt, RevertAt, Tail time.Duration }

// Loader starts the load (kotoki-load) and returns the function that stops it.
type Loader interface {
	Start(ctx context.Context, vus int) (stop func() error, err error)
}

// Driver performs one Action in the environment.
type Driver interface {
	Do(ctx context.Context, a Action) error
}

// Verifier runs kotoki-verify and returns the number of violations.
type Verifier interface {
	Run(ctx context.Context, since time.Time) (int, error)
}

// Querier evaluates a PromQL expression to one number.
type Querier interface {
	Query(ctx context.Context, expr string) (float64, error)
}

// Clock is time; fake in tests.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// Deps are the collaborators of Run.
type Deps struct {
	// Prepare, when set, runs first: it makes sure that nothing of an earlier run is left (sessions
	// that still hold the users of the load). An error stops the run before anything is started.
	Prepare func(ctx context.Context) error
	Load    Loader
	Driver  Driver
	Prom    Querier
	Verify  Verifier
	Clock   Clock
}

// Options shape one run. Durations are the real-time spec values (13.13.1: inject at T+5 m, revert
// at T+15 m); Scale multiplies those two waits (1 = real time, 0.1 = a tenth). Total-RevertAt is the
// time the load keeps running after the revert, to see the recovery; Scale does not apply to it.
type Options struct {
	Env                       Env
	VUs                       int
	Scale                     float64
	InjectAt, RevertAt, Total time.Duration
}

// AssertionResult is the verdict of one assertion.
type AssertionResult struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Pass   bool    `json:"pass"`
	Value  float64 `json:"value"`
	Detail string  `json:"detail,omitempty"`
}

// Report is the outcome of a run.
type Report struct {
	ID      string            `json:"id"`
	Pass    bool              `json:"pass"`
	Reason  string            `json:"reason,omitempty"` // why the run did not pass
	Results []AssertionResult `json:"results"`
}

// ByID returns the scenario with the given ID.
func ByID(id string) (Scenario, bool) {
	for _, s := range Table {
		if s.ID == id {
			return s, true
		}
	}
	return Scenario{}, false
}

// ErrNeedsCluster is returned when a scenario that is not LocalOK is run against compose.
var ErrNeedsCluster = errors.New("scenario needs a Kubernetes cluster (MANUAL GATE, Task 24)")
