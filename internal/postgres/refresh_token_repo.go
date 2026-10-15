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

type refreshRepo struct{ q *sqlcgen.Queries }

var _ auth.RefreshTokenRepository = (*refreshRepo)(nil)

// RefreshTokens returns the transaction's refresh token repository.
func (t *Tx) RefreshTokens() auth.RefreshTokenRepository { return &refreshRepo{q: t.Queries()} }

func toRefresh(t sqlcgen.RefreshToken) auth.RefreshToken {
	return auth.RefreshToken{
		ID: t.ID, UserID: t.UserID, FamilyID: t.FamilyID, ParentID: fromNullUUID(t.ParentID), TokenHash: t.TokenHash,
		UserAgent: fromText(t.UserAgent), IP: t.IpAddress, CreatedAt: fromTsz(t.CreatedAt), ExpiresAt: fromTsz(t.ExpiresAt),
		RevokedAt: fromTszPtr(t.RevokedAt), RevokeReason: fromText(t.RevokeReason), ReplacedByID: fromNullUUID(t.ReplacedByID),
	}
}

func (r *refreshRepo) Insert(ctx context.Context, t auth.RefreshToken) error {
	err := r.q.InsertRefreshToken(ctx, sqlcgen.InsertRefreshTokenParams{
		ID: t.ID, UserID: t.UserID, FamilyID: t.FamilyID, ParentID: nullUUID(t.ParentID), TokenHash: t.TokenHash,
		UserAgent: textPtr(t.UserAgent), IpAddress: t.IP, CreatedAt: tsz(t.CreatedAt), ExpiresAt: tsz(t.ExpiresAt),
	})
	if err != nil {
		return internalErr(err, "insert refresh token failed")
	}
	return nil
}

func refreshResult(t sqlcgen.RefreshToken, err error, what string) (auth.RefreshToken, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.RefreshToken{}, auth.ErrRefreshTokenNotFound
	}
	if err != nil {
		return auth.RefreshToken{}, internalErr(err, what)
	}
	return toRefresh(t), nil
}

func (r *refreshRepo) GetByID(ctx context.Context, id uuid.UUID) (auth.RefreshToken, error) {
	t, err := r.q.GetRefreshTokenByID(ctx, id)
	return refreshResult(t, err, "read refresh token failed")
}

func (r *refreshRepo) LockByHash(ctx context.Context, hash []byte) (auth.RefreshToken, error) {
	t, err := r.q.LockRefreshTokenByHash(ctx, hash)
	return refreshResult(t, err, "lock refresh token failed")
}

func (r *refreshRepo) MarkRotated(ctx context.Context, id, replacedBy uuid.UUID, at time.Time) error {
	if err := r.q.MarkRefreshTokenRotated(ctx, sqlcgen.MarkRefreshTokenRotatedParams{At: tsz(at), ReplacedBy: replacedBy, ID: id}); err != nil {
		return internalErr(err, "mark refresh token rotated failed")
	}
	return nil
}

func (r *refreshRepo) RevokeFamily(ctx context.Context, familyID uuid.UUID, reason string, at time.Time) (int64, error) {
	n, err := r.q.RevokeRefreshFamily(ctx, sqlcgen.RevokeRefreshFamilyParams{At: tsz(at), Reason: reason, FamilyID: familyID})
	if err != nil {
		return 0, internalErr(err, "revoke refresh family failed")
	}
	return n, nil
}

func (r *refreshRepo) RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string, at time.Time) (int64, error) {
	n, err := r.q.RevokeAllRefreshForUser(ctx, sqlcgen.RevokeAllRefreshForUserParams{At: tsz(at), Reason: reason, UserID: userID})
	if err != nil {
		return 0, internalErr(err, "revoke refresh tokens failed")
	}
	return n, nil
}

func (r *refreshRepo) DeleteAllForUser(ctx context.Context, userID uuid.UUID) error {
	if err := r.q.DeleteAllRefreshForUser(ctx, userID); err != nil {
		return internalErr(err, "delete refresh tokens failed")
	}
	return nil
}

func (r *refreshRepo) CountActive(ctx context.Context, userID uuid.UUID, now time.Time) (int, error) {
	n, err := r.q.CountActiveRefresh(ctx, sqlcgen.CountActiveRefreshParams{UserID: userID, Now: tsz(now)})
	if err != nil {
		return 0, internalErr(err, "count refresh tokens failed")
	}
	return int(n), nil
}
