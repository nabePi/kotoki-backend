package httpapi

import (
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

type memoryItemResponse struct {
	ID               uuid.UUID  `json:"id"`
	Category         string     `json:"category"`
	Content          string     `json:"content"`
	DisplayText      string     `json:"display_text"`
	Confidence       float64    `json:"confidence"`
	ObservationCount int        `json:"observation_count"`
	SourceSessionID  *uuid.UUID `json:"source_session_id"`
	FirstObservedAt  string     `json:"first_observed_at"`
	LastObservedAt   string     `json:"last_observed_at"`
	ExpiresAt        string     `json:"expires_at"`
}

// memoryListResponse is the body of GET /profile/memories: the list envelope plus memory_enabled.
// It never carries the embedding, the key, the status or a tombstone.
type memoryListResponse struct {
	Data          []memoryItemResponse `json:"data"`
	NextCursor    *string              `json:"next_cursor"`
	MemoryEnabled bool                 `json:"memory_enabled"`
}

func newMemoryListResponse(res memory.ListResult, next *string) memoryListResponse {
	out := memoryListResponse{Data: make([]memoryItemResponse, 0, len(res.Items)), NextCursor: next, MemoryEnabled: res.MemoryEnabled}
	for _, m := range res.Items {
		out.Data = append(out.Data, memoryItemResponse{
			ID: m.ID, Category: string(m.Category), Content: m.Content, DisplayText: m.DisplayText,
			Confidence: round(m.Confidence, 3), ObservationCount: m.ObservationCount, SourceSessionID: m.SourceSessionID,
			FirstObservedAt: formatTime(m.FirstObservedAt), LastObservedAt: formatTime(m.LastObservedAt), ExpiresAt: formatTime(m.ExpiresAt),
		})
	}
	return out
}
