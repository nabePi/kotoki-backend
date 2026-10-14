package httpapi

import (
	"strings"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

type balanceResponse struct {
	AvailableSeconds int64   `json:"available_seconds"`
	ReservedSeconds  int64   `json:"reserved_seconds"`
	AllowedDurations []int64 `json:"allowed_durations"`
	UpdatedAt        string  `json:"updated_at"`
}

func newBalanceResponse(v balance.BalanceView) balanceResponse {
	d := v.AllowedDurations
	if d == nil {
		d = []int64{}
	}
	return balanceResponse{AvailableSeconds: v.AvailableSeconds, ReservedSeconds: v.ReservedSeconds, AllowedDurations: d, UpdatedAt: formatTime(v.UpdatedAt)}
}

// ledgerEntryUserResponse is the user-facing ledger item: nine fields, never the
// admin reason, admin id or idempotency key (spec 4.7).
type ledgerEntryUserResponse struct {
	ID                    string  `json:"id"`
	EntryType             string  `json:"entry_type"`
	AvailableSecondsDelta int64   `json:"available_seconds_delta"`
	ReservedSecondsDelta  int64   `json:"reserved_seconds_delta"`
	AvailableSecondsAfter int64   `json:"available_seconds_after"`
	ReservedSecondsAfter  int64   `json:"reserved_seconds_after"`
	SessionID             *string `json:"session_id"`
	Description           string  `json:"description"`
	CreatedAt             string  `json:"created_at"`
}

func uuidPtrString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func newLedgerEntryUserResponse(e balance.LedgerEntry) ledgerEntryUserResponse {
	return ledgerEntryUserResponse{
		ID: e.ID.String(), EntryType: string(e.EntryType), AvailableSecondsDelta: e.AvailableSecondsDelta,
		ReservedSecondsDelta: e.ReservedSecondsDelta, AvailableSecondsAfter: e.AvailableSecondsAfter,
		ReservedSecondsAfter: e.ReservedSecondsAfter, SessionID: uuidPtrString(e.SessionID),
		Description: balance.Description(e), CreatedAt: formatTime(e.CreatedAt),
	}
}

// parseEntryTypes reads ?entry_type=GRANT,CONSUME; unknown values are a 400.
func parseEntryTypes(raw string) ([]balance.EntryType, error) {
	if raw == "" {
		return nil, nil
	}
	var out []balance.EntryType
	for _, p := range strings.Split(raw, ",") {
		t := balance.EntryType(strings.TrimSpace(p))
		if !t.Valid() {
			return nil, FieldError("entry_type", "not_allowed")
		}
		out = append(out, t)
	}
	return out, nil
}

// parseUUIDCursor decodes a cursor whose key is a UUID string.
func parseUUIDCursor(raw string) (*uuid.UUID, error) {
	if raw == "" {
		return nil, nil
	}
	var key string
	if err := DecodeCursor(raw, &key); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(key)
	if err != nil {
		return nil, DecodeCursor("!", &key) // same INVALID_CURSOR error
	}
	return &id, nil
}

func nextCursor(next *uuid.UUID) *string {
	if next == nil {
		return nil
	}
	c := EncodeCursor(next.String())
	return &c
}
