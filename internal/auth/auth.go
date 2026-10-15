package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Principal is the authenticated caller attached to a request context.
type Principal struct {
	UserID   uuid.UUID
	Role     Role
	FamilyID uuid.UUID
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the Principal stored by WithPrincipal.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// PasswordHasher hashes and verifies passwords (argon2id, PHC strings).
type PasswordHasher interface {
	Hash(ctx context.Context, plaintext string) (string, error)          // PHC string
	Verify(ctx context.Context, plaintext, encoded string) (bool, error) // constant-time
	NeedsRehash(encoded string) bool                                     // params changed → rehash on successful login
}

// TokenIssuer issues and parses access tokens.
type TokenIssuer interface {
	IssueAccess(p AccessSubject, now time.Time) (token string, expiresAt time.Time, err error)
	ParseAccess(token string, now time.Time) (Claims, error) // ErrTokenExpired, ErrTokenInvalid
}

// Mailer sends transactional mail.
type Mailer interface {
	Send(ctx context.Context, msg MailMessage) error
}

// MailMessage is one outgoing mail.
type MailMessage struct {
	To       string
	Subject  string
	TextBody string
	HTMLBody string // may be empty
	Tag      string // "password_reset" — for logs/metrics
}

// TicketStore issues and consumes one-time WebSocket tickets.
type TicketStore interface {
	Issue(ctx context.Context, t WSTicket, ttl time.Duration) (plaintext string, err error)
	Consume(ctx context.Context, plaintext string) (WSTicket, error) // ErrTicketInvalid if absent/expired/used
}

// WSTicket is the payload bound to a realtime ticket.
type WSTicket struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	IssuedAt  time.Time
}

// TicketSessionReader is the read-only query behind ticket issuance (spec 3.11
// step 3). auth does not import session/balance; status is the canonical string.
type TicketSessionReader interface {
	GetTicketTarget(ctx context.Context, sessionID, userID uuid.UUID) (TicketTarget, error) // ErrSessionNotFound
}

// TicketTarget is the session state a ticket request needs.
type TicketTarget struct {
	Status          string    // RESERVED | STARTING | ACTIVE | … (spec 5.4)
	StartDeadlineAt time.Time // balance_reservations.start_deadline_at
}

// AccountOpener opens the balance account during registration, inside the auth tx.
type AccountOpener interface {
	OpenAccount(ctx context.Context, userID uuid.UUID, initialSeconds int64, at time.Time) error
}

// AccountDeleter performs steps 2–4 of account deletion (spec 3.13.3) in one tx.
type AccountDeleter interface {
	DeleteAccount(ctx context.Context, userID uuid.UUID, at time.Time) error // ErrAccountHasActiveSession
}

// AccessSubject is the identity an access token is issued for.
type AccessSubject struct {
	UserID   uuid.UUID
	Role     Role
	FamilyID uuid.UUID
}

// Claims are the verified claims of an access token (spec 3.6).
type Claims struct {
	UserID                         uuid.UUID
	Role                           Role
	FamilyID, JTI                  uuid.UUID
	IssuedAt, NotBefore, ExpiresAt time.Time
	KeyID                          string
}
