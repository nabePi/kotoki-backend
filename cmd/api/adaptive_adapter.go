package main

import (
	"context"
	"encoding/json"

	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// adaptiveAdapter adapts learner.Service to session.AdaptiveProvider (spec 12.8.2). It lives here
// because learner must not import session. An error is returned as is, so that session.Service.Create
// maps it to 503 SERVICE_UNAVAILABLE: there is no default difficulty to fall back to.
type adaptiveAdapter struct{ svc *learner.Service }

var _ session.AdaptiveProvider = adaptiveAdapter{}

// Adaptive returns the adaptive difficulty of the new session as the JSON object stored in
// sessions.adaptive_difficulty.
func (a adaptiveAdapter) Adaptive(ctx context.Context, req session.AdaptiveRequest) (json.RawMessage, error) {
	ad, err := a.svc.ComputeAdaptiveDifficulty(ctx, learner.AdaptiveRequest{UserID: req.UserID, Config: req.Config})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(ad)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "encode adaptive difficulty failed")
	}
	return raw, nil
}
