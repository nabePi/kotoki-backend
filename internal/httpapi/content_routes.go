package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerContentRoutes declares the public catalog (spec 5.8.5, 6.12). There is no conversation
// style or expression profile endpoint (D-014).
func registerContentRoutes(user chi.Router, h *contentHandler) {
	limited := user.With(RateLimit(h.limiter, ratelimit.Get("api_user_default"), UserSubject, h.log))
	limited.Get("/topics", HandleNoBody(http.StatusOK, h.topics))
	limited.Get("/scenarios", HandleNoBody(http.StatusOK, h.scenarios))
	limited.Get("/scenarios/{scenario_id}", HandleNoBody(http.StatusOK, h.scenario))
	limited.Get("/characters", HandleNoBody(http.StatusOK, h.characters))
	limited.Get("/characters/{character_id}", HandleNoBody(http.StatusOK, h.character))
}
