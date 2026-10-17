//go:build !windows

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
	"github.com/nabePi/kotoki-backend/test/load/gate"
	"github.com/nabePi/kotoki-backend/test/load/promtext"
	"github.com/nabePi/kotoki-backend/test/load/scenario"
	"github.com/nabePi/kotoki-backend/test/load/server"
	"github.com/nabePi/kotoki-backend/test/load/vu"
)

// outboxLagMetric is sampled during a scrape-mode run: the worst pod is the lag of that instant.
const outboxLagMetric = "kotoki_outbox_oldest_pending_age_seconds"

// runOptions are the parsed flags of `run`.
type runOptions struct {
	Spec                        gate.Spec // the gate with --vus/--ramp/--hold applied: the whole run, all instances
	APIURL, WSURL, Origin       string
	UsersFile, Fixtures, Out    string
	Instances, InstanceIndex    int
	PromURL                     string
	MetricsURLs                 []string
	Seed                        int64
	BackgroundREST, SampleEvery time.Duration
}

func parseRun(args []string) (runOptions, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		o                 runOptions
		gateName, profile string
		metricsURLs       string
		vus               int
		ramp, hold        time.Duration
		noAudioSave       bool
	)
	fs.StringVar(&o.APIURL, "api-url", "http://localhost:8080", "REST API base URL")
	fs.StringVar(&o.WSURL, "ws-url", "ws://localhost:8081/api/v1/realtime", "realtime WebSocket URL")
	fs.StringVar(&o.Origin, "origin", "", "Origin header of the WebSocket handshake")
	fs.StringVar(&o.UsersFile, "users-file", "users.csv", "email,password per user (kotoki-load seed-users)")
	fs.IntVar(&vus, "vus", 0, "concurrent users of the whole run, all instances together (default: the gate's)")
	fs.DurationVar(&ramp, "ramp", 0, "ramp-up (default: the gate's)")
	fs.DurationVar(&hold, "hold", 0, "time at full load after the ramp (default: the gate's)")
	fs.StringVar(&o.Fixtures, "fixtures", "testdata/audio", "directory with manifest.json and the WAV fixtures (or its synth/ subdirectory)")
	fs.StringVar(&profile, "profile", "default", "behaviour profile (only default)")
	fs.IntVar(&o.Instances, "instances", 1, "number of generator instances sharing the run")
	fs.IntVar(&o.InstanceIndex, "instance-index", 0, "this instance, 0-based")
	fs.StringVar(&o.PromURL, "prom-url", "", "Prometheus base URL for the server-side criteria (staging)")
	fs.StringVar(&metricsURLs, "metrics-urls", "", "comma-separated /metrics URLs to scrape before and after the run (local)")
	fs.StringVar(&gateName, "gate", "", "gate: g1, g2, g3, g4a, g4b, soak, real, g1-local")
	fs.StringVar(&o.Out, "out", "", "result file (JSON)")
	fs.Int64Var(&o.Seed, "seed", 1, "seed of the behaviour model")
	fs.DurationVar(&o.BackgroundREST, "background-rest", time.Minute, "period of each user's background REST calls (0: off)")
	fs.DurationVar(&o.SampleEvery, "sample-every", 15*time.Second, "period of the outbox lag samples in --metrics-urls mode (0: off)")
	fs.BoolVar(&noAudioSave, "no-audio-save", true, "the generator never decodes or saves the AI audio (always on)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case fs.NArg() > 0:
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case !noAudioSave:
		return o, errors.New("--no-audio-save cannot be turned off: the generator never decodes or saves the AI audio")
	case profile != "default":
		return o, fmt.Errorf("unknown --profile %q (only default)", profile)
	case gateName == "":
		return o, errors.New("--gate is required")
	}
	spec, ok := gate.Specs[gateName]
	if !ok {
		return o, fmt.Errorf("unknown --gate %q", gateName)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["vus"] {
		if vus <= 0 {
			return o, errors.New("--vus must be positive")
		}
		spec.VUs = vus
	}
	if set["ramp"] {
		spec.Ramp = ramp
	}
	if set["hold"] {
		spec.Hold = hold
	}
	if spec.Ramp < 0 || spec.Hold < 0 {
		return o, errors.New("--ramp and --hold cannot be negative")
	}
	o.Spec = spec
	if o.Instances < 1 || o.InstanceIndex < 0 || o.InstanceIndex >= o.Instances {
		return o, fmt.Errorf("--instance-index %d is outside --instances %d", o.InstanceIndex, o.Instances)
	}
	switch {
	case metricsURLs != "" && o.PromURL != "":
		return o, errors.New("--metrics-urls and --prom-url are exclusive")
	case metricsURLs == "" && o.PromURL == "":
		return o, errors.New("one of --metrics-urls or --prom-url is required: a gate cannot pass without server data")
	}
	for _, u := range strings.Split(metricsURLs, ",") {
		if u = strings.TrimSpace(u); u != "" {
			m, err := metricsURL(u)
			if err != nil {
				return o, err
			}
			o.MetricsURLs = append(o.MetricsURLs, m)
		}
	}
	if metricsURLs != "" && len(o.MetricsURLs) == 0 {
		return o, errors.New("--metrics-urls is empty")
	}
	return o, nil
}

// metricsURL adds /metrics to a bare host URL (the ops port serves it there).
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

// readUsers reads users.csv: email,password per row, the header row optional.
func readUsers(path string) ([]vu.Credential, error) {
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
	var out []vu.Credential
	for i, row := range rows {
		if i == 0 && row[0] == "email" {
			continue
		}
		out = append(out, vu.Credential{Email: row[0], Password: row[1]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no users", path)
	}
	return out, nil
}

// sliceForInstance gives instance index its share of the run: the VUs of the whole run are divided
// over the instances (the first ones take the remainder), and the users file is cut into disjoint
// contiguous parts of that size.
func sliceForInstance(users []vu.Credential, totalVUs, instances, index int) ([]vu.Credential, int, error) {
	if instances < 1 || index < 0 || index >= instances {
		return nil, 0, fmt.Errorf("instance %d is outside %d instances", index, instances)
	}
	if totalVUs > len(users) {
		return nil, 0, fmt.Errorf("%d VUs need as many users, the users file has %d (kotoki-load seed-users --count)", totalVUs, len(users))
	}
	per, rem := totalVUs/instances, totalVUs%instances
	n := per
	if index < rem {
		n++
	}
	if n == 0 {
		return nil, 0, fmt.Errorf("%d VUs are too few for %d instances", totalVUs, instances)
	}
	start := index*per + min(index, rem)
	return users[start : start+n], n, nil
}

// runConfig is a run that is ready to start.
type runConfig struct {
	Spec   gate.Spec
	Runner *vu.Runner
	// Exactly one of Source (scrape mode: a snapshot before and after, plus periodic outbox lag
	// samples) and Prom (staging) provides the server-side observations.
	Source server.Source
	Prom   server.Querier
	// Extra supplies observations that neither provides (K10-K12 from kube, K13/K14 from
	// kotoki-verify); its keys override the server's.
	Extra       func(ctx context.Context) (gate.Observation, error)
	SampleEvery time.Duration
	Out         string
	Meta        map[string]string
	Instance    int
	Instances   int
	Now         func() time.Time
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseRun(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-load run: %v\n", err)
		return 2
	}
	cfg, err := build(o, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-load run: %v\n", err)
		return 2
	}
	return execute(ctx, cfg, stdout, stderr)
}

// build wires a run from its options: users, fixtures (pre-encoded once), planner, runner, sources.
func build(o runOptions, stderr io.Writer) (runConfig, error) {
	all, err := readUsers(o.UsersFile)
	if err != nil {
		return runConfig{}, err
	}
	users, vus, err := sliceForInstance(all, o.Spec.VUs, o.Instances, o.InstanceIndex)
	if err != nil {
		return runConfig{}, err
	}
	fixtures, dir, err := scenario.LoadFixtures(o.Fixtures)
	if err != nil {
		return runConfig{}, err
	}
	// VU i of every instance must draw its own sequence: the seed moves with the instance.
	planner := scenario.NewPlanner(o.Seed+int64(o.InstanceIndex)*1_000_000, fixtures)
	names := planner.FixtureNames()
	if len(names) == 0 {
		return runConfig{}, fmt.Errorf("no usable speech fixture in %s", dir)
	}
	pre := make(map[string][][]byte, len(names))
	for _, n := range names {
		pcm, err := audio.ReadWAV(filepath.Join(dir, n+".wav"))
		if err != nil {
			return runConfig{}, fmt.Errorf("fixture %s: %w", n, err)
		}
		if pre[n], err = audio.PreEncode(pcm); err != nil {
			return runConfig{}, fmt.Errorf("fixture %s: %w", n, err)
		}
	}
	if _, ok := pre[scenario.BargeInFixture]; !ok {
		_, _ = fmt.Fprintf(stderr, "kotoki-load: warning: no %s fixture, the run has no barge-in\n", scenario.BargeInFixture)
	}
	cfg := runConfig{
		Spec: o.Spec, Out: o.Out, SampleEvery: o.SampleEvery, Instance: o.InstanceIndex, Instances: o.Instances,
		Runner: &vu.Runner{
			Users: users, VUs: vus, Planner: planner, Conv: kotokiclient.RunConversation, Agg: vu.NewAggregator(),
			Ramp: o.Spec.Ramp, Hold: o.Spec.Hold, BackgroundRESTEvery: o.BackgroundREST,
			APIURL: o.APIURL, WSURL: o.WSURL, Origin: o.Origin, Pre: pre,
			Logf: func(f string, a ...any) { _, _ = fmt.Fprintf(stderr, "kotoki-load: "+f+"\n", a...) },
		},
		Meta: map[string]string{"api_url": o.APIURL, "fixtures": dir, "seed": fmt.Sprint(o.Seed)},
	}
	if o.PromURL != "" {
		cfg.Prom = server.PromAPI{BaseURL: o.PromURL}
	} else {
		cfg.Source = server.Scraper{URLs: o.MetricsURLs}
	}
	return cfg, nil
}

// execute runs the VUs, evaluates the gate and writes the result; it returns the exit status.
func execute(ctx context.Context, cfg runConfig, stdout, stderr io.Writer) int {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	var before promtext.Samples
	if cfg.Source != nil {
		var err error
		if before, err = cfg.Source.Snapshot(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "kotoki-load run: baseline scrape failed, not starting: %v\n", err)
			return 2
		}
	}
	started := now()
	agg := cfg.Runner.Agg

	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		errs   []string
		addErr = func(format string, a ...any) {
			errMu.Lock()
			errs = append(errs, fmt.Sprintf(format, a...))
			errMu.Unlock()
		}
		sampler context.CancelFunc
	)
	if cfg.Source != nil && cfg.SampleEvery > 0 {
		var sctx context.Context
		sctx, sampler = context.WithCancel(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for kotokiclient.SleepContext(sctx, cfg.SampleEvery) == nil {
				snap, err := cfg.Source.Snapshot(sctx)
				if err != nil {
					if sctx.Err() == nil {
						addErr("outbox lag sample: %v", err)
					}
					continue
				}
				if v, ok := maxOf(snap, outboxLagMetric); ok {
					agg.AddOutboxLag(v)
				}
			}
		}()
	}
	runErr := cfg.Runner.Run(ctx)
	if sampler != nil {
		sampler()
	}
	wg.Wait()
	if runErr != nil {
		_, _ = fmt.Fprintf(stderr, "kotoki-load run: %v\n", runErr)
		return 2
	}
	finished := now()

	// the run is over: collect its results even when ctx was cancelled to end it
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	obs := gate.Observation{}
	switch {
	case cfg.Prom != nil:
		window := max(finished.Sub(started).Round(time.Second), time.Second)
		o, es := server.FromProm(fctx, cfg.Prom, window)
		obs = o
		for _, e := range es {
			addErr("prometheus: %v", e)
		}
	case cfg.Source != nil:
		if after, err := cfg.Source.Snapshot(fctx); err != nil {
			addErr("final scrape: %v", err)
		} else {
			obs = server.FromScrape(before, after, agg.Snapshot())
		}
	}
	if cfg.Extra != nil {
		if eo, err := cfg.Extra(fctx); err != nil {
			addErr("extra observations: %v", err)
		} else {
			for k, v := range eo {
				obs[k] = v
			}
		}
	}
	// what the generator measured itself is authoritative for the client-side keys
	for k, v := range server.FromScrape(nil, nil, agg.Snapshot()) {
		obs[k] = v
	}

	results := gate.Evaluate(cfg.Spec, obs)
	pass := gate.AllPass(results)
	verdict := "FAIL"
	if pass {
		verdict = "PASS"
	}
	printResults(stdout, cfg.Spec, results, verdict)
	for _, e := range errs {
		_, _ = fmt.Fprintf(stderr, "kotoki-load run: %s\n", e)
	}
	if cfg.Out != "" {
		meta := map[string]string{"started_at": started.UTC().Format(time.RFC3339), "finished_at": finished.UTC().Format(time.RFC3339)}
		for k, v := range cfg.Meta {
			meta[k] = v
		}
		if err := writeResult(cfg, results, obs, pass, errs, meta); err != nil {
			_, _ = fmt.Fprintf(stderr, "kotoki-load run: write %s: %v\n", cfg.Out, err)
			return 2
		}
	}
	if !pass {
		return 1
	}
	return 0
}

func maxOf(s promtext.Samples, name string) (float64, bool) {
	var m float64
	found := false
	for _, x := range s {
		if x.Name == name && !math.IsNaN(x.Value) && (!found || x.Value > m) {
			m, found = x.Value, true
		}
	}
	return m, found
}

func printResults(w io.Writer, spec gate.Spec, rs []gate.Result, verdict string) {
	for _, r := range rs {
		status, value := "FAIL", fmt.Sprintf("%g", r.Value)
		switch {
		case r.Skipped:
			status, value = "SKIP", "-"
		case r.Pass:
			status = "PASS"
		case math.IsNaN(r.Value):
			value = "-"
		}
		line := fmt.Sprintf("  %-4s %-5s %-10s %s (%s)", r.ID, status, value, r.Name, r.Threshold)
		if r.Reason != "" {
			line += ": " + r.Reason
		}
		_, _ = fmt.Fprintln(w, line)
	}
	_, _ = fmt.Fprintf(w, "GATE %s: %s\n", spec.Name, verdict)
}

type resultJSON struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Pass      bool     `json:"pass"`
	Skipped   bool     `json:"skipped"`
	Value     *float64 `json:"value"` // null: no data
	Threshold string   `json:"threshold"`
	Reason    string   `json:"reason,omitempty"`
}

func writeResult(cfg runConfig, rs []gate.Result, obs gate.Observation, pass bool, errs []string, meta map[string]string) error {
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	out := struct {
		Gate        string             `json:"gate"`
		Mode        string             `json:"mode"`
		VUs         int                `json:"vus"`
		Ramp        string             `json:"ramp"`
		Hold        string             `json:"hold"`
		Instance    int                `json:"instance"`
		Instances   int                `json:"instances"`
		Pass        bool               `json:"pass"`
		Results     []resultJSON       `json:"results"`
		Observation map[string]float64 `json:"observation"`
		Client      vu.Summary         `json:"client"`
		Errors      []string           `json:"errors"`
		Meta        map[string]string  `json:"meta"`
	}{
		Gate: cfg.Spec.Name, Mode: cfg.Spec.Mode, VUs: cfg.Spec.VUs, Ramp: cfg.Spec.Ramp.String(), Hold: cfg.Spec.Hold.String(),
		Instance: cfg.Instance, Instances: cfg.Instances, Pass: pass, Observation: map[string]float64{},
		Client: cfg.Runner.Agg.Summary(), Errors: append([]string{}, errs...), Meta: meta,
	}
	for _, r := range rs {
		j := resultJSON{ID: r.ID, Name: r.Name, Pass: r.Pass, Skipped: r.Skipped, Threshold: r.Threshold, Reason: r.Reason}
		if finite(r.Value) {
			v := r.Value
			j.Value = &v
		}
		out.Results = append(out.Results, j)
	}
	for k, v := range obs {
		if finite(v) {
			out.Observation[k] = v
		}
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Out), 0o750); err != nil {
		return err
	}
	return os.WriteFile(cfg.Out, append(raw, '\n'), 0o600)
}
