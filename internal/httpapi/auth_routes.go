package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerAuthRoutes declares every /auth/* path (spec 5.9.1). The per-IP policies
// are attached here; the per-email policies run in the handler after the body is decoded.
func registerAuthRoutes(pub, _ chi.Router, h *authHandler) {
	limitIP := func(policy string) func(http.Handler) http.Handler {
		return RateLimit(h.limiter, ratelimit.Get(policy), IPSubject(h.cfg.TrustedProxies), h.log)
	}
	origin := OriginCheck(h.cfg.CORSOrigins)

	pub.With(limitIP("auth_register_ip")).Post("/auth/register", Handle(http.StatusCreated, h.register))
	pub.With(limitIP("auth_login_ip")).Post("/auth/login", Handle(http.StatusOK, h.login))
	pub.With(limitIP("auth_refresh_ip"), origin).Post("/auth/refresh", HandleNoBody(http.StatusOK, h.refresh))
	pub.With(limitIP("auth_refresh_ip"), origin).Post("/auth/logout", HandleNoBody(http.StatusNoContent, h.logout))
	pub.With(limitIP("auth_forgot_ip")).Post("/auth/forgot-password", Handle(http.StatusAccepted, h.forgot))
	pub.With(limitIP("auth_reset_ip")).Post("/auth/reset-password", Handle(http.StatusNoContent, h.reset))
	pub.With(limitIP("api_ip_default")).Get("/auth/terms", HandleNoBody(http.StatusOK, h.terms))
}
