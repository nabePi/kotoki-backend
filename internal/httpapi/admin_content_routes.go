package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// registerAdminContentRoutes declares /admin/content/* (spec 6.13.1). The group already carries
// Authn, the admin_user rate limit and RequireAdmin. {family} is scenarios|prompts, {entity} is
// topics|scenarios; characters have no versions and no status endpoint: PATCH edits them directly.
func registerAdminContentRoutes(a chi.Router, h *adminContentHandler) {
	a.Get("/admin/content/validation-report", HandleNoBody(http.StatusOK, h.validationReport))
	a.Get("/admin/content/characters/{id}", HandleNoBody(http.StatusOK, h.getCharacter))
	a.Patch("/admin/content/characters/{id}", Handle(http.StatusOK, h.patchCharacter))
	a.Get("/admin/content/{family}/{parent}/versions", HandleNoBody(http.StatusOK, h.listVersions))
	a.Post("/admin/content/{family}/{parent}/versions", Handle(http.StatusCreated, h.createVersion))
	a.Get("/admin/content/{family}/{parent}/versions/{version_id}", HandleNoBody(http.StatusOK, h.getVersion))
	a.Post("/admin/content/{family}/{parent}/versions/{version_id}/publish", HandleNoBody(http.StatusOK, h.publish))
	a.Post("/admin/content/{family}/{parent}/versions/{version_id}/retire", HandleNoBody(http.StatusOK, h.retire))
	a.Delete("/admin/content/{family}/{parent}/versions/{version_id}", HandleNoBody(http.StatusNoContent, h.deleteVersion))
	a.Patch("/admin/content/{entity}/{id}/status", Handle(http.StatusOK, h.setStatus))
}
