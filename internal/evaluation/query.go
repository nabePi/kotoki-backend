package evaluation

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// SessionRef is what the matrix of spec 10.11 needs to know about the session; the caller has
// already established that the session belongs to the requesting user.
type SessionRef struct {
	ID               uuid.UUID
	Status           string
	StartedAt        *time.Time
	TranscriptPurged bool
}

// View is the outcome of the matrix. Eval is nil when no evaluation row exists yet (Status is then
// the synthesized PENDING); Findings are only loaded for a COMPLETED evaluation whose details are
// still retained.
type View struct {
	SessionID        uuid.UUID
	Status           Status
	Eval             *Evaluation
	Findings         []FindingRow
	TranscriptPurged bool
}

// QueryService is the read side of the evaluation API (spec 10.11). It never writes.
type QueryService struct {
	r Reader
}

// NewQueryService returns a QueryService over r.
func NewQueryService(r Reader) *QueryService { return &QueryService{r: r} }

// Get evaluates the matrix in order: a session that has not ended is 409 SESSION_NOT_ENDED
// (details.session_status); a terminal session that was never ACTIVE is 404 EVALUATION_NOT_AVAILABLE;
// otherwise the stored evaluation, or a synthesized PENDING one when the worker has not written a row.
func (q *QueryService) Get(ctx context.Context, ref SessionRef) (View, error) {
	switch ref.Status {
	case "RESERVED", "STARTING", "ACTIVE", "ENDING":
		return View{}, errs.WithDetails(errs.New(errs.CodeSessionNotEnded, "session has not ended"),
			map[string]any{"session_status": ref.Status})
	}
	if ref.StartedAt == nil {
		return View{}, errs.New(errs.CodeEvaluationNotAvailable, "session never started")
	}
	ev, ok, err := q.r.GetBySession(ctx, ref.ID)
	if err != nil {
		return View{}, err
	}
	if !ok {
		return View{SessionID: ref.ID, Status: StatusPending, TranscriptPurged: ref.TranscriptPurged}, nil
	}
	v := View{SessionID: ref.ID, Status: ev.Status, Eval: &ev, TranscriptPurged: ref.TranscriptPurged}
	if ev.Status == StatusCompleted && ev.DetailsPurgedAt == nil {
		if v.Findings, err = q.r.ListFindings(ctx, ev.ID); err != nil {
			return View{}, err
		}
	}
	return v, nil
}
