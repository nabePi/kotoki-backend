package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// TicketConfig configures realtime ticket issuance. RealtimeURL is REALTIME_PUBLIC_URL
// (without path); the service appends /api/v1/realtime.
type TicketConfig struct {
	TTL         time.Duration
	RealtimeURL string
}

// TicketResult is the POST /sessions/{id}/realtime-ticket payload.
type TicketResult struct {
	Ticket      string
	ExpiresIn   int
	RealtimeURL string
}

// TicketService issues one-time WebSocket tickets (spec 3.11, D-022).
type TicketService struct {
	store   Store
	reader  TicketSessionReader
	tickets TicketStore
	clk     clock.Clock
	cfg     TicketConfig
}

// NewTicketService returns a TicketService.
func NewTicketService(store Store, reader TicketSessionReader, tickets TicketStore, clk clock.Clock, cfg TicketConfig) *TicketService {
	return &TicketService{store: store, reader: reader, tickets: tickets, clk: clk, cfg: cfg}
}

// Issue checks the account and the session state, then stores a ticket bound to both.
// Stale terms do not block a ticket: only creating a session requires current terms.
func (s *TicketService) Issue(ctx context.Context, userID, sessionID uuid.UUID) (TicketResult, error) {
	var user User
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		user, err = tx.Users().GetByID(ctx, userID)
		return err
	})
	if errors.Is(err, ErrUserNotFound) {
		return TicketResult{}, errs.New(errs.CodeUserNotFound, "user not found")
	}
	if err != nil {
		return TicketResult{}, err
	}
	if user.Status != UserStatusActive {
		return TicketResult{}, errs.New(errs.CodeAccountSuspended, "account suspended")
	}

	target, err := s.reader.GetTicketTarget(ctx, sessionID, userID)
	if errors.Is(err, ErrSessionNotFound) {
		return TicketResult{}, errs.New(errs.CodeSessionNotFound, "session not found")
	}
	if err != nil {
		return TicketResult{}, err
	}
	now := s.clk.Now()
	switch target.Status {
	case "RESERVED":
		if !now.Before(target.StartDeadlineAt) {
			return TicketResult{}, errs.New(errs.CodeSessionStartDeadlinePassed, "session start deadline passed")
		}
	case "STARTING", "ACTIVE":
	default:
		return TicketResult{}, errs.WithDetails(errs.New(errs.CodeSessionNotConnectable, "session is not connectable"),
			map[string]any{"status": target.Status})
	}

	ticket, err := s.tickets.Issue(ctx, WSTicket{UserID: userID, SessionID: sessionID, IssuedAt: now}, s.cfg.TTL)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeServiceUnavailable {
			return TicketResult{}, err
		}
		return TicketResult{}, errs.Wrap(err, errs.CodeServiceUnavailable, "ticket store unavailable")
	}
	return TicketResult{
		Ticket:      ticket,
		ExpiresIn:   int(s.cfg.TTL / time.Second),
		RealtimeURL: strings.TrimRight(s.cfg.RealtimeURL, "/") + "/api/v1/realtime",
	}, nil
}
