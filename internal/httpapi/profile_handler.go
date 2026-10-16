package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type profileHandler struct {
	svc     profileService
	users   userService
	limiter ratelimit.Limiter
	log     *slog.Logger
}

func (h *profileHandler) get(ctx context.Context, req Request[struct{}]) (profileResponse, error) {
	v, err := h.svc.GetProfile(ctx, req.Principal.UserID)
	if err != nil {
		return profileResponse{}, err
	}
	return newProfileResponse(v), nil
}

func (h *profileHandler) patch(ctx context.Context, req Request[patchProfileRequest]) (profileResponse, error) {
	if _, err := h.users.RequireActive(ctx, req.Principal.UserID); err != nil {
		return profileResponse{}, err
	}
	v, err := h.svc.PatchProfile(ctx, req.Principal.UserID, req.Body.input())
	if err != nil {
		return profileResponse{}, err
	}
	return newProfileResponse(v), nil
}

func (h *profileHandler) skills(ctx context.Context, req Request[struct{}]) (skillsResponse, error) {
	v, err := h.svc.Skills(ctx, req.Principal.UserID)
	if err != nil {
		return skillsResponse{}, err
	}
	return newSkillsResponse(v), nil
}

func (h *profileHandler) history(ctx context.Context, req Request[struct{}]) (historyResponse, error) {
	skill, ok := learner.ParseSkill(req.PathParam("skill"))
	if !ok {
		return historyResponse{}, FieldError("skill", "invalid_format")
	}
	q := req.Query()
	limit, err := ParseLimit(q)
	if err != nil {
		return historyResponse{}, err
	}
	var after *learner.HistoryCursor
	if c := q.Get("cursor"); c != "" {
		if after, err = decodeHistoryCursor(c); err != nil {
			return historyResponse{}, err
		}
	}
	rows, next, err := h.svc.History(ctx, req.Principal.UserID, skill, after, limit)
	if err != nil {
		return historyResponse{}, err
	}
	var nc *string
	if next != nil {
		s := EncodeCursor([]any{next.AssessedAt.UTC().Format(time.RFC3339Nano), next.ID})
		nc = &s
	}
	return newHistoryResponse(skill, rows, nc), nil
}

func (h *profileHandler) resetSkills(ctx context.Context, req Request[struct{}]) (NoContent, error) {
	if _, err := h.users.RequireActive(ctx, req.Principal.UserID); err != nil {
		return NoContent{}, err
	}
	return NoContent{}, h.svc.ResetSkills(ctx, req.Principal.UserID)
}

// decodeHistoryCursor reads the key [assessed_at, id] of spec 5.8.1; anything else is INVALID_CURSOR.
func decodeHistoryCursor(s string) (*learner.HistoryCursor, error) {
	var k []json.RawMessage
	if err := DecodeCursor(s, &k); err != nil {
		return nil, err
	}
	if len(k) != 2 {
		return nil, invalidCursor()
	}
	var at time.Time
	var id uuid.UUID
	if json.Unmarshal(k[0], &at) != nil || json.Unmarshal(k[1], &id) != nil || at.IsZero() || id == uuid.Nil {
		return nil, invalidCursor()
	}
	return &learner.HistoryCursor{AssessedAt: at.UTC(), ID: id}, nil
}
