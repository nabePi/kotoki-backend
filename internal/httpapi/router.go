package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// NewRouter builds the public API: three route groups (public, user, admin) under
// /api/v1 with the middleware order of spec 3.7. Operational endpoints are never served here.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestID, ClientIPMiddleware(d.Cfg.TrustedProxies), Recoverer(d.Log), AccessLog(d.Log))
	if d.Metrics != nil {
		r.Use(HTTPMetrics(d.Metrics))
	}
	r.Use(Telemetry, SecurityHeaders, CORS(d.Cfg.CORSOrigins), BodyLimit(d.Cfg.BodyLimit))
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, errs.New(errs.CodeNotFound, "route not found"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, errs.New(errs.CodeMethodNotAllowed, "method not allowed"))
	})

	r.Route("/api/v1", func(api chi.Router) {
		pub := api
		user := api.With(Authn(d.Tokens, d.Clock))
		registerAuthRoutes(pub, user, &authHandler{svc: d.Auth, users: d.Users, limiter: d.Limiter, log: d.Log, cfg: d.Cfg})
		registerMeRoutes(user, &meHandler{svc: d.Users, limiter: d.Limiter, log: d.Log, cfg: d.Cfg})
		registerBalanceRoutes(user, &balanceHandler{svc: d.Balance, limiter: d.Limiter, log: d.Log})
		registerContentRoutes(user, &contentHandler{svc: d.Content, limiter: d.Limiter, log: d.Log})
		registerSessionRoutes(user, &sessionHandler{svc: d.Session, query: d.SessionQuery, tickets: d.Tickets, clk: d.Clock, limiter: d.Limiter, log: d.Log})
		adminGroup := user.With(
			RateLimit(d.Limiter, ratelimit.Get("admin_user"), UserSubject, d.Log),
			RequireAdmin(d.AdminGate, d.Log))
		registerAdminRoutes(adminGroup, &adminHandler{svc: d.Admin, clk: d.Clock})
		registerAdminContentRoutes(adminGroup, &adminContentHandler{svc: d.ContentAdmin})
	})
	return r
}
