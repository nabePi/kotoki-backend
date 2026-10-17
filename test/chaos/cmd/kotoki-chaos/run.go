//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/test/chaos/driver"
	"github.com/nabePi/kotoki-backend/test/chaos/live"
	"github.com/nabePi/kotoki-backend/test/chaos/probe"
	"github.com/nabePi/kotoki-backend/test/chaos/scenario"
)

func cmdList(stdout io.Writer) int {
	for _, s := range scenario.Table {
		where := "cluster only (MANUAL GATE, Task 24)"
		if s.LocalOK {
			where = "compose or cluster"
		}
		_, _ = fmt.Fprintf(stdout, "%-4s %-55s %s\n", s.ID, s.Title, where)
	}
	return 0
}

// options are the parsed flags of `run`.
type options struct {
	ID    string
	Env   scenario.Env
	Opts  scenario.Options
	Tail  time.Duration
	Scale float64

	APIURL, WSURL, Origin string
	UsersFile, Fixtures   string
	MetricsURLs           []string
	PromURL               string
	DB, RedisURL          string
	AwaitEvaluations      time.Duration
	ComposeProject        string
	LoadBin, OutDir       string
	SpreadIPs             bool
	SampleEvery           time.Duration
	RefillEvery           time.Duration
}

func parseRun(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		env, metrics       string
		vus                int
		scale              float64
		injectAt, revertAt time.Duration
		tail               time.Duration
	)
	fs.StringVar(&env, "env", "", "compose or kube (required)")
	fs.IntVar(&vus, "vus", 100, "concurrent users (spec 13.13.1: 100)")
	fs.Float64Var(&scale, "time-scale", 1, "multiplies the waits: 1 = real time (inject T+5m, revert T+15m), 0.1 = a tenth")
	fs.DurationVar(&injectAt, "inject-at", 5*time.Minute, "inject this long after the load starts (before --time-scale)")
	fs.DurationVar(&revertAt, "revert-at", 15*time.Minute, "revert this long after the load starts (before --time-scale)")
	fs.DurationVar(&tail, "tail", 2*time.Minute, "keep the load running this long after the revert, to see the recovery (before --time-scale)")
	fs.StringVar(&o.APIURL, "api-url", "http://localhost:8080", "REST API base URL")
	fs.StringVar(&o.WSURL, "ws-url", "ws://localhost:8081/api/v1/realtime", "realtime WebSocket URL")
	fs.StringVar(&o.Origin, "origin", "http://localhost:3000", "Origin header of the WebSocket handshake")
	fs.StringVar(&o.UsersFile, "users-file", "", "email,password per user (kotoki-load seed-users); the last user is the probe's, so it needs more users than --vus")
	fs.StringVar(&o.Fixtures, "fixtures", "testdata/audio", "directory with manifest.json and the WAV fixtures")
	fs.StringVar(&metrics, "metrics-urls", "", "comma-separated ops URLs scraped for the assertions (compose)")
	fs.StringVar(&o.PromURL, "prom-url", "", "Prometheus base URL for the assertions (kube)")
	fs.StringVar(&o.DB, "db", envOr("DATABASE_URL", ""), "PostgreSQL URL for kotoki-verify (env DATABASE_URL)")
	fs.StringVar(&o.RedisURL, "redis-url", envOr("REDIS_URL", "redis://localhost:6379/0"), "Redis URL of the fake provider faults (env REDIS_URL)")
	fs.DurationVar(&o.AwaitEvaluations, "await-evaluations", 5*time.Minute, "kotoki-verify V8: how long completed sessions get to receive their evaluation")
	fs.StringVar(&o.ComposeProject, "compose-project", "", "compose project of the run: the only one the commands touch (required for --env compose)")
	fs.StringVar(&o.LoadBin, "load-bin", "bin/kotoki-load", "the kotoki-load binary")
	fs.StringVar(&o.OutDir, "out-dir", "out/chaos", "directory of the report and the load output")
	fs.BoolVar(&o.SpreadIPs, "spread-ips", true, "each VU presents its own address (local runs behind TRUSTED_PROXY_CIDRS)")
	fs.DurationVar(&o.SampleEvery, "sample-every", 2*time.Second, "period of the /metrics samples (compose)")
	fs.DurationVar(&o.RefillEvery, "refill-every", 5*time.Second, "give the users of the load their session-start budgets back this often (0: only at the start); see the reset action of the scenario package")

	// the scenario id is the first argument: flags may follow it
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return o, errors.New("run needs a scenario id first (F1..F16, see `kotoki-chaos list`)")
	}
	o.ID = strings.ToUpper(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	switch scenario.Env(env) {
	case scenario.Compose, scenario.Kube:
		o.Env = scenario.Env(env)
	default:
		return o, errors.New("--env must be compose or kube")
	}
	if scale <= 0 {
		return o, errors.New("--time-scale must be positive")
	}
	if vus <= 0 {
		return o, errors.New("--vus must be positive")
	}
	if revertAt <= injectAt {
		return o, errors.New("--revert-at must be after --inject-at")
	}
	if tail < 0 {
		return o, errors.New("--tail cannot be negative")
	}
	o.Scale = scale
	o.Tail = tail
	o.Opts = scenario.Options{Env: o.Env, VUs: vus, Scale: scale, InjectAt: injectAt, RevertAt: revertAt, Total: revertAt + tail}
	for _, u := range strings.Split(metrics, ",") {
		if u = strings.TrimSpace(u); u != "" {
			m, err := metricsURL(u)
			if err != nil {
				return o, err
			}
			o.MetricsURLs = append(o.MetricsURLs, m)
		}
	}
	if o.Env == scenario.Compose {
		switch {
		case o.ComposeProject == "":
			return o, errors.New("--compose-project is required for --env compose: the commands touch only that project")
		case len(o.MetricsURLs) == 0:
			return o, errors.New("--metrics-urls is required for --env compose: the stack has no Prometheus")
		case o.PromURL != "":
			return o, errors.New("--prom-url is for --env kube")
		}
	} else if o.PromURL == "" {
		return o, errors.New("--prom-url is required for --env kube")
	}
	if o.UsersFile == "" {
		return o, errors.New("--users-file is required")
	}
	if o.DB == "" {
		return o, errors.New("--db (or DATABASE_URL) is required: kotoki-verify belongs to every scenario")
	}
	return o, nil
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func metricsURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("bad metrics URL %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/metrics"
	}
	return u.String(), nil
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseRun(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: %v\n", err)
		return 2
	}
	sc, ok := scenario.ByID(o.ID)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: unknown scenario %q (F14 was removed from v1; see `kotoki-chaos list`)\n", o.ID)
		return 2
	}
	if o.Env == scenario.Compose && !sc.LocalOK {
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: %s needs a cluster: %v\n", sc.ID, scenario.ErrNeedsCluster)
		return 2
	}
	if err := os.MkdirAll(o.OutDir, 0o750); err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: %v\n", err)
		return 2
	}
	deps, cleanup, err := build(ctx, o, sc, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: %v\n", err)
		return 2
	}
	defer cleanup()

	started := time.Now()
	rep, runErr := scenario.Run(ctx, sc, deps, o.Opts)
	finished := time.Now()
	printReport(stdout, sc, rep, runErr)
	if err := writeReport(o, sc, rep, runErr, started, finished); err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: write report: %v\n", err)
	}
	switch {
	case runErr != nil:
		_, _ = fmt.Fprintf(stderr, "kotoki-chaos run: %v\n", runErr)
		return 2
	case !rep.Pass:
		return 1
	}
	return 0
}

// build wires the collaborators of one run.
func build(ctx context.Context, o options, sc scenario.Scenario, stdout, stderr io.Writer) (scenario.Deps, func(), error) {
	var (
		cleanups []func()
		refillWG sync.WaitGroup
	)
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (scenario.Deps, func(), error) { cleanup(); return scenario.Deps{}, func() {}, err }

	users, err := readUsers(o.UsersFile)
	if err != nil {
		return fail(err)
	}
	if len(users) <= o.Opts.VUs {
		return fail(fmt.Errorf("%s has %d users, --vus %d needs more: the last user is the probe's (seed-users --count)", o.UsersFile, len(users), o.Opts.VUs))
	}

	ropt, err := redis.ParseURL(o.RedisURL)
	if err != nil {
		return fail(errors.New("--redis-url (or REDIS_URL) is not a valid Redis URL"))
	}
	rdb := redis.NewClient(ropt)
	cleanups = append(cleanups, func() { _ = rdb.Close() })

	logf := func(format string, a ...any) { _, _ = fmt.Fprintf(stdout, format+"\n", a...) }
	probeUser := users[len(users)-1]
	common := probe.Common{
		APIURL: o.APIURL, WSURL: o.WSURL, Origin: o.Origin, Email: probeUser[0], Password: probeUser[1],
		FixtureDir: o.Fixtures, Out: logf,
	}
	if o.SpreadIPs {
		common.ClientIP = "10.254.0.1" // outside the range kotoki-load --spread-ips hands to its VUs
	}

	d := driver.Multi{
		Fault: driver.Redis{KV: driver.GoRedis{Client: rdb}, Until: 2 * (o.Opts.Total + o.AwaitEvaluations)},
		Probe: driver.Probe{Funcs: map[string]func(context.Context, []string) error{
			scenario.ProbeConverseDrop:   probe.ConverseDrop{Common: common}.Run,
			scenario.ProbeExpirySpeaking: probe.ExpirySpeaking{Common: common}.Run,
		}},
	}
	deps := scenario.Deps{Driver: d, Clock: realClock{}}

	loadOut := filepath.Join(o.OutDir, sc.ID+"-load.json")
	loadLog, err := os.Create(filepath.Join(o.OutDir, sc.ID+"-load.log"))
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, func() { _ = loadLog.Close() })
	ramp := min(time.Minute, o.Opts.InjectAt/2)
	loadArgs := []string{"run", "--gate", "g1-local", "--api-url", o.APIURL, "--ws-url", o.WSURL, "--origin", o.Origin,
		"--users-file", o.UsersFile, "--fixtures", o.Fixtures, "--hold", "0",
		"--ramp", time.Duration(float64(ramp) * o.Scale).String(), "--out", loadOut}
	if o.SpreadIPs {
		loadArgs = append(loadArgs, "--spread-ips")
	}

	switch o.Env {
	case scenario.Compose:
		if err := requireProject(ctx, o.ComposeProject); err != nil {
			return fail(err)
		}
		loadArgs = append(loadArgs, "--metrics-urls", strings.Join(o.MetricsURLs, ","))
		sampler := live.NewSampler(o.MetricsURLs, o.SampleEvery)
		sctx, cancel := context.WithCancel(ctx)
		idle := live.IdleWaiter{Count: live.CountOpenSessions(o.DB), Out: stdout}
		// the baseline of every counter is taken only once nothing of an earlier run is left, or
		// the leftovers would be counted as effects of this one
		deps.Prepare = func(ctx context.Context) error {
			if err := idle.Wait(ctx); err != nil {
				return err
			}
			// ... and their session budget is whole (an earlier scenario may have used it up)
			if err := d.Do(ctx, scenario.Action{Kind: scenario.KindReset, Argv: []string{scenario.ResetRateLimits}}); err != nil {
				return err
			}
			if err := sampler.Prime(ctx); err != nil {
				return fmt.Errorf("baseline scrape failed, not starting: %w", err)
			}
			sampler.Start(sctx)
			startRefill(sctx, d, o.RefillEvery, stderr, &refillWG)
			return nil
		}
		cleanups = append(cleanups, func() {
			cancel()
			refillWG.Wait()
			sampler.Wait()
			for _, e := range sampler.Errors() {
				_, _ = fmt.Fprintf(stderr, "kotoki-chaos: sampler: %s\n", e)
			}
		})
		deps.Prom = sampler
		env := append(os.Environ(), "COMPOSE_PROJECT_NAME="+o.ComposeProject)
		run := func(ctx context.Context, argv []string) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is checked by driver.Compose
			cmd.Env = env
			cmd.Stdout, cmd.Stderr = stdout, stderr
			return cmd.Run()
		}
		deps.Driver = withCmd(d, driver.Compose{Run: run, Resolve: live.Republisher{Find: live.FindPublished(o.DB), Since: time.Now()}.Resolve})
	case scenario.Kube:
		loadArgs = append(loadArgs, "--prom-url", o.PromURL)
		deps.Prom = promAPI(o.PromURL)
		kctx, kcancel := context.WithCancel(ctx)
		cleanups = append(cleanups, func() { kcancel(); refillWG.Wait() })
		deps.Prepare = func(ctx context.Context) error {
			if err := d.Do(ctx, scenario.Action{Kind: scenario.KindReset, Argv: []string{scenario.ResetRateLimits}}); err != nil {
				return err
			}
			startRefill(kctx, d, o.RefillEvery, stderr, &refillWG)
			return nil
		}
		run := func(ctx context.Context, argv []string) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is checked by driver.Kube
			cmd.Stdout, cmd.Stderr = stdout, stderr
			return cmd.Run()
		}
		output := func(ctx context.Context, argv []string) (string, error) {
			var b bytes.Buffer
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is built by driver.Kube
			cmd.Stdout, cmd.Stderr = &b, stderr
			err := cmd.Run()
			return b.String(), err
		}
		deps.Driver = withCmd(d, driver.Kube{Run: run, Output: output, Namespace: "kotoki"})
	}
	deps.Load = live.Loader{Bin: o.LoadBin, Args: loadArgs, Out: loadLog}
	deps.Verify = live.Verifier{DSN: o.DB, Await: o.AwaitEvaluations, Out: stdout}
	return deps, cleanup, nil
}

// startRefill empties the session-start budgets of the rate limiter every period until ctx ends. Sessions
// that a fault makes fail within seconds use up session_create_user (5 burst + 10 per hour),
// ws_connect_ip (30 per minute) and ws_attach_user (10 per minute); a user without budget makes no
// request, or is refused at the WebSocket and stays with a RESERVED session that blocks every new one. The circuit breaker would never get the
// call that half-opens it, and the recovery after the revert could not be seen. The limiter itself is not
// under test here.
func startRefill(ctx context.Context, d scenario.Driver, every time.Duration, errOut io.Writer, wg *sync.WaitGroup) {
	if every <= 0 {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := d.Do(ctx, scenario.Action{Kind: scenario.KindReset, Argv: []string{scenario.ResetRateLimits}}); err != nil && ctx.Err() == nil {
					_, _ = fmt.Fprintf(errOut, "kotoki-chaos: refill: %v\n", err)
				}
			}
		}
	}()
}

func withCmd(m driver.Multi, cmd scenario.Driver) scenario.Driver {
	m.Cmd = cmd
	return m
}

// requireProject refuses to run unless the compose project already has containers: the commands of a
// scenario restart or stop services of that project, and it must be the one under test.
func requireProject(ctx context.Context, project string) error {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-p", project, "ps", "-q") //nolint:gosec // G204: project is an operator flag
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("docker compose -p %s ps: %w", project, err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("compose project %q has no containers: start the stack first (the scenario only touches this project)", project)
	}
	return nil
}

func readUsers(path string) ([][2]string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator chooses the users file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(f)
	r.FieldsPerRecord = 2
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out [][2]string
	for i, row := range rows {
		if i == 0 && row[0] == "email" {
			continue
		}
		out = append(out, [2]string{row[0], row[1]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no users", path)
	}
	return out, nil
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	return kotokiclient.SleepContext(ctx, d)
}

func printReport(w io.Writer, sc scenario.Scenario, rep scenario.Report, runErr error) {
	_, _ = fmt.Fprintf(w, "\n%s %s\n", sc.ID, sc.Title)
	for _, r := range rep.Results {
		status := "FAIL"
		if r.Pass {
			status = "PASS"
		}
		if r.Kind == scenario.AssertManual {
			status = "TODO"
		}
		line := fmt.Sprintf("  %-5s %-60s", status, r.Name)
		if r.Kind != scenario.AssertManual {
			line += fmt.Sprintf(" value=%g", r.Value)
		}
		if r.Detail != "" {
			line += "  " + r.Detail
		}
		_, _ = fmt.Fprintln(w, line)
	}
	switch {
	case runErr != nil:
		_, _ = fmt.Fprintf(w, "%s: ERROR %v\n", sc.ID, runErr)
	case rep.Pass:
		_, _ = fmt.Fprintf(w, "%s: PASS\n", sc.ID)
	default:
		_, _ = fmt.Fprintf(w, "%s: FAIL (%s)\n", sc.ID, rep.Reason)
	}
}

func writeReport(o options, sc scenario.Scenario, rep scenario.Report, runErr error, started, finished time.Time) error {
	injectAt, revertAt, tail, scale := o.Opts.InjectAt, o.Opts.RevertAt, o.Tail, o.Scale
	if sc.Fixed != nil { // the scenario's own real-time window replaces the flags
		injectAt, revertAt, tail, scale = sc.Fixed.InjectAt, sc.Fixed.RevertAt, sc.Fixed.Tail, 1
	}
	doc := map[string]any{
		"id": sc.ID, "title": sc.Title, "env": o.Env, "pass": rep.Pass, "reason": rep.Reason,
		"vus": o.Opts.VUs, "time_scale": scale, "inject_at": injectAt.String(), "revert_at": revertAt.String(),
		"tail": tail.String(), "started_at": started.UTC().Format(time.RFC3339), "finished_at": finished.UTC().Format(time.RFC3339),
		"results": rep.Results,
	}
	if runErr != nil {
		doc["error"] = runErr.Error()
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.OutDir, sc.ID+".json"), append(b, '\n'), 0o600)
}
