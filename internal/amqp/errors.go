package amqp

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Class is what the consumer does with a delivery after the handler returned (spec 9.7.3).
type Class int

// Classes of a handler outcome.
const (
	ClassAck     Class = iota // success, duplicate or deliberately skipped
	ClassRetry                // retryable: climb the retry ladder
	ClassDead                 // not retryable: straight to the DLQ
	ClassRequeue              // shutdown: Nack(requeue=true), not counted as a retry
)

// codeRetriesExhausted is the x-error-class of a message that used up its retries.
const codeRetriesExhausted = "RETRIES_EXHAUSTED"

// Classify maps a handler error to a Class by the 11 ordered rules of spec 9.7.3. Decode,
// unsupported-version and unknown-type failures reach it already wrapped as PermanentError (rule 4),
// and a recovered panic arrives as a plain error (rule 10, falls to rule 11).
func Classify(err error, shuttingDown bool) Class {
	if err == nil || errors.Is(err, events.ErrAlreadyProcessed) || errors.Is(err, events.ErrSkippedDeletedUser) { // 1
		return ClassAck
	}
	if shuttingDown && errors.Is(err, context.Canceled) { // 2
		return ClassRequeue
	}
	var perm *events.PermanentError
	if errors.As(err, &perm) { // 3, 4
		return ClassDead
	}
	var pe *provider.Error
	if errors.As(err, &pe) { // 5, 6
		if provider.IsRetryable(err) {
			return ClassRetry
		}
		return ClassDead
	}
	// 7: PgError classes 40, 08, 53 and 57P01/57P03 are retryable and, like any other SQLSTATE,
	// fall through to the default. 8: constraint (23) and data exception (22) never succeed on retry.
	var pg *pgconn.PgError
	if errors.As(err, &pg) && len(pg.Code) >= 2 && (pg.Code[:2] == "23" || pg.Code[:2] == "22") {
		return ClassDead
	}
	return ClassRetry // 9 (deadline), 10 (panic), 11 (everything else)
}

// errorCode is the x-error-class of a dead-lettered message: the PermanentError code, otherwise
// RETRIES_EXHAUSTED.
func errorCode(err error) string {
	var perm *events.PermanentError
	if errors.As(err, &perm) {
		return perm.Code
	}
	return codeRetriesExhausted
}

// headerInt reads an integer header of any AMQP integer width; absent or non-integer is 0.
func headerInt(h amqp091.Table, key string) int {
	switch v := h[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	}
	return 0
}

// truncate cuts s to at most n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// asPermanent returns err when it already is a PermanentError, else wraps it with code.
func asPermanent(code string, err error) error {
	var perm *events.PermanentError
	if errors.As(err, &perm) {
		return err
	}
	return events.Permanent(code, err)
}
