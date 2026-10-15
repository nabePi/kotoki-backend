package httpapi

import (
	"context"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type meHandler struct {
	svc     userService
	limiter ratelimit.Limiter
	log     *slog.Logger
	cfg     Config
}

func (h *meHandler) get(ctx context.Context, req Request[struct{}]) (meResponse, error) {
	u, err := h.svc.Me(ctx, req.Principal.UserID)
	if err != nil {
		return meResponse{}, err
	}
	return newMeResponse(u, h.svc.CurrentTermsVersion()), nil
}

func (h *meHandler) consents(ctx context.Context, req Request[consentsRequest]) (meResponse, error) {
	u, err := h.svc.UpdateConsents(ctx, auth.ConsentsInput{UserID: req.Principal.UserID, TermsVersion: req.Body.TermsVersion})
	if err != nil {
		return meResponse{}, err
	}
	return newMeResponse(u, h.svc.CurrentTermsVersion()), nil
}

func (h *meHandler) delete(ctx context.Context, req Request[deleteAccountRequest]) (statusResponse, error) {
	if err := h.svc.DeleteAccount(ctx, req.Principal.UserID, req.Body.Password); err != nil {
		return statusResponse{}, err
	}
	req.SetCookie(newRefreshCookie(h.cfg, "", 0))
	return statusResponse{Status: "DELETION_SCHEDULED"}, nil
}
