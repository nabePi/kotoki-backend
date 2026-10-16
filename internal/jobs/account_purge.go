package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

const (
	accountPurgeName     = "account_purge"
	accountPurgeUsers    = 50
	purgeTranscriptBatch = 1000
)

// PurgeStore holds the per-user steps of spec 13.9.3. Every step is idempotent and commits on its
// own, so a purge that crashed half way is safe to start again from the first step.
type PurgeStore interface {
	// ListCandidates returns up to limit users with status DELETED, purged_at NULL and deleted_at
	// before deletedBefore, oldest deleted_at first.
	ListCandidates(ctx context.Context, deletedBefore time.Time, limit int) ([]uuid.UUID, error)
	DeleteMemories(ctx context.Context, userID uuid.UUID) error
	DeleteEvaluations(ctx context.Context, userID uuid.UUID) error
	DeleteLearner(ctx context.Context, userID uuid.UUID) error
	DeleteTranscripts(ctx context.Context, userID uuid.UUID, batch int) error
	ClearSummaries(ctx context.Context, userID uuid.UUID) error
	// Anonymize rewrites the user row and sets purged_at = now. It is the last step: purged_at
	// marks a user as completely purged.
	Anonymize(ctx context.Context, userID uuid.UUID, now time.Time) error
}

// AccountPurgeJob purges the data of accounts deleted more than ACCOUNT_PURGE_DELAY ago: memory,
// evaluations, learner profile, transcripts and session summaries, then anonymizes the user row.
// Balance accounts, reservations, ledger and the sessions rows are kept (financial records).
type AccountPurgeJob struct {
	s     PurgeStore
	delay time.Duration
	m     *Metrics
	clk   clock.Clock
	log   *slog.Logger
}

// NewAccountPurgeJob builds the job.
func NewAccountPurgeJob(s PurgeStore, delay time.Duration, m *Metrics, clk clock.Clock, log *slog.Logger) *AccountPurgeJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &AccountPurgeJob{s: s, delay: delay, m: m, clk: clk, log: log}
}

// Run implements Job. A user whose purge fails is logged and skipped; the next user still runs and
// the failures are returned together. The failed user keeps purged_at NULL and is picked up again
// by the next run.
func (j *AccountPurgeJob) Run(ctx context.Context) error {
	users, err := j.s.ListCandidates(ctx, j.clk.Now().Add(-j.delay), accountPurgeUsers)
	if err != nil {
		return fmt.Errorf("list purge candidates: %w", err)
	}
	var errs []error
	for _, u := range users {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := j.purge(ctx, u); err != nil {
			j.m.JobItems.WithLabelValues(accountPurgeName, "users_failed").Inc()
			j.log.ErrorContext(ctx, "account_purge: user purge failed", "user_id", u.String(), "error", err.Error())
			errs = append(errs, fmt.Errorf("user %s: %w", u, err))
			continue
		}
		j.m.JobItems.WithLabelValues(accountPurgeName, "users_purged").Inc()
		j.log.InfoContext(ctx, "account_purged", "audit", true, "audit_event", "account_purged", "user_id", u.String())
	}
	return errors.Join(errs...)
}

// purge runs the steps in the order of spec 13.9.3 and stops at the first failure, so Anonymize
// (which sets purged_at) only runs once every earlier step has succeeded.
func (j *AccountPurgeJob) purge(ctx context.Context, u uuid.UUID) error {
	steps := []struct {
		name string
		run  func() error
	}{
		{"memories", func() error { return j.s.DeleteMemories(ctx, u) }},
		{"evaluations", func() error { return j.s.DeleteEvaluations(ctx, u) }},
		{"learner", func() error { return j.s.DeleteLearner(ctx, u) }},
		{"transcripts", func() error { return j.s.DeleteTranscripts(ctx, u, purgeTranscriptBatch) }},
		{"summaries", func() error { return j.s.ClearSummaries(ctx, u) }},
		{"anonymize", func() error { return j.s.Anonymize(ctx, u, j.clk.Now()) }},
	}
	for _, st := range steps {
		if err := st.run(); err != nil {
			return fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return nil
}
