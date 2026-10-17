//go:build !windows

package vu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/test/load/scenario"
)

// fakeAPI is the REST side of a load run: login, catalog, balance and session creation.
type fakeAPI struct {
	srv                               *httptest.Server
	logins, creates, balances, topics atomic.Int64
	create401Once                     atomic.Bool
	mu                                sync.Mutex
	created                           []kotokiclient.CreateSessionRequest
	emails                            []string
	xffs                              map[string]string // email -> X-Forwarded-For of its login
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.logins.Add(1)
		var in struct{ Email, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.emails = append(f.emails, in.Email)
		if f.xffs == nil {
			f.xffs = map[string]string{}
		}
		f.xffs[in.Email] = r.Header.Get("X-Forwarded-For")
		f.mu.Unlock()
		write(w, 200, map[string]any{"access_token": "tok-" + in.Email, "user": map[string]any{"id": "u"}})
	})
	mux.HandleFunc("GET /api/v1/topics", func(w http.ResponseWriter, _ *http.Request) {
		f.topics.Add(1)
		write(w, 200, map[string]any{"items": []map[string]any{{"id": "01929a40-0000-7000-8000-000000000001", "code": "t"}}})
	})
	mux.HandleFunc("GET /api/v1/scenarios", func(w http.ResponseWriter, _ *http.Request) {
		write(w, 200, map[string]any{"items": []map[string]any{{"id": "01929a40-0000-7000-8000-000000000002", "topic_id": "01929a40-0000-7000-8000-000000000001"}}})
	})
	mux.HandleFunc("GET /api/v1/characters", func(w http.ResponseWriter, _ *http.Request) {
		write(w, 200, map[string]any{"items": []map[string]any{{"id": "01929a40-0000-7000-8000-000000000003", "code": "c"}}})
	})
	mux.HandleFunc("GET /api/v1/balance", func(w http.ResponseWriter, _ *http.Request) {
		f.balances.Add(1)
		write(w, 200, map[string]any{"available_seconds": 36000})
	})
	mux.HandleFunc("POST /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if f.create401Once.CompareAndSwap(true, false) {
			write(w, 401, map[string]any{"error": map[string]any{"code": "TOKEN_EXPIRED", "message": "expired", "request_id": "q"}})
			return
		}
		f.creates.Add(1)
		var in kotokiclient.CreateSessionRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.created = append(f.created, in)
		f.mu.Unlock()
		write(w, 201, map[string]any{"id": uuid.NewString(), "status": "RESERVED", "conversation_start_mode": in.ConversationStartMode, "duration_seconds": in.DurationSeconds})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newClientNoKeepAlive(url string) func() *kotokiclient.Client {
	return func() *kotokiclient.Client {
		return kotokiclient.New(url, kotokiclient.WithHTTPClient(&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}))
	}
}

func testFixtures() []scenario.Fixture {
	return []scenario.Fixture{
		{Name: "ja_self_intro", DurationMS: 4000, Tags: []string{"speech"}},
		{Name: "ja_filler_pause", DurationMS: 4000, Tags: []string{"speech", "filler"}},
		{Name: "ja_barge_in", DurationMS: 3000, Tags: []string{"speech", "barge-in"}},
	}
}

func users(n int) []Credential {
	out := make([]Credential, n)
	for i := range out {
		out[i] = Credential{Email: "load-" + string(rune('a'+i)) + "@load.test", Password: "pw"}
	}
	return out
}

func okConv(report kotokiclient.Report) (Conversation, *atomic.Int64) {
	var n atomic.Int64
	return func(ctx context.Context, _ *kotokiclient.Client, _ kotokiclient.SessionSpec, _ []kotokiclient.Step, _ kotokiclient.Sink) (kotokiclient.Report, error) {
		select {
		case <-ctx.Done():
			return kotokiclient.Report{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		n.Add(1) // counts finished conversations only
		return report, nil
	}, &n
}

func TestRunner_NewSessionAfterEach(t *testing.T) {
	api := newFakeAPI(t)
	conv, convs := okConv(kotokiclient.Report{ConnectMS: 12, Status: "COMPLETED", TurnLatencyMS: []int64{800}})
	agg := NewAggregator()
	r := &Runner{
		Users: users(3), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: agg,
		APIURL: api.srv.URL, NewClient: newClientNoKeepAlive(api.srv.URL), Backoff: 10 * time.Millisecond,
	}
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Run(ctx))
	require.GreaterOrEqual(t, convs.Load(), int64(3), "every VU starts a new session after the previous one")
	require.GreaterOrEqual(t, api.creates.Load(), int64(3))
	require.Equal(t, int64(3), api.logins.Load(), "one login per VU, not per session")
	require.ElementsMatch(t, []string{"load-a@load.test", "load-b@load.test", "load-c@load.test"}, api.emails)
	// A conversation finishing just as the run ends is counted by Conv but, being cut by the end, not
	// recorded: at most one per VU may differ.
	recorded, finished := agg.Summary().Sessions, int(convs.Load())
	require.LessOrEqual(t, recorded, finished)
	require.GreaterOrEqual(t, recorded, finished-3)

	api.srv.CloseClientConnections()
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 }, 2*time.Second, 20*time.Millisecond, "no goroutine left behind")
}

func TestRunner_CancelReturnsFast(t *testing.T) {
	api := newFakeAPI(t)
	blocked := make(chan struct{}, 8)
	conv := func(ctx context.Context, _ *kotokiclient.Client, _ kotokiclient.SessionSpec, _ []kotokiclient.Step, _ kotokiclient.Sink) (kotokiclient.Report, error) {
		blocked <- struct{}{}
		<-ctx.Done()
		return kotokiclient.Report{}, ctx.Err()
	}
	agg := NewAggregator()
	r := &Runner{Users: users(2), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: agg,
		APIURL: api.srv.URL, NewClient: newClientNoKeepAlive(api.srv.URL), Backoff: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
			t.Fatal("conversations did not start")
		}
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	require.Less(t, time.Since(start), 100*time.Millisecond)
	require.Zero(t, agg.Summary().Sessions, "a conversation cut by the end of the run is not a result")
}

func TestRunner_RampSpacing(t *testing.T) {
	api := newFakeAPI(t)
	fake := clock.NewFake(time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	starts := map[int]time.Time{}
	conv := func(ctx context.Context, _ *kotokiclient.Client, _ kotokiclient.SessionSpec, _ []kotokiclient.Step, _ kotokiclient.Sink) (kotokiclient.Report, error) {
		<-ctx.Done()
		return kotokiclient.Report{}, ctx.Err()
	}
	r := &Runner{
		Users: users(10), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: NewAggregator(),
		APIURL: api.srv.URL, NewClient: newClientNoKeepAlive(api.srv.URL), Ramp: time.Second,
		Now:   fake.Now,
		Sleep: func(ctx context.Context, d time.Duration) error { fake.Advance(d); return ctx.Err() },
		OnVUStart: func(idx int, at time.Time) {
			mu.Lock()
			defer mu.Unlock()
			starts[idx] = at
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(starts) == 10 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	t0 := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	for k := 0; k < 10; k++ {
		require.WithinDuration(t, t0.Add(time.Duration(k)*100*time.Millisecond), starts[k], 30*time.Millisecond, "VU %d", k)
	}
}

func TestRunner_ErrorCounted(t *testing.T) {
	api := newFakeAPI(t)
	var n atomic.Int64
	conv := func(_ context.Context, _ *kotokiclient.Client, _ kotokiclient.SessionSpec, _ []kotokiclient.Step, _ kotokiclient.Sink) (kotokiclient.Report, error) {
		n.Add(1)
		return kotokiclient.Report{}, &kotokiclient.ServerError{Code: "INTERNAL", Message: "boom"}
	}
	agg := NewAggregator()
	r := &Runner{Users: users(1), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: agg,
		APIURL: api.srv.URL, NewClient: newClientNoKeepAlive(api.srv.URL), Backoff: 30 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Run(ctx))
	got := n.Load()
	require.GreaterOrEqual(t, got, int64(3), "the loop goes on after an error")
	require.LessOrEqual(t, got, int64(10), "with a backoff between attempts, not a hot loop")
	ca := agg.Snapshot()
	require.Equal(t, int(got), ca.ConnectTotal)
	require.Zero(t, ca.ConnectOK)
	require.Equal(t, int(got), agg.Summary().SessionErrors)
}

func TestRunner_BackgroundREST(t *testing.T) {
	api := newFakeAPI(t)
	conv, _ := okConv(kotokiclient.Report{ConnectMS: 1})
	r := &Runner{Users: users(1), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: NewAggregator(),
		APIURL: api.srv.URL, NewClient: newClientNoKeepAlive(api.srv.URL), BackgroundRESTEvery: 20 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Run(ctx))
	require.GreaterOrEqual(t, api.balances.Load(), int64(3))
	require.GreaterOrEqual(t, api.topics.Load(), int64(4), "one catalog at start plus one per tick")
}

func TestRunner_LogsInAgainOn401(t *testing.T) {
	api := newFakeAPI(t)
	api.create401Once.Store(true)
	conv, convs := okConv(kotokiclient.Report{ConnectMS: 1})
	r := &Runner{Users: users(1), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: NewAggregator(),
		APIURL: api.srv.URL, NewClient: newClientNoKeepAlive(api.srv.URL), Backoff: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Run(ctx))
	require.GreaterOrEqual(t, convs.Load(), int64(1))
	require.Equal(t, int64(2), api.logins.Load(), "an expired access token is replaced by a new login, once")
}

func TestRunner_SessionRequestComesFromThePlan(t *testing.T) {
	api := newFakeAPI(t)
	var spec kotokiclient.SessionSpec
	var steps []kotokiclient.Step
	var once sync.Once
	conv := func(ctx context.Context, _ *kotokiclient.Client, s kotokiclient.SessionSpec, st []kotokiclient.Step, _ kotokiclient.Sink) (kotokiclient.Report, error) {
		once.Do(func() { spec, steps = s, st })
		<-ctx.Done()
		return kotokiclient.Report{}, ctx.Err()
	}
	pre := map[string][][]byte{"ja_self_intro": {{9}}, "ja_filler_pause": {{8}}, "ja_barge_in": {{7}}}
	r := &Runner{Users: users(1), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: NewAggregator(),
		APIURL: api.srv.URL, WSURL: "ws://rt/api/v1/realtime", Origin: "https://app.test", Pre: pre,
		NewClient: newClientNoKeepAlive(api.srv.URL)}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Run(ctx))
	require.NotEqual(t, uuid.Nil, spec.SessionID)
	require.Equal(t, "ws://rt/api/v1/realtime", spec.WSURL)
	require.Equal(t, "https://app.test", spec.Origin)
	require.NotEmpty(t, steps)
	api.mu.Lock()
	defer api.mu.Unlock()
	require.NotEmpty(t, api.created)
	c := api.created[0]
	require.Contains(t, []int{300, 600, 900}, c.DurationSeconds)
	require.Contains(t, []string{"USER_FIRST", "AI_FIRST"}, c.ConversationStartMode)
	require.Equal(t, uuid.MustParse("01929a40-0000-7000-8000-000000000002"), c.ScenarioID)
	require.Equal(t, uuid.MustParse("01929a40-0000-7000-8000-000000000001"), c.TopicID)
	require.Equal(t, uuid.MustParse("01929a40-0000-7000-8000-000000000003"), c.CharacterID)
}

func TestRunner_Validation(t *testing.T) {
	conv, _ := okConv(kotokiclient.Report{})
	base := Runner{Users: users(2), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: NewAggregator(), APIURL: "http://x"}
	r := base
	r.VUs = 3
	require.ErrorContains(t, r.Run(context.Background()), "users")
	r = base
	r.Users = nil
	require.Error(t, r.Run(context.Background()))
	r = base
	r.Conv = nil
	require.Error(t, r.Run(context.Background()))
	r = base
	r.Agg = nil
	require.Error(t, r.Run(context.Background()))
	r = base
	r.Planner = nil
	require.Error(t, r.Run(context.Background()))
}

func TestSamplesFromReport(t *testing.T) {
	s := samplesFromReport(kotokiclient.Report{
		ConnectMS: 40, TurnLatencyMS: []int64{900, 1100}, BargeInStopMS: []int64{300}, ResumeMS: []int64{1500},
	})
	require.Equal(t, Samples{ConnectMS: 40, TurnLatencyMS: []int64{900, 1100}, BargeInStopMS: []int64{300}, ResumeMS: []int64{1500}}, s)
}

func TestAggregator_Counts(t *testing.T) {
	a := NewAggregator()
	// a session with a drop that resumed, one that connected but failed later, one that never connected
	a.Add(Samples{ConnectMS: 30, TurnLatencyMS: []int64{1000, 2000}, BargeInStopMS: []int64{400, 600}, ResumeMS: []int64{2000}}, 1, nil)
	a.Add(Samples{ConnectMS: 30}, 1, kotokiclient.ErrAITimeout) // drop planned, never resumed
	a.Add(Samples{}, 0, &kotokiclient.ServerError{Code: "X"})
	a.Add(Samples{ConnectMS: 20}, 0, nil)

	// more resumes than drops (a plan with no drop, a stray resume) never count above what was expected
	a.Add(Samples{ConnectMS: 30, ResumeMS: []int64{1000, 1000}}, 1, nil)
	c := a.Snapshot()
	require.Equal(t, 5, c.ConnectTotal)
	require.Equal(t, 4, c.ConnectOK)
	require.Equal(t, 3, c.ResumeTotal)
	require.Equal(t, 2, c.ResumeOK)
	require.Equal(t, 2, c.ClientBargeIn.Count())
	require.InDelta(t, 0.6, c.ClientBargeIn.Quantile(0.95), 0.01, "barge-in histogram holds seconds")
	require.Equal(t, 3, c.Resume.Count())

	sum := a.Summary()
	require.Equal(t, 5, sum.Sessions)
	require.Equal(t, 2, sum.SessionErrors)
	require.InDelta(t, 2.0, sum.TurnP95S, 0.05)

	// Snapshot is a copy: later samples do not change it
	a.Add(Samples{ConnectMS: 1, BargeInStopMS: []int64{100}}, 0, nil)
	require.Equal(t, 2, c.ClientBargeIn.Count())
	require.Equal(t, 3, a.Snapshot().ClientBargeIn.Count())
}

func TestAggregator_OutboxLag(t *testing.T) {
	a := NewAggregator()
	a.AddOutboxLag(0.5)
	a.AddOutboxLag(1.5)
	require.Equal(t, []float64{0.5, 1.5}, a.Snapshot().OutboxLagSamples)
}

func TestSpreadIP_DistinctValidAddresses(t *testing.T) {
	seen := map[string]bool{}
	for n := 0; n < 70000; n++ {
		ip := SpreadIP(n)
		a, err := netip.ParseAddr(ip)
		require.NoError(t, err, ip)
		require.True(t, a.Is4() && netip.MustParsePrefix("10.0.0.0/8").Contains(a), ip)
		require.False(t, seen[ip], "address %s repeated at %d", ip, n)
		seen[ip] = true
	}
}

func TestRunner_SpreadIPsGivesEveryVUItsOwnAddress(t *testing.T) {
	api := newFakeAPI(t)
	conv, _ := okConv(kotokiclient.Report{Status: "COMPLETED"})
	var mu sync.Mutex
	byClient := map[string]bool{}
	wrapped := func(ctx context.Context, c *kotokiclient.Client, s kotokiclient.SessionSpec, st []kotokiclient.Step, k kotokiclient.Sink) (kotokiclient.Report, error) {
		mu.Lock()
		byClient[c.ClientIP()] = true
		mu.Unlock()
		return conv(ctx, c, s, st, k)
	}
	r := &Runner{
		Users: users(3), Planner: scenario.NewPlanner(1, testFixtures()), Conv: wrapped, Agg: NewAggregator(),
		Hold: 150 * time.Millisecond, APIURL: api.srv.URL, SpreadIPs: true, IPBase: 100, Pre: map[string][][]byte{},
	}
	require.NoError(t, r.Run(context.Background()))
	require.Equal(t, map[string]string{
		"load-a@load.test": SpreadIP(100), "load-b@load.test": SpreadIP(101), "load-c@load.test": SpreadIP(102),
	}, api.xffs, "the REST calls of VU i come from SpreadIP(IPBase+i)")
	require.Len(t, byClient, 3)
	require.NotContains(t, byClient, "")
}

func TestRunner_NoSpreadIPsSendsNoForwardedFor(t *testing.T) {
	api := newFakeAPI(t)
	conv, _ := okConv(kotokiclient.Report{Status: "COMPLETED"})
	r := &Runner{
		Users: users(1), Planner: scenario.NewPlanner(1, testFixtures()), Conv: conv, Agg: NewAggregator(),
		Hold: 100 * time.Millisecond, APIURL: api.srv.URL, Pre: map[string][][]byte{},
	}
	require.NoError(t, r.Run(context.Background()))
	require.Equal(t, map[string]string{"load-a@load.test": ""}, api.xffs)
}
