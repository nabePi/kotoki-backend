package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerProfileRoutes declares the profile and skill endpoints of the User group (spec 5.9.7).
func registerProfileRoutes(user chi.Router, h *profileHandler) {
	limited := user.With(RateLimit(h.limiter, ratelimit.Get("api_user_default"), UserSubject, h.log))
	limited.Get("/profile", HandleNoBody(http.StatusOK, h.get))
	limited.Patch("/profile", Handle(http.StatusOK, h.patch))
	limited.Get("/profile/skills", HandleNoBody(http.StatusOK, h.skills))
	limited.Get("/profile/skills/{skill}/history", HandleNoBody(http.StatusOK, h.history))
	limited.Delete("/profile/skills", HandleNoBody(http.StatusNoContent, h.resetSkills))
}
