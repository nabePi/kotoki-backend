package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

type purgeCall struct {
	step  string
	user  uuid.UUID
	batch int
	at    time.Time
}

type fakePurge struct {
	candidates []uuid.UUID
	listErr    error
	listBefore time.Time
	listLimit  int
	calls      []purgeCall
	failAt     map[string]map[uuid.UUID]error // step -> user -> error
}

func newFakePurge(users ...uuid.UUID) *fakePurge {
	return &fakePurge{candidates: users, failAt: map[string]map[uuid.UUID]error{}}
}

func (f *fakePurge) fail(step string, user uuid.UUID, err error) {
	if f.failAt[step] == nil {
		f.failAt[step] = map[uuid.UUID]error{}
	}
	f.failAt[step][user] = err
}

func (f *fakePurge) step(step string, u uuid.UUID, batch int, at time.Time) error {
	f.calls = append(f.calls, purgeCall{step, u, batch, at})
	return f.failAt[step][u]
}

func (f *fakePurge) ListCandidates(_ context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	f.listBefore, f.listLimit = before, limit
	return f.candidates, f.listErr
}
func (f *fakePurge) DeleteMemories(_ context.Context, u uuid.UUID) error {
	return f.step("memories", u, 0, time.Time{})
}
func (f *fakePurge) DeleteEvaluations(_ context.Context, u uuid.UUID) error {
	return f.step("evaluations", u, 0, time.Time{})
}
func (f *fakePurge) DeleteLearner(_ context.Context, u uuid.UUID) error {
	return f.step("learner", u, 0, time.Time{})
}
func (f *fakePurge) DeleteTranscripts(_ context.Context, u uuid.UUID, batch int) error {
	return f.step("transcripts", u, batch, time.Time{})
}
func (f *fakePurge) ClearSummaries(_ context.Context, u uuid.UUID) error {
	return f.step("summaries", u, 0, time.Time{})
}
func (f *fakePurge) Anonymize(_ context.Context, u uuid.UUID, now time.Time) error {
	return f.step("anonymize", u, 0, now)
}

func (f *fakePurge) stepsOf(u uuid.UUID) []string {
	var out []string
	for _, c := range f.calls {
		if c.user == u {
			out = append(out, c.step)
		}
	}
	return out
}

func newPurgeJob(s jobs.PurgeStore, now time.Time) (*jobs.AccountPurgeJob, *jobs.Metrics, *bytes.Buffer) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	logs := &bytes.Buffer{}
	return jobs.NewAccountPurgeJob(s, time.Hour, m, clock.NewFake(now), slog.New(slog.NewJSONHandler(logs, nil))), m, logs
}

func TestPurge_StepOrderPerUser(t *testing.T) {
	u := uuid.Must(uuid.NewV7())
	f := newFakePurge(u)
	job, _, _ := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, []string{"memories", "evaluations", "learner", "transcripts", "summaries", "anonymize"}, f.stepsOf(u))
	for _, c := range f.calls {
		if c.step == "transcripts" {
			assert.Equal(t, 1000, c.batch)
		}
	}
}

func TestPurge_Max50UsersPerRun(t *testing.T) {
	f := newFakePurge()
	job, _, _ := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, 50, f.listLimit, "the store is asked for at most 50 users, ordered by deleted_at")
}

func TestPurge_CandidateCutoff(t *testing.T) {
	T := utc(2027, 6, 15, 12, 0)
	f := newFakePurge()
	job, _, _ := newPurgeJob(f, T)
	require.NoError(t, job.Run(context.Background()))
	assert.True(t, f.listBefore.Equal(T.Add(-time.Hour)), "deleted_at < now - ACCOUNT_PURGE_DELAY, got %s", f.listBefore)

	f = newFakePurge()
	m := jobs.NewMetrics(prometheus.NewRegistry())
	job = jobs.NewAccountPurgeJob(f, 90*time.Minute, m, clock.NewFake(T), nil)
	require.NoError(t, job.Run(context.Background()))
	assert.True(t, f.listBefore.Equal(T.Add(-90*time.Minute)), "the delay comes from config")
}

func TestPurge_AnonymizeUsesClockNow(t *testing.T) {
	T := utc(2027, 6, 15, 12, 0)
	u := uuid.Must(uuid.NewV7())
	f := newFakePurge(u)
	job, _, _ := newPurgeJob(f, T)
	require.NoError(t, job.Run(context.Background()))
	last := f.calls[len(f.calls)-1]
	require.Equal(t, "anonymize", last.step)
	assert.True(t, last.at.Equal(T))
}

func TestPurge_FailureMidwayNotAnonymized_NextUserContinues(t *testing.T) {
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	f := newFakePurge(a, b)
	f.fail("evaluations", a, errors.New("deadlock detected"))
	job, m, logs := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	err := job.Run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), a.String())
	assert.Contains(t, err.Error(), "deadlock detected")
	assert.Equal(t, []string{"memories", "evaluations"}, f.stepsOf(a), "nothing after the failed step, in particular no anonymize")
	assert.Equal(t, []string{"memories", "evaluations", "learner", "transcripts", "summaries", "anonymize"}, f.stepsOf(b))
	assert.Contains(t, logs.String(), `"level":"ERROR"`)
	assert.Contains(t, logs.String(), a.String())
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_failed")))
}

func TestPurge_AnonymizeFailureIsAFailure(t *testing.T) {
	u := uuid.Must(uuid.NewV7())
	f := newFakePurge(u)
	f.fail("anonymize", u, errors.New("boom"))
	job, m, logs := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	require.Error(t, job.Run(context.Background()))
	assert.Equal(t, 0.0, promtest.ToFloat64(m.JobItems.WithLabelValues("account_purge", "users_purged")))
	assert.NotContains(t, logs.String(), "account_purged", "no audit line for a user that is not purged")
}

func TestPurge_AuditLogOnlyUserID(t *testing.T) {
	u := uuid.Must(uuid.NewV7())
	f := newFakePurge(u)
	job, _, logs := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	require.NoError(t, job.Run(context.Background()))
	out := logs.String()
	assert.Contains(t, out, `"audit":true`)
	assert.Contains(t, out, `"audit_event":"account_purged"`)
	assert.Contains(t, out, `"user_id":"`+u.String()+`"`)
}

func TestPurge_ListError(t *testing.T) {
	f := newFakePurge()
	f.listErr = errors.New("connection refused")
	job, _, _ := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	err := job.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
	assert.Empty(t, f.calls)
}

func TestPurge_CancelledContextStops(t *testing.T) {
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	f := newFakePurge(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job, _, _ := newPurgeJob(f, utc(2027, 6, 15, 12, 0))
	require.ErrorIs(t, job.Run(ctx), context.Canceled)
	assert.Empty(t, f.calls)
}
