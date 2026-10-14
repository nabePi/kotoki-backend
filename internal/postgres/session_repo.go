package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type sessionRepo struct{ q *sqlcgen.Queries }

var _ session.Repository = (*sessionRepo)(nil)

// NewSessionRepo returns a session.Repository over the pool for reads outside a transaction (REST
// queries, sweeper candidates); writes go through (*Tx).Sessions().
func NewSessionRepo(pool *pgxpool.Pool) session.Repository { return &sessionRepo{q: sqlcgen.New(pool)} }

// Sessions returns the transaction's session repository.
func (t *Tx) Sessions() session.Repository { return &sessionRepo{q: t.Queries()} }

// unique-index names of migration 000009 (translated to domain errors on 23505).
const (
	idxOneOpenPerUser    = "ux_sessions_one_open_per_user"
	idxUserIdempotency   = "ux_sessions_user_idempotency"
	pgUniqueViolationSQL = "23505"
)

func sessErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return session.ErrNotFound
	}
	return internalErr(err, what+" failed")
}

func toSession(r sqlcgen.Session) session.Session {
	s := session.Session{
		ID: r.ID, UserID: r.UserID, Status: session.Status(r.Status), ConversationStartMode: session.StartMode(r.ConversationStartMode),
		DurationSeconds: int(r.DurationSeconds), TopicID: r.TopicID, ScenarioID: r.ScenarioID, ScenarioVersionID: r.ScenarioVersionID,
		CharacterID: r.CharacterID, PromptVersionID: r.PromptVersionID, OpeningPromptVersionID: fromNullUUID(r.OpeningPromptVersionID),
		ConfigSnapshot: json.RawMessage(r.ConfigSnapshot), AdaptiveDifficulty: json.RawMessage(r.AdaptiveDifficulty),
		RuntimeFence: r.RuntimeFence, ReservedSeconds: int(r.ReservedSeconds), ChargedSeconds: int(r.ChargedSeconds),
		DisconnectedSeconds: int(r.DisconnectedSeconds), FailureDetail: fromText(r.FailureDetail), SummaryText: fromText(r.SummaryText),
		TranscriptPurgedAt: fromTszPtr(r.TranscriptPurgedAt), CreateIdempotencyKey: r.CreateIdempotencyKey, CreateRequestHash: r.CreateRequestHash,
		CreatedAt: fromTsz(r.CreatedAt), StartingAt: fromTszPtr(r.StartingAt), StartedAt: fromTszPtr(r.StartedAt), ExpiresAt: fromTszPtr(r.ExpiresAt),
		EndingAt: fromTszPtr(r.EndingAt), EndedAt: fromTszPtr(r.EndedAt), HandoffAt: fromTszPtr(r.HandoffAt),
		UpdatedAt: fromTsz(r.UpdatedAt), Version: r.Version,
	}
	if r.EndReason.Valid {
		er := session.EndReason(r.EndReason.String)
		s.EndReason = &er
	}
	return s
}

func sessionList(rows []sqlcgen.Session) []session.Session {
	out := make([]session.Session, len(rows))
	for i, r := range rows {
		out[i] = toSession(r)
	}
	return out
}

// Insert stores a RESERVED session; the two unique indexes become domain errors.
func (r *sessionRepo) Insert(ctx context.Context, s session.Session) error {
	err := r.q.InsertSession(ctx, sqlcgen.InsertSessionParams{
		ID: s.ID, UserID: s.UserID, ConversationStartMode: string(s.ConversationStartMode), DurationSeconds: int32(s.DurationSeconds), //nolint:gosec // validated duration
		TopicID: s.TopicID, ScenarioID: s.ScenarioID, ScenarioVersionID: s.ScenarioVersionID, CharacterID: s.CharacterID,
		PromptVersionID: s.PromptVersionID, ConfigSnapshot: []byte(s.ConfigSnapshot), AdaptiveDifficulty: []byte(s.AdaptiveDifficulty),
		ReservedSeconds: int32(s.ReservedSeconds), CreateIdempotencyKey: s.CreateIdempotencyKey, CreateRequestHash: s.CreateRequestHash, //nolint:gosec // validated duration
		CreatedAt: tsz(s.CreatedAt),
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == pgUniqueViolationSQL {
		switch pe.ConstraintName {
		case idxOneOpenPerUser:
			return session.ErrAlreadyOpen
		case idxUserIdempotency:
			return session.ErrDuplicateIdempotency
		}
	}
	if err != nil {
		return internalErr(err, "insert session failed")
	}
	return nil
}

func (r *sessionRepo) GetByID(ctx context.Context, id uuid.UUID) (session.Session, error) {
	row, err := r.q.GetSessionByID(ctx, id)
	if err != nil {
		return session.Session{}, sessErr(err, "read session")
	}
	return toSession(row), nil
}

func (r *sessionRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (session.Session, error) {
	row, err := r.q.GetSessionForUpdate(ctx, id)
	if err != nil {
		return session.Session{}, sessErr(err, "lock session")
	}
	return toSession(row), nil
}

func (r *sessionRepo) OwnerOf(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	u, err := r.q.GetSessionOwner(ctx, id)
	if err != nil {
		return uuid.Nil, sessErr(err, "read session owner")
	}
	return u, nil
}

func (r *sessionRepo) FindByIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) (session.Session, error) {
	row, err := r.q.FindSessionByIdempotencyKey(ctx, sqlcgen.FindSessionByIdempotencyKeyParams{UserID: userID, CreateIdempotencyKey: key})
	if err != nil {
		return session.Session{}, sessErr(err, "find session by idempotency key")
	}
	return toSession(row), nil
}

func (r *sessionRepo) FindOpenByUser(ctx context.Context, userID uuid.UUID) (session.Session, error) {
	row, err := r.q.FindOpenSessionByUser(ctx, userID)
	if err != nil {
		return session.Session{}, sessErr(err, "find open session")
	}
	return toSession(row), nil
}

func optText[T ~string](v *T) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: string(*v), Valid: true}
}

func optInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true} //nolint:gosec // small counters
}

// CAS applies the patch when status (and, for the runtime, the fence) still match; false means no
// row matched and the caller re-reads to tell an idempotent repeat from a conflict.
func (r *sessionRepo) CAS(ctx context.Context, id uuid.UUID, from session.Status, fence session.Fence, p session.Patch) (session.Session, bool, error) {
	arg := sqlcgen.CASSessionParams{
		ToStatus: string(p.To), EndReason: optText(p.EndReason), FailureDetail: optText(p.FailureDetail),
		StartingAt: tszPtr(p.StartingAt), StartedAt: tszPtr(p.StartedAt), ExpiresAt: tszPtr(p.ExpiresAt), EndingAt: tszPtr(p.EndingAt),
		EndedAt: tszPtr(p.EndedAt), ChargedSeconds: optInt4(p.ChargedSeconds), OpeningPromptVersionID: nullUUID(p.OpeningPromptVersionID),
		BumpFence: p.BumpFence, At: tsz(p.At), ID: id, FromStatus: string(from),
	}
	if fence != nil {
		arg.Fence = pgtype.Int8{Int64: *fence, Valid: true}
	}
	row, err := r.q.CASSession(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, false, nil
	}
	if err != nil {
		return session.Session{}, false, internalErr(err, "update session failed")
	}
	return toSession(row), true, nil
}

func (r *sessionRepo) Claim(ctx context.Context, id uuid.UUID, allowed []session.Status) (session.Session, error) {
	list := make([]string, len(allowed))
	for i, st := range allowed {
		list[i] = string(st)
	}
	row, err := r.q.ClaimSession(ctx, sqlcgen.ClaimSessionParams{ID: id, Allowed: list})
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, session.ErrInvalidTransition
	}
	if err != nil {
		return session.Session{}, internalErr(err, "claim session failed")
	}
	return toSession(row), nil
}

func (r *sessionRepo) MarkHandoff(ctx context.Context, id uuid.UUID, fence int64, at time.Time) (bool, error) {
	n, err := r.q.MarkSessionHandoff(ctx, sqlcgen.MarkSessionHandoffParams{At: tsz(at), ID: id, Fence: fence})
	if err != nil {
		return false, internalErr(err, "mark handoff failed")
	}
	return n == 1, nil
}

func (r *sessionRepo) AddDisconnected(ctx context.Context, id uuid.UUID, fence int64, n int, at time.Time) (session.Session, bool, error) {
	row, err := r.q.AddSessionDisconnected(ctx, sqlcgen.AddSessionDisconnectedParams{N: int32(n), At: tsz(at), ID: id, Fence: fence}) //nolint:gosec // seconds of one episode
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, false, nil
	}
	if err != nil {
		return session.Session{}, false, internalErr(err, "add disconnected seconds failed")
	}
	return toSession(row), true, nil
}

func (r *sessionRepo) ListByUser(ctx context.Context, userID uuid.UUID, f session.ListFilter, after *uuid.UUID, limit int) ([]session.Session, error) {
	var statuses []string
	for _, st := range f.Statuses {
		statuses = append(statuses, string(st))
	}
	rows, err := r.q.ListSessionsByUser(ctx, sqlcgen.ListSessionsByUserParams{UserID: userID, Statuses: statuses, After: nullUUID(after), RowLimit: int32(min(limit, 1<<30))}) //nolint:gosec // bounded above
	if err != nil {
		return nil, internalErr(err, "list sessions failed")
	}
	return sessionList(rows), nil
}

func (r *sessionRepo) GetRuntimeRecord(ctx context.Context, id uuid.UUID) (session.RuntimeRecord, error) {
	s, err := r.GetByID(ctx, id)
	if err != nil {
		return session.RuntimeRecord{}, err
	}
	return session.RuntimeRecordOf(s), nil
}

func (r *sessionRepo) SetSummary(ctx context.Context, id uuid.UUID, text string, at time.Time) error {
	n, err := r.q.SetSessionSummary(ctx, sqlcgen.SetSessionSummaryParams{Text: pgtype.Text{String: text, Valid: true}, At: tsz(at), ID: id})
	if err != nil {
		return internalErr(err, "set summary failed")
	}
	if n != 1 {
		return session.ErrNotFound
	}
	return nil
}

func limit32(n int) int32 { return int32(min(n, 1<<30)) } //nolint:gosec // bounded above

func (r *sessionRepo) ListExpired(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	ids, err := r.q.ListExpiredSessions(ctx, sqlcgen.ListExpiredSessionsParams{ExpiresAt: tsz(before), Limit: limit32(limit)})
	return ids, wrapList(err, "list expired sessions")
}

func (r *sessionRepo) ListStuckEnding(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	ids, err := r.q.ListStuckEndingSessions(ctx, sqlcgen.ListStuckEndingSessionsParams{EndingAt: tsz(before), Limit: limit32(limit)})
	return ids, wrapList(err, "list stuck ending sessions")
}

func (r *sessionRepo) ListStuckStarting(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	ids, err := r.q.ListStuckStartingSessions(ctx, sqlcgen.ListStuckStartingSessionsParams{StartingAt: tsz(before), Limit: limit32(limit)})
	return ids, wrapList(err, "list stuck starting sessions")
}

func (r *sessionRepo) ListHandoffStale(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	ids, err := r.q.ListHandoffStaleSessions(ctx, sqlcgen.ListHandoffStaleSessionsParams{HandoffAt: tsz(before), Limit: limit32(limit)})
	return ids, wrapList(err, "list stale handoff sessions")
}

func (r *sessionRepo) ListActiveForHeartbeat(ctx context.Context, startedBefore time.Time, limit int) ([]session.ActiveRef, error) {
	rows, err := r.q.ListActiveSessionsForHeartbeat(ctx, sqlcgen.ListActiveSessionsForHeartbeatParams{StartedAt: tsz(startedBefore), Limit: limit32(limit)})
	if err != nil {
		return nil, wrapList(err, "list active sessions")
	}
	out := make([]session.ActiveRef, len(rows))
	for i, row := range rows {
		out[i] = session.ActiveRef{ID: row.ID, StartedAt: fromTsz(row.StartedAt), HandoffAt: fromTszPtr(row.HandoffAt)}
	}
	return out, nil
}

func (r *sessionRepo) CountActiveNonHandoff(ctx context.Context) (int, error) {
	n, err := r.q.CountActiveNonHandoffSessions(ctx)
	return int(n), wrapList(err, "count active sessions")
}

func (r *sessionRepo) ListAbandonCandidates(ctx context.Context, deadlineBefore time.Time, limit int) ([]uuid.UUID, error) {
	ids, err := r.q.ListAbandonCandidates(ctx, sqlcgen.ListAbandonCandidatesParams{StartDeadlineAt: tsz(deadlineBefore), Limit: limit32(limit)})
	return ids, wrapList(err, "list abandon candidates")
}

func wrapList(err error, what string) error {
	if err == nil {
		return nil
	}
	return internalErr(err, what+" failed")
}
