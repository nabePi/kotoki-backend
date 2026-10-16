//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient"
)

// inOrder asserts that want occurs in got as a subsequence (other events may sit in between).
func inOrder(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	require.Equal(t, len(want), i, "event order: wanted %v as a subsequence of %v", want, got)
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestE1UserFirstBasic is E1 (spec 14.5): USER_FIRST, one utterance, the AI answers, the user ends.
func TestE1UserFirstBasic(t *testing.T) {
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")
	rec := newRecorder()
	rep, err := converse(t, u, s, []kotokiclient.Step{
		kotokiclient.Speak{Packets: packets(t, "ja_self_intro"), Label: "ja_self_intro"},
		kotokiclient.WaitAIDone{},
		kotokiclient.End{},
	}, rec)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	// The events of Bab 7, in order.
	inOrder(t, rec.types(), []string{
		"session.ready", "session.started",
		"speech.started", "turn.started", "stt.partial", "speech.ended", "stt.final",
		"ai.generation_started", "ai.text_delta", "ai.audio_started", "ai.sentence", "ai.audio_completed",
		"turn.completed", "session.ending", "session.completed",
	})
	gens := rec.of("ai.generation_started")
	require.Len(t, gens, 1)
	require.NotNil(t, gens[0].Generation)
	require.GreaterOrEqual(t, *gens[0].Generation, uint32(1))
	assert.Positive(t, rec.frameCount(*gens[0].Generation), "downstream audio of the answer")

	// One COMPLETED turn with its latency columns.
	eventually(t, func() bool { return len(turnRows(t, s.ID)) == 1 && turnRows(t, s.ID)[0].Status != nil }, "turn persisted")
	turns := turnRows(t, s.ID)
	require.Len(t, turns, 1)
	tr := turns[0]
	assert.Equal(t, "COMPLETED", str(tr.Status))
	assert.Equal(t, 1, tr.Seq)
	require.NotNil(t, tr.UserText)
	assert.NotEmpty(t, *tr.UserText)
	assert.NotEmpty(t, str(tr.AssistantText))
	for name, v := range map[string]*int{
		"turn_end_to_first_audio_ms": tr.TurnEndToFirstAudio, "llm_first_token_ms": tr.LLMFirstToken, "tts_first_audio_ms": tr.TTSFirstAudio,
	} {
		if assert.NotNil(t, v, name) {
			assert.Positive(t, *v, name)
		}
	}

	// The session and the balance settle together.
	eventually(t, func() bool {
		got, err := u.Client.GetSession(t.Context(), s.ID)
		return err == nil && got.Status == "COMPLETED"
	}, "session COMPLETED")
	got, err := u.Client.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.NotNil(t, got.EndReason)
	assert.Equal(t, "USER_ENDED", *got.EndReason)
	charged := int64(got.ChargedSeconds)
	assert.Equal(t, got.ChargedSeconds, rep.ChargedSeconds)
	require.Positive(t, charged)
	require.Less(t, charged, int64(300))

	var consumed, released *int64
	require.NoError(t, stack.pool.QueryRow(t.Context(),
		`SELECT consumed_seconds, released_seconds FROM balance_reservations WHERE session_id = $1 AND status = 'SETTLED'`, s.ID).Scan(&consumed, &released))
	require.NotNil(t, consumed)
	assert.Equal(t, charged, *consumed)
	assert.Equal(t, int64(300)-charged, *released)

	ledger := ledgerRows(t, s.ID)
	require.Len(t, ledger, 3, "RESERVE, CONSUME, RELEASE: %+v", ledger)
	assert.Equal(t, ledgerRow{"RESERVE", -300, 300}, ledger[0])
	assert.Equal(t, ledgerRow{"CONSUME", 0, -charged}, ledger[1])
	assert.Equal(t, ledgerRow{"RELEASE", 300 - charged, -(300 - charged)}, ledger[2])
	bal, err := u.Client.Balance(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3600-charged, bal.AvailableSeconds)
	assert.Zero(t, bal.ReservedSeconds)
}

// TestE2AIFirst is E2: the AI opens the conversation (turn 0 without user text, D-079).
func TestE2AIFirst(t *testing.T) {
	u := newUser(t)
	s := startSession(t, u, "AI_FIRST")
	rec := newRecorder()
	rep, err := converse(t, u, s, []kotokiclient.Step{kotokiclient.WaitAIDone{}, kotokiclient.End{}}, rec)
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	inOrder(t, rec.types(), []string{
		"session.ready", "session.started", "ai.generation_started", "ai.audio_started", "ai.audio_completed", "turn.completed", "session.completed",
	})
	gens := rec.of("ai.generation_started")
	require.NotEmpty(t, gens)
	require.NotNil(t, gens[0].Generation)
	assert.Positive(t, rec.frameCount(*gens[0].Generation), "audio of the opening")

	eventually(t, func() bool { r := turnRows(t, s.ID); return len(r) > 0 && r[0].Status != nil }, "opening turn persisted")
	turns := turnRows(t, s.ID)
	assert.Equal(t, 0, turns[0].Seq)
	assert.Nil(t, turns[0].UserText, "user_text of the opening")
	assert.NotEmpty(t, str(turns[0].AssistantText))
	assert.Equal(t, "COMPLETED", str(turns[0].Status))
}

// TestE6CancelReserved is E6: cancelling a RESERVED session frees the reservation.
func TestE6CancelReserved(t *testing.T) {
	u := newUser(t)
	before, err := u.Client.Balance(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(3600), before.AvailableSeconds)

	s := startSession(t, u, "USER_FIRST")
	held, err := u.Client.Balance(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(3300), held.AvailableSeconds)
	require.Equal(t, int64(300), held.ReservedSeconds)

	require.NoError(t, u.Client.CancelSession(t.Context(), s.ID))
	got, err := u.Client.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	assert.Equal(t, "CANCELLED", got.Status)
	var status string
	require.NoError(t, stack.pool.QueryRow(t.Context(), `SELECT status FROM balance_reservations WHERE session_id = $1`, s.ID).Scan(&status))
	assert.Equal(t, "RELEASED", status)
	after, err := u.Client.Balance(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(3600), after.AvailableSeconds)
	assert.Zero(t, after.ReservedSeconds)
}

// closeCode waits for the connection to end and returns its close code.
func closeCode(t *testing.T, rc *kotokiclient.RealtimeConn) websocket.StatusCode {
	t.Helper()
	select {
	case <-rc.Done():
	case <-time.After(eventuallyWait):
		t.Fatal("connection did not close")
	}
	return websocket.CloseStatus(rc.Err())
}

// firstError returns the code of the error event the server sent before closing.
func firstError(t *testing.T, rc *kotokiclient.RealtimeConn) string {
	t.Helper()
	for {
		select {
		case e := <-rc.Events():
			if p, ok := e.Value.Payload.(protocol.ErrorPayload); ok {
				return p.Error.Code
			}
		case <-time.After(eventuallyWait):
			t.Fatal("no error event")
		}
	}
}

func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return "kotoki:ws_ticket:" + hex.EncodeToString(sum[:])
}

// TestE11TicketSingleUseAndExpiry is E11: a ticket works once and only for 30 s.
func TestE11TicketSingleUseAndExpiry(t *testing.T) {
	u := newUser(t)
	s := startSession(t, u, "USER_FIRST")
	ctx := ctxT(t, 30*time.Second)

	t.Run("second use", func(t *testing.T) {
		ticket, err := u.Client.RealtimeTicket(ctx, s.ID)
		require.NoError(t, err)
		ttl := stack.rdb.PTTL(ctx, ticketKey(ticket)).Val()
		require.Positive(t, ttl)
		assert.LessOrEqual(t, ttl, 30*time.Second)

		first, err := kotokiclient.Dial(ctx, stack.wsURL, ticket, testOrigin)
		require.NoError(t, err)
		select {
		case e := <-first.Events():
			require.Equal(t, "session.ready", e.Value.Type)
		case <-time.After(eventuallyWait):
			t.Fatal("no session.ready on the first use")
		}
		defer func() { _ = first.Close(websocket.StatusNormalClosure, "") }()

		second, err := kotokiclient.Dial(ctx, stack.wsURL, ticket, testOrigin)
		require.NoError(t, err, "the upgrade itself succeeds; the refusal is the close code")
		assert.Equal(t, "TICKET_INVALID", firstError(t, second))
		assert.Equal(t, websocket.StatusCode(4001), closeCode(t, second))
	})

	t.Run("expired", func(t *testing.T) {
		ticket, err := u.Client.RealtimeTicket(ctx, s.ID)
		require.NoError(t, err)
		key := ticketKey(ticket)
		require.EqualValues(t, 1, stack.rdb.Exists(ctx, key).Val())
		// Redis expires it as it would after 30 s.
		require.NoError(t, stack.rdb.PExpire(ctx, key, time.Millisecond).Err())
		eventually(t, func() bool { return stack.rdb.Exists(ctx, key).Val() == 0 }, "ticket expired")

		rc, err := kotokiclient.Dial(ctx, stack.wsURL, ticket, testOrigin)
		require.NoError(t, err)
		assert.Equal(t, "TICKET_INVALID", firstError(t, rc))
		assert.Equal(t, websocket.StatusCode(4001), closeCode(t, rc))
	})
}
