package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/summary"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// Spec 9.8: which flag turns on which part of the process. The publisher and the jobs live only in
// the core flags; the four feature flags are consumers.
func TestHandlerSets_Section98(t *testing.T) {
	core := handlerSet([]string{"outbox", "sweeper", "cleanup"})
	assert.True(t, core.Publisher)
	assert.Empty(t, core.Consumers)
	assert.Len(t, core.Jobs, 8, "2 sweeper jobs + 6 cleanup jobs: the whole table of 9.9.1")
	assert.True(t, core.NeedsAMQP())

	sweeperOnly := handlerSet([]string{"sweeper"})
	assert.False(t, sweeperOnly.Publisher)
	assert.Equal(t, []string{"session_sweeper", "reservation_cleanup"}, sweeperOnly.Jobs)
	assert.False(t, sweeperOnly.NeedsAMQP())

	cleanup := handlerSet([]string{"cleanup"})
	assert.Len(t, cleanup.Jobs, 6)
	assert.NotContains(t, cleanup.Jobs, "session_sweeper")

	eval := handlerSet([]string{"evaluation"})
	assert.False(t, eval.Publisher)
	assert.Empty(t, eval.Jobs)
	assert.Equal(t, []string{"evaluation"}, eval.Consumers)
	assert.True(t, eval.NeedsAMQP())

	assert.Equal(t, []string{"memory"}, handlerSet([]string{"memory"}).Consumers)
	assert.Equal(t, []string{"learner", "summary"}, handlerSet([]string{"learner", "summary"}).Consumers)

	all := handlerSet([]string{"outbox", "evaluation", "memory", "learner", "summary", "sweeper", "cleanup"})
	assert.True(t, all.Publisher)
	assert.Len(t, all.Jobs, 8)
	assert.Equal(t, []string{"evaluation", "memory", "learner", "summary"}, all.Consumers)

	dup := handlerSet([]string{"memory", "memory", "outbox", "outbox"})
	assert.Equal(t, []string{"memory"}, dup.Consumers)
	assert.True(t, dup.Publisher)
}

// Until M8 fills the seam, a feature handler without a consumer must stop the process at startup:
// silently running without it would leave its queue growing. evaluation and summary exist (M7).
// M8 removes this test once memory and learner are wired.
func TestBuildConsumers_UnimplementedHandlerFailsStartup(t *testing.T) {
	for _, h := range []string{"memory", "learner"} {
		_, err := buildConsumers([]string{"outbox", h}, consumerDeps{})
		require.ErrorIs(t, err, errHandlerNotImplemented, h)
		assert.Contains(t, err.Error(), h)
	}
	cs, err := buildConsumers([]string{"outbox", "sweeper", "cleanup"}, consumerDeps{})
	require.NoError(t, err)
	assert.Empty(t, cs)
}

type stubRouter struct{ provider.AsyncRouter }

func m7Deps() consumerDeps {
	return consumerDeps{
		Reg: prometheus.NewRegistry(), Router: stubRouter{}, Log: slog.New(slog.DiscardHandler), Clock: clock.System{},
		EvalCfg: evaluation.Config{HandlerTimeout: 123 * time.Second}, SummaryCfg: summary.DefaultConfig(),
	}
}

func TestBuildConsumers_EvaluationAndSummary(t *testing.T) {
	d := m7Deps()
	cs, err := buildConsumers([]string{"outbox", "evaluation", "summary"}, d)
	require.NoError(t, err)
	assert.Len(t, cs, 2)

	want := map[string]time.Duration{events.QueueEvaluation: 123 * time.Second, events.QueueSummary: 60 * time.Second}
	for flag, queue := range map[string]string{"evaluation": events.QueueEvaluation, "summary": events.QueueSummary} {
		h, err := newHandler(flag, m7Deps())
		require.NoError(t, err, flag)
		assert.Equal(t, queue, h.Queue(), flag)
		assert.Equal(t, map[string]int{"session.completed": 1}, h.SupportedEvents(), flag)
		assert.Equal(t, want[queue], h.Timeout(), flag)
	}

	_, err = buildConsumers([]string{"evaluation"}, consumerDeps{Reg: prometheus.NewRegistry()})
	require.Error(t, err, "a consumer without its provider router is a startup error, not a nil dereference later")
	assert.NotErrorIs(t, err, errHandlerNotImplemented)
}

// platform/config does not import the feature packages, so the worker maps the groups field by
// field; distinct values per field catch a swapped or dropped one.
func TestFeatureConfigMapping(t *testing.T) {
	cfg := config.Worker{
		Evaluation: config.WorkerEvaluation{
			MinUserTurns: 4, MinUserSpeechSeconds: 21, MinActiveSeconds: 61, MinFindingConfidence: 0.7,
			TranscriptMaxChars: 1234, HandlerTimeout: 7 * time.Second,
		},
		Summary: config.WorkerSummary{MaxChars: 222},
	}
	assert.Equal(t, evaluation.Config{
		MinUserTurns: 4, MinUserSpeechSeconds: 21, MinActiveSeconds: 61, MinFindingConfidence: 0.7,
		TranscriptMaxChars: 1234, HandlerTimeout: 7 * time.Second,
	}, evaluationConfig(cfg))
	assert.Equal(t, summary.Config{MaxChars: 222}, summaryConfig(cfg))
}

func TestBuildJobs_CoversTheWholeTable(t *testing.T) {
	reg := prometheus.NewRegistry()
	js := buildJobs(jobDeps{
		Pool: nil, Redis: nil, Clock: clock.System{}, IDs: idgen.NewV7(), Log: nil,
		Metrics: jobs.NewMetrics(reg), SessionMetrics: metrics.NewSession(reg), Cfg: config.Worker{Jobs: config.Jobs{SweeperInterval: 10}},
	})
	require.Len(t, js, len(jobs.Specs))
	for _, s := range jobs.Specs {
		assert.NotNil(t, js[s.Name], s.Name)
	}
}

func TestRunRejectsEmptyOrUnknownHandlers(t *testing.T) {
	testutil.ApplyEnv(t, testutil.MinimalEnv(t))
	t.Setenv("WORKER_HANDLERS", "")
	require.Error(t, run(context.Background(), nil))
	err := run(context.Background(), []string{"--handlers=outbox,bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus")
}

func TestRunJob_UnknownJobExit2(t *testing.T) {
	err := run(context.Background(), []string{"run-job", "no_such_job"})
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
	assert.Contains(t, err.Error(), "no_such_job")

	err = run(context.Background(), []string{"run-job"})
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err), "missing name is a usage error")
}

func TestDLQReplay_UsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{"dlq-replay"},
		{"dlq-replay", "--queue", "nope"},
		{"dlq-replay", "--queue", "evaluation.session_completed", "--limit", "-1"},
		{"dlq-replay", "--bogus"},
	} {
		err := run(context.Background(), args)
		require.Error(t, err, args)
		assert.Equal(t, 2, exitCode(err), args)
	}
}

func TestExitCode(t *testing.T) {
	assert.Equal(t, 1, exitCode(errors.New("x")))
	assert.Equal(t, 2, exitCode(usagef("bad")))
	assert.Equal(t, 0, exitCode(nil))
}
