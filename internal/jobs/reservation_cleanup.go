package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/session"
)

const (
	reservationCleanupName  = "reservation_cleanup"
	reservationCleanupBatch = 200
)

// AbandonSource lists RESERVED sessions whose start deadline has passed (spec 9.9.3).
type AbandonSource interface {
	ListAbandonCandidates(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
}

// Abandoner is session.Service.Abandon.
type Abandoner interface {
	Abandon(ctx context.Context, id uuid.UUID, actor session.Actor) (session.Session, error)
}

// ReservationCleanupJob abandons sessions that were created but never started, which releases
// their reservation. The transaction and its re-checks belong to session.Service.Abandon.
type ReservationCleanupJob struct {
	src AbandonSource
	ab  Abandoner
	m   *Metrics
	clk clock.Clock
	log *slog.Logger
}

// NewReservationCleanupJob builds the job.
func NewReservationCleanupJob(src AbandonSource, ab Abandoner, m *Metrics, clk clock.Clock, log *slog.Logger) *ReservationCleanupJob {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ReservationCleanupJob{src: src, ab: ab, m: m, clk: clk, log: log}
}

// Run implements Job. Only a failure to list candidates (or a cancelled context) is an error; a
// failing session is logged and counted and the loop goes on.
func (j *ReservationCleanupJob) Run(ctx context.Context) error {
	ids, err := j.src.ListAbandonCandidates(ctx, j.clk.Now(), reservationCleanupBatch)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := j.ab.Abandon(ctx, id, session.ActorCleanup)
		switch {
		case err == nil:
			j.m.JobItems.WithLabelValues(reservationCleanupName, "ok").Inc()
		case errors.Is(err, session.ErrInvalidTransition), errors.Is(err, session.ErrFenceLost):
			j.m.JobItems.WithLabelValues(reservationCleanupName, "noop").Inc() // the user started or cancelled first
		default:
			j.m.JobItems.WithLabelValues(reservationCleanupName, "error").Inc()
			j.log.ErrorContext(ctx, "reservation_cleanup: abandon failed", "session_id", id.String(), "error", err.Error())
		}
	}
	return nil
}
