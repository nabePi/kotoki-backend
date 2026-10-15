package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// registerBalanceRoutes declares the user balance endpoints (spec 5.9.5).
func registerBalanceRoutes(user chi.Router, h *balanceHandler) {
	limited := user.With(RateLimit(h.limiter, ratelimit.Get("api_user_default"), UserSubject, h.log))
	limited.Get("/balance", HandleNoBody(http.StatusOK, h.get))
	limited.Get("/balance/transactions", HandleNoBody(http.StatusOK, h.transactions))
}
