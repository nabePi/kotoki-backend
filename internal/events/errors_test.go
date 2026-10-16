package events_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
)

func TestPermanent_UnwrapAndError(t *testing.T) {
	cause := errors.New("boom")
	err := events.Permanent("ENTITY_NOT_FOUND", cause)
	require.EqualError(t, err, "ENTITY_NOT_FOUND: boom")
	require.ErrorIs(t, err, cause)
	var pe *events.PermanentError
	require.ErrorAs(t, fmt.Errorf("wrap: %w", err), &pe)
	require.Equal(t, "ENTITY_NOT_FOUND", pe.Code)
}

func TestPermanent_NilCause(t *testing.T) {
	require.EqualError(t, events.Permanent("INVARIANT_VIOLATION", nil), "INVARIANT_VIOLATION")
}

func TestErrSkippedDeletedUser_IsDistinctSentinel(t *testing.T) {
	wrapped := fmt.Errorf("handle: %w", events.ErrSkippedDeletedUser)
	require.ErrorIs(t, wrapped, events.ErrSkippedDeletedUser)
	require.NotErrorIs(t, wrapped, events.ErrAlreadyProcessed)
	require.NotErrorIs(t, fmt.Errorf("x: %w", events.ErrAlreadyProcessed), events.ErrSkippedDeletedUser)
	var pe *events.PermanentError
	require.False(t, errors.As(wrapped, &pe))
}

func TestErrAlreadyProcessed_Message(t *testing.T) {
	require.EqualError(t, events.ErrAlreadyProcessed, "event already processed")
}
