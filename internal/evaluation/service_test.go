package evaluation_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

type memSource struct {
	sess    evaluation.SessionInfo
	turns   []evaluation.TurnRow
	notFnd  bool
	onTurns func(from, to time.Time)
	onLoad  func(id uuid.UUID)
}

func (m *memSource) LoadSession(_ context.Context, id uuid.UUID) (evaluation.SessionInfo, error) {
	if m.onLoad != nil {
		m.onLoad(id)
	}
	if m.notFnd {
		return evaluation.SessionInfo{}, evaluation.ErrSessionNotFound
	}
	return m.sess, nil
}

func (m *memSource) LoadTurns(_ context.Context, _ uuid.UUID, from, to time.Time) ([]evaluation.TurnRow, error) {
	if m.onTurns != nil {
		m.onTurns(from, to)
	}
	return m.turns, nil
}

type memContent struct {
	pv                     content.PromptVersion
	pvErr                  error
	sv                     content.ScenarioVersion
	topic                  content.Topic
	scenery                content.Scenario
	svErr, topicErr, scErr error
}

func (m *memContent) GetScenarioVersion(context.Context, uuid.UUID) (content.ScenarioVersion, error) {
	return m.sv, m.svErr
}

func (m *memContent) GetTopicByID(context.Context, uuid.UUID) (content.Topic, error) {
	return m.topic, m.topicErr
}

func (m *memContent) GetScenario(context.Context, uuid.UUID) (content.Scenario, error) {
	return m.scenery, m.scErr
}

func (m *memContent) GetPublishedPromptVersion(context.Context, content.PromptKey) (content.PromptVersion, error) {
	return m.pv, m.pvErr
}

type countingRouter struct {
	ev    provider.EvaluationProvider
	calls int
}

func (r *countingRouter) Evaluation() provider.EvaluationProvider {
	r.calls++
	return r.ev
}

type harness struct {
	store  *fakes.EvaluationStore
	src    *memSource
	ct     *memContent
	ev     *scriptedEval
	router *countingRouter
	clk    *clock.Fake
	reg    *prometheus.Registry
	m      *evaluation.Metrics
	env    events.Envelope
	p      events.SessionCompletedV1
	turns  []evaluation.TurnRow
}

func userTurn(seq int, text string, secs int) evaluation.TurnRow {
	start := t0.Add(time.Duration(seq) * 20 * time.Second)
	return evaluation.TurnRow{
		ID: uuid.New(), CreatedAt: start, Seq: seq, Status: ptr("COMPLETED"),
		UserText: ptr(text), AssistantText: ptr("そうですか。"),
		UserStartedAt: &start, UserEndedAt: ptr(start.Add(time.Duration(secs) * time.Second)),
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	schema, err := os.ReadFile("testdata/output_schema.json")
	require.NoError(t, err)
	clk := clock.NewFake(t0.Add(10 * time.Minute))
	sessionID, userID := uuid.New(), uuid.New()
	turns := []evaluation.TurnRow{
		userTurn(1, "昨日、会社に働いていました。", 7),
		userTurn(2, "とても忙しいでした。", 7),
		userTurn(3, "えっと、えっと、あの、はい。", 7),
		userTurn(4, "ありがとうございました。", 7),
	}
	started, ended := t0, t0.Add(5*time.Minute)
	h := &harness{
		store: fakes.NewEvaluationStore(fakes.WithEvaluationClock(clk)),
		clk:   clk, reg: prometheus.NewRegistry(), turns: turns,
		src: &memSource{turns: turns, sess: evaluation.SessionInfo{
			ID: sessionID, UserID: userID, ScenarioID: uuid.New(), ScenarioVersionID: uuid.New(), TopicID: uuid.New(),
			Status: "COMPLETED", StartingAt: ptr(t0.Add(-5 * time.Second)), StartedAt: &started, EndedAt: &ended,
			ConfigSnapshot: json.RawMessage(`{"character":{"name":"葵"}}`), UserStatus: "ACTIVE",
		}},
		ct: &memContent{
			pv: content.PromptVersion{
				ID: uuid.New(), Key: content.PromptEvaluation, Version: 1, SystemTemplate: "SYS {{.RubricVersion}}",
				UserTemplate: ptr("{{.Transcript}}"), OutputSchema: schema,
			},
			sv:    content.ScenarioVersion{DifficultyLevel: 3, Formality: content.FormalityPolite, Goal: "chat"},
			topic: content.Topic{Name: "Work"}, scenery: content.Scenario{Name: "Office"},
		},
		ev: &scriptedEval{steps: []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(evaluation.ExampleOutput)}},
	}
	h.m = evaluation.NewMetrics(h.reg)
	h.router = &countingRouter{ev: h.ev}
	h.p = events.SessionCompletedV1{SessionID: sessionID, UserTurnCount: 4, EndedAt: ended, StartedAt: started}
	raw, err := json.Marshal(h.p)
	require.NoError(t, err)
	h.env = events.Envelope{
		EventID: uuid.New(), EventType: events.SessionCompleted, EventVersion: 1, UserID: userID,
		AggregateType: events.AggregateSession, AggregateID: sessionID, Payload: raw,
	}
	return h
}

func (h *harness) service() *evaluation.Service {
	return evaluation.NewService(evaluation.Deps{
		Store: h.store, Source: h.src, Content: h.ct, Router: h.router, Cfg: evaluation.DefaultConfig(),
		Clock: h.clk, IDs: idgen.NewV7(), Metrics: h.m, Schemas: evaluation.NewSchemaCache(), Log: slog.New(slog.DiscardHandler),
	})
}

func (h *harness) process(t *testing.T) error {
	t.Helper()
	return h.service().Process(t.Context(), h.env, h.p)
}

func (h *harness) row(t *testing.T) evaluation.Evaluation {
	t.Helper()
	e, ok := h.store.Evaluation(h.p.SessionID)
	require.True(t, ok, "evaluation row expected")
	return e
}

func withOutput(t *testing.T, f func(m map[string]any)) string {
	t.Helper()
	return string(mutateOutput(t, f))
}

func TestProcessNormal(t *testing.T) {
	h := newHarness(t)
	extra := withOutput(t, func(m map[string]any) {
		fs := m["findings"].([]any)
		add := func(dim, typ, sev string, turn any, orig, corr any, conf float64) {
			fs = append(fs, map[string]any{
				"dimension": dim, "finding_type": typ, "severity": sev, "turn_seq": turn, "original": orig, "corrected": corr,
				"explanation": "Penjelasan singkat untuk temuan ini.", "pattern_key": nil, "confidence": conf,
			})
		}
		add("TASK_ACHIEVEMENT", "OBJECTIVE_NOT_MET", "MAJOR", nil, nil, nil, 0.9)
		add("VOCABULARY", "STRENGTH", "INFO", nil, nil, nil, 0.9)
		add("FLUENCY", "EXCESSIVE_FILLER", "INFO", 3, "えっと、えっと", nil, 0.9)
		add("NATURALNESS", "UNNATURAL_EXPRESSION", "MINOR", 2, "とても", "非常に", 0.8)
		add("NATURALNESS", "UNNATURAL_EXPRESSION", "MINOR", 1, "昨日", "きのう", 0.8)
		m["findings"] = fs
	})
	h.ev.steps[0] = okJSON(extra)

	require.NoError(t, h.process(t))

	e := h.row(t)
	require.Equal(t, evaluation.StatusCompleted, e.Status)
	require.False(t, e.InsufficientEvidence)
	require.Equal(t, 52, *e.Scores.Grammar)
	require.Equal(t, 55, *e.Scores.Vocabulary)
	require.Equal(t, 45, *e.Scores.Fluency)
	require.Equal(t, 50, *e.Scores.Naturalness)
	require.Equal(t, 60, *e.Scores.TaskAchievement)
	require.InDelta(t, 52.4, *e.OverallScore, 1e-9) // (52+55+45+50+60)/5
	require.Equal(t, 62, *e.ListeningResponseSignal)
	require.Equal(t, 38, *e.InteractionComplexitySignal)
	require.Equal(t, map[string]float64{
		"GRAMMAR": 0.8, "VOCABULARY": 0.7, "FLUENCY": 0.75, "NATURALNESS": 0.7, "TASK_ACHIEVEMENT": 0.8,
		"LISTENING_RESPONSE": 0.7, "INTERACTION_COMPLEXITY": 0.6,
	}, e.DimensionConfidence)
	require.Contains(t, e.DimensionComments["GRAMMAR"], "partikel")
	require.Equal(t, 1, e.LLMCallCount)
	require.Equal(t, "rubric-v1", e.RubricVersion)
	require.Equal(t, h.ct.pv.ID, *e.PromptVersionID)
	require.Equal(t, "OPENAI", strings.ToUpper(*e.Provider))
	require.Equal(t, "gpt-x", *e.Model)
	require.Equal(t, h.env.EventID, e.SourceEventID)
	require.Equal(t, 100, *e.InputTokens)
	require.Equal(t, 4, *e.UserTurnCount)
	require.Equal(t, 28, *e.UserSpeechSeconds)
	require.Equal(t, 300, *e.ActiveSeconds)
	require.Equal(t, 3, *e.ScenarioDifficultyLevel)
	require.Len(t, e.Recommendations, 1)
	require.NotNil(t, e.SummaryFeedback)

	fs, err := h.store.ListFindings(t.Context(), e.ID)
	require.NoError(t, err)
	var got []string
	for i, f := range fs {
		require.Equal(t, i+1, f.Ordinal)
		got = append(got, string(f.Type))
	}
	// MAJOR (turn-less last), MINOR by turn (1 before 2) then dimension (GRAMMAR < NATURALNESS), INFO, STRENGTH last.
	require.Equal(t, []string{"PARTICLE_ERROR", "OBJECTIVE_NOT_MET", "UNNATURAL_EXPRESSION", "CONJUGATION_ERROR", "UNNATURAL_EXPRESSION", "EXCESSIVE_FILLER", "STRENGTH"}, got)
	require.Equal(t, h.turns[0].ID, *fs[0].TurnID)
	require.Equal(t, h.turns[2].ID, *fs[5].TurnID)
	require.Nil(t, fs[1].TurnID)
	require.Nil(t, fs[6].TurnID)

	evs := h.store.Outbox.Events()
	require.Len(t, evs, 1)
	require.Equal(t, events.EvaluationCompleted, evs[0].EventType)
	require.Equal(t, events.AggregateEvaluation, evs[0].AggregateType)
	require.Equal(t, e.ID, evs[0].AggregateID)
	require.Equal(t, events.ProducerWorker, evs[0].Producer)
	require.Equal(t, h.env.UserID, evs[0].UserID)
	pl := evs[0].Payload.(events.EvaluationCompletedV1)
	require.Equal(t, e.ID, pl.EvaluationID)
	require.Equal(t, h.p.SessionID, pl.SessionID)
	require.Equal(t, "rubric-v1", pl.RubricVersion)
	require.Equal(t, 52, *pl.Scores.Grammar)
	require.Equal(t, 62, pl.Signals.ListeningResponse.Score)
	require.Equal(t, map[string]float64{
		"GRAMMAR": 0.8, "VOCABULARY": 0.7, "FLUENCY": 0.75, "NATURALNESS": 0.7, "TASK_ACHIEVEMENT": 0.8,
	}, pl.DimensionConfidence, "only scored dimensions, no signals")
	require.InDelta(t, 0.7, pl.Signals.ListeningResponse.Confidence, 1e-9)
	require.InDelta(t, 0.6, pl.Signals.InteractionComplexity.Confidence, 1e-9)
	require.Equal(t, 28, pl.UserSpeechSeconds)
	require.Equal(t, 3, pl.ScenarioDifficultyLevel)
	require.WithinDuration(t, *h.src.sess.EndedAt, pl.SessionEndedAt, 0)
	require.Equal(t, map[string]events.SeverityCounts{
		"GRAMMAR": {Major: 1, Minor: 1}, "NATURALNESS": {Minor: 2}, "FLUENCY": {Info: 1}, "TASK_ACHIEVEMENT": {Major: 1},
	}, pl.FindingCounts)

	ok, err := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
	require.NoError(t, err)
	require.True(t, ok)

	// the request carries the rendered prompt
	require.Len(t, h.ev.reqs, 1)
	require.Equal(t, "SYS rubric-v1", h.ev.reqs[0].Messages[0].Content)
	require.Contains(t, h.ev.reqs[0].Messages[1].Content, "[T1] USER")

	// latency = completed_at - ended_at (5 minutes)
	require.InDelta(t, 300.0, latencySum(t, h.reg), 0.001)
}

func latencySum(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "kotoki_evaluation_latency_seconds" && len(mf.GetMetric()) > 0 {
			return mf.GetMetric()[0].GetHistogram().GetSampleSum()
		}
	}
	return 0
}

func TestProcessInsufficientEvidence(t *testing.T) {
	cases := []struct {
		name  string
		shape func(h *harness)
	}{
		{"two user turns", func(h *harness) { h.src.turns = h.turns[:2] }},
		{"speech 19s", func(h *harness) {
			h.src.turns = []evaluation.TurnRow{
				userTurn(1, "昨日、会社に働いていました。", 7), userTurn(2, "とても忙しいでした。", 6), userTurn(3, "はい、そうです。", 6),
			}
		}},
		{"active 59s", func(h *harness) { h.src.sess.EndedAt = ptr(t0.Add(59 * time.Second)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.shape(h)
			h.p.UserTurnCount = 3
			require.NoError(t, h.process(t))

			require.Zero(t, h.router.calls, "provider must not be touched")
			require.Empty(t, h.ev.reqs)
			e := h.row(t)
			require.Equal(t, evaluation.StatusCompleted, e.Status)
			require.True(t, e.InsufficientEvidence)
			require.Nil(t, e.Scores.Grammar)
			require.Nil(t, e.OverallScore)
			require.Nil(t, e.ListeningResponseSignal)
			require.Equal(t, evaluation.InsufficientSummary, *e.SummaryFeedback)
			require.Zero(t, e.LLMCallCount)
			require.Empty(t, e.DimensionConfidence)
			fs, err := h.store.ListFindings(t.Context(), e.ID)
			require.NoError(t, err)
			require.Empty(t, fs)

			evs := h.store.Outbox.Events()
			require.Len(t, evs, 1)
			pl := evs[0].Payload.(events.EvaluationCompletedV1)
			require.True(t, pl.InsufficientEvidence)
			require.Nil(t, pl.OverallScore)
			require.Nil(t, pl.Scores.Grammar)
			require.Nil(t, pl.Signals.ListeningResponse)
			require.Empty(t, pl.FindingCounts)
		})
	}
}

func TestProcessAtThresholdCallsLLM(t *testing.T) {
	h := newHarness(t)
	h.src.turns = []evaluation.TurnRow{
		userTurn(1, "昨日、会社に働いていました。", 7), userTurn(2, "とても忙しいでした。", 7), userTurn(3, "はい、そうです。", 6),
	}
	h.src.sess.EndedAt = ptr(t0.Add(60 * time.Second))
	h.p.UserTurnCount = 3
	require.NoError(t, h.process(t))
	require.Equal(t, 1, h.router.calls)
	require.False(t, h.row(t).InsufficientEvidence)
}

// A session that was ACTIVE and then ended FAILED is evaluated like a COMPLETED one.
func TestProcessEvaluatesFailedSession(t *testing.T) {
	h := newHarness(t)
	h.src.sess.Status = "FAILED"
	h.p.FinalStatus = "FAILED"
	require.NoError(t, h.process(t))
	require.Equal(t, evaluation.StatusCompleted, h.row(t).Status)
	require.Equal(t, 1, h.store.Outbox.Len())
}

func TestProcessUserDeleted(t *testing.T) {
	h := newHarness(t)
	h.src.sess.UserStatus = "DELETED"
	err := h.process(t)
	require.ErrorIs(t, err, events.ErrSkippedDeletedUser)
	_, ok := h.store.Evaluation(h.p.SessionID)
	require.False(t, ok)
	require.Zero(t, h.router.calls)
	require.Zero(t, h.store.Outbox.Len())
	done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
	require.True(t, done)
}

func TestProcessSkipsSessionWithoutStart(t *testing.T) {
	for name, shape := range map[string]func(h *harness){
		"status active":    func(h *harness) { h.src.sess.Status = "ACTIVE" },
		"status cancelled": func(h *harness) { h.src.sess.Status = "CANCELLED" },
		"no started_at":    func(h *harness) { h.src.sess.StartedAt = nil },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			shape(h)
			require.NoError(t, h.process(t))
			_, ok := h.store.Evaluation(h.p.SessionID)
			require.False(t, ok)
			require.Zero(t, h.router.calls)
			done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
			require.True(t, done)
			require.Zero(t, h.store.Outbox.Len())
		})
	}
}

func TestProcessPermanentErrors(t *testing.T) {
	var perm *events.PermanentError

	t.Run("user mismatch", func(t *testing.T) {
		h := newHarness(t)
		h.env.UserID = uuid.New()
		require.ErrorAs(t, h.process(t), &perm)
		require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
		_, ok := h.store.Evaluation(h.p.SessionID)
		require.False(t, ok)
	})
	t.Run("session not found", func(t *testing.T) {
		h := newHarness(t)
		h.src.notFnd = true
		require.ErrorAs(t, h.process(t), &perm)
		require.Equal(t, "ENTITY_NOT_FOUND", perm.Code)
	})
	t.Run("transcript purged", func(t *testing.T) {
		h := newHarness(t)
		h.src.turns = nil
		require.ErrorAs(t, h.process(t), &perm)
		require.Equal(t, "TRANSCRIPT_UNAVAILABLE", perm.Code)
		require.Zero(t, h.router.calls)
	})
	t.Run("no turns and no user turns is insufficient", func(t *testing.T) {
		h := newHarness(t)
		h.src.turns = nil
		h.p.UserTurnCount = 0
		require.NoError(t, h.process(t))
		require.True(t, h.row(t).InsufficientEvidence)
	})
}

func TestProcessConfigurationErrorsBeforeClaim(t *testing.T) {
	cases := map[string]func(h *harness){
		"no prompt":       func(h *harness) { h.ct.pvErr = content.ErrNotFound },
		"nil template":    func(h *harness) { h.ct.pv.UserTemplate = nil },
		"nil schema":      func(h *harness) { h.ct.pv.OutputSchema = nil },
		"broken schema":   func(h *harness) { h.ct.pv.OutputSchema = json.RawMessage(`{"type":`) },
		"unknown keyword": func(h *harness) { h.ct.pv.OutputSchema = json.RawMessage(`{"type":"nonsense"}`) },
	}
	for name, shape := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			shape(h)
			var perm *events.PermanentError
			require.ErrorAs(t, h.process(t), &perm)
			require.Equal(t, "CONFIGURATION_ERROR", perm.Code)
			_, ok := h.store.Evaluation(h.p.SessionID)
			require.False(t, ok, "no claim before configuration is verified")
			require.Zero(t, h.router.calls)
		})
	}
}

func TestProcessPromptStoreErrorIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.ct.pvErr = errors.New("db down")
	err := h.process(t)
	var perm *events.PermanentError
	require.Error(t, err)
	require.False(t, errors.As(err, &perm))
}

func TestProcessAlreadyCompleted(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.process(t))
	require.Equal(t, 1, h.store.Outbox.Len())

	// a different event for the same session (e.g. a replay) finds the row COMPLETED
	h.env.EventID = uuid.New()
	require.NoError(t, h.process(t)) // MarkProcessed only: no error, no second event
	require.Equal(t, 1, h.store.Outbox.Len())
	require.Len(t, h.ev.reqs, 1)
	done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
	require.True(t, done)
}

func TestProcessRedeliveryHasOneEffect(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.process(t))
	first := h.row(t)

	err := h.process(t)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	require.Equal(t, 1, h.store.Outbox.Len())
	require.Len(t, h.ev.reqs, 1)
	require.Equal(t, first.AttemptCount, h.row(t).AttemptCount, "completed row is not claimed again")
}

func TestProcessMarkProcessedConflictCommitsNothing(t *testing.T) {
	h := newHarness(t)
	// another pod finished the same event while this one was inside the LLM call
	h.ev.steps[0] = func(provider.StructuredRequest) (provider.StructuredResult, error) {
		require.NoError(t, h.store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
			return tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, h.env)
		}))
		return okJSON(evaluation.ExampleOutput)(provider.StructuredRequest{})
	}
	err := h.process(t)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	require.Zero(t, h.store.Outbox.Len())
	e := h.row(t)
	require.Equal(t, evaluation.StatusRunning, e.Status, "the final transaction rolled back")
	fs, err := h.store.ListFindings(t.Context(), e.ID)
	require.NoError(t, err)
	require.Empty(t, fs)
}

func TestProcessCompleteLostRace(t *testing.T) {
	h := newHarness(t)
	// the row became COMPLETED between claim and the final transaction (another event, same session)
	h.ev.steps[0] = func(provider.StructuredRequest) (provider.StructuredResult, error) {
		e, _ := h.store.Evaluation(h.p.SessionID)
		require.NoError(t, h.store.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
			ok, err := tx.Evaluations().Complete(ctx, e.ID, evaluation.CompletedUpdate{
				InsufficientEvidence: true, SummaryFeedback: "x", CompletedAt: h.clk.Now(),
			})
			require.True(t, ok)
			return err
		}))
		return okJSON(evaluation.ExampleOutput)(provider.StructuredRequest{})
	}
	err := h.process(t)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	require.Zero(t, h.store.Outbox.Len())
	done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
	require.False(t, done, "rolled back together with the failed update")
}

func TestProcessLLMErrorLeavesRunning(t *testing.T) {
	h := newHarness(t)
	perr := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "boom")
	h.ev.steps[0] = failWith(perr)
	err := h.process(t)
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	e := h.row(t)
	require.Equal(t, evaluation.StatusRunning, e.Status)
	require.Equal(t, 1, e.AttemptCount)
	require.Zero(t, h.store.Outbox.Len())
	done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
	require.False(t, done)
}

func TestProcessInvalidOutputIsInvalidModelOutput(t *testing.T) {
	h := newHarness(t)
	bad := invalidSchemaJSON(t)
	h.ev.steps = []func(provider.StructuredRequest) (provider.StructuredResult, error){okJSON(bad), okJSON(bad)}
	err := h.process(t)
	require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput)
	require.Len(t, h.ev.reqs, 2)
}

func TestProcessRetryAfterFailureCompletes(t *testing.T) {
	h := newHarness(t)
	h.ev.steps = []func(provider.StructuredRequest) (provider.StructuredResult, error){
		failWith(errors.New("transient")), okJSON(evaluation.ExampleOutput),
	}
	svc := h.service()
	err := svc.Process(t.Context(), h.env, h.p)
	require.Error(t, err)
	svc.RecordFailure(t.Context(), h.env, err, false)
	require.Equal(t, evaluation.StatusPending, h.row(t).Status)

	require.NoError(t, svc.Process(t.Context(), h.env, h.p))
	e := h.row(t)
	require.Equal(t, evaluation.StatusCompleted, e.Status)
	require.Equal(t, 2, e.AttemptCount)
	require.Nil(t, e.LastError)
	require.Equal(t, 1, h.store.Outbox.Len())
}

func TestRecordFailure(t *testing.T) {
	boom := errors.New(strings.Repeat("界", 3000))

	t.Run("retryable moves to PENDING with truncated last_error", func(t *testing.T) {
		h := newHarness(t)
		svc := h.service()
		h.ev.steps[0] = failWith(boom)
		require.Error(t, svc.Process(t.Context(), h.env, h.p))
		svc.RecordFailure(t.Context(), h.env, boom, false)
		e := h.row(t)
		require.Equal(t, evaluation.StatusPending, e.Status)
		require.Len(t, []rune(*e.LastError), 2000)
		require.Zero(t, h.store.Outbox.Len())
	})

	t.Run("final fails once and emits exactly one event", func(t *testing.T) {
		h := newHarness(t)
		svc := h.service()
		h.ev.steps[0] = failWith(boom)
		require.Error(t, svc.Process(t.Context(), h.env, h.p))
		svc.RecordFailure(t.Context(), h.env, boom, true)
		svc.RecordFailure(t.Context(), h.env, boom, true)

		e := h.row(t)
		require.Equal(t, evaluation.StatusFailed, e.Status)
		require.Equal(t, evaluation.FailInternalError, *e.FailureCode)
		evs := h.store.Outbox.Events()
		require.Len(t, evs, 1)
		require.Equal(t, events.EvaluationFailed, evs[0].EventType)
		require.Equal(t, events.AggregateEvaluation, evs[0].AggregateType)
		require.Equal(t, e.ID, evs[0].AggregateID)
		require.Equal(t, h.env.UserID, evs[0].UserID)
		pl := evs[0].Payload.(events.EvaluationFailedV1)
		require.Equal(t, "INTERNAL_ERROR", pl.FailureCode)
		require.Equal(t, 1, pl.AttemptCount)
		require.Equal(t, h.p.SessionID, pl.SessionID)
		require.Equal(t, e.ID, pl.EvaluationID)
		require.WithinDuration(t, h.clk.Now(), pl.FailedAt, 0)
		done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
		require.False(t, done, "FAILED never writes processed_events")
	})

	t.Run("final after PENDING and failure code from cause", func(t *testing.T) {
		h := newHarness(t)
		svc := h.service()
		h.ev.steps = []func(provider.StructuredRequest) (provider.StructuredResult, error){
			failWith(provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "x")),
		}
		err := svc.Process(t.Context(), h.env, h.p)
		require.Error(t, err)
		svc.RecordFailure(t.Context(), h.env, err, false)
		svc.RecordFailure(t.Context(), h.env, err, true)
		require.Equal(t, evaluation.FailProviderUnavailable, *h.row(t).FailureCode)
		require.Equal(t, "PROVIDER_UNAVAILABLE", h.store.Outbox.Events()[0].Payload.(events.EvaluationFailedV1).FailureCode)
	})

	t.Run("completed row is left alone", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, h.process(t))
		h.service().RecordFailure(t.Context(), h.env, boom, true)
		require.Equal(t, evaluation.StatusCompleted, h.row(t).Status)
		require.Equal(t, 1, h.store.Outbox.Len())
	})

	t.Run("without a row nothing happens", func(t *testing.T) {
		h := newHarness(t)
		h.service().RecordFailure(t.Context(), h.env, boom, true)
		_, ok := h.store.Evaluation(h.p.SessionID)
		require.False(t, ok)
		require.Zero(t, h.store.Outbox.Len())
	})

	t.Run("store error is swallowed", func(t *testing.T) {
		h := newHarness(t)
		h.store.FailNext("MarkPending", errors.New("db"))
		require.NotPanics(t, func() { h.service().RecordFailure(t.Context(), h.env, boom, false) })
	})

	t.Run("undecodable payload is ignored", func(t *testing.T) {
		h := newHarness(t)
		h.env.Payload = json.RawMessage(`{`)
		require.NotPanics(t, func() { h.service().RecordFailure(t.Context(), h.env, boom, true) })
	})
}

func TestFailureCodeOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want evaluation.FailureCode
	}{
		{"transcript", events.Permanent("TRANSCRIPT_UNAVAILABLE", errors.New("x")), evaluation.FailTranscriptUnavailable},
		{"configuration", events.Permanent("CONFIGURATION_ERROR", errors.New("x")), evaluation.FailConfigurationError},
		{"entity not found", events.Permanent("ENTITY_NOT_FOUND", errors.New("x")), evaluation.FailInternalError},
		{"invariant", events.Permanent("INVARIANT_VIOLATION", errors.New("x")), evaluation.FailInternalError},
		{"decode", events.Permanent("DECODE_ERROR", errors.New("x")), evaluation.FailInternalError},
		{"provider", provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "x"), evaluation.FailProviderUnavailable},
		{"wrapped provider", errors.Join(errors.New("ctx"), provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindAuth, "x")), evaluation.FailProviderUnavailable},
		{"invalid output", &evaluation.OutputError{Problems: []string{"p"}}, evaluation.FailInvalidModelOutput},
		{"sentinel", evaluation.ErrInvalidModelOutput, evaluation.FailInvalidModelOutput},
		{"other", errors.New("x"), evaluation.FailInternalError},
		{"deadline", context.DeadlineExceeded, evaluation.FailInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, evaluation.FailureCodeOf(tc.err))
		})
	}
}

func TestProcessSpan(t *testing.T) {
	t.Run("internal error is an error span", func(t *testing.T) {
		rec := testutil.NewTraceRecorder(t)
		h := newHarness(t)
		h.ev.steps[0] = failWith(errors.New("boom"))
		require.Error(t, h.process(t))
		span := rec.Find("evaluation.Process")
		require.Equal(t, "Error", span.Status.Code.String())
	})
	t.Run("success is not an error span", func(t *testing.T) {
		rec := testutil.NewTraceRecorder(t)
		h := newHarness(t)
		require.NoError(t, h.process(t))
		require.NotEqual(t, "Error", rec.Find("evaluation.Process").Status.Code.String())
	})
}

func TestProcessCharacterNameAndFormalityReachPrompt(t *testing.T) {
	h := newHarness(t)
	h.ct.pv.UserTemplate = ptr("{{.CharacterName}}|{{.ExpectedFormality}}|{{.TopicTitle}}|{{.ScenarioTitle}}|{{.UserTurnCount}}")
	h.src.sess.AdaptiveDifficulty = json.RawMessage(`{"formality":"CASUAL"}`)
	require.NoError(t, h.process(t))
	require.Equal(t, "葵|casual (plain form, タメ口)|Work|Office|4", h.ev.reqs[0].Messages[1].Content)
}

func TestProcessTurnWindow(t *testing.T) {
	h := newHarness(t)
	var gotFrom, gotTo time.Time
	h.src.onTurns = func(from, to time.Time) { gotFrom, gotTo = from, to }
	require.NoError(t, h.process(t))
	require.Equal(t, h.src.sess.StartingAt.Add(-time.Minute), gotFrom)
	require.Equal(t, h.src.sess.EndedAt.Add(time.Minute), gotTo)
}

func TestProcessContentLookups(t *testing.T) {
	cases := map[string]func(h *harness, err error){
		"scenario version": func(h *harness, err error) { h.ct.svErr = err },
		"topic":            func(h *harness, err error) { h.ct.topicErr = err },
		"scenario":         func(h *harness, err error) { h.ct.scErr = err },
	}
	for name, set := range cases {
		t.Run(name+" missing is permanent", func(t *testing.T) {
			h := newHarness(t)
			set(h, content.ErrNotFound)
			var perm *events.PermanentError
			require.ErrorAs(t, h.process(t), &perm)
			require.Equal(t, "ENTITY_NOT_FOUND", perm.Code)
			require.Zero(t, h.router.calls)
		})
		t.Run(name+" outage is retryable", func(t *testing.T) {
			h := newHarness(t)
			set(h, errors.New("db down"))
			err := h.process(t)
			var perm *events.PermanentError
			require.Error(t, err)
			require.False(t, errors.As(err, &perm))
		})
	}
}

func TestProcessStoreFailureCommitsNothing(t *testing.T) {
	for _, method := range []string{"Complete", "ReplaceFindings", "Enqueue"} {
		t.Run(method, func(t *testing.T) {
			h := newHarness(t)
			h.ev.steps = append(h.ev.steps, okJSON(evaluation.ExampleOutput))
			boom := errors.New("db down")
			if method == "Enqueue" {
				h.store.Outbox.FailNext(method, boom)
			} else {
				h.store.FailNext(method, boom)
			}
			require.ErrorIs(t, h.process(t), boom)
			require.Zero(t, h.store.Outbox.Len())
			require.Equal(t, evaluation.StatusRunning, h.row(t).Status)
			done, _ := h.store.Processed.IsProcessed(t.Context(), events.QueueEvaluation, h.env.EventID)
			require.False(t, done)

			// the retry completes with exactly one event
			require.NoError(t, h.process(t))
			require.Equal(t, evaluation.StatusCompleted, h.row(t).Status)
			require.Equal(t, 1, h.store.Outbox.Len())
		})
	}
}

func TestProcessWithoutCharacterSnapshot(t *testing.T) {
	h := newHarness(t)
	h.src.sess.ConfigSnapshot = json.RawMessage(`not json`)
	h.ct.pv.UserTemplate = ptr("[{{.CharacterName}}]")
	require.NoError(t, h.process(t))
	require.Equal(t, "[]", h.ev.reqs[0].Messages[1].Content)
}
