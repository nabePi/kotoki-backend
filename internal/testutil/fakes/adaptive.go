package fakes

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.AdaptiveProvider = (*Adaptive)(nil)

// Adaptive is a session.AdaptiveProvider returning a fixed JSON object and recording the requests.
type Adaptive struct {
	FailInjector
	mu       sync.Mutex
	Result   json.RawMessage
	requests []session.AdaptiveRequest
}

// NewAdaptive returns a provider answering result (default `{"formality":"POLITE"}`).
func NewAdaptive(result string) *Adaptive {
	if result == "" {
		result = `{"formality":"POLITE"}`
	}
	return &Adaptive{Result: json.RawMessage(result)}
}

// Adaptive implements session.AdaptiveProvider.
func (a *Adaptive) Adaptive(_ context.Context, req session.AdaptiveRequest) (json.RawMessage, error) {
	if err := a.Take("Adaptive"); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, req)
	return a.Result, nil
}

// Requests returns the requests seen so far.
func (a *Adaptive) Requests() []session.AdaptiveRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]session.AdaptiveRequest(nil), a.requests...)
}
