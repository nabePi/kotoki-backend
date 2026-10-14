package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerMeRoutes declares /me* (spec 5.9.2); every route needs a Bearer token.
func registerMeRoutes(user chi.Router, h *meHandler) {
	limit := func(policy string) func(http.Handler) http.Handler {
		return RateLimit(h.limiter, ratelimit.Get(policy), UserSubject, h.log)
	}
	user.With(limit("api_user_default")).Get("/me", HandleNoBody(http.StatusOK, h.get))
	user.With(limit("api_user_default")).Put("/me/consents", Handle(http.StatusOK, h.consents))
	user.With(limit("account_delete_user")).Delete("/me", Handle(http.StatusAccepted, h.delete))
}
