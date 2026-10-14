package content

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// VersionState is the row a publish/retire/delete decision is made on (locked FOR UPDATE).
type VersionState struct {
	ID      uuid.UUID
	Version int
	Status  VersionStatus
}

// TxRepository is the transactional side of content: the reads of Repository (same names, run
// inside the transaction) plus the writes the admin lifecycle needs. Lock order: parent first,
// then version (spec 6.13.1).
type TxRepository interface {
	Repository

	// LockParent locks the parent of a versioned family and returns its id: the scenario id, or
	// the prompt_configs id of the key. ErrNotFound when it does not exist.
	LockParent(ctx context.Context, fam Family, parent string) (uuid.UUID, error)
	// LockVersion locks one version under the parent; ErrNotFound when the version does not
	// belong to that parent.
	LockVersion(ctx context.Context, fam Family, parentID, versionID uuid.UUID) (VersionState, error)
	// NextVersionNumber is max(version)+1 of the parent; call it under LockParent.
	NextVersionNumber(ctx context.Context, fam Family, parentID uuid.UUID) (int, error)
	InsertScenarioVersion(ctx context.Context, v ScenarioVersion) error // always inserted as DRAFT
	InsertPromptVersion(ctx context.Context, v PromptVersion) error     // always inserted as DRAFT
	// RetirePublished retires the PUBLISHED version of the parent and returns its id (nil if none).
	RetirePublished(ctx context.Context, fam Family, parentID uuid.UUID, at time.Time) (*uuid.UUID, error)
	SetPublished(ctx context.Context, fam Family, versionID uuid.UUID, at time.Time) error
	SetRetired(ctx context.Context, fam Family, versionID uuid.UUID, at time.Time) error
	DeleteDraft(ctx context.Context, fam Family, versionID uuid.UUID) error
	// SetEntityStatus changes the status of a topic ("topics") or scenario ("scenarios").
	SetEntityStatus(ctx context.Context, entity string, id uuid.UUID, to EntityStatus, at time.Time) error
	LockCharacter(ctx context.Context, id uuid.UUID) (Character, error)
	UpdateCharacter(ctx context.Context, c Character, at time.Time) error
	// Admin reads (never cached).
	ListScenarioVersions(ctx context.Context, scenarioID uuid.UUID) ([]ScenarioVersion, error)
	ListPromptVersions(ctx context.Context, key PromptKey) ([]PromptVersion, error)
}
