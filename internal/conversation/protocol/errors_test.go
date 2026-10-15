package protocol

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestCatalog(t *testing.T) {
	rows := []struct {
		code      errs.Code
		fatal     bool
		close     int
		retryable bool
	}{
		{errs.CodeTicketInvalid, true, 4001, true},
		{errs.CodeForbidden, true, 4003, false},
		{errs.CodeSessionNotFound, true, 4004, false},
		{errs.CodeHandshakeTimeout, true, 4008, true},
		{errs.CodeSessionStateConflict, true, 4009, false},
		{errs.CodeSessionNotResumable, true, 4010, false},
		{errs.CodeSessionStartDeadlinePassed, true, 4010, false},
		{errs.CodeSessionTakenOver, true, 4011, false},
		{errs.CodeRateLimited, true, 4029, true},
		{errs.CodeServerDraining, true, 4100, true},
		{errs.CodeSlowConsumer, true, 4101, true},
		{errs.CodeHeartbeatTimeout, true, 4102, true},
		{errs.CodeProviderFailure, true, 4500, false},
		{errs.CodeServerAtCapacity, true, 4503, true},
		{errs.CodeServiceUnavailable, true, 4503, true},
		{errs.CodeProtocolViolation, true, 1008, false},
		{errs.CodeUnsupportedProtocolVersion, true, 1008, false},
		{errs.CodeUnsupportedAudioFormat, true, 1008, false},
		{errs.CodeInternalError, true, 1011, true},
		{errs.CodeInvalidMessage, false, 0, false},
		{errs.CodeCommandRejected, false, 0, false},
		{errs.CodeAudioFrameInvalid, false, 0, false},
		{errs.CodeAiGenerationFailed, false, 0, false},
	}
	require.Len(t, Catalog, len(rows))
	for _, r := range rows {
		s, ok := Catalog[r.code]
		require.True(t, ok, r.code)
		require.Equal(t, Spec{Fatal: r.fatal, Close: r.close, Retryable: r.retryable}, s, r.code)
		require.Equal(t, r.close, CloseCode(r.code), r.code)
		_, hasMsg := errs.Messages[r.code]
		require.True(t, hasMsg, r.code)
	}
	require.Equal(t, 0, CloseCode(errs.Code("NOPE")))
}

func TestNewErrorFormat(t *testing.T) {
	rid := uuid.MustParse("0192f1a0-2222-7000-8000-000000000009")
	p := NewError(errs.CodeAudioFrameInvalid, rid, nil, nil)
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.JSONEq(t, `{"error":{"code":"AUDIO_FRAME_INVALID","message":"Audio dari mikrofon tidak terbaca dengan baik.",
"request_id":"0192f1a0-2222-7000-8000-000000000009","details":null},"retryable":false,"fatal":false,"ref_event_id":null}`, string(raw))

	ref := uuid.MustParse("0192f1a0-3333-7000-8000-000000000001")
	p = NewError(errs.CodeServerAtCapacity, rid, &ref, map[string]any{"a": 1})
	require.Equal(t, ref.String(), p.Error.RequestID)
	require.Equal(t, ref.String(), *p.RefEventID)
	require.True(t, p.Fatal)
	require.True(t, p.Retryable)

	p = NewError(errs.CodeSessionStateConflict, rid, nil, nil)
	require.False(t, p.Retryable)
	p = NewError(errs.CodeSessionStateConflict, rid, nil, nil, WithRetryable(true))
	require.True(t, p.Retryable)
	require.True(t, p.Fatal)
}
