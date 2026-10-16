package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const (
	transcriptRetentionName = "transcript_retention"

	evaluationBatch = 500
	turnsBatch      = 5000
	eventsBatch     = 5000
	summaryBatch    = 1000
	tokenBatch      = 1000

	// turnMargin keeps a turn until it is certain that its session ended at least TranscriptDays
	// ago: a turn is created at most ~1 h 5 min before sessions.ended_at (spec 9.9.4 step 2).
	turnMargin = 2 * time.Hour
	day        = 24 * time.Hour
)

// RetentionStore runs the batches of spec 9.9.4. Each call is one batch in one transaction and
// returns the rows it handled; a result below the batch size means the step is exhausted.
type RetentionStore interface {
	PurgeEvaluationDetails(ctx context.Context, before time.Time, batch int) (int, error)
	DeleteTurnsBefore(ctx context.Context, cutoff time.Time, batch int) (int, error)
	DeleteEventsBefore(ctx context.Context, cutoff time.Time, batch int) (int, error)
	PurgeSessionSummaries(ctx context.Context, before time.Time, batch int) (int, error)
	DeleteExpiredTokens(ctx context.Context, now time.Time, tokenRetention time.Duration, batch int) (int, error)
}

// RetentionConfig holds the retention windows in days (RETENTION_* env).
type RetentionConfig struct{ TranscriptDays, SessionEventsDays, ExpiredTokensDays int }

// TranscriptRetentionJob enforces the data retention policy of D-099/R-21: evaluation feedback
// text, turn transcripts and session summaries 180 days after the session ended, session events
// after 365 days, and expired tokens after 7 days. Whole partitions past retention are dropped by
// partition_maintenance; this job deletes the rows inside the boundary partition.
type TranscriptRetentionJob struct {
	s   RetentionStore
	cfg RetentionConfig
	m   *Metrics
	clk clock.Clock
	log *slog.Logger
}

// NewTranscriptRetentionJob builds the job.
func NewTranscriptRetentionJob(s RetentionStore, cfg RetentionConfig, m *Metrics, clk clock.Clock, log *slog.Logger) *TranscriptRetentionJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &TranscriptRetentionJob{s: s, cfg: cfg, m: m, clk: clk, log: log}
}

type retentionStep struct {
	action string
	batch  int
	run    func(ctx context.Context, batch int) (int, error)
}

// Run implements Job. Steps run in the order of spec 9.9.4, each repeated until a batch comes back
// short. A failing step is logged and the next one still runs; all failures are returned together.
func (j *TranscriptRetentionJob) Run(ctx context.Context) error {
	now := j.clk.Now()
	transcriptBefore := now.Add(-time.Duration(j.cfg.TranscriptDays) * day)
	steps := []retentionStep{
		{"evaluation_details", evaluationBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.PurgeEvaluationDetails(ctx, transcriptBefore, b)
		}},
		{"turns", turnsBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.DeleteTurnsBefore(ctx, transcriptBefore.Add(-turnMargin), b)
		}},
		{"events", eventsBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.DeleteEventsBefore(ctx, now.Add(-time.Duration(j.cfg.SessionEventsDays)*day), b)
		}},
		{"summaries", summaryBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.PurgeSessionSummaries(ctx, transcriptBefore, b)
		}},
		{"tokens", tokenBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.DeleteExpiredTokens(ctx, now, time.Duration(j.cfg.ExpiredTokensDays)*day, b)
		}},
	}
	var errs []error
	for _, st := range steps {
		if err := j.drain(ctx, st); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
		}
	}
	return errors.Join(errs...)
}

func (j *TranscriptRetentionJob) drain(ctx context.Context, st retentionStep) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", st.action, err)
		}
		n, err := st.run(ctx, st.batch)
		if n > 0 {
			j.m.JobItems.WithLabelValues(transcriptRetentionName, st.action).Add(float64(n))
		}
		if err != nil {
			j.log.ErrorContext(ctx, "transcript_retention: step failed", "step", st.action, "error", err.Error())
			return fmt.Errorf("%s: %w", st.action, err)
		}
		if n < st.batch {
			return nil
		}
	}
}
