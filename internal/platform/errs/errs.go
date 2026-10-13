// Package errs defines the canonical error codes, kinds and origin-location
// tracking shared by REST and WebSocket layers (spec 5.10, 7.4.8, D-013).
package errs

import "errors"

// Kind classifies an error as caused by the caller (business) or the system (internal).
type Kind int

const (
	KindBusiness Kind = iota
	KindInternal
)

var internalCodes = map[Code]bool{
	CodeInternalError:        true,
	CodeContentMisconfigured: true,
	CodeServiceUnavailable:   true,
	CodeProviderFailure:      true,
	CodeAiGenerationFailed:   true,
}

var registered = func() map[Code]bool {
	m := make(map[Code]bool)
	for _, c := range AllCodes() {
		m[c] = true
	}
	return m
}()

// Error is the concrete error carrying a code, message, origin and cause.
type Error struct {
	code   Code
	msg    string
	loc    Location
	hasLoc bool
	cause  error
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error { return e.cause }

// New creates an error with code and message, recording the caller's location.
func New(code Code, msg string) error {
	return &Error{code: code, msg: msg, loc: caller(1), hasLoc: true}
}

// Wrap wraps err with code and message. The origin location is recorded only
// if err's chain has none yet.
func Wrap(err error, code Code, msg string) error {
	e := &Error{code: code, msg: msg, cause: err}
	if _, ok := LocationOf(err); !ok {
		e.loc, e.hasLoc = caller(1), true
	}
	return e
}

// CodeOf returns the code of the outermost errs error in the chain, or "".
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}

// KindOf returns KindInternal for errors without a registered code and for
// the internal codes; KindBusiness otherwise.
func KindOf(err error) Kind {
	c := CodeOf(err)
	if !registered[c] || internalCodes[c] {
		return KindInternal
	}
	return KindBusiness
}

type detailed struct {
	err     error
	details any
}

func (d *detailed) Error() string { return d.err.Error() }
func (d *detailed) Unwrap() error { return d.err }

// WithDetails attaches structured details (e.g. the `details` field of the
// REST error object) to err. A nil err stays nil.
func WithDetails(err error, details any) error {
	if err == nil {
		return nil
	}
	return &detailed{err: err, details: details}
}

// DetailsOf returns the outermost details attached via WithDetails, or nil.
func DetailsOf(err error) any {
	var d *detailed
	if errors.As(err, &d) {
		return d.details
	}
	return nil
}
