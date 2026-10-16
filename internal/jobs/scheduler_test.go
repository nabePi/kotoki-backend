package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

func TestSpecs_MatchSection991(t *testing.T) {
	type row struct {
		name, flag string
		interval   time.Duration
		lock       int64
		timeout    time.Duration
	}
	// spec 9.9.1: eight jobs; lock key 7100004 is not used by any job
	want := []row{
		{"session_sweeper", "sweeper", 10 * time.Second, 7100001, 30 * time.Second},
		{"reservation_cleanup", "sweeper", 30 * time.Second, 7100002, 25 * time.Second},
		{"transcript_retention", "cleanup", time.Hour, 7100003, 15 * time.Minute},
		{"outbox_retention", "cleanup", time.Hour, 7100005, 15 * time.Minute},
		{"balance_reconcile", "cleanup", time.Hour, 7100006, 10 * time.Minute},
		{"account_purge", "cleanup", 15 * time.Minute, 7100007, 10 * time.Minute},
		{"memory_cleanup", "cleanup", time.Hour, 7100008, 15 * time.Minute},
		{"partition_maintenance", "cleanup", 24 * time.Hour, 7100009, 10 * time.Minute},
	}
	require.Len(t, jobs.Specs, len(want))
	for i, w := range want {
		got := jobs.Specs[i]
		assert.Equal(t, w, row{got.Name, got.Flag, got.Interval, got.LockKey, got.Timeout}, w.name)
		byName, ok := jobs.SpecByName(w.name)
		require.True(t, ok)
		assert.Equal(t, got, byName)
	}
	_, ok := jobs.SpecByName("nope")
	assert.False(t, ok)
}

func TestSpecsForFlags_Sweeper_Cleanup(t *testing.T) {
	names := func(s []jobs.Spec) []string {
		var out []string
		for _, sp := range s {
			out = append(out, sp.Name)
		}
		return out
	}
	assert.Equal(t, []string{"session_sweeper", "reservation_cleanup"}, names(jobs.SpecsForFlags([]string{"sweeper"})))
	assert.Len(t, jobs.SpecsForFlags([]string{"cleanup"}), 6)
	assert.Len(t, jobs.SpecsForFlags([]string{"sweeper", "cleanup"}), 8)
	assert.Len(t, jobs.SpecsForFlags([]string{"cleanup", "cleanup"}), 6, "a repeated flag does not repeat a job")
	assert.Empty(t, jobs.SpecsForFlags([]string{"outbox"}))
	assert.Empty(t, jobs.SpecsForFlags([]string{"evaluation", "memory", "learner", "summary"}))
	assert.Empty(t, jobs.SpecsForFlags(nil))
}

// ---- doubles ---------------------------------------------------------------------------------

type fakeLocker struct {
	mu   sync.Mutex
	keys []int64
	ran  bool // value reported when err is nil; set by newFakeLocker
	err  error
}

func newFakeLocker(ran bool) *fakeLocker { return &fakeLocker{ran: ran} }

func (l *fakeLocker) RunExclusive(ctx context.Context, key int64, fn func(context.Context) error) (bool, error) {
	l.mu.Lock()
	l.keys = append(l.keys, key)
	ran, err := l.ran, l.err
	l.mu.Unlock()
	if err != nil {
		return false, err
	}
	if !ran {
		return false, nil
	}
	return true, fn(ctx)
}

type funcJob func(ctx context.Context) error

func (f funcJob) Run(ctx context.Context) error { return f(ctx) }

// timers replaces After/NewTicker/Jitter: After and tickers are controlled by the test.
type timers struct {
	mu        sync.Mutex
	jitterIn  []time.Duration
	afterReq  []time.Duration
	afterCh   chan time.Time
	tickerReq []time.Duration
	tickCh    chan time.Time
}

func newTimers() *timers {
	return &timers{afterCh: make(chan time.Time, 1), tickCh: make(chan time.Time, 1)}
}

func (tm *timers) Jitter(d time.Duration) time.Duration {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.jitterIn = append(tm.jitterIn, d)
	return d / 2
}

func (tm *timers) After(d time.Duration) <-chan time.Time {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.afterReq = append(tm.afterReq, d)
	return tm.afterCh
}

func (tm *timers) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.tickerReq = append(tm.tickerReq, d)
	return tm.tickCh, func() {}
}

func (tm *timers) tickerRequests() []time.Duration {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return append([]time.Duration(nil), tm.tickerReq...)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newSched(t *testing.T, loc jobs.Locker, tm *timers, clk clock.Clock, sweeper time.Duration) (*jobs.Scheduler, *jobs.Metrics) {
	t.Helper()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	return jobs.NewScheduler(jobs.SchedulerDeps{
		Locker: loc, Metrics: m, Clock: clk, Log: quiet,
		After: tm.After, NewTicker: tm.NewTicker, Jitter: tm.Jitter, SweeperInterval: sweeper,
	}), m
}

func startRun(s *jobs.Scheduler) (cancel context.CancelFunc, done chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	return cancel, done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func runs(m *jobs.Metrics, job, result string) float64 {
	return promtest.ToFloat64(m.JobRuns.WithLabelValues(job, result))
}

var testSpec = jobs.Spec{Name: "t_job", Flag: "cleanup", Interval: time.Hour, LockKey: 42, Timeout: 20 * time.Second}

// ---- scheduler ------------------------------------------------------------------------------

func TestScheduler_InitialJitterThenTicks(t *testing.T) {
	tm := newTimers()
	loc := newFakeLocker(true)
	s, _ := newSched(t, loc, tm, clock.System{}, 0)
	var calls atomic.Int64
	s.Register(testSpec, funcJob(func(context.Context) error { calls.Add(1); return nil }))

	cancel, done := startRun(s)
	require.Eventually(t, func() bool { tm.mu.Lock(); defer tm.mu.Unlock(); return len(tm.afterReq) == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, time.Hour/2, tm.afterReq[0], "the initial wait is the jitter drawn in [0, interval)")
	assert.Equal(t, []time.Duration{time.Hour}, tm.jitterIn)
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 0, calls.Load(), "no run before the initial jitter elapsed")

	tm.afterCh <- time.Now()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	tm.tickCh <- time.Now()
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 2*time.Second, 5*time.Millisecond)
	tm.tickCh <- time.Now()
	require.Eventually(t, func() bool { return calls.Load() == 3 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, []time.Duration{time.Hour}, tm.tickerRequests())
	assert.Equal(t, []int64{42, 42, 42}, loc.keys, "every run takes the job's advisory lock key")

	cancel()
	waitDone(t, done)
}

func TestScheduler_NoOverlapOnSameInstance(t *testing.T) {
	tm := newTimers()
	s, _ := newSched(t, newFakeLocker(true), tm, clock.System{}, 0)
	release := make(chan struct{})
	var cur, peak, calls atomic.Int64
	s.Register(testSpec, funcJob(func(context.Context) error {
		n := cur.Add(1)
		defer cur.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		calls.Add(1)
		<-release
		return nil
	}))
	cancel, done := startRun(s)
	tm.afterCh <- time.Now()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)

	tm.tickCh <- time.Now() // the slow run is still going
	time.Sleep(100 * time.Millisecond)
	assert.EqualValues(t, 1, calls.Load(), "a tick during a run does not start a second run")

	close(release)
	require.Eventually(t, func() bool { return calls.Load() == 2 }, 2*time.Second, 5*time.Millisecond, "the pending tick runs after the first run ended")
	assert.EqualValues(t, 1, peak.Load(), "never two runs of one job at once")
	cancel()
	waitDone(t, done)
}

func TestScheduler_SkippedLocked_Metric(t *testing.T) {
	tm := newTimers()
	loc := newFakeLocker(false)
	s, m := newSched(t, loc, tm, clock.System{}, 0)
	var calls atomic.Int64
	s.Register(testSpec, funcJob(func(context.Context) error { calls.Add(1); return nil }))
	cancel, done := startRun(s)
	tm.afterCh <- time.Now()

	require.Eventually(t, func() bool { return runs(m, "t_job", "skipped_locked") == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.EqualValues(t, 0, calls.Load(), "the job is not called when another instance holds the lock")
	assert.EqualValues(t, 0, runs(m, "t_job", "ok"))
	assert.EqualValues(t, 0, runs(m, "t_job", "error"))
	assert.EqualValues(t, 0, promtest.ToFloat64(m.JobLastSuccess.WithLabelValues("t_job")))
	cancel()
	waitDone(t, done)
}

func histogramOf(t *testing.T, o prometheus.Observer) *dto.Histogram {
	t.Helper()
	var d dto.Metric
	require.NoError(t, o.(prometheus.Metric).Write(&d))
	return d.GetHistogram()
}

func TestScheduler_OkAndErrorResults(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	tm := newTimers()
	s, m := newSched(t, newFakeLocker(true), tm, clk, 0)
	var fail atomic.Bool
	s.Register(testSpec, funcJob(func(context.Context) error {
		clk.Advance(3 * time.Second)
		if fail.Load() {
			return errors.New("boom")
		}
		return nil
	}))
	cancel, done := startRun(s)

	tm.afterCh <- time.Now()
	require.Eventually(t, func() bool { return runs(m, "t_job", "ok") == 1 }, 2*time.Second, 5*time.Millisecond)
	okAt := promtest.ToFloat64(m.JobLastSuccess.WithLabelValues("t_job"))
	assert.EqualValues(t, t0.Add(3*time.Second).Unix(), okAt, "last_success is the finish time in epoch seconds")
	h := histogramOf(t, m.JobDuration.WithLabelValues("t_job"))
	assert.EqualValues(t, 1, h.GetSampleCount())
	assert.InDelta(t, 3, h.GetSampleSum(), 0.001)

	fail.Store(true)
	tm.tickCh <- time.Now()
	require.Eventually(t, func() bool { return runs(m, "t_job", "error") == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.EqualValues(t, okAt, promtest.ToFloat64(m.JobLastSuccess.WithLabelValues("t_job")), "an error does not move last_success")
	assert.EqualValues(t, 2, histogramOf(t, m.JobDuration.WithLabelValues("t_job")).GetSampleCount(), "errors are timed too")
	assert.EqualValues(t, 1, runs(m, "t_job", "ok"))
	cancel()
	waitDone(t, done)
}

func TestScheduler_PanicIsAnErrorResult(t *testing.T) {
	tm := newTimers()
	s, m := newSched(t, newFakeLocker(true), tm, clock.System{}, 0)
	s.Register(testSpec, funcJob(func(context.Context) error { panic("kaboom") }))
	cancel, done := startRun(s)
	tm.afterCh <- time.Now()
	require.Eventually(t, func() bool { return runs(m, "t_job", "error") == 1 }, 2*time.Second, 5*time.Millisecond)
	tm.tickCh <- time.Now()
	require.Eventually(t, func() bool { return runs(m, "t_job", "error") == 2 }, 2*time.Second, 5*time.Millisecond, "the loop survives a panicking job")
	cancel()
	waitDone(t, done)
}

func TestScheduler_RunTimeout(t *testing.T) {
	tm := newTimers()
	s, _ := newSched(t, newFakeLocker(true), tm, clock.System{}, 0)
	got := make(chan time.Duration, 1)
	s.Register(testSpec, funcJob(func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			got <- -1
			return nil
		}
		got <- time.Until(dl)
		return nil
	}))
	cancel, done := startRun(s)
	tm.afterCh <- time.Now()

	select {
	case left := <-got:
		assert.Greater(t, left, testSpec.Timeout-3*time.Second)
		assert.LessOrEqual(t, left, testSpec.Timeout)
	case <-time.After(2 * time.Second):
		t.Fatal("job did not run")
	}
	cancel()
	waitDone(t, done)
}

func TestScheduler_StopsStartingNewRunsOnCancel(t *testing.T) {
	tm := newTimers()
	s, _ := newSched(t, newFakeLocker(true), tm, clock.System{}, 0)
	release := make(chan struct{})
	var calls atomic.Int64
	var ctxErrInRun atomic.Value
	s.Register(testSpec, funcJob(func(ctx context.Context) error {
		calls.Add(1)
		<-release
		ctxErrInRun.Store(ctx.Err() == nil)
		return nil
	}))
	cancel, done := startRun(s)
	tm.afterCh <- time.Now()
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	tm.tickCh <- time.Now()
	select {
	case <-done:
		t.Fatal("Run returned while a run was still in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	waitDone(t, done)
	assert.EqualValues(t, 1, calls.Load(), "no new run after cancel")
	assert.Equal(t, true, ctxErrInRun.Load(), "the run in progress finishes with a live context")
}

func TestScheduler_CancelDuringInitialJitter(t *testing.T) {
	tm := newTimers()
	s, _ := newSched(t, newFakeLocker(true), tm, clock.System{}, 0)
	var calls atomic.Int64
	s.Register(testSpec, funcJob(func(context.Context) error { calls.Add(1); return nil }))
	cancel, done := startRun(s)
	require.Eventually(t, func() bool { tm.mu.Lock(); defer tm.mu.Unlock(); return len(tm.afterReq) == 1 }, 2*time.Second, 5*time.Millisecond)

	cancel()
	waitDone(t, done)
	assert.EqualValues(t, 0, calls.Load())
}

func TestScheduler_SweeperIntervalOverride(t *testing.T) {
	tm := newTimers()
	s, _ := newSched(t, newFakeLocker(true), tm, clock.System{}, 2*time.Second)
	noop := funcJob(func(context.Context) error { return nil })
	s.Register(jobs.Spec{Name: "session_sweeper", Flag: "sweeper", Interval: 10 * time.Second, LockKey: 7100001, Timeout: 30 * time.Second}, noop)
	s.Register(jobs.Spec{Name: "reservation_cleanup", Flag: "sweeper", Interval: 30 * time.Second, LockKey: 7100002, Timeout: 25 * time.Second}, noop)
	cancel, done := startRun(s)
	t.Cleanup(func() { cancel(); waitDone(t, done) })

	// two goroutines wait on the shared After channel; release both
	tm.afterCh <- time.Now()
	tm.afterCh <- time.Now()
	require.Eventually(t, func() bool { return len(tm.tickerRequests()) == 2 }, 2*time.Second, 5*time.Millisecond)
	assert.ElementsMatch(t, []time.Duration{2 * time.Second, 30 * time.Second}, tm.tickerRequests(),
		"only session_sweeper takes SweeperInterval")
	tm.mu.Lock()
	defer tm.mu.Unlock()
	assert.ElementsMatch(t, []time.Duration{2 * time.Second, 30 * time.Second}, tm.jitterIn, "the jitter range follows the effective interval")
}

func TestRunOnce_UnknownJob_Error(t *testing.T) {
	s, _ := newSched(t, newFakeLocker(true), newTimers(), clock.System{}, 0)

	ran, err := s.RunOnce(context.Background(), "nope")

	require.Error(t, err)
	assert.False(t, ran)
	assert.Contains(t, err.Error(), "nope")
}

func TestRunOnce_RunsWithLockAndTimeout(t *testing.T) {
	loc := newFakeLocker(true)
	s, m := newSched(t, loc, newTimers(), clock.System{}, 0)
	var hasDeadline bool
	s.Register(testSpec, funcJob(func(ctx context.Context) error { _, hasDeadline = ctx.Deadline(); return nil }))

	ran, err := s.RunOnce(context.Background(), "t_job")

	require.NoError(t, err)
	assert.True(t, ran)
	assert.True(t, hasDeadline)
	assert.Equal(t, []int64{42}, loc.keys)
	assert.EqualValues(t, 1, runs(m, "t_job", "ok"))

	boom := errors.New("boom")
	s.Register(jobs.Spec{Name: "bad", Flag: "cleanup", Interval: time.Hour, LockKey: 43, Timeout: time.Minute},
		funcJob(func(context.Context) error { return boom }))
	ran, err = s.RunOnce(context.Background(), "bad")
	require.ErrorIs(t, err, boom)
	assert.True(t, ran)

	loc.mu.Lock()
	loc.ran = false
	loc.mu.Unlock()
	ran, err = s.RunOnce(context.Background(), "t_job")
	require.NoError(t, err)
	assert.False(t, ran, "another instance holds the lock")
	assert.EqualValues(t, 1, runs(m, "t_job", "skipped_locked"))
}
