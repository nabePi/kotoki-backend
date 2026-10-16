// Package jobs is the scheduler and the scheduled jobs of the worker (spec 9.9). A job calls the
// feature services (and the retention repositories in internal/postgres); the feature packages
// never import jobs.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// Spec is one row of the canonical job table (spec 9.9.1).
type Spec struct {
	Name     string
	Flag     string // worker flag that enables the job: sweeper or cleanup
	Interval time.Duration
	LockKey  int64 // pg advisory lock key; a code constant, never configuration
	Timeout  time.Duration
}

// Specs is the only job table (spec 9.9.1, R-8). Lock key 7100004 belongs to no job.
var Specs = []Spec{
	{Name: "session_sweeper", Flag: "sweeper", Interval: 10 * time.Second, LockKey: 7100001, Timeout: 30 * time.Second},
	{Name: "reservation_cleanup", Flag: "sweeper", Interval: 30 * time.Second, LockKey: 7100002, Timeout: 25 * time.Second},
	{Name: "transcript_retention", Flag: "cleanup", Interval: time.Hour, LockKey: 7100003, Timeout: 15 * time.Minute},
	{Name: "outbox_retention", Flag: "cleanup", Interval: time.Hour, LockKey: 7100005, Timeout: 15 * time.Minute},
	{Name: "balance_reconcile", Flag: "cleanup", Interval: time.Hour, LockKey: 7100006, Timeout: 10 * time.Minute},
	{Name: "account_purge", Flag: "cleanup", Interval: 15 * time.Minute, LockKey: 7100007, Timeout: 10 * time.Minute},
	{Name: "memory_cleanup", Flag: "cleanup", Interval: time.Hour, LockKey: 7100008, Timeout: 15 * time.Minute},
	{Name: "partition_maintenance", Flag: "cleanup", Interval: 24 * time.Hour, LockKey: 7100009, Timeout: 10 * time.Minute},
}

const sweeperJob = "session_sweeper"

// SpecByName returns the spec of the named job.
func SpecByName(name string) (Spec, bool) {
	for _, s := range Specs {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// SpecsForFlags returns, in table order, the specs whose flag is in flags (sweeper, cleanup).
func SpecsForFlags(flags []string) []Spec {
	var out []Spec
	for _, s := range Specs {
		if slices.Contains(flags, s.Flag) {
			out = append(out, s)
		}
	}
	return out
}

// Job is one scheduled unit of work. It must be idempotent and work in bounded batches: a run
// cut off by its timeout is continued by the next run.
type Job interface {
	Run(ctx context.Context) error
}

// Job run results of kotoki_job_runs_total.
const (
	resultOK            = "ok"
	resultError         = "error"
	resultSkippedLocked = "skipped_locked"
)

// SchedulerDeps are the collaborators of a Scheduler. After, NewTicker and Jitter exist so tests
// control time; zero values mean the real ones. SweeperInterval > 0 replaces the interval of
// session_sweeper only (SWEEPER_INTERVAL, dev/test).
type SchedulerDeps struct {
	Locker          Locker
	Metrics         *Metrics
	Clock           clock.Clock
	Log             *slog.Logger
	After           func(time.Duration) <-chan time.Time
	NewTicker       func(time.Duration) (<-chan time.Time, func())
	Jitter          func(time.Duration) time.Duration
	SweeperInterval time.Duration
}

type entry struct {
	spec Spec
	job  Job
}

// Scheduler runs registered jobs, one goroutine per job.
type Scheduler struct {
	d SchedulerDeps

	mu      sync.Mutex
	entries []entry
}

// NewScheduler fills the defaults of d.
func NewScheduler(d SchedulerDeps) *Scheduler {
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.After == nil {
		d.After = time.After
	}
	if d.NewTicker == nil {
		d.NewTicker = func(iv time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(iv)
			return t.C, t.Stop
		}
	}
	if d.Jitter == nil {
		d.Jitter = rand.N[time.Duration]
	}
	return &Scheduler{d: d}
}

// Register adds job under spec; registering a name again replaces the earlier job.
func (s *Scheduler) Register(spec Spec, job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].spec.Name == spec.Name {
			s.entries[i] = entry{spec, job}
			return
		}
	}
	s.entries = append(s.entries, entry{spec, job})
}

func (s *Scheduler) interval(spec Spec) time.Duration {
	if spec.Name == sweeperJob && s.d.SweeperInterval > 0 {
		return s.d.SweeperInterval
	}
	return spec.Interval
}

// Run blocks until ctx ends. Each job waits a random 0..interval, runs, then runs again on every
// tick; a job never overlaps itself on this instance. Once ctx ends no new run starts and Run
// waits for the runs in progress, which keep a live context bounded by Spec.Timeout.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	entries := slices.Clone(s.entries)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, e)
		}()
	}
	wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context, e entry) {
	iv := s.interval(e.spec)
	select {
	case <-ctx.Done():
		return
	case <-s.d.After(s.d.Jitter(iv)):
	}
	ticks, stop := s.d.NewTicker(iv)
	defer stop()
	for {
		if ctx.Err() != nil {
			return
		}
		// the run outlives a cancelled ctx on purpose: shutdown waits for it instead of cutting it off
		_, _ = s.execute(context.WithoutCancel(ctx), e)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// RunOnce runs the named registered job now (worker run-job), under the same advisory lock, timeout
// and metrics as a scheduled run. ran=false means another instance holds the lock.
func (s *Scheduler) RunOnce(ctx context.Context, name string) (bool, error) {
	s.mu.Lock()
	idx := slices.IndexFunc(s.entries, func(e entry) bool { return e.spec.Name == name })
	var e entry
	if idx >= 0 {
		e = s.entries[idx]
	}
	s.mu.Unlock()
	if idx < 0 {
		return false, fmt.Errorf("jobs: unknown or unregistered job %q", name)
	}
	return s.execute(ctx, e)
}

// execute runs one job once under the advisory lock with the spec timeout, and records the metrics.
func (s *Scheduler) execute(ctx context.Context, e entry) (bool, error) {
	name := e.spec.Name
	rctx, cancel := context.WithTimeout(ctx, e.spec.Timeout)
	defer cancel()

	start := s.d.Clock.Now()
	ran, err := s.d.Locker.RunExclusive(rctx, e.spec.LockKey, func(c context.Context) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("jobs: %s panicked: %v", name, r)
			}
		}()
		return e.job.Run(c)
	})
	end := s.d.Clock.Now()

	m := s.d.Metrics
	switch {
	case err != nil:
		m.JobDuration.WithLabelValues(name).Observe(end.Sub(start).Seconds())
		m.JobRuns.WithLabelValues(name, resultError).Inc()
		s.d.Log.ErrorContext(ctx, "job run failed", "job", name, "error", err)
	case !ran:
		m.JobRuns.WithLabelValues(name, resultSkippedLocked).Inc()
		s.d.Log.DebugContext(ctx, "job skipped, lock held elsewhere", "job", name)
	default:
		// the result counter goes last: whoever sees the run counted also sees its duration and timestamp
		m.JobDuration.WithLabelValues(name).Observe(end.Sub(start).Seconds())
		m.JobLastSuccess.WithLabelValues(name).Set(float64(end.Unix()))
		m.JobRuns.WithLabelValues(name, resultOK).Inc()
	}
	return ran, err
}
