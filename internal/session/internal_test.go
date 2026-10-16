package session

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestBalanceErrMapsReservationProblemsToInternalErrors(t *testing.T) {
	err := balanceErr(balance.ErrReservationState)
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err))
	require.ErrorIs(t, err, balance.ErrReservationState)
	require.Contains(t, err.Error(), "reservation_state_mismatch")

	err = balanceErr(balance.ErrReservationNotFound)
	require.Equal(t, errs.CodeInternalError, errs.CodeOf(err))
	require.ErrorIs(t, err, balance.ErrReservationNotFound)

	other := errors.New("connection reset")
	require.Same(t, other, balanceErr(other), "anything else passes through untouched")
}

func TestNoControlPublishesNothing(t *testing.T) {
	require.NoError(t, noControl{}.Publish(context.Background(), uuid.New(), ControlMessage{Type: "status_changed"}))
}
