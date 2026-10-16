package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// List paging (spec 5.9.7).
const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// ParseCategory reports whether s is a memory category.
func ParseCategory(s string) (Category, bool) {
	switch c := Category(s); c {
	case CategoryProfileFact, CategoryPreference, CategoryLearningPattern, CategoryEpisodic:
		return c, true
	}
	return "", false
}

// ListResult is one page of the memories of a user. Next is nil on the last page.
type ListResult struct {
	Items         []Memory
	Next          *ListCursor
	MemoryEnabled bool
}

// Service is the user-facing side of memory (spec 11.9): list, delete one and delete all. Every
// operation acts for the user it is given and for nobody else; a memory of another user is
// indistinguishable from a missing one.
type Service struct {
	store    Store
	bump     MemVer
	settings ProfileSettings
	clock    clock.Clock
	log      *slog.Logger
}

// NewService returns a Service. bump invalidates the memory context of the active sessions of the
// user after a deletion; settings reads the memory switch of the profile. log may be nil.
func NewService(s Store, bump MemVer, settings ProfileSettings, clk clock.Clock, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: s, bump: bump, settings: settings, clock: clk, log: log}
}

// List returns the ACTIVE, unexpired memories of the user ordered (category, last_observed_at DESC,
// id), at most limit after the cursor (a limit of 0 or less means 20, the maximum is 100), with
// the memory switch of the profile.
func (s *Service) List(ctx context.Context, userID uuid.UUID, cat *Category, after *ListCursor, limit int) (ListResult, error) {
	if cat != nil {
		if _, ok := ParseCategory(string(*cat)); !ok {
			return ListResult{}, errs.New(errs.CodeValidationFailed, "invalid category")
		}
	}
	if after != nil {
		if _, ok := ParseCategory(string(after.Category)); !ok || after.ID == uuid.Nil || after.LastObservedAt.IsZero() {
			return ListResult{}, errs.New(errs.CodeInvalidCursor, "invalid memory cursor")
		}
	}
	switch {
	case limit <= 0:
		limit = defaultListLimit
	case limit > maxListLimit:
		limit = maxListLimit
	}
	enabled, err := s.settings.EnsureMemoryEnabled(ctx, userID)
	if err != nil {
		return ListResult{}, fmt.Errorf("read memory switch: %w", err)
	}
	rows, err := s.store.Reader().List(ctx, userID, cat, after, limit+1, s.clock.Now()) // one more tells whether a page follows
	if err != nil {
		return ListResult{}, fmt.Errorf("list memories: %w", err)
	}
	res := ListResult{Items: rows, MemoryEnabled: enabled}
	if len(rows) > limit {
		res.Items = rows[:limit]
		last := res.Items[limit-1]
		res.Next = &ListCursor{Category: last.Category, LastObservedAt: last.LastObservedAt, ID: last.ID}
	}
	return res, nil
}

// Delete hard-deletes the ACTIVE memory id of the user with the supersede chain behind it and, but
// for EPISODIC, leaves the 90-day tombstone of its slot (spec 11.9). A memory that does not exist,
// belongs to another user or is not ACTIVE is MEMORY_NOT_FOUND. The memory context of the active
// sessions is invalidated after the commit.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		repo := tx.Memories()
		if err := repo.LockUser(ctx, userID); err != nil { // serialises with the extraction worker
			return err
		}
		ok, err := repo.Delete(ctx, userID, id, s.clock.Now())
		if err != nil {
			return err
		}
		if !ok {
			return errs.New(errs.CodeMemoryNotFound, "memory not found")
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return errs.New(errs.CodeMemoryNotFound, "memory not found")
		}
		return err
	}
	s.invalidate(ctx, userID)
	return nil
}

// DeleteAll removes every memory of the user, superseded rows and tombstones included, and creates
// no tombstone. It is idempotent.
func (s *Service) DeleteAll(ctx context.Context, userID uuid.UUID) error {
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		repo := tx.Memories()
		if err := repo.LockUser(ctx, userID); err != nil {
			return err
		}
		return repo.DeleteAll(ctx, userID)
	})
	if err != nil {
		return err
	}
	s.invalidate(ctx, userID)
	return nil
}

// invalidate bumps memver. The deletion is committed and authoritative, so a Redis failure is
// logged and not reported as a failed request: a retry of Delete would only answer 404.
func (s *Service) invalidate(ctx context.Context, userID uuid.UUID) {
	if err := s.bump.Bump(ctx, []uuid.UUID{userID}); err != nil {
		s.log.ErrorContext(ctx, "memory memver bump failed after deletion", slog.String("user_id", userID.String()), slog.Any("error", err))
	}
}
