package internal_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// *learner.Consumer must satisfy amqp.Handler structurally (learner never imports amqp).
var _ amqp.Handler = (*learner.Consumer)(nil)

// *learner.Service must satisfy conversation.LearnerContextProvider structurally (learner never
// imports conversation).
var _ conversation.LearnerContextProvider = (*learner.Service)(nil)

// TestLearnerConsumerClassification drives the real Consumer and feeds each outcome to
// amqp.Classify, the way the AMQP consumer does after Handle (spec 9.7.3).
func TestLearnerConsumerClassification(t *testing.T) {
	ended := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	score := 80

	type scenario struct {
		name  string
		shape func(ev *learner.EvaluationEvidence, st *fakes.LearnerStore, gate *fakes.LearnerUserGate, env *events.Envelope)
		// processed records the event as processed before Handle (a redelivery after a commit).
		processed bool
		want      amqp.Class
		check     func(t *testing.T, err error)
	}
	scenarios := []scenario{
		{name: "success", want: amqp.ClassAck},
		{name: "too few turns", want: amqp.ClassAck, shape: func(ev *learner.EvaluationEvidence, _ *fakes.LearnerStore, _ *fakes.LearnerUserGate, _ *events.Envelope) {
			ev.NTurns = 2
		}},
		{name: "deleted user", want: amqp.ClassAck, shape: func(_ *learner.EvaluationEvidence, _ *fakes.LearnerStore, g *fakes.LearnerUserGate, env *events.Envelope) {
			g.MarkDeleted(env.UserID)
		}, check: func(t *testing.T, err error) { require.ErrorIs(t, err, events.ErrSkippedDeletedUser) }},
		{name: "already processed", want: amqp.ClassAck, processed: true, check: func(t *testing.T, err error) {
			require.ErrorIs(t, err, events.ErrAlreadyProcessed)
		}},
		{name: "broken payload", want: amqp.ClassDead, shape: func(_ *learner.EvaluationEvidence, _ *fakes.LearnerStore, _ *fakes.LearnerUserGate, env *events.Envelope) {
			env.Payload = json.RawMessage(`{"evaluation_id": 5`)
		}},
		{name: "evaluation not committed yet", want: amqp.ClassRetry, shape: func(_ *learner.EvaluationEvidence, _ *fakes.LearnerStore, _ *fakes.LearnerUserGate, env *events.Envelope) {
			env.Payload = mustPayload(t, uuid.New(), uuid.New(), ended)
		}, check: func(t *testing.T, err error) { require.ErrorIs(t, err, learner.ErrEvaluationNotVisible) }},
		{name: "evaluation not completed", want: amqp.ClassDead, shape: func(ev *learner.EvaluationEvidence, _ *fakes.LearnerStore, _ *fakes.LearnerUserGate, _ *events.Envelope) {
			ev.Status = "FAILED"
		}},
		{name: "evaluation of another user", want: amqp.ClassDead, shape: func(ev *learner.EvaluationEvidence, _ *fakes.LearnerStore, _ *fakes.LearnerUserGate, _ *events.Envelope) {
			ev.UserID = uuid.New()
		}},
		{name: "database error", want: amqp.ClassRetry, shape: func(_ *learner.EvaluationEvidence, st *fakes.LearnerStore, _ *fakes.LearnerUserGate, _ *events.Envelope) {
			st.FailNext("SaveState", errors.New("connection reset"))
		}},
		{name: "user lookup error", want: amqp.ClassRetry, shape: func(_ *learner.EvaluationEvidence, _ *fakes.LearnerStore, g *fakes.LearnerUserGate, _ *events.Envelope) {
			g.FailNext("UserDeleted", errors.New("connection reset"))
		}},
		{name: "handler timeout", want: amqp.ClassRetry, shape: func(_ *learner.EvaluationEvidence, st *fakes.LearnerStore, _ *fakes.LearnerUserGate, _ *events.Envelope) {
			st.FailNext("InsertEstimate", context.DeadlineExceeded)
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			uid, evID, sid := uuid.New(), uuid.New(), uuid.New()
			clk := clock.NewFake(ended)
			store := fakes.NewLearnerStore(fakes.WithLearnerClock(clk))
			gate := fakes.NewLearnerUserGate()
			ev := learner.EvaluationEvidence{
				EvaluationID: evID, SessionID: sid, UserID: uid, Status: "COMPLETED", SessionEndedAt: ended, NTurns: 12,
				Scores:             map[learner.Skill]*int{learner.SkillGrammar: &score},
				AdaptiveDifficulty: json.RawMessage(`{}`), ScenarioDifficulty: 3,
			}
			env := events.Envelope{
				EventID: uuid.New(), EventType: events.EvaluationCompleted, EventVersion: 1, UserID: uid, AggregateID: evID,
				Payload: mustPayload(t, evID, sid, ended),
			}
			if sc.shape != nil {
				sc.shape(&ev, store, gate, &env)
			}
			store.SetEvidence(ev)
			if sc.processed {
				require.NoError(t, store.Processed.MarkProcessed(t.Context(), events.QueueLearner, env))
			}
			c := learner.NewConsumer(store, gate, clk, idgen.NewSequence(t), nil, nil)
			err := c.Handle(t.Context(), events.Delivery{Envelope: env, Queue: events.QueueLearner})
			require.Equal(t, sc.want, amqp.Classify(err, false), "err=%v", err)
			if sc.check != nil {
				sc.check(t, err)
			}
		})
	}
}

func mustPayload(t *testing.T, evID, sid uuid.UUID, ended time.Time) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(events.EvaluationCompletedV1{EvaluationID: evID, SessionID: sid, SessionEndedAt: ended, CompletedAt: ended})
	require.NoError(t, err)
	return raw
}
