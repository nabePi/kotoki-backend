package evaluation_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func (h *harness) consumer(cfg evaluation.Config) *evaluation.Consumer {
	return evaluation.NewConsumer(h.service(), cfg)
}

func (h *harness) delivery() events.Delivery {
	return events.Delivery{Envelope: h.env, Queue: events.QueueEvaluation}
}

func TestConsumerContract(t *testing.T) {
	h := newHarness(t)
	c := h.consumer(evaluation.DefaultConfig())
	require.Equal(t, "evaluation.session_completed", c.Queue())
	require.Equal(t, map[string]int{"session.completed": 1}, c.SupportedEvents())
	require.Equal(t, 200*time.Second, c.Timeout())

	cfg := evaluation.DefaultConfig()
	cfg.HandlerTimeout = 7 * time.Second
	require.Equal(t, 7*time.Second, h.consumer(cfg).Timeout())
}

func TestHandleDecodesPayload(t *testing.T) {
	t.Run("valid payload reaches Process", func(t *testing.T) {
		h := newHarness(t)
		var loaded uuid.UUID
		h.src.onLoad = func(id uuid.UUID) { loaded = id }
		require.NoError(t, h.consumer(evaluation.DefaultConfig()).Handle(t.Context(), h.delivery()))
		require.Equal(t, h.p.SessionID, loaded)
		require.Equal(t, evaluation.StatusCompleted, h.row(t).Status)
		require.Equal(t, 1, h.store.Outbox.Len())
	})

	t.Run("broken payload is permanent DECODE_ERROR", func(t *testing.T) {
		h := newHarness(t)
		called := false
		h.src.onLoad = func(uuid.UUID) { called = true }
		d := h.delivery()
		d.Envelope.Payload = json.RawMessage(`{"session_id": 5`)
		err := h.consumer(evaluation.DefaultConfig()).Handle(t.Context(), d)
		var perm *events.PermanentError
		require.ErrorAs(t, err, &perm)
		require.Equal(t, "DECODE_ERROR", perm.Code)
		require.False(t, called)
	})

	t.Run("process errors pass through unchanged", func(t *testing.T) {
		h := newHarness(t)
		h.ev.steps[0] = failWith(errors.New("llm down"))
		err := h.consumer(evaluation.DefaultConfig()).Handle(t.Context(), h.delivery())
		require.EqualError(t, err, "llm down")
	})
}

func TestOnFailureDelegates(t *testing.T) {
	boom := provider.NewError(provider.OpenAI, provider.CapEvaluation, provider.KindUnavailable, "down")

	t.Run("retryable", func(t *testing.T) {
		h := newHarness(t)
		h.ev.steps[0] = failWith(boom)
		c := h.consumer(evaluation.DefaultConfig())
		require.Error(t, c.Handle(t.Context(), h.delivery()))
		c.OnFailure(context.Background(), h.delivery(), boom, false)
		require.Equal(t, evaluation.StatusPending, h.row(t).Status)
		require.Zero(t, h.store.Outbox.Len())
	})

	t.Run("final keeps the envelope and emits evaluation.failed", func(t *testing.T) {
		h := newHarness(t)
		h.ev.steps[0] = failWith(boom)
		c := h.consumer(evaluation.DefaultConfig())
		require.Error(t, c.Handle(t.Context(), h.delivery()))
		c.OnFailure(context.Background(), h.delivery(), boom, true)
		require.Equal(t, evaluation.StatusFailed, h.row(t).Status)
		evs := h.store.Outbox.Events()
		require.Len(t, evs, 1)
		require.Equal(t, events.EvaluationFailed, evs[0].EventType)
		require.Equal(t, h.env.UserID, evs[0].UserID)
		require.Equal(t, h.p.SessionID, evs[0].Payload.(events.EvaluationFailedV1).SessionID)
	})
}
