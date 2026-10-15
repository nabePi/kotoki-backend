//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// sumSeries adds up every series of a metric (whatever its labels) in a scrape.
func sumSeries(m map[string]float64, name string) float64 {
	var sum float64
	for series, v := range m {
		if series == name || strings.HasPrefix(series, name+"{") {
			sum += v
		}
	}
	return sum
}

// TestE7STTFailureEndsSession is E7 (spec 14.5, D-017): the STT hangs and there is no second vendor
// to move to. Each utterance ends without a FINAL (STT_FINAL_TIMEOUT); the third such timeout in a
// row fails the vendor (provider.finalTimeoutLimit), and the session fails with PROVIDER_FAILURE
// (close 4500). With the fault gone, a new session works. Faults are global to the Redis: this test
// never runs in parallel.
func TestE7STTFailureEndsSession(t *testing.T) {
	setFaults(t, `{"STT.FAKE_PRIMARY":{"mode":"timeout","rate":1.0}}`)
	u := newUser(t) // registering takes longer than a fault poll (100 ms): the pod has the fault by now
	s := startSession(t, u, "USER_FIRST")
	rec := newRecorder()
	var steps []kotokiclient.Step
	for i := 0; i < 3; i++ {
		steps = append(steps, kotokiclient.Speak{Packets: packets(t, "ja_question"), Label: "ja_question"}, kotokiclient.WaitAIDone{Timeout: 30 * time.Second})
	}
	rep, err := converse(t, u, s, append(steps, kotokiclient.End{}), rec)
	if err != nil { // the fatal error event may be seen before session.failed
		var se *kotokiclient.ServerError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, "PROVIDER_FAILURE", se.Code)
	}
	assert.Equal(t, "FAILED", rep.Status)
	assert.Equal(t, 4500, rep.CloseCode)
	assert.Contains(t, rec.types(), "session.failed")
	for _, typ := range rec.types() {
		assert.NotContains(t, typ, "provider", "no vendor switch is announced (D-017): %s", typ)
	}
	eventually(t, func() bool {
		for _, r := range turnRows(t, s.ID) {
			if r.Status == nil {
				return false
			}
		}
		return true
	}, "turns closed")
	for _, r := range turnRows(t, s.ID) {
		assert.NotEqual(t, "COMPLETED", str(r.Status), "an utterance the STT never transcribed is not answered")
	}

	got := waitSession(t, s.ID, "FAILED")
	assert.Equal(t, "PROVIDER_FAILURE", str(got.EndReason))
	assert.Empty(t, sessionEvents(t, s.ID)["PROVIDER_FALLBACK"], "no vendor was switched")
	bal, err := u.Client.Balance(t.Context())
	require.NoError(t, err)
	assert.Zero(t, bal.ReservedSeconds, "the reservation is settled")

	// Fault removed: a new session is normal.
	clearFaults(t)
	u2 := newUser(t) // again longer than a poll
	s2 := startSession(t, u2, "USER_FIRST")
	rep2, err := converse(t, u2, s2, []kotokiclient.Step{
		kotokiclient.Speak{Packets: packets(t, "ja_greeting_short"), Label: "ja_greeting_short"},
		kotokiclient.WaitAIDone{},
		kotokiclient.End{},
	}, newRecorder())
	require.NoError(t, err)
	assert.Equal(t, "COMPLETED", rep2.Status)
	eventually(t, func() bool { r := turnRows(t, s2.ID); return len(r) == 1 && r[0].Status != nil }, "turn persisted")
	assert.Equal(t, "COMPLETED", str(turnRows(t, s2.ID)[0].Status))
}

// retireOpeningBySQL retires the PUBLISHED OPENING prompt version the way the admin endpoint would
// if it allowed it (it does not, see TestE12), and publishes it again when the test ends.
func retireOpeningBySQL(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, stack.pool.QueryRow(t.Context(), `UPDATE prompt_versions SET status = 'RETIRED', retired_at = now()
		WHERE status = 'PUBLISHED' AND prompt_config_id = (SELECT id FROM prompt_configs WHERE key = 'OPENING') RETURNING id`).Scan(&id))
	t.Cleanup(func() {
		_, err := stack.pool.Exec(context.Background(), `UPDATE prompt_versions SET status = 'PUBLISHED', retired_at = NULL WHERE id = $1`, id)
		require.NoError(t, err, "publish the OPENING version again")
	})
	return id
}

// TestE12OpeningFallback is E12 (spec 14.5, S-2): with no PUBLISHED OPENING prompt an AI_FIRST
// session still opens, with the scenario's static opening_line. It does not run beside E2: both
// touch the one PUBLISHED OPENING version.
func TestE12OpeningFallback(t *testing.T) {
	u := newUser(t)

	// The admin endpoint refuses to retire a PUBLISHED prompt version (M3: publish a replacement
	// instead), so the retirement itself is done in SQL.
	_, err := stack.pool.Exec(t.Context(), `UPDATE users SET role = 'ADMIN' WHERE id = $1`, u.ID)
	require.NoError(t, err)
	require.NoError(t, u.Client.Login(ctxT(t, 30*time.Second), u.Email, strongPassword), "log in again to carry the role")
	var openingVersion uuid.UUID
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT id FROM prompt_versions WHERE status = 'PUBLISHED'
		AND prompt_config_id = (SELECT id FROM prompt_configs WHERE key = 'OPENING')`).Scan(&openingVersion))
	req, err := http.NewRequestWithContext(ctxT(t, 10*time.Second), http.MethodPost,
		stack.apiURL+"/api/v1/admin/content/prompts/OPENING/versions/"+openingVersion.String()+"/retire", bytes.NewReader(nil))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+u.Client.AccessToken())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	_ = resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, "CONTENT_VERSION_REQUIRED", body.Error.Code)

	retired := retireOpeningBySQL(t)
	require.Equal(t, openingVersion, retired)
	before := sumSeries(scrape(t, stack.rt.opsAddr), "kotoki_content_opening_fallback_total")

	s := startSession(t, u, "AI_FIRST")
	rec := newRecorder()
	rep, err := converse(t, u, s, []kotokiclient.Step{kotokiclient.WaitAIDone{}, kotokiclient.End{}}, rec)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status, "a missing OPENING prompt does not fail the session")
	gens := rec.of("ai.generation_started")
	require.NotEmpty(t, gens)
	assert.Positive(t, rec.frameCount(*gens[0].Generation), "audio of the static opening")

	got := waitSession(t, s.ID, "COMPLETED")
	assert.Nil(t, got.OpeningPromptVer, "opening_prompt_version_id is NULL without a PUBLISHED OPENING")
	var openingLine string
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT sv.opening_line FROM sessions s JOIN scenario_versions sv ON sv.id = s.scenario_version_id
		WHERE s.id = $1`, s.ID).Scan(&openingLine))
	eventually(t, func() bool { r := turnRows(t, s.ID); return len(r) > 0 && r[0].Status != nil }, "opening turn persisted")
	turn0 := turnRows(t, s.ID)[0]
	assert.Equal(t, 0, turn0.Seq)
	assert.Nil(t, turn0.UserText)
	assert.Equal(t, openingLine, str(turn0.AssistantText), "turn 0 says the static opening_line")
	assert.Equal(t, "COMPLETED", str(turn0.Status))
	after := sumSeries(scrape(t, stack.rt.opsAddr), "kotoki_content_opening_fallback_total")
	assert.Equal(t, before+1, after, "kotoki_content_opening_fallback_total")
}

// wallSeconds is how long the session was billable: from start to the start of its ending.
func wallSeconds(r sessionRow) float64 { return r.EndingAt.Sub(*r.StartedAt).Seconds() }

// e13Steps is five question/answer turns and the end.
func e13Steps(t *testing.T) []kotokiclient.Step {
	var steps []kotokiclient.Step
	for i := 0; i < 5; i++ {
		name := "ja_question"
		if i%2 == 1 {
			name = "ja_greeting_short"
		}
		steps = append(steps, kotokiclient.Speak{Packets: packets(t, name), Label: name}, kotokiclient.WaitAIDone{})
	}
	return append(steps, kotokiclient.End{})
}

// TestE13ContentFilter is E13 (spec 14.5, S-9): four turns whose LLM answer is CONTENT_FILTERED each
// end FAILED with the APOLOGY played, the session stays ACTIVE (these do not count toward
// MAX_CONSECUTIVE_FAILED_TURNS), the counter reads 4, and once the fault is gone the next turn
// completes. The charge does not depend on the failures.
func TestE13ContentFilter(t *testing.T) {
	// The control: the same five turns without a fault.
	uc := newUser(t)
	sc := startSession(t, uc, "USER_FIRST")
	repc, err := converse(t, uc, sc, e13Steps(t), newRecorder())
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", repc.Status)
	control := waitSession(t, sc.ID, "COMPLETED")
	for _, r := range turnRows(t, sc.ID) {
		require.Equal(t, "COMPLETED", str(r.Status))
	}

	before := scrape(t, stack.rt.opsAddr)
	setFaults(t, `{"LLM.FAKE_PRIMARY":{"mode":"error","kind":"CONTENT_FILTERED","rate":1.0}}`)
	u := newUser(t) // registering takes longer than a fault poll (100 ms)
	s := startSession(t, u, "USER_FIRST")
	rec := newRecorder()
	var done atomic.Int32
	sink := kotokiclient.SinkFuncs{
		Event: func(e kotokiclient.Received[protocol.ServerEvent]) {
			rec.OnEvent(e)
			// After the fourth answer the fault is removed; the next utterance takes longer than a
			// poll before its LLM request is made.
			if _, ok := e.Value.Payload.(protocol.TurnCompleted); ok && !e.Value.Replay && done.Add(1) == 4 {
				_ = stack.rdb.Del(t.Context(), faultsKey).Err()
			}
		},
		Audio: rec.OnAudio,
	}
	rep, err := converse(t, u, s, e13Steps(t), sink)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	eventually(t, func() bool {
		r := turnRows(t, s.ID)
		if len(r) != 5 {
			return false
		}
		for _, x := range r {
			if x.Status == nil {
				return false
			}
		}
		return true
	}, "five turns persisted")
	turns := turnRows(t, s.ID)
	for i := 0; i < 4; i++ {
		assert.Equal(t, "FAILED", str(turns[i].Status), "turn %d", i+1)
	}
	assert.Equal(t, "COMPLETED", str(turns[4].Status), "the turn after the fault was removed")

	// Every turn starts a REPLY generation (the four that failed too); an APOLOGY follows each failure.
	var apologies, replies int
	for _, g := range rec.of("ai.generation_started") {
		kind := g.Payload.(protocol.AIGenerationStarted).Kind
		switch kind {
		case "APOLOGY":
			apologies++
			assert.Positive(t, rec.frameCount(*g.Generation), "apology audio of generation %d", *g.Generation)
		case "REPLY":
			replies++
		}
	}
	assert.Equal(t, 4, apologies)
	assert.Equal(t, 5, replies)
	assert.Equal(t, 4.0, sumSeries(scrape(t, stack.rt.opsAddr), "kotoki_llm_content_filtered_total")-sumSeries(before, "kotoki_llm_content_filtered_total"),
		"kotoki_llm_content_filtered_total")

	// The session was never ended by the failures, and the charge is the same function of time as
	// in the control: the billable seconds from start to the user's end, with nothing taken off for
	// failed turns (real seconds differ between two runs, the relation to the wall clock does not).
	got := waitSession(t, s.ID, "COMPLETED")
	assert.Equal(t, "USER_ENDED", str(got.EndReason))
	for name, r := range map[string]sessionRow{"fault": got, "control": control} {
		assert.InDelta(t, wallSeconds(r), float64(r.Charged), 1.0, "%s: charged_seconds against start..ending", name)
	}
	t.Logf("E13 charged_seconds fault=%d (wall %.1fs) control=%d (wall %.1fs)", got.Charged, wallSeconds(got), control.Charged, wallSeconds(control))
}
