package events

import "errors"

// PermanentError marks a failure that retrying cannot fix; the consumer framework dead-letters it
// at once (spec 9.7.3). Code is one of DECODE_ERROR, UNSUPPORTED_EVENT_VERSION, ENTITY_NOT_FOUND,
// INVARIANT_VIOLATION, CONFIGURATION_ERROR or a domain error code.
type PermanentError struct {
	Code string
	Err  error
}

// Permanent wraps err as a PermanentError with code.
func Permanent(code string, err error) error { return &PermanentError{Code: code, Err: err} }

func (e *PermanentError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *PermanentError) Unwrap() error { return e.Err }

// ErrSkippedDeletedUser is returned by a handler that found the event's user DELETED after it
// committed MarkProcessed alone; the framework ACKs and counts result="skipped_deleted_user"
// (spec 9.7.2, 13.9.3).
var ErrSkippedDeletedUser = errors.New("event skipped: user deleted")
