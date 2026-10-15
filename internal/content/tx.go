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

// AuditEntry is one admin_audit_logs row written by the content admin use cases.
type AuditEntry struct {
	AdminUserID uuid.UUID
	Action      string // CONTENT_VERSION_CREATED | …_PUBLISHED | …_RETIRED | …_DELETED | CONTENT_STATUS_CHANGED | CHARACTER_UPDATED
	TargetType  string // SCENARIO | PROMPT | TOPIC | CHARACTER
	TargetID    string
	Details     map[string]any
	RequestID   string
}

// AuditWriter appends to the audit log inside the transaction.
type AuditWriter interface {
	Record(ctx context.Context, e AuditEntry) error
}

// Tx is one content transaction: content reads and writes plus the audit log.
type Tx interface {
	Content() TxRepository
	Audit() AuditWriter
}

// Store runs content transactions; an error from fn rolls everything back.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Audit actions and target types written by the content admin use cases (spec 3.3.3).
const (
	ActionVersionCreated   = "CONTENT_VERSION_CREATED"
	ActionVersionPublished = "CONTENT_VERSION_PUBLISHED"
	ActionVersionRetired   = "CONTENT_VERSION_RETIRED"
	ActionVersionDeleted   = "CONTENT_VERSION_DELETED"
	ActionStatusChanged    = "CONTENT_STATUS_CHANGED"
	ActionCharacterUpdated = "CHARACTER_UPDATED"

	TargetTopic     = "TOPIC"
	TargetScenario  = "SCENARIO"
	TargetPrompt    = "PROMPT"
	TargetCharacter = "CHARACTER"
)
