package internal_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// *evaluation.Consumer must satisfy amqp.Handler structurally (evaluation never imports amqp).
var _ amqp.Handler = (*evaluation.Consumer)(nil)

type evalSource struct {
	sess  evaluation.SessionInfo
	turns []evaluation.TurnRow
}

func (s *evalSource) LoadSession(context.Context, uuid.UUID) (evaluation.SessionInfo, error) {
	return s.sess, nil
}

func (s *evalSource) LoadTurns(context.Context, uuid.UUID, time.Time, time.Time) ([]evaluation.TurnRow, error) {
	return s.turns, nil
}

type evalContent struct{ pv content.PromptVersion }

func (c evalContent) GetScenarioVersion(context.Context, uuid.UUID) (content.ScenarioVersion, error) {
	return content.ScenarioVersion{DifficultyLevel: 2, Formality: content.FormalityPolite}, nil
}
func (c evalContent) GetTopicByID(context.Context, uuid.UUID) (content.Topic, error) {
	return content.Topic{}, nil
}
func (c evalContent) GetScenario(context.Context, uuid.UUID) (content.Scenario, error) {
	return content.Scenario{}, nil
}
func (c evalContent) GetPublishedPromptVersion(context.Context, content.PromptKey) (content.PromptVersion, error) {
	if c.pv.ID == uuid.Nil {
		return c.pv, content.ErrNotFound
	}
	return c.pv, nil
}

type evalLLM struct {
	result func() (provider.StructuredResult, error)
}

func (evalLLM) Name() provider.Name { return provider.OpenAI }
func (l evalLLM) GenerateJSON(context.Context, provider.StructuredRequest) (provider.StructuredResult, error) {
	return l.result()
}

type evalRouter struct{ ev provider.EvaluationProvider }

func (r evalRouter) Evaluation() provider.EvaluationProvider { return r.ev }

// TestEvaluationConsumerClassification drives the real Consumer and feeds each outcome to
// amqp.Classify, the way the AMQP consumer does after Handle (spec 9.7.3).
func TestEvaluationConsumerClassification(t *testing.T) {
	schema, err := os.ReadFile("evaluation/testdata/output_schema.json")
	require.NoError(t, err)
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	userTurn := func(seq int, text string) evaluation.TurnRow {
		s := start.Add(time.Duration(seq) * 20 * time.Second)
		st, u, a := "COMPLETED", text, "はい"
		e := s.Add(8 * time.Second)
		return evaluation.TurnRow{ID: uuid.New(), CreatedAt: s, Seq: seq, Status: &st, UserText: &u, AssistantText: &a, UserStartedAt: &s, UserEndedAt: &e}
	}
	valid := func() (provider.StructuredResult, error) {
		return provider.StructuredResult{JSON: json.RawMessage(evaluation.ExampleOutput), Provider: provider.OpenAI, Model: "m"}, nil
	}

	type scenario struct {
		name  string
		shape func(src *evalSource, ct *evalContent, llm *evalLLM, st *fakes.EvaluationStore)
		want  amqp.Class
		// processed records the event as processed before Handle (a redelivery after a commit).
		processed bool
		check     func(t *testing.T, err error)
	}
	tmpl := "{{.Transcript}}"
	scenarios := []scenario{
		{name: "success", want: amqp.ClassAck},
		{name: "deleted user", want: amqp.ClassAck, shape: func(s *evalSource, _ *evalContent, _ *evalLLM, _ *fakes.EvaluationStore) {
			s.sess.UserStatus = "DELETED"
		}, check: func(t *testing.T, err error) { require.ErrorIs(t, err, events.ErrSkippedDeletedUser) }},
		{name: "session not ended", want: amqp.ClassAck, shape: func(s *evalSource, _ *evalContent, _ *evalLLM, _ *fakes.EvaluationStore) {
			s.sess.StartedAt = nil
		}},
		{name: "already processed", want: amqp.ClassAck, processed: true, check: func(t *testing.T, err error) { require.ErrorIs(t, err, events.ErrAlreadyProcessed) }},
		{name: "configuration error", want: amqp.ClassDead, shape: func(_ *evalSource, c *evalContent, _ *evalLLM, _ *fakes.EvaluationStore) {
			c.pv.UserTemplate = nil
		}},
		{name: "transcript purged", want: amqp.ClassDead, shape: func(s *evalSource, _ *evalContent, _ *evalLLM, _ *fakes.EvaluationStore) {
			s.turns = nil
		}},
		{name: "provider unavailable", want: amqp.ClassRetry, shape: func(_ *evalSource, _ *evalContent, l *evalLLM, _ *fakes.EvaluationStore) {
			l.result = func() (provider.StructuredResult, error) {
				e := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "x")
				e.Retryable = true
				return provider.StructuredResult{}, e
			}
		}},
		{name: "provider auth", want: amqp.ClassDead, shape: func(_ *evalSource, _ *evalContent, l *evalLLM, _ *fakes.EvaluationStore) {
			l.result = func() (provider.StructuredResult, error) {
				return provider.StructuredResult{}, provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindAuth, "x")
			}
		}},
		{name: "invalid model output", want: amqp.ClassRetry, shape: func(_ *evalSource, _ *evalContent, l *evalLLM, _ *fakes.EvaluationStore) {
			l.result = func() (provider.StructuredResult, error) {
				return provider.StructuredResult{JSON: json.RawMessage(`{}`), Provider: provider.OpenAI}, nil
			}
		}, check: func(t *testing.T, err error) { require.ErrorIs(t, err, evaluation.ErrInvalidModelOutput) }},
		{name: "database error", want: amqp.ClassRetry, shape: func(_ *evalSource, _ *evalContent, _ *evalLLM, st *fakes.EvaluationStore) {
			st.FailNext("Claim", errors.New("connection reset"))
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			src := &evalSource{turns: []evaluation.TurnRow{
				userTurn(1, "昨日、会社に働いていました。"), userTurn(2, "とても忙しいでした。"), userTurn(3, "はい。"),
			}}
			uid := uuid.New()
			ended := start.Add(5 * time.Minute)
			src.sess = evaluation.SessionInfo{ID: uuid.New(), UserID: uid, Status: "COMPLETED", StartedAt: &start, EndedAt: &ended, UserStatus: "ACTIVE"}
			ct := &evalContent{pv: content.PromptVersion{ID: uuid.New(), Version: 1, SystemTemplate: "S", UserTemplate: &tmpl, OutputSchema: schema}}
			llm := &evalLLM{result: valid}
			st := fakes.NewEvaluationStore()
			if sc.shape != nil {
				sc.shape(src, ct, llm, st)
			}
			p := events.SessionCompletedV1{SessionID: src.sess.ID, UserTurnCount: 3, StartedAt: start, EndedAt: ended}
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			env := events.Envelope{EventID: uuid.New(), EventType: events.SessionCompleted, EventVersion: 1, UserID: uid, AggregateID: src.sess.ID, Payload: raw}
			if sc.processed {
				require.NoError(t, st.Processed.MarkProcessed(t.Context(), events.QueueEvaluation, env))
			}
			svc := evaluation.NewService(evaluation.Deps{
				Store: st, Source: src, Content: ct, Router: evalRouter{llm}, Cfg: evaluation.DefaultConfig(),
				Clock: clock.NewFake(ended), IDs: idgen.NewV7(),
			})
			err = evaluation.NewConsumer(svc, evaluation.DefaultConfig()).Handle(t.Context(), events.Delivery{Envelope: env, Queue: events.QueueEvaluation})
			require.Equal(t, sc.want, amqp.Classify(err, false), "err=%v", err)
			if sc.check != nil {
				sc.check(t, err)
			}
		})
	}
}
