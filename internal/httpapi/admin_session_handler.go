package httpapi

import (
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

const defaultEventsLimit, maxEventsLimit = 100, 100

type adminSessionHandler struct {
	svc   adminSessionService
	audit transcriptAuditor
	clk   clock.Clock
}

func parseEventsLimit(raw string) (int, error) {
	if raw == "" {
		return defaultEventsLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, FieldError("limit", "invalid_type")
	}
	if n < 1 || n > maxEventsLimit {
		return 0, FieldError("limit", "out_of_range")
	}
	return n, nil
}

func (h *adminSessionHandler) listByUser(ctx context.Context, req Request[struct{}]) (Page[adminSessionResponse], error) {
	userID, err := req.PathUUID("user_id")
	if err != nil {
		return Page[adminSessionResponse]{}, err
	}
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return Page[adminSessionResponse]{}, err
	}
	statuses, err := parseStatuses(q.Get("status"))
	if err != nil {
		return Page[adminSessionResponse]{}, err
	}
	after, err := parseUUIDCursor(q.Get("cursor"))
	if err != nil {
		return Page[adminSessionResponse]{}, err
	}
	rows, err := h.svc.ListByUser(ctx, userID, statuses, after, limit+1)
	if err != nil {
		return Page[adminSessionResponse]{}, err
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		id := rows[limit-1].ID
		next = &id
	}
	now := h.clk.Now()
	items := make([]adminSessionResponse, len(rows))
	for i, v := range rows {
		items[i] = newAdminSessionResponse(v, now)
	}
	return NewPage(items, nextCursor(next)), nil
}

func (h *adminSessionHandler) get(ctx context.Context, req Request[struct{}]) (adminSessionResponse, error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return adminSessionResponse{}, err
	}
	v, err := h.svc.Get(ctx, id)
	if err != nil {
		return adminSessionResponse{}, err
	}
	return newAdminSessionResponse(v, h.clk.Now()), nil
}

// turns returns the transcript. The audit row is written only after the read succeeded (404 and 410
// leave none) and before anything is returned: if it cannot be written the transcript is not sent.
func (h *adminSessionHandler) turns(ctx context.Context, req Request[struct{}]) (Page[adminTurnResponse], error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return Page[adminTurnResponse]{}, err
	}
	q := req.Query()
	limit, err := parseTurnsLimit(q.Get("limit"))
	if err != nil {
		return Page[adminTurnResponse]{}, err
	}
	var after *int
	if raw := q.Get("cursor"); raw != "" {
		var seq int
		if err := DecodeCursor(raw, &seq); err != nil {
			return Page[adminTurnResponse]{}, err
		}
		after = &seq
	}
	owner, err := h.svc.Get(ctx, id)
	if err != nil {
		return Page[adminTurnResponse]{}, err
	}
	rows, err := h.svc.Turns(ctx, id, after, limit+1)
	if err != nil {
		return Page[adminTurnResponse]{}, err
	}
	if err := h.audit.RecordTranscriptView(ctx, req.Principal.UserID, id, owner.UserID, metaOf(req.RequestID, req.ClientIP, req.UserAgent)); err != nil {
		return Page[adminTurnResponse]{}, err
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := EncodeCursor(rows[limit-1].Sequence)
		next = &c
	}
	items := make([]adminTurnResponse, len(rows))
	for i, t := range rows {
		items[i] = newAdminTurnResponse(t)
	}
	return NewPage(items, next), nil
}

func (h *adminSessionHandler) events(ctx context.Context, req Request[struct{}]) (Page[adminEventResponse], error) {
	id, err := req.PathUUIDOr("session_id", errs.CodeSessionNotFound)
	if err != nil {
		return Page[adminEventResponse]{}, err
	}
	q := req.Query()
	limit, err := parseEventsLimit(q.Get("limit"))
	if err != nil {
		return Page[adminEventResponse]{}, err
	}
	after, err := decodeEventCursor(q.Get("cursor"))
	if err != nil {
		return Page[adminEventResponse]{}, err
	}
	rows, err := h.svc.Events(ctx, id, after, limit+1)
	if err != nil {
		return Page[adminEventResponse]{}, err
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := EncodeCursor(eventCursorKey(rows[limit-1]))
		next = &c
	}
	items := make([]adminEventResponse, len(rows))
	for i, e := range rows {
		items[i] = newAdminEventResponse(e)
	}
	return NewPage(items, next), nil
}
