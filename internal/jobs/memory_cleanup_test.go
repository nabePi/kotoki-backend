package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
)

type fakeExpiredMemories struct {
	batches [][]uuid.UUID // successive batch results
	errAt   int           // 1-based call that fails (0 = never)
	limits  []int
	calls   int
}

func (f *fakeExpiredMemories) DeleteExpiredMemories(_ context.Context, limit int) ([]uuid.UUID, error) {
	f.calls++
	f.limits = append(f.limits, limit)
	if f.errAt == f.calls {
		return nil, errors.New("serialization failure")
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

type fakeBumper struct {
	calls  [][]uuid.UUID
	err    error
	ctxErr error // ctx.Err() as seen by the last Bump
}

func (b *fakeBumper) Bump(ctx context.Context, ids []uuid.UUID) error {
	b.ctxErr = ctx.Err()
	b.calls = append(b.calls, append([]uuid.UUID(nil), ids...))
	return b.err
}

// rows returns n user ids drawn round-robin from users.
func rows(users []uuid.UUID, n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = users[i%len(users)]
	}
	return out
}

func newUsers(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.Must(uuid.NewV7())
	}
	return out
}

func newMemoryJob(s jobs.ExpiredMemoryStore, b jobs.MemVerBumper) (*jobs.MemoryCleanupJob, *jobs.Metrics, *bytes.Buffer) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	logs := &bytes.Buffer{}
	return jobs.NewMemoryCleanupJob(s, b, m, slog.New(slog.NewJSONHandler(logs, nil))), m, logs
}

func TestMemoryCleanup_LoopsUntilUnder1000_BumpsUniqueUsersOnce(t *testing.T) {
	users := newUsers(3)
	st := &fakeExpiredMemories{batches: [][]uuid.UUID{rows(users, 1000), rows(users[:2], 40)}}
	b := &fakeBumper{}
	job, m, _ := newMemoryJob(st, b)
	require.NoError(t, job.Run(context.Background()))

	assert.Equal(t, 2, st.calls, "full batch then a short one")
	assert.Equal(t, []int{1000, 1000}, st.limits)
	require.Len(t, b.calls, 1, "one Bump per run")
	assert.ElementsMatch(t, users, b.calls[0], "3 unique users, no duplicates")
	assert.Len(t, b.calls[0], 3)
	assert.Equal(t, 1040.0, promtest.ToFloat64(m.JobItems.WithLabelValues("memory_cleanup", "memories_deleted")))
	assert.Equal(t, 1040.0, promtest.ToFloat64(m.MemoryCleanupDeleted), "kotoki_memory_cleanup_deleted_total (spec 13.7)")
}

func TestMemoryCleanup_ExactMultipleOfBatchNeedsOneMoreCall(t *testing.T) {
	users := newUsers(1)
	st := &fakeExpiredMemories{batches: [][]uuid.UUID{rows(users, 1000)}}
	job, _, _ := newMemoryJob(st, &fakeBumper{})
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, 2, st.calls)
}

func TestMemoryCleanup_NoRows_NoBump(t *testing.T) {
	st := &fakeExpiredMemories{}
	b := &fakeBumper{}
	job, _, _ := newMemoryJob(st, b)
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, 1, st.calls)
	assert.Empty(t, b.calls)
}

func TestMemoryCleanup_BumpErrorReturned(t *testing.T) {
	st := &fakeExpiredMemories{batches: [][]uuid.UUID{newUsers(2)}}
	b := &fakeBumper{err: errors.New("redis down")}
	job, _, logs := newMemoryJob(st, b)
	err := job.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redis down")
	assert.Contains(t, logs.String(), `"level":"ERROR"`)
}

func TestMemoryCleanup_StoreErrorStillBumpsAlreadyDeletedUsers(t *testing.T) {
	users := newUsers(2)
	st := &fakeExpiredMemories{batches: [][]uuid.UUID{rows(users, 1000)}, errAt: 2}
	b := &fakeBumper{}
	job, _, _ := newMemoryJob(st, b)
	err := job.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "serialization failure")
	require.Len(t, b.calls, 1, "rows of the first batch are gone: their memver must still move")
	assert.ElementsMatch(t, users, b.calls[0])
}

func TestMemoryCleanup_BothErrorsReturned(t *testing.T) {
	st := &fakeExpiredMemories{batches: [][]uuid.UUID{rows(newUsers(1), 1000)}, errAt: 2}
	job, _, _ := newMemoryJob(st, &fakeBumper{err: errors.New("redis down")})
	err := job.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "serialization failure")
	assert.Contains(t, err.Error(), "redis down")
}

func TestMemoryCleanup_CancelledContextStops(t *testing.T) {
	st := &fakeExpiredMemories{batches: [][]uuid.UUID{rows(newUsers(1), 1000)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job, _, _ := newMemoryJob(st, &fakeBumper{})
	require.ErrorIs(t, job.Run(ctx), context.Canceled)
	assert.Zero(t, st.calls)
}

// Shutdown in the middle of a run: the committed deletes still get their memver bump.
func TestMemoryCleanup_CancelMidRunStillBumpsWithLiveContext(t *testing.T) {
	users := newUsers(2)
	ctx, cancel := context.WithCancel(context.Background())
	st := &cancelAfterFirst{fakeExpiredMemories: fakeExpiredMemories{batches: [][]uuid.UUID{rows(users, 1000)}}, cancel: cancel}
	b := &fakeBumper{}
	job, _, _ := newMemoryJob(st, b)
	err := job.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, b.calls, 1)
	assert.ElementsMatch(t, users, b.calls[0])
	assert.NoError(t, b.ctxErr, "the bump must not run on the cancelled job context")
}

type cancelAfterFirst struct {
	fakeExpiredMemories
	cancel context.CancelFunc
}

func (c *cancelAfterFirst) DeleteExpiredMemories(ctx context.Context, limit int) ([]uuid.UUID, error) {
	ids, err := c.fakeExpiredMemories.DeleteExpiredMemories(ctx, limit)
	c.cancel()
	return ids, err
}
