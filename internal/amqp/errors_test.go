package amqp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

func TestClassify(t *testing.T) {
	boom := errors.New("boom")
	pg := func(code string) error { return fmt.Errorf("query: %w", &pgconn.PgError{Code: code}) }
	cases := []struct {
		name     string
		err      error
		shutdown bool
		want     amqp.Class
	}{
		{"nil", nil, false, amqp.ClassAck},
		{"already processed", events.ErrAlreadyProcessed, false, amqp.ClassAck},
		{"already processed wrapped", fmt.Errorf("tx: %w", events.ErrAlreadyProcessed), false, amqp.ClassAck},
		{"skipped deleted user", events.ErrSkippedDeletedUser, false, amqp.ClassAck},
		{"skipped deleted user wrapped", fmt.Errorf("h: %w", events.ErrSkippedDeletedUser), false, amqp.ClassAck},
		{"canceled while shutting down", context.Canceled, true, amqp.ClassRequeue},
		{"canceled wrapped while shutting down", fmt.Errorf("h: %w", context.Canceled), true, amqp.ClassRequeue},
		{"canceled not shutting down", context.Canceled, false, amqp.ClassRetry},
		{"permanent", events.Permanent("ENTITY_NOT_FOUND", boom), false, amqp.ClassDead},
		{"permanent wrapped", fmt.Errorf("h: %w", events.Permanent("INVARIANT_VIOLATION", boom)), false, amqp.ClassDead},
		{"provider not retryable", &provider.Error{Retryable: false}, false, amqp.ClassDead},
		{"provider not retryable wrapped", fmt.Errorf("h: %w", &provider.Error{Retryable: false}), false, amqp.ClassDead},
		{"provider retryable", &provider.Error{Retryable: true}, false, amqp.ClassRetry},
		{"pg 40001", pg("40001"), false, amqp.ClassRetry},
		{"pg 40P01", pg("40P01"), false, amqp.ClassRetry},
		{"pg 08006", pg("08006"), false, amqp.ClassRetry},
		{"pg 53300", pg("53300"), false, amqp.ClassRetry},
		{"pg 57P01", pg("57P01"), false, amqp.ClassRetry},
		{"pg 57P03", pg("57P03"), false, amqp.ClassRetry},
		{"pg 23505", pg("23505"), false, amqp.ClassDead},
		{"pg 23503", pg("23503"), false, amqp.ClassDead},
		{"pg 22001", pg("22001"), false, amqp.ClassDead},
		{"pg other class", pg("42P01"), false, amqp.ClassRetry},
		{"deadline exceeded", context.DeadlineExceeded, false, amqp.ClassRetry},
		{"deadline exceeded wrapped", fmt.Errorf("h: %w", context.DeadlineExceeded), true, amqp.ClassRetry},
		{"plain error", boom, false, amqp.ClassRetry},
		{"plain error shutting down", boom, true, amqp.ClassRetry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, amqp.Classify(c.err, c.shutdown))
		})
	}
}

func TestClassify_PermanentWinsOverShutdown(t *testing.T) {
	perm := events.Permanent("ENTITY_NOT_FOUND", errors.New("gone"))
	assert.Equal(t, amqp.ClassDead, amqp.Classify(perm, true), "permanent while shutting down is still dead")
	assert.Equal(t, amqp.ClassRetry, amqp.Classify(context.Canceled, false), "a bare Canceled is not permanent")
	assert.Equal(t, amqp.ClassRequeue, amqp.Classify(context.Canceled, true))
}
