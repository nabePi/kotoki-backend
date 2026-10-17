//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := realMain(context.Background(), args, &out, &errb)
	return rc, out.String(), errb.String()
}

func TestList(t *testing.T) {
	rc, out, _ := run(t, "list")
	require.Equal(t, 0, rc)
	for _, s := range scenario.Table {
		require.Contains(t, out, s.ID+" ")
	}
	require.NotContains(t, out, "F14 ")
	require.Contains(t, out, "cluster only")
}

func TestUsageAndUnknownCommand(t *testing.T) {
	rc, _, errs := run(t)
	require.Equal(t, 2, rc)
	require.Contains(t, errs, "usage")
	rc, _, _ = run(t, "explode")
	require.Equal(t, 2, rc)
	rc, out, _ := run(t, "help")
	require.Equal(t, 0, rc)
	require.Contains(t, out, "usage")
}

func users(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("email,password\n")
	for i := 1; i <= n; i++ {
		b.WriteString("load-" + string(rune('a'+i%26)) + "@load.test,pw\n")
	}
	p := filepath.Join(t.TempDir(), "users.csv")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

func base(t *testing.T) []string {
	return []string{"--env", "compose", "--compose-project", "p", "--metrics-urls", "http://localhost:9090",
		"--users-file", users(t, 3), "--db", "postgres://x"}
}

func TestParseRun_Defaults(t *testing.T) {
	o, err := parseRun(append([]string{"f1"}, base(t)...))
	require.NoError(t, err)
	require.Equal(t, "F1", o.ID)
	require.Equal(t, 100, o.Opts.VUs)
	require.Equal(t, 5*time.Minute, o.Opts.InjectAt)
	require.Equal(t, 15*time.Minute, o.Opts.RevertAt)
	require.Equal(t, 17*time.Minute, o.Opts.Total, "the load keeps running 2 minutes after the revert")
	require.Equal(t, []string{"http://localhost:9090/metrics"}, o.MetricsURLs)
	require.True(t, o.SpreadIPs)
}

func TestParseRun_Rejects(t *testing.T) {
	t.Setenv("DATABASE_URL", "") // the default of --db
	for name, args := range map[string][]string{
		"no id":                {"--env", "compose"},
		"flag first":           append([]string{"--vus", "5", "F1"}, base(t)...),
		"no env":               {"F1", "--users-file", "x", "--db", "y"},
		"bad env":              {"F1", "--env", "swarm"},
		"compose w/o project":  {"F1", "--env", "compose", "--metrics-urls", "http://h:1", "--users-file", "x", "--db", "y"},
		"compose w/o metrics":  {"F1", "--env", "compose", "--compose-project", "p", "--users-file", "x", "--db", "y"},
		"compose with prom":    append(base(t), "--prom-url", "http://prom"),
		"kube w/o prom":        {"F9", "--env", "kube", "--users-file", "x", "--db", "y"},
		"bad metrics url":      {"F1", "--env", "compose", "--compose-project", "p", "--metrics-urls", "nope", "--users-file", "x", "--db", "y"},
		"revert before inject": append(base(t), "--inject-at", "10m", "--revert-at", "5m"),
		"zero scale":           append(base(t), "--time-scale", "0"),
		"negative tail":        append(base(t), "--tail", "-1s"),
		"stray argument":       append(base(t), "extra"),
		"no db":                {"F1", "--env", "compose", "--compose-project", "p", "--metrics-urls", "http://h:1", "--users-file", "x"},
		"no users file":        {"F1", "--env", "compose", "--compose-project", "p", "--metrics-urls", "http://h:1", "--db", "y"},
	} {
		if name == "compose with prom" || name == "stray argument" || name == "zero scale" ||
			name == "negative tail" || name == "revert before inject" {
			args = append([]string{"F1"}, args...)
		}
		_, err := parseRun(args)
		require.Error(t, err, name)
	}
}

func TestRun_ClusterScenarioOnComposeIsRefusedBeforeAnythingStarts(t *testing.T) {
	args := append([]string{"run", "F9"}, base(t)...)
	rc, _, errs := run(t, args...)
	require.Equal(t, 2, rc)
	require.Contains(t, errs, "needs a cluster")
}

func TestRun_UnknownScenarioAndF14(t *testing.T) {
	for _, id := range []string{"F14", "F99"} {
		rc, _, errs := run(t, append([]string{"run", id}, base(t)...)...)
		require.Equal(t, 2, rc)
		require.Contains(t, errs, "unknown scenario")
	}
}

func TestRun_NeedsMoreUsersThanVUs(t *testing.T) {
	out := t.TempDir()
	args := append([]string{"run", "F1", "--vus", "3", "--out-dir", out}, base(t)...) // base has 3 users: none left for the probe
	rc, _, errs := run(t, args...)
	require.Equal(t, 2, rc)
	require.Contains(t, errs, "probe")
}

func TestReadUsers(t *testing.T) {
	u, err := readUsers(users(t, 2))
	require.NoError(t, err)
	require.Len(t, u, 2)
	_, err = readUsers(filepath.Join(t.TempDir(), "missing.csv"))
	require.Error(t, err)
}

func TestRealClockSleepStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, realClock{}.Sleep(ctx, time.Hour))
}

type countDriver struct{ resets atomic.Int32 }

func (c *countDriver) Do(_ context.Context, a scenario.Action) error {
	if a.Kind == scenario.KindReset && len(a.Argv) == 1 && a.Argv[0] == scenario.ResetRateLimits {
		c.resets.Add(1)
	}
	return nil
}

func TestStartRefill_ResetsPeriodicallyUntilTheContextEnds(t *testing.T) {
	d := &countDriver{}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	startRefill(ctx, d, 10*time.Millisecond, &bytes.Buffer{}, &wg)
	require.Eventually(t, func() bool { return d.resets.Load() >= 3 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	wg.Wait()
	n := d.resets.Load()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, n, d.resets.Load(), "no reset after the run ended")
}

func TestStartRefill_ZeroPeriodStartsNothing(t *testing.T) {
	d := &countDriver{}
	var wg sync.WaitGroup
	startRefill(context.Background(), d, 0, &bytes.Buffer{}, &wg)
	wg.Wait()
	require.Zero(t, d.resets.Load())
}

func TestParseRun_RefillDefault(t *testing.T) {
	o, err := parseRun(append([]string{"F1"}, base(t)...))
	require.NoError(t, err)
	require.Equal(t, 5*time.Second, o.RefillEvery)
	o, err = parseRun(append(append([]string{"F1"}, base(t)...), "--refill-every", "0"))
	require.NoError(t, err)
	require.Zero(t, o.RefillEvery)
}

func TestWriteReport_RecordsTheWindowThatWasActuallyUsed(t *testing.T) {
	read := func(o options, sc scenario.Scenario) map[string]any {
		t.Helper()
		o.OutDir = t.TempDir()
		rep := scenario.Report{ID: sc.ID, Pass: true, Results: []scenario.AssertionResult{{Name: "x", Kind: "prom", Pass: true, Value: 3}}}
		require.NoError(t, writeReport(o, sc, rep, nil, time.Unix(0, 0), time.Unix(60, 0)))
		raw, err := os.ReadFile(filepath.Join(o.OutDir, sc.ID+".json"))
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(raw, &doc))
		return doc
	}
	o := options{Env: scenario.Compose, Scale: 0.1, Tail: 2 * time.Minute,
		Opts: scenario.Options{VUs: 20, InjectAt: 5 * time.Minute, RevertAt: 15 * time.Minute}}

	doc := read(o, scenario.Scenario{ID: "FX", Title: "spec timeline"})
	require.Equal(t, "5m0s", doc["inject_at"])
	require.Equal(t, 0.1, doc["time_scale"])

	doc = read(o, scenario.Scenario{ID: "FY", Title: "fixed", Fixed: &scenario.Window{InjectAt: 90 * time.Second, RevertAt: 2 * time.Minute, Tail: 30 * time.Second}})
	require.Equal(t, "1m30s", doc["inject_at"])
	require.Equal(t, "2m0s", doc["revert_at"])
	require.Equal(t, "30s", doc["tail"])
	require.EqualValues(t, 1, doc["time_scale"], "a fixed window ignores --time-scale")
	require.Equal(t, true, doc["pass"])
	require.NotNil(t, doc["results"])
}
