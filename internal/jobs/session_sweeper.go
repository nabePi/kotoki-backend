package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// sessionSweeperName is the job label of session_sweeper in the job metrics.
const sessionSweeperName = "session_sweeper"

// SessionTicker is the part of session.Sweeper the job needs.
type SessionTicker interface {
	Tick(ctx context.Context) (session.SweepReport, error)
}

// SessionSweeperJob runs one session.Sweeper tick per run and turns its report into metrics. The
// rules live in session (spec 5.6.4); this job holds no business logic.
type SessionSweeperJob struct {
	newSweeper func() SessionTicker
	m          *Metrics
	log        *slog.Logger
	clk        clock.Clock
	interval   time.Duration

	mu      sync.Mutex
	sweeper SessionTicker
	lastRun time.Time
}

// NewSessionSweeperJob builds the job. newSweeper is called for the first run and again whenever
// more than 2x interval passed since the previous run: another instance may have held the lock in
// between, so the heartbeat guard of the new Sweeper starts from zero (spec 9.9.2, safeguard 1).
func NewSessionSweeperJob(newSweeper func() SessionTicker, m *Metrics, log *slog.Logger, clk clock.Clock, interval time.Duration) *SessionSweeperJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SessionSweeperJob{newSweeper: newSweeper, m: m, log: log, clk: clk, interval: interval}
}

// Run implements Job. It returns an error only when the tick itself failed.
func (j *SessionSweeperJob) Run(ctx context.Context) error {
	sw := j.sweeperFor(j.clk.Now())
	rep, err := sw.Tick(ctx)
	j.record(ctx, rep)
	return err
}

func (j *SessionSweeperJob) sweeperFor(now time.Time) SessionTicker {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.sweeper == nil || now.Sub(j.lastRun) > 2*j.interval {
		j.sweeper = j.newSweeper()
	}
	j.lastRun = now
	return j.sweeper
}

func (j *SessionSweeperJob) record(ctx context.Context, rep session.SweepReport) {
	for rule, res := range rep.Rule {
		for _, r := range []struct {
			result string
			n      int
		}{{"ok", res.OK}, {"noop", res.Noop}, {"error", res.Error}} {
			j.m.SweeperActions.WithLabelValues(rule, r.result).Add(float64(r.n))
			j.m.JobItems.WithLabelValues(sessionSweeperName, "rule_"+rule+"_"+r.result).Add(float64(r.n))
		}
	}
	suspended := 0.0
	if rep.HeartbeatSuspended {
		suspended = 1
	}
	j.m.HeartbeatChecksSuspended.Set(suspended)
	if rep.MassHeartbeatLoss {
		j.m.MassHeartbeatLoss.Inc()
		j.log.ErrorContext(ctx, "sweeper.mass_heartbeat_loss", "candidates", rep.HeartbeatCandidates, "active", rep.ActiveNonHandoff)
	}
}
