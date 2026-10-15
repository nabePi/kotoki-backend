package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrorKind is the normalised failure class (spec 8.3).
type ErrorKind string

// Error kinds, normalised across vendors.
const (
	KindTimeout         ErrorKind = "TIMEOUT"
	KindRateLimited     ErrorKind = "RATE_LIMITED"
	KindAuth            ErrorKind = "AUTH"
	KindInvalidRequest  ErrorKind = "INVALID_REQUEST"
	KindUnavailable     ErrorKind = "UNAVAILABLE"
	KindContentFiltered ErrorKind = "CONTENT_FILTERED"
	KindInvalidResponse ErrorKind = "INVALID_RESPONSE"
	KindCancelled       ErrorKind = "CANCELLED" // the caller gave up: not a failure
)

const (
	maxErrorMessage   = 512
	vendorCodeLocal   = "local_concurrency"
	vendorCodeCircuit = "circuit_open"
)

// Error is a provider failure. Message never carries an API key.
type Error struct {
	Provider   Name
	Capability Capability
	Kind       ErrorKind
	Retryable  bool
	HTTPStatus int           // 0 when not HTTP
	VendorCode string        // raw vendor code, e.g. "insufficient_quota"
	RetryAfter time.Duration // from Retry-After when present
	Message    string        // vendor message, cut to 512 bytes
	Cause      error
}

// NewError builds an Error with Message cut to 512 bytes on a rune boundary.
func NewError(p Name, c Capability, k ErrorKind, msg string) *Error {
	return &Error{Provider: p, Capability: c, Kind: k, Message: truncateUTF8(msg, maxErrorMessage)}
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// NewCircuitOpen is the fail-fast error returned while a breaker is open.
func NewCircuitOpen(c Capability) *Error {
	return &Error{Capability: c, Kind: KindUnavailable, VendorCode: vendorCodeCircuit, Retryable: true,
		Message: "circuit breaker open"}
}

// Error formats as "provider OPENAI/LLM: RATE_LIMITED (429 rate_limit_exceeded): msg".
func (e *Error) Error() string {
	var detail []string
	if e.HTTPStatus != 0 {
		detail = append(detail, fmt.Sprint(e.HTTPStatus))
	}
	if e.VendorCode != "" {
		detail = append(detail, e.VendorCode)
	}
	s := fmt.Sprintf("provider %s/%s: %s", e.Provider, e.Capability, e.Kind)
	if len(detail) > 0 {
		s += " (" + strings.Join(detail, " ") + ")"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

// KindOf classifies any error: *Error → Kind, context.Canceled → CANCELLED,
// context.DeadlineExceeded → TIMEOUT, everything else UNAVAILABLE.
func KindOf(err error) ErrorKind {
	var pe *Error
	switch {
	case errors.As(err, &pe):
		return pe.Kind
	case errors.Is(err, context.Canceled):
		return KindCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return KindTimeout
	default:
		return KindUnavailable
	}
}

// IsRetryable reports whether the same vendor may be retried (async paths only).
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	k := KindOf(err)
	return k == KindTimeout || k == KindUnavailable
}

// FallbackEligible reports whether a failure may trigger the per-character TTS
// fallback voice (the only vendor fallback that exists, D-017).
func FallbackEligible(err error) bool {
	switch KindOf(err) {
	case KindContentFiltered, KindCancelled:
		return false
	default:
		return true
	}
}

// CountsForBreaker reports whether a failure counts towards the circuit breaker.
func CountsForBreaker(err error) bool {
	var pe *Error
	switch KindOf(err) {
	case KindContentFiltered, KindCancelled:
		return false
	case KindInvalidRequest:
		return errors.As(err, &pe) && pe.HTTPStatus == 404 // model/voice does not exist
	case KindRateLimited:
		return !errors.As(err, &pe) || pe.VendorCode != vendorCodeLocal
	default:
		return true
	}
}

// ClassifyHTTPStatus maps an HTTP status to a Kind without importing net/http.
func ClassifyHTTPStatus(status int) ErrorKind {
	switch status {
	case 408, 504:
		return KindTimeout
	case 429:
		return KindRateLimited
	case 401, 403:
		return KindAuth
	case 400, 404, 409, 413, 422:
		return KindInvalidRequest
	default:
		return KindUnavailable
	}
}
