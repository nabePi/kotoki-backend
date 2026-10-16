package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const (
	outboxRetentionName = "outbox_retention"
	outboxBatch         = 5000

	// outboxFailedDays is fixed (spec 9.3.4): a FAILED row is assumed investigated after 30 days.
	outboxFailedDays = 30
)

// OutboxRetentionStore deletes old messaging bookkeeping rows (spec 9.3.4). Each call deletes at
// most batch rows and returns how many; a result below batch means the step is exhausted.
type OutboxRetentionStore interface {
	DeleteOutboxPublished(ctx context.Context, before time.Time, batch int) (int, error)
	DeleteOutboxFailed(ctx context.Context, before time.Time, batch int) (int, error)
	DeleteProcessedEvents(ctx context.Context, before time.Time, batch int) (int, error)
}

// OutboxRetentionConfig holds RETENTION_OUTBOX_PUBLISHED_DAYS and RETENTION_PROCESSED_EVENTS_DAYS.
// FAILED outbox rows are always kept 30 days.
type OutboxRetentionConfig struct{ PublishedDays, ProcessedEventsDays int }

// OutboxRetentionJob removes PUBLISHED outbox rows, FAILED outbox rows and processed_events past
// their retention.
type OutboxRetentionJob struct {
	s   OutboxRetentionStore
	cfg OutboxRetentionConfig
	m   *Metrics
	clk clock.Clock
	log *slog.Logger
}

// NewOutboxRetentionJob builds the job.
func NewOutboxRetentionJob(s OutboxRetentionStore, cfg OutboxRetentionConfig, m *Metrics, clk clock.Clock, log *slog.Logger) *OutboxRetentionJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &OutboxRetentionJob{s: s, cfg: cfg, m: m, clk: clk, log: log}
}

// Run implements Job: three steps in order, each repeated until a batch is short.
func (j *OutboxRetentionJob) Run(ctx context.Context) error {
	now := j.clk.Now()
	steps := []retentionStep{
		{"outbox_published", outboxBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.DeleteOutboxPublished(ctx, now.Add(-time.Duration(j.cfg.PublishedDays)*day), b)
		}},
		{"outbox_failed", outboxBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.DeleteOutboxFailed(ctx, now.Add(-outboxFailedDays*day), b)
		}},
		{"processed_events", outboxBatch, func(ctx context.Context, b int) (int, error) {
			return j.s.DeleteProcessedEvents(ctx, now.Add(-time.Duration(j.cfg.ProcessedEventsDays)*day), b)
		}},
	}
	return runRetentionSteps(ctx, outboxRetentionName, steps, j.m, j.log)
}
