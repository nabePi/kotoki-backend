package fakes

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// LearnerStore is an in-memory learner.Store. WithinTx serialises callers (it doubles as the
// advisory lock) and, when fn fails, restores every learner table and forgets the processed_events
// rows written in the transaction. Every row is keyed by the user it was written for, and no
// method reads across users. The read side (evidence, findings, exposure, session counts) is data
// the test provides with the Set* methods.
type LearnerStore struct {
	FailInjector
	// Processed records the idempotency rows of committed transactions.
	Processed *Processed

	mu        sync.Mutex
	clock     clock.Clock
	profiles  map[uuid.UUID]learner.Profile
	states    map[uuid.UUID]map[learner.Skill]learner.StateRow
	estimates map[uuid.UUID]map[uuid.UUID]learner.EstimateRow // user -> estimate id

	evidence map[uuid.UUID]learner.EvaluationEvidence // by evaluation id
	findings map[uuid.UUID]findingsData
	exposure map[uuid.UUID][]learner.ExposureRow
	topics   map[topicKey]int
	dialect  map[uuid.UUID]int
	stats    map[uuid.UUID]learner.ProfileStats
}

type findingsData struct {
	stats       map[learner.Skill]learner.FindingStat
	evaluations int
}

type topicKey struct{ user, topic uuid.UUID }

var _ learner.Store = (*LearnerStore)(nil)

// LearnerOption customises NewLearnerStore.
type LearnerOption func(*LearnerStore)

// WithLearnerClock sets the clock behind created_at and updated_at.
func WithLearnerClock(c clock.Clock) LearnerOption { return func(s *LearnerStore) { s.clock = c } }

// NewLearnerStore returns an empty store.
func NewLearnerStore(opts ...LearnerOption) *LearnerStore {
	s := &LearnerStore{
		Processed: NewProcessed(), clock: clock.System{},
		profiles:  map[uuid.UUID]learner.Profile{},
		states:    map[uuid.UUID]map[learner.Skill]learner.StateRow{},
		estimates: map[uuid.UUID]map[uuid.UUID]learner.EstimateRow{},
		evidence:  map[uuid.UUID]learner.EvaluationEvidence{},
		findings:  map[uuid.UUID]findingsData{},
		exposure:  map[uuid.UUID][]learner.ExposureRow{},
		topics:    map[topicKey]int{},
		dialect:   map[uuid.UUID]int{},
		stats:     map[uuid.UUID]learner.ProfileStats{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SetEvidence registers the evidence Evidence returns for e.EvaluationID.
func (s *LearnerStore) SetEvidence(e learner.EvaluationEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evidence[e.EvaluationID] = cloneEvidence(e)
}

// SetFindings sets what RecentFindings returns for userID.
func (s *LearnerStore) SetFindings(userID uuid.UUID, stats map[learner.Skill]learner.FindingStat, evaluations int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findings[userID] = findingsData{stats: maps.Clone(stats), evaluations: evaluations}
}

// SetExposure sets what VoiceExposure returns for userID.
func (s *LearnerStore) SetExposure(userID uuid.UUID, rows []learner.ExposureRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exposure[userID] = slices.Clone(rows)
}

// SetTopicSessions sets what TopicSessions90d returns for (userID, topicID).
func (s *LearnerStore) SetTopicSessions(userID, topicID uuid.UUID, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topics[topicKey{userID, topicID}] = n
}

// SetStats sets what Stats returns for userID.
func (s *LearnerStore) SetStats(userID uuid.UUID, st learner.ProfileStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats[userID] = st
}

// SetDialectSessions sets what DialectSessions90d returns for userID.
func (s *LearnerStore) SetDialectSessions(userID uuid.UUID, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dialect[userID] = n
}

// Profile returns the stored profile of userID (false when none).
func (s *LearnerStore) Profile(userID uuid.UUID) (learner.Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[userID]
	return p, ok
}

// States returns the stored states of userID in D-097 order.
func (s *LearnerStore) States(userID uuid.UUID) []learner.StateRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedStates(userID)
}

// Estimates returns every estimate of userID in replay order, whatever the skill.
func (s *LearnerStore) Estimates(userID uuid.UUID) []learner.EstimateRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Collect(maps.Values(s.estimates[userID]))
	slices.SortFunc(out, cmpReplay)
	return out
}

func cloneEvidence(e learner.EvaluationEvidence) learner.EvaluationEvidence {
	e.Scores = maps.Clone(e.Scores)
	e.DimensionConfidence = maps.Clone(e.DimensionConfidence)
	e.AdaptiveDifficulty = slices.Clone(e.AdaptiveDifficulty)
	return e
}

func cloneState(r learner.StateRow) learner.StateRow {
	if r.Ceiling != nil {
		c := *r.Ceiling
		r.Ceiling = &c
	}
	if r.LastEvaluationID != nil {
		id := *r.LastEvaluationID
		r.LastEvaluationID = &id
	}
	if r.LastEvidenceAt != nil {
		at := *r.LastEvidenceAt
		r.LastEvidenceAt = &at
	}
	return r
}

func cloneEstimate(r learner.EstimateRow) learner.EstimateRow {
	if r.DEff != nil {
		d := *r.DEff
		r.DEff = &d
	}
	return r
}

type learnerState struct {
	profiles  map[uuid.UUID]learner.Profile
	states    map[uuid.UUID]map[learner.Skill]learner.StateRow
	estimates map[uuid.UUID]map[uuid.UUID]learner.EstimateRow
}

func (s *LearnerStore) snapshot() learnerState {
	st := learnerState{
		profiles:  maps.Clone(s.profiles),
		states:    make(map[uuid.UUID]map[learner.Skill]learner.StateRow, len(s.states)),
		estimates: make(map[uuid.UUID]map[uuid.UUID]learner.EstimateRow, len(s.estimates)),
	}
	for u, m := range s.states {
		c := make(map[learner.Skill]learner.StateRow, len(m))
		for k, v := range m {
			c[k] = cloneState(v)
		}
		st.states[u] = c
	}
	for u, m := range s.estimates {
		c := make(map[uuid.UUID]learner.EstimateRow, len(m))
		for k, v := range m {
			c[k] = cloneEstimate(v)
		}
		st.estimates[u] = c
	}
	return st
}

func (s *LearnerStore) sortedStates(userID uuid.UUID) []learner.StateRow {
	out := make([]learner.StateRow, 0, 6)
	for _, k := range learner.AllSkills() {
		if r, ok := s.states[userID][k]; ok {
			out = append(out, cloneState(r))
		}
	}
	return out
}

func cmpReplay(a, b learner.EstimateRow) int {
	if c := a.AssessedAt.Compare(b.AssessedAt); c != 0 {
		return c
	}
	if c := compareUUID(a.EvaluationID, b.EvaluationID); c != 0 {
		return c
	}
	return strings.Compare(string(a.Skill), string(b.Skill)) // estimates of several skills share both: keep Estimates stable
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// WithinTx implements learner.Store.
func (s *LearnerStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx learner.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.snapshot()
	tx := &learnerTx{s: s}
	if err := fn(ctx, tx); err != nil {
		s.profiles, s.states, s.estimates = before.profiles, before.states, before.estimates
		for _, k := range tx.marked {
			s.Processed.Forget(k.consumer, k.id)
		}
		return err
	}
	return nil
}

// Reader implements learner.Store.
func (s *LearnerStore) Reader() learner.Reader { return learnerReader{s: s} }

type learnerTx struct {
	s      *LearnerStore
	marked []processedKey2
}

func (t *learnerTx) Profiles() learner.ProfileRepository       { return &learnerRepo{s: t.s} }
func (t *learnerTx) SkillStates() learner.SkillStateRepository { return &learnerRepo{s: t.s} }
func (t *learnerTx) Estimates() learner.EstimateRepository     { return &learnerRepo{s: t.s} }
func (t *learnerTx) Evaluations() learner.EvaluationReader     { return &learnerRepo{s: t.s} }
func (t *learnerTx) Processed() events.ProcessedEvents         { return &learnerProcessed{tx: t} }

type learnerProcessed struct{ tx *learnerTx }

func (p *learnerProcessed) MarkProcessed(ctx context.Context, consumer string, ev events.Envelope) error {
	if err := p.tx.s.Processed.MarkProcessed(ctx, consumer, ev); err != nil {
		return err
	}
	p.tx.marked = append(p.tx.marked, processedKey2{consumer, ev.EventID})
	return nil
}

// learnerRepo implements the four repository ports; it runs inside WithinTx (store lock held).
type learnerRepo struct{ s *LearnerStore }

var (
	_ learner.ProfileRepository    = (*learnerRepo)(nil)
	_ learner.SkillStateRepository = (*learnerRepo)(nil)
	_ learner.EstimateRepository   = (*learnerRepo)(nil)
	_ learner.EvaluationReader     = (*learnerRepo)(nil)
)

func (r *learnerRepo) Ensure(_ context.Context, userID uuid.UUID) error {
	if err := r.s.Take("Ensure"); err != nil {
		return err
	}
	now := r.s.clock.Now().UTC()
	if _, ok := r.s.profiles[userID]; !ok {
		r.s.profiles[userID] = learner.Profile{
			UserID: userID, PreferredSpeechSpeed: "NORMAL", PreferredFormality: "AUTO", DialectPreference: "STANDARD_ONLY",
			MemoryEnabled: true, CreatedAt: now, UpdatedAt: now,
		}
	}
	if r.s.states[userID] == nil {
		r.s.states[userID] = map[learner.Skill]learner.StateRow{}
	}
	for _, k := range learner.AllSkills() {
		if _, ok := r.s.states[userID][k]; ok {
			continue
		}
		p := learner.PriorFor(nil, k)
		r.s.states[userID][k] = learner.StateRow{
			Skill: k, PriorLevel: p.Level, PriorConfidence: p.Confidence, Level: p.Level, Confidence: p.Confidence,
			Trend: learner.TrendInsufficient,
		}
	}
	return nil
}

func (r *learnerRepo) Get(_ context.Context, userID uuid.UUID) (learner.Profile, error) {
	if err := r.s.Take("GetProfile"); err != nil {
		return learner.Profile{}, err
	}
	p, ok := r.s.profiles[userID]
	if !ok {
		return learner.Profile{}, learner.ErrNotFound
	}
	return p, nil
}

func (r *learnerRepo) Update(_ context.Context, userID uuid.UUID, row learner.ProfileRow) (learner.Profile, error) {
	if err := r.s.Take("UpdateProfile"); err != nil {
		return learner.Profile{}, err
	}
	p, ok := r.s.profiles[userID]
	if !ok {
		return learner.Profile{}, learner.ErrNotFound
	}
	p.Nickname, p.SelfDeclaredLevel, p.TargetLevel = row.Nickname, row.SelfDeclaredLevel, row.TargetLevel
	p.LearningGoal, p.LearningGoalNote = row.LearningGoal, row.LearningGoalNote
	p.PreferredSpeechSpeed, p.PreferredFormality, p.DialectPreference = row.PreferredSpeechSpeed, row.PreferredFormality, row.DialectPreference
	p.MemoryEnabled, p.OnboardingCompletedAt = row.MemoryEnabled, row.OnboardingCompletedAt
	p.UpdatedAt = r.s.clock.Now().UTC()
	r.s.profiles[userID] = p
	return p, nil
}

func (r *learnerRepo) LockUser(context.Context, uuid.UUID) error { return r.s.Take("LockUser") }

func (r *learnerRepo) ResetPriors(_ context.Context, userID uuid.UUID, level *string, _ time.Time) error {
	if err := r.s.Take("ResetPriors"); err != nil {
		return err
	}
	for k, st := range r.s.states[userID] {
		if st.EvidenceCount != 0 {
			continue
		}
		p := learner.PriorFor(level, k)
		st.PriorLevel, st.PriorConfidence, st.Level, st.Confidence = p.Level, p.Confidence, p.Level, p.Confidence
		r.s.states[userID][k] = st
	}
	return nil
}

func (r *learnerRepo) Stats(_ context.Context, userID uuid.UUID) (learner.ProfileStats, error) {
	if err := r.s.Take("Stats"); err != nil {
		return learner.ProfileStats{}, err
	}
	return r.s.stats[userID], nil
}

func (r *learnerRepo) List(_ context.Context, userID uuid.UUID, forUpdate bool) ([]learner.StateRow, error) {
	method := "ListStates"
	if forUpdate {
		method = "ListStatesForUpdate" // lets a test tell the locking read from the plain one
	}
	if err := r.s.Take(method); err != nil {
		return nil, err
	}
	return r.s.sortedStates(userID), nil
}

func (r *learnerRepo) Save(_ context.Context, userID uuid.UUID, row learner.StateRow, _ time.Time) error {
	if err := r.s.Take("SaveState"); err != nil {
		return err
	}
	st, ok := r.s.states[userID][row.Skill]
	if !ok {
		return learner.ErrNotFound
	}
	row = cloneState(row)
	st.Level, st.Ceiling, st.Confidence, st.Trend = row.Level, row.Ceiling, row.Confidence, row.Trend
	st.EvidenceCount, st.LastEvaluationID, st.LastEvidenceAt = row.EvidenceCount, row.LastEvaluationID, row.LastEvidenceAt
	r.s.states[userID][row.Skill] = st
	return nil
}

func (r *learnerRepo) Reset(_ context.Context, userID uuid.UUID, _ time.Time) error {
	if err := r.s.Take("ResetStates"); err != nil {
		return err
	}
	for k, st := range r.s.states[userID] {
		st.Level, st.Confidence, st.Ceiling = st.PriorLevel, st.PriorConfidence, nil
		st.Trend, st.EvidenceCount, st.LastEvaluationID, st.LastEvidenceAt = learner.TrendInsufficient, 0, nil, nil
		r.s.states[userID][k] = st
	}
	return nil
}

func (r *learnerRepo) Insert(_ context.Context, userID uuid.UUID, row learner.EstimateRow) (bool, error) {
	if err := r.s.Take("InsertEstimate"); err != nil {
		return false, err
	}
	// ux_proficiency_estimates_eval_skill is global, not per user
	for _, m := range r.s.estimates {
		for _, e := range m {
			if e.EvaluationID == row.EvaluationID && e.Skill == row.Skill {
				return false, nil
			}
		}
	}
	if r.s.estimates[userID] == nil {
		r.s.estimates[userID] = map[uuid.UUID]learner.EstimateRow{}
	}
	r.s.estimates[userID][row.ID] = cloneEstimate(row)
	return true, nil
}

func (r *learnerRepo) bySkill(userID uuid.UUID, skill learner.Skill) []learner.EstimateRow {
	var out []learner.EstimateRow
	for _, e := range r.s.estimates[userID] {
		if e.Skill == skill {
			out = append(out, cloneEstimate(e))
		}
	}
	return out
}

func (r *learnerRepo) ListBySkill(_ context.Context, userID uuid.UUID, skill learner.Skill) ([]learner.EstimateRow, error) {
	if err := r.s.Take("ListBySkill"); err != nil {
		return nil, err
	}
	out := r.bySkill(userID, skill)
	slices.SortFunc(out, cmpReplay)
	return out, nil
}

func (r *learnerRepo) UpdateDerived(_ context.Context, userID uuid.UUID, row learner.EstimateRow) error {
	if err := r.s.Take("UpdateDerived"); err != nil {
		return err
	}
	e, ok := r.s.estimates[userID][row.ID]
	if !ok {
		return learner.ErrNotFound
	}
	e.Previous, e.Level, e.Confidence, e.Count, e.Trend = row.Previous, row.Level, row.Confidence, row.Count, row.Trend
	r.s.estimates[userID][row.ID] = e
	return nil
}

func (r *learnerRepo) DeleteAll(_ context.Context, userID uuid.UUID) error {
	if err := r.s.Take("DeleteAll"); err != nil {
		return err
	}
	delete(r.s.estimates, userID)
	return nil
}

func (r *learnerRepo) History(_ context.Context, userID uuid.UUID, skill learner.Skill, after *learner.HistoryCursor, limit int) ([]learner.EstimateRow, error) {
	if err := r.s.Take("History"); err != nil {
		return nil, err
	}
	rows := r.bySkill(userID, skill)
	slices.SortFunc(rows, func(a, b learner.EstimateRow) int {
		if c := b.AssessedAt.Compare(a.AssessedAt); c != 0 {
			return c
		}
		return compareUUID(b.ID, a.ID)
	})
	var out []learner.EstimateRow
	for _, e := range rows {
		if after != nil {
			if c := e.AssessedAt.Compare(after.AssessedAt); c > 0 || (c == 0 && compareUUID(e.ID, after.ID) >= 0) {
				continue
			}
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (r *learnerRepo) Evidence(_ context.Context, userID, evaluationID uuid.UUID) (learner.EvaluationEvidence, error) {
	if err := r.s.Take("Evidence"); err != nil {
		return learner.EvaluationEvidence{}, err
	}
	e, ok := r.s.evidence[evaluationID]
	if !ok {
		return learner.EvaluationEvidence{}, learner.ErrEvaluationNotVisible
	}
	if e.UserID != userID {
		return learner.EvaluationEvidence{}, errs.Wrap(learner.ErrNotFound, errs.CodeNotFound, fmt.Sprintf("evaluation %s", evaluationID))
	}
	return cloneEvidence(e), nil
}

type learnerReader struct{ s *LearnerStore }

func (r learnerReader) RecentFindings(_ context.Context, userID uuid.UUID) (map[learner.Skill]learner.FindingStat, int, error) {
	if err := r.s.Take("RecentFindings"); err != nil {
		return nil, 0, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	f := r.s.findings[userID]
	out := maps.Clone(f.stats)
	if out == nil {
		out = map[learner.Skill]learner.FindingStat{}
	}
	return out, f.evaluations, nil
}

func (r learnerReader) VoiceExposure(_ context.Context, userID uuid.UUID, _ time.Time) ([]learner.ExposureRow, error) {
	if err := r.s.Take("VoiceExposure"); err != nil {
		return nil, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return slices.Clone(r.s.exposure[userID]), nil
}

func (r learnerReader) TopicSessions90d(_ context.Context, userID, topicID uuid.UUID, _ time.Time) (int, error) {
	if err := r.s.Take("TopicSessions90d"); err != nil {
		return 0, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.topics[topicKey{userID, topicID}], nil
}

func (r learnerReader) DialectSessions90d(_ context.Context, userID uuid.UUID, _ time.Time) (int, error) {
	if err := r.s.Take("DialectSessions90d"); err != nil {
		return 0, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.dialect[userID], nil
}
