package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerMemoryRoutes declares the memory endpoints of the User group (spec 5.9.7, 11.9).
func registerMemoryRoutes(user chi.Router, h *memoryHandler) {
	limited := user.With(RateLimit(h.limiter, ratelimit.Get("api_user_default"), UserSubject, h.log))
	limited.Get("/profile/memories", HandleNoBody(http.StatusOK, h.list))
	limited.Delete("/profile/memories", HandleNoBody(http.StatusNoContent, h.deleteAll))
	limited.Delete("/profile/memories/{memory_id}", HandleNoBody(http.StatusNoContent, h.delete))
}
