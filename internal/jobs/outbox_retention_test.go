package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

type outboxCall struct {
	step   string
	before time.Time
	batch  int
}

type fakeOutboxRetention struct {
	calls   []outboxCall
	results map[string][]int
	errs    map[string]error
}

func newFakeOutboxRetention() *fakeOutboxRetention {
	return &fakeOutboxRetention{results: map[string][]int{}, errs: map[string]error{}}
}

func (f *fakeOutboxRetention) next(step string, before time.Time, batch int) (int, error) {
	f.calls = append(f.calls, outboxCall{step, before, batch})
	if err := f.errs[step]; err != nil {
		return 0, err
	}
	r := f.results[step]
	if len(r) == 0 {
		return 0, nil
	}
	f.results[step] = r[1:]
	return r[0], nil
}

func (f *fakeOutboxRetention) DeleteOutboxPublished(_ context.Context, before time.Time, batch int) (int, error) {
	return f.next("outbox_published", before, batch)
}

func (f *fakeOutboxRetention) DeleteOutboxFailed(_ context.Context, before time.Time, batch int) (int, error) {
	return f.next("outbox_failed", before, batch)
}

func (f *fakeOutboxRetention) DeleteProcessedEvents(_ context.Context, before time.Time, batch int) (int, error) {
	return f.next("processed_events", before, batch)
}

func (f *fakeOutboxRetention) callsOf(step string) []outboxCall {
	var out []outboxCall
	for _, c := range f.calls {
		if c.step == step {
			out = append(out, c)
		}
	}
	return out
}

func newOutboxJob(s jobs.OutboxRetentionStore, now time.Time) (*jobs.OutboxRetentionJob, *jobs.Metrics) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	cfg := jobs.OutboxRetentionConfig{PublishedDays: 7, ProcessedEventsDays: 30}
	return jobs.NewOutboxRetentionJob(s, cfg, m, clock.NewFake(now), nil), m
}

func TestOutboxRetention_Cutoffs(t *testing.T) {
	T := utc(2027, 6, 15, 12, 0)
	f := newFakeOutboxRetention()
	job, _ := newOutboxJob(f, T)
	require.NoError(t, job.Run(context.Background()))

	d := 24 * time.Hour
	one := func(step string) outboxCall {
		cs := f.callsOf(step)
		require.Len(t, cs, 1, step)
		return cs[0]
	}
	assert.True(t, one("outbox_published").before.Equal(T.Add(-7*d)), "published: T-7d")
	assert.True(t, one("outbox_failed").before.Equal(T.Add(-30*d)), "failed: T-30d, fixed")
	assert.True(t, one("processed_events").before.Equal(T.Add(-30*d)), "processed_events: T-30d")
	var order []string
	for _, c := range f.calls {
		order = append(order, c.step)
	}
	assert.Equal(t, []string{"outbox_published", "outbox_failed", "processed_events"}, order)
}

func TestOutboxRetention_UsesConfiguredDays(t *testing.T) {
	T := utc(2027, 6, 15, 12, 0)
	f := newFakeOutboxRetention()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := jobs.NewOutboxRetentionJob(f, jobs.OutboxRetentionConfig{PublishedDays: 2, ProcessedEventsDays: 10}, m, clock.NewFake(T), nil)
	require.NoError(t, job.Run(context.Background()))
	d := 24 * time.Hour
	assert.True(t, f.callsOf("outbox_published")[0].before.Equal(T.Add(-2*d)))
	assert.True(t, f.callsOf("outbox_failed")[0].before.Equal(T.Add(-30*d)), "FAILED stays 30 days whatever the config")
	assert.True(t, f.callsOf("processed_events")[0].before.Equal(T.Add(-10*d)))
}

func TestOutboxRetention_LoopsUntilBatchUnder5000(t *testing.T) {
	f := newFakeOutboxRetention()
	for _, s := range []string{"outbox_published", "outbox_failed", "processed_events"} {
		f.results[s] = []int{5000, 5000, 120}
	}
	job, m := newOutboxJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))
	for _, s := range []string{"outbox_published", "outbox_failed", "processed_events"} {
		cs := f.callsOf(s)
		assert.Len(t, cs, 3, s)
		for _, c := range cs {
			assert.Equal(t, 5000, c.batch, s)
		}
		assert.Equal(t, 10120.0, promtest.ToFloat64(m.JobItems.WithLabelValues("outbox_retention", s)), s)
	}
}

func TestOutboxRetention_ShortBatchStops(t *testing.T) {
	f := newFakeOutboxRetention()
	f.results["outbox_published"] = []int{4999}
	job, _ := newOutboxJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))
	assert.Len(t, f.callsOf("outbox_published"), 1)
}

func TestOutboxRetention_ErrorContinuesToNextStep(t *testing.T) {
	f := newFakeOutboxRetention()
	f.errs["outbox_published"] = errors.New("deadlock detected")
	job, m := newOutboxJob(f, utc(2027, 6, 15, 12, 0))
	err := job.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbox_published")
	assert.Contains(t, err.Error(), "deadlock detected")
	assert.Len(t, f.callsOf("outbox_published"), 1, "no tight retry loop")
	assert.Len(t, f.callsOf("outbox_failed"), 1)
	assert.Len(t, f.callsOf("processed_events"), 1)
	assert.Equal(t, 0.0, promtest.ToFloat64(m.JobItems.WithLabelValues("outbox_retention", "outbox_published")))
}

func TestOutboxRetention_CancelledContextStops(t *testing.T) {
	f := newFakeOutboxRetention()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job, _ := newOutboxJob(f, utc(2027, 6, 15, 12, 0))
	require.ErrorIs(t, job.Run(ctx), context.Canceled)
	assert.Empty(t, f.calls)
}
