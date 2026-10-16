package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

const (
	memoryCleanupName  = "memory_cleanup"
	memoryCleanupBatch = 1000
)

// ExpiredMemoryStore hard-deletes up to limit expired memories (any status) and returns the
// user_id of every deleted row (spec 11.8.3, SysDeleteExpiredMemories). The retention policy
// behind "expired" is M8's; this port stays stable when it changes.
type ExpiredMemoryStore interface {
	DeleteExpiredMemories(ctx context.Context, limit int) ([]uuid.UUID, error)
}

// MemVerBumper moves the per-user memory version so that cached retrievals are dropped.
type MemVerBumper interface {
	Bump(ctx context.Context, userIDs []uuid.UUID) error
}

// MemoryCleanupJob deletes expired memories in batches of 1.000 until a batch is short, then bumps
// the memory version of every affected user once. Memories of DELETED users belong to account_purge.
type MemoryCleanupJob struct {
	s   ExpiredMemoryStore
	b   MemVerBumper
	m   *Metrics
	log *slog.Logger
}

// NewMemoryCleanupJob builds the job.
func NewMemoryCleanupJob(s ExpiredMemoryStore, b MemVerBumper, m *Metrics, log *slog.Logger) *MemoryCleanupJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &MemoryCleanupJob{s: s, b: b, m: m, log: log}
}

// Run implements Job. Rows already deleted when a later batch fails still get their users bumped:
// the memver has to follow every committed delete.
func (j *MemoryCleanupJob) Run(ctx context.Context) error {
	var (
		errs  []error
		users []uuid.UUID
		seen  = map[uuid.UUID]struct{}{}
	)
	for {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("delete expired memories: %w", err))
			break
		}
		ids, err := j.s.DeleteExpiredMemories(ctx, memoryCleanupBatch)
		if len(ids) > 0 {
			j.m.JobItems.WithLabelValues(memoryCleanupName, "memories_deleted").Add(float64(len(ids)))
		}
		for _, id := range ids {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				users = append(users, id)
			}
		}
		if err != nil {
			j.log.ErrorContext(ctx, "memory_cleanup: delete batch failed", "error", err.Error())
			errs = append(errs, fmt.Errorf("delete expired memories: %w", err))
			break
		}
		if len(ids) < memoryCleanupBatch {
			break
		}
	}
	if len(users) > 0 {
		// Bump even when the context was cancelled mid-run: the deletes are already committed.
		if err := j.b.Bump(context.WithoutCancel(ctx), users); err != nil {
			j.log.ErrorContext(ctx, "memory_cleanup: memver bump failed", "users", len(users), "error", err.Error())
			errs = append(errs, fmt.Errorf("bump memver: %w", err))
		}
	}
	return errors.Join(errs...)
}
