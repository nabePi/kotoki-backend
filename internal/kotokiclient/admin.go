package kotokiclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// ErrIdempotencyKeyReused is wrapped by the error of an admin call the server refused with
// 409 IDEMPOTENCY_KEY_REUSED (the key was used before with another payload).
var ErrIdempotencyKeyReused = errors.New("kotokiclient: idempotency key reused with a different request")

// AdminGrantBalance grants seconds of balance to a user (POST /admin/users/{id}/balance-grants).
// The client must be signed in as an admin. idempotencyKey must be a UUID (the server checks); a
// replay with the same key and payload succeeds again without granting twice.
func (c *Client) AdminGrantBalance(ctx context.Context, userID uuid.UUID, seconds int64, reason, idempotencyKey string) error {
	body := struct {
		Seconds int64  `json:"seconds"`
		Reason  string `json:"reason"`
	}{seconds, reason}
	err := c.do(ctx, http.MethodPost, "/admin/users/"+userID.String()+"/balance-grants",
		callOpts{body: body, headers: map[string]string{"Idempotency-Key": idempotencyKey}}, nil)
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusConflict && api.Code == "IDEMPOTENCY_KEY_REUSED" {
		return fmt.Errorf("%w: %w", ErrIdempotencyKeyReused, api)
	}
	return err
}
