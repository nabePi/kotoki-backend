package summary_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/summary"
)

func (r *rig) delivery() events.Delivery {
	return events.Delivery{Envelope: r.env, Queue: events.QueueSummary}
}

func TestConsumerContract(t *testing.T) {
	c := summary.NewConsumer(newRig(t).service())
	require.Equal(t, "summary.session_completed", c.Queue())
	require.Equal(t, map[string]int{"session.completed": 1}, c.SupportedEvents())
	require.Equal(t, 60*time.Second, c.Timeout())
}

func TestConsumerHandle(t *testing.T) {
	t.Run("valid payload reaches Process", func(t *testing.T) {
		r := newRig(t)
		require.NoError(t, summary.NewConsumer(r.service()).Handle(t.Context(), r.delivery()))
		require.NotNil(t, r.store.text(r.sid))
		require.True(t, r.processed())
	})
	t.Run("broken payload is permanent DECODE_ERROR", func(t *testing.T) {
		r := newRig(t)
		d := r.delivery()
		d.Envelope.Payload = json.RawMessage(`{"session_id": 5`)
		err := summary.NewConsumer(r.service()).Handle(t.Context(), d)
		requirePermanent(t, err, "DECODE_ERROR")
		require.Zero(t, r.src.loads)
		require.Zero(t, r.rt.calls())
	})
	t.Run("Process errors pass through unchanged", func(t *testing.T) {
		r := newRig(t)
		boom := errors.New("llm down")
		r.rt.reply = func(int) (provider.TextResult, error) { return provider.TextResult{}, boom }
		require.Equal(t, boom, summary.NewConsumer(r.service()).Handle(t.Context(), r.delivery()))
	})
}

func TestOnFailureCountsAndLogs(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := newRig(t)
	svc := summary.NewService(summary.Deps{
		Store: r.store, Source: r.src, Prompts: r.pr, Router: r.rt, Cfg: r.cfg, Metrics: summary.NewMetrics(reg), Log: slog.New(slog.DiscardHandler),
	})
	c := summary.NewConsumer(svc)
	perm := events.Permanent("CONFIGURATION_ERROR", errors.New("no prompt"))
	prov := provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindUnavailable, "down")
	for _, cause := range []error{perm, perm, prov, summary.ErrEmptySummary, errors.New("db")} {
		c.OnFailure(context.Background(), r.delivery(), cause, false)
	}
	c.OnFailure(context.Background(), r.delivery(), events.Permanent("DECODE_ERROR", errors.New("x")), true)
	count := func(reason string) float64 { return failureCount(t, reg, reason) }
	require.Equal(t, 2.0, count("configuration_error"))
	require.Equal(t, 1.0, count("provider_error"))
	require.Equal(t, 1.0, count("empty_output"))
	require.Equal(t, 1.0, count("internal_error"))
	require.Equal(t, 1.0, count("decode_error"))
	// OnFailure writes no state: no summary, no processed row
	require.Nil(t, r.store.text(r.sid))
	require.False(t, r.processed())
}

func TestFailureReason(t *testing.T) {
	for err, want := range map[error]string{
		events.Permanent("ENTITY_NOT_FOUND", errors.New("x")):          summary.ReasonNotFound,
		events.Permanent("INVARIANT_VIOLATION", errors.New("x")):       summary.ReasonInvariant,
		events.Permanent("TRANSCRIPT_UNAVAILABLE", errors.New("x")):    summary.ReasonTranscript,
		events.Permanent("UNSUPPORTED_EVENT_VERSION", errors.New("x")): summary.ReasonPermanent,
	} {
		require.Equal(t, want, summary.FailureReason(err))
	}
	var nilMetrics *summary.Metrics
	nilMetrics.Failure("x") // safe on nil
}

func failureCount(t *testing.T, reg *prometheus.Registry, reason string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "kotoki_summary_failures_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
