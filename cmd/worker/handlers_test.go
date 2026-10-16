package main

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
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

// Until M7/M8 fill the seam, a feature handler must stop the process at startup: silently running
// without the consumer would leave its queue growing.
func TestBuildConsumers_UnimplementedHandlerFailsStartup(t *testing.T) {
	for _, h := range []string{"evaluation", "memory", "learner", "summary"} {
		_, err := buildConsumers([]string{"outbox", h}, consumerDeps{})
		require.ErrorIs(t, err, errHandlerNotImplemented, h)
		assert.Contains(t, err.Error(), h)
	}
	cs, err := buildConsumers([]string{"outbox", "sweeper", "cleanup"}, consumerDeps{})
	require.NoError(t, err)
	assert.Empty(t, cs)
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
