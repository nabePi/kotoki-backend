package admin

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
)

// UserView is a user plus the balance account shown in admin lists.
type UserView struct {
	User    auth.User
	Balance balance.Account
}

// UserDetail adds what an operator needs to judge a single account.
type UserDetail struct {
	UserView
	OpenSessionID           *uuid.UUID
	ActiveRefreshTokenCount int
}

func accountOf(ctx context.Context, tx Tx, userID uuid.UUID) (balance.Account, error) {
	acc, err := tx.Balance().GetAccountByUserID(ctx, userID)
	if errors.Is(err, balance.ErrAccountNotFound) {
		return balance.Account{}, nil
	}
	return acc, err
}

// ListUsers pages through users (id DESC) with their balances.
func (s *Service) ListUsers(ctx context.Context, f auth.UserFilter, after *uuid.UUID, limit int) ([]UserView, *uuid.UUID, error) {
	var views []UserView
	var next *uuid.UUID
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		users, err := tx.Users().List(ctx, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(users) > limit {
			users = users[:limit]
			id := users[limit-1].ID
			next = &id
		}
		for _, u := range users {
			acc, err := accountOf(ctx, tx, u.ID)
			if err != nil {
				return err
			}
			views = append(views, UserView{User: u, Balance: acc})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return views, next, nil
}

// GetUser returns one user with balance, open session and active token count.
func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (UserDetail, error) {
	var d UserDetail
	err := s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		u, err := tx.Users().GetByID(ctx, id)
		if err != nil {
			return userNotFound(err)
		}
		acc, err := accountOf(ctx, tx, id)
		if err != nil {
			return err
		}
		open, err := tx.SessionGate().OpenSessions(ctx, id)
		if err != nil {
			return err
		}
		count, err := tx.RefreshTokens().CountActive(ctx, id, s.clk.Now())
		if err != nil {
			return err
		}
		d = UserDetail{UserView: UserView{User: u, Balance: acc}, ActiveRefreshTokenCount: count}
		if len(open) > 0 {
			sid := open[0].ID
			d.OpenSessionID = &sid
		}
		return nil
	})
	return d, err
}

// ChangeStatusInput is a PATCH /admin/users/{id} request.
type ChangeStatusInput struct {
	AdminID, TargetID uuid.UUID
	To                auth.UserStatus
	Reason            string
	Meta              RequestMeta
}

func validStatus(s auth.UserStatus) bool {
	return s == auth.UserStatusActive || s == auth.UserStatusSuspended || s == auth.UserStatusDeleted
}

// ChangeStatus applies an admin status change (spec 3.12): ACTIVE↔SUSPENDED,
// either → DELETED (final). A no-op change writes no audit row.
func (s *Service) ChangeStatus(ctx context.Context, in ChangeStatusInput) (view UserView, err error) {
	ctx, span := otel.Start(ctx, "admin.ChangeStatus")
	defer otel.End(span, &err)
	if !validStatus(in.To) {
		return UserView{}, errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid status"),
			map[string]any{"fields": []map[string]any{{"field": "status", "reason": "invalid_value"}}})
	}
	if n := len([]rune(in.Reason)); n < 3 || n > 500 {
		return UserView{}, validationError("reason", "length")
	}
	now := s.clk.Now()
	err = s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if in.To == auth.UserStatusDeleted { // lock order (spec 4.6.0): account before user
			if _, lerr := tx.Balance().LockAccountByUserID(ctx, in.TargetID); lerr != nil && !errors.Is(lerr, balance.ErrAccountNotFound) {
				return lerr
			}
		}
		u, lerr := tx.Users().LockByID(ctx, in.TargetID)
		if lerr != nil {
			return userNotFound(lerr)
		}
		if in.TargetID == in.AdminID {
			return errs.New(errs.CodeCannotModifySelf, "an admin cannot change their own status")
		}
		if u.Status == in.To {
			acc, aerr := accountOf(ctx, tx, u.ID)
			view = UserView{User: u, Balance: acc}
			return aerr
		}
		if u.Status == auth.UserStatusDeleted {
			return errs.New(errs.CodeInvalidUserStatusTransition, "deleted accounts are final")
		}
		revoked := int64(0)
		if in.To == auth.UserStatusDeleted {
			count, cerr := tx.RefreshTokens().CountActive(ctx, u.ID, now)
			if cerr != nil {
				return cerr
			}
			revoked = int64(count)
			if derr := s.deleteAccountTx(ctx, tx, u.ID, now); derr != nil {
				return derr
			}
		} else {
			if uerr := tx.Users().UpdateStatus(ctx, u.ID, in.To, now); uerr != nil {
				return uerr
			}
			if in.To == auth.UserStatusSuspended {
				n, rerr := tx.RefreshTokens().RevokeAllForUser(ctx, u.ID, "USER_STATUS_CHANGED", now)
				if rerr != nil {
					return rerr
				}
				revoked = n
			}
		}
		if aerr := tx.Audit().Insert(ctx, s.audit(in.AdminID, ActionUserStatusChange, u.ID, in.Meta, "", in.Reason,
			map[string]any{"from": string(u.Status), "to": string(in.To), "revoked_refresh_tokens": revoked})); aerr != nil {
			return aerr
		}
		updated, gerr := tx.Users().GetByID(ctx, u.ID)
		if gerr != nil {
			return gerr
		}
		acc, aerr := accountOf(ctx, tx, u.ID)
		view = UserView{User: updated, Balance: acc}
		return aerr
	})
	return view, err
}

// DeleteAccount is the account-deletion routine behind DELETE /me (spec 3.13.3
// steps 2–4); it satisfies auth.AccountDeleter.
func (s *Service) DeleteAccount(ctx context.Context, userID uuid.UUID, at time.Time) (err error) {
	ctx, span := otel.Start(ctx, "admin.DeleteAccount")
	defer otel.End(span, &err)
	return s.store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if _, lerr := tx.Balance().LockAccountByUserID(ctx, userID); lerr != nil && !errors.Is(lerr, balance.ErrAccountNotFound) {
			return lerr
		}
		if _, lerr := tx.Users().LockByID(ctx, userID); lerr != nil {
			return userNotFound(lerr)
		}
		return s.deleteAccountTx(ctx, tx, userID, at)
	})
}

var _ auth.AccountDeleter = (*Service)(nil)

// deleteAccountTx runs steps 2c–4 of §3.13.3 once the account and user are locked:
// refuse while a session runs, cancel a RESERVED one, mark DELETED, drop tokens.
func (s *Service) deleteAccountTx(ctx context.Context, tx Tx, userID uuid.UUID, at time.Time) error {
	open, err := tx.SessionGate().OpenSessions(ctx, userID)
	if err != nil {
		return err
	}
	for _, o := range open {
		if o.Status == "STARTING" || o.Status == "ACTIVE" || o.Status == "ENDING" {
			return errs.WithDetails(
				errs.Wrap(auth.ErrAccountHasActiveSession, errs.CodeAccountHasActiveSession, "account has an active session"),
				map[string]any{"session_id": o.ID.String()})
		}
	}
	for _, o := range open {
		if o.Status == "RESERVED" {
			if err := tx.SessionGate().CancelReserved(ctx, o.ID, at); err != nil {
				return err
			}
		}
	}
	if err := tx.Users().UpdateStatus(ctx, userID, auth.UserStatusDeleted, at); err != nil {
		return err
	}
	if err := tx.RefreshTokens().DeleteAllForUser(ctx, userID); err != nil {
		return err
	}
	return tx.PasswordResets().DeleteAllForUser(ctx, userID)
}
