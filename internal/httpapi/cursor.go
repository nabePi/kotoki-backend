package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

const (
	defaultLimit = 20
	maxLimit     = 100
	cursorV      = 1
)

// Page is the list envelope (spec 5.8.1).
type Page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// NewPage builds a Page; a nil slice renders as [].
func NewPage[T any](items []T, next *string) Page[T] {
	if items == nil {
		items = []T{}
	}
	return Page[T]{Data: items, NextCursor: next}
}

type cursorPayload struct {
	V int             `json:"v"`
	K json.RawMessage `json:"k"`
}

// EncodeCursor encodes key as the opaque cursor {"v":1,"k":key} (base64url, no padding).
func EncodeCursor(key any) string {
	k, err := json.Marshal(key)
	if err != nil {
		k = []byte("null")
	}
	b, _ := json.Marshal(cursorPayload{V: cursorV, K: k})
	return base64.RawURLEncoding.EncodeToString(b)
}

func invalidCursor() error { return errs.New(errs.CodeInvalidCursor, "invalid cursor") }

// DecodeCursor decodes s into key; any failure is INVALID_CURSOR.
func DecodeCursor(s string, key any) error {
	invalid := invalidCursor()
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return invalid
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.V != cursorV || len(p.K) == 0 {
		return invalid
	}
	if err := json.Unmarshal(p.K, key); err != nil {
		return invalid
	}
	return nil
}

// ParseLimit reads `limit` (default 20, 1–100).
func ParseLimit(q url.Values) (int, error) {
	s := q.Get("limit")
	if s == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, FieldError("limit", "invalid_type")
	}
	if n < 1 || n > maxLimit {
		return 0, FieldError("limit", "out_of_range")
	}
	return n, nil
}
