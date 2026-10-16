package fakes

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// MemoryStore is an in-memory memory.Store with real cosine similarity. WithinTx serialises
// callers (it doubles as the advisory lock) and restores the rows when fn fails. Every row is
// keyed by its user and no method reads across users. Like ux_memories_active_slot and
// ux_memories_tombstone_slot, Insert rejects a second ACTIVE (or DELETED) row of a slot.
type MemoryStore struct {
	FailInjector
	// Processed records the idempotency rows of committed transactions; Outbox the events of
	// committed ones.
	Processed *Processed
	Outbox    *Outbox

	mu    sync.Mutex
	clock clock.Clock
	rows  map[uuid.UUID]map[uuid.UUID]memory.Memory // user -> memory id
	ops   []string
}

var _ memory.Store = (*MemoryStore)(nil)

// MemoryOption customises NewMemoryStore.
type MemoryOption func(*MemoryStore)

// WithMemoryClock sets the clock behind created_at and updated_at of tombstones.
func WithMemoryClock(c clock.Clock) MemoryOption { return func(s *MemoryStore) { s.clock = c } }

// NewMemoryStore returns an empty store.
func NewMemoryStore(opts ...MemoryOption) *MemoryStore {
	s := &MemoryStore{
		Processed: NewProcessed(), Outbox: &Outbox{}, clock: clock.System{},
		rows: map[uuid.UUID]map[uuid.UUID]memory.Memory{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Put stores m as it is, bypassing every rule (test setup).
func (s *MemoryStore) Put(m memory.Memory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows[m.UserID] == nil {
		s.rows[m.UserID] = map[uuid.UUID]memory.Memory{}
	}
	s.rows[m.UserID][m.ID] = cloneMemory(m)
}

// Get returns the row id of userID whatever its status.
func (s *MemoryStore) Get(userID, id uuid.UUID) (memory.Memory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.rows[userID][id]
	return cloneMemory(m), ok
}

// All returns every row of userID (any status) ordered by (category, key, status, id).
func (s *MemoryStore) All(userID uuid.UUID) []memory.Memory {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]memory.Memory, 0, len(s.rows[userID]))
	for _, m := range s.rows[userID] {
		out = append(out, cloneMemory(m))
	}
	slices.SortFunc(out, func(a, b memory.Memory) int {
		return strings.Compare(fmt.Sprint(a.Category, a.Key, a.Status, a.ID), fmt.Sprint(b.Category, b.Key, b.Status, b.ID))
	})
	return out
}

// Ops returns the writes of the repository in the order they were made ("supersede", "insert",
// "reinforce", ...), each followed by the id they act on.
func (s *MemoryStore) Ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ops)
}

func cloneMemory(m memory.Memory) memory.Memory {
	m.SourceTurnIDs = slices.Clone(m.SourceTurnIDs)
	m.Embedding = slices.Clone(m.Embedding)
	return m
}

// WithinTx implements memory.Store.
func (s *MemoryStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx memory.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := make(map[uuid.UUID]map[uuid.UUID]memory.Memory, len(s.rows))
	for u, m := range s.rows {
		c := make(map[uuid.UUID]memory.Memory, len(m))
		for id, r := range m {
			c[id] = cloneMemory(r)
		}
		before[u] = c
	}
	opsBefore, outboxBefore := len(s.ops), s.Outbox.Len()
	tx := &memoryTx{s: s}
	if err := fn(ctx, tx); err != nil {
		s.rows = before
		s.ops = s.ops[:opsBefore]
		s.Outbox.Truncate(outboxBefore)
		for _, k := range tx.marked {
			s.Processed.Forget(k.consumer, k.id)
		}
		return err
	}
	return nil
}

// Reader implements memory.Store.
func (s *MemoryStore) Reader() memory.Reader { return memoryReader{s: s} }

type memoryTx struct {
	s      *MemoryStore
	marked []processedKey2
}

func (t *memoryTx) Memories() memory.Repository       { return &memoryRepo{s: t.s} }
func (t *memoryTx) Processed() events.ProcessedEvents { return &memoryProcessed{tx: t} }
func (t *memoryTx) Outbox() events.OutboxWriter       { return t.s.Outbox }

type memoryProcessed struct{ tx *memoryTx }

func (p *memoryProcessed) MarkProcessed(ctx context.Context, consumer string, ev events.Envelope) error {
	if err := p.tx.s.Processed.MarkProcessed(ctx, consumer, ev); err != nil {
		return err
	}
	p.tx.marked = append(p.tx.marked, processedKey2{consumer, ev.EventID})
	return nil
}

// memoryReader reads outside a transaction: it takes the store lock itself.
type memoryReader struct{ s *MemoryStore }

func (r memoryReader) ListCore(ctx context.Context, userID uuid.UUID, now time.Time) ([]memory.CoreRow, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return (&memoryRepo{s: r.s}).ListCore(ctx, userID, now)
}

func (r memoryReader) SearchEpisodic(ctx context.Context, userID uuid.UUID, qvec []float32, minSim float64, k int, exclude *uuid.UUID, now time.Time) ([]memory.EpisodicRow, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return (&memoryRepo{s: r.s}).SearchEpisodic(ctx, userID, qvec, minSim, k, exclude, now)
}

func (r memoryReader) List(ctx context.Context, userID uuid.UUID, cat *memory.Category, after *memory.ListCursor, limit int, now time.Time) ([]memory.Memory, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return (&memoryRepo{s: r.s}).List(ctx, userID, cat, after, limit, now)
}

// memoryRepo implements memory.Repository; it runs with the store lock held.
type memoryRepo struct{ s *MemoryStore }

var _ memory.Repository = (*memoryRepo)(nil)

func (r *memoryRepo) userRows(userID uuid.UUID) map[uuid.UUID]memory.Memory { return r.s.rows[userID] }

func (r *memoryRepo) record(op string, id uuid.UUID) { r.s.ops = append(r.s.ops, op+":"+id.String()) }

func inGroup(g memory.CapGroup, c memory.Category) bool {
	switch g {
	case memory.CapFactPref:
		return c == memory.CategoryProfileFact || c == memory.CategoryPreference
	case memory.CapLearning:
		return c == memory.CategoryLearningPattern
	default:
		return c == memory.CategoryEpisodic
	}
}

func (r *memoryRepo) TombstoneActive(_ context.Context, userID uuid.UUID, cat memory.Category, key string, now time.Time) (bool, error) {
	if err := r.s.Take("TombstoneActive"); err != nil {
		return false, err
	}
	for _, m := range r.userRows(userID) {
		if m.Status == memory.StatusDeleted && m.Category == cat && m.Key == key && m.ExpiresAt.After(now) {
			return true, nil
		}
	}
	return false, nil
}

func (r *memoryRepo) GetActiveSlotForUpdate(_ context.Context, userID uuid.UUID, cat memory.Category, key string) (*memory.Memory, error) {
	if err := r.s.Take("GetActiveSlotForUpdate"); err != nil {
		return nil, err
	}
	for _, m := range r.userRows(userID) {
		if m.Status == memory.StatusActive && m.Category == cat && m.Key == key {
			c := cloneMemory(m)
			return &c, nil
		}
	}
	return nil, nil
}

func (r *memoryRepo) FindNearestActive(_ context.Context, userID uuid.UUID, cat memory.Category, qvec []float32) (*memory.Nearest, error) {
	if err := r.s.Take("FindNearestActive"); err != nil {
		return nil, err
	}
	var best *memory.Nearest
	for _, id := range slices.SortedFunc(maps.Keys(r.userRows(userID)), compareUUID) {
		m := r.userRows(userID)[id]
		if m.Status != memory.StatusActive || m.Category != cat || len(m.Embedding) == 0 {
			continue
		}
		if sim := memory.Cosine(qvec, m.Embedding); best == nil || sim > best.Similarity {
			best = &memory.Nearest{ID: m.ID, Key: m.Key, Similarity: sim}
		}
	}
	return best, nil
}

func (r *memoryRepo) Insert(_ context.Context, userID uuid.UUID, m memory.Memory) error {
	if err := r.s.Take("Insert"); err != nil {
		return err
	}
	if m.UserID != userID {
		return fmt.Errorf("fakes: memory insert for user %s carries user %s", userID, m.UserID)
	}
	rows := r.userRows(userID)
	if _, dup := rows[m.ID]; dup {
		return fmt.Errorf("fakes: duplicate memory id %s", m.ID)
	}
	if m.Status == memory.StatusActive || m.Status == memory.StatusDeleted {
		for _, o := range rows {
			if o.Status == m.Status && o.Category == m.Category && o.Key == m.Key {
				return fmt.Errorf("fakes: unique violation: %s slot (%s, %s) of user %s", m.Status, m.Category, m.Key, userID)
			}
		}
	}
	if rows == nil {
		r.s.rows[userID] = map[uuid.UUID]memory.Memory{}
	}
	r.s.rows[userID][m.ID] = cloneMemory(m)
	r.record("insert", m.ID)
	return nil
}

func (r *memoryRepo) Reinforce(_ context.Context, userID, id uuid.UUID, in memory.ReinforceInput) error {
	if err := r.s.Take("Reinforce"); err != nil {
		return err
	}
	m, ok := r.userRows(userID)[id]
	if !ok || m.Status != memory.StatusActive {
		return memory.ErrNotFound
	}
	m.ObservationCount++
	m.Confidence = memory.ReinforcedConfidence(m.Confidence, in.Confidence)
	m.Importance = max(m.Importance, in.Importance)
	m.LastObservedAt = in.Now
	if in.ExpiresAt.After(m.ExpiresAt) {
		m.ExpiresAt = in.ExpiresAt
	}
	session := in.SourceSessionID
	m.SourceSessionID = &session
	m.SourceTurnIDs = slices.Clone(in.SourceTurnIDs)
	if in.PromptVersionID != nil {
		pv := *in.PromptVersionID
		m.ExtractionPromptVersionID = &pv
	}
	m.UpdatedAt = in.Now
	r.s.rows[userID][id] = m
	r.record("reinforce", id)
	return nil
}

func (r *memoryRepo) Supersede(_ context.Context, userID, oldID, newID uuid.UUID, expiresAt, now time.Time) error {
	if err := r.s.Take("Supersede"); err != nil {
		return err
	}
	m, ok := r.userRows(userID)[oldID]
	if !ok || m.Status != memory.StatusActive {
		return memory.ErrNotFound
	}
	m.Status = memory.StatusSuperseded
	m.SupersededBy = &newID
	m.ExpiresAt = expiresAt
	m.UpdatedAt = now
	r.s.rows[userID][oldID] = m
	r.record("supersede", oldID)
	return nil
}

func (r *memoryRepo) active(userID uuid.UUID, g memory.CapGroup) []memory.Memory {
	var out []memory.Memory
	for _, m := range r.userRows(userID) {
		if m.Status == memory.StatusActive && inGroup(g, m.Category) {
			out = append(out, m)
		}
	}
	return out
}

func (r *memoryRepo) CountActive(_ context.Context, userID uuid.UUID, g memory.CapGroup) (int, error) {
	if err := r.s.Take("CountActive"); err != nil {
		return 0, err
	}
	return len(r.active(userID, g)), nil
}

func (r *memoryRepo) DeleteLowest(_ context.Context, userID uuid.UUID, g memory.CapGroup, n int) error {
	if err := r.s.Take("DeleteLowest"); err != nil {
		return err
	}
	rows := r.active(userID, g)
	// importance and confidence are NUMERIC(4,3) in PostgreSQL: score in thousandths so that equal
	// stored values tie exactly instead of differing by float noise.
	score := func(m memory.Memory) int {
		if g == memory.CapEpisodic {
			return 0 // the oldest observation goes first
		}
		return 6*int(math.Round(m.Importance*1000)) + 4*int(math.Round(m.Confidence*1000))
	}
	slices.SortFunc(rows, func(a, b memory.Memory) int {
		if sa, sb := score(a), score(b); sa != sb {
			return sa - sb
		}
		if c := a.LastObservedAt.Compare(b.LastObservedAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	for _, m := range rows[:min(n, len(rows))] {
		r.detach(userID, m.ID)
		delete(r.s.rows[userID], m.ID)
		r.record("delete_lowest", m.ID)
	}
	return nil
}

// detach clears superseded_by pointers to id (ON DELETE SET NULL).
func (r *memoryRepo) detach(userID, id uuid.UUID) {
	for k, m := range r.userRows(userID) {
		if m.SupersededBy != nil && *m.SupersededBy == id {
			m.SupersededBy = nil
			r.s.rows[userID][k] = m
		}
	}
}

func (r *memoryRepo) Delete(_ context.Context, userID, id uuid.UUID, now time.Time) (bool, error) {
	if err := r.s.Take("Delete"); err != nil {
		return false, err
	}
	m, ok := r.userRows(userID)[id]
	if !ok || m.Status != memory.StatusActive {
		return false, nil
	}
	chain := map[uuid.UUID]bool{}
	for frontier := []uuid.UUID{id}; len(frontier) > 0; {
		var next []uuid.UUID
		for _, f := range frontier {
			for _, o := range r.userRows(userID) {
				if o.SupersededBy != nil && *o.SupersededBy == f && !chain[o.ID] {
					chain[o.ID] = true
					next = append(next, o.ID)
				}
			}
		}
		frontier = next
	}
	for cid := range chain {
		delete(r.s.rows[userID], cid)
	}
	r.detach(userID, id)
	delete(r.s.rows[userID], id)
	r.record("delete", id)
	if m.Category == memory.CategoryEpisodic {
		return true, nil
	}
	expires := now.Add(memory.TombstoneTTL)
	for k, o := range r.userRows(userID) {
		if o.Status == memory.StatusDeleted && o.Category == m.Category && o.Key == m.Key {
			o.ExpiresAt, o.UpdatedAt = expires, now
			r.s.rows[userID][k] = o
			return true, nil
		}
	}
	tid, err := uuid.NewV7()
	if err != nil {
		return false, err
	}
	r.s.rows[userID][tid] = memory.Memory{
		ID: tid, UserID: userID, Category: m.Category, Key: m.Key, EvidenceType: memory.EvidenceUserDeleted,
		ObservationCount: 1, Status: memory.StatusDeleted,
		FirstObservedAt: now, LastObservedAt: now, ExpiresAt: expires, CreatedAt: now, UpdatedAt: now,
	}
	return true, nil
}

func (r *memoryRepo) DeleteAll(_ context.Context, userID uuid.UUID) error {
	if err := r.s.Take("DeleteAll"); err != nil {
		return err
	}
	delete(r.s.rows, userID)
	r.s.ops = append(r.s.ops, "delete_all:"+userID.String())
	return nil
}

func (r *memoryRepo) LockUser(context.Context, uuid.UUID) error { return r.s.Take("LockUser") }

func (r *memoryRepo) ListCore(_ context.Context, userID uuid.UUID, now time.Time) ([]memory.CoreRow, error) {
	if err := r.s.Take("ListCore"); err != nil {
		return nil, err
	}
	var rows []memory.Memory
	for _, m := range r.userRows(userID) {
		if m.Status == memory.StatusActive && m.Category != memory.CategoryEpisodic && m.ExpiresAt.After(now) {
			rows = append(rows, m)
		}
	}
	slices.SortFunc(rows, func(a, b memory.Memory) int {
		if c := b.LastObservedAt.Compare(a.LastObservedAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	out := make([]memory.CoreRow, 0, min(len(rows), 200))
	for _, m := range rows[:min(len(rows), 200)] {
		out = append(out, memory.CoreRow{
			ID: m.ID, Category: m.Category, Key: m.Key, Content: m.Content, Confidence: m.Confidence,
			Importance: m.Importance, ObservationCount: m.ObservationCount, LastObservedAt: m.LastObservedAt,
			Embedding: slices.Clone(m.Embedding),
		})
	}
	return out, nil
}

func (r *memoryRepo) SearchEpisodic(_ context.Context, userID uuid.UUID, qvec []float32, minSim float64, k int, exclude *uuid.UUID, now time.Time) ([]memory.EpisodicRow, error) {
	if err := r.s.Take("SearchEpisodic"); err != nil {
		return nil, err
	}
	var out []memory.EpisodicRow
	for _, m := range r.userRows(userID) {
		if m.Status != memory.StatusActive || m.Category != memory.CategoryEpisodic || !m.ExpiresAt.After(now) || len(m.Embedding) == 0 {
			continue
		}
		if exclude != nil && m.SourceSessionID != nil && *m.SourceSessionID == *exclude {
			continue
		}
		if sim := memory.Cosine(qvec, m.Embedding); sim >= minSim {
			out = append(out, memory.EpisodicRow{ID: m.ID, Content: m.Content, LastObservedAt: m.LastObservedAt, Similarity: sim})
		}
	}
	slices.SortFunc(out, func(a, b memory.EpisodicRow) int {
		if a.Similarity != b.Similarity {
			if a.Similarity > b.Similarity {
				return -1
			}
			return 1
		}
		return compareUUID(a.ID, b.ID)
	})
	return out[:min(k, len(out))], nil
}

// cmpList orders like the SQL of List: category (text), last_observed_at DESC, id.
func cmpList(a, b memory.Memory) int {
	if c := strings.Compare(string(a.Category), string(b.Category)); c != 0 {
		return c
	}
	if c := b.LastObservedAt.Compare(a.LastObservedAt); c != 0 {
		return c
	}
	return compareUUID(a.ID, b.ID)
}

func (r *memoryRepo) List(_ context.Context, userID uuid.UUID, cat *memory.Category, after *memory.ListCursor, limit int, now time.Time) ([]memory.Memory, error) {
	if err := r.s.Take("List"); err != nil {
		return nil, err
	}
	var rows []memory.Memory
	for _, m := range r.userRows(userID) {
		if m.Status != memory.StatusActive || !m.ExpiresAt.After(now) || (cat != nil && m.Category != *cat) {
			continue
		}
		if after != nil && cmpList(m, memory.Memory{Category: after.Category, LastObservedAt: after.LastObservedAt, ID: after.ID}) <= 0 {
			continue
		}
		m = cloneMemory(m)
		m.Embedding = nil // List never loads the embedding, like the SQL
		rows = append(rows, m)
	}
	slices.SortFunc(rows, cmpList)
	return rows[:min(limit, len(rows))], nil
}
