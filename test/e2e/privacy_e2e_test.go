//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// Privacy end to end (spec 13.9, plan M10 Task 14): account deletion through the REST API and the
// account_purge job on the real binaries, and a scan of everything the subprocesses logged and
// exposed as metrics after real sessions.

const privacyVersion = "2026-10-01" // PRIVACY_POLICY_VERSION of MinimalEnv

// ---- running a worker job -------------------------------------------------------------------------------------------

var (
	jobLogMu    sync.Mutex
	jobLogPaths []string
)

// runWorkerJob runs `worker run-job <name>` as a subprocess against the shared stack with
// ACCOUNT_PURGE_DELAY=1s. Its log (the logger writes to stderr, the result to stdout) is kept for
// the log scans. It returns the log of this run.
func runWorkerJob(t *testing.T, name string) string {
	t.Helper()
	stack.workerMu.Lock()
	if stack.workerBin == "" {
		stack.workerBin = stack.build(t, "worker")
	}
	bin := stack.workerBin
	stack.workerMu.Unlock()

	jobLogMu.Lock()
	path := filepath.Join(stack.logDir, fmt.Sprintf("job-%s-%d.log", name, len(jobLogPaths)))
	jobLogPaths = append(jobLogPaths, path)
	jobLogMu.Unlock()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run-job", name)
	cmd.Env = stack.baseEnv(t, map[string]string{
		"WORKER_HTTP_ADDR": testutil.FreeAddr(t), "WORKER_HANDLERS": "cleanup",
		"RABBITMQ_URL": "amqp://guest:guest@127.0.0.1:5672/", "ACCOUNT_PURGE_DELAY": "1s",
	})
	cmd.Stdout, cmd.Stderr = f, f
	require.NoError(t, cmd.Run(), "worker run-job %s\n%s", name, readFile(t, path))
	out := readFile(t, path)
	require.Contains(t, out, "job="+name+" ran=true", "the job ran (it was not skipped for a held lock)")
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// ---- scanning logs and metrics for PII -------------------------------------------------------------------------------

// piiNeedle is a value that must not appear anywhere the servers write.
type piiNeedle struct{ Label, Value string }

var (
	reJWT    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	reBearer = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	reUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
)

// scanForPII returns one finding per (log, line, reason): a line holding a needle (as written, or
// as JSON would escape it), a JWT, a bearer credential or an e-mail address. The finding holds the
// line so a failure can be diagnosed; callers pass dummy values only.
func scanForPII(logs map[string][]byte, needles []piiNeedle) []string {
	var out []string
	for name, content := range logs {
		for i, line := range strings.Split(string(content), "\n") {
			report := func(reason string) {
				out = append(out, fmt.Sprintf("%s:%d: %s: %s", name, i+1, reason, truncate(line, 240)))
			}
			for _, n := range needles {
				if n.Value == "" {
					continue
				}
				if strings.Contains(line, n.Value) || strings.Contains(line, jsonEscaped(n.Value)) {
					report("contains " + n.Label)
				}
			}
			if reJWT.MatchString(line) {
				report("contains a JWT")
			}
			if reBearer.MatchString(line) {
				report("contains a bearer credential")
			}
			if reEmail.MatchString(line) {
				report("contains an e-mail address")
			}
		}
	}
	return out
}

func jsonEscaped(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// processLogs reads the log of every subprocess of the suite so far (api, realtime pods, workers)
// and of every job run.
func processLogs(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	stack.procMu.Lock()
	for _, p := range stack.procs {
		b, err := os.ReadFile(p.logPath)
		require.NoError(t, err)
		out[p.name] = b
	}
	stack.procMu.Unlock()
	jobLogMu.Lock()
	defer jobLogMu.Unlock()
	for _, p := range jobLogPaths {
		out[filepath.Base(p)] = []byte(readFile(t, p))
	}
	return out
}

// textsOfUser returns what the user said and what the AI answered, the summaries and the
// memories of the user's sessions: the conversation text that must never reach a log.
func textsOfUser(t *testing.T, userID uuid.UUID) []piiNeedle {
	t.Helper()
	var out []piiNeedle
	add := func(label string, v *string) {
		if v != nil && len([]rune(*v)) >= 5 {
			out = append(out, piiNeedle{label, *v})
		}
	}
	rows, err := stack.pool.Query(t.Context(), `SELECT t.user_text, t.assistant_text, t.assistant_spoken_text
		FROM session_turns t JOIN sessions s ON s.id = t.session_id WHERE s.user_id = $1`, userID)
	require.NoError(t, err)
	for rows.Next() {
		var u, a, sp *string
		require.NoError(t, rows.Scan(&u, &a, &sp))
		add("user_text", u)
		add("assistant_text", a)
		add("assistant_spoken_text", sp)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	rows, err = stack.pool.Query(t.Context(), `SELECT summary_text FROM sessions WHERE user_id = $1`, userID)
	require.NoError(t, err)
	for rows.Next() {
		var s *string
		require.NoError(t, rows.Scan(&s))
		add("summary_text", s)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	rows, err = stack.pool.Query(t.Context(), `SELECT content, display_text FROM memories WHERE user_id = $1`, userID)
	require.NoError(t, err)
	for rows.Next() {
		var c, d *string
		require.NoError(t, rows.Scan(&c, &d))
		add("memory content", c)
		add("memory display_text", d)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	return out
}

// forbiddenLabels are label names that identify a person or a session; a metric must never have one.
var forbiddenLabels = map[string]bool{
	"user_id": true, "session_id": true, "email": true, "ip": true, "client_ip": true, "token": true,
	"ticket": true, "user": true, "phone": true, "full_name": true, "nickname": true, "trace_id": true, "request_id": true,
}

// metricsPII returns the problems of a scrape: forbidden label names, label values that are UUIDs
// or contain an at sign, and any needle in the raw exposition.
func metricsPII(raw string, needles []piiNeedle) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		open := strings.IndexByte(line, '{')
		if open >= 0 {
			for _, l := range splitLabels(line[open+1 : strings.LastIndexByte(line, '}')]) {
				name, val, _ := strings.Cut(l, "=")
				val = strings.Trim(val, `"`)
				switch {
				case forbiddenLabels[name]:
					out = append(out, "label "+name+" on "+truncate(line, 160))
				case reUUID.MatchString(val):
					out = append(out, "uuid in label "+name+" on "+truncate(line, 160))
				case strings.Contains(val, "@"):
					out = append(out, "at sign in label "+name+" on "+truncate(line, 160))
				}
			}
		}
		for _, n := range needles {
			if n.Value != "" && strings.Contains(line, n.Value) {
				out = append(out, n.Label+" in "+truncate(line, 160))
			}
		}
	}
	return out
}

func rawMetrics(t *testing.T, opsAddr string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctxT(t, 10*time.Second), http.MethodGet, "http://"+opsAddr+"/metrics", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var sb strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// TestPrivacy_Meta_DetectsLeak: the scanners are not vacuous. A clean log passes; each kind of leak,
// injected into an otherwise clean log, is found.
func TestPrivacy_Meta_DetectsLeak(t *testing.T) {
	email := "meta-" + uuid.NewString() + "@example.com"
	needles := []piiNeedle{{"email", email}, {"user_text", "これはテストの発話です。<こんにちは>&"}, {"password", "Sup3r-S3cret-Pass!"}}
	clean := `{"time":"2026-10-17T00:00:00Z","level":"INFO","msg":"http request","route":"/api/v1/auth/login","status":401,"email_hash":"ab12cd34","request_id":"` + uuid.NewString() + `"}`
	require.Empty(t, scanForPII(map[string][]byte{"api": []byte(clean + "\n" + clean)}, needles), "a clean log has no findings")

	for name, leak := range map[string]string{
		"email":         `{"msg":"login failed","email":"` + email + `"}`,
		"other email":   `{"msg":"login failed","email":"somebody.else@mail.test"}`,
		"user text":     `{"msg":"turn","text":"` + jsonEscaped("これはテストの発話です。<こんにちは>&") + `"}`,
		"user text raw": "turn: これはテストの発話です。<こんにちは>&",
		"password":      `{"msg":"x","detail":"password was Sup3r-S3cret-Pass!"}`,
		"jwt":           `{"msg":"x","t":"ey` + `JhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eHh4In0.c2lnbmF0dXJlc2lnbmF0dXJl"}`,
		"bearer":        `Authorization: Bearer abcdef0123456789`,
	} {
		got := scanForPII(map[string][]byte{"api": []byte(clean + "\n" + leak + "\n" + clean)}, needles)
		assert.NotEmpty(t, got, "a leak of kind %q must be found", name)
		for _, f := range got {
			assert.Contains(t, f, "api:2:", "the finding names the log and the line")
		}
	}

	// the metrics scanner: forbidden label, UUID value, at sign, needle
	assert.Empty(t, metricsPII("# HELP m x\nm_total{route=\"/api/v1/sessions/{session_id}\",status=\"200\"} 3\nup 1\n", needles))
	assert.NotEmpty(t, metricsPII(`m_total{user_id="x"} 1`, needles))
	assert.NotEmpty(t, metricsPII(`m_total{route="`+uuid.NewString()+`"} 1`, needles))
	assert.NotEmpty(t, metricsPII(`m_total{who="`+email+`"} 1`, needles))
	assert.NotEmpty(t, metricsPII(`m_total{who="x"} 1`+"\nm_info 1 # "+email, needles))
}

// ---- account deletion -----------------------------------------------------------------------------------------------

// deleteMe sends DELETE /api/v1/me as u.
func deleteMe(t *testing.T, u *user, password string) secResp {
	t.Helper()
	body, err := json.Marshal(map[string]string{"password": password})
	require.NoError(t, err)
	return secDo(t, http.MethodDelete, "/api/v1/me", jsonHeaders("Authorization", "Bearer "+u.Client.AccessToken()), strings.NewReader(string(body)))
}

// dataCounts counts the rows of the tables the purge must empty.
func dataCounts(t *testing.T, id uuid.UUID) map[string]int {
	t.Helper()
	q := map[string]string{
		"memories":              `SELECT count(*) FROM memories WHERE user_id = $1`,
		"evaluation_findings":   `SELECT count(*) FROM evaluation_findings WHERE user_id = $1`,
		"evaluations":           `SELECT count(*) FROM evaluations WHERE user_id = $1`,
		"proficiency_estimates": `SELECT count(*) FROM proficiency_estimates WHERE user_id = $1`,
		"learner_skill_states":  `SELECT count(*) FROM learner_skill_states WHERE user_id = $1`,
		"learner_profiles":      `SELECT count(*) FROM learner_profiles WHERE user_id = $1`,
		"session_turns":         `SELECT count(*) FROM session_turns t JOIN sessions s ON s.id = t.session_id WHERE s.user_id = $1`,
		"session_events":        `SELECT count(*) FROM session_events e JOIN sessions s ON s.id = e.session_id WHERE s.user_id = $1`,
		"sessions":              `SELECT count(*) FROM sessions WHERE user_id = $1`,
		"sessions_with_summary": `SELECT count(*) FROM sessions WHERE user_id = $1 AND summary_text IS NOT NULL`,
		"balance_ledger":        `SELECT count(*) FROM balance_ledger WHERE user_id = $1`,
		"balance_reservations":  `SELECT count(*) FROM balance_reservations WHERE user_id = $1`,
		"balance_accounts":      `SELECT count(*) FROM balance_accounts WHERE user_id = $1`,
		"refresh_tokens":        `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`,
		"password_reset_tokens": `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`,
	}
	out := map[string]int{}
	for k, sql := range q {
		var n int
		require.NoError(t, stack.pool.QueryRow(t.Context(), sql, id).Scan(&n), k)
		out[k] = n
	}
	return out
}

type userRow struct {
	Email, FullName, PasswordHash, Status string
	Phone                                 *string
	DeletedAt, PurgedAt                   *time.Time
}

func readUserRow(t *testing.T, id uuid.UUID) userRow {
	t.Helper()
	var r userRow
	require.NoError(t, stack.pool.QueryRow(t.Context(),
		`SELECT email, full_name, password_hash, status, phone_number, deleted_at, purged_at FROM users WHERE id = $1`, id).Scan(
		&r.Email, &r.FullName, &r.PasswordHash, &r.Status, &r.Phone, &r.DeletedAt, &r.PurgedAt))
	return r
}

func apiErrorOf(t *testing.T, err error) *kotokiclient.APIError {
	t.Helper()
	var ae *kotokiclient.APIError
	require.True(t, errors.As(err, &ae), "want a REST error, got %v", err)
	return ae
}

func TestPrivacy_AccountDeletion_EndToEnd(t *testing.T) {
	// A finished conversation whose evaluation, memories and skill profile exist...
	u, e, s1 := firstSession(t, fullHandlers, m8Long)
	eventually(t, func() bool { return pipelineDone(t, u, s1) }, "evaluation, memory and learner finished")
	eventually(t, func() bool { return dataCounts(t, u.ID)["sessions_with_summary"] == 1 }, "summary stored")
	full := dataCounts(t, u.ID)
	for _, k := range []string{"memories", "evaluations", "proficiency_estimates", "learner_skill_states", "learner_profiles", "session_turns", "session_events", "balance_ledger", "balance_accounts", "refresh_tokens"} {
		require.Positive(t, full[k], "the scenario has %s to purge", k)
	}
	t.Logf("before deletion: %v", full)

	// ... and a second session that is RESERVED.
	s2 := e.create(t, u)

	// (1) wrong password
	r := deleteMe(t, u, "Not-the-password-0!")
	require.Equal(t, http.StatusUnauthorized, r.Status, string(r.Body))
	assert.Equal(t, "INVALID_CREDENTIALS", r.errorObject(t)["code"])
	assert.Equal(t, "ACTIVE", readUserRow(t, u.ID).Status)

	// (2) a session is ACTIVE: the second session is played (4 s), the deletion is attempted meanwhile
	done := make(chan error, 1)
	go func() {
		_, err := converse(t, u, s2, []kotokiclient.Step{kotokiclient.Think{D: 4 * time.Second}, kotokiclient.End{}}, nil)
		done <- err
	}()
	eventually(t, func() bool { return readSession(t, s2.ID).Status == "ACTIVE" }, "session 2 active")
	r = deleteMe(t, u, strongPassword)
	require.Equal(t, http.StatusConflict, r.Status, string(r.Body))
	assert.Equal(t, "ACCOUNT_HAS_ACTIVE_SESSION", r.errorObject(t)["code"])
	assert.Equal(t, "ACTIVE", readUserRow(t, u.ID).Status, "a refused deletion changes nothing")
	assert.Positive(t, dataCounts(t, u.ID)["refresh_tokens"])
	require.NoError(t, <-done)
	waitSession(t, s2.ID, "COMPLETED")
	e.worker.stop() // no consumer is half way through session 2 while the account goes away

	// (3) the session is over; a third one is RESERVED; the deletion goes through
	s3 := e.create(t, u)
	preDelete := dataCounts(t, u.ID)
	r = deleteMe(t, u, strongPassword)
	require.Equal(t, http.StatusAccepted, r.Status, string(r.Body))
	var accepted map[string]string
	require.NoError(t, json.Unmarshal(r.Body, &accepted))
	assert.Equal(t, map[string]string{"status": "DELETION_SCHEDULED"}, accepted)
	var cleared bool
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == "kotoki_refresh" {
			cleared = c.Value == "" && c.MaxAge < 0 && c.Path == "/api/v1/auth"
		}
	}
	assert.True(t, cleared, "the refresh cookie is cleared: %v", r.Header.Values("Set-Cookie"))
	assert.Equal(t, "no-store", r.Header.Get("Cache-Control"))

	got := readSession(t, s3.ID)
	assert.Equal(t, "CANCELLED", got.Status)
	assert.Equal(t, "USER_CANCELLED", str(got.EndReason))
	assert.Equal(t, []ledgerRow{{"RESERVE", -300, 300}, {"RELEASE", 300, -300}}, ledgerRows(t, s3.ID), "the reservation is released")
	testutil.AssertReconciled(t, stack.pool)

	// (4) the account is closed
	ae := apiErrorOf(t, u.Client.Login(ctxT(t, 15*time.Second), u.Email, strongPassword))
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, ae.Status, "login: %v", ae)
	assert.NotEmpty(t, ae.Code)
	c := stack.choose(t, u)
	_, err := u.Client.CreateSession(ctxT(t, 15*time.Second), kotokiclient.CreateSessionRequest{
		TopicID: c.topic, ScenarioID: c.scenario, CharacterID: c.character, DurationSeconds: 300, ConversationStartMode: "USER_FIRST",
	})
	ae = apiErrorOf(t, err)
	assert.Equal(t, http.StatusForbidden, ae.Status)
	assert.Equal(t, "ACCOUNT_SUSPENDED", ae.Code, "POST /sessions of a deleted account")
	_, err = u.Client.RealtimeTicket(ctxT(t, 15*time.Second), s2.ID)
	ae = apiErrorOf(t, err)
	assert.Equal(t, http.StatusForbidden, ae.Status)
	assert.Equal(t, "ACCOUNT_SUSPENDED", ae.Code, "realtime-ticket of a deleted account")
	clearRateLimit(t, "account_delete_user")
	assert.Equal(t, http.StatusForbidden, deleteMe(t, u, strongPassword).Status, "a second deletion is refused, not repeated")

	after := dataCounts(t, u.ID)
	assert.Zero(t, after["refresh_tokens"])
	assert.Zero(t, after["password_reset_tokens"])
	row := readUserRow(t, u.ID)
	assert.Equal(t, "DELETED", row.Status)
	require.NotNil(t, row.DeletedAt)
	assert.Nil(t, row.PurgedAt)
	assert.Equal(t, u.Email, row.Email, "nothing is anonymized before the purge")
	for _, k := range []string{"memories", "evaluations", "proficiency_estimates", "learner_skill_states", "learner_profiles", "session_turns"} {
		assert.Equal(t, preDelete[k], after[k], "%s is still there until the purge job runs", k)
	}
	assert.Equal(t, preDelete["session_events"]+1, after["session_events"], "the cancellation of session 3 is the only new event")
	beforePurge := after
	assert.Equal(t, 3, beforePurge["sessions"])

	// (5) the purge job; it only takes accounts older than ACCOUNT_PURGE_DELAY (1 s)
	var firstRun string
	require.Eventually(t, func() bool {
		firstRun += runWorkerJob(t, "account_purge")
		return readUserRow(t, u.ID).PurgedAt != nil
	}, eventuallyWait, 200*time.Millisecond, "account_purge purges the account")

	// (6) what is gone, what stays
	row = readUserRow(t, u.ID)
	assert.Equal(t, "deleted+"+u.ID.String()+"@deleted.invalid", row.Email)
	assert.Equal(t, "deleted", row.FullName)
	assert.Nil(t, row.Phone)
	assert.Equal(t, "!", row.PasswordHash)
	assert.Equal(t, "DELETED", row.Status)
	require.NotNil(t, row.PurgedAt)
	purged := dataCounts(t, u.ID)
	for _, k := range []string{"memories", "evaluation_findings", "evaluations", "proficiency_estimates", "learner_skill_states", "learner_profiles", "session_turns", "session_events", "sessions_with_summary", "refresh_tokens", "password_reset_tokens"} {
		assert.Zero(t, purged[k], "%s of the user is purged", k)
	}
	assert.Equal(t, beforePurge["sessions"], purged["sessions"], "sessions are kept (ledger and reservations point at them)")
	assert.Equal(t, beforePurge["balance_ledger"], purged["balance_ledger"], "the ledger is a financial record")
	assert.Equal(t, beforePurge["balance_reservations"], purged["balance_reservations"])
	assert.Equal(t, beforePurge["balance_accounts"], purged["balance_accounts"])
	testutil.AssertReconciled(t, stack.pool)
	// the old e-mail address is free again and not recoverable from the user row
	var n int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email = $1 OR full_name = 'E2E User' AND id = $2`, u.Email, u.ID).Scan(&n))
	assert.Zero(t, n)

	// the purge is audited by user id only
	var audited []map[string]any
	for _, line := range strings.Split(firstRun, "\n") {
		if !strings.Contains(line, `"account_purged"`) {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		if m["user_id"] == u.ID.String() {
			audited = append(audited, m)
		}
	}
	require.Len(t, audited, 1, "one account_purged line for the user in the job log:\n%s", firstRun)
	assert.Equal(t, "account_purged", audited[0]["audit_event"])
	assert.Equal(t, true, audited[0]["audit"])
	assert.NotContains(t, fmt.Sprint(audited[0]), "@", "no e-mail address in the purge audit")
	assert.NotContains(t, firstRun, u.Email)

	// (7) idempotent: a second run changes nothing and logs nothing about this user
	purgedAt := *row.PurgedAt
	second := runWorkerJob(t, "account_purge")
	assert.NotContains(t, second, u.ID.String(), "the user is not picked up again")
	assert.Equal(t, purged, dataCounts(t, u.ID))
	again := readUserRow(t, u.ID)
	assert.Equal(t, row.Email, again.Email)
	assert.True(t, purgedAt.Equal(*again.PurgedAt), "purged_at does not move")

	// nothing left of the user's text in the outbox payloads either
	var payloads string
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(payload::text, E'\n'), '') FROM event_outbox WHERE user_id = $1`, u.ID).Scan(&payloads))
	assert.NotContains(t, payloads, u.Email)
}

// ---- log and metric hygiene -----------------------------------------------------------------------------------------

func TestPrivacy_LogsContainNoPII(t *testing.T) {
	const nickname = "Budi-Santoso-Privacy-Check"
	u, e, s1 := firstSession(t, fullHandlers, m8Long)
	code, body := api(t, u, http.MethodPatch, "/profile", `{"nickname":"`+nickname+`"}`)
	require.Equal(t, http.StatusOK, code, "%v", body)
	eventually(t, func() bool { return pipelineDone(t, u, s1) }, "evaluation, memory and learner finished")

	// a second session of which we hold the ticket: a valid WebSocket handshake with it
	s2 := e.create(t, u)
	ticket, err := u.Client.RealtimeTicket(ctxT(t, 10*time.Second), s2.ID)
	require.NoError(t, err)
	wctx := ctxT(t, 10*time.Second)
	ws, _, err := websocket.Dial(wctx, "ws://"+stack.rt.addr+"/api/v1/realtime?ticket="+ticket, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {testOrigin}}, Subprotocols: []string{"kotoki.v1"},
	})
	require.NoError(t, err)
	_ = ws.CloseNow()

	// requests that put secrets in places a careless server logs: a wrong password, a refresh
	// cookie that is not a token, a bearer token that is not a JWT, a ticket on a refused handshake
	ghost := "ghost-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "@example.com"
	const ghostPassword = "Gh0st-Wrong-Passw0rd!z"
	badRefresh := "refresh-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	badBearer := "bearer-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	badTicket := "ticket-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	clearLoginLimit(t)
	secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(`{"email":"`+ghost+`","password":"`+ghostPassword+`"}`))
	secDo(t, http.MethodPost, "/api/v1/auth/login", jsonHeaders(), strings.NewReader(`{"email":"`+u.Email+`","password":"`+ghostPassword+`"}`))
	secDo(t, http.MethodPost, "/api/v1/auth/refresh", map[string]string{"Origin": testOrigin, "Cookie": "kotoki_refresh=" + badRefresh}, nil)
	secDo(t, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer " + badBearer}, nil)
	secDo(t, http.MethodGet, "/api/v1/sessions?ticket="+badTicket, map[string]string{"Authorization": "Bearer " + u.Client.AccessToken()}, nil)
	if req, err := http.NewRequestWithContext(ctxT(t, 10*time.Second), http.MethodGet, "http://"+stack.rt.addr+"/api/v1/realtime?ticket="+badTicket, http.NoBody); err == nil {
		req.Header.Set("Origin", "http://evil.test")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	// a purge job run too: the worker logs of the cleanup path are part of the sample
	runWorkerJob(t, "account_purge")

	needles := []piiNeedle{
		{"email of the user", u.Email}, {"local part of the e-mail", strings.TrimSuffix(u.Email, "@example.com")},
		{"password", strongPassword}, {"password of a failed login", ghostPassword}, {"e-mail of a failed login", ghost},
		{"access token", u.Client.AccessToken()}, {"refresh token", u.Client.RefreshToken()},
		{"refresh cookie value", badRefresh}, {"bearer value", badBearer}, {"ticket (valid)", ticket}, {"ticket (refused)", badTicket},
		{"full name", "E2E User"}, {"nickname", nickname},
	}
	needles = append(needles, textsOfUser(t, u.ID)...)
	require.Greater(t, len(textsOfUser(t, u.ID)), 4, "the scenario produced conversation text to look for")
	if fixture, err := os.ReadFile(filepath.Join(stack.root, "testdata", "audio", "synth", "ja_self_intro.txt")); err == nil {
		needles = append(needles, piiNeedle{"ja_self_intro.txt", strings.TrimSpace(string(fixture))})
	}
	require.NotEmpty(t, u.Client.RefreshToken(), "the client holds a refresh token to look for")

	logs := processLogs(t)
	var seen int
	for _, b := range logs {
		seen += len(b)
	}
	require.Greater(t, seen, 1000, "the processes logged something")
	assert.Contains(t, string(logs["api"]), `"audit_event":"login_failed"`, "the sample includes the audit lines")
	assert.Empty(t, scanForPII(logs, needles), "server logs hold PII, a token or conversation text")

	// metrics of the three binaries
	for name, ops := range map[string]string{"api": stack.api.opsAddr, "realtime": stack.rt.opsAddr, "worker": e.worker.opsAddr} {
		assert.Empty(t, metricsPII(rawMetrics(t, ops), append(needles, piiNeedle{"user id", u.ID.String()}, piiNeedle{"session id", s1.ID.String()})), "/metrics of %s", name)
	}
}

// ---- terms gate -----------------------------------------------------------------------------------------------------

func TestPrivacy_TermsGate(t *testing.T) {
	r := secDo(t, http.MethodGet, "/api/v1/auth/terms", nil, nil)
	require.Equal(t, http.StatusOK, r.Status)
	var terms map[string]string
	require.NoError(t, json.Unmarshal(r.Body, &terms))
	assert.Equal(t, map[string]string{"terms_version": privacyVersion}, terms, "GET /auth/terms is PRIVACY_POLICY_VERSION")

	// registration with another version is refused, naming the field
	clearRateLimit(t, "auth_register_ip")
	email := fmt.Sprintf("terms-%s@example.com", strings.ReplaceAll(uuid.NewString(), "-", ""))
	register := func(version string) secResp {
		body, err := json.Marshal(map[string]string{"email": email, "password": strongPassword, "full_name": "Terms User", "terms_version": version})
		require.NoError(t, err)
		return secDo(t, http.MethodPost, "/api/v1/auth/register", jsonHeaders(), strings.NewReader(string(body)))
	}
	for _, v := range []string{"2025-01-01", "", "2026-10-02", privacyVersion + " "} {
		r = register(v)
		require.Equal(t, http.StatusBadRequest, r.Status, "terms_version %q: %s", v, r.Body)
		eo := r.errorObject(t)
		assert.Equal(t, "VALIDATION_FAILED", eo["code"], v)
		fields, _ := eo["details"].(map[string]any)["fields"].([]any)
		require.NotEmpty(t, fields, "details name the field: %v", eo["details"])
		assert.Equal(t, "terms_version", fields[0].(map[string]any)["field"], v)
	}
	var n int
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&n))
	assert.Zero(t, n, "a refused registration creates no user")

	// the current version registers, and the acceptance is recorded
	r = register(privacyVersion)
	require.Equal(t, http.StatusCreated, r.Status, string(r.Body))
	var version string
	var acceptedAt *time.Time
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT terms_version, terms_accepted_at FROM users WHERE email = $1`, email).Scan(&version, &acceptedAt))
	assert.Equal(t, privacyVersion, version)
	require.NotNil(t, acceptedAt)

	// a user on an older version cannot start a session until the new terms are accepted
	u := newUser(t)
	_, err := stack.pool.Exec(t.Context(), `UPDATE users SET terms_version = '2025-01-01' WHERE id = $1`, u.ID)
	require.NoError(t, err)
	c := stack.choose(t, u)
	req := kotokiclient.CreateSessionRequest{TopicID: c.topic, ScenarioID: c.scenario, CharacterID: c.character, DurationSeconds: 300, ConversationStartMode: "USER_FIRST"}
	_, err = u.Client.CreateSession(ctxT(t, 15*time.Second), req)
	ae := apiErrorOf(t, err)
	assert.Equal(t, http.StatusForbidden, ae.Status)
	assert.Equal(t, "TERMS_NOT_ACCEPTED", ae.Code)
	code, _ := api(t, u, http.MethodGet, "/me", "")
	assert.Equal(t, http.StatusOK, code, "other endpoints keep working")
	code, body := api(t, u, http.MethodPut, "/me/consents", `{"terms_version":"`+privacyVersion+`"}`)
	require.Equal(t, http.StatusOK, code, "%v", body)
	assert.Equal(t, privacyVersion, body["terms_version"])
	s, err := u.Client.CreateSession(ctxT(t, 15*time.Second), req)
	require.NoError(t, err)
	assert.Equal(t, "RESERVED", s.Status)
}
