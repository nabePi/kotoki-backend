package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// memorySource implements memory.Source: the transcript side of the extraction worker, read
// outside any transaction.
type memorySource struct {
	q     *sqlcgen.Queries
	clock clock.Clock
}

var _ memory.Source = memorySource{}

// NewMemorySource returns the memory.Source over pool; clock decides which memories are expired.
func NewMemorySource(pool *pgxpool.Pool, c clock.Clock) memory.Source {
	return memorySource{q: sqlcgen.New(pool), clock: c}
}

// UserDeleted reports users.status = 'DELETED'. A user without a row counts as deleted: there is
// nobody to write for, and retrying cannot change that.
func (s memorySource) UserDeleted(ctx context.Context, userID uuid.UUID) (bool, error) {
	status, err := s.q.GetUserStatus(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, internalErr(err, "get user status failed")
	}
	return status == "DELETED", nil
}

func (s memorySource) Session(ctx context.Context, id uuid.UUID) (memory.SessionInfo, error) {
	r, err := s.q.GetSessionForSummary(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.SessionInfo{}, memory.ErrNotFound
	}
	if err != nil {
		return memory.SessionInfo{}, internalErr(err, "load session for memory failed")
	}
	info := memory.SessionInfo{UserID: r.UserID, ScenarioName: r.ScenarioName}
	if t := fromTszPtr(r.EndedAt); t != nil {
		info.EndedAt = *t
	}
	return info, nil
}

func (s memorySource) Turns(ctx context.Context, id uuid.UUID) ([]memory.TurnRow, error) {
	rows, err := s.q.ListTurnsForMemory(ctx, id)
	if err != nil {
		return nil, internalErr(err, "load turns for memory failed")
	}
	out := make([]memory.TurnRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, memory.TurnRow{
			ID: r.ID, Seq: int(r.SequenceNumber), Status: r.Status.String, UserText: fromText(r.UserText),
			AssistantText: r.AssistantText.String, AssistantSpokenText: r.AssistantSpokenText.String,
		})
	}
	return out, nil
}

func (s memorySource) Existing(ctx context.Context, userID uuid.UUID, limit int) ([]memory.ExistingMemoryLine, error) {
	return NewMemoryRepo(s.q, idgen.NewV7(), s.clock).ListExisting(ctx, userID, limit, s.clock.Now())
}

// memoryProfile implements memory.ProfileSettings with the learner's EnsureProfile.
type memoryProfile struct{ r *learnerRepo }

var _ memory.ProfileSettings = memoryProfile{}

// NewMemoryProfileSettings returns the memory.ProfileSettings over pool: it creates the learner
// profile with its skill states on first use (spec 12.3) and reads memory_enabled.
func NewMemoryProfileSettings(pool *pgxpool.Pool, ids idgen.Generator, c clock.Clock) memory.ProfileSettings {
	return memoryProfile{r: NewLearnerRepo(sqlcgen.New(pool), ids, c)}
}

func (p memoryProfile) EnsureMemoryEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	if err := p.r.Ensure(ctx, userID); err != nil {
		return false, err
	}
	prof, err := p.r.Get(ctx, userID)
	if err != nil {
		if errors.Is(err, learner.ErrNotFound) {
			return false, internalErr(err, "learner profile missing after ensure")
		}
		return false, err
	}
	return prof.MemoryEnabled, nil
}

// memoryPrompts implements memory.PromptSource over the content repository.
type memoryPrompts struct{ repo content.Repository }

var _ memory.PromptSource = memoryPrompts{}

// NewMemoryPromptSource returns the memory.PromptSource that reads the PUBLISHED version of a prompt.
func NewMemoryPromptSource(pool *pgxpool.Pool) memory.PromptSource {
	return memoryPrompts{repo: NewContentRepo(pool)}
}

func (p memoryPrompts) Published(ctx context.Context, key string) (prompt.Template, uuid.UUID, error) {
	pv, err := p.repo.GetPublishedPromptVersion(ctx, content.PromptKey(key))
	if errors.Is(err, content.ErrNotFound) {
		return prompt.Template{}, uuid.Nil, memory.ErrPromptNotFound
	}
	if err != nil {
		return prompt.Template{}, uuid.Nil, err
	}
	return pv.Template(), pv.ID, nil
}
