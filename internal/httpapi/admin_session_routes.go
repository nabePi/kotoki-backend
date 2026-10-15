package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// registerAdminSessionRoutes declares the admin session reads (spec 5.9.8). The group already
// carries Authn, the admin_user rate limit and RequireAdmin.
func registerAdminSessionRoutes(a chi.Router, h *adminSessionHandler) {
	a.Get("/admin/users/{user_id}/sessions", HandleNoBody(http.StatusOK, h.listByUser))
	a.Get("/admin/sessions/{session_id}", HandleNoBody(http.StatusOK, h.get))
	a.Get("/admin/sessions/{session_id}/turns", HandleNoBody(http.StatusOK, h.turns))
	a.Get("/admin/sessions/{session_id}/events", HandleNoBody(http.StatusOK, h.events))
}
