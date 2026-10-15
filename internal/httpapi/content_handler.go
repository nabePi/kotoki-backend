package httpapi

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

type contentHandler struct {
	svc     contentCatalogService
	limiter ratelimit.Limiter
	log     *slog.Logger
}

// catalogCache is the Cache-Control of every catalog response (spec 6.12).
const catalogCache = "private, max-age=60"

func (h *contentHandler) topics(ctx context.Context, req Request[struct{}]) (itemsResponse[topicResponse], error) {
	list, err := h.svc.Topics(ctx)
	if err != nil {
		return itemsResponse[topicResponse]{}, err
	}
	items := make([]topicResponse, len(list))
	for i, v := range list {
		items[i] = newTopicResponse(v)
	}
	req.SetHeader("Cache-Control", catalogCache)
	return newItems(items), nil
}

func (h *contentHandler) scenarios(ctx context.Context, req Request[struct{}]) (itemsResponse[scenarioResponse], error) {
	var topicID *uuid.UUID
	if raw := req.Query().Get("topic_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return itemsResponse[scenarioResponse]{}, FieldError("topic_id", "invalid_format")
		}
		topicID = &id
	}
	list, err := h.svc.Scenarios(ctx, topicID)
	if err != nil {
		return itemsResponse[scenarioResponse]{}, err
	}
	items := make([]scenarioResponse, len(list))
	for i, v := range list {
		items[i] = newScenarioResponse(v)
	}
	req.SetHeader("Cache-Control", catalogCache)
	return newItems(items), nil
}

func (h *contentHandler) scenario(ctx context.Context, req Request[struct{}]) (scenarioResponse, error) {
	id, err := req.PathUUID("scenario_id")
	if err != nil {
		return scenarioResponse{}, err
	}
	v, err := h.svc.Scenario(ctx, id)
	if err != nil {
		return scenarioResponse{}, err
	}
	req.SetHeader("Cache-Control", catalogCache)
	return newScenarioResponse(v), nil
}

func (h *contentHandler) characters(ctx context.Context, req Request[struct{}]) (itemsResponse[characterResponse], error) {
	list, err := h.svc.Characters(ctx)
	if err != nil {
		return itemsResponse[characterResponse]{}, err
	}
	items := make([]characterResponse, len(list))
	for i, v := range list {
		items[i] = newCharacterResponse(v)
	}
	req.SetHeader("Cache-Control", catalogCache)
	return newItems(items), nil
}

func (h *contentHandler) character(ctx context.Context, req Request[struct{}]) (characterResponse, error) {
	id, err := req.PathUUID("character_id")
	if err != nil {
		return characterResponse{}, err
	}
	v, err := h.svc.Character(ctx, id)
	if err != nil {
		return characterResponse{}, err
	}
	req.SetHeader("Cache-Control", catalogCache)
	return newCharacterResponse(v), nil
}
