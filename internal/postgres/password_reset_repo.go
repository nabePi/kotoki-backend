package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type resetRepo struct{ q *sqlcgen.Queries }

var _ auth.PasswordResetRepository = (*resetRepo)(nil)

// PasswordResets returns the transaction's password reset repository.
func (t *Tx) PasswordResets() auth.PasswordResetRepository { return &resetRepo{q: t.Queries()} }

func (r *resetRepo) InvalidateOpenForUser(ctx context.Context, userID uuid.UUID, at time.Time) error {
	if err := r.q.InvalidateOpenResets(ctx, sqlcgen.InvalidateOpenResetsParams{At: tsz(at), UserID: userID}); err != nil {
		return internalErr(err, "invalidate reset tokens failed")
	}
	return nil
}

func (r *resetRepo) Insert(ctx context.Context, t auth.PasswordResetToken) error {
	err := r.q.InsertPasswordReset(ctx, sqlcgen.InsertPasswordResetParams{
		ID: t.ID, UserID: t.UserID, TokenHash: t.TokenHash, RequestedIp: t.RequestedIP, CreatedAt: tsz(t.CreatedAt), ExpiresAt: tsz(t.ExpiresAt),
	})
	if err != nil {
		return internalErr(err, "insert reset token failed")
	}
	return nil
}

func (r *resetRepo) FindByHash(ctx context.Context, hash []byte) (auth.ResetLookup, error) {
	row, err := r.q.FindPasswordResetByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ResetLookup{}, auth.ErrResetTokenNotFound
	}
	if err != nil {
		return auth.ResetLookup{}, internalErr(err, "find reset token failed")
	}
	return auth.ResetLookup{
		Token: auth.PasswordResetToken{ID: row.ID, UserID: row.UserID, TokenHash: row.TokenHash, RequestedIP: row.RequestedIp,
			CreatedAt: fromTsz(row.CreatedAt), ExpiresAt: fromTsz(row.ExpiresAt), UsedAt: fromTszPtr(row.UsedAt), InvalidatedAt: fromTszPtr(row.InvalidatedAt)},
		Email: row.OwnerEmail, UserStatus: auth.UserStatus(row.OwnerStatus),
	}, nil
}

func (r *resetRepo) LockByHash(ctx context.Context, hash []byte) (auth.ResetLookup, error) {
	row, err := r.q.LockPasswordResetByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ResetLookup{}, auth.ErrResetTokenNotFound
	}
	if err != nil {
		return auth.ResetLookup{}, internalErr(err, "lock reset token failed")
	}
	return auth.ResetLookup{
		Token: auth.PasswordResetToken{ID: row.ID, UserID: row.UserID, TokenHash: row.TokenHash, RequestedIP: row.RequestedIp,
			CreatedAt: fromTsz(row.CreatedAt), ExpiresAt: fromTsz(row.ExpiresAt), UsedAt: fromTszPtr(row.UsedAt), InvalidatedAt: fromTszPtr(row.InvalidatedAt)},
		Email: row.OwnerEmail, UserStatus: auth.UserStatus(row.OwnerStatus),
	}, nil
}

func (r *resetRepo) MarkUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	if err := r.q.MarkResetUsed(ctx, sqlcgen.MarkResetUsedParams{At: tsz(at), ID: id}); err != nil {
		return internalErr(err, "mark reset token used failed")
	}
	return nil
}

func (r *resetRepo) InvalidateOthers(ctx context.Context, userID, exceptID uuid.UUID, at time.Time) error {
	if err := r.q.InvalidateOtherResets(ctx, sqlcgen.InvalidateOtherResetsParams{At: tsz(at), UserID: userID, ExceptID: exceptID}); err != nil {
		return internalErr(err, "invalidate other reset tokens failed")
	}
	return nil
}

func (r *resetRepo) DeleteAllForUser(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.DeleteAllResetsForUser(ctx, userID); err != nil {
		return internalErr(err, "delete reset tokens failed")
	}
	return nil
}
