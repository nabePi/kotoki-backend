package admin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
)

func TestRecordTranscriptViewWritesAudit(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	owner := e.user(t, "hana@example.com", auth.RoleUser, 0)
	sessionID := uuid.New()

	require.NoError(t, e.svc.RecordTranscriptView(context.Background(), adm.ID, sessionID, owner.ID, e.meta()))

	rows := e.store.Audit()
	require.Len(t, rows, 1)
	a := rows[0]
	require.Equal(t, admin.ActionSessionTranscriptView, a.Action)
	require.Equal(t, "SESSION", a.TargetType)
	require.Equal(t, sessionID.String(), a.TargetID)
	require.Equal(t, owner.ID, *a.TargetUserID)
	require.Equal(t, adm.ID, a.AdminUserID)
	require.Equal(t, "req-123", a.RequestID)
	require.Nil(t, a.IdempotencyKey)
}

func TestRecordTranscriptViewFailureIsReturned(t *testing.T) {
	e := newEnv(t)
	adm := e.user(t, "root@example.com", auth.RoleAdmin, 0)
	boom := errors.New("audit down")
	e.store.FailNext("Audit.Insert", boom)

	err := e.svc.RecordTranscriptView(context.Background(), adm.ID, uuid.New(), uuid.New(), e.meta())
	require.ErrorIs(t, err, boom)
	require.Empty(t, e.store.Audit())
}
