package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type sessionAdminRepo struct{ q *sqlcgen.Queries }

var _ session.AdminRepository = (*sessionAdminRepo)(nil)

// NewSessionAdminRepo returns the admin read side of sessions over the pool.
func NewSessionAdminRepo(pool *pgxpool.Pool) session.AdminRepository {
	return &sessionAdminRepo{q: sqlcgen.New(pool)}
}

// adminRow is the common shape of the single and the list query.
type adminRow struct {
	s                                           sqlcgen.Session
	topic, scenario, character, avatar          string
	resID, resAccount                           uuid.NullUUID
	resStatus                                   pgtype.Text
	resReserved, resConsumed, resReleasedSecs   pgtype.Int8
	resDeadline, resCreated, resSettled, resRel pgtype.Timestamptz
}

func int64Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func toAdminView(r adminRow) session.AdminView {
	v := session.AdminView{View: toView(r.s, r.topic, r.scenario, r.character, r.avatar, r.resDeadline)}
	if r.resID.Valid {
		v.Reservation = &balance.Reservation{
			ID: r.resID.UUID, UserID: r.s.UserID, AccountID: r.resAccount.UUID, SessionID: r.s.ID, ReservedSeconds: r.resReserved.Int64,
			ConsumedSeconds: int64Ptr(r.resConsumed), ReleasedSeconds: int64Ptr(r.resReleasedSecs), Status: balance.ReservationStatus(r.resStatus.String),
			StartDeadlineAt: fromTsz(r.resDeadline), CreatedAt: fromTsz(r.resCreated), SettledAt: fromTszPtr(r.resSettled), ReleasedAt: fromTszPtr(r.resRel),
		}
	}
	return v
}

func (r *sessionAdminRepo) GetAdminView(ctx context.Context, id uuid.UUID) (session.AdminView, error) {
	row, err := r.q.GetSessionAdminView(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.AdminView{}, session.ErrNotFound
	}
	if err != nil {
		return session.AdminView{}, internalErr(err, "read session failed")
	}
	return toAdminView(adminRow{row.Session, row.TopicName, row.ScenarioName, row.CharacterName, row.AvatarKey, row.ResID, row.ResAccountID, row.ResStatus,
		row.ResReservedSeconds, row.ResConsumedSeconds, row.ResReleasedSeconds, row.ResStartDeadlineAt, row.ResCreatedAt, row.ResSettledAt, row.ResReleasedAt}), nil
}

func (r *sessionAdminRepo) ListAdminViews(ctx context.Context, userID uuid.UUID, statuses []session.Status, after *uuid.UUID, limit int) ([]session.AdminView, error) {
	var list []string
	for _, st := range statuses {
		list = append(list, string(st))
	}
	rows, err := r.q.ListSessionAdminViews(ctx, sqlcgen.ListSessionAdminViewsParams{UserID: userID, Statuses: list, After: nullUUID(after), RowLimit: limit32(limit)})
	if err != nil {
		return nil, internalErr(err, "list sessions failed")
	}
	out := make([]session.AdminView, len(rows))
	for i, row := range rows {
		out[i] = toAdminView(adminRow{row.Session, row.TopicName, row.ScenarioName, row.CharacterName, row.AvatarKey, row.ResID, row.ResAccountID, row.ResStatus,
			row.ResReservedSeconds, row.ResConsumedSeconds, row.ResReleasedSeconds, row.ResStartDeadlineAt, row.ResCreatedAt, row.ResSettledAt, row.ResReleasedAt})
	}
	return out, nil
}

func (r *sessionAdminRepo) ListAllTurns(ctx context.Context, sessionID uuid.UUID, startingAt time.Time, endedAt *time.Time, afterSeq *int, limit int) ([]session.TurnRecord, error) {
	arg := sqlcgen.ListAllTurnsForAdminParams{SessionID: sessionID, Since: tsz(startingAt.Add(-partitionSlack)), AfterSeq: optInt4(afterSeq), RowLimit: limit32(limit)}
	if endedAt != nil {
		arg.Until = tsz(endedAt.Add(partitionSlack))
	}
	rows, err := r.q.ListAllTurnsForAdmin(ctx, arg)
	if err != nil {
		return nil, internalErr(err, "list turns failed")
	}
	return turnList(rows), nil
}

func (r *sessionAdminRepo) ListEvents(ctx context.Context, sessionID uuid.UUID, after *session.EventCursor, limit int) ([]session.Event, error) {
	arg := sqlcgen.ListSessionEventsPageParams{SessionID: sessionID, RowLimit: limit32(limit)}
	if after != nil {
		arg.AfterAt, arg.AfterID = tsz(after.CreatedAt), uuid.NullUUID{UUID: after.ID, Valid: true}
	}
	rows, err := r.q.ListSessionEventsPage(ctx, arg)
	if err != nil {
		return nil, internalErr(err, "list session events failed")
	}
	out := make([]session.Event, len(rows))
	for i, e := range rows {
		ev := session.Event{ID: e.ID, SessionID: e.SessionID, Type: session.EventType(e.EventType), Actor: session.Actor(e.Actor), TurnID: fromNullUUID(e.TurnID),
			Payload: e.Payload, CreatedAt: fromTsz(e.CreatedAt)}
		if e.FromStatus.Valid {
			st := session.Status(e.FromStatus.String)
			ev.FromStatus = &st
		}
		if e.ToStatus.Valid {
			st := session.Status(e.ToStatus.String)
			ev.ToStatus = &st
		}
		out[i] = ev
	}
	return out, nil
}
