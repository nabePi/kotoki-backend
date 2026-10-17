//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/test/load/gate"
	"github.com/nabePi/kotoki-backend/test/load/promtext"
	"github.com/nabePi/kotoki-backend/test/load/scenario"
	"github.com/nabePi/kotoki-backend/test/load/server"
	"github.com/nabePi/kotoki-backend/test/load/verify"
	"github.com/nabePi/kotoki-backend/test/load/vu"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- seed-users ----

type seedAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	emails []string
	grants map[string]int64 // user id -> seconds
	keys   map[string]bool
}

func newSeedAPI(t *testing.T, wrap ...func(http.Handler) http.Handler) *seedAPI {
	t.Helper()
	f := &seedAPI{grants: map[string]int64{}, keys: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Email, Password string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		if in.Email != "admin@kotoki.test" || in.Password != "adminpw" {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "INVALID_CREDENTIALS", "message": "no", "request_id": "q"}})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "tok-admin", "user": map[string]any{"id": "admin"}})
	})
	mux.HandleFunc("GET /api/v1/auth/terms", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"terms_version": "2026-10"})
	})
	mux.HandleFunc("POST /api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var in kotokiclient.RegisterRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "2026-10", in.TermsVersion)
		require.GreaterOrEqual(t, len(in.Password), 12)
		f.mu.Lock()
		f.emails = append(f.emails, in.Email)
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"access_token": "tok-" + in.Email, "user": map[string]any{"id": uuid.NewSHA1(uuid.NameSpaceDNS, []byte(in.Email)).String(), "email": in.Email}})
	})
	mux.HandleFunc("POST /api/v1/admin/users/{id}/balance-grants", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer tok-admin", r.Header.Get("Authorization"))
		var in struct {
			Seconds int64  `json:"seconds"`
			Reason  string `json:"reason"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		key := r.Header.Get("Idempotency-Key")
		_, err := uuid.Parse(key)
		require.NoError(t, err)
		f.mu.Lock()
		if f.keys[key] {
			t.Errorf("idempotency key %s used twice: keys are unique per user", key) // not require: this is a handler goroutine
		}
		f.keys[key] = true
		f.grants[r.PathValue("id")] = in.Seconds
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"id": "x"})
	})
	var h http.Handler = mux
	for _, w := range wrap {
		h = w(h)
	}
	f.srv = httptest.NewServer(h)
	t.Cleanup(f.srv.Close)
	return f
}

func TestSeedUsers_WritesCSV(t *testing.T) {
	api := newSeedAPI(t)
	out := filepath.Join(t.TempDir(), "users.csv")
	var stdout, stderr bytes.Buffer
	rc := realMain(context.Background(), []string{"seed-users", "--api-url", api.srv.URL, "--count", "5", "--balance", "36000",
		"--admin-email", "admin@kotoki.test", "--admin-password", "adminpw", "--concurrency", "2", "--out", out}, &stdout, &stderr)
	require.Equal(t, 0, rc, stderr.String())

	f, err := os.Open(out)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"email", "password"}, rows[0])
	require.Len(t, rows, 6)
	pw := map[string]bool{}
	for i, row := range rows[1:] {
		require.Equal(t, "load-"+itoa(i+1)+"@load.test", row[0], "rows follow the user index")
		require.GreaterOrEqual(t, len(row[1]), 12)
		pw[row[1]] = true
	}
	require.Len(t, pw, 5, "every user has its own password")
	require.Len(t, api.grants, 5)
	for _, s := range api.grants {
		require.Equal(t, int64(36000), s)
	}
	st, err := os.Stat(out)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "the file holds passwords")
	require.NotContains(t, stdout.String()+stderr.String(), rows[1][1], "passwords are never printed")
}

func TestSeedUsers_Errors(t *testing.T) {
	api := newSeedAPI(t)
	out := filepath.Join(t.TempDir(), "users.csv")
	var stdout, stderr bytes.Buffer
	rc := realMain(context.Background(), []string{"seed-users", "--api-url", api.srv.URL, "--count", "2",
		"--admin-email", "admin@kotoki.test", "--admin-password", "wrong", "--out", out}, &stdout, &stderr)
	require.Equal(t, 2, rc)
	require.Contains(t, stderr.String(), "admin login")
	_, err := os.Stat(out)
	require.True(t, os.IsNotExist(err), "no file on failure")

	rc = realMain(context.Background(), []string{"seed-users", "--count", "0", "--admin-email", "a", "--admin-password", "b", "--out", out}, &stdout, &stderr)
	require.Equal(t, 2, rc)
}

func itoa(i int) string { return strconv.Itoa(i) }

// ---- users file, instances ----

func TestReadUsers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.csv")
	require.NoError(t, os.WriteFile(p, []byte("email,password\na@load.test,pw1\nb@load.test,pw,with,comma\n"), 0o600))
	_, err := readUsers(p)
	require.Error(t, err, "three fields is not email,password")

	require.NoError(t, os.WriteFile(p, []byte("email,password\na@load.test,pw1\nb@load.test,\"pw,2\"\n"), 0o600))
	us, err := readUsers(p)
	require.NoError(t, err)
	require.Equal(t, []vu.Credential{{Email: "a@load.test", Password: "pw1"}, {Email: "b@load.test", Password: "pw,2"}}, us)

	require.NoError(t, os.WriteFile(p, []byte("a@load.test,pw1\n"), 0o600))
	us, err = readUsers(p)
	require.NoError(t, err, "the header is optional")
	require.Len(t, us, 1)

	_, err = readUsers(filepath.Join(t.TempDir(), "missing.csv"))
	require.Error(t, err)
}

func TestSliceForInstance(t *testing.T) {
	users := make([]vu.Credential, 10)
	for i := range users {
		users[i].Email = string(rune('a' + i))
	}
	var all []string
	vusSum := 0
	for i := 0; i < 3; i++ {
		us, vus, err := sliceForInstance(users, 10, 3, i)
		require.NoError(t, err)
		require.Len(t, us, vus, "an instance holds as many users as VUs")
		vusSum += vus
		for _, u := range us {
			all = append(all, u.Email)
		}
	}
	require.Equal(t, 10, vusSum, "the instances together run every VU")
	require.Equal(t, []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, all, "disjoint, contiguous")

	_, _, err := sliceForInstance(users, 10, 3, 3)
	require.Error(t, err)
	_, _, err = sliceForInstance(users, 11, 1, 0)
	require.ErrorContains(t, err, "users")
	_, _, err = sliceForInstance(users, 10, 0, 0)
	require.Error(t, err)
}

// ---- flags ----

func TestParseRun_GateDefaultsAndOverrides(t *testing.T) {
	o, err := parseRun([]string{"--gate", "g2", "--metrics-urls", "http://a:9090,http://b:9091/metrics", "--out", "o.json"})
	require.NoError(t, err)
	require.Equal(t, 250, o.Spec.VUs)
	require.Equal(t, 5*time.Minute, o.Spec.Ramp)
	require.Equal(t, 30*time.Minute, o.Spec.Hold)
	require.Equal(t, []string{"http://a:9090/metrics", "http://b:9091/metrics"}, o.MetricsURLs, "a bare host gets /metrics")
	require.Equal(t, "o.json", o.Out)

	o, err = parseRun([]string{"--gate", "g1-local", "--vus", "20", "--ramp", "10s", "--hold", "1m", "--prom-url", "http://prom:9090"})
	require.NoError(t, err)
	require.Equal(t, 20, o.Spec.VUs)
	require.Equal(t, 10*time.Second, o.Spec.Ramp)
	require.Equal(t, time.Minute, o.Spec.Hold)
	require.Equal(t, "g1-local", o.Spec.Name)
	require.NotEmpty(t, o.Spec.Skip, "the gate's skips are kept")
}

func TestParseRun_Rejects(t *testing.T) {
	for name, args := range map[string][]string{
		"no gate":         {"--metrics-urls", "http://a"},
		"unknown gate":    {"--gate", "g9", "--metrics-urls", "http://a"},
		"no server data":  {"--gate", "g1"},
		"both sources":    {"--gate", "g1", "--metrics-urls", "http://a", "--prom-url", "http://p"},
		"bad profile":     {"--gate", "g1", "--metrics-urls", "http://a", "--profile", "spiky"},
		"zero vus":        {"--gate", "g1", "--metrics-urls", "http://a", "--vus", "0"},
		"instance range":  {"--gate", "g1", "--metrics-urls", "http://a", "--instances", "2", "--instance-index", "2"},
		"stray argument":  {"--gate", "g1", "--metrics-urls", "http://a", "extra"},
		"unknown flag":    {"--gate", "g1", "--metrics-urls", "http://a", "--nope"},
		"negative hold":   {"--gate", "g1", "--metrics-urls", "http://a", "--hold", "-1s"},
		"bad metrics url": {"--gate", "g1", "--metrics-urls", "://"},
	} {
		_, err := parseRun(args)
		require.Error(t, err, name)
	}
}

func TestParseRun_NoAudioSaveIsAlwaysOn(t *testing.T) {
	_, err := parseRun([]string{"--gate", "g1", "--metrics-urls", "http://a", "--no-audio-save=false"})
	require.Error(t, err, "audio is never decoded or saved by the generator")
	_, err = parseRun([]string{"--gate", "g1", "--metrics-urls", "http://a", "--no-audio-save"})
	require.NoError(t, err)
}

// ---- execute ----

type fakeSource struct {
	mu    sync.Mutex
	calls int
	text  []string // snapshot i returns text[min(i, len-1)]
}

func (f *fakeSource) Snapshot(context.Context) (promtext.Samples, error) {
	f.mu.Lock()
	i := min(f.calls, len(f.text)-1)
	f.calls++
	f.mu.Unlock()
	return promtext.Parse(strings.NewReader(f.text[i]))
}

const turnBefore = `# TYPE kotoki_realtime_turn_end_to_first_audio_seconds histogram
kotoki_realtime_turn_end_to_first_audio_seconds_bucket{kind="reply",le="1.5"} 0
kotoki_realtime_turn_end_to_first_audio_seconds_bucket{kind="reply",le="2"} 0
kotoki_realtime_turn_end_to_first_audio_seconds_bucket{kind="reply",le="+Inf"} 0
kotoki_realtime_turn_end_to_first_audio_seconds_count{kind="reply"} 0
`

func turnAfter(under15, under2 int) string {
	return strings.NewReplacer("1.5\"} 0", "1.5\"} "+itoa(under15), "2\"} 0", "2\"} "+itoa(under2), "+Inf\"} 0", "+Inf\"} "+itoa(under2), "} 0\n", "} "+itoa(under2)+"\n").Replace(turnBefore)
}

// goodExtra supplies every observation the scrape cannot (K10-K12, K14, K15 and the rest of K13),
// all healthy, so that a run's verdict depends on what the scrape and the generator measured.
func goodExtra(context.Context) (gate.Observation, error) {
	return gate.Observation{
		"server_overhead_p95_s": 0.1, "bargein_stop_p95_s": 0.1, "client_bargein_p95_s": 0.5, "rest_p95_s": 0.1, "session_create_p95_s": 0.1,
		"ws_connect_ok_ratio": 1, "session_failed_server_error_ratio": 0, "resume_ok_ratio": 1,
		"slow_consumer_total": 0, "protocol_violations_total": 0,
		"realtime_cpu_ratio_max": 0.5, "realtime_mem_ratio_max": 0.5, "realtime_restarts": 0,
		"pg_acquire_wait_p99_s": 0.01, "pg_cpu_ratio": 0.3, "redis_cpu_ratio": 0.2, "redis_errors": 0,
		"outbox_lag_p99_s": 1, "dlq_depth": 0, "sessions_without_evaluation": 0,
		"verify_violations": 0, "soak_rss_drift": 0.01, "soak_goroutine_drift": 0.01,
	}, nil
}

type restAPI struct{ srv *httptest.Server }

func newRestAPI(t *testing.T) *restAPI {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"access_token": "t", "user": map[string]any{"id": "u"}})
	})
	mux.HandleFunc("GET /api/v1/topics", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"items": []any{}}) })
	mux.HandleFunc("GET /api/v1/scenarios", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"items": []map[string]any{{"id": uuid.NewString(), "topic_id": uuid.NewString()}}})
	})
	mux.HandleFunc("GET /api/v1/characters", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"items": []map[string]any{{"id": uuid.NewString()}}})
	})
	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 201, map[string]any{"id": uuid.NewString(), "status": "RESERVED"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &restAPI{srv: srv}
}

func testRunConfig(t *testing.T, src server.Source, extra func(context.Context) (gate.Observation, error)) (runConfig, string) {
	t.Helper()
	api := newRestAPI(t)
	conv := func(ctx context.Context, _ *kotokiclient.Client, _ kotokiclient.SessionSpec, steps []kotokiclient.Step, _ kotokiclient.Sink) (kotokiclient.Report, error) {
		select {
		case <-ctx.Done():
			return kotokiclient.Report{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		rep := kotokiclient.Report{ConnectMS: 10, BargeInStopMS: []int64{400}, TurnLatencyMS: []int64{900}}
		for _, st := range steps {
			if _, drop := st.(kotokiclient.Drop); drop {
				rep.ResumeMS = []int64{1500} // a well-behaved server: the dropped session comes back
			}
		}
		return rep, nil
	}
	spec := gate.Specs["g1"]
	spec.VUs, spec.Ramp, spec.Hold = 2, 50*time.Millisecond, 150*time.Millisecond
	agg := vu.NewAggregator()
	out := filepath.Join(t.TempDir(), "res", "g1.json")
	return runConfig{
		Spec: spec, Source: src, Extra: extra, Out: out,
		Runner: &vu.Runner{
			Users: []vu.Credential{{Email: "a@load.test", Password: "p"}, {Email: "b@load.test", Password: "p"}},
			Planner: scenario.NewPlanner(1, []scenario.Fixture{
				{Name: "ja_self_intro", DurationMS: 4000, Tags: []string{"speech"}},
			}),
			Conv: conv, Agg: agg, APIURL: api.srv.URL, Ramp: spec.Ramp, Hold: spec.Hold, Backoff: 10 * time.Millisecond,
			NewClient: func() *kotokiclient.Client {
				return kotokiclient.New(api.srv.URL, kotokiclient.WithHTTPClient(&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}))
			},
		},
		Instances: 1,
	}, out
}

type resultFile struct {
	Gate    string `json:"gate"`
	Pass    bool   `json:"pass"`
	Results []struct {
		ID      string   `json:"id"`
		Pass    bool     `json:"pass"`
		Skipped bool     `json:"skipped"`
		Value   *float64 `json:"value"`
		Reason  string   `json:"reason"`
	} `json:"results"`
	Observation map[string]float64 `json:"observation"`
	Client      vu.Summary         `json:"client"`
	Errors      []string           `json:"errors"`
	Contract    *struct {
		Pass    bool     `json:"pass"`
		Missing []string `json:"missing"`
		Error   string   `json:"error"`
	} `json:"metric_contract"`
}

func readResult(t *testing.T, p string) resultFile {
	t.Helper()
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	var rf resultFile
	require.NoError(t, json.Unmarshal(raw, &rf), "the result file is valid JSON (no NaN)")
	return rf
}

func TestRun_ExitNonZeroWhenGateFails(t *testing.T) {
	// 100 turns in the window, all between 1.5 s and 2 s: P95 about 1.98 s, over the 1.5 s limit
	src := &fakeSource{text: []string{turnBefore, turnAfter(0, 100)}}
	extra := func(ctx context.Context) (gate.Observation, error) {
		o, _ := goodExtra(ctx)
		return o, nil
	}
	cfg, out := testRunConfig(t, src, extra)
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, execute(context.Background(), cfg, &stdout, &stderr), stderr.String())

	rf := readResult(t, out)
	require.False(t, rf.Pass)
	require.Greater(t, rf.Observation["turn_p95_s"], 1.5)
	require.Equal(t, "K1", rf.Results[0].ID)
	require.False(t, rf.Results[0].Pass)
	for _, r := range rf.Results[1:] {
		require.True(t, r.Pass, "%s: %s", r.ID, r.Reason)
	}
	require.Contains(t, stdout.String(), "GATE g1: FAIL")
	require.Positive(t, rf.Client.Sessions, "the VUs really ran")
}

func TestRun_ExitZeroWhenEveryCriterionPasses(t *testing.T) {
	src := &fakeSource{text: []string{turnBefore, turnAfter(100, 100)}} // 100 turns under 1.5 s
	cfg, out := testRunConfig(t, src, goodExtra)
	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, execute(context.Background(), cfg, &stdout, &stderr), stdout.String()+stderr.String())
	rf := readResult(t, out)
	require.True(t, rf.Pass)
	require.Contains(t, stdout.String(), "GATE g1: PASS")
	require.Equal(t, 2, src.calls, "one scrape before the VUs start and one after they stop")
}

func TestRun_MissingDataFails(t *testing.T) {
	// no turn observed at all and nothing else supplied: K1 and others fail with "no data"
	src := &fakeSource{text: []string{turnBefore, turnBefore}}
	cfg, out := testRunConfig(t, src, nil)
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, execute(context.Background(), cfg, &stdout, &stderr))
	rf := readResult(t, out)
	require.False(t, rf.Pass)
	require.Equal(t, "no data", rf.Results[0].Reason)
	require.Nil(t, rf.Results[0].Value, "NaN is written as null")
}

func TestRun_ScrapeFailureAbortsBeforeTheRun(t *testing.T) {
	src := &errSource{}
	cfg, out := testRunConfig(t, src, goodExtra)
	var stdout, stderr bytes.Buffer
	require.Equal(t, 2, execute(context.Background(), cfg, &stdout, &stderr))
	require.Contains(t, stderr.String(), "baseline")
	_, err := os.Stat(out)
	require.True(t, os.IsNotExist(err))
	require.Zero(t, cfg.Runner.Agg.Summary().Sessions, "no VU was started")
}

type errSource struct{}

func (errSource) Snapshot(context.Context) (promtext.Samples, error) {
	return nil, context.DeadlineExceeded
}

func TestRun_ExtraErrorIsReportedAndFailsTheGate(t *testing.T) {
	src := &fakeSource{text: []string{turnBefore, turnAfter(100, 100)}}
	extra := func(context.Context) (gate.Observation, error) { return nil, os.ErrDeadlineExceeded }
	cfg, out := testRunConfig(t, src, extra)
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, execute(context.Background(), cfg, &stdout, &stderr))
	require.NotEmpty(t, readResult(t, out).Errors)
}

func TestRun_SamplesOutboxLagDuringTheRun(t *testing.T) {
	lag := `kotoki_outbox_oldest_pending_age_seconds{instance="a"} 2
kotoki_outbox_oldest_pending_age_seconds{instance="b"} 7
`
	src := &fakeSource{text: []string{turnBefore, lag}} // the first snapshot is the baseline, every later one shows the lag
	cfg, _ := testRunConfig(t, src, goodExtra)
	cfg.SampleEvery = 20 * time.Millisecond
	var stdout, stderr bytes.Buffer
	execute(context.Background(), cfg, &stdout, &stderr)
	samples := cfg.Runner.Agg.Snapshot().OutboxLagSamples
	require.NotEmpty(t, samples)
	for _, s := range samples {
		require.Equal(t, 7.0, s, "the worst pod is the sample")
	}
}

func TestRun_PromModeUsesPromAPIForServerKeys(t *testing.T) {
	cfg, out := testRunConfig(t, nil, goodExtra)
	cfg.Prom = fixedQuerier(1.0)
	var stdout, stderr bytes.Buffer
	rc := execute(context.Background(), cfg, &stdout, &stderr)
	require.Equal(t, 0, rc, stdout.String()+stderr.String())
	rf := readResult(t, out)
	require.Equal(t, 1.0, rf.Observation["turn_p95_s"], "from Prometheus")
	require.InDelta(t, 0.4, rf.Observation["client_bargein_p95_s"], 0.005, "the generator's own measurement wins for the client-side key")
}

type fixedQuerier float64

func (q fixedQuerier) Query(context.Context, string) (float64, error) { return float64(q), nil }

// ---- build and dispatch ----

func TestBuild_WiresRunnerFromOptions(t *testing.T) {
	users := filepath.Join(t.TempDir(), "users.csv")
	var sb strings.Builder
	sb.WriteString("email,password\n")
	for i := 1; i <= 7; i++ {
		sb.WriteString("load-" + itoa(i) + "@load.test,pw" + itoa(i) + "\n")
	}
	require.NoError(t, os.WriteFile(users, []byte(sb.String()), 0o600))

	o, err := parseRun([]string{"--gate", "g1-local", "--vus", "7", "--users-file", users, "--fixtures", filepath.Join("..", "..", "..", "..", "testdata", "audio"),
		"--metrics-urls", "http://localhost:9090", "--instances", "2", "--instance-index", "1", "--api-url", "http://api", "--ws-url", "ws://rt", "--origin", "https://o",
		"--out", "o.json", "--seed", "5"})
	require.NoError(t, err)
	var stderr bytes.Buffer
	cfg, err := build(o, &stderr)
	require.NoError(t, err, stderr.String())
	require.Empty(t, stderr.String(), "no warning: ja_barge_in is in the fixtures")

	r := cfg.Runner
	require.Equal(t, 3, r.VUs, "7 VUs over 2 instances: instance 1 has 3")
	require.Equal(t, "load-5@load.test", r.Users[0].Email, "instance 0 has 4 users, instance 1 the next ones")
	require.Len(t, r.Users, 3)
	require.Equal(t, "ws://rt", r.WSURL)
	require.Equal(t, "https://o", r.Origin)
	require.Equal(t, "http://api", r.APIURL)
	require.Equal(t, time.Minute, r.Ramp)
	require.Equal(t, 5*time.Minute, r.Hold)
	require.Contains(t, r.Pre, "ja_barge_in")
	require.NotEmpty(t, r.Pre["ja_self_intro"], "pre-encoded packets")
	require.NotContains(t, r.Pre, "ja_long_utterance", "only the fixtures the planner can pick are encoded")
	require.NotNil(t, cfg.Source)
	require.Nil(t, cfg.Prom)
	require.Equal(t, 1, cfg.Instance)

	o.Fixtures = t.TempDir()
	_, err = build(o, &stderr)
	require.Error(t, err)
	o.Fixtures = filepath.Join("..", "..", "..", "..", "testdata", "audio")
	o.UsersFile = filepath.Join(t.TempDir(), "missing.csv")
	_, err = build(o, &stderr)
	require.Error(t, err)
}

func TestBuild_PromURLSelectsPromAPI(t *testing.T) {
	users := filepath.Join(t.TempDir(), "users.csv")
	require.NoError(t, os.WriteFile(users, []byte("a@load.test,pw\n"), 0o600))
	o, err := parseRun([]string{"--gate", "real", "--vus", "1", "--users-file", users, "--fixtures", filepath.Join("..", "..", "..", "..", "testdata", "audio"), "--prom-url", "http://prom:9090"})
	require.NoError(t, err)
	cfg, err := build(o, io.Discard)
	require.NoError(t, err)
	require.Nil(t, cfg.Source)
	require.Equal(t, server.PromAPI{BaseURL: "http://prom:9090"}, cfg.Prom)
}

func TestRealMain_Dispatch(t *testing.T) {
	var out, errb bytes.Buffer
	require.Equal(t, 2, realMain(context.Background(), nil, &out, &errb))
	require.Contains(t, errb.String(), "usage")
	errb.Reset()
	require.Equal(t, 2, realMain(context.Background(), []string{"bogus"}, &out, &errb))
	require.Contains(t, errb.String(), "unknown command")
	require.Equal(t, 0, realMain(context.Background(), []string{"help"}, &out, &errb))
	errb.Reset()
	require.Equal(t, 2, realMain(context.Background(), []string{"run", "--gate", "g1"}, &out, &errb))
	require.Contains(t, errb.String(), "server data")
}

func TestMetricsURL_RejectsNonHTTP(t *testing.T) {
	_, err := metricsURL("ftp://x")
	require.Error(t, err)
	_, err = metricsURL("localhost:9090")
	require.Error(t, err)
}

// ---- --verify-db ----

func TestVerifyExtra_MapsViolationsToK13K14(t *testing.T) {
	var got verify.Options
	defer func(old func(context.Context, string, verify.Options) ([]verify.Violation, error)) { runVerify = old }(runVerify)
	var vs []verify.Violation
	runVerify = func(_ context.Context, dsn string, o verify.Options) ([]verify.Violation, error) {
		require.Equal(t, "postgres://u:pw@h/db", dsn)
		got = o
		return vs, nil
	}
	since := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

	obs, err := verifyExtra("postgres://u:pw@h/db", since, 5*time.Minute)(context.Background())
	require.NoError(t, err)
	require.Equal(t, gate.Observation{"verify_violations": 0, "sessions_without_evaluation": 0}, obs, "clean: zeros are measured, not absent")
	require.True(t, got.Since.Equal(since))
	require.Equal(t, 5*time.Minute, got.AwaitEvaluations)

	vs = []verify.Violation{{Check: "V1"}, {Check: "V3"}, {Check: "V8"}}
	obs, err = verifyExtra("postgres://u:pw@h/db", since, time.Minute)(context.Background())
	require.NoError(t, err)
	require.Equal(t, gate.Observation{"verify_violations": 2, "sessions_without_evaluation": 1}, obs)

	vs = []verify.Violation{{Check: "V1"}}
	obs, _ = verifyExtra("postgres://u:pw@h/db", since, 0)(context.Background())
	require.Equal(t, gate.Observation{"verify_violations": 1}, obs, "V8 was not run: the key stays absent so K13 fails with no data")
}

func TestVerifyExtra_ErrorNeverCarriesTheDSN(t *testing.T) {
	defer func(old func(context.Context, string, verify.Options) ([]verify.Violation, error)) { runVerify = old }(runVerify)
	runVerify = func(_ context.Context, dsn string, _ verify.Options) ([]verify.Violation, error) {
		return nil, errors.New("dial " + dsn + " refused")
	}
	_, err := verifyExtra("postgres://u:s3cret@h/db", time.Now(), time.Minute)(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret")
}

func TestParseRun_VerifyFlags(t *testing.T) {
	o, err := parseRun([]string{"--gate", "g1", "--metrics-urls", "http://a", "--verify-db", "postgres://x/y"})
	require.NoError(t, err)
	require.Equal(t, "postgres://x/y", o.VerifyDB)
	require.Equal(t, 5*time.Minute, o.AwaitEvaluations)
	_, err = parseRun([]string{"--gate", "g1", "--metrics-urls", "http://a", "--await-evaluations", "-1s"})
	require.Error(t, err)
}

func TestBuild_VerifyDBSetsExtra(t *testing.T) {
	users := filepath.Join(t.TempDir(), "users.csv")
	require.NoError(t, os.WriteFile(users, []byte("a@load.test,pw\n"), 0o600))
	args := []string{"--gate", "real", "--vus", "1", "--users-file", users, "--fixtures", filepath.Join("..", "..", "..", "..", "testdata", "audio"), "--prom-url", "http://p"}
	o, err := parseRun(args)
	require.NoError(t, err)
	cfg, err := build(o, io.Discard)
	require.NoError(t, err)
	require.Nil(t, cfg.Extra)
	o, err = parseRun(append(args, "--verify-db", "postgres://x/y"))
	require.NoError(t, err)
	cfg, err = build(o, io.Discard)
	require.NoError(t, err)
	require.NotNil(t, cfg.Extra)
}

// ---- metric contract ----

func contractFile(t *testing.T, names ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "metrics.txt")
	require.NoError(t, os.WriteFile(p, []byte(strings.Join(names, "\n")+"\n"), 0o600))
	return p
}

func TestRun_MetricContractFailureFailsAnOtherwiseGreenGate(t *testing.T) {
	src := &fakeSource{text: []string{turnBefore, turnAfter(100, 100)}} // every K passes (see ExitZero test)
	cfg, out := testRunConfig(t, src, goodExtra)
	cfg.MetricsFile = contractFile(t, "kotoki_realtime_turn_end_to_first_audio_seconds", "kotoki_does_not_exist", "?kotoki_optional_total")
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, execute(context.Background(), cfg, &stdout, &stderr), stdout.String()+stderr.String())

	rf := readResult(t, out)
	require.False(t, rf.Pass)
	for _, r := range rf.Results {
		require.True(t, r.Pass, "%s: the K criteria themselves are green", r.ID)
	}
	require.NotNil(t, rf.Contract)
	require.False(t, rf.Contract.Pass)
	require.Equal(t, []string{"kotoki_does_not_exist"}, rf.Contract.Missing)
	require.Contains(t, stdout.String(), "metric-contract")
	require.Contains(t, stdout.String(), "GATE g1: FAIL")
}

func TestRun_MetricContractPasses(t *testing.T) {
	src := &fakeSource{text: []string{turnBefore, turnAfter(100, 100)}}
	cfg, out := testRunConfig(t, src, goodExtra)
	cfg.MetricsFile = contractFile(t, "kotoki_realtime_turn_end_to_first_audio_seconds")
	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, execute(context.Background(), cfg, &stdout, &stderr), stdout.String()+stderr.String())
	rf := readResult(t, out)
	require.True(t, rf.Pass)
	require.NotNil(t, rf.Contract)
	require.True(t, rf.Contract.Pass)
	require.Empty(t, rf.Contract.Missing)
}

func TestRun_NoMetricContractWithoutTheFlag(t *testing.T) {
	src := &fakeSource{text: []string{turnBefore, turnAfter(100, 100)}}
	cfg, out := testRunConfig(t, src, goodExtra)
	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, execute(context.Background(), cfg, &stdout, &stderr))
	require.Nil(t, readResult(t, out).Contract)
	require.NotContains(t, stdout.String(), "metric-contract")
}

func TestRun_UnreadableMetricContractFileFailsTheGate(t *testing.T) {
	src := &fakeSource{text: []string{turnBefore, turnAfter(100, 100)}}
	cfg, out := testRunConfig(t, src, goodExtra)
	cfg.MetricsFile = filepath.Join(t.TempDir(), "missing.txt")
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, execute(context.Background(), cfg, &stdout, &stderr))
	rf := readResult(t, out)
	require.NotNil(t, rf.Contract)
	require.False(t, rf.Contract.Pass)
	require.NotEmpty(t, rf.Contract.Error)
}

func TestParseRun_MetricsFile(t *testing.T) {
	o, err := parseRun([]string{"--gate", "g1-local", "--metrics-urls", "http://a", "--metrics-file", "m.txt"})
	require.NoError(t, err)
	require.Equal(t, "m.txt", o.MetricsFile)
	_, err = parseRun([]string{"--gate", "g1-local", "--prom-url", "http://p", "--metrics-file", "m.txt"})
	require.ErrorContains(t, err, "metrics-file", "the contract reads a scrape: it needs --metrics-urls")
}

func TestParseRun_SpreadIPs(t *testing.T) {
	o, err := parseRun([]string{"--gate", "g1-local", "--metrics-urls", "http://a", "--spread-ips"})
	require.NoError(t, err)
	require.True(t, o.SpreadIPs)
	o, err = parseRun([]string{"--gate", "g1-local", "--metrics-urls", "http://a"})
	require.NoError(t, err)
	require.False(t, o.SpreadIPs, "off unless asked")
}

func TestBuild_SpreadIPsStartsAtTheInstancesFirstUser(t *testing.T) {
	users := filepath.Join(t.TempDir(), "users.csv")
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString("load-" + itoa(i) + "@load.test,pw\n")
	}
	require.NoError(t, os.WriteFile(users, []byte(sb.String()), 0o600))
	base := []string{"--gate", "g1-local", "--vus", "10", "--users-file", users, "--fixtures", filepath.Join("..", "..", "..", "..", "testdata", "audio"),
		"--metrics-urls", "http://localhost:9090", "--instances", "3", "--spread-ips"}
	for idx, want := range map[int]int{0: 0, 1: 4, 2: 7} { // 10 VUs over 3 instances: 4, 3, 3
		o, err := parseRun(append(append([]string{}, base...), "--instance-index", itoa(idx)))
		require.NoError(t, err)
		cfg, err := build(o, io.Discard)
		require.NoError(t, err)
		require.True(t, cfg.Runner.SpreadIPs)
		require.Equal(t, want, cfg.Runner.IPBase, "instance %d: its users' addresses never collide with another instance's", idx)
	}
}

// ---- seed-users: rate limits ----

func TestSeedUsers_RetriesRegisterWhileRateLimited(t *testing.T) {
	var mu sync.Mutex
	attempts := map[string]int{}
	api := newSeedAPI(t, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/register" {
				var in kotokiclient.RegisterRequest
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &in)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				mu.Lock()
				attempts[in.Email]++
				n := attempts[in.Email]
				mu.Unlock()
				if n <= 2 {
					w.Header().Set("Retry-After", "3")
					writeJSON(w, 429, map[string]any{"error": map[string]any{"code": "RATE_LIMITED", "message": "slow", "request_id": "q"}})
					return
				}
			}
			inner.ServeHTTP(w, r)
		})
	})
	var slept []time.Duration
	var smu sync.Mutex
	o := seedOptions{APIURL: api.srv.URL, Count: 2, Balance: 100, AdminEmail: "admin@kotoki.test", AdminPassword: "adminpw", Concurrency: 1,
		Out: filepath.Join(t.TempDir(), "users.csv"),
		sleep: func(_ context.Context, d time.Duration) error {
			smu.Lock()
			slept = append(slept, d)
			smu.Unlock()
			return nil
		}}
	require.NoError(t, seedUsers(context.Background(), o, func(ip string) *kotokiclient.Client { return kotokiclient.New(o.APIURL, kotokiclient.WithClientIP(ip)) }, io.Discard))
	require.Equal(t, map[string]int{"load-1@load.test": 3, "load-2@load.test": 3}, attempts)
	require.Equal(t, []time.Duration{3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second}, slept, "waits Retry-After before each retry")
	require.Len(t, api.grants, 2)
}

func TestSeedUsers_GivesUpWhenRateLimitedForGood(t *testing.T) {
	var calls int
	var mu sync.Mutex
	api := newSeedAPI(t, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/register" {
				mu.Lock()
				calls++
				mu.Unlock()
				writeJSON(w, 429, map[string]any{"error": map[string]any{"code": "RATE_LIMITED", "message": "slow", "request_id": "q"}})
				return
			}
			inner.ServeHTTP(w, r)
		})
	})
	o := seedOptions{APIURL: api.srv.URL, Count: 1, Balance: 100, AdminEmail: "admin@kotoki.test", AdminPassword: "adminpw",
		Out: filepath.Join(t.TempDir(), "users.csv"), sleep: func(context.Context, time.Duration) error { return nil }}
	err := seedUsers(context.Background(), o, func(string) *kotokiclient.Client { return kotokiclient.New(o.APIURL) }, io.Discard)
	require.ErrorContains(t, err, "register load-1@load.test")
	require.Equal(t, maxSeedAttempts, calls, "bounded retries, no endless loop")
	_, statErr := os.Stat(o.Out)
	require.True(t, os.IsNotExist(statErr))
}

func TestSeedUsers_SpreadIPsGivesEachUserItsOwnAddress(t *testing.T) {
	xff := map[string]string{}
	var mu sync.Mutex
	api := newSeedAPI(t, func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/register" {
				var in kotokiclient.RegisterRequest
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &in)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				mu.Lock()
				xff[in.Email] = r.Header.Get("X-Forwarded-For")
				mu.Unlock()
			}
			inner.ServeHTTP(w, r)
		})
	})
	out := filepath.Join(t.TempDir(), "users.csv")
	var stderr bytes.Buffer
	rc := realMain(context.Background(), []string{"seed-users", "--api-url", api.srv.URL, "--count", "3", "--admin-email", "admin@kotoki.test",
		"--admin-password", "adminpw", "--spread-ips", "--out", out}, io.Discard, &stderr)
	require.Equal(t, 0, rc, stderr.String())
	require.Equal(t, map[string]string{"load-1@load.test": vu.SpreadIP(0), "load-2@load.test": vu.SpreadIP(1), "load-3@load.test": vu.SpreadIP(2)}, xff,
		"user n registers from SpreadIP(n-1), the address kotoki-load run gives that VU")
}

func TestSeedUsers_AdminPasswordFromEnvNeverEchoedInHelp(t *testing.T) {
	t.Setenv("KOTOKI_ADMIN_EMAIL", "admin@kotoki.test")
	t.Setenv("KOTOKI_ADMIN_PASSWORD", "s3cret-from-env")
	var stdout, stderr bytes.Buffer
	realMain(context.Background(), []string{"seed-users", "-h"}, &stdout, &stderr)
	require.NotContains(t, stdout.String()+stderr.String(), "s3cret-from-env", "flag defaults are printed by -h: the env password must not be one")
	require.Contains(t, stderr.String(), "KOTOKI_ADMIN_PASSWORD")

	api := newSeedAPI(t)
	out := filepath.Join(t.TempDir(), "users.csv")
	t.Setenv("KOTOKI_ADMIN_PASSWORD", "adminpw")
	stderr.Reset()
	rc := realMain(context.Background(), []string{"seed-users", "--api-url", api.srv.URL, "--count", "1", "--out", out}, &stdout, &stderr)
	require.Equal(t, 0, rc, stderr.String())

	// the flag wins over the environment
	t.Setenv("KOTOKI_ADMIN_PASSWORD", "wrong")
	rc = realMain(context.Background(), []string{"seed-users", "--api-url", api.srv.URL, "--count", "1", "--admin-password", "adminpw", "--out", out}, &stdout, &stderr)
	require.Equal(t, 0, rc, stderr.String())
}

// ---- capacity ----

func TestCapacity_PrintsTable(t *testing.T) {
	var out, errb bytes.Buffer
	require.Equal(t, 0, realMain(context.Background(), []string{"capacity", "--ccu", "1000"}, &out, &errb), errb.String())
	require.Regexp(t, `(?m)^realtime pods\s+15\b`, out.String())
	require.Regexp(t, `(?m)^PG connections \(pool budget\)\s+187\b`, out.String())
	require.NotContains(t, out.String(), "measured")
}

func TestCapacity_RejectsBadCCU(t *testing.T) {
	for _, ccu := range []string{"0", "-5", "x"} {
		var out, errb bytes.Buffer
		require.Equal(t, 2, realMain(context.Background(), []string{"capacity", "--ccu", ccu}, &out, &errb), ccu)
		require.Empty(t, out.String())
	}
	var out, errb bytes.Buffer
	require.Equal(t, 2, realMain(context.Background(), []string{"capacity"}, &out, &errb), "ccu is required")
	require.Equal(t, 2, realMain(context.Background(), []string{"capacity", "--ccu", "10", "extra"}, &out, &errb))
}

func TestCapacity_MeasuredExitCode(t *testing.T) {
	dir := t.TempDir()
	write := func(cpu string) string {
		p := filepath.Join(dir, "m.json")
		body := `{"cpu_cores_per_100_sessions":` + cpu + `,"mem_mb_per_100_sessions":280,"stt_egress_mbps":340,"pg_tx_per_sec":140,"redis_ops_per_sec":3600}`
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	var out, errb bytes.Buffer
	require.Equal(t, 0, realMain(context.Background(), []string{"capacity", "--ccu", "1000", "--measured", write("1.2")}, &out, &errb), errb.String())
	require.Contains(t, out.String(), "OK")
	require.NotContains(t, out.String(), "FAIL")

	out.Reset()
	require.Equal(t, 1, realMain(context.Background(), []string{"capacity", "--ccu", "1000", "--measured", write("1.6")}, &out, &errb))
	require.Contains(t, out.String(), "FAIL")

	errb.Reset()
	require.Equal(t, 2, realMain(context.Background(), []string{"capacity", "--ccu", "1000", "--measured", filepath.Join(dir, "none.json")}, &out, &errb))
	require.NotEmpty(t, errb.String())
}
