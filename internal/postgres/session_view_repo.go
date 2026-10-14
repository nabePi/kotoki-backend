package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type sessionViewRepo struct{ q *sqlcgen.Queries }

var _ session.ViewRepository = (*sessionViewRepo)(nil)

// NewSessionViewRepo returns the REST read side of sessions over the pool.
func NewSessionViewRepo(pool *pgxpool.Pool) session.ViewRepository {
	return &sessionViewRepo{q: sqlcgen.New(pool)}
}

func toView(s sqlcgen.Session, topic, scenario, character, avatar string, deadline pgtype.Timestamptz) session.View {
	v := session.View{Session: toSession(s), TopicName: topic, ScenarioName: scenario, CharacterName: character, StartDeadlineAt: fromTsz(deadline)}
	if avatar != "" {
		a := avatar
		v.AvatarKey = &a
	}
	return v
}

func (r *sessionViewRepo) GetView(ctx context.Context, userID, id uuid.UUID) (session.View, error) {
	row, err := r.q.GetSessionView(ctx, sqlcgen.GetSessionViewParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return session.View{}, session.ErrNotFound
	}
	if err != nil {
		return session.View{}, internalErr(err, "read session view failed")
	}
	return toView(row.Session, row.TopicName, row.ScenarioName, row.CharacterName, row.AvatarKey, row.StartDeadlineAt), nil
}

func (r *sessionViewRepo) ListViews(ctx context.Context, userID uuid.UUID, statuses []session.Status, after *uuid.UUID, limit int) ([]session.View, error) {
	var list []string
	for _, st := range statuses {
		list = append(list, string(st))
	}
	rows, err := r.q.ListSessionViews(ctx, sqlcgen.ListSessionViewsParams{UserID: userID, Statuses: list, After: nullUUID(after), RowLimit: limit32(limit)})
	if err != nil {
		return nil, internalErr(err, "list session views failed")
	}
	out := make([]session.View, len(rows))
	for i, row := range rows {
		out[i] = toView(row.Session, row.TopicName, row.ScenarioName, row.CharacterName, row.AvatarKey, row.StartDeadlineAt)
	}
	return out, nil
}
