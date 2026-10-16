package learner

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// History paging (spec 12.9.4).
const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
)

// Deps are the collaborators of Service. Content is the loader of the session configuration that
// the adaptive difficulty of a new session reads.
type Deps struct {
	Store   Store
	Content content.SessionConfigLoader
	Clock   clock.Clock
	IDs     idgen.Generator
	Log     *slog.Logger
}

// Service is the learner feature: profile, onboarding, skills view, history and reset. Every
// operation acts for the user it is given and for nobody else.
type Service struct {
	store   Store
	content content.SessionConfigLoader
	clock   clock.Clock
	ids     idgen.Generator
	log     *slog.Logger
}

// NewService returns a Service over d.
func NewService(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: d.Store, content: d.Content, clock: d.Clock, ids: d.IDs, log: log}
}

// EnsureProfile creates the profile and the six skill states of the user when missing (spec 12.3).
// It joins the caller's transaction.
func (s *Service) EnsureProfile(ctx context.Context, tx Tx, userID uuid.UUID) error {
	return tx.Profiles().Ensure(ctx, userID)
}

// GetProfile returns the profile of the user with its overall level and practice totals.
func (s *Service) GetProfile(ctx context.Context, userID uuid.UUID) (ProfileView, error) {
	var view ProfileView
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		view = ProfileView{}
		if err := s.EnsureProfile(ctx, tx, userID); err != nil {
			return err
		}
		v, err := s.profileView(ctx, tx, userID)
		view = v
		return err
	})
	return view, err
}

func (s *Service) profileView(ctx context.Context, tx Tx, userID uuid.UUID) (ProfileView, error) {
	p, err := tx.Profiles().Get(ctx, userID)
	if err != nil {
		return ProfileView{}, err
	}
	states, err := tx.SkillStates().List(ctx, userID, false)
	if err != nil {
		return ProfileView{}, err
	}
	stats, err := tx.Profiles().Stats(ctx, userID)
	if err != nil {
		return ProfileView{}, err
	}
	return newProfileView(p, states, stats, s.clock.Now()), nil
}

// PatchProfile applies a partial update. The input is validated as a whole before anything is
// written. A self_declared_level in the patch resets the priors while no skill has evidence and
// completes the onboarding (spec 12.3); once there is evidence only the profile column changes.
func (s *Service) PatchProfile(ctx context.Context, userID uuid.UUID, in PatchInput) (ProfileView, error) {
	in, err := validatePatch(in)
	if err != nil {
		return ProfileView{}, err
	}
	var view ProfileView
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		view = ProfileView{}
		// the same lock as the evaluation worker: a reset of priors must not interleave with an update
		if err := tx.Profiles().LockUser(ctx, userID); err != nil {
			return err
		}
		if err := s.EnsureProfile(ctx, tx, userID); err != nil {
			return err
		}
		if in.empty() {
			v, err := s.profileView(ctx, tx, userID)
			view = v
			return err
		}
		cur, err := tx.Profiles().Get(ctx, userID)
		if err != nil {
			return err
		}
		row := in.apply(cur.Row())
		if in.SelfDeclaredLevel.Set {
			states, err := tx.SkillStates().List(ctx, userID, true)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(states, func(st StateRow) bool { return st.EvidenceCount > 0 }) {
				now := s.clock.Now()
				if err := tx.Profiles().ResetPriors(ctx, userID, row.SelfDeclaredLevel, now); err != nil {
					return err
				}
				if row.SelfDeclaredLevel != nil && row.OnboardingCompletedAt == nil {
					row.OnboardingCompletedAt = &now
				}
			}
		}
		if _, err := tx.Profiles().Update(ctx, userID, row); err != nil {
			return err
		}
		v, err := s.profileView(ctx, tx, userID)
		view = v
		return err
	})
	return view, err
}

// Skills returns the six skills, the focus skills of spec 12.7 (without a scenario focus) and the
// voice exposure of the last 90 days.
func (s *Service) Skills(ctx context.Context, userID uuid.UUID) (SkillsView, error) {
	var states []StateRow
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		states = nil
		if err := s.EnsureProfile(ctx, tx, userID); err != nil {
			return err
		}
		var err error
		states, err = tx.SkillStates().List(ctx, userID, false)
		return err
	})
	if err != nil {
		return SkillsView{}, err
	}
	now := s.clock.Now()
	rd := s.store.Reader()
	findings, evaluations, err := rd.RecentFindings(ctx, userID)
	if err != nil {
		return SkillsView{}, err
	}
	exposure, err := rd.VoiceExposure(ctx, userID, now)
	if err != nil {
		return SkillsView{}, err
	}

	v := SkillsView{
		Skills:           make([]SkillView, 0, len(states)),
		FocusSkills:      FocusSkills(focusInput(states, findings, evaluations, now)),
		VoiceExposure:    newVoiceExposure(exposure),
		AlgorithmVersion: AlgorithmVersion,
	}
	for _, st := range states {
		v.Skills = append(v.Skills, newSkillView(st, now))
	}
	return v, nil
}

// History returns up to limit estimates of a skill, newest first, after the cursor, and the cursor
// of the next page (nil on the last page). A limit of 0 or less means 20; the maximum is 100.
func (s *Service) History(ctx context.Context, userID uuid.UUID, skill Skill, after *HistoryCursor, limit int) ([]EstimateRow, *HistoryCursor, error) {
	if _, ok := ParseSkill(string(skill)); !ok {
		return nil, nil, validationError([]violation{{"skill", reasonInvalidFormat}})
	}
	if after != nil && (after.ID == uuid.Nil || after.AssessedAt.IsZero()) {
		return nil, nil, errs.New(errs.CodeInvalidCursor, "invalid history cursor")
	}
	switch {
	case limit <= 0:
		limit = defaultHistoryLimit
	case limit > maxHistoryLimit:
		limit = maxHistoryLimit
	}
	var rows []EstimateRow
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		rows = nil
		if err := s.EnsureProfile(ctx, tx, userID); err != nil {
			return err
		}
		var err error
		rows, err = tx.Estimates().History(ctx, userID, skill, after, limit+1) // one more tells whether a page follows
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *HistoryCursor
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = &HistoryCursor{AssessedAt: last.AssessedAt, ID: last.ID}
	}
	return rows, next, nil
}

// ResetSkills deletes the user's proficiency history and returns every skill to its prior
// (spec 12.9.5). processed_events are kept: old evaluations are not applied again.
func (s *Service) ResetSkills(ctx context.Context, userID uuid.UUID) error {
	var now time.Time
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		now = s.clock.Now()
		if err := tx.Profiles().LockUser(ctx, userID); err != nil {
			return err
		}
		if err := s.EnsureProfile(ctx, tx, userID); err != nil {
			return err
		}
		if err := tx.Estimates().DeleteAll(ctx, userID); err != nil {
			return err
		}
		return tx.SkillStates().Reset(ctx, userID, now)
	})
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "learner skills reset", slog.String("user_id", userID.String()))
	return nil
}
