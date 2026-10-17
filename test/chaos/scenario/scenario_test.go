package scenario_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/faultdoc"
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

// ---- table ------------------------------------------------------------------------------------

func TestTable_HasF1ToF16(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range scenario.Table {
		require.False(t, seen[s.ID], "duplicate %s", s.ID)
		seen[s.ID] = true
		require.NotEmpty(t, s.Title, s.ID)
		require.NotEmpty(t, s.Expect, "%s needs at least one assertion", s.ID)
	}
	for i := 1; i <= 16; i++ {
		id := fmt.Sprintf("F%d", i)
		if i == 14 { // removed from v1 (spec 13.13.2); the numbers are not shifted
			require.False(t, seen[id], "F14 is not part of v1")
			continue
		}
		require.True(t, seen[id], "missing %s", id)
	}
	require.Len(t, scenario.Table, 15)
}

func TestTable_LocalOK(t *testing.T) {
	local := map[string]bool{"F1": true, "F2": true, "F3": true, "F4": true, "F5": true, "F6": true,
		"F8": true, "F13": true, "F15": true, "F16": true}
	for _, s := range scenario.Table {
		require.Equal(t, local[s.ID], s.LocalOK, s.ID)
	}
}

func fault(t *testing.T, a scenario.Action) map[string]faultdoc.Fault {
	t.Helper()
	require.Equal(t, scenario.KindFault, a.Kind)
	// the document must pass the same validation the runtime applies
	_, err := faultdoc.Build(time.Now().Add(time.Hour), a.Fault)
	require.NoError(t, err)
	return a.Fault
}

func TestTable_FaultScenarios(t *testing.T) {
	get := func(id string) scenario.Scenario {
		s, ok := scenario.ByID(id)
		require.True(t, ok, id)
		return s
	}
	for _, env := range []scenario.Env{scenario.Compose, scenario.Kube} {
		f1 := fault(t, get("F1").Inject[env][0])
		require.Equal(t, faultdoc.Fault{Mode: "timeout", Rate: 1.0, AfterMS: 2000}, f1["STT.FAKE_PRIMARY"])

		f2 := fault(t, get("F2").Inject[env][0])
		require.Equal(t, "timeout", f2["LLM.FAKE_PRIMARY"].Mode)

		f3 := fault(t, get("F3").Inject[env][0])
		require.Equal(t, "fail_after_first_audio", f3["TTS.FAKE_PRIMARY"].Mode)

		f4 := fault(t, get("F4").Inject[env][0])
		for _, k := range []string{"STT.FAKE_PRIMARY", "LLM.FAKE_PRIMARY"} {
			require.Equal(t, "error", f4[k].Mode, k)
			require.InDelta(t, 1.0, f4[k].Rate, 0, k)
		}

		f5 := fault(t, get("F5").Inject[env][0])
		require.Contains(t, f5, "TTS.FAKE_PRIMARY")
		require.Contains(t, f5, "TTS.FAKE_FALLBACK")

		for _, id := range []string{"F1", "F2", "F3", "F4", "F5"} {
			rv := get(id).Revert[env]
			require.Len(t, rv, 1, id)
			require.Equal(t, scenario.KindFault, rv[0].Kind, id)
			require.Empty(t, rv[0].Fault, "%s: the revert clears the document", id)
		}
	}
	// F1 pins D-017: no vendor switch for STT
	var noSwitch bool
	for _, a := range get("F1").Expect {
		if strings.Contains(a.Expr, `kotoki_provider_fallback_total{capability="STT"}`) {
			noSwitch = true
		}
	}
	require.True(t, noSwitch, "F1 must assert that no fallback happened")
}

// TestTable_F6ProvesTheRestartWasSeen: "no false heartbeat_lost" is also true when nothing happened, so
// F6 must also assert that the pods saw Redis fail.
func TestTable_F6ProvesTheRestartWasSeen(t *testing.T) {
	f6, _ := scenario.ByID("F6")
	var seen bool
	for _, a := range f6.Expect {
		if strings.Contains(a.Expr, "kotoki_redis_command_errors_total") && a.Cmp == ">" && a.Want == 0 {
			seen = true
		}
	}
	require.True(t, seen)
}

func TestTable_F4WatchesTheTransitionsNotTheGauge(t *testing.T) {
	f4, _ := scenario.ByID("F4")
	var to []string
	for _, a := range f4.Expect {
		for _, m := range regexp.MustCompile(`circuit_transitions_total\{to="([A-Z_]+)"\}`).FindAllStringSubmatch(a.Expr, -1) {
			to = append(to, m[1])
		}
	}
	require.ElementsMatch(t, []string{"OPEN", "HALF_OPEN", "CLOSED"}, to)
}

func TestTable_CommandScenarios(t *testing.T) {
	get := func(id string) scenario.Scenario { s, _ := scenario.ByID(id); return s }
	join := func(as []scenario.Action) string {
		var b strings.Builder
		for _, a := range as {
			b.WriteString(strings.Join(a.Argv, " ") + "\n")
		}
		return b.String()
	}
	require.Contains(t, join(get("F6").Inject[scenario.Compose]), "docker compose restart redis")
	require.Contains(t, join(get("F8").Inject[scenario.Compose]), "docker compose stop rabbitmq")
	require.Contains(t, join(get("F8").Revert[scenario.Compose]), "docker compose start rabbitmq")
	require.Contains(t, join(get("F9").Inject[scenario.Kube]), "--grace-period=0 --force")
	require.Contains(t, join(get("F10").Inject[scenario.Kube]), "rollout restart deployment/kotoki-realtime")
	require.Contains(t, join(get("F13").Inject[scenario.Compose]), "session.completed")
}

// metricsAndAlerts reads the canonical lists of deploy/k8s/hack.
func hackLines(t *testing.T, name string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "k8s", "hack", name))
	require.NoError(t, err)
	out := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimPrefix(strings.TrimSpace(l), "?")
		if l != "" && !strings.HasPrefix(l, "#") {
			out[l] = true
		}
	}
	return out
}

var kotokiName = regexp.MustCompile(`\bkotoki_[a-z0-9_]+`)

// TestTable_ExpressionsUseKnownNames guards the drift of spec 13.1 review focus 1: a chaos
// assertion on a metric or alert that does not exist could only fail, or worse pass on "or vector(0)".
func TestTable_ExpressionsUseKnownNames(t *testing.T) {
	metrics := hackLines(t, "metrics.txt")
	alerts := hackLines(t, "alert-names.txt")
	alertRE := regexp.MustCompile(`alertname="([A-Za-z]+)"`)
	for _, s := range scenario.Table {
		for _, a := range s.Expect {
			if a.Kind != scenario.AssertProm {
				continue
			}
			require.NotEmpty(t, a.Expr, "%s/%s", s.ID, a.Name)
			require.Contains(t, []string{">", ">=", "<", "<=", "==", "!="}, a.Cmp, "%s/%s", s.ID, a.Name)
			for _, n := range kotokiName.FindAllString(a.Expr, -1) {
				base := regexp.MustCompile(`_(bucket|sum|count)$`).ReplaceAllString(n, "")
				require.True(t, metrics[n] || metrics[base], "%s/%s: unknown metric %s", s.ID, a.Name, n)
			}
			for _, m := range alertRE.FindAllStringSubmatch(a.Expr, -1) {
				require.True(t, alerts[m[1]], "%s/%s: unknown alert %s", s.ID, a.Name, m[1])
			}
		}
	}
}

func TestTable_LocalScenariosHaveNoManualAssertion(t *testing.T) {
	for _, s := range scenario.Table {
		if !s.LocalOK {
			continue
		}
		for _, a := range s.Expect {
			if a.Only == scenario.Kube {
				continue
			}
			require.NotEqual(t, scenario.AssertManual, a.Kind, "%s/%s would never pass locally", s.ID, a.Name)
		}
	}
}

// ---- runner -----------------------------------------------------------------------------------

type log struct {
	mu sync.Mutex
	ev []string
}

func (l *log) add(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, fmt.Sprintf(f, a...))
}
func (l *log) get() []string { l.mu.Lock(); defer l.mu.Unlock(); return append([]string(nil), l.ev...) }

type fakeClock struct {
	l *log
	t time.Time
}

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.l.add("sleep:%s", d)
	c.t = c.t.Add(d)
	return nil
}

type fakeLoad struct {
	l       *log
	err     error
	stopErr error
}

func (f fakeLoad) Start(_ context.Context, vus int) (func() error, error) {
	f.l.add("load.start:%d", vus)
	if f.err != nil {
		return nil, f.err
	}
	return func() error { f.l.add("load.stop"); return f.stopErr }, nil
}

type fakeDriver struct {
	l       *log
	failOn  string
	ctxDead bool
}

func (f *fakeDriver) Do(ctx context.Context, a scenario.Action) error {
	name := a.Kind
	if len(a.Argv) > 0 {
		name += ":" + strings.Join(a.Argv, " ")
	}
	if a.Kind == scenario.KindFault && len(a.Fault) == 0 {
		name += ":clear"
	}
	f.l.add("do:%s", name)
	if ctx.Err() != nil {
		f.ctxDead = true
	}
	if f.failOn != "" && strings.Contains(name, f.failOn) {
		return errors.New("driver boom")
	}
	return nil
}

type fakeVerify struct {
	l     *log
	n     int
	err   error
	since *time.Time
}

func (f fakeVerify) Run(_ context.Context, since time.Time) (int, error) {
	f.l.add("verify")
	if f.since != nil {
		*f.since = since
	}
	return f.n, f.err
}

type fakeProm struct {
	l    *log
	vals map[string]float64
}

func (f fakeProm) Query(_ context.Context, expr string) (float64, error) {
	f.l.add("query:%s", expr)
	v, ok := f.vals[expr]
	if !ok {
		return 0, errors.New("no data")
	}
	return v, nil
}

func testScenario() scenario.Scenario {
	return scenario.Scenario{
		ID: "FT", Title: "test", LocalOK: true,
		Inject: map[scenario.Env][]scenario.Action{scenario.Compose: {{Kind: scenario.KindCmd, Argv: []string{"inject"}}}},
		Revert: map[scenario.Env][]scenario.Action{scenario.Compose: {{Kind: scenario.KindCmd, Argv: []string{"revert"}}}},
		Expect: []scenario.Assertion{
			{Kind: scenario.AssertProm, Name: "errors rose", Expr: "up", Cmp: ">", Want: 0},
			{Kind: scenario.AssertVerify, Name: "integrity"},
		},
	}
}

func setup(prom map[string]float64) (*log, scenario.Deps, *fakeDriver, *time.Time) {
	l := &log{}
	drv := &fakeDriver{l: l}
	since := new(time.Time)
	return l, scenario.Deps{
		Load: fakeLoad{l: l}, Driver: drv, Prom: fakeProm{l: l, vals: prom},
		Verify: fakeVerify{l: l, since: since}, Clock: &fakeClock{l: l, t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)},
	}, drv, since
}

func opts(scale float64) scenario.Options {
	return scenario.Options{Env: scenario.Compose, VUs: 100, Scale: scale,
		InjectAt: 5 * time.Minute, RevertAt: 15 * time.Minute, Total: 15 * time.Minute}
}

func TestRun_Timeline(t *testing.T) {
	l, d, _, since := setup(map[string]float64{"up": 3})
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.NoError(t, err)
	require.True(t, rep.Pass, rep.Reason)
	require.Equal(t, []string{
		"load.start:100", "sleep:5m0s", "do:cmd:inject", "sleep:10m0s", "do:cmd:revert",
		"load.stop", "verify", "query:up",
	}, l.get())
	require.Equal(t, time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), *since, "verify looks at sessions created since the load started")
}

func TestRun_TimelineScaled(t *testing.T) {
	l, d, _, _ := setup(map[string]float64{"up": 3})
	_, err := scenario.Run(t.Context(), testScenario(), d, opts(0.1))
	require.NoError(t, err)
	ev := l.get()
	require.Equal(t, "sleep:30s", ev[1])
	require.Equal(t, "sleep:1m0s", ev[3])
}

func TestRun_TailAfterRevert(t *testing.T) {
	l, d, _, _ := setup(map[string]float64{"up": 3})
	o := opts(1)
	o.Total = 17 * time.Minute
	_, err := scenario.Run(t.Context(), testScenario(), d, o)
	require.NoError(t, err)
	require.Equal(t, []string{"do:cmd:revert", "sleep:2m0s", "load.stop"}, l.get()[4:7], "the load keeps running after the revert to see the recovery")
}

func TestRun_TailIsNotScaled(t *testing.T) {
	l, d, _, _ := setup(map[string]float64{"up": 3})
	o := opts(0.1)
	o.Total = 17 * time.Minute
	_, err := scenario.Run(t.Context(), testScenario(), d, o)
	require.NoError(t, err)
	require.Equal(t, "sleep:2m0s", l.get()[5], "recovery time does not shrink with the experiment's clock")
}

func TestRun_FixedWindowIgnoresScaleAndOptions(t *testing.T) {
	sc := testScenario()
	sc.Fixed = &scenario.Window{InjectAt: 90 * time.Second, RevertAt: 2 * time.Minute, Tail: 20 * time.Second}
	l, d, _, _ := setup(map[string]float64{"up": 3})
	_, err := scenario.Run(t.Context(), sc, d, opts(0.1))
	require.NoError(t, err)
	require.Equal(t, []string{"load.start:100", "sleep:1m30s", "do:cmd:inject", "sleep:30s", "do:cmd:revert", "sleep:20s", "load.stop"}, l.get()[:7])
}

func TestTable_RealTimeScenarios(t *testing.T) {
	for _, id := range []string{"F13", "F15"} {
		sc, _ := scenario.ByID(id)
		require.NotNil(t, sc.Fixed, id)
		require.Greater(t, sc.Fixed.RevertAt, sc.Fixed.InjectAt, id)
	}
	for _, sc := range scenario.Table {
		if sc.ID != "F13" && sc.ID != "F15" {
			require.Nil(t, sc.Fixed, "%s follows the spec timeline", sc.ID)
		}
	}
	f15, _ := scenario.ByID("F15")
	require.Equal(t, scenario.KindProbe, f15.Inject[scenario.Compose][0].Kind)
}

func TestRun_FailsWhenAssertionFails(t *testing.T) {
	_, d, _, _ := setup(map[string]float64{"up": 0}) // want > 0
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.NoError(t, err)
	require.False(t, rep.Pass)
	require.Contains(t, rep.Reason, "errors rose")
	require.False(t, rep.Results[0].Pass)
	require.InDelta(t, 0, rep.Results[0].Value, 0)
}

func TestRun_FailsWhenPromHasNoData(t *testing.T) {
	_, d, _, _ := setup(map[string]float64{}) // missing data must fail, never pass
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.NoError(t, err)
	require.False(t, rep.Pass)
	require.Contains(t, rep.Results[0].Detail, "no data")
}

func TestRun_AllComparators(t *testing.T) {
	cases := []struct {
		cmp  string
		v    float64
		want bool
	}{
		{">", 2, true}, {">", 1, false}, {">=", 1, true}, {">=", 0.9, false},
		{"<", 0.9, true}, {"<", 1, false}, {"<=", 1, true}, {"<=", 1.1, false},
		{"==", 1, true}, {"==", 2, false}, {"!=", 2, true}, {"!=", 1, false},
	}
	for _, c := range cases {
		sc := testScenario()
		sc.Expect = []scenario.Assertion{{Kind: scenario.AssertProm, Name: "x", Expr: "up", Cmp: c.cmp, Want: 1}}
		_, d, _, _ := setup(map[string]float64{"up": c.v})
		rep, err := scenario.Run(t.Context(), sc, d, opts(1))
		require.NoError(t, err)
		require.Equal(t, c.want, rep.Pass, "%v %s 1", c.v, c.cmp)
	}
	sc := testScenario()
	sc.Expect = []scenario.Assertion{{Kind: scenario.AssertProm, Name: "x", Expr: "up", Cmp: "~", Want: 1}}
	_, d, _, _ := setup(map[string]float64{"up": 1})
	rep, err := scenario.Run(t.Context(), sc, d, opts(1))
	require.NoError(t, err)
	require.False(t, rep.Pass, "an unknown comparator never passes")
}

func TestRun_FailsWhenVerifyNonZero(t *testing.T) {
	l := &log{}
	_, d, _, _ := setup(map[string]float64{"up": 3})
	d.Verify = fakeVerify{l: l, n: 2}
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.NoError(t, err)
	require.False(t, rep.Pass)
	require.Contains(t, rep.Reason, "integrity")
	require.Contains(t, rep.Results[1].Detail, "2 violation")
}

func TestRun_VerifyNonZeroFailsWithoutVerifyAssertion(t *testing.T) {
	sc := testScenario()
	sc.Expect = sc.Expect[:1]
	_, d, _, _ := setup(map[string]float64{"up": 3})
	d.Verify = fakeVerify{l: &log{}, n: 1}
	rep, err := scenario.Run(t.Context(), sc, d, opts(1))
	require.NoError(t, err)
	require.False(t, rep.Pass, "kotoki-verify is part of every scenario (spec 13.13.1)")
}

func TestRun_VerifyError(t *testing.T) {
	_, d, _, _ := setup(map[string]float64{"up": 3})
	d.Verify = fakeVerify{l: &log{}, err: errors.New("db down")}
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.Error(t, err)
	require.False(t, rep.Pass)
}

func TestRun_RevertAlwaysRunsOnError(t *testing.T) {
	l, d, drv, _ := setup(map[string]float64{"up": 3})
	drv.failOn = "inject"
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.ErrorContains(t, err, "driver boom")
	require.False(t, rep.Pass)
	ev := l.get()
	require.Contains(t, ev, "do:cmd:revert")
	require.Contains(t, ev, "load.stop", "the load is stopped on every path")
	require.NotContains(t, ev, "verify", "a run whose injection failed proves nothing")
}

func TestRun_RevertRunsWhenContextIsCancelled(t *testing.T) {
	l, d, drv, _ := setup(map[string]float64{"up": 3})
	ctx, cancel := context.WithCancel(t.Context())
	d.Clock = cancelClock{inner: d.Clock, cancel: cancel, after: "sleep:10m0s"}
	_, err := scenario.Run(ctx, testScenario(), d, opts(1))
	require.Error(t, err)
	require.Contains(t, l.get(), "do:cmd:revert")
	require.False(t, drv.ctxDead, "the revert must not run on the cancelled context: it is what removes the fault")
}

// cancelClock cancels the context when the sleep is reached, then reports the cancellation.
type cancelClock struct {
	inner  scenario.Clock
	cancel func()
	after  string
}

func (c cancelClock) Now() time.Time { return c.inner.Now() }
func (c cancelClock) Sleep(ctx context.Context, d time.Duration) error {
	if d == 10*time.Minute {
		c.cancel()
		return ctx.Err()
	}
	return c.inner.Sleep(ctx, d)
}

func TestRun_PrepareRunsFirstAndCanStopTheRun(t *testing.T) {
	l, d, _, _ := setup(map[string]float64{"up": 3})
	d.Prepare = func(context.Context) error { l.add("prepare"); return nil }
	_, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.NoError(t, err)
	require.Equal(t, []string{"prepare", "load.start:100"}, l.get()[:2])

	l2, d2, _, _ := setup(nil)
	d2.Prepare = func(context.Context) error { return errors.New("sessions still open") }
	rep, err := scenario.Run(t.Context(), testScenario(), d2, opts(1))
	require.ErrorContains(t, err, "sessions still open")
	require.False(t, rep.Pass)
	require.Empty(t, l2.get(), "nothing is started when the preparation fails")
}

func TestRun_LoadStartError(t *testing.T) {
	l, d, _, _ := setup(nil)
	d.Load = fakeLoad{l: l, err: errors.New("no users")}
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.ErrorContains(t, err, "no users")
	require.False(t, rep.Pass)
	require.NotContains(t, l.get(), "do:cmd:inject")
}

func TestRun_LoadStopError(t *testing.T) {
	l, d, _, _ := setup(map[string]float64{"up": 3})
	d.Load = fakeLoad{l: l, stopErr: errors.New("load crashed")}
	rep, err := scenario.Run(t.Context(), testScenario(), d, opts(1))
	require.ErrorContains(t, err, "load crashed")
	require.False(t, rep.Pass)
}

func TestRun_ManualAssertionMarksIncomplete(t *testing.T) {
	sc := testScenario()
	sc.Expect = append(sc.Expect, scenario.Assertion{Kind: scenario.AssertManual, Name: "operator confirms the alert fired"})
	_, d, _, _ := setup(map[string]float64{"up": 3})
	rep, err := scenario.Run(t.Context(), sc, d, opts(1))
	require.NoError(t, err)
	require.False(t, rep.Pass)
	require.Equal(t, "manual check required", rep.Reason)
}

func TestRun_OnlyFiltersByEnvironment(t *testing.T) {
	sc := testScenario()
	sc.Expect = append(sc.Expect, scenario.Assertion{Kind: scenario.AssertProm, Name: "alert fired", Expr: "ALERTS", Cmp: ">", Want: 0, Only: scenario.Kube})
	l, d, _, _ := setup(map[string]float64{"up": 3})
	rep, err := scenario.Run(t.Context(), sc, d, opts(1))
	require.NoError(t, err)
	require.True(t, rep.Pass, rep.Reason)
	require.NotContains(t, l.get(), "query:ALERTS")
}

func TestRun_RejectsScenarioThatNeedsACluster(t *testing.T) {
	sc := testScenario()
	sc.LocalOK = false
	_, d, _, _ := setup(nil)
	_, err := scenario.Run(t.Context(), sc, d, opts(1))
	require.ErrorIs(t, err, scenario.ErrNeedsCluster)
}

func TestRun_RejectsBadOptions(t *testing.T) {
	_, d, _, _ := setup(nil)
	for name, mod := range map[string]func(*scenario.Options){
		"no vus":              func(o *scenario.Options) { o.VUs = 0 },
		"no env":              func(o *scenario.Options) { o.Env = "" },
		"revert before start": func(o *scenario.Options) { o.RevertAt = o.InjectAt },
		"negative scale":      func(o *scenario.Options) { o.Scale = -1 },
	} {
		o := opts(1)
		mod(&o)
		_, err := scenario.Run(t.Context(), testScenario(), d, o)
		require.Error(t, err, name)
	}
}

func TestRun_NoFaultScenarioStillRunsLoadAndVerify(t *testing.T) {
	sc := scenario.Scenario{ID: "FN", Title: "no injection", LocalOK: true,
		Expect: []scenario.Assertion{{Kind: scenario.AssertVerify, Name: "integrity"}}}
	l, d, _, _ := setup(nil)
	rep, err := scenario.Run(t.Context(), sc, d, opts(1))
	require.NoError(t, err)
	require.True(t, rep.Pass, rep.Reason)
	require.NotContains(t, strings.Join(l.get(), ","), "do:")
}
