package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type userRepo struct{ q *sqlcgen.Queries }

var _ auth.UserRepository = (*userRepo)(nil)

// Users returns the transaction's user repository.
func (t *Tx) Users() auth.UserRepository { return &userRepo{q: t.Queries()} }

func toUser(u sqlcgen.User) auth.User {
	return auth.User{
		ID: u.ID, Email: u.Email, PhoneNumber: fromText(u.PhoneNumber), FullName: u.FullName, PasswordHash: u.PasswordHash,
		Role: auth.Role(u.Role), Status: auth.UserStatus(u.Status), PasswordChangedAt: fromTsz(u.PasswordChangedAt),
		LastLoginAt: fromTszPtr(u.LastLoginAt), TermsVersion: u.TermsVersion, TermsAcceptedAt: fromTsz(u.TermsAcceptedAt),
		CreatedAt: fromTsz(u.CreatedAt), UpdatedAt: fromTsz(u.UpdatedAt), DeletedAt: fromTszPtr(u.DeletedAt), PurgedAt: fromTszPtr(u.PurgedAt),
	}
}

func (r *userRepo) Insert(ctx context.Context, u auth.User) error {
	err := r.q.InsertUser(ctx, sqlcgen.InsertUserParams{
		ID: u.ID, Email: u.Email, PhoneNumber: textPtr(u.PhoneNumber), FullName: u.FullName, PasswordHash: u.PasswordHash,
		Role: string(u.Role), Status: string(u.Status), PasswordChangedAt: tsz(u.PasswordChangedAt), LastLoginAt: tszPtr(u.LastLoginAt),
		TermsVersion: u.TermsVersion, TermsAcceptedAt: tsz(u.TermsAcceptedAt), CreatedAt: tsz(u.CreatedAt), UpdatedAt: tsz(u.UpdatedAt),
		DeletedAt: tszPtr(u.DeletedAt), PurgedAt: tszPtr(u.PurgedAt),
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == uniqueViolation {
		switch pe.ConstraintName {
		case "ux_users_email":
			return auth.ErrEmailTaken
		case "ux_users_phone":
			return auth.ErrPhoneTaken
		}
	}
	if err != nil {
		return internalErr(err, "insert user failed")
	}
	return nil
}

func userResult(u sqlcgen.User, err error, what string) (auth.User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, auth.ErrUserNotFound
	}
	if err != nil {
		return auth.User{}, internalErr(err, what)
	}
	return toUser(u), nil
}

func (r *userRepo) GetByID(ctx context.Context, id uuid.UUID) (auth.User, error) {
	u, err := r.q.GetUserByID(ctx, id)
	return userResult(u, err, "read user failed")
}

func (r *userRepo) GetByEmail(ctx context.Context, email string) (auth.User, error) {
	u, err := r.q.GetUserByEmail(ctx, email)
	return userResult(u, err, "read user by email failed")
}

func (r *userRepo) LockByID(ctx context.Context, id uuid.UUID) (auth.User, error) {
	u, err := r.q.LockUserByID(ctx, id)
	return userResult(u, err, "lock user failed")
}

func (r *userRepo) UpdateLastLogin(ctx context.Context, id uuid.UUID, at time.Time) error {
	if err := r.q.UpdateUserLastLogin(ctx, sqlcgen.UpdateUserLastLoginParams{At: tsz(at), ID: id}); err != nil {
		return internalErr(err, "update last login failed")
	}
	return nil
}

func (r *userRepo) SetPasswordHash(ctx context.Context, id uuid.UUID, hash string, changedAt *time.Time) error {
	err := r.q.SetUserPasswordHash(ctx, sqlcgen.SetUserPasswordHashParams{PasswordHash: hash, ChangedAt: tszPtr(changedAt), ID: id})
	if err != nil {
		return internalErr(err, "set password hash failed")
	}
	return nil
}

func (r *userRepo) UpdateConsents(ctx context.Context, id uuid.UUID, termsVersion string, at time.Time) (auth.User, error) {
	u, err := r.q.UpdateUserConsents(ctx, sqlcgen.UpdateUserConsentsParams{TermsVersion: termsVersion, At: tsz(at), ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		// either no such user or not ACTIVE: tell them apart
		if _, gerr := r.q.GetUserByID(ctx, id); errors.Is(gerr, pgx.ErrNoRows) {
			return auth.User{}, auth.ErrUserNotFound
		}
		return auth.User{}, auth.ErrUserNotActive
	}
	return userResult(u, err, "update consents failed")
}

func (r *userRepo) UpdateStatus(ctx context.Context, id uuid.UUID, to auth.UserStatus, at time.Time) error {
	if err := r.q.UpdateUserStatus(ctx, sqlcgen.UpdateUserStatusParams{Status: string(to), At: tsz(at), ID: id}); err != nil {
		return internalErr(err, "update user status failed")
	}
	return nil
}

func (r *userRepo) List(ctx context.Context, f auth.UserFilter, after *uuid.UUID, limit int) ([]auth.User, error) {
	p := sqlcgen.ListUsersParams{EmailPrefix: f.EmailPrefix, After: nullUUID(after), RowLimit: int32(min(limit, 1<<30))} //nolint:gosec // bounded above
	if f.Status != nil {
		p.Status = pgtype.Text{String: string(*f.Status), Valid: true}
	}
	if f.Role != nil {
		p.Role = pgtype.Text{String: string(*f.Role), Valid: true}
	}
	rows, err := r.q.ListUsers(ctx, p)
	if err != nil {
		return nil, internalErr(err, "list users failed")
	}
	out := make([]auth.User, len(rows))
	for i, u := range rows {
		out[i] = toUser(u)
	}
	return out, nil
}
