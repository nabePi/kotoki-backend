package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type memoryHandler struct {
	svc     memoryService
	users   userService
	limiter ratelimit.Limiter
	log     *slog.Logger
}

func (h *memoryHandler) list(ctx context.Context, req Request[struct{}]) (memoryListResponse, error) {
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return memoryListResponse{}, err
	}
	var cat *memory.Category
	if s := q.Get("category"); s != "" {
		c, ok := memory.ParseCategory(s)
		if !ok {
			return memoryListResponse{}, FieldError("category", "invalid_format")
		}
		cat = &c
	}
	var after *memory.ListCursor
	if c := q.Get("cursor"); c != "" {
		if after, err = decodeMemoryCursor(c); err != nil {
			return memoryListResponse{}, err
		}
	}
	res, err := h.svc.List(ctx, req.Principal.UserID, cat, after, limit)
	if err != nil {
		return memoryListResponse{}, err
	}
	var next *string
	if res.Next != nil {
		s := EncodeCursor([]any{string(res.Next.Category), res.Next.LastObservedAt.UTC().Format(time.RFC3339Nano), res.Next.ID})
		next = &s
	}
	return newMemoryListResponse(res, next), nil
}

func (h *memoryHandler) delete(ctx context.Context, req Request[struct{}]) (NoContent, error) {
	if _, err := h.users.RequireActive(ctx, req.Principal.UserID); err != nil {
		return NoContent{}, err
	}
	id, err := req.PathUUIDOr("memory_id", errs.CodeMemoryNotFound)
	if err != nil {
		return NoContent{}, err
	}
	return NoContent{}, h.svc.Delete(ctx, req.Principal.UserID, id)
}

func (h *memoryHandler) deleteAll(ctx context.Context, req Request[struct{}]) (NoContent, error) {
	if _, err := h.users.RequireActive(ctx, req.Principal.UserID); err != nil {
		return NoContent{}, err
	}
	return NoContent{}, h.svc.DeleteAll(ctx, req.Principal.UserID)
}

// decodeMemoryCursor reads the key [category, last_observed_at, id] of spec 5.9.7.
func decodeMemoryCursor(s string) (*memory.ListCursor, error) {
	var k []json.RawMessage
	if err := DecodeCursor(s, &k); err != nil {
		return nil, err
	}
	if len(k) != 3 {
		return nil, invalidCursor()
	}
	var cat string
	var at time.Time
	var id uuid.UUID
	if json.Unmarshal(k[0], &cat) != nil || json.Unmarshal(k[1], &at) != nil || json.Unmarshal(k[2], &id) != nil {
		return nil, invalidCursor()
	}
	c, ok := memory.ParseCategory(cat)
	if !ok || at.IsZero() || id == uuid.Nil {
		return nil, invalidCursor()
	}
	return &memory.ListCursor{Category: c, LastObservedAt: at.UTC(), ID: id}, nil
}
