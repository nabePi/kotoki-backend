package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// memoryRepo implements memory.Repository (and so memory.Reader) over one sqlc handle. Every query
// is scoped by the user_id it is given (db/queries/memories.sql); none reads across users, and a
// row of another user is indistinguishable from a missing one (spec 11.10).
type memoryRepo struct {
	q     *sqlcgen.Queries
	ids   idgen.Generator
	clock clock.Clock
}

var _ memory.Repository = (*memoryRepo)(nil)

// NewMemoryRepo returns the memory repository over q (a pool or a transaction).
func NewMemoryRepo(q *sqlcgen.Queries, ids idgen.Generator, c clock.Clock) *memoryRepo { //nolint:revive // returned type is only used through the memory ports
	return &memoryRepo{q: q, ids: ids, clock: c}
}

// memoryTx adapts *Tx to memory.Tx: *Tx has no Memories accessor (the feature port is not part of
// the shared transaction type), but its Processed and Outbox already match.
type memoryTx struct{ t *Tx }

var _ memory.Tx = memoryTx{}

func (x memoryTx) Memories() memory.Repository {
	return NewMemoryRepo(x.t.Queries(), x.t.IDs(), x.t.Clock())
}
func (x memoryTx) Processed() events.ProcessedEvents { return x.t.Processed() }
func (x memoryTx) Outbox() events.OutboxWriter       { return x.t.Outbox() }

type memoryStoreView struct{ s *Store }

var _ memory.Store = memoryStoreView{}

// MemoryStore returns the memory.Store view of s.
func (s *Store) MemoryStore() memory.Store { return memoryStoreView{s: s} }

func (v memoryStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx memory.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, memoryTx{t: tx}) })
}

func (v memoryStoreView) Reader() memory.Reader {
	return NewMemoryRepo(v.s.Queries(), v.s.ids, v.s.clock)
}

// NewMemoryReader returns the non-transactional memory reader over pool.
func NewMemoryReader(pool *pgxpool.Pool) memory.Reader {
	return NewMemoryRepo(sqlcgen.New(pool), idgen.NewV7(), clock.System{})
}

func vecPtr(v []float32) *pgvector.Vector {
	if len(v) == 0 {
		return nil
	}
	x := pgvector.NewVector(v)
	return &x
}

func vecSlice(v *pgvector.Vector) []float32 {
	if v == nil {
		return nil
	}
	return v.Slice()
}

func turnIDs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{} // source_turn_ids is NOT NULL
	}
	return ids
}

// capCategories is the categories of a cap group and whether eviction orders by score (else by the
// oldest observation).
func capCategories(g memory.CapGroup) (cats []string, byScore bool) {
	switch g {
	case memory.CapFactPref:
		return []string{string(memory.CategoryProfileFact), string(memory.CategoryPreference)}, true
	case memory.CapLearning:
		return []string{string(memory.CategoryLearningPattern)}, true
	default:
		return []string{string(memory.CategoryEpisodic)}, false
	}
}

func (r *memoryRepo) LockUser(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.LockMemoryUser(ctx, userID.String()); err != nil {
		return internalErr(err, "lock memory user failed")
	}
	return nil
}

func (r *memoryRepo) TombstoneActive(ctx context.Context, userID uuid.UUID, cat memory.Category, key string, now time.Time) (bool, error) {
	ok, err := r.q.HasActiveTombstone(ctx, sqlcgen.HasActiveTombstoneParams{
		UserID: userID, Category: string(cat), MemoryKey: key, Now: tsz(now),
	})
	if err != nil {
		return false, internalErr(err, "check memory tombstone failed")
	}
	return ok, nil
}

func toMemory(m sqlcgen.Memory) memory.Memory {
	return memory.Memory{
		ID: m.ID, UserID: m.UserID, Category: memory.Category(m.Category), Key: m.MemoryKey,
		Content: m.Content, DisplayText: m.DisplayText, EvidenceType: m.EvidenceType,
		Confidence: numFloat(m.Confidence), Importance: numFloat(m.Importance), ObservationCount: int(m.ObservationCount),
		SourceSessionID: fromNullUUID(m.SourceSessionID), SourceTurnIDs: m.SourceTurnIds,
		ExtractionPromptVersionID: fromNullUUID(m.ExtractionPromptVersionID),
		Embedding:                 vecSlice(m.Embedding), EmbeddingModel: textOrEmpty(m.EmbeddingModel),
		Status: m.Status, SupersededBy: fromNullUUID(m.SupersededBy),
		FirstObservedAt: fromTsz(m.FirstObservedAt), LastObservedAt: fromTsz(m.LastObservedAt), ExpiresAt: fromTsz(m.ExpiresAt),
		CreatedAt: fromTsz(m.CreatedAt), UpdatedAt: fromTsz(m.UpdatedAt),
	}
}

func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func (r *memoryRepo) GetActiveSlotForUpdate(ctx context.Context, userID uuid.UUID, cat memory.Category, key string) (*memory.Memory, error) {
	row, err := r.q.GetActiveSlotForUpdate(ctx, sqlcgen.GetActiveSlotForUpdateParams{
		UserID: userID, Category: string(cat), MemoryKey: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // the port reports "no ACTIVE memory in the slot" as nil
	}
	if err != nil {
		return nil, internalErr(err, "get active memory slot failed")
	}
	m := toMemory(row)
	return &m, nil
}

func (r *memoryRepo) FindNearestActive(ctx context.Context, userID uuid.UUID, cat memory.Category, qvec []float32) (*memory.Nearest, error) {
	row, err := r.q.FindNearestActive(ctx, sqlcgen.FindNearestActiveParams{
		UserID: userID, Category: string(cat), Query: pgvector.NewVector(qvec),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // the port reports "no ACTIVE memory in the category" as nil
	}
	if err != nil {
		return nil, internalErr(err, "find nearest memory failed")
	}
	return &memory.Nearest{ID: row.ID, Key: row.MemoryKey, Similarity: row.Similarity}, nil
}

func (r *memoryRepo) Insert(ctx context.Context, userID uuid.UUID, m memory.Memory) error {
	if m.UserID != userID {
		return fmt.Errorf("postgres: memory insert for user %s carries user %s", userID, m.UserID)
	}
	now := r.clock.Now()
	created, updated := m.CreatedAt, m.UpdatedAt
	if created.IsZero() {
		created = now
	}
	if updated.IsZero() {
		updated = now
	}
	model := pgtype.Text{}
	if m.EmbeddingModel != "" {
		model = pgtype.Text{String: m.EmbeddingModel, Valid: true}
	}
	if err := r.q.InsertMemory(ctx, sqlcgen.InsertMemoryParams{
		ID: m.ID, UserID: userID, Category: string(m.Category), MemoryKey: m.Key,
		Content: m.Content, DisplayText: m.DisplayText, EvidenceType: m.EvidenceType,
		Confidence: num(m.Confidence, 3), Importance: num(m.Importance, 3), ObservationCount: int32(m.ObservationCount), //nolint:gosec // a small count
		SourceSessionID: nullUUID(m.SourceSessionID), SourceTurnIds: turnIDs(m.SourceTurnIDs),
		ExtractionPromptVersionID: nullUUID(m.ExtractionPromptVersionID),
		Embedding:                 vecPtr(m.Embedding), EmbeddingModel: model,
		Status: m.Status, SupersededBy: nullUUID(m.SupersededBy),
		FirstObservedAt: tsz(m.FirstObservedAt), LastObservedAt: tsz(m.LastObservedAt), ExpiresAt: tsz(m.ExpiresAt),
		CreatedAt: tsz(created), UpdatedAt: tsz(updated),
	}); err != nil {
		return internalErr(err, "insert memory failed")
	}
	return nil
}

func (r *memoryRepo) Reinforce(ctx context.Context, userID, id uuid.UUID, in memory.ReinforceInput) error {
	n, err := r.q.ReinforceMemory(ctx, sqlcgen.ReinforceMemoryParams{
		Confidence: num(in.Confidence, 3), Importance: num(in.Importance, 3), Now: tsz(in.Now), ExpiresAt: tsz(in.ExpiresAt),
		SourceSessionID: uuid.NullUUID{UUID: in.SourceSessionID, Valid: true}, SourceTurnIds: turnIDs(in.SourceTurnIDs),
		ExtractionPromptVersionID: nullUUID(in.PromptVersionID), UserID: userID, ID: id,
	})
	if err != nil {
		return internalErr(err, "reinforce memory failed")
	}
	if n == 0 {
		return memory.ErrNotFound
	}
	return nil
}

func (r *memoryRepo) Supersede(ctx context.Context, userID, oldID, newID uuid.UUID, expiresAt, now time.Time) error {
	n, err := r.q.SupersedeMemory(ctx, sqlcgen.SupersedeMemoryParams{
		NewID: uuid.NullUUID{UUID: newID, Valid: true}, ExpiresAt: tsz(expiresAt), Now: tsz(now), UserID: userID, ID: oldID,
	})
	if err != nil {
		return internalErr(err, "supersede memory failed")
	}
	if n == 0 {
		return memory.ErrNotFound
	}
	return nil
}

func (r *memoryRepo) CountActive(ctx context.Context, userID uuid.UUID, group memory.CapGroup) (int, error) {
	cats, _ := capCategories(group)
	n, err := r.q.CountActiveByGroup(ctx, sqlcgen.CountActiveByGroupParams{UserID: userID, Categories: cats})
	if err != nil {
		return 0, internalErr(err, "count active memories failed")
	}
	return int(n), nil
}

func (r *memoryRepo) DeleteLowest(ctx context.Context, userID uuid.UUID, group memory.CapGroup, n int) error {
	if n <= 0 {
		return nil
	}
	cats, byScore := capCategories(group)
	if _, err := r.q.DeleteLowestInGroup(ctx, sqlcgen.DeleteLowestInGroupParams{
		UserID: userID, Categories: cats, ByScore: byScore, N: int32(n), //nolint:gosec // a small count
	}); err != nil {
		return internalErr(err, "delete lowest memories failed")
	}
	return nil
}

func (r *memoryRepo) Delete(ctx context.Context, userID, id uuid.UUID, now time.Time) (bool, error) {
	m, err := r.q.GetActiveForUpdate(ctx, sqlcgen.GetActiveForUpdateParams{UserID: userID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, internalErr(err, "lock memory failed")
	}
	if _, err := r.q.DeleteSupersedeChain(ctx, sqlcgen.DeleteSupersedeChainParams{
		UserID: userID, ID: uuid.NullUUID{UUID: id, Valid: true},
	}); err != nil {
		return false, internalErr(err, "delete memory supersede chain failed")
	}
	if _, err := r.q.DeleteMemoryByID(ctx, sqlcgen.DeleteMemoryByIDParams{ID: id, UserID: userID}); err != nil {
		return false, internalErr(err, "delete memory failed")
	}
	if memory.Category(m.Category) == memory.CategoryEpisodic {
		return true, nil // an episode has no slot to protect
	}
	if err := r.q.UpsertTombstone(ctx, sqlcgen.UpsertTombstoneParams{
		ID: r.ids.New(), UserID: userID, Category: m.Category, MemoryKey: m.MemoryKey,
		Now: tsz(now), ExpiresAt: tsz(now.Add(memory.TombstoneTTL)),
	}); err != nil {
		return false, internalErr(err, "write memory tombstone failed")
	}
	return true, nil
}

func (r *memoryRepo) DeleteAll(ctx context.Context, userID uuid.UUID) error {
	if _, err := r.q.DeleteAllMemories(ctx, userID); err != nil {
		return internalErr(err, "delete all memories failed")
	}
	return nil
}

func (r *memoryRepo) ListCore(ctx context.Context, userID uuid.UUID, now time.Time) ([]memory.CoreRow, error) {
	rows, err := r.q.ListCoreCandidates(ctx, sqlcgen.ListCoreCandidatesParams{UserID: userID, Now: tsz(now)})
	if err != nil {
		return nil, internalErr(err, "list core memories failed")
	}
	out := make([]memory.CoreRow, 0, len(rows))
	for _, m := range rows {
		out = append(out, memory.CoreRow{
			ID: m.ID, Category: memory.Category(m.Category), Key: m.MemoryKey, Content: m.Content,
			Confidence: numFloat(m.Confidence), Importance: numFloat(m.Importance), ObservationCount: int(m.ObservationCount),
			LastObservedAt: fromTsz(m.LastObservedAt), Embedding: vecSlice(m.Embedding),
		})
	}
	return out, nil
}

func (r *memoryRepo) ListExisting(ctx context.Context, userID uuid.UUID, limit int, now time.Time) ([]memory.ExistingMemoryLine, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.q.ListExistingForPrompt(ctx, sqlcgen.ListExistingForPromptParams{
		UserID: userID, Now: tsz(now), MaxRows: int32(limit), //nolint:gosec // limit is a small count
	})
	if err != nil {
		return nil, internalErr(err, "list existing memories failed")
	}
	out := make([]memory.ExistingMemoryLine, 0, len(rows))
	for _, m := range rows {
		out = append(out, memory.NewExistingMemoryLine(memory.Category(m.Category), m.MemoryKey, m.Content))
	}
	return out, nil
}

func (r *memoryRepo) SearchEpisodic(ctx context.Context, userID uuid.UUID, qvec []float32, minSim float64, k int, exclude *uuid.UUID, now time.Time) ([]memory.EpisodicRow, error) {
	if k <= 0 {
		return nil, nil
	}
	rows, err := r.q.SearchEpisodic(ctx, sqlcgen.SearchEpisodicParams{
		UserID: userID, Query: pgvector.NewVector(qvec), MinSimilarity: minSim, K: int32(k), //nolint:gosec // k is a small limit
		Now: tsz(now), ExcludeSessionID: nullUUID(exclude),
	})
	if err != nil {
		return nil, internalErr(err, "search episodic memories failed")
	}
	out := make([]memory.EpisodicRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, memory.EpisodicRow{ID: e.ID, Content: e.Content, LastObservedAt: fromTsz(e.LastObservedAt), Similarity: e.Similarity})
	}
	return out, nil
}

func (r *memoryRepo) List(ctx context.Context, userID uuid.UUID, cat *memory.Category, after *memory.ListCursor, limit int, now time.Time) ([]memory.Memory, error) {
	if limit <= 0 {
		return nil, nil
	}
	p := sqlcgen.ListMemoriesParams{UserID: userID, Now: tsz(now), MaxRows: int32(limit)} //nolint:gosec // limit is a page size
	if cat != nil {
		p.Category = pgtype.Text{String: string(*cat), Valid: true}
	}
	if after != nil {
		p.AfterCategory = pgtype.Text{String: string(after.Category), Valid: true}
		p.AfterObservedAt = tsz(after.LastObservedAt)
		p.AfterID = uuid.NullUUID{UUID: after.ID, Valid: true}
	}
	rows, err := r.q.ListMemories(ctx, p)
	if err != nil {
		return nil, internalErr(err, "list memories failed")
	}
	out := make([]memory.Memory, 0, len(rows))
	for _, m := range rows {
		out = append(out, toMemory(sqlcgen.Memory{
			ID: m.ID, UserID: m.UserID, Category: m.Category, MemoryKey: m.MemoryKey, Content: m.Content,
			DisplayText: m.DisplayText, EvidenceType: m.EvidenceType, Confidence: m.Confidence, Importance: m.Importance,
			ObservationCount: m.ObservationCount, SourceSessionID: m.SourceSessionID, SourceTurnIds: m.SourceTurnIds,
			ExtractionPromptVersionID: m.ExtractionPromptVersionID, EmbeddingModel: m.EmbeddingModel, Status: m.Status,
			SupersededBy: m.SupersededBy, FirstObservedAt: m.FirstObservedAt, LastObservedAt: m.LastObservedAt,
			ExpiresAt: m.ExpiresAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		}))
	}
	return out, nil
}
