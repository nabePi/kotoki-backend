package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

type retCall struct {
	step  string
	at    time.Time // before / cutoff / now
	batch int
	ret   time.Duration
}

type fakeRetention struct {
	calls []retCall
	// results per step: successive batch results; after they run out the step returns 0.
	results map[string][]int
	errs    map[string]error
}

func (f *fakeRetention) next(step string, at time.Time, batch int, ret time.Duration) (int, error) {
	f.calls = append(f.calls, retCall{step, at, batch, ret})
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

func (f *fakeRetention) PurgeEvaluationDetails(_ context.Context, before time.Time, batch int) (int, error) {
	return f.next("evaluation_details", before, batch, 0)
}

func (f *fakeRetention) DeleteTurnsBefore(_ context.Context, cutoff time.Time, batch int) (int, error) {
	return f.next("turns", cutoff, batch, 0)
}

func (f *fakeRetention) DeleteEventsBefore(_ context.Context, cutoff time.Time, batch int) (int, error) {
	return f.next("events", cutoff, batch, 0)
}

func (f *fakeRetention) PurgeSessionSummaries(_ context.Context, before time.Time, batch int) (int, error) {
	return f.next("summaries", before, batch, 0)
}

func (f *fakeRetention) DeleteExpiredTokens(_ context.Context, now time.Time, ret time.Duration, batch int) (int, error) {
	return f.next("tokens", now, batch, ret)
}

func newFakeRetention() *fakeRetention {
	return &fakeRetention{results: map[string][]int{}, errs: map[string]error{}}
}

func (f *fakeRetention) callsOf(step string) []retCall {
	var out []retCall
	for _, c := range f.calls {
		if c.step == step {
			out = append(out, c)
		}
	}
	return out
}

var retCfg = jobs.RetentionConfig{TranscriptDays: 180, SessionEventsDays: 365, ExpiredTokensDays: 7}

func newRetentionJob(s jobs.RetentionStore, now time.Time) (*jobs.TranscriptRetentionJob, *jobs.Metrics, *bytes.Buffer) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	logs := &bytes.Buffer{}
	return jobs.NewTranscriptRetentionJob(s, retCfg, m, clock.NewFake(now), slog.New(slog.NewJSONHandler(logs, nil))), m, logs
}

func TestRetentionJob_LoopsEachStepUntilShortBatch(t *testing.T) {
	f := newFakeRetention()
	f.results["evaluation_details"] = []int{500, 500, 17}
	f.results["turns"] = []int{5000, 5000, 4999}
	f.results["events"] = []int{5000, 0}
	f.results["summaries"] = []int{1000, 3}
	f.results["tokens"] = []int{1000, 1000, 1}
	job, _, _ := newRetentionJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))

	assert.Len(t, f.callsOf("evaluation_details"), 3)
	assert.Len(t, f.callsOf("turns"), 3)
	assert.Len(t, f.callsOf("events"), 2, "a full batch is followed by one more call")
	assert.Len(t, f.callsOf("summaries"), 2)
	assert.Len(t, f.callsOf("tokens"), 3)
	for step, batch := range map[string]int{"evaluation_details": 500, "turns": 5000, "events": 5000, "summaries": 1000, "tokens": 1000} {
		for _, c := range f.callsOf(step) {
			assert.Equal(t, batch, c.batch, step)
		}
	}
	// Order of spec 9.9.4.
	var order []string
	for _, c := range f.calls {
		if len(order) == 0 || order[len(order)-1] != c.step {
			order = append(order, c.step)
		}
	}
	assert.Equal(t, []string{"evaluation_details", "turns", "events", "summaries", "tokens"}, order)
}

func TestRetentionJob_Cutoffs(t *testing.T) {
	T := utc(2027, 6, 15, 12, 0)
	f := newFakeRetention()
	job, _, _ := newRetentionJob(f, T)
	require.NoError(t, job.Run(context.Background()))

	one := func(step string) retCall {
		cs := f.callsOf(step)
		require.Len(t, cs, 1, step)
		return cs[0]
	}
	d := 24 * time.Hour
	assert.True(t, one("evaluation_details").at.Equal(T.Add(-180*d)), "evaluations: T-180d")
	assert.True(t, one("turns").at.Equal(T.Add(-180*d-2*time.Hour)), "turns: T-180d-2h")
	assert.True(t, one("events").at.Equal(T.Add(-365*d)), "events: T-365d")
	assert.True(t, one("summaries").at.Equal(T.Add(-180*d)), "summaries: T-180d")
	tok := one("tokens")
	assert.True(t, tok.at.Equal(T))
	assert.Equal(t, 7*d, tok.ret)
}

func TestRetentionJob_UsesConfiguredDays(t *testing.T) {
	T := utc(2027, 6, 15, 12, 0)
	f := newFakeRetention()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := jobs.NewTranscriptRetentionJob(f, jobs.RetentionConfig{TranscriptDays: 30, SessionEventsDays: 90, ExpiredTokensDays: 3}, m, clock.NewFake(T), nil)
	require.NoError(t, job.Run(context.Background()))
	d := 24 * time.Hour
	assert.True(t, f.callsOf("turns")[0].at.Equal(T.Add(-30*d-2*time.Hour)))
	assert.True(t, f.callsOf("events")[0].at.Equal(T.Add(-90*d)))
	assert.Equal(t, 3*d, f.callsOf("tokens")[0].ret)
}

func TestRetentionJob_StepErrorContinuesAndReturnsError(t *testing.T) {
	f := newFakeRetention()
	f.errs["turns"] = errors.New("deadlock detected")
	f.results["evaluation_details"] = []int{500, 1}
	job, _, logs := newRetentionJob(f, utc(2027, 6, 15, 12, 0))
	err := job.Run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "turns")
	assert.Contains(t, err.Error(), "deadlock detected")
	assert.Len(t, f.callsOf("turns"), 1, "a failing step is not retried in a tight loop")
	assert.Len(t, f.callsOf("events"), 1, "later steps still ran")
	assert.Len(t, f.callsOf("summaries"), 1)
	assert.Len(t, f.callsOf("tokens"), 1)
	assert.Contains(t, logs.String(), `"level":"ERROR"`)
	assert.Contains(t, logs.String(), `"step":"turns"`)
}

func TestRetentionJob_ItemsMetric(t *testing.T) {
	f := newFakeRetention()
	f.results["evaluation_details"] = []int{500, 7}
	f.results["turns"] = []int{5000, 12}
	f.results["events"] = []int{3}
	f.results["summaries"] = []int{2}
	f.results["tokens"] = []int{9}
	job, m, _ := newRetentionJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))
	item := func(a string) float64 {
		return promtest.ToFloat64(m.JobItems.WithLabelValues("transcript_retention", a))
	}
	assert.Equal(t, 507.0, item("evaluation_details"))
	assert.Equal(t, 5012.0, item("turns"))
	assert.Equal(t, 3.0, item("events"))
	assert.Equal(t, 2.0, item("summaries"))
	assert.Equal(t, 9.0, item("tokens"))
}

func TestRetentionJob_ItemsCountedEvenWhenTheBatchErrors(t *testing.T) {
	f := newFakeRetention()
	f.errs["events"] = errors.New("boom")
	job, m, _ := newRetentionJob(f, utc(2027, 6, 15, 12, 0))
	require.Error(t, job.Run(context.Background()))
	assert.Equal(t, 0.0, promtest.ToFloat64(m.JobItems.WithLabelValues("transcript_retention", "events")))
}

func TestRetentionJob_CancelledContextStops(t *testing.T) {
	f := newFakeRetention()
	f.results["evaluation_details"] = []int{500, 500, 500, 500}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job, _, _ := newRetentionJob(f, utc(2027, 6, 15, 12, 0))
	err := job.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, f.calls, "nothing runs once the context is done")
}
