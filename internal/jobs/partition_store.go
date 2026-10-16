package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/nabePi/kotoki-backend/internal/postgres"
)

// PgPartitionStore is the PartitionStore over the Postgres partition repository. The repository
// deals in raw bound text and plain names (internal/postgres must not import this package); the
// bound parsing and the naming rules stay here, next to the pure planning functions.
type PgPartitionStore struct{ repo *postgres.PartitionRepo }

// NewPgPartitionStore wraps repo.
func NewPgPartitionStore(repo *postgres.PartitionRepo) *PgPartitionStore {
	return &PgPartitionStore{repo: repo}
}

// List implements PartitionStore. A partition whose bound cannot be parsed (e.g. a DEFAULT
// partition) is an error: the job then neither drops nor reports anything for that table.
func (s *PgPartitionStore) List(ctx context.Context, table string) ([]Partition, error) {
	rows, err := s.repo.List(ctx, table)
	if err != nil {
		return nil, err
	}
	out := make([]Partition, 0, len(rows))
	for _, r := range rows {
		lower, upper, err := ParseBound(r.Bound)
		if err != nil {
			return nil, fmt.Errorf("partition %s: %w", r.Name, err)
		}
		out = append(out, Partition{Name: r.Name, Lower: lower, Upper: upper, DetachPending: r.DetachPending})
	}
	return out, nil
}

// Ensure implements PartitionStore for the UTC month containing month.
func (s *PgPartitionStore) Ensure(ctx context.Context, table string, month time.Time) error {
	lower := monthStart(month)
	return s.repo.Ensure(ctx, table, PartitionName(table, lower), lower, lower.AddDate(0, 1, 0))
}

// Drop implements PartitionStore.
func (s *PgPartitionStore) Drop(ctx context.Context, table string, p Partition) error {
	return s.repo.Drop(ctx, table, p.Name, p.DetachPending)
}
