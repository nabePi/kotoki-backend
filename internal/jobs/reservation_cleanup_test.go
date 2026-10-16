package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/nabePi/kotoki-backend/internal/session"
)

type fakeAbandonSource struct {
	ids   []uuid.UUID
	err   error
	now   time.Time
	limit int
}

func (f *fakeAbandonSource) ListAbandonCandidates(_ context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	f.now, f.limit = now, limit
	return f.ids, f.err
}

type fakeAbandoner struct {
	calls  []uuid.UUID
	actors []session.Actor
	errs   map[uuid.UUID]error
}

func (f *fakeAbandoner) Abandon(_ context.Context, id uuid.UUID, a session.Actor) (session.Session, error) {
	f.calls = append(f.calls, id)
	f.actors = append(f.actors, a)
	return session.Session{ID: id}, f.errs[id]
}

func ids(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.Must(uuid.NewV7())
	}
	return out
}

func newCleanup(src *fakeAbandonSource, ab *fakeAbandoner, clk clock.Clock) (*jobs.ReservationCleanupJob, *jobs.Metrics) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	return jobs.NewReservationCleanupJob(src, ab, m, clk, slog.New(slog.NewTextHandler(io.Discard, nil))), m
}

func TestReservationCleanup_AbandonsEachCandidate_ActorCleanup_Limit200(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	src := &fakeAbandonSource{ids: ids(3)}
	ab := &fakeAbandoner{}
	job, m := newCleanup(src, ab, clk)
	require.NoError(t, job.Run(context.Background()))

	assert.Equal(t, 200, src.limit)
	assert.Equal(t, clk.Now(), src.now)
	assert.Equal(t, src.ids, ab.calls)
	for _, a := range ab.actors {
		assert.Equal(t, session.ActorCleanup, a)
	}
	assert.Equal(t, "CLEANUP", string(session.ActorCleanup))
	assert.Equal(t, 3.0, promtest.ToFloat64(m.JobItems.WithLabelValues("reservation_cleanup", "ok")))
}

func TestReservationCleanup_InvalidTransitionIsNoop_OtherErrorContinues(t *testing.T) {
	src := &fakeAbandonSource{ids: ids(4)}
	ab := &fakeAbandoner{errs: map[uuid.UUID]error{
		src.ids[0]: &session.TransitionError{From: session.StatusStarting, To: session.StatusAbandoned},
		src.ids[1]: fmt.Errorf("wrapped: %w", session.ErrInvalidTransition),
		src.ids[2]: errors.New("db down"),
	}}
	job, m := newCleanup(src, ab, clock.System{})
	require.NoError(t, job.Run(context.Background()), "per-session errors never fail the run")

	assert.Equal(t, src.ids, ab.calls, "the loop goes on after an error")
	assert.Equal(t, 2.0, promtest.ToFloat64(m.JobItems.WithLabelValues("reservation_cleanup", "noop")))
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("reservation_cleanup", "error")))
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("reservation_cleanup", "ok")))
}

func TestReservationCleanup_ListErrorReturned(t *testing.T) {
	boom := errors.New("boom")
	job, _ := newCleanup(&fakeAbandonSource{err: boom}, &fakeAbandoner{}, clock.System{})
	require.ErrorIs(t, job.Run(context.Background()), boom)
}

func TestReservationCleanup_CancelledContextStopsLoop(t *testing.T) {
	src := &fakeAbandonSource{ids: ids(3)}
	ab := &fakeAbandoner{}
	job, _ := newCleanup(src, ab, clock.System{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, job.Run(ctx), context.Canceled)
	assert.Empty(t, ab.calls)
}
