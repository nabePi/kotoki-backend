package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerEvaluationRoutes declares GET /sessions/{session_id}/evaluation of the User group
// (spec 5.9.6, 10.11).
func registerEvaluationRoutes(user chi.Router, h *evaluationHandler) {
	limited := user.With(RateLimit(h.limiter, ratelimit.Get("api_user_default"), UserSubject, h.log))
	limited.Get("/sessions/{session_id}/evaluation", HandleNoBody(http.StatusOK, h.get))
}
