//go:build integration || e2e

package testutil

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/events"
)

// TurnSeed is one session_turns row of a finished session; Status defaults to COMPLETED and
// UserSeconds is the length of the user's speech (user_ended_at - user_started_at).
type TurnSeed struct {
	UserText, AssistantText string
	Status                  string
	UserSeconds             float64
}

// FinishedSessionOpts describes SeedFinishedSession. UserStatus defaults to ACTIVE and Status to
// COMPLETED (FAILED sessions get a started_at as well: they were ACTIVE before they failed).
// StartedSecondsAgo defaults to ActiveSeconds + 10. UserID, when set, makes the session belong to
// that existing user (UserStatus is then ignored) instead of a freshly seeded one.
type FinishedSessionOpts struct {
	UserID            uuid.UUID
	UserStatus        string
	Status            string
	Turns             []TurnSeed
	StartedSecondsAgo int
	ActiveSeconds     int
}

// FinishedSession is a terminal session plus the session.completed event that announces it.
type FinishedSession struct {
	ID, UserID uuid.UUID
	Envelope   events.Envelope
	Payload    events.SessionCompletedV1
	TurnIDs    []uuid.UUID
	Content    ContentIDs
}

// WithPayload returns the session with Payload changed by mut and Envelope rebuilt from it (same
// event id), e.g. to announce more user turns than the database holds.
func (f FinishedSession) WithPayload(t testing.TB, mut func(*events.SessionCompletedV1)) FinishedSession {
	t.Helper()
	mut(&f.Payload)
	raw, err := json.Marshal(f.Payload)
	if err != nil {
		t.Fatalf("testutil: marshal payload: %v", err)
	}
	f.Envelope.Payload = raw
	return f
}

// SeedFinishedSession seeds the catalog and prompts (ApplySeed), a user, a COMPLETED or FAILED
// session with its turns, and returns the session.completed envelope that describes it. Turns are
// spread evenly over the active window, numbered from 1.
func SeedFinishedSession(t testing.TB, pool *pgxpool.Pool, o FinishedSessionOpts) FinishedSession {
	t.Helper()
	ctx := context.Background()
	ApplySeed(t, pool)

	status, endReason := o.Status, "USER_ENDED"
	if status == "" {
		status = "COMPLETED"
	}
	if status == "FAILED" {
		endReason = "PROVIDER_FAILURE"
	}
	userID := o.UserID
	if userID == uuid.Nil {
		var userRow Row
		if o.UserStatus == "DELETED" {
			userRow = Row{"status": "DELETED", "deleted_at": time.Now().UTC()}
		} else if o.UserStatus != "" {
			userRow = Row{"status": o.UserStatus}
		}
		userID = SeedUser(t, pool, userRow)
	}
	c := seedFinishedContent(t, pool)

	ago := o.StartedSecondsAgo
	if ago == 0 {
		ago = o.ActiveSeconds + 10
	}
	startedAt := time.Now().UTC().Add(-time.Duration(ago) * time.Second).Truncate(time.Millisecond)
	endedAt := startedAt.Add(time.Duration(o.ActiveSeconds) * time.Second)
	charged := min(o.ActiveSeconds, 300)
	sessionRow := Row{
		"status": status, "end_reason": endReason,
		"starting_at": startedAt.Add(-2 * time.Second), "started_at": startedAt,
		"expires_at": startedAt.Add(300 * time.Second), "ending_at": endedAt, "ended_at": endedAt,
		"charged_seconds": charged, "adaptive_difficulty": []byte(`{"formality":"POLITE","level":2}`),
	}
	sessionID := SeedSession(t, pool, userID, c, sessionRow)

	step := time.Duration(o.ActiveSeconds) * time.Second / time.Duration(len(o.Turns)+1)
	ids := make([]uuid.UUID, 0, len(o.Turns))
	userTurns := 0
	for i, ts := range o.Turns {
		id := newID()
		ids = append(ids, id)
		turnStatus := ts.Status
		if turnStatus == "" {
			turnStatus = "COMPLETED"
		}
		created := startedAt.Add(time.Duration(i+1) * step)
		userEnd := created.Add(time.Duration(ts.UserSeconds * float64(time.Second)))
		MustInsert(ctx, t, pool, "session_turns", Row{
			"id": id, "session_id": sessionID, "sequence_number": i + 1, "status": turnStatus,
			"user_text": ts.UserText, "assistant_text": ts.AssistantText, "generation": 0,
			"user_started_at": created, "user_ended_at": userEnd, "created_at": created, "completed_at": userEnd,
		})
		if strings.TrimSpace(ts.UserText) != "" {
			userTurns++
		}
	}

	payload := events.SessionCompletedV1{
		SessionID: sessionID, FinalStatus: status, EndReason: endReason, ConversationStartMode: "USER_FIRST",
		TopicID: c.TopicID, ScenarioID: c.ScenarioID, ScenarioVersionID: c.ScenarioVersionID,
		CharacterID: c.CharacterID, PromptVersionID: c.PromptVersionID, DurationSeconds: 300,
		StartedAt: startedAt, EndedAt: endedAt, ActiveSeconds: o.ActiveSeconds, ChargedSeconds: charged,
		TurnCount: len(o.Turns), UserTurnCount: userTurns,
	}
	ev := events.NewSessionCompleted(newID(), endedAt, events.ProducerRealtime, userID, payload)
	env, err := ev.Envelope("", nil)
	if err != nil {
		t.Fatalf("testutil: envelope: %v", err)
	}
	return FinishedSession{ID: sessionID, UserID: userID, Envelope: env, Payload: payload, TurnIDs: ids, Content: c}
}

// seedFinishedContent adds a topic, scenario, published version and character beside the seeded
// prompts and points PromptVersionID at the seeded CONVERSATION version 1.
func seedFinishedContent(t testing.TB, pool *pgxpool.Pool) ContentIDs {
	t.Helper()
	ctx := context.Background()
	topic := TopicRow(nil)
	MustInsert(ctx, t, pool, "topics", topic)
	sc := ScenarioRow(topic["id"].(uuid.UUID), nil)
	MustInsert(ctx, t, pool, "scenarios", sc)
	sv := ScenarioVersionRow(sc["id"].(uuid.UUID), 1, Row{"status": "PUBLISHED", "published_at": time.Now().UTC()})
	MustInsert(ctx, t, pool, "scenario_versions", sv)
	ch := CharacterRow(nil)
	MustInsert(ctx, t, pool, "characters", ch)
	var pv uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT pv.id FROM prompt_versions pv JOIN prompt_configs pc ON pc.id = pv.prompt_config_id
		WHERE pc.key = 'CONVERSATION' AND pv.status = 'PUBLISHED'`).Scan(&pv); err != nil {
		t.Fatalf("testutil: seeded CONVERSATION prompt: %v", err)
	}
	return ContentIDs{
		TopicID: topic["id"].(uuid.UUID), ScenarioID: sc["id"].(uuid.UUID), ScenarioVersionID: sv["id"].(uuid.UUID),
		CharacterID: ch["id"].(uuid.UUID), PromptVersionID: pv,
	}
}
