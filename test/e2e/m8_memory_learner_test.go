//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// M8 end to end (spec 14.5, E8): two sessions in a row on the real binaries. Session 1 is spoken
// through cmd/realtime; the worker (outbox, evaluation, summary, memory, learner; fake providers)
// turns it into an evaluation, memories and a skill profile; session 2 is created and started with
// that state and reads it back through the REST API and the memctx of Redis.

const (
	// m8Scenario is the seeded scenario the M8 scenarios practise: the one with the shortest
	// situation text, so its retrieval query (topic + situation) fits an episode (150 runes).
	m8Scenario  = "01920000-0002-7000-8000-000000000007"
	m8Occupying = "バックエンドエンジニアとして働いている。"
	// m8Score is the score every dimension gets from the fake evaluation in these scenarios, fixed
	// (the embedded fixture hashes a different score per request) so the profile moves the same way
	// on every run: far above the N5 prior of the user.
	m8Score = 100
	// m8Pipeline is E8's bound: from the end of the session to evaluation, memory and learner done.
	m8Pipeline = 30 * time.Second
)

// Utterances of the scripted sessions (fixtures of testdata/audio/synth), each at least five runes.
// The memory worker needs three such turns; the evaluation needs three turns, 20 s of speech and 60 s
// of session (otherwise it is scored with insufficient evidence and the learner has nothing to
// learn from), and the learner four turns, so the full pipeline uses the long utterances of m8Long
// and the scenarios that only look at memory the short m8Short.
var (
	m8Short = []string{"ja_self_intro", "ja_question", "ja_self_intro"}
	m8Long  = []string{"ja_long_utterance", "ja_long_utterance", "ja_self_intro", "ja_question"}
)

// m8Env is one scenario's worker and its fixtures.
type m8Env struct {
	rabbitURL string
	worker    *proc
	scenario  kotokiclient.Scenario
	character uuid.UUID
}

// newM8Env starts the broker and a worker with the given handlers. The worker reads two fixtures
// from a directory of its own: the evaluation rubric with a fixed score, and the memory extraction
// whose episode is the text the runtime will query with, so the fake embedding (hashed trigrams,
// not semantic) finds the episode at the start of the next session.
func newM8Env(t *testing.T, u *user, handlers string) *m8Env {
	t.Helper()
	cat, err := u.Client.Catalog(ctxT(t, 30*time.Second))
	require.NoError(t, err)
	e := &m8Env{rabbitURL: testutil.NewRabbitMQ(t)}
	for _, sc := range cat.Scenarios {
		if sc.ID.String() == m8Scenario {
			e.scenario = sc
		}
	}
	require.NotEqual(t, uuid.Nil, e.scenario.ID, "scenario %s is seeded", m8Scenario)
	require.NotEmpty(t, cat.Characters)
	e.character = cat.Characters[0].ID

	dir := t.TempDir()
	fixtures := filepath.Join(stack.root, "internal", "provider", "fake", "fixtures")
	rubric, err := os.ReadFile(filepath.Join(fixtures, "kotoki_evaluation_rubric_v1.json"))
	require.NoError(t, err)
	rubric = bytes.ReplaceAll(rubric, []byte(`"{{score}}"`), fmt.Appendf(nil, "%d", m8Score))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kotoki_evaluation_rubric_v1.json"), rubric, 0o600))

	raw, err := os.ReadFile(filepath.Join(fixtures, "kotoki_memory_extraction_v1.json"))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	doc["episode"].(map[string]any)["content_ja"] = e.retrievalQuery(t)
	raw, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kotoki_memory_extraction_v1.json"), raw, 0o600))

	e.worker = stack.startWorkerWith(t, e.rabbitURL, handlers, map[string]string{"FAKE_EVAL_FIXTURE_DIR": dir})
	t.Cleanup(e.worker.stop)
	return e
}

// retrievalQuery is the text the runtime embeds at session start (topic name + situation), with
// the line break written as a full stop: an episode content has no control characters.
func (e *m8Env) retrievalQuery(t *testing.T) string {
	t.Helper()
	var topic, situation string
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT t.name_ja, sv.situation FROM scenario_versions sv
		JOIN scenarios s ON s.id = sv.scenario_id JOIN topics t ON t.id = s.topic_id
		WHERE s.id = $1 AND sv.status = 'PUBLISHED'`, e.scenario.ID).Scan(&topic, &situation))
	q := topic + "。" + situation
	require.LessOrEqual(t, len([]rune(q)), 150, "the query must fit an episode")
	return q
}

func (e *m8Env) create(t *testing.T, u *user) kotokiclient.Session {
	t.Helper()
	s, err := u.Client.CreateSession(ctxT(t, 30*time.Second), kotokiclient.CreateSessionRequest{
		TopicID: e.scenario.TopicID, ScenarioID: e.scenario.ID, CharacterID: e.character,
		DurationSeconds: 300, ConversationStartMode: "USER_FIRST",
	})
	require.NoError(t, err)
	require.Equal(t, "RESERVED", s.Status)
	return s
}

// speak plays one user turn per fixture name and ends the session. It returns the memctx the
// runtime had written when the session started: later turns refresh the episodes from what is said,
// so the context at the end is not the one the session began with.
func speak(t *testing.T, u *user, s kotokiclient.Session, names []string) *memory.MemoryContext {
	t.Helper()
	var atStart *memory.MemoryContext
	rec := newRecorder()
	sink := kotokiclient.SinkFuncs{Event: func(ev kotokiclient.Received[protocol.ServerEvent]) {
		rec.OnEvent(ev)
		if ev.Value.Type == "session.started" {
			atStart = memCtx(t, s.ID)
		}
	}, Audio: rec.OnAudio}
	var steps []kotokiclient.Step
	for _, name := range names {
		steps = append(steps, kotokiclient.Speak{Packets: packets(t, name), Label: name}, kotokiclient.WaitAIDone{})
	}
	rep, err := converse(t, u, s, append(steps, kotokiclient.End{}), sink)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)
	return atStart
}

// api calls the REST API as u.
func api(t *testing.T, u *user, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctxT(t, 15*time.Second), method, stack.apiURL+"/api/v1"+path, rd)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+u.Client.AccessToken())
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	}
	return resp.StatusCode, out
}

func listMemories(t *testing.T, u *user) []map[string]any {
	t.Helper()
	code, body := api(t, u, http.MethodGet, "/profile/memories?limit=100", "")
	require.Equal(t, http.StatusOK, code)
	var out []map[string]any
	for _, m := range body["data"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func findMemory(ms []map[string]any, content string) map[string]any {
	for _, m := range ms {
		if m["content"] == content {
			return m
		}
	}
	return nil
}

func skillState(t *testing.T, u *user, skill string) map[string]any {
	t.Helper()
	code, body := api(t, u, http.MethodGet, "/profile/skills", "")
	require.Equal(t, http.StatusOK, code)
	for _, s := range body["skills"].([]any) {
		if m := s.(map[string]any); m["skill"] == skill {
			return m
		}
	}
	t.Fatalf("skill %s not in the profile", skill)
	return nil
}

func evaluationStatus(t *testing.T, u *user, id uuid.UUID) string {
	t.Helper()
	ev, err := u.Client.GetEvaluation(t.Context(), id)
	if err != nil {
		return ""
	}
	return ev.Status
}

// processed counts the processed_events rows of consumer for the event type of the session.
func processed(t *testing.T, consumer, eventType string, sessionID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*) FROM processed_events p JOIN event_outbox o ON o.event_id = p.event_id
		WHERE p.consumer = $1 AND o.event_type = $2 AND (o.aggregate_id = $3 OR o.payload->>'session_id' = $3::text)`, consumer, eventType, sessionID).Scan(&n))
	return n
}

// adaptiveOf is sessions.adaptive_difficulty of a session.
func adaptiveOf(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT adaptive_difficulty FROM sessions WHERE id = $1`, id).Scan(&raw))
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// memCtx is kotoki:session:{id}:memctx, nil when the key is missing.
func memCtx(t *testing.T, id uuid.UUID) *memory.MemoryContext {
	t.Helper()
	raw, err := stack.rdb.Get(t.Context(), "kotoki:session:"+id.String()+":memctx").Bytes()
	if err != nil {
		return nil
	}
	var mc memory.MemoryContext
	require.NoError(t, json.Unmarshal(raw, &mc))
	return &mc
}

func hasMemory(items []memory.ContextItem, content string) bool {
	for _, it := range items {
		if it.Content == content {
			return true
		}
	}
	return false
}

// firstSession registers a user at level N5, starts a worker with the handlers, plays the first
// session with the utterances and returns once the session is COMPLETED. The worker has not
// necessarily finished.
func firstSession(t *testing.T, handlers string, utterances []string) (*user, *m8Env, kotokiclient.Session) {
	t.Helper()
	u := newUser(t)
	code, body := api(t, u, http.MethodPatch, "/profile", `{"nickname":"Budi","self_declared_level":"N5"}`)
	require.Equal(t, http.StatusOK, code, "%v", body)
	e := newM8Env(t, u, handlers)
	s1 := e.create(t, u)
	speak(t, u, s1, utterances)
	waitSession(t, s1.ID, "COMPLETED")
	return u, e, s1
}

const (
	fullHandlers   = "outbox,evaluation,summary,memory,learner"
	memoryHandlers = "outbox,memory"
)

// memoriesDone is true when the memory worker has stored the memories of the session. The helpers
// read the tables: the REST API rate-limits a 50 ms poll.
func memoriesDone(t *testing.T, u *user, s kotokiclient.Session) bool {
	t.Helper()
	var n int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*) FROM memories WHERE user_id = $1 AND source_session_id = $2`, u.ID, s.ID).Scan(&n))
	return n > 0
}

// pipelineDone is true when the evaluation is COMPLETED with scores, the memories are extracted and
// the skill profile counts the session.
func pipelineDone(t *testing.T, u *user, s kotokiclient.Session) bool {
	t.Helper()
	var evaluated, estimates int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM evaluations WHERE session_id = $1 AND status = 'COMPLETED' AND NOT insufficient_evidence),
		(SELECT count(*) FROM proficiency_estimates WHERE user_id = $2 AND session_id = $1)`, s.ID, u.ID).Scan(&evaluated, &estimates))
	return evaluated == 1 && estimates > 0 && memoriesDone(t, u, s)
}

func TestE2E_MemoryExtractedAndRetrievedOnNextSession(t *testing.T) {
	u, e, s1 := firstSession(t, fullHandlers, m8Long)
	eventually(t, func() bool { return pipelineDone(t, u, s1) }, "evaluation, memory and learner finished for session 1")

	// the evaluation, the memories and the profile that session 1 left
	assert.Equal(t, "COMPLETED", evaluationStatus(t, u, s1.ID))
	mems := listMemories(t, u)
	occ := findMemory(mems, m8Occupying)
	require.NotNil(t, occ, "occupation memory in %v", mems)
	assert.Equal(t, "PROFILE_FACT", occ["category"])
	assert.Equal(t, "Bekerja sebagai backend engineer.", occ["display_text"], "Japanese content, Indonesian display text")
	assert.Equal(t, s1.ID.String(), occ["source_session_id"])
	gr := skillState(t, u, "GRAMMAR")
	assert.Equal(t, float64(1), gr["evidence_count"])
	assert.Greater(t, gr["estimated_level"], float64(1), "the N5 grammar prior of 1.0 rose after a far better evaluation")
	assert.Equal(t, 1, processed(t, "memory.session_completed", "session.completed", s1.ID))
	assert.Equal(t, 1, processed(t, "learner.evaluation_completed", "evaluation.completed", s1.ID))

	code, prof := api(t, u, http.MethodGet, "/profile", "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, float64(1), prof["completed_session_count"])

	// session 2: its adaptive difficulty comes from the profile that session 1 changed
	ad1 := adaptiveOf(t, s1.ID)
	s2 := e.create(t, u)
	ad2 := adaptiveOf(t, s2.ID)
	assert.EqualValues(t, 1, ad2["algorithm_version"])
	assert.NotNil(t, ad2["scenario_difficulty"])
	assert.NotEqual(t, ad1, ad2, "the profile after session 1 changes the difficulty of session 2")
	var snapshot map[string]any
	var rawSnap []byte
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT config_snapshot FROM sessions WHERE id = $1`, s2.ID).Scan(&rawSnap))
	require.NoError(t, json.Unmarshal(rawSnap, &snapshot))
	assert.NotEmpty(t, snapshot, "config_snapshot is filled")

	// the runtime loads the memories into its context at session.start
	mc := speak(t, u, s2, m8Short[:1])
	require.NotNil(t, mc, "kotoki:session:{id}:memctx exists")
	assert.Equal(t, u.ID, mc.UserID)
	assert.False(t, mc.Degraded)
	assert.GreaterOrEqual(t, len(mc.Core), 1)
	assert.True(t, hasMemory(mc.Core, m8Occupying), "occupation is in the core memory: %+v", mc.Core)
	require.GreaterOrEqual(t, len(mc.Episodic), 1, "the episode of session 1 is recalled")
	assert.Equal(t, memory.CategoryEpisodic, mc.Episodic[0].Category)

	// session 2 ended normally: balance and ledger are intact
	waitSession(t, s2.ID, "COMPLETED")
	testutil.AssertReconciled(t, stack.pool)
}

func TestE2E_MemoryIsolationBetweenUsers(t *testing.T) {
	a, e, s1 := firstSession(t, memoryHandlers, m8Short)
	eventually(t, func() bool { return memoriesDone(t, a, s1) }, "memories of A")
	require.NotEmpty(t, listMemories(t, a))

	b := newUser(t)
	sb := e.create(t, b)
	mc := speak(t, b, sb, m8Short[:1])
	waitSession(t, sb.ID, "COMPLETED")
	require.NotNil(t, mc, "B's memctx is written even when empty")
	assert.Equal(t, b.ID, mc.UserID)
	assert.Empty(t, mc.Core, "nothing of A in B's core memory")
	assert.Empty(t, mc.Episodic, "nothing of A in B's episodes")
	assert.Empty(t, listMemories(t, b))
	assert.Zero(t, skillState(t, b, "GRAMMAR")["evidence_count"])

	// B cannot touch the memories of A
	occ := findMemory(listMemories(t, a), m8Occupying)
	require.NotNil(t, occ)
	code, body := api(t, b, http.MethodDelete, "/profile/memories/"+occ["id"].(string), "")
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "MEMORY_NOT_FOUND", body["error"].(map[string]any)["code"])
	assert.NotNil(t, findMemory(listMemories(t, a), m8Occupying))
}

func TestE2E_DeleteMemoryTakesEffect(t *testing.T) {
	u, e, s1 := firstSession(t, memoryHandlers, m8Short)
	eventually(t, func() bool { return memoriesDone(t, u, s1) }, "memories of session 1")
	occ := findMemory(listMemories(t, u), m8Occupying)
	require.NotNil(t, occ)
	id := occ["id"].(string)

	verBefore, _ := stack.rdb.Get(t.Context(), "kotoki:user:"+u.ID.String()+":memver").Int64()
	code, _ := api(t, u, http.MethodDelete, "/profile/memories/"+id, "")
	require.Equal(t, http.StatusNoContent, code)
	verAfter, err := stack.rdb.Get(t.Context(), "kotoki:user:"+u.ID.String()+":memver").Int64()
	require.NoError(t, err)
	assert.Greater(t, verAfter, verBefore, "memver moves so running sessions drop the memory")
	assert.Nil(t, findMemory(listMemories(t, u), m8Occupying))

	// session 3 does not load it, and the worker does not recreate the slot after it
	s3 := e.create(t, u)
	mc := speak(t, u, s3, m8Short)
	waitSession(t, s3.ID, "COMPLETED")
	require.NotNil(t, mc)
	assert.False(t, hasMemory(mc.Core, m8Occupying), "the deleted memory is not in the context: %+v", mc.Core)
	assert.NotEmpty(t, mc.Core, "the other memories are")
	eventually(t, func() bool { return processed(t, "memory.session_completed", "session.completed", s3.ID) == 1 }, "memory worker handled session 3")
	assert.Nil(t, findMemory(listMemories(t, u), m8Occupying), "tombstone: the worker did not recreate the slot")
	var tomb int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*) FROM memories WHERE user_id = $1 AND memory_key = 'occupation' AND status = 'DELETED'`, u.ID).Scan(&tomb))
	assert.Equal(t, 1, tomb)
}

// TestE2E_PipelineWithin30s is E8 (spec 14.5): from the end of the session, the evaluation is
// COMPLETED, the memory extracted and the learner updated within 30 s.
func TestE2E_PipelineWithin30s(t *testing.T) {
	u, _, s1 := firstSession(t, fullHandlers, m8Long)
	end := time.Now()
	require.Eventually(t, func() bool { return pipelineDone(t, u, s1) }, m8Pipeline, eventuallyTick,
		"evaluation, memory and learner within %s of the end of the session", m8Pipeline)
	t.Logf("E8: pipeline finished %.1f s after the session ended", time.Since(end).Seconds())
}
