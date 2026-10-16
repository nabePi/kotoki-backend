package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// Dedup, retention and cap constants (spec 11.6, 11.8).
const (
	SimDedup = 0.90

	ReinforceBonus   = 0.050
	ReinforceCeiling = 0.990

	TTLProfileFact     = 730 * day
	TTLPreference      = 365 * day
	TTLLearningPattern = 180 * day
	TTLEpisodic        = 180 * day
	SupersededTTL      = 30 * day
	TombstoneTTL       = 90 * day

	CapFactPrefMax = 100
	CapLearningMax = 40
	CapEpisodicMax = 200

	// ReasonUserDeletedSlot rejects a candidate whose slot the user deleted (tombstone).
	ReasonUserDeletedSlot = "USER_DELETED_SLOT"

	day = 24 * time.Hour
)

// ErrNoEmbedding is returned for a candidate that reaches ApplyCandidate without an embedding.
var ErrNoEmbedding = errors.New("memory: candidate has no embedding")

// Action is the outcome of ApplyCandidate; its value is the label of kotoki_memory_apply_total.
type Action string

// Outcomes.
const (
	ActionCreated    Action = "created"
	ActionReinforced Action = "reinforced"
	ActionSuperseded Action = "superseded"
	ActionRejected   Action = "rejected"
)

// ReinforcedConfidence is the confidence of a reinforced memory: LEAST(0.990, GREATEST(old, new) +
// 0.050) (spec 11.6).
func ReinforcedConfidence(old, candidate float64) float64 {
	return math.Min(ReinforceCeiling, math.Max(old, candidate)+ReinforceBonus)
}

// TTLFor is the expires_at of a new or reinforced memory made from c at now (spec 11.8.1).
func TTLFor(c Candidate, now time.Time) time.Time {
	switch c.Category {
	case CategoryProfileFact:
		if c.IsTemporary {
			days := DefaultValidDays
			if c.ValidDays != nil {
				days = *c.ValidDays
			}
			days = min(MaxValidDays, max(1, days))
			return now.Add(time.Duration(days) * day)
		}
		return now.Add(TTLProfileFact)
	case CategoryPreference:
		return now.Add(TTLPreference)
	case CategoryLearningPattern:
		return now.Add(TTLLearningPattern)
	default:
		return now.Add(TTLEpisodic)
	}
}

// Applier decides what a validated candidate does to the user's memories (spec 11.6) and keeps
// them within the caps (spec 11.8.2). It holds no state of its own.
type Applier struct {
	ids idgen.Generator
	clk clock.Clock
	m   *Metrics
}

// NewApplier returns an Applier; m may be nil.
func NewApplier(ids idgen.Generator, clk clock.Clock, m *Metrics) *Applier {
	return &Applier{ids: ids, clk: clk, m: m}
}

// Apply stores one candidate inside tx, in the order of spec 11.6: tombstone, same slot, nearest
// memory of the category, insert. The caller holds the user lock (ApplyAll takes it).
func (a *Applier) Apply(ctx context.Context, tx Tx, userID uuid.UUID, c Candidate) (Action, error) {
	act, err := a.apply(ctx, tx.Memories(), userID, c)
	if err != nil {
		return "", err
	}
	a.m.Applied(act)
	return act, nil
}

func (a *Applier) apply(ctx context.Context, repo Repository, userID uuid.UUID, c Candidate) (Action, error) {
	if len(c.Embedding) == 0 {
		return "", ErrNoEmbedding
	}
	now := a.clk.Now().UTC()
	expires := TTLFor(c, now)

	deleted, err := repo.TombstoneActive(ctx, userID, c.Category, c.Key, now)
	if err != nil {
		return "", fmt.Errorf("memory: tombstone: %w", err)
	}
	if deleted {
		a.m.Rejected(ReasonUserDeletedSlot)
		return ActionRejected, nil
	}

	slot, err := repo.GetActiveSlotForUpdate(ctx, userID, c.Category, c.Key)
	if err != nil {
		return "", fmt.Errorf("memory: slot: %w", err)
	}
	if slot != nil {
		sim := 0.0 // a slot without embedding counts as different
		if len(slot.Embedding) > 0 {
			sim = Cosine(c.Embedding, slot.Embedding)
		}
		if sim >= SimDedup {
			return ActionReinforced, a.reinforce(ctx, repo, userID, slot.ID, c, expires, now)
		}
		return ActionSuperseded, a.supersede(ctx, repo, userID, slot.ID, c, expires, now)
	}

	near, err := repo.FindNearestActive(ctx, userID, c.Category, c.Embedding)
	if err != nil {
		return "", fmt.Errorf("memory: nearest: %w", err)
	}
	if near != nil && near.Similarity >= SimDedup {
		if c.Category == CategoryEpisodic {
			return ActionSuperseded, a.supersede(ctx, repo, userID, near.ID, c, expires, now)
		}
		return ActionReinforced, a.reinforce(ctx, repo, userID, near.ID, c, expires, now) // the old key stays
	}

	if err := repo.Insert(ctx, userID, newMemory(a.ids.New(), userID, c, expires, now)); err != nil {
		return "", fmt.Errorf("memory: insert: %w", err)
	}
	return ActionCreated, nil
}

func (a *Applier) reinforce(ctx context.Context, repo Repository, userID, id uuid.UUID, c Candidate, expires, now time.Time) error {
	err := repo.Reinforce(ctx, userID, id, ReinforceInput{
		Confidence: c.Confidence, Importance: c.Importance, ExpiresAt: expires,
		SourceSessionID: c.SessionID, SourceTurnIDs: c.TurnIDs, PromptVersionID: c.PromptVersionID, Now: now,
	})
	if err != nil {
		return fmt.Errorf("memory: reinforce: %w", err)
	}
	return nil
}

// supersede retires old and inserts the candidate. The UPDATE goes first so the unique index of
// the ACTIVE slot is free for the INSERT.
func (a *Applier) supersede(ctx context.Context, repo Repository, userID, oldID uuid.UUID, c Candidate, expires, now time.Time) error {
	newID := a.ids.New()
	if err := repo.Supersede(ctx, userID, oldID, newID, now.Add(SupersededTTL), now); err != nil {
		return fmt.Errorf("memory: supersede: %w", err)
	}
	if err := repo.Insert(ctx, userID, newMemory(newID, userID, c, expires, now)); err != nil {
		return fmt.Errorf("memory: insert: %w", err)
	}
	return nil
}

func newMemory(id, userID uuid.UUID, c Candidate, expires, now time.Time) Memory {
	session := c.SessionID
	m := Memory{
		ID: id, UserID: userID, Category: c.Category, Key: c.Key,
		Content: c.ContentJA, DisplayText: c.DisplayID, EvidenceType: c.EvidenceType,
		Confidence: c.Confidence, Importance: c.Importance, ObservationCount: 1,
		SourceTurnIDs: slices.Clone(c.TurnIDs), ExtractionPromptVersionID: c.PromptVersionID,
		Embedding: slices.Clone(c.Embedding), EmbeddingModel: c.EmbeddingModel, Status: StatusActive,
		FirstObservedAt: now, LastObservedAt: now, ExpiresAt: expires, CreatedAt: now, UpdatedAt: now,
	}
	if session != uuid.Nil {
		m.SourceSessionID = &session
	}
	return m
}

// ApplyAll applies the candidates of one extraction in the order of spec 11.6 (PROFILE_FACT,
// PREFERENCE, LEARNING_PATTERN, EPISODIC; confidence descending within a category) under the user
// lock, then enforces the caps. It returns the outcome per candidate in that order.
func (a *Applier) ApplyAll(ctx context.Context, tx Tx, userID uuid.UUID, cands []Candidate) ([]Action, error) {
	if err := tx.Memories().LockUser(ctx, userID); err != nil {
		return nil, fmt.Errorf("memory: lock user: %w", err)
	}
	ordered := slices.Clone(cands)
	slices.SortStableFunc(ordered, func(x, y Candidate) int {
		if d := cmp.Compare(categoryOrder[x.Category], categoryOrder[y.Category]); d != 0 {
			return d
		}
		return cmp.Compare(y.Confidence, x.Confidence)
	})
	out := make([]Action, 0, len(ordered))
	for _, c := range ordered {
		act, err := a.Apply(ctx, tx, userID, c)
		if err != nil {
			return nil, err
		}
		out = append(out, act)
	}
	if err := a.EnforceCaps(ctx, tx, userID); err != nil {
		return nil, err
	}
	return out, nil
}

// EnforceCaps deletes the lowest ACTIVE memories of every group that is over its cap (spec 11.8.2).
func (a *Applier) EnforceCaps(ctx context.Context, tx Tx, userID uuid.UUID) error {
	repo := tx.Memories()
	for _, g := range []struct {
		group CapGroup
		limit int
	}{{CapFactPref, CapFactPrefMax}, {CapLearning, CapLearningMax}, {CapEpisodic, CapEpisodicMax}} {
		n, err := repo.CountActive(ctx, userID, g.group)
		if err != nil {
			return fmt.Errorf("memory: count %s: %w", g.group, err)
		}
		if n > g.limit {
			if err := repo.DeleteLowest(ctx, userID, g.group, n-g.limit); err != nil {
				return fmt.Errorf("memory: cap %s: %w", g.group, err)
			}
		}
	}
	return nil
}

// Cosine is the cosine similarity of a and b; 0 when the dimensions differ or a vector is zero.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
