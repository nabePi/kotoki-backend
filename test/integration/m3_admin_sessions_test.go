//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/session"
)

// adminAuthz promotes a fresh user to ADMIN and logs in again so the token carries the role.
func (l *lifecycle) adminAuthz() string {
	l.t.Helper()
	_, _ = l.newUser()
	email := "user" + string(rune('a'+l.users)) + "@example.com"
	_, err := l.pool.Exec(context.Background(), `UPDATE users SET role = 'ADMIN' WHERE email = $1`, email)
	require.NoError(l.t, err)
	r := l.call(http.MethodPost, "/api/v1/auth/login", `{"email":"`+email+`","password":"correct horse battery"}`)
	require.Equal(l.t, http.StatusOK, r.code, string(r.body))
	tok, _ := r.json(l.t)["access_token"].(string)
	return "Bearer " + tok
}

func TestAdminSessions(t *testing.T) {
	l := newLifecycle(t)
	ctx := context.Background()
	adm := l.adminAuthz()
	userID, authz := l.newUser()

	sid := l.sessionID(l.create(authz, "admin-key-0001", "USER_FIRST", "600"))
	fence, _ := l.start(sid)
	turnID := uuid.Must(uuid.NewV7())
	now := l.clk.Now()
	require.NoError(t, l.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		if err := tx.Turns().Insert(ctx, fence, session.TurnStart{ID: turnID, SessionID: sid, Sequence: 1, Generation: 1, CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.Turns().UpdateUser(ctx, fence, turnID, session.TurnUser{CreatedAt: now, Text: "hello"}); err != nil {
			return err
		}
		reply := "hi"
		return tx.Turns().Finalize(ctx, fence, turnID, session.TurnFinal{CreatedAt: now, Status: session.TurnCompleted, AssistantText: &reply, CompletedAt: now})
	}))
	l.clk.Advance(420*time.Second + 400*time.Millisecond)
	require.NoError(t, l.runtime.BeginEnding(ctx, sid, fence, session.EndUserEnded))
	l.clk.Advance(2 * time.Second)
	_, err := l.runtime.Complete(ctx, sid, fence)
	require.NoError(t, err)

	t.Run("user token is refused", func(t *testing.T) {
		require.Equal(t, 403, l.call(http.MethodGet, "/api/v1/admin/sessions/"+sid.String(), "", "Authorization", authz).code)
	})

	t.Run("get shows the settled reservation and redacts the vendor", func(t *testing.T) {
		r := l.call(http.MethodGet, "/api/v1/admin/sessions/"+sid.String(), "", "Authorization", adm)
		require.Equal(t, 200, r.code, string(r.body))
		m := r.json(t)
		require.Equal(t, userID.String(), m["user_id"])
		res := m["reservation"].(map[string]any)
		require.Equal(t, "SETTLED", res["status"])
		require.EqualValues(t, m["charged_seconds"], res["consumed_seconds"])
		require.EqualValues(t, 421, res["consumed_seconds"])
		require.NotNil(t, res["settled_at"])
		ch := m["config_snapshot"].(map[string]any)["character"].(map[string]any)
		require.Equal(t, "[REDACTED]", ch["voice_id"])
		require.Equal(t, "[REDACTED]", ch["provider_name"])
		require.Equal(t, "Haruka", ch["name"])
		require.Zero(t, l.count(`SELECT count(*) FROM admin_audit_logs WHERE action = 'SESSION_TRANSCRIPT_VIEW'`))
	})

	t.Run("unknown session is 404", func(t *testing.T) {
		r := l.call(http.MethodGet, "/api/v1/admin/sessions/"+uuid.NewString(), "", "Authorization", adm)
		require.Equal(t, 404, r.code)
		require.Equal(t, "SESSION_NOT_FOUND", r.errCode(t))
	})

	t.Run("list by user with status filter", func(t *testing.T) {
		r := l.call(http.MethodGet, "/api/v1/admin/users/"+userID.String()+"/sessions?status=COMPLETED", "", "Authorization", adm)
		require.Equal(t, 200, r.code, string(r.body))
		data := r.json(t)["data"].([]any)
		require.Len(t, data, 1)
		require.Equal(t, sid.String(), data[0].(map[string]any)["id"])
		r = l.call(http.MethodGet, "/api/v1/admin/users/"+userID.String()+"/sessions?status=CANCELLED", "", "Authorization", adm)
		require.Empty(t, r.json(t)["data"])
	})

	t.Run("turns write exactly one audit row", func(t *testing.T) {
		r := l.call(http.MethodGet, "/api/v1/admin/sessions/"+sid.String()+"/turns", "", "Authorization", adm)
		require.Equal(t, 200, r.code, string(r.body))
		data := r.json(t)["data"].([]any)
		require.Len(t, data, 1)
		require.Equal(t, "hello", data[0].(map[string]any)["user_text"])
		require.Equal(t, 1, l.count(`SELECT count(*) FROM admin_audit_logs WHERE action = 'SESSION_TRANSCRIPT_VIEW' AND target_type = 'SESSION' AND target_id = $1 AND target_user_id = $2`, sid.String(), userID))
	})

	t.Run("events keyset pages the whole timeline without repeats", func(t *testing.T) {
		full := l.call(http.MethodGet, "/api/v1/admin/sessions/"+sid.String()+"/events", "", "Authorization", adm)
		require.Equal(t, 200, full.code, string(full.body))
		all := full.json(t)["data"].([]any)
		require.Len(t, all, len(l.eventTypes(sid)))

		var got []string
		cursor := ""
		for range 10 {
			path := "/api/v1/admin/sessions/" + sid.String() + "/events?limit=2"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			p := l.call(http.MethodGet, path, "", "Authorization", adm)
			require.Equal(t, 200, p.code, string(p.body))
			for _, e := range p.json(t)["data"].([]any) {
				got = append(got, e.(map[string]any)["event_type"].(string))
			}
			next, _ := p.json(t)["next_cursor"].(string)
			if next == "" {
				break
			}
			cursor = next
		}
		require.Equal(t, l.eventTypes(sid), got)
	})

	t.Run("purged transcript is 410 and writes no audit", func(t *testing.T) {
		before := l.count(`SELECT count(*) FROM admin_audit_logs WHERE action = 'SESSION_TRANSCRIPT_VIEW'`)
		_, err := l.pool.Exec(ctx, `UPDATE sessions SET transcript_purged_at = now() WHERE id = $1`, sid)
		require.NoError(t, err)
		r := l.call(http.MethodGet, "/api/v1/admin/sessions/"+sid.String()+"/turns", "", "Authorization", adm)
		require.Equal(t, 410, r.code)
		require.Equal(t, "TRANSCRIPT_EXPIRED", r.errCode(t))
		require.Equal(t, before, l.count(`SELECT count(*) FROM admin_audit_logs WHERE action = 'SESSION_TRANSCRIPT_VIEW'`))
	})
}
