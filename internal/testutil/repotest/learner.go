package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/learner"
)

// LearnerHarness is one fresh learner store plus what it needs to accept rows: PostgreSQL requires
// real users, sessions and evaluations (foreign keys), a fake does not.
type LearnerHarness struct {
	Store learner.Store
	// NewUser returns a fresh user.
	NewUser func(t *testing.T) uuid.UUID
	// NewEvaluation returns an evaluation of userID and the session it evaluated.
	NewEvaluation func(t *testing.T, userID uuid.UUID) (evaluationID, sessionID uuid.UUID)
}

var learnerAt = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func (h LearnerHarness) tx(t *testing.T, fn func(ctx context.Context, tx learner.Tx)) {
	t.Helper()
	require.NoError(t, h.Store.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		fn(ctx, tx)
		return nil
	}))
}

// ensured returns a new user whose profile and states exist.
func (h LearnerHarness) ensured(t *testing.T) uuid.UUID {
	t.Helper()
	u := h.NewUser(t)
	h.tx(t, func(ctx context.Context, tx learner.Tx) { require.NoError(t, tx.Profiles().Ensure(ctx, u)) })
	return u
}

func (h LearnerHarness) estimate(t *testing.T, userID uuid.UUID, skill learner.Skill, at time.Time) learner.EstimateRow {
	t.Helper()
	ev, ss := h.NewEvaluation(t, userID)
	d := 3
	return learner.EstimateRow{
		ID: uuid.Must(uuid.NewV7()), EvaluationID: ev, SessionID: ss, Skill: skill, AlgorithmVersion: learner.AlgorithmVersion,
		RawScore: 58, DEff: &d, LObs: 2.66, CObs: 0.5, AssessedAt: at,
		Previous: 1, Level: 1.5, Confidence: 0.2, Count: 1, Trend: learner.TrendInsufficient,
	}
}

func (h LearnerHarness) insert(t *testing.T, userID uuid.UUID, row learner.EstimateRow) bool {
	t.Helper()
	var ok bool
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		ok, err = tx.Estimates().Insert(ctx, userID, row)
		require.NoError(t, err)
	})
	return ok
}

func (h LearnerHarness) states(t *testing.T, userID uuid.UUID) []learner.StateRow {
	t.Helper()
	var out []learner.StateRow
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		out, err = tx.SkillStates().List(ctx, userID, false)
		require.NoError(t, err)
	})
	return out
}

func (h LearnerHarness) listBySkill(t *testing.T, userID uuid.UUID, skill learner.Skill) []learner.EstimateRow {
	t.Helper()
	var out []learner.EstimateRow
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		out, err = tx.Estimates().ListBySkill(ctx, userID, skill)
		require.NoError(t, err)
	})
	return out
}

func (h LearnerHarness) history(t *testing.T, userID uuid.UUID, skill learner.Skill, after *learner.HistoryCursor, limit int) []learner.EstimateRow {
	t.Helper()
	var out []learner.EstimateRow
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		out, err = tx.Estimates().History(ctx, userID, skill, after, limit)
		require.NoError(t, err)
	})
	return out
}

// saveEvidence gives a user's skill an evidence-bearing state.
func (h LearnerHarness) saveEvidence(t *testing.T, userID uuid.UUID, skill learner.Skill, level float64) {
	t.Helper()
	ev, _ := h.NewEvaluation(t, userID)
	ceiling, last := level, learnerAt.Add(-time.Hour)
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		require.NoError(t, tx.SkillStates().Save(ctx, userID, learner.StateRow{
			Skill: skill, Level: level, Confidence: 0.4, Ceiling: &ceiling, Trend: learner.TrendStable,
			EvidenceCount: 2, LastEvaluationID: &ev, LastEvidenceAt: &last,
		}, learnerAt))
	})
}

func stateOf(t *testing.T, rows []learner.StateRow, skill learner.Skill) learner.StateRow {
	t.Helper()
	for _, r := range rows {
		if r.Skill == skill {
			return r
		}
	}
	t.Fatalf("no state for %s", skill)
	return learner.StateRow{}
}

// LearnerRepoSuite is the contract of the learner repositories, run against the in-memory fake and
// PostgreSQL alike.
func LearnerRepoSuite(t *testing.T, mk func(t *testing.T) LearnerHarness) {
	Each(t, mk,
		Case[LearnerHarness]{Name: "EnsureIsIdempotentWithSixStates", Run: learnerEnsure},
		Case[LearnerHarness]{Name: "GetAndUpdateProfile", Run: learnerProfileUpdate},
		Case[LearnerHarness]{Name: "ProfileOfUserWithoutOneIsNotFound", Run: learnerNoProfile},
		Case[LearnerHarness]{Name: "SaveChangesStateButNeverThePriors", Run: learnerSave},
		Case[LearnerHarness]{Name: "ResetPriorsSkipsStatesWithEvidence", Run: learnerResetPriors},
		Case[LearnerHarness]{Name: "ResetRestoresPriorsKeepsEstimates", Run: learnerReset},
		Case[LearnerHarness]{Name: "EstimateInsertConflictIsNoop", Run: learnerEstimateConflict},
		Case[LearnerHarness]{Name: "ListBySkillReplayOrder", Run: learnerReplayOrder},
		Case[LearnerHarness]{Name: "UpdateDerivedTouchesOnlyDerivedColumns", Run: learnerUpdateDerived},
		Case[LearnerHarness]{Name: "DeleteAllRemovesOnlyOwnEstimates", Run: learnerDeleteAll},
		Case[LearnerHarness]{Name: "HistoryOrderAndCursor", Run: learnerHistory},
		Case[LearnerHarness]{Name: "IsolationOtherUserRowsInvisible", Run: learnerIsolation},
		Case[LearnerHarness]{Name: "IsolationWritesNeverReachOtherUsers", Run: learnerWriteIsolation},
		Case[LearnerHarness]{Name: "TransactionRollsBack", Run: learnerRollback},
	)
}

func learnerEnsure(t *testing.T, h LearnerHarness) {
	u := h.NewUser(t)
	for range 2 {
		h.tx(t, func(ctx context.Context, tx learner.Tx) { require.NoError(t, tx.Profiles().Ensure(ctx, u)) })
	}
	states := h.states(t, u)
	require.Len(t, states, 6)
	for i, s := range states {
		require.Equal(t, learner.AllSkills()[i], s.Skill, "D-097 order")
		require.InDelta(t, 1.00, s.PriorLevel, 1e-9)
		require.InDelta(t, 0.050, s.PriorConfidence, 1e-9)
		require.InDelta(t, 1.00, s.Level, 1e-9)
		require.InDelta(t, 0.050, s.Confidence, 1e-9)
		require.Nil(t, s.Ceiling)
		require.Equal(t, learner.TrendInsufficient, s.Trend)
		require.Zero(t, s.EvidenceCount)
		require.Nil(t, s.LastEvaluationID)
		require.Nil(t, s.LastEvidenceAt)
	}
	var p learner.Profile
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		p, err = tx.Profiles().Get(ctx, u)
		require.NoError(t, err)
	})
	require.Equal(t, u, p.UserID)
	require.Equal(t, "NORMAL", p.PreferredSpeechSpeed)
	require.Equal(t, "AUTO", p.PreferredFormality)
	require.Equal(t, "STANDARD_ONLY", p.DialectPreference)
	require.True(t, p.MemoryEnabled)
	require.Nil(t, p.Nickname)
	require.Nil(t, p.OnboardingCompletedAt)
}

func learnerProfileUpdate(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	nick, lvl, goal, note := "Budi", "N4", "JOB_INTERVIEW", "wawancara"
	done := learnerAt
	var got learner.Profile
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		got, err = tx.Profiles().Update(ctx, u, learner.ProfileRow{
			Nickname: &nick, SelfDeclaredLevel: &lvl, TargetLevel: nil, LearningGoal: &goal, LearningGoalNote: &note,
			PreferredSpeechSpeed: "SLOWER", PreferredFormality: "POLITE", DialectPreference: "ALLOW_DIALECT",
			MemoryEnabled: false, OnboardingCompletedAt: &done,
		})
		require.NoError(t, err)
	})
	require.Equal(t, u, got.UserID)
	require.Equal(t, &nick, got.Nickname)
	require.Equal(t, &lvl, got.SelfDeclaredLevel)
	require.Nil(t, got.TargetLevel)
	require.Equal(t, &goal, got.LearningGoal)
	require.Equal(t, &note, got.LearningGoalNote)
	require.Equal(t, "SLOWER", got.PreferredSpeechSpeed)
	require.Equal(t, "POLITE", got.PreferredFormality)
	require.Equal(t, "ALLOW_DIALECT", got.DialectPreference)
	require.False(t, got.MemoryEnabled)
	require.NotNil(t, got.OnboardingCompletedAt)
	require.True(t, got.OnboardingCompletedAt.Equal(done))

	// null clears a nullable column again
	var cleared learner.Profile
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		row := got.Row()
		row.Nickname, row.LearningGoalNote = nil, nil
		var err error
		cleared, err = tx.Profiles().Update(ctx, u, row)
		require.NoError(t, err)
	})
	require.Nil(t, cleared.Nickname)
	require.Nil(t, cleared.LearningGoalNote)
	require.Equal(t, &lvl, cleared.SelfDeclaredLevel)
}

func learnerNoProfile(t *testing.T, h LearnerHarness) {
	u := h.NewUser(t)
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		_, err := tx.Profiles().Get(ctx, u)
		require.ErrorIs(t, err, learner.ErrNotFound)
		_, err = tx.Profiles().Update(ctx, u, learner.ProfileRow{
			PreferredSpeechSpeed: "NORMAL", PreferredFormality: "AUTO", DialectPreference: "STANDARD_ONLY", MemoryEnabled: true,
		})
		require.ErrorIs(t, err, learner.ErrNotFound)
		err = tx.SkillStates().Save(ctx, u, learner.StateRow{Skill: learner.SkillGrammar, Trend: learner.TrendInsufficient}, learnerAt)
		require.ErrorIs(t, err, learner.ErrNotFound)
		states, err := tx.SkillStates().List(ctx, u, false)
		require.NoError(t, err)
		require.Empty(t, states)
	})
}

func learnerSave(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	h.saveEvidence(t, u, learner.SkillFluency, 2.4)
	s := stateOf(t, h.states(t, u), learner.SkillFluency)
	require.InDelta(t, 2.4, s.Level, 1e-9)
	require.InDelta(t, 0.4, s.Confidence, 1e-9)
	require.NotNil(t, s.Ceiling)
	require.InDelta(t, 2.4, *s.Ceiling, 1e-9)
	require.Equal(t, learner.TrendStable, s.Trend)
	require.Equal(t, 2, s.EvidenceCount)
	require.NotNil(t, s.LastEvaluationID)
	require.NotNil(t, s.LastEvidenceAt)
	require.InDelta(t, 1.00, s.PriorLevel, 1e-9, "priors are not written by Save")
	require.InDelta(t, 0.050, s.PriorConfidence, 1e-9)
	g := stateOf(t, h.states(t, u), learner.SkillGrammar)
	require.InDelta(t, 1.00, g.Level, 1e-9, "other skills untouched")
	require.Zero(t, g.EvidenceCount)
}

func learnerResetPriors(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	h.saveEvidence(t, u, learner.SkillFluency, 2.4)
	n4 := "N4"
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		require.NoError(t, tx.Profiles().ResetPriors(ctx, u, &n4, learnerAt))
	})
	rows := h.states(t, u)
	for _, s := range rows {
		if s.Skill == learner.SkillFluency {
			require.InDelta(t, 2.4, s.Level, 1e-9, "a state with evidence keeps its level")
			require.InDelta(t, 1.00, s.PriorLevel, 1e-9, "and its prior")
			continue
		}
		p := learner.PriorFor(&n4, s.Skill)
		require.InDelta(t, p.Level, s.PriorLevel, 1e-9, s.Skill)
		require.InDelta(t, p.Level, s.Level, 1e-9, s.Skill)
		require.InDelta(t, p.Confidence, s.PriorConfidence, 1e-9, s.Skill)
		require.InDelta(t, p.Confidence, s.Confidence, 1e-9, s.Skill)
	}
	// skipping onboarding (nil level) goes back to the NULL prior
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		require.NoError(t, tx.Profiles().ResetPriors(ctx, u, nil, learnerAt))
	})
	require.InDelta(t, 1.00, stateOf(t, h.states(t, u), learner.SkillGrammar).Level, 1e-9)
}

func learnerReset(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	n3 := "N3"
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		require.NoError(t, tx.Profiles().ResetPriors(ctx, u, &n3, learnerAt))
	})
	h.saveEvidence(t, u, learner.SkillGrammar, 4.1)
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		require.NoError(t, tx.SkillStates().Reset(ctx, u, learnerAt))
	})
	for _, s := range h.states(t, u) {
		p := learner.PriorFor(&n3, s.Skill)
		require.InDelta(t, p.Level, s.Level, 1e-9, s.Skill)
		require.InDelta(t, p.Confidence, s.Confidence, 1e-9, s.Skill)
		require.Nil(t, s.Ceiling, s.Skill)
		require.Equal(t, learner.TrendInsufficient, s.Trend, s.Skill)
		require.Zero(t, s.EvidenceCount, s.Skill)
		require.Nil(t, s.LastEvaluationID, s.Skill)
		require.Nil(t, s.LastEvidenceAt, s.Skill)
	}
}

func learnerEstimateConflict(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	row := h.estimate(t, u, learner.SkillGrammar, learnerAt)
	require.True(t, h.insert(t, u, row))
	again := row
	again.ID = uuid.Must(uuid.NewV7())
	again.RawScore = 99
	require.False(t, h.insert(t, u, again), "same (evaluation, skill) is a no-op")
	got := h.listBySkill(t, u, learner.SkillGrammar)
	require.Len(t, got, 1)
	require.Equal(t, row.ID, got[0].ID)
	require.InDelta(t, 58, got[0].RawScore, 1e-9, "the first row stays")
	// another skill of the same evaluation is a different row
	other := row
	other.ID, other.Skill = uuid.Must(uuid.NewV7()), learner.SkillVocabulary
	require.True(t, h.insert(t, u, other))
}

func learnerReplayOrder(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	a := h.estimate(t, u, learner.SkillGrammar, learnerAt)
	b := h.estimate(t, u, learner.SkillGrammar, learnerAt.Add(-48*time.Hour))
	c := h.estimate(t, u, learner.SkillGrammar, learnerAt) // same instant as a: ordered by evaluation id
	for _, r := range []learner.EstimateRow{a, b, c} {
		require.True(t, h.insert(t, u, r))
	}
	require.True(t, h.insert(t, u, h.estimate(t, u, learner.SkillFluency, learnerAt.Add(-time.Hour))))
	got := h.listBySkill(t, u, learner.SkillGrammar)
	require.Len(t, got, 3, "only the asked skill")
	require.Equal(t, b.ID, got[0].ID)
	tie := []learner.EstimateRow{a, c}
	if c.EvaluationID.String() < a.EvaluationID.String() {
		tie = []learner.EstimateRow{c, a}
	}
	require.Equal(t, tie[0].ID, got[1].ID)
	require.Equal(t, tie[1].ID, got[2].ID)
}

func learnerUpdateDerived(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	row := h.estimate(t, u, learner.SkillGrammar, learnerAt)
	require.True(t, h.insert(t, u, row))
	upd := row
	upd.Previous, upd.Level, upd.Confidence, upd.Count, upd.Trend = 2.0, 2.27, 0.389, 2, learner.TrendImproving
	upd.RawScore, upd.LObs, upd.CObs = 1, 0.1, 0.1 // evidence columns must not move
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		require.NoError(t, tx.Estimates().UpdateDerived(ctx, u, upd))
	})
	got := h.listBySkill(t, u, learner.SkillGrammar)[0]
	require.InDelta(t, 2.0, got.Previous, 1e-9)
	require.InDelta(t, 2.27, got.Level, 1e-9)
	require.InDelta(t, 0.389, got.Confidence, 1e-9)
	require.Equal(t, 2, got.Count)
	require.Equal(t, learner.TrendImproving, got.Trend)
	require.InDelta(t, 58, got.RawScore, 1e-9)
	require.InDelta(t, 2.66, got.LObs, 1e-9)
	require.InDelta(t, 0.5, got.CObs, 1e-9)
	require.True(t, got.AssessedAt.Equal(row.AssessedAt))
}

func learnerDeleteAll(t *testing.T, h LearnerHarness) {
	a, b := h.ensured(t), h.ensured(t)
	require.True(t, h.insert(t, a, h.estimate(t, a, learner.SkillGrammar, learnerAt)))
	require.True(t, h.insert(t, b, h.estimate(t, b, learner.SkillGrammar, learnerAt)))
	h.tx(t, func(ctx context.Context, tx learner.Tx) { require.NoError(t, tx.Estimates().DeleteAll(ctx, a)) })
	require.Empty(t, h.listBySkill(t, a, learner.SkillGrammar))
	require.Len(t, h.listBySkill(t, b, learner.SkillGrammar), 1)
}

func learnerHistory(t *testing.T, h LearnerHarness) {
	u := h.ensured(t)
	for i := range 5 {
		r := h.estimate(t, u, learner.SkillGrammar, learnerAt.Add(time.Duration(i)*time.Hour))
		require.True(t, h.insert(t, u, r))
	}
	// two rows share an instant: the id breaks the tie, descending
	tie := h.estimate(t, u, learner.SkillGrammar, learnerAt.Add(4*time.Hour))
	require.True(t, h.insert(t, u, tie))
	require.True(t, h.insert(t, u, h.estimate(t, u, learner.SkillFluency, learnerAt.Add(9*time.Hour))))

	all := h.history(t, u, learner.SkillGrammar, nil, 10)
	require.Len(t, all, 6)
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		require.False(t, cur.AssessedAt.After(prev.AssessedAt), "newest first")
		if cur.AssessedAt.Equal(prev.AssessedAt) {
			require.Greater(t, prev.ID.String(), cur.ID.String(), "id descending on a tie")
		}
	}

	var got []learner.EstimateRow
	var after *learner.HistoryCursor
	for range 10 {
		page := h.history(t, u, learner.SkillGrammar, after, 2)
		if len(page) == 0 {
			break
		}
		require.LessOrEqual(t, len(page), 2)
		got = append(got, page...)
		last := page[len(page)-1]
		after = &learner.HistoryCursor{AssessedAt: last.AssessedAt, ID: last.ID}
	}
	require.Equal(t, all, got, "paging by cursor walks the same sequence without gaps or repeats")
	require.Empty(t, h.history(t, u, learner.SkillGrammar, after, 2))
	require.Empty(t, h.history(t, u, learner.SkillNaturalness, nil, 10))
}

func learnerIsolation(t *testing.T, h LearnerHarness) {
	a, b := h.ensured(t), h.ensured(t)
	nick := "Bee"
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		_, err := tx.Profiles().Update(ctx, b, learner.ProfileRow{
			Nickname: &nick, PreferredSpeechSpeed: "FASTER", PreferredFormality: "CASUAL", DialectPreference: "ALLOW_DIALECT", MemoryEnabled: false,
		})
		require.NoError(t, err)
	})
	h.saveEvidence(t, b, learner.SkillGrammar, 4.4)
	rowB := h.estimate(t, b, learner.SkillGrammar, learnerAt)
	require.True(t, h.insert(t, b, rowB))

	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		p, err := tx.Profiles().Get(ctx, a)
		require.NoError(t, err)
		require.Equal(t, a, p.UserID)
		require.Nil(t, p.Nickname)
		require.Equal(t, "NORMAL", p.PreferredSpeechSpeed)
		require.True(t, p.MemoryEnabled)

		states, err := tx.SkillStates().List(ctx, a, false)
		require.NoError(t, err)
		require.Len(t, states, 6)
		for _, s := range states {
			require.Zero(t, s.EvidenceCount, "B's evidence is not A's")
			require.InDelta(t, 1.00, s.Level, 1e-9)
		}
		locked, err := tx.SkillStates().List(ctx, a, true)
		require.NoError(t, err)
		require.Len(t, locked, 6)

		est, err := tx.Estimates().ListBySkill(ctx, a, learner.SkillGrammar)
		require.NoError(t, err)
		require.Empty(t, est)
		hist, err := tx.Estimates().History(ctx, a, learner.SkillGrammar, nil, 50)
		require.NoError(t, err)
		require.Empty(t, hist)

		stats, err := tx.Profiles().Stats(ctx, a)
		require.NoError(t, err)
		require.Zero(t, stats.CompletedSessions)
		require.Zero(t, stats.PracticeSeconds)
		require.Nil(t, stats.LastSessionAt)
	})
	// and the other way round: B still sees exactly its own
	require.Len(t, h.listBySkill(t, b, learner.SkillGrammar), 1)
	require.Len(t, h.history(t, b, learner.SkillGrammar, nil, 50), 1)
}

func learnerWriteIsolation(t *testing.T, h LearnerHarness) {
	a, b := h.ensured(t), h.ensured(t)
	h.saveEvidence(t, b, learner.SkillGrammar, 4.4)
	rowB := h.estimate(t, b, learner.SkillGrammar, learnerAt)
	require.True(t, h.insert(t, b, rowB))
	beforeStates := h.states(t, b)
	beforeEst := h.listBySkill(t, b, learner.SkillGrammar)
	var beforeProfile learner.Profile
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		var err error
		beforeProfile, err = tx.Profiles().Get(ctx, b)
		require.NoError(t, err)
	})

	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		// A rewrites the derived columns of B's estimate by its id: no row of A matches
		forged := rowB
		forged.Level, forged.Trend, forged.Count = 0.1, learner.TrendDeclining, 9
		require.ErrorIs(t, tx.Estimates().UpdateDerived(ctx, a, forged), learner.ErrNotFound)

		// A's profile writes, state writes, resets and deletes only reach A
		nick := "Aye"
		_, err := tx.Profiles().Update(ctx, a, learner.ProfileRow{
			Nickname: &nick, PreferredSpeechSpeed: "SLOWER", PreferredFormality: "POLITE", DialectPreference: "STANDARD_ONLY", MemoryEnabled: false,
		})
		require.NoError(t, err)
		n1 := "N1"
		require.NoError(t, tx.Profiles().ResetPriors(ctx, a, &n1, learnerAt))
		require.NoError(t, tx.SkillStates().Save(ctx, a, learner.StateRow{Skill: learner.SkillGrammar, Level: 0.5, Confidence: 0.1, Trend: learner.TrendInsufficient}, learnerAt))
		require.NoError(t, tx.SkillStates().Reset(ctx, a, learnerAt))
		require.NoError(t, tx.Estimates().DeleteAll(ctx, a))
	})

	require.Equal(t, beforeStates, h.states(t, b), "B's states are unchanged")
	require.Equal(t, beforeEst, h.listBySkill(t, b, learner.SkillGrammar), "B's estimates are unchanged")
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		p, err := tx.Profiles().Get(ctx, b)
		require.NoError(t, err)
		require.Equal(t, beforeProfile, p, "B's profile is unchanged")
	})
}

func learnerRollback(t *testing.T, h LearnerHarness) {
	u := h.NewUser(t)
	boom := context.DeadlineExceeded
	err := h.Store.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		require.NoError(t, tx.Profiles().Ensure(ctx, u))
		return boom
	})
	require.ErrorIs(t, err, boom)
	h.tx(t, func(ctx context.Context, tx learner.Tx) {
		_, err := tx.Profiles().Get(ctx, u)
		require.ErrorIs(t, err, learner.ErrNotFound, "a failed transaction leaves no profile")
	})
}
