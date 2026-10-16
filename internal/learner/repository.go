package learner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// ErrNotFound is returned when a row does not exist for the user: another user's row is
// indistinguishable from a missing one (spec 12.10).
var ErrNotFound = errors.New("learner: not found")

// ErrEvaluationNotVisible means the evaluation row is not committed (yet); the consumer retries
// (spec 12.5.3). An evaluation that exists for another user is not this error: it is ErrNotFound.
var ErrEvaluationNotVisible = errors.New("learner: evaluation not visible yet")

// Profile is one learner_profiles row.
type Profile struct {
	UserID                                                                   uuid.UUID
	Nickname, SelfDeclaredLevel, TargetLevel, LearningGoal, LearningGoalNote *string
	PreferredSpeechSpeed, PreferredFormality, DialectPreference              string
	MemoryEnabled                                                            bool
	OnboardingCompletedAt                                                    *time.Time
	CreatedAt, UpdatedAt                                                     time.Time
}

// ProfileRow is the writable part of a Profile (every column but the key and the timestamps).
type ProfileRow struct {
	Nickname, SelfDeclaredLevel, TargetLevel, LearningGoal, LearningGoalNote *string
	PreferredSpeechSpeed, PreferredFormality, DialectPreference              string
	MemoryEnabled                                                            bool
	OnboardingCompletedAt                                                    *time.Time
}

// Row returns the writable columns of p.
func (p Profile) Row() ProfileRow {
	return ProfileRow{
		Nickname: p.Nickname, SelfDeclaredLevel: p.SelfDeclaredLevel, TargetLevel: p.TargetLevel,
		LearningGoal: p.LearningGoal, LearningGoalNote: p.LearningGoalNote,
		PreferredSpeechSpeed: p.PreferredSpeechSpeed, PreferredFormality: p.PreferredFormality,
		DialectPreference: p.DialectPreference, MemoryEnabled: p.MemoryEnabled,
		OnboardingCompletedAt: p.OnboardingCompletedAt,
	}
}

// ProfileStats are the practice totals shown with the profile.
type ProfileStats struct {
	CompletedSessions int
	PracticeSeconds   int64
	LastSessionAt     *time.Time
}

// StateRow is one learner_skill_states row.
type StateRow struct {
	Skill                              Skill
	PriorLevel, PriorConfidence, Level float64
	Confidence                         float64
	Ceiling                            *float64
	Trend                              Trend
	EvidenceCount                      int
	LastEvaluationID                   *uuid.UUID
	LastEvidenceAt                     *time.Time
}

// EstimateRow is one proficiency_estimates row: the evidence columns (RawScore .. CObs) and the
// derived ones (Previous .. Trend) that a replay rewrites.
type EstimateRow struct {
	ID, EvaluationID, SessionID uuid.UUID
	Skill                       Skill
	AlgorithmVersion            int
	RawScore                    float64
	DEff                        *int
	LObs, CObs                  float64
	AssessedAt                  time.Time
	Previous, Level, Confidence float64
	Count                       int
	Trend                       Trend
}

// HistoryCursor is the keyset position of the history listing: rows come newest first, ordered by
// (assessed_at, id) descending, and the next page holds the rows strictly before the cursor.
type HistoryCursor struct {
	AssessedAt time.Time
	ID         uuid.UUID
}

// EvaluationEvidence is the part of an evaluation (and of its session) that the proficiency
// algorithm reads (spec 12.4). Scores holds all four dimension scores; a nil entry is a missing
// score. NTurns counts the valid turns of 12.4. AdaptiveDifficulty is sessions.adaptive_difficulty.
type EvaluationEvidence struct {
	EvaluationID, SessionID, UserID uuid.UUID
	Status                          string
	Scores                          map[Skill]*int
	ListeningSignal                 *int
	InteractionSignal               *int
	DimensionConfidence             map[string]float64
	SessionEndedAt                  time.Time
	AdaptiveDifficulty              json.RawMessage
	ScenarioDifficulty              int
	NTurns                          int
}

// ExposureRow is one (gender, accent) group of the voice exposure of the last 90 days.
type ExposureRow struct {
	Gender, Accent string
	Sessions       int
	Seconds        int64
}

// ProfileRepository reads and writes learner_profiles. Every method takes the user it acts for.
type ProfileRepository interface {
	// Ensure creates the profile and the six skill states of the user when missing (spec 12.3).
	Ensure(ctx context.Context, userID uuid.UUID) error
	// Get returns the profile; ErrNotFound when the user has none.
	Get(ctx context.Context, userID uuid.UUID) (Profile, error)
	// Update replaces the writable columns; ErrNotFound when the user has no profile.
	Update(ctx context.Context, userID uuid.UUID, row ProfileRow) (Profile, error)
	// LockUser takes the transaction-scoped advisory lock that serialises learner writes of a user.
	LockUser(ctx context.Context, userID uuid.UUID) error
	// ResetPriors sets the priors of onboarding level (nil: skipped) on every skill state that has
	// no evidence, and the current level and confidence with them.
	ResetPriors(ctx context.Context, userID uuid.UUID, level *string, now time.Time) error
	// Stats returns the practice totals of the user's COMPLETED sessions.
	Stats(ctx context.Context, userID uuid.UUID) (ProfileStats, error)
}

// SkillStateRepository reads and writes learner_skill_states.
type SkillStateRepository interface {
	// List returns the user's six states in D-097 order, locking them when forUpdate.
	List(ctx context.Context, userID uuid.UUID, forUpdate bool) ([]StateRow, error)
	// Save writes the mutable columns of the state of row.Skill (never the priors);
	// ErrNotFound when the user has no such state.
	Save(ctx context.Context, userID uuid.UUID, row StateRow, now time.Time) error
	// Reset returns every state of the user to its prior (spec 12.9.5).
	Reset(ctx context.Context, userID uuid.UUID, now time.Time) error
}

// EstimateRepository reads and writes proficiency_estimates.
type EstimateRepository interface {
	// Insert stores the estimate; inserted is false when (evaluation, skill) already has one.
	Insert(ctx context.Context, userID uuid.UUID, row EstimateRow) (inserted bool, err error)
	// ListBySkill returns the estimates of a skill in replay order: assessed_at, then evaluation id.
	ListBySkill(ctx context.Context, userID uuid.UUID, skill Skill) ([]EstimateRow, error)
	// UpdateDerived rewrites only the derived columns of the estimate row.ID; ErrNotFound when the
	// user has no such estimate.
	UpdateDerived(ctx context.Context, userID uuid.UUID, row EstimateRow) error
	// DeleteAll removes every estimate of the user.
	DeleteAll(ctx context.Context, userID uuid.UUID) error
	// History returns at most limit estimates of a skill, newest first, strictly after the cursor.
	History(ctx context.Context, userID uuid.UUID, skill Skill, after *HistoryCursor, limit int) ([]EstimateRow, error)
}

// EvaluationReader reads the evidence of an evaluation.
type EvaluationReader interface {
	// Evidence returns the evidence of the evaluation of userID. ErrEvaluationNotVisible when the
	// row does not exist; an error wrapping ErrNotFound (errs.CodeNotFound) when it exists but the
	// evaluation or its session belongs to somebody else.
	Evidence(ctx context.Context, userID, evaluationID uuid.UUID) (EvaluationEvidence, error)
}

// Reader is the non-transactional read side used when a session is created and by the REST views.
type Reader interface {
	// RecentFindings counts the findings of the user's last 5 COMPLETED evaluations by skill
	// (spec 12.7) and the number of evaluations counted; dimensions that are not skills are left out.
	RecentFindings(ctx context.Context, userID uuid.UUID) (map[Skill]FindingStat, int, error)
	// VoiceExposure groups the COMPLETED sessions of the 90 days before now by voice (spec 12.8.6).
	VoiceExposure(ctx context.Context, userID uuid.UUID, now time.Time) ([]ExposureRow, error)
	// TopicSessions90d counts the COMPLETED sessions of the topic in the 90 days before now.
	TopicSessions90d(ctx context.Context, userID, topicID uuid.UUID, now time.Time) (int, error)
	// DialectSessions90d counts the COMPLETED sessions with a KANSAI character in the 90 days before now.
	DialectSessions90d(ctx context.Context, userID uuid.UUID, now time.Time) (int, error)
}

// Tx is the transactional port of the learner feature.
type Tx interface {
	Profiles() ProfileRepository
	SkillStates() SkillStateRepository
	Estimates() EstimateRepository
	Evaluations() EvaluationReader
	Processed() events.ProcessedEvents
}

// Store runs learner transactions and hands out the plain reader.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	Reader() Reader
}
