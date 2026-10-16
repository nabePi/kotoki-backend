//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// The M8 gate: the whole post-session pipeline on real containers. session.completed goes through
// the outbox and RabbitMQ to the real evaluation, memory and learner consumers (fake providers);
// the results are read back through the real REST endpoints and the real retriever.

const (
	gateEvalConsumer = "evaluation.session_completed"
	gateInterview    = "面接練習（バックエンドエンジニア）。Goを選んだ理由とマイクロサービスの経験を説明した。"
)

// gateRig is profileHTTP (Postgres, Redis, real services, REST) plus a running pipeline on RabbitMQ.
type gateRig struct {
	*profileHTTP
	url string
	adm *testutil.RabbitAdmin
}

// scoredEval answers like the fake evaluation provider but with every dimension scored by the n-th
// entry of scores (call order = session order, the gate announces one session at a time).
func scoredEval(scores ...int) *scriptedEval {
	s := &scriptedEval{fallback: newFakeEval()}
	for _, sc := range scores {
		s.steps = append(s.steps, func(req provider.StructuredRequest) (provider.StructuredResult, error) {
			res, err := newFakeEval().GenerateJSON(context.Background(), req)
			if err != nil {
				return res, err
			}
			var doc map[string]any
			if err := json.Unmarshal(res.JSON, &doc); err != nil {
				return res, err
			}
			for _, group := range []string{"scores", "signals"} {
				m, _ := doc[group].(map[string]any)
				for _, v := range m {
					v.(map[string]any)["score"] = sc
				}
			}
			res.JSON, err = json.Marshal(doc)
			return res, err
		})
	}
	return s
}

// newGateRig starts the outbox publisher and the evaluation, memory and learner consumers.
func newGateRig(t *testing.T, eval *scriptedEval) *gateRig {
	t.Helper()
	h := newProfileHTTP(t)
	url := testutil.NewRabbitMQ(t)
	log := slog.New(slog.DiscardHandler)
	pool, clk, ids := h.rig.pool, clock.System{}, idgen.NewV7()
	store := postgres.NewStore(pool)

	am := amqp.NewMetrics(prometheus.NewRegistry())
	confirm := amqp.NewConfirmPublisher(m6Dial(t, url), 5*time.Second)
	outbox := amqp.NewOutboxPublisher(pool, confirm, amqp.PublisherConfig{
		PollInterval: 100 * time.Millisecond, ConfirmTimeout: 5 * time.Second, TxTimeout: 15 * time.Second, BatchSize: 100, MaxAttempts: 10,
	}, am, log)

	evalCfg := evaluation.DefaultConfig()
	evalSvc := evaluation.NewService(evaluation.Deps{
		Store: store.EvaluationStore(), Source: postgres.NewEvaluationSource(pool), Content: postgres.NewContentRepo(pool),
		Router: eval, Cfg: evalCfg, Clock: clk, IDs: ids, Schemas: evaluation.NewSchemaCache(), Log: log,
	})
	memCons := memory.NewConsumer(memory.ConsumerDeps{
		Store: store.MemoryStore(), Source: postgres.NewMemorySource(pool, clk),
		Settings: postgres.NewMemoryProfileSettings(pool, ids, clk), Prompts: postgres.NewMemoryPromptSource(pool),
		Provider: h.rig.prov, MemVer: h.bump, Applier: memory.NewApplier(ids, clk, nil), Clock: clk, IDs: ids,
	})
	learnCons := learner.NewConsumer(store.LearnerStore(), store.LearnerUserGate(), clk, ids, nil, nil)
	processed := postgres.NewProcessedEventsReader(pool)
	consumers := []*amqp.Consumer{
		amqp.NewConsumer(evaluation.NewConsumer(evalSvc, evalCfg), m6Dial(t, url), confirm, processed, am, log, clk),
		amqp.NewConsumer(memCons, m6Dial(t, url), confirm, processed, am, log, clk),
		amqp.NewConsumer(learnCons, m6Dial(t, url), confirm, processed, am, log, clk),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 1+len(consumers))
	go func() { _ = outbox.Run(ctx); done <- struct{}{} }()
	for _, c := range consumers {
		go func() { _ = c.Run(ctx); done <- struct{}{} }()
	}
	t.Cleanup(func() {
		for _, c := range consumers {
			_ = c.Shutdown(context.Background(), 0)
		}
		cancel()
		for range 1 + len(consumers) {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("gate goroutine did not stop")
				return
			}
		}
	})
	return &gateRig{profileHTTP: h, url: url, adm: testutil.RabbitMQAdmin(t, url)}
}

// finish seeds a finished session of userID (zero: new user) that ended agoSeconds-ish ago and
// announces it through the outbox.
func (g *gateRig) finish(userID uuid.UUID, startedAgo int) testutil.FinishedSession {
	g.t.Helper()
	fs := testutil.SeedFinishedSession(g.t, g.rig.pool, testutil.FinishedSessionOpts{
		UserID: userID, Turns: seedTurns(5, 25), ActiveSeconds: 120, StartedSecondsAgo: startedAgo,
	})
	announce(g.t, g.rig.pool, fs)
	return fs
}

// settled waits until the three consumers have processed the session and its evaluation reached the
// learner.
func (g *gateRig) settled(fs testutil.FinishedSession) {
	g.t.Helper()
	pool := g.rig.pool
	require.Eventually(g.t, func() bool {
		return count(g.t, pool, `SELECT count(*) FROM processed_events WHERE event_id = $1`, fs.Envelope.EventID) >= 2 &&
			count(g.t, pool, `SELECT count(*) FROM proficiency_estimates WHERE session_id = $1`, fs.ID) > 0
	}, 40*time.Second, 100*time.Millisecond, "evaluation, memory and learner processed session %s", fs.ID)
}

func (g *gateRig) skills(uid uuid.UUID) map[string]map[string]any {
	g.t.Helper()
	r := g.call(uid, http.MethodGet, profileBase+"/skills", "")
	require.Equal(g.t, 200, r.code, string(r.body))
	out := map[string]map[string]any{}
	for _, s := range r.json(g.t)["skills"].([]any) {
		m := s.(map[string]any)
		out[m["skill"].(string)] = m
	}
	return out
}

func (g *gateRig) memories(uid uuid.UUID) []map[string]any {
	g.t.Helper()
	r := g.call(uid, http.MethodGet, memoriesBase+"?limit=100", "")
	require.Equal(g.t, 200, r.code, string(r.body))
	var out []map[string]any
	for _, m := range r.json(g.t)["data"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func memoryByContent(ms []map[string]any, content string) map[string]any {
	for _, m := range ms {
		if m["content"] == content {
			return m
		}
	}
	return nil
}

func TestM8Gate_EvaluationCompletedChangesSkillState(t *testing.T) {
	g := newGateRig(t, scoredEval(70, 85, 100))
	uid := testutil.SeedUser(t, g.rig.pool, nil)
	require.Equal(t, 200, g.call(uid, http.MethodPatch, profileBase, `{"nickname":"Budi","self_declared_level":"N5"}`).code)
	prior := g.skills(uid)["GRAMMAR"]
	require.Equal(t, float64(1), prior["estimated_level"], "N5 prior")
	require.Zero(t, prior["evidence_count"])

	var levels []float64
	for i, ago := range []int{3000, 2000, 1000} {
		fs := g.finish(uid, ago)
		g.settled(fs)
		sk := g.skills(uid)["GRAMMAR"]
		require.Equal(t, float64(i+1), sk["evidence_count"], "evidence after session %d", i+1)
		levels = append(levels, sk["estimated_level"].(float64))
		if i < 2 {
			require.Equal(t, "INSUFFICIENT_DATA", sk["trend"], "trend after session %d", i+1)
		}
	}
	require.NotEqual(t, prior["estimated_level"], levels[0], "the first evaluation moves the estimate")
	require.Greater(t, levels[0], 1.0, "the first evaluation raises the N5 prior")
	require.Greater(t, levels[1], levels[0], "rising scores raise the level: %v", levels)
	require.Greater(t, levels[2], levels[1], "rising scores raise the level: %v", levels)
	require.Equal(t, "IMPROVING", g.skills(uid)["GRAMMAR"]["trend"], "levels %v", levels)

	// the learner consumer ran exactly once per evaluation
	require.Equal(t, 3, count(t, g.rig.pool, `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1 AND skill = 'GRAMMAR'`, uid))
	require.Equal(t, 3, count(t, g.rig.pool, `SELECT count(*) FROM processed_events WHERE consumer = 'learner.evaluation_completed'`))
	hist := g.call(uid, http.MethodGet, profileBase+"/skills/GRAMMAR/history", "").json(t)
	require.Len(t, hist["data"], 3)
}

func TestM8Gate_MemoryExtractedThenRetrievedNextSession(t *testing.T) {
	g := newGateRig(t, withFake())
	first := g.finish(uuid.Nil, 3000)
	g.settled(first)
	uid := first.UserID

	require.Eventually(t, func() bool {
		return memoryByContent(g.memories(uid), "バックエンドエンジニアとして働いている。") != nil
	},
		10*time.Second, 50*time.Millisecond, "occupation listed")
	occ := memoryByContent(g.memories(uid), "バックエンドエンジニアとして働いている。")
	require.Equal(t, "PROFILE_FACT", occ["category"])
	require.Equal(t, "Bekerja sebagai backend engineer.", occ["display_text"], "Japanese content, Indonesian display text")
	require.Equal(t, first.ID.String(), occ["source_session_id"])
	require.Len(t, g.memories(uid), 4, "three memories and the episode")

	// a new session: the memory and the episode of the old one are in its context, and memctx is written
	mc, sid := g.build(uid, gateInterview)
	require.False(t, mc.Degraded)
	var ids []uuid.UUID
	for _, it := range mc.Core {
		ids = append(ids, it.ID)
	}
	require.Contains(t, ids, uuid.MustParse(occ["id"].(string)))
	require.Len(t, mc.Episodic, 1)
	require.Equal(t, memory.CategoryEpisodic, mc.Episodic[0].Category)
	cached, err := g.cache.GetContext(context.Background(), sid)
	require.NoError(t, err)
	require.NotNil(t, cached, "memctx is filled in Redis")
	require.Equal(t, mc.Core, cached.Core)
	require.Equal(t, mc.Episodic, cached.Episodic)
	require.Equal(t, int64(1), mc.UserMemVer)
}

func TestM8Gate_UserIsolationAcrossWholePipeline(t *testing.T) {
	g := newGateRig(t, withFake())
	a := g.finish(uuid.Nil, 3000)
	b := g.finish(uuid.Nil, 3000) // identical transcript, hence identical embeddings
	g.settled(a)
	g.settled(b)

	memA, memB := g.memories(a.UserID), g.memories(b.UserID)
	require.Len(t, memA, 4)
	require.Len(t, memB, 4)
	idsA := map[any]bool{}
	for _, m := range memA {
		idsA[m["id"]] = true
	}
	for _, m := range memB {
		require.False(t, idsA[m["id"]], "no memory of A in the list of B")
		require.Equal(t, b.ID.String(), m["source_session_id"])
	}

	// the contexts built for the two users hold only their own rows (same text, same vectors)
	mcA, sidA := g.build(a.UserID, gateInterview)
	mcB, sidB := g.build(b.UserID, gateInterview)
	own := func(uid uuid.UUID, mc memory.MemoryContext) {
		for _, it := range append(append([]memory.ContextItem{}, mc.Core...), mc.Episodic...) {
			require.Equal(t, 1, count(t, g.rig.pool, `SELECT count(*) FROM memories WHERE id = $1 AND user_id = $2`, it.ID, uid), "memory %s is not of user %s", it.ID, uid)
		}
	}
	own(a.UserID, mcA)
	own(b.UserID, mcB)
	require.NotEmpty(t, mcA.Core)
	require.NotEmpty(t, mcB.Core)
	cA, err := g.cache.GetContext(context.Background(), sidA)
	require.NoError(t, err)
	cB, err := g.cache.GetContext(context.Background(), sidB)
	require.NoError(t, err)
	require.NotEqual(t, cA.Core[0].ID, cB.Core[0].ID)

	// skill states and history
	for _, u := range []uuid.UUID{a.UserID, b.UserID} {
		require.Equal(t, float64(1), g.skills(u)["GRAMMAR"]["evidence_count"])
		require.Len(t, g.call(u, http.MethodGet, profileBase+"/skills/GRAMMAR/history", "").json(t)["data"], 1)
	}

	// B cannot delete a memory of A: 404, and A keeps it
	theirs := memA[0]["id"].(string)
	r := g.call(b.UserID, http.MethodDelete, memoriesBase+"/"+theirs, "")
	require.Equal(t, 404, r.code, string(r.body))
	require.Equal(t, "MEMORY_NOT_FOUND", r.errCode(t))
	require.Len(t, g.memories(a.UserID), 4)
	// deleting all of B leaves A untouched
	require.Equal(t, 204, g.call(b.UserID, http.MethodDelete, memoriesBase, "").code)
	require.Empty(t, g.memories(b.UserID))
	require.Len(t, g.memories(a.UserID), 4)
	require.Equal(t, float64(1), g.skills(a.UserID)["GRAMMAR"]["evidence_count"])

	// an event whose user_id is not the owner of the session goes to the dead-letter queue and
	// writes nothing for either user
	forged := events.NewSessionCompleted(uuid.Must(uuid.NewV7()), a.Payload.EndedAt, events.ProducerRealtime, b.UserID, a.Payload)
	require.NoError(t, g.store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, forged)
	}))
	require.Eventually(t, func() bool { return g.adm.QueueMessages(amqp.DLQName(events.QueueMemory)) == 1 },
		30*time.Second, 100*time.Millisecond, "memory dead-letters the forged event")
	require.Empty(t, g.memories(b.UserID), "B got nothing from A's session")
	require.Len(t, g.memories(a.UserID), 4)
	require.Equal(t, 0, count(t, g.rig.pool, `SELECT count(*) FROM memories WHERE user_id = $1 AND source_session_id = $2`, b.UserID, a.ID))
}

func TestM8Gate_MemoryDisabledAndDeleteTakeEffectNextTurn(t *testing.T) {
	g := newGateRig(t, withFake())
	first := g.finish(uuid.Nil, 3000)
	g.settled(first)
	uid := first.UserID
	occ := memoryByContent(g.memories(uid), "バックエンドエンジニアとして働いている。")
	require.NotNil(t, occ)
	occID := uuid.MustParse(occ["id"].(string))

	// the session that is running has the memory loaded
	mc, sid := g.build(uid, gateInterview)
	mc.SessionID = sid
	require.True(t, hasItem(mc.Core, occID))

	// the user deletes it: the running session drops it on the next reload, a new one never loads it
	require.Equal(t, 204, g.call(uid, http.MethodDelete, memoriesBase+"/"+occID.String(), "").code)
	changed, err := g.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, hasItem(mc.Core, occID))
	next, _ := g.build(uid, gateInterview)
	require.False(t, hasItem(next.Core, occID))

	// memory_enabled=false (spec 11.9: it applies to sessions created afterwards): a new session starts empty
	require.Equal(t, 200, g.call(uid, http.MethodPatch, profileBase, `{"preferences":{"memory_enabled":false}}`).code)
	off, _ := g.build(uid, gateInterview)
	require.Empty(t, off.Core)
	require.Empty(t, off.Episodic)
	require.False(t, off.Degraded)

	// the next session's extraction is skipped and the deleted slot stays a tombstone
	llm := g.rig.prov.llmCalls.Load()
	second := g.finish(uid, 2000)
	require.Eventually(t, func() bool {
		return count(t, g.rig.pool, `SELECT count(*) FROM processed_events WHERE consumer = 'memory.session_completed' AND event_id = $1`, second.Envelope.EventID) == 1
	},
		30*time.Second, 100*time.Millisecond)
	require.Equal(t, llm, g.rig.prov.llmCalls.Load(), "no extraction call while memory is off")
	require.Equal(t, 0, count(t, g.rig.pool, `SELECT count(*) FROM memories WHERE user_id = $1 AND memory_key = 'occupation' AND status = 'ACTIVE'`, uid))
	require.Equal(t, 1, count(t, g.rig.pool, `SELECT count(*) FROM memories WHERE user_id = $1 AND memory_key = 'occupation' AND status = 'DELETED'`, uid))
}

func hasItem(items []memory.ContextItem, id uuid.UUID) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func TestM8Gate_EmbeddingProviderDown_SessionStillStarts(t *testing.T) {
	g := newGateRig(t, withFake())
	first := g.finish(uuid.Nil, 3000)
	g.settled(first)

	g.rig.prov.embedErr.Store(&provider.Error{Kind: provider.KindUnavailable, Retryable: true, Message: "embedding down"})
	mc, sid := g.build(first.UserID, gateInterview)
	require.True(t, mc.Degraded, "degraded, not failed")
	require.NotEmpty(t, mc.Core, "core memories do not need an embedding")
	require.Empty(t, mc.Episodic)
	cached, err := g.cache.GetContext(context.Background(), sid)
	require.NoError(t, err)
	require.NotNil(t, cached)

	// POST /sessions does not depend on the embedding provider (it is never called there)
	l := newLifecycle(t)
	_, authz := l.newUser()
	created := l.create(authz, "gate-embed-down-0001", "AI_FIRST", "600")
	require.Equal(t, http.StatusCreated, created.code, string(created.body))
}
