package httpapi

import (
	"context"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type evaluationHandler struct {
	sessions evaluationSessionGetter
	query    evaluationQuery
	limiter  ratelimit.Limiter
	log      *slog.Logger
}

// retryAfterSeconds is the polling hint of a PENDING or RUNNING evaluation (spec 10.11).
const retryAfterSeconds = "3"

// get resolves the session through the owner-scoped session read (a session of another user or an
// unknown id is SESSION_NOT_FOUND) and then applies the evaluation matrix.
func (h *evaluationHandler) get(ctx context.Context, req Request[struct{}]) (evaluationResponse, error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return evaluationResponse{}, err
	}
	s, err := h.sessions.Get(ctx, req.Principal.UserID, id)
	if err != nil {
		return evaluationResponse{}, err
	}
	v, err := h.query.Get(ctx, evaluation.SessionRef{
		ID: s.ID, Status: string(s.Status), StartedAt: s.StartedAt, TranscriptPurged: s.TranscriptPurgedAt != nil,
	})
	if err != nil {
		return evaluationResponse{}, err
	}
	if v.Status == evaluation.StatusPending || v.Status == evaluation.StatusRunning {
		req.SetHeader("Retry-After", retryAfterSeconds)
	}
	return newEvaluationResponse(v), nil
}
