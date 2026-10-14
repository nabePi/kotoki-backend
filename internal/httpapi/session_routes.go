package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerSessionRoutes declares the session endpoints of the User group (spec 5.8.5, 5.9.4); each
// route carries exactly the rate policy of the index.
func registerSessionRoutes(user chi.Router, h *sessionHandler) {
	policy := func(name string) chi.Router {
		return user.With(RateLimit(h.limiter, ratelimit.Get(name), UserSubject, h.log))
	}
	policy("session_create_user").Post("/sessions", Handle(http.StatusCreated, h.create))
	def := policy("api_user_default")
	def.Get("/sessions", HandleNoBody(http.StatusOK, h.list))
	def.Get("/sessions/{session_id}", HandleNoBody(http.StatusOK, h.get))
	def.Post("/sessions/{session_id}/cancel", HandleNoBody(http.StatusOK, h.cancel))
	policy("ticket_user").Post("/sessions/{session_id}/realtime-ticket", HandleNoBody(http.StatusOK, h.ticket))
	def.Get("/sessions/{session_id}/turns", HandleNoBody(http.StatusOK, h.turns))
}
