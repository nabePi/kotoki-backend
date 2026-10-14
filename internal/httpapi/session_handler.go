package httpapi

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type sessionHandler struct {
	svc     sessionService
	query   sessionQueryService
	tickets ticketService
	clk     clock.Clock
	limiter ratelimit.Limiter
	log     *slog.Logger
}

const (
	defaultTurnsLimit = 50
	maxTurnsLimit     = 100
)

func (h *sessionHandler) view(ctx context.Context, userID, id uuid.UUID) (sessionResponse, error) {
	v, err := h.query.Get(ctx, userID, id)
	if err != nil {
		return sessionResponse{}, err
	}
	return newSessionResponse(v, h.clk.Now()), nil
}

func (h *sessionHandler) create(ctx context.Context, req Request[createSessionRequest]) (sessionResponse, error) {
	in, err := req.Body.input(req.Principal.UserID, req.IdempotencyKey)
	if err != nil {
		return sessionResponse{}, err
	}
	res, err := h.svc.Create(ctx, in)
	if err != nil {
		return sessionResponse{}, err
	}
	out, err := h.view(ctx, req.Principal.UserID, res.Session.ID)
	if err != nil {
		return sessionResponse{}, err
	}
	if res.Replayed {
		req.SetStatus(200)
		req.SetHeader("Idempotent-Replayed", "true")
	} else {
		req.SetHeader("Location", "/api/v1/sessions/"+res.Session.ID.String())
	}
	return out, nil
}

func (h *sessionHandler) list(ctx context.Context, req Request[struct{}]) (Page[sessionListItem], error) {
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return Page[sessionListItem]{}, err
	}
	statuses, err := parseStatuses(q.Get("status"))
	if err != nil {
		return Page[sessionListItem]{}, err
	}
	after, err := parseUUIDCursor(q.Get("cursor"))
	if err != nil {
		return Page[sessionListItem]{}, err
	}
	rows, err := h.query.List(ctx, req.Principal.UserID, statuses, after, limit+1)
	if err != nil {
		return Page[sessionListItem]{}, err
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		id := rows[limit-1].ID
		next = &id
	}
	items := make([]sessionListItem, len(rows))
	for i, v := range rows {
		items[i] = newSessionListItem(v)
	}
	return NewPage(items, nextCursor(next)), nil
}

func (h *sessionHandler) get(ctx context.Context, req Request[struct{}]) (sessionResponse, error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return sessionResponse{}, err
	}
	return h.view(ctx, req.Principal.UserID, id)
}

func (h *sessionHandler) cancel(ctx context.Context, req Request[struct{}]) (sessionResponse, error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return sessionResponse{}, err
	}
	if _, err := h.svc.Cancel(ctx, req.Principal.UserID, id); err != nil {
		return sessionResponse{}, err
	}
	return h.view(ctx, req.Principal.UserID, id)
}

func (h *sessionHandler) ticket(ctx context.Context, req Request[struct{}]) (ticketResponse, error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return ticketResponse{}, err
	}
	res, err := h.tickets.Issue(ctx, req.Principal.UserID, id)
	if err != nil {
		return ticketResponse{}, err
	}
	return newTicketResponse(id, res), nil
}

// parseTurnsLimit reads `limit` for the transcript: default 50, 1–100.
func parseTurnsLimit(raw string) (int, error) {
	if raw == "" {
		return defaultTurnsLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, FieldError("limit", "invalid_type")
	}
	if n < 1 || n > maxTurnsLimit {
		return 0, FieldError("limit", "out_of_range")
	}
	return n, nil
}

func (h *sessionHandler) turns(ctx context.Context, req Request[struct{}]) (Page[turnResponse], error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return Page[turnResponse]{}, err
	}
	q := req.Query()
	limit, err := parseTurnsLimit(q.Get("limit"))
	if err != nil {
		return Page[turnResponse]{}, err
	}
	var after *int
	if raw := q.Get("cursor"); raw != "" {
		var seq int
		if err := DecodeCursor(raw, &seq); err != nil {
			return Page[turnResponse]{}, err
		}
		after = &seq
	}
	rows, err := h.query.Turns(ctx, req.Principal.UserID, id, after, limit+1)
	if err != nil {
		return Page[turnResponse]{}, err
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := EncodeCursor(rows[limit-1].Sequence)
		next = &c
	}
	items := make([]turnResponse, len(rows))
	for i, t := range rows {
		items[i] = newTurnResponse(t)
	}
	return NewPage(items, next), nil
}
