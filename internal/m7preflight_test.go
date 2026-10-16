package internal_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

// Compiling this file proves the M3/M4/M6 contracts M7 builds on still exist with the shapes it
// expects; a compile error here means a milestone drifted (fix there, record a deviation).
var (
	_ func(prompt.Template, any) (prompt.Rendered, error) = prompt.Render
	_ prompt.Template
	_ prompt.Rendered
	_ prompt.Params
	_ func(string, func() []any) = prompt.RegisterFixtures

	_ func(provider.AsyncRouter) provider.EvaluationProvider                                         = provider.AsyncRouter.Evaluation
	_ func(provider.AsyncRouter, context.Context, provider.TextRequest) (provider.TextResult, error) = provider.AsyncRouter.CompleteText
	_ provider.StructuredRequest
	_ provider.StructuredResult
	_ func(error) bool = provider.IsRetryable
	_                  = fake.NewEvaluation

	_ content.PromptVersion
	_ content.ScenarioVersion
	_ interface {
		GetScenarioVersion(context.Context, uuid.UUID) (content.ScenarioVersion, error)
		GetTopicByID(context.Context, uuid.UUID) (content.Topic, error)
		GetScenario(context.Context, uuid.UUID) (content.Scenario, error)
		GetPublishedPromptVersion(context.Context, content.PromptKey) (content.PromptVersion, error)
	} = content.Repository(nil)

	_ events.Envelope
	_ events.Delivery
	_ func(events.Envelope) (events.SessionCompletedV1, error) = events.DecodePayload[events.SessionCompletedV1]
	_ func(string, error) error                                = events.Permanent
	_ events.PermanentError
	_ error = events.ErrAlreadyProcessed
	_ error = events.ErrSkippedDeletedUser
	_ events.ProcessedEvents
	_ events.OutboxWriter
	_ events.Event
	_ string = events.ProducerWorker
	_ string = events.QueueEvaluation
	_ string = events.QueueSummary
	_ events.SessionCompletedV1
	_ events.EvaluationCompletedV1
	_ events.EvaluationFailedV1
	_ func(uuid.UUID, time.Time, string, uuid.UUID, events.EvaluationCompletedV1) events.Event = events.NewEvaluationCompleted
	_ func(uuid.UUID, time.Time, string, uuid.UUID, events.EvaluationFailedV1) events.Event    = events.NewEvaluationFailed

	_ amqp.Handler
)

func TestM7Preflight(t *testing.T) { t.Log("contracts compile") }
