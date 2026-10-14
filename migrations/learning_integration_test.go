//go:build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

type learnEnv struct {
	sessEnv
}

func newLearnEnv(t *testing.T) learnEnv { return learnEnv{newSessEnv(t)} }

func (e learnEnv) profile(t *testing.T, o testutil.Row) uuid.UUID {
	uid := testutil.SeedUser(t, e.pool, nil)
	r := testutil.Row{"user_id": uid}
	for k, v := range o {
		r[k] = v
	}
	testutil.MustInsert(t, context.Background(), e.pool, "learner_profiles", r)
	return uid
}

func skillRow(uid uuid.UUID, o testutil.Row) testutil.Row {
	r := testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": uid, "skill": "GRAMMAR",
		"prior_level": 2.0, "prior_confidence": 0.1, "estimated_level": 2.0, "confidence": 0.1,
	}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func (e learnEnv) evaluation(t *testing.T, o testutil.Row) testutil.Row {
	uid := testutil.SeedUser(t, e.pool, nil)
	sid := testutil.SeedSession(t, e.pool, uid, e.c, nil)
	r := evalRow(uid, sid, o)
	testutil.MustInsert(t, context.Background(), e.pool, "evaluations", r)
	return r
}

func evalRow(uid, sid uuid.UUID, o testutil.Row) testutil.Row {
	r := testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "session_id": sid, "user_id": uid, "status": "PENDING",
		"rubric_version": "v1", "source_event_id": uuid.Must(uuid.NewV7()),
	}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func memoryRow(uid uuid.UUID, o testutil.Row) testutil.Row {
	now := time.Now().UTC()
	r := testutil.Row{
		"id": uuid.Must(uuid.NewV7()), "user_id": uid, "category": "PREFERENCE", "memory_key": "likes_tea",
		"content": "お茶が好き。", "display_text": "Suka teh.", "evidence_type": "EXPLICIT_USER_STATEMENT",
		"confidence": 0.8, "importance": 0.5, "first_observed_at": now, "last_observed_at": now,
		"expires_at": now.Add(365 * 24 * time.Hour),
	}
	for k, v := range o {
		r[k] = v
	}
	return r
}

func TestLearningProfileAndSkills(t *testing.T) {
	e := newLearnEnv(t)
	ctx := context.Background()
	ins := func(table string, r testutil.Row) error { return testutil.TryInsert(ctx, e.pool, table, r) }

	t.Run("nickname 31 rejected", func(t *testing.T) {
		uid := testutil.SeedUser(t, e.pool, nil)
		testutil.RequirePgErr(t, ins("learner_profiles", testutil.Row{"user_id": uid, "nickname": strings.Repeat("a", 31)}), "23514", "chk_lp_nickname")
	})
	t.Run("nickname 30 accepted", func(t *testing.T) { e.profile(t, testutil.Row{"nickname": strings.Repeat("a", 30)}) })
	t.Run("speed", func(t *testing.T) {
		uid := testutil.SeedUser(t, e.pool, nil)
		testutil.RequirePgErr(t, ins("learner_profiles", testutil.Row{"user_id": uid, "preferred_speech_speed": "FAST"}), "23514", "chk_lp_speed")
	})

	uid := e.profile(t, nil)
	bad := func(name string, o testutil.Row, con string) {
		t.Run(name, func(t *testing.T) {
			u := e.profile(t, nil)
			testutil.RequirePgErr(t, ins("learner_skill_states", skillRow(u, o)), "23514", con)
		})
	}
	bad("skill READING", testutil.Row{"skill": "READING"}, "chk_lss_skill")
	bad("level 5.5", testutil.Row{"estimated_level": 5.5}, "chk_lss_level")
	bad("trend UP", testutil.Row{"trend": "UP"}, "chk_lss_trend")
	t.Run("duplicate user skill", func(t *testing.T) {
		require.NoError(t, ins("learner_skill_states", skillRow(uid, nil)))
		testutil.RequirePgErr(t, ins("learner_skill_states", skillRow(uid, nil)), "23505", "ux_learner_skill_states_user_skill")
	})
	t.Run("proficiency raw_score 101", func(t *testing.T) {
		ev := e.evaluation(t, nil)
		r := testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "user_id": uid, "skill": "GRAMMAR", "source_evaluation_id": ev["id"],
			"session_id": ev["session_id"], "algorithm_version": 1, "raw_score": 101, "evidence_level": 2.0,
			"evidence_confidence": 0.5, "previous_level": 2.0, "estimated_level": 2.0, "confidence": 0.5,
			"evidence_count": 1, "trend": "STABLE", "assessed_at": time.Now().UTC(),
		}
		testutil.RequirePgErr(t, ins("proficiency_estimates", r), "23514", "chk_pe_raw")
	})
}

func TestMemoriesChecks(t *testing.T) {
	e := newLearnEnv(t)
	ctx := context.Background()
	uid := testutil.SeedUser(t, e.pool, nil)
	ins := func(r testutil.Row) error { return testutil.TryInsert(ctx, e.pool, "memories", r) }
	bad := func(name string, o testutil.Row, state, con string) {
		t.Run(name, func(t *testing.T) { testutil.RequirePgErr(t, ins(memoryRow(uid, o)), state, con) })
	}
	bad("category", testutil.Row{"category": "MISC"}, "23514", "chk_memories_category")
	bad("key format", testutil.Row{"memory_key": "Bad Key"}, "23514", "chk_memories_key_fmt")
	bad("confidence", testutil.Row{"confidence": 1.2}, "23514", "chk_memories_confidence")
	bad("superseded_by on ACTIVE", testutil.Row{"superseded_by": uuid.Must(uuid.NewV7())}, "23514", "chk_memories_superseded_by")
	bad("embedding dim 3", testutil.Row{"memory_key": "dim3", "embedding": "[1,2,3]"}, "22000", "")
	t.Run("two ACTIVE in one slot", func(t *testing.T) {
		require.NoError(t, ins(memoryRow(uid, testutil.Row{"memory_key": "slot_a"})))
		testutil.RequirePgErr(t, ins(memoryRow(uid, testutil.Row{"memory_key": "slot_a"})), "23505", "ux_memories_active_slot")
	})
}

func TestEvaluationsChecks(t *testing.T) {
	e := newLearnEnv(t)
	ctx := context.Background()
	fresh := func(t *testing.T) (uuid.UUID, uuid.UUID) {
		uid := testutil.SeedUser(t, e.pool, nil)
		return uid, testutil.SeedSession(t, e.pool, uid, e.c, nil)
	}
	try := func(t *testing.T, o testutil.Row) error {
		uid, sid := fresh(t)
		return testutil.TryInsert(ctx, e.pool, "evaluations", evalRow(uid, sid, o))
	}
	t.Run("status DONE", func(t *testing.T) {
		testutil.RequirePgErr(t, try(t, testutil.Row{"status": "DONE"}), "23514", "chk_evaluations_status")
	})
	t.Run("FAILED without failure_code", func(t *testing.T) {
		testutil.RequirePgErr(t, try(t, testutil.Row{"status": "FAILED"}), "23514", "chk_evaluations_failed")
	})
	t.Run("COMPLETED without completed_at", func(t *testing.T) {
		testutil.RequirePgErr(t, try(t, testutil.Row{"status": "COMPLETED", "insufficient_evidence": true}), "23514", "chk_evaluations_completed_at")
	})
	t.Run("one evaluation per session", func(t *testing.T) {
		uid, sid := fresh(t)
		testutil.MustInsert(t, ctx, e.pool, "evaluations", evalRow(uid, sid, nil))
		testutil.RequirePgErr(t, testutil.TryInsert(ctx, e.pool, "evaluations", evalRow(uid, sid, nil)), "23505", "uq_evaluations_session")
	})
	t.Run("TestEvaluationsNoUsedFallback", func(t *testing.T) {
		var n int
		require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='evaluations' AND column_name='used_fallback'`).Scan(&n))
		require.Zero(t, n)
	})

	finding := func(ev testutil.Row, ordinal int, o testutil.Row) testutil.Row {
		r := testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "evaluation_id": ev["id"], "session_id": ev["session_id"], "user_id": ev["user_id"],
			"ordinal": ordinal, "dimension": "GRAMMAR", "finding_type": "PARTICLE_ERROR", "severity": "MINOR",
			"explanation": "Partikel salah.", "confidence": 0.9, "source": "LLM",
		}
		for k, v := range o {
			r[k] = v
		}
		return r
	}
	t.Run("finding severity", func(t *testing.T) {
		ev := e.evaluation(t, nil)
		testutil.RequirePgErr(t, testutil.TryInsert(ctx, e.pool, "evaluation_findings", finding(ev, 1, testutil.Row{"severity": "HIGH"})), "23514", "chk_ef_severity")
	})
	t.Run("finding duplicate ordinal", func(t *testing.T) {
		ev := e.evaluation(t, nil)
		testutil.MustInsert(t, ctx, e.pool, "evaluation_findings", finding(ev, 1, nil))
		testutil.RequirePgErr(t, testutil.TryInsert(ctx, e.pool, "evaluation_findings", finding(ev, 1, nil)), "23505", "uq_evaluation_findings_ordinal")
	})
}

func fkNames(t *testing.T, dsn string) map[string]bool {
	out := map[string]bool{}
	for _, n := range queryStrings(t, dsn, `SELECT conname FROM pg_constraint WHERE conname IN ('fk_lss_last_evaluation','fk_pe_source_evaluation')`) {
		out[n] = true
	}
	return out
}

func TestEvaluationFKsAdded(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	require.NoError(t, newMigrator(t, dsn).Up())
	require.Equal(t, map[string]bool{"fk_lss_last_evaluation": true, "fk_pe_source_evaluation": true}, fkNames(t, dsn))
}

func TestDownEvaluationsDropsFKs(t *testing.T) {
	dsn := testutil.NewEmptyPostgresDSN(t)
	m := newMigrator(t, dsn)
	require.NoError(t, m.Migrate(16))
	require.NoError(t, m.Steps(-1))
	require.Empty(t, fkNames(t, dsn))
	require.ElementsMatch(t, []string{"learner_skill_states", "proficiency_estimates"},
		queryStrings(t, dsn, `SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename IN ('learner_skill_states','proficiency_estimates')`))
	require.NoError(t, m.Steps(1))
}
