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
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/summary"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// *summary.Consumer must satisfy amqp.Handler structurally (summary never imports amqp).
var _ amqp.Handler = (*summary.Consumer)(nil)

type sumSource struct {
	info  summary.SessionInfo
	turns []summary.TurnRow
}

func (s *sumSource) LoadSession(context.Context, uuid.UUID) (summary.SessionInfo, error) {
	return s.info, nil
}

func (s *sumSource) LoadTurns(context.Context, uuid.UUID, time.Time, time.Time) ([]summary.TurnRow, error) {
	return s.turns, nil
}

type sumPrompts struct{ pr summary.Prompt }

func (p sumPrompts) PublishedPrompt(context.Context) (summary.Prompt, error) { return p.pr, nil }

type sumStore struct {
	fakes.FailInjector
	processed *fakes.Processed
}

type sumTx struct{ s *sumStore }

func (s *sumStore) WithinTx(ctx context.Context, fn func(context.Context, summary.Tx) error) error {
	return fn(ctx, sumTx{s})
}
func (t sumTx) Processed() events.ProcessedEvents { return t.s.processed }
func (t sumTx) Sessions() summary.SessionWriter   { return sumWriter(t) }

type sumWriter struct{ s *sumStore }

func (w sumWriter) SetSummary(context.Context, uuid.UUID, string) (bool, error) {
	return true, w.s.Take("SetSummary")
}

type sumRouter struct {
	reply func() (provider.TextResult, error)
}

func (r sumRouter) CompleteText(context.Context, provider.TextRequest) (provider.TextResult, error) {
	return r.reply()
}

// TestSummaryConsumerClassification drives the real Consumer and feeds each outcome to
// amqp.Classify, the way the AMQP consumer does after Handle (spec 9.7.3).
func TestSummaryConsumerClassification(t *testing.T) {
	completed := "COMPLETED"
	ok := func() (provider.TextResult, error) {
		return provider.TextResult{Text: "ユーザーは旅行の話をした。", Provider: provider.OpenAI, Model: "m"}, nil
	}
	type scenario struct {
		name      string
		shape     func(src *sumSource, pr *sumPrompts, rt *sumRouter, st *sumStore, p *events.SessionCompletedV1)
		want      amqp.Class
		processed bool // the event was already recorded (a redelivery after a commit)
		check     func(t *testing.T, err error)
	}
	scenarios := []scenario{
		{name: "success", want: amqp.ClassAck},
		{name: "zero user turns", want: amqp.ClassAck, shape: func(_ *sumSource, _ *sumPrompts, _ *sumRouter, _ *sumStore, p *events.SessionCompletedV1) {
			p.UserTurnCount = 0
		}},
		{name: "deleted user", want: amqp.ClassAck, shape: func(s *sumSource, _ *sumPrompts, _ *sumRouter, _ *sumStore, _ *events.SessionCompletedV1) {
			s.info.UserStatus = "DELETED"
		}, check: func(t *testing.T, err error) { require.ErrorIs(t, err, events.ErrSkippedDeletedUser) }},
		{name: "already processed", want: amqp.ClassAck, processed: true, check: func(t *testing.T, err error) {
			require.ErrorIs(t, err, events.ErrAlreadyProcessed)
		}},
		{name: "configuration error", want: amqp.ClassDead, shape: func(_ *sumSource, pr *sumPrompts, _ *sumRouter, _ *sumStore, _ *events.SessionCompletedV1) {
			pr.pr.Template.User = ""
		}},
		{name: "transcript purged", want: amqp.ClassDead, shape: func(s *sumSource, _ *sumPrompts, _ *sumRouter, _ *sumStore, _ *events.SessionCompletedV1) {
			s.turns = nil
		}},
		{name: "provider unavailable", want: amqp.ClassRetry, shape: func(_ *sumSource, _ *sumPrompts, rt *sumRouter, _ *sumStore, _ *events.SessionCompletedV1) {
			rt.reply = func() (provider.TextResult, error) {
				e := provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindUnavailable, "x")
				e.Retryable = true
				return provider.TextResult{}, e
			}
		}},
		{name: "provider auth", want: amqp.ClassDead, shape: func(_ *sumSource, _ *sumPrompts, rt *sumRouter, _ *sumStore, _ *events.SessionCompletedV1) {
			rt.reply = func() (provider.TextResult, error) {
				return provider.TextResult{}, provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindAuth, "x")
			}
		}},
		{name: "empty output", want: amqp.ClassRetry, shape: func(_ *sumSource, _ *sumPrompts, rt *sumRouter, _ *sumStore, _ *events.SessionCompletedV1) {
			rt.reply = func() (provider.TextResult, error) { return provider.TextResult{Text: " "}, nil }
		}, check: func(t *testing.T, err error) { require.ErrorIs(t, err, summary.ErrEmptySummary) }},
		{name: "database error", want: amqp.ClassRetry, shape: func(_ *sumSource, _ *sumPrompts, _ *sumRouter, st *sumStore, _ *events.SessionCompletedV1) {
			st.FailNext("SetSummary", errors.New("connection reset"))
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			uid, sid := uuid.New(), uuid.New()
			start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
			ended := start.Add(5 * time.Minute)
			src := &sumSource{
				info:  summary.SessionInfo{ID: sid, UserID: uid, UserStatus: "ACTIVE", TopicName: "旅行", ScenarioName: "ホテル", StartedAt: &start, EndedAt: &ended},
				turns: []summary.TurnRow{{Seq: 1, Status: &completed, UserText: ptrStr("京都へ行きます。"), AssistantText: ptrStr("いいですね。")}},
			}
			pr := &sumPrompts{pr: summary.Prompt{ID: uuid.New(), Template: prompt.Template{Key: "SUMMARY", Version: 1, System: "S", User: "{{.Transcript}}"}}}
			rt := &sumRouter{reply: ok}
			st := &sumStore{processed: fakes.NewProcessed()}
			p := events.SessionCompletedV1{SessionID: sid, UserTurnCount: 1, StartedAt: start, EndedAt: ended}
			if sc.shape != nil {
				sc.shape(src, pr, rt, st, &p)
			}
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			env := events.Envelope{EventID: uuid.New(), EventType: events.SessionCompleted, EventVersion: 1, UserID: uid, AggregateID: sid, Payload: raw}
			if sc.processed {
				require.NoError(t, st.processed.MarkProcessed(t.Context(), events.QueueSummary, env))
			}
			svc := summary.NewService(summary.Deps{Store: st, Source: src, Prompts: pr, Router: rt, Cfg: summary.DefaultConfig()})
			err = summary.NewConsumer(svc).Handle(t.Context(), events.Delivery{Envelope: env, Queue: events.QueueSummary})
			require.Equal(t, sc.want, amqp.Classify(err, false), "err=%v", err)
			if sc.check != nil {
				sc.check(t, err)
			}
		})
	}
}

func ptrStr(s string) *string { return &s }
