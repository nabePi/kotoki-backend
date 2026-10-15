package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// registerAdminRoutes declares the admin endpoints of M2 (spec 5.9.8). The group
// already carries Authn, the admin_user rate limit and RequireAdmin.
func registerAdminRoutes(a chi.Router, h *adminHandler) {
	a.Get("/admin/users", HandleNoBody(http.StatusOK, h.listUsers))
	a.Get("/admin/users/{user_id}", HandleNoBody(http.StatusOK, h.getUser))
	a.Patch("/admin/users/{user_id}", Handle(http.StatusOK, h.patchUser))
	a.Post("/admin/users/{user_id}/balance-grants", Handle(http.StatusCreated, h.grant))
	a.Post("/admin/users/{user_id}/balance-adjustments", Handle(http.StatusCreated, h.adjust))
	a.Get("/admin/users/{user_id}/balance/transactions", HandleNoBody(http.StatusOK, h.transactions))
	a.Get("/admin/audit-logs", HandleNoBody(http.StatusOK, h.auditLogs))
}
