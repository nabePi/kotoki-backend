package httpapi

import (
	"context"
	"encoding/json"

	"github.com/nabePi/kotoki-backend/internal/content"
)

type adminContentHandler struct{ svc contentAdminService }

func (h *adminContentHandler) family(req Request[json.RawMessage]) content.Family {
	return content.Family(req.PathParam("family"))
}

func (h *adminContentHandler) listVersions(ctx context.Context, req Request[struct{}]) (itemsResponse[any], error) {
	list, err := h.svc.ListVersions(ctx, content.Family(req.PathParam("family")), req.PathParam("parent"))
	if err != nil {
		return itemsResponse[any]{}, err
	}
	items := make([]any, len(list))
	for i, v := range list {
		items[i] = newVersionResponse(v)
	}
	return newItems(items), nil
}

func (h *adminContentHandler) getVersion(ctx context.Context, req Request[struct{}]) (any, error) {
	id, err := req.PathUUID("version_id")
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetVersion(ctx, content.Family(req.PathParam("family")), req.PathParam("parent"), id)
	if err != nil {
		return nil, err
	}
	return newVersionResponse(v), nil
}

func (h *adminContentHandler) createVersion(ctx context.Context, req Request[json.RawMessage]) (any, error) {
	v, err := h.svc.CreateDraft(ctx, req.Principal.UserID, h.family(req), req.PathParam("parent"), req.Body, nil)
	if err != nil {
		return nil, err
	}
	return newVersionResponse(v), nil
}

func (h *adminContentHandler) publish(ctx context.Context, req Request[struct{}]) (publishResponse, error) {
	id, err := req.PathUUID("version_id")
	if err != nil {
		return publishResponse{}, err
	}
	res, err := h.svc.Publish(ctx, req.Principal.UserID, content.Family(req.PathParam("family")), req.PathParam("parent"), id)
	if err != nil {
		return publishResponse{}, err
	}
	return publishResponse{Version: newVersionResponse(res.Version), RetiredVersionID: res.RetiredVersionID, Warnings: orEmptyIssues(res.Warnings)}, nil
}

func (h *adminContentHandler) retire(ctx context.Context, req Request[struct{}]) (retireResponse, error) {
	id, err := req.PathUUID("version_id")
	if err != nil {
		return retireResponse{}, err
	}
	res, err := h.svc.Retire(ctx, req.Principal.UserID, content.Family(req.PathParam("family")), req.PathParam("parent"), id)
	if err != nil {
		return retireResponse{}, err
	}
	return retireResponse{Version: newVersionResponse(res.Version), Warnings: orEmptyIssues(res.Warnings)}, nil
}

func (h *adminContentHandler) deleteVersion(ctx context.Context, req Request[struct{}]) (NoContent, error) {
	id, err := req.PathUUID("version_id")
	if err != nil {
		return NoContent{}, err
	}
	err = h.svc.DeleteDraft(ctx, req.Principal.UserID, content.Family(req.PathParam("family")), req.PathParam("parent"), id)
	return NoContent{}, err
}

func (h *adminContentHandler) setStatus(ctx context.Context, req Request[statusRequest]) (entityStatusResponse, error) {
	id, err := req.PathUUID("id")
	if err != nil {
		return entityStatusResponse{}, err
	}
	v, err := h.svc.SetStatus(ctx, req.Principal.UserID, req.PathParam("entity"), id, content.EntityStatus(req.Body.Status))
	if err != nil {
		return entityStatusResponse{}, err
	}
	return entityStatusResponse(v), nil
}

func (h *adminContentHandler) getCharacter(ctx context.Context, req Request[struct{}]) (characterAdminResponse, error) {
	id, err := req.PathUUID("id")
	if err != nil {
		return characterAdminResponse{}, err
	}
	c, err := h.svc.GetCharacter(ctx, id)
	if err != nil {
		return characterAdminResponse{}, err
	}
	return newCharacterAdminResponse(c), nil
}

func (h *adminContentHandler) patchCharacter(ctx context.Context, req Request[patchCharacterRequest]) (characterUpdateResponse, error) {
	id, err := req.PathUUID("id")
	if err != nil {
		return characterUpdateResponse{}, err
	}
	patch, err := req.Body.patch()
	if err != nil {
		return characterUpdateResponse{}, err
	}
	res, err := h.svc.UpdateCharacter(ctx, req.Principal.UserID, id, patch)
	if err != nil {
		return characterUpdateResponse{}, err
	}
	return characterUpdateResponse{Character: newCharacterAdminResponse(res.Character), Warnings: orEmptyIssues(res.Warnings)}, nil
}

func (h *adminContentHandler) validationReport(ctx context.Context, _ Request[struct{}]) (validationReportResponse, error) {
	errsOut, warns, err := h.svc.ValidationReport(ctx)
	if err != nil {
		return validationReportResponse{}, err
	}
	return validationReportResponse{Errors: orEmptyIssues(errsOut), Warnings: orEmptyIssues(warns)}, nil
}
