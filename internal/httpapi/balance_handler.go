package httpapi

import (
	"context"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type balanceHandler struct {
	svc     balanceService
	limiter ratelimit.Limiter
	log     *slog.Logger
}

func (h *balanceHandler) get(ctx context.Context, req Request[struct{}]) (balanceResponse, error) {
	v, err := h.svc.GetBalance(ctx, req.Principal.UserID)
	if err != nil {
		return balanceResponse{}, err
	}
	return newBalanceResponse(v), nil
}

func (h *balanceHandler) transactions(ctx context.Context, req Request[struct{}]) (Page[ledgerEntryUserResponse], error) {
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return Page[ledgerEntryUserResponse]{}, err
	}
	types, err := parseEntryTypes(q.Get("entry_type"))
	if err != nil {
		return Page[ledgerEntryUserResponse]{}, err
	}
	after, err := parseUUIDCursor(q.Get("cursor"))
	if err != nil {
		return Page[ledgerEntryUserResponse]{}, err
	}
	entries, next, err := h.svc.ListTransactions(ctx, req.Principal.UserID, types, after, limit)
	if err != nil {
		return Page[ledgerEntryUserResponse]{}, err
	}
	items := make([]ledgerEntryUserResponse, len(entries))
	for i, e := range entries {
		items[i] = newLedgerEntryUserResponse(e)
	}
	return NewPage(items, nextCursor(next)), nil
}
