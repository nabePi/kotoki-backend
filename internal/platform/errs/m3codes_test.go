package errs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestM3CodesHaveMessagesAndStatus(t *testing.T) {
	cases := []struct {
		code   errs.Code
		status int
	}{
		{errs.CodeBalanceInsufficient, 402}, {errs.CodeSessionNotFound, 404}, {errs.CodeSessionAlreadyOpen, 409},
		{errs.CodeSessionNotCancellable, 409}, {errs.CodeSessionNotConnectable, 409}, {errs.CodeSessionStartDeadlinePassed, 409},
		{errs.CodeIdempotencyKeyRequired, 400}, {errs.CodeIdempotencyKeyInvalid, 400}, {errs.CodeIdempotencyKeyReused, 409},
		{errs.CodeInvalidCursor, 400}, {errs.CodeTranscriptExpired, 410},
		{errs.CodeTopicNotAvailable, 422}, {errs.CodeScenarioNotAvailable, 422}, {errs.CodeScenarioTopicMismatch, 422},
		{errs.CodeCharacterNotAvailable, 422}, {errs.CodeContentValidationFailed, 422},
		{errs.CodeContentMisconfigured, 500}, {errs.CodeContentInvalidTransition, 409}, {errs.CodeContentVersionRequired, 409},
		{errs.CodeNotFound, 404}, {errs.CodeTermsNotAccepted, 403}, {errs.CodeAccountSuspended, 403}, {errs.CodeServiceUnavailable, 503},
	}
	for _, c := range cases {
		require.NotEmpty(t, errs.Messages[c.code], c.code)
		require.Equal(t, c.status, httpapi.StatusOf(c.code), c.code)
	}
}

func TestM3RemovedCodesAbsent(t *testing.T) {
	all := map[errs.Code]bool{}
	for _, c := range errs.AllCodes() {
		all[c] = true
	}
	for _, removed := range []errs.Code{"CONVERSATION_STYLE_NOT_AVAILABLE", "EXPRESSION_PROFILE_NOT_AVAILABLE", "CONTENT_IN_USE_AS_DEFAULT"} {
		require.False(t, all[removed], removed)
	}
	require.Len(t, errs.AllCodes(), 67)
}
