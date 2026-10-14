package redisstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

const ticketKeyPrefix = "kotoki:ws_ticket:"

// TicketStore implements auth.TicketStore on Redis: the key is the SHA-256 of
// the ticket, so a Redis dump never contains a usable ticket.
type TicketStore struct {
	rdb redis.UniversalClient
	clk clock.Clock
}

var _ auth.TicketStore = (*TicketStore)(nil)

// NewTicketStore returns a TicketStore.
func NewTicketStore(rdb redis.UniversalClient, clk clock.Clock) *TicketStore {
	return &TicketStore{rdb: rdb, clk: clk}
}

type ticketBody struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	IssuedAt  string `json:"issued_at"`
}

func ticketKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return ticketKeyPrefix + hex.EncodeToString(sum[:])
}

func unavailable(err error) error {
	if errs.CodeOf(err) == errs.CodeServiceUnavailable {
		return err
	}
	return errs.Wrap(err, errs.CodeServiceUnavailable, "ticket store unavailable")
}

// Issue stores t for ttl under a fresh random ticket (SET NX; one retry on a hash collision).
func (s *TicketStore) Issue(ctx context.Context, t auth.WSTicket, ttl time.Duration) (string, error) {
	body, err := json.Marshal(ticketBody{
		UserID: t.UserID.String(), SessionID: t.SessionID.String(), IssuedAt: t.IssuedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternalError, "ticket encoding failed")
	}
	for attempt := 0; attempt < 2; attempt++ {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", errs.Wrap(err, errs.CodeInternalError, "ticket generation failed")
		}
		ticket := base64.RawURLEncoding.EncodeToString(b)
		ok, err := s.rdb.SetNX(ctx, ticketKey(ticket), body, ttl).Result()
		if err != nil {
			return "", unavailable(err)
		}
		if ok {
			return ticket, nil
		}
	}
	return "", errs.New(errs.CodeInternalError, "ticket collision")
}

// Consume atomically reads and deletes the ticket (GETDEL).
func (s *TicketStore) Consume(ctx context.Context, plaintext string) (auth.WSTicket, error) {
	val, err := s.rdb.GetDel(ctx, ticketKey(plaintext)).Result()
	if errors.Is(err, redis.Nil) {
		return auth.WSTicket{}, auth.ErrTicketInvalid
	}
	if err != nil {
		return auth.WSTicket{}, unavailable(err)
	}
	var b ticketBody
	if err := json.Unmarshal([]byte(val), &b); err != nil {
		return auth.WSTicket{}, auth.ErrTicketInvalid
	}
	uid, err1 := uuid.Parse(b.UserID)
	sid, err2 := uuid.Parse(b.SessionID)
	issued, err3 := time.Parse(time.RFC3339Nano, b.IssuedAt)
	if err1 != nil || err2 != nil || err3 != nil {
		return auth.WSTicket{}, auth.ErrTicketInvalid
	}
	return auth.WSTicket{UserID: uid, SessionID: sid, IssuedAt: issued}, nil
}
