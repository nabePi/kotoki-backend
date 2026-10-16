package memory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// ErrNotFound is returned when a row does not exist for the user: another user's row is
// indistinguishable from a missing one (spec 11.10).
var ErrNotFound = errors.New("memory: not found")

// Memory statuses (memories.status).
const (
	StatusActive     = "ACTIVE"
	StatusSuperseded = "SUPERSEDED"
	StatusDeleted    = "DELETED"
)

// Evidence types of a stored memory.
const (
	EvidenceExplicit    = "EXPLICIT_USER_STATEMENT"
	EvidenceUserDeleted = "USER_DELETED"
)

// Memory is one memories row. A tombstone (StatusDeleted) has empty Content/DisplayText and no
// Embedding.
type Memory struct {
	ID, UserID                uuid.UUID
	Category                  Category
	Key                       string
	Content, DisplayText      string
	EvidenceType              string
	Confidence, Importance    float64
	ObservationCount          int
	SourceSessionID           *uuid.UUID
	SourceTurnIDs             []uuid.UUID
	ExtractionPromptVersionID *uuid.UUID
	Embedding                 []float32
	EmbeddingModel            string
	Status                    string
	SupersededBy              *uuid.UUID
	FirstObservedAt           time.Time
	LastObservedAt            time.Time
	ExpiresAt                 time.Time
	CreatedAt, UpdatedAt      time.Time
}

// Nearest is the nearest ACTIVE memory of a category to a query vector.
type Nearest struct {
	ID         uuid.UUID
	Key        string
	Similarity float64 // 1 - cosine distance
}

// ReinforceInput is what REINFORCE (spec 11.6) applies to an ACTIVE memory: the repository adds one
// observation, sets confidence = ReinforcedConfidence(old, Confidence), importance =
// max(old, Importance), expires_at = max(old, ExpiresAt) and replaces the source columns. Content
// and embedding are never touched.
type ReinforceInput struct {
	Confidence, Importance float64 // of the candidate
	ExpiresAt              time.Time
	SourceSessionID        uuid.UUID
	SourceTurnIDs          []uuid.UUID
	PromptVersionID        *uuid.UUID
	Now                    time.Time
}

// CapGroup is a group of categories that share one cap of ACTIVE memories (spec 11.8.2).
type CapGroup string

// Cap groups.
const (
	CapFactPref CapGroup = "FACT_PREF"        // PROFILE_FACT + PREFERENCE
	CapLearning CapGroup = "LEARNING_PATTERN" // LEARNING_PATTERN
	CapEpisodic CapGroup = "EPISODIC"         // EPISODIC
)

// CoreRow is a retrieval candidate of the core block (spec 11.7.4 ListCoreCandidates).
type CoreRow struct {
	ID               uuid.UUID
	Category         Category
	Key, Content     string
	Confidence       float64
	Importance       float64
	ObservationCount int
	LastObservedAt   time.Time
	Embedding        []float32
}

// EpisodicRow is one episodic search hit.
type EpisodicRow struct {
	ID             uuid.UUID
	Content        string
	LastObservedAt time.Time
	Similarity     float64
}

// ListCursor is the keyset position of List, ordered (category, last_observed_at DESC, id): the
// next page holds the rows strictly after it.
type ListCursor struct {
	Category       Category
	LastObservedAt time.Time
	ID             uuid.UUID
}

// Reader is the non-transactional read side.
type Reader interface {
	// ListCore returns the ACTIVE, unexpired PROFILE_FACT/PREFERENCE/LEARNING_PATTERN rows of the
	// user, newest observation first, at most 200.
	ListCore(ctx context.Context, userID uuid.UUID, now time.Time) ([]CoreRow, error)
	// SearchEpisodic returns at most k ACTIVE, unexpired episodes of the user with similarity >=
	// minSim, nearest first, skipping those of the session exclude.
	SearchEpisodic(ctx context.Context, userID uuid.UUID, qvec []float32, minSim float64, k int, exclude *uuid.UUID, now time.Time) ([]EpisodicRow, error)
	// List returns at most limit ACTIVE, unexpired memories of the user (all categories, or cat),
	// ordered (category, last_observed_at DESC, id), strictly after the cursor. The embedding is
	// not loaded (Memory.Embedding is nil).
	List(ctx context.Context, userID uuid.UUID, cat *Category, after *ListCursor, limit int, now time.Time) ([]Memory, error)
}

// Repository is the memory port; every method takes the user it acts for, and a row of another
// user is indistinguishable from a missing one (spec 11.10). It is used inside a Tx.
type Repository interface {
	Reader

	// TombstoneActive reports whether the user deleted the slot less than 90 days ago.
	TombstoneActive(ctx context.Context, userID uuid.UUID, cat Category, key string, now time.Time) (bool, error)
	// GetActiveSlotForUpdate returns the ACTIVE memory of the slot, locked; nil when none.
	GetActiveSlotForUpdate(ctx context.Context, userID uuid.UUID, cat Category, key string) (*Memory, error)
	// FindNearestActive returns the ACTIVE memory of the category nearest to qvec; nil when none.
	FindNearestActive(ctx context.Context, userID uuid.UUID, cat Category, qvec []float32) (*Nearest, error)
	// Insert stores m; at most one ACTIVE row per (user, category, key).
	Insert(ctx context.Context, userID uuid.UUID, m Memory) error
	// Reinforce applies in to the ACTIVE memory id; an error wrapping ErrNotFound when none.
	Reinforce(ctx context.Context, userID, id uuid.UUID, in ReinforceInput) error
	// Supersede marks the ACTIVE memory oldID SUPERSEDED by newID (which Insert stores afterwards,
	// in the same transaction) and sets its expires_at.
	Supersede(ctx context.Context, userID, oldID, newID uuid.UUID, expiresAt, now time.Time) error
	// CountActive counts the ACTIVE memories of the cap group.
	CountActive(ctx context.Context, userID uuid.UUID, group CapGroup) (int, error)
	// DeleteLowest hard-deletes the n lowest ACTIVE memories of the group (spec 11.8.2).
	DeleteLowest(ctx context.Context, userID uuid.UUID, group CapGroup, n int) error
	// Delete removes the ACTIVE memory id with the supersede chain behind it and, except for
	// EPISODIC, leaves a 90-day tombstone of its slot. false when it is not an ACTIVE memory of
	// the user.
	Delete(ctx context.Context, userID, id uuid.UUID, now time.Time) (bool, error)
	// DeleteAll removes every memory of the user, tombstones included, and creates no tombstone.
	DeleteAll(ctx context.Context, userID uuid.UUID) error
	// LockUser takes the transaction-scoped advisory lock that serialises memory writes of a user.
	LockUser(ctx context.Context, userID uuid.UUID) error
}

// Tx is the transactional port of the memory feature.
type Tx interface {
	Memories() Repository
	Processed() events.ProcessedEvents
	Outbox() events.OutboxWriter
}

// Store runs memory transactions and hands out the plain reader.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	Reader() Reader
}
