package auth

import "errors"

// Sentinel errors returned by ports and services. Services translate them to
// errs codes at the HTTP boundary.
var (
	ErrEmailTaken              = errors.New("auth: email already registered")
	ErrPhoneTaken              = errors.New("auth: phone number already registered")
	ErrUserNotFound            = errors.New("auth: user not found")
	ErrUserNotActive           = errors.New("auth: user is not active")
	ErrRefreshTokenNotFound    = errors.New("auth: refresh token not found")
	ErrResetTokenNotFound      = errors.New("auth: reset token not found")
	ErrTokenExpired            = errors.New("auth: token expired")
	ErrTokenInvalid            = errors.New("auth: token invalid")
	ErrTicketInvalid           = errors.New("auth: ticket invalid")
	ErrSessionNotFound         = errors.New("auth: session not found")
	ErrAccountHasActiveSession = errors.New("auth: account has an active session")
)
