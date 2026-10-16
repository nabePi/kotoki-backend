package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// CookieConfig describes the refresh-token cookie.
type CookieConfig struct {
	Secure bool
	Domain string
	MaxAge time.Duration
}

// Config holds the HTTP-layer settings.
type Config struct {
	CORSOrigins    []string
	TrustedProxies []netip.Prefix
	BodyLimit      int64
	Cookie         CookieConfig
}

// authService is what the auth handlers need; *auth.Service satisfies it.
type authService interface {
	Register(context.Context, auth.RegisterInput) (auth.AuthResult, error)
	Login(context.Context, auth.LoginInput) (auth.AuthResult, error)
	Refresh(context.Context, auth.RefreshInput) (auth.RefreshResult, error)
	Logout(context.Context, string) error
	ForgotPassword(context.Context, auth.ForgotInput) error
	ResetPassword(context.Context, auth.ResetInput) error
	TermsVersion() string
}

// userService is what the /me handlers need; *auth.UserService satisfies it.
type userService interface {
	Me(context.Context, uuid.UUID) (auth.User, error)
	UpdateConsents(context.Context, auth.ConsentsInput) (auth.User, error)
	DeleteAccount(ctx context.Context, userID uuid.UUID, password string) error
	CurrentTermsVersion() string
	// RequireActive is the ACTIVE check of the write endpoints (ACCOUNT_SUSPENDED otherwise).
	RequireActive(ctx context.Context, userID uuid.UUID) (auth.User, error)
}

// balanceService is what the user balance handlers need; *balance.Service satisfies it.
type balanceService interface {
	GetBalance(context.Context, uuid.UUID) (balance.BalanceView, error)
	ListTransactions(ctx context.Context, userID uuid.UUID, types []balance.EntryType, after *uuid.UUID, limit int) ([]balance.LedgerEntry, *uuid.UUID, error)
}

// adminService is what the admin handlers need; *admin.Service satisfies it.
type adminService interface {
	ListUsers(ctx context.Context, f auth.UserFilter, after *uuid.UUID, limit int) ([]admin.UserView, *uuid.UUID, error)
	GetUser(context.Context, uuid.UUID) (admin.UserDetail, error)
	ChangeStatus(context.Context, admin.ChangeStatusInput) (admin.UserView, error)
	Grant(context.Context, admin.GrantRequest) (balance.MovementResult, error)
	Adjust(context.Context, admin.AdjustRequest) (balance.MovementResult, error)
	ListTransactions(ctx context.Context, userID uuid.UUID, types []balance.EntryType, after *uuid.UUID, limit int) ([]balance.LedgerEntry, *uuid.UUID, error)
	ListAudit(ctx context.Context, f admin.AuditFilter, after *uuid.UUID, limit int) ([]admin.AuditLog, *uuid.UUID, error)
}

// contentCatalogService is what the public catalog handlers need; *content.CatalogService satisfies it.
type contentCatalogService interface {
	Topics(context.Context) ([]content.TopicView, error)
	Scenarios(ctx context.Context, topicID *uuid.UUID) ([]content.ScenarioView, error)
	Scenario(context.Context, uuid.UUID) (content.ScenarioView, error)
	Characters(context.Context) ([]content.CharacterView, error)
	Character(context.Context, uuid.UUID) (content.CharacterView, error)
}

// contentAdminService is what the admin content handlers need; *content.AdminService satisfies it.
type contentAdminService interface {
	ListVersions(ctx context.Context, fam content.Family, parent string) ([]content.VersionView, error)
	GetVersion(ctx context.Context, fam content.Family, parent string, id uuid.UUID) (content.VersionView, error)
	CreateDraft(ctx context.Context, actor uuid.UUID, fam content.Family, parent string, body json.RawMessage, copyFrom *uuid.UUID) (content.VersionView, error)
	Publish(ctx context.Context, actor uuid.UUID, fam content.Family, parent string, id uuid.UUID) (content.PublishResult, error)
	Retire(ctx context.Context, actor uuid.UUID, fam content.Family, parent string, id uuid.UUID) (content.RetireResult, error)
	DeleteDraft(ctx context.Context, actor uuid.UUID, fam content.Family, parent string, id uuid.UUID) error
	SetStatus(ctx context.Context, actor uuid.UUID, entity string, id uuid.UUID, to content.EntityStatus) (content.EntityView, error)
	GetCharacter(context.Context, uuid.UUID) (content.Character, error)
	UpdateCharacter(ctx context.Context, actor, id uuid.UUID, p content.CharacterPatch) (content.CharacterUpdateResult, error)
	ValidationReport(context.Context) (errs, warns []content.Issue, err error)
}

// sessionService is what the session handlers write through; *session.Service satisfies it.
type sessionService interface {
	Create(ctx context.Context, in session.CreateSessionInput) (session.CreateSessionResult, error)
	Cancel(ctx context.Context, userID, sessionID uuid.UUID) (session.Session, error)
}

// sessionQueryService is the read side; *session.QueryService satisfies it.
type sessionQueryService interface {
	Get(ctx context.Context, userID, id uuid.UUID) (session.View, error)
	List(ctx context.Context, userID uuid.UUID, statuses []session.Status, after *uuid.UUID, limit int) ([]session.View, error)
	Turns(ctx context.Context, userID, id uuid.UUID, afterSeq *int, limit int) ([]session.TurnRecord, error)
}

// evaluationSessionGetter is the owner-scoped session read of the evaluation endpoint (a session of
// another user does not exist for the caller); *session.QueryService satisfies it.
type evaluationSessionGetter interface {
	Get(ctx context.Context, userID, id uuid.UUID) (session.View, error)
}

// evaluationQuery is the evaluation read side; *evaluation.QueryService satisfies it.
type evaluationQuery interface {
	Get(ctx context.Context, ref evaluation.SessionRef) (evaluation.View, error)
}

// adminSessionService is the admin read side of sessions; *session.AdminQueryService satisfies it.
type adminSessionService interface {
	ListByUser(ctx context.Context, userID uuid.UUID, statuses []session.Status, after *uuid.UUID, limit int) ([]session.AdminView, error)
	Get(ctx context.Context, id uuid.UUID) (session.AdminView, error)
	Turns(ctx context.Context, id uuid.UUID, afterSeq *int, limit int) ([]session.TurnRecord, error)
	Events(ctx context.Context, id uuid.UUID, after *session.EventCursor, limit int) ([]session.Event, error)
}

// transcriptAuditor writes the SESSION_TRANSCRIPT_VIEW audit row; *admin.Service satisfies it.
type transcriptAuditor interface {
	RecordTranscriptView(ctx context.Context, adminID, sessionID, ownerID uuid.UUID, meta admin.RequestMeta) error
}

// ticketService issues WebSocket tickets; *auth.TicketService satisfies it.
type ticketService interface {
	Issue(ctx context.Context, userID, sessionID uuid.UUID) (auth.TicketResult, error)
}

// profileService is the profile and skills side; *learner.Service satisfies it.
type profileService interface {
	GetProfile(ctx context.Context, userID uuid.UUID) (learner.ProfileView, error)
	PatchProfile(ctx context.Context, userID uuid.UUID, in learner.PatchInput) (learner.ProfileView, error)
	Skills(ctx context.Context, userID uuid.UUID) (learner.SkillsView, error)
	History(ctx context.Context, userID uuid.UUID, skill learner.Skill, after *learner.HistoryCursor, limit int) ([]learner.EstimateRow, *learner.HistoryCursor, error)
	ResetSkills(ctx context.Context, userID uuid.UUID) error
}

// memoryService is the memory side; *memory.Service satisfies it.
type memoryService interface {
	List(ctx context.Context, userID uuid.UUID, cat *memory.Category, after *memory.ListCursor, limit int) (memory.ListResult, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	DeleteAll(ctx context.Context, userID uuid.UUID) error
}

// Deps are the collaborators of NewRouter, wired by hand in cmd/api.
type Deps struct {
	Log       *slog.Logger
	Cfg       Config
	Limiter   ratelimit.Limiter
	Tokens    auth.TokenIssuer
	Clock     clock.Clock
	Metrics   *metrics.HTTP // optional
	Auth      authService
	Users     userService
	Balance   balanceService
	Admin     adminService
	AdminGate AdminGate

	Content      contentCatalogService
	ContentAdmin contentAdminService

	Session      sessionService
	SessionQuery sessionQueryService
	Tickets      ticketService

	// Evaluation serves GET /sessions/{id}/evaluation; the route is registered only when it is set.
	Evaluation evaluationQuery

	// Profile serves /profile, /profile/skills and its history; Memories serves /profile/memories.
	// Each group of routes is registered only when its service is set.
	Profile  profileService
	Memories memoryService

	AdminSessions adminSessionService
	AdminAudit    transcriptAuditor
}
