package internal_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// runtimeFacade is the ten-method surface of spec 7.13.1 the realtime runtime consumes.
type runtimeFacade interface {
	GetForAttach(ctx context.Context, sessionID uuid.UUID) (session.RuntimeRecord, error)
	ClaimRuntime(ctx context.Context, sessionID uuid.UUID, allowed []session.Status) (int64, session.RuntimeRecord, error)
	MarkStarting(ctx context.Context, sessionID uuid.UUID, fence int64) (session.RuntimeRecord, error)
	Activate(ctx context.Context, sessionID uuid.UUID, fence int64) (time.Time, time.Time, error)
	FailStart(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error)
	AddDisconnected(ctx context.Context, sessionID uuid.UUID, fence int64, seconds int) error
	MarkHandoff(ctx context.Context, sessionID uuid.UUID, fence int64) error
	BeginEnding(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) error
	Complete(ctx context.Context, sessionID uuid.UUID, fence int64) (session.Settlement, error)
	Fail(ctx context.Context, sessionID uuid.UUID, fence int64, reason session.EndReason) (session.Settlement, error)
}

// Compile-only contract checks: a build error here means a milestone drifted.
var (
	_ provider.RealtimeRouter                             = provider.RealtimeRouter(nil)
	_ func(prompt.Template, any) (prompt.Rendered, error) = prompt.Render
	_ content.SessionConfigLoader                         = content.SessionConfigLoader(nil)
	_ auth.TicketStore                                    = auth.TicketStore(nil)
	_ ratelimit.Limiter                                   = ratelimit.Limiter(nil)
	_ runtimeFacade                                       = (*session.RuntimeService)(nil)
	_ *uuid.UUID                                          = session.Event{}.TurnID
)

func TestM5Preflight(t *testing.T) { t.Log("contracts compile") }

func TestWSCodesExist(t *testing.T) {
	codes := []errs.Code{
		errs.CodeTicketInvalid, errs.CodeForbidden, errs.CodeSessionNotFound, errs.CodeHandshakeTimeout,
		errs.CodeSessionStateConflict, errs.CodeSessionNotResumable, errs.CodeSessionStartDeadlinePassed,
		errs.CodeSessionTakenOver, errs.CodeRateLimited, errs.CodeServerDraining, errs.CodeSlowConsumer,
		errs.CodeHeartbeatTimeout, errs.CodeProviderFailure, errs.CodeServerAtCapacity, errs.CodeServiceUnavailable,
		errs.CodeProtocolViolation, errs.CodeUnsupportedProtocolVersion, errs.CodeUnsupportedAudioFormat,
		errs.CodeInternalError, errs.CodeInvalidMessage, errs.CodeCommandRejected, errs.CodeAudioFrameInvalid,
		errs.CodeAiGenerationFailed, errs.CodeOriginNotAllowed,
	}
	for _, c := range codes {
		require.NotEmpty(t, errs.Messages[c], c)
	}
	require.Equal(t, "Koneksi tidak valid atau sudah kedaluwarsa. Menyambungkan ulang…", errs.Messages[errs.CodeTicketInvalid])
	require.Equal(t, "Permintaan ditolak.", errs.Messages[errs.CodeOriginNotAllowed])
	require.Equal(t, 503, httpapi.StatusOf(errs.CodeServerDraining))
	require.Equal(t, 503, httpapi.StatusOf(errs.CodeServerAtCapacity))
	require.Equal(t, 429, httpapi.StatusOf(errs.CodeRateLimited))
	require.Equal(t, 403, httpapi.StatusOf(errs.CodeOriginNotAllowed))
}
