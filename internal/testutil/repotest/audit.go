package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/admin"
	"github.com/nabePi/kotoki-backend/internal/auth"
)

func adminTxRun(t *testing.T, s admin.Store, fn func(ctx context.Context, tx admin.Tx)) {
	t.Helper()
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx admin.Tx) error {
		fn(ctx, tx)
		return nil
	}))
}

func newAudit(adminID, target uuid.UUID, action string, at time.Time) admin.AuditLog {
	return admin.AuditLog{
		ID: uuid.Must(uuid.NewV7()), AdminUserID: adminID, Action: action, TargetType: admin.TargetUser,
		TargetID: target.String(), TargetUserID: &target, RequestID: "req-1",
		Details: map[string]any{"seconds": float64(600)}, CreatedAt: at,
	}
}

// AuditRepoSuite runs the admin.AuditRepository contract. Users are created
// through the same store, so a database-backed store satisfies its foreign keys.
func AuditRepoSuite(t *testing.T, newStore func(t *testing.T) admin.Store) {
	t.Helper()
	Each(t, newStore,
		Case[admin.Store]{Name: "InsertAndList", Run: func(t *testing.T, s admin.Store) {
			a, u := NewUser("admin@example.com"), NewUser("user@example.com")
			a.Role = auth.RoleAdmin
			adminTxRun(t, s, func(ctx context.Context, tx admin.Tx) {
				require.NoError(t, tx.Users().Insert(ctx, a))
				require.NoError(t, tx.Users().Insert(ctx, u))
				idem := "0192f1a0-1111-7000-8000-000000000001"
				reason := "support credit"
				row := newAudit(a.ID, u.ID, admin.ActionBalanceGrant, now)
				row.IdempotencyKey, row.Reason = &idem, &reason
				require.NoError(t, tx.Audit().Insert(ctx, row))

				got, err := tx.Audit().List(ctx, admin.AuditFilter{}, nil, 10)
				require.NoError(t, err)
				require.Len(t, got, 1)
				require.Equal(t, row.ID, got[0].ID)
				require.Equal(t, admin.ActionBalanceGrant, got[0].Action)
				require.Equal(t, u.ID.String(), got[0].TargetID)
				require.Equal(t, "req-1", got[0].RequestID)
				require.NotNil(t, got[0].IdempotencyKey)
				require.Equal(t, idem, *got[0].IdempotencyKey)
				require.Equal(t, float64(600), got[0].Details["seconds"])
			})
		}},
		Case[admin.Store]{Name: "FiltersAndCursor", Run: func(t *testing.T, s admin.Store) {
			a1, a2, u1, u2 := NewUser("a1@example.com"), NewUser("a2@example.com"), NewUser("u1@example.com"), NewUser("u2@example.com")
			adminTxRun(t, s, func(ctx context.Context, tx admin.Tx) {
				for _, u := range []auth.User{a1, a2, u1, u2} {
					require.NoError(t, tx.Users().Insert(ctx, u))
				}
				rows := []admin.AuditLog{
					newAudit(a1.ID, u1.ID, admin.ActionBalanceGrant, now),
					newAudit(a1.ID, u2.ID, admin.ActionBalanceAdjustment, now),
					newAudit(a2.ID, u1.ID, admin.ActionUserStatusChange, now),
					newAudit(a1.ID, u1.ID, admin.ActionBalanceGrant, now),
				}
				for _, r := range rows {
					require.NoError(t, tx.Audit().Insert(ctx, r))
				}

				all, err := tx.Audit().List(ctx, admin.AuditFilter{}, nil, 10)
				require.NoError(t, err)
				require.Len(t, all, 4)
				for i := 1; i < len(all); i++ {
					require.Greater(t, all[i-1].ID.String(), all[i].ID.String(), "id DESC")
				}

				byAdmin, err := tx.Audit().List(ctx, admin.AuditFilter{AdminUserID: &a2.ID}, nil, 10)
				require.NoError(t, err)
				require.Len(t, byAdmin, 1)
				byTarget, err := tx.Audit().List(ctx, admin.AuditFilter{TargetUserID: &u1.ID}, nil, 10)
				require.NoError(t, err)
				require.Len(t, byTarget, 3)
				byAction, err := tx.Audit().List(ctx, admin.AuditFilter{Action: admin.ActionBalanceGrant}, nil, 10)
				require.NoError(t, err)
				require.Len(t, byAction, 2)

				page1, err := tx.Audit().List(ctx, admin.AuditFilter{}, nil, 3)
				require.NoError(t, err)
				require.Len(t, page1, 3)
				page2, err := tx.Audit().List(ctx, admin.AuditFilter{}, &page1[2].ID, 3)
				require.NoError(t, err)
				require.Len(t, page2, 1)
			})
		}},
	)
}
