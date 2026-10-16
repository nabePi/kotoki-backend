//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/summary"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestPGSummarySourceLoadTurns(t *testing.T) {
	p := newPGEvaluation(t)
	_, sid := p.newSession(t, nil, nil)
	now := time.Now().UTC()
	insert := func(seq int, status, user, ai, spoken *string) {
		row := testutil.Row{"id": uuid.Must(uuid.NewV7()), "session_id": sid, "sequence_number": seq, "generation": 1, "created_at": now}
		if status != nil {
			row["status"], row["completed_at"] = *status, now
		}
		for col, v := range map[string]*string{"user_text": user, "assistant_text": ai, "assistant_spoken_text": spoken} {
			if v != nil {
				row[col] = *v
			}
		}
		testutil.MustInsert(context.Background(), t, p.pool, "session_turns", row)
	}
	completed, discarded := "COMPLETED", "DISCARDED"
	hello, ghost, reply, spoken := "会社で働いています", "ノイズ", "そうですか。", "そうですか"
	insert(3, &completed, &hello, &reply, &spoken)
	insert(1, &completed, &hello, &reply, nil)
	insert(2, &discarded, &ghost, &ghost, nil)
	insert(4, nil, nil, nil, nil) // a turn in progress (status NULL)

	src := postgres.NewSummarySource(p.pool)
	turns, err := src.LoadTurns(context.Background(), sid, now.Add(-time.Minute), now.Add(time.Minute))
	require.NoError(t, err)
	seqs := make([]int, len(turns))
	for i, tr := range turns {
		seqs[i] = tr.Seq
	}
	require.Equal(t, []int{1, 3, 4}, seqs, "DISCARDED is skipped and the rest is ordered by sequence_number")
	require.Equal(t, "COMPLETED", *turns[0].Status)
	require.Equal(t, hello, *turns[0].UserText)
	require.Equal(t, reply, *turns[0].AssistantText)
	require.Nil(t, turns[0].AssistantSpokenText)
	require.Equal(t, spoken, *turns[1].AssistantSpokenText)
	require.Nil(t, turns[2].Status)
	require.Nil(t, turns[2].UserText)

	none, err := src.LoadTurns(context.Background(), sid, now.Add(time.Hour), now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Empty(t, none, "created_at after the window")
	none, err = src.LoadTurns(context.Background(), sid, now.Add(-2*time.Hour), now.Add(-time.Hour))
	require.NoError(t, err)
	require.Empty(t, none, "created_at before the window")
	other, err := src.LoadTurns(context.Background(), uuid.Must(uuid.NewV7()), now.Add(-time.Minute), now.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, other, "turns of another session")
}

func TestPGSummarySourceLoadSession(t *testing.T) {
	p := newPGEvaluation(t)
	started, ended := time.Now().UTC().Add(-10*time.Minute).Truncate(time.Millisecond), time.Now().UTC().Truncate(time.Millisecond)
	uid, sid := p.newSession(t, testutil.Row{"status": "SUSPENDED"}, testutil.Row{
		"status": "COMPLETED", "end_reason": "USER_ENDED", "ending_at": ended, "starting_at": started.Add(-time.Second),
		"started_at": started, "ended_at": ended})

	src := postgres.NewSummarySource(p.pool)
	info, err := src.LoadSession(context.Background(), sid)
	require.NoError(t, err)
	require.Equal(t, sid, info.ID)
	require.Equal(t, uid, info.UserID)
	require.Equal(t, "SUSPENDED", info.UserStatus, "the owner's status comes from users")
	require.True(t, started.Equal(*info.StartedAt))
	require.True(t, ended.Equal(*info.EndedAt))
	require.True(t, started.Add(-time.Second).Equal(*info.StartingAt))
	var topic, scenario string
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT t.name, sc.name FROM topics t, scenarios sc WHERE t.id = $1 AND sc.id = $2`,
		p.content.TopicID, p.content.ScenarioID).Scan(&topic, &scenario))
	require.NotEmpty(t, topic)
	require.Equal(t, topic, info.TopicName)
	require.Equal(t, scenario, info.ScenarioName)

	_, err = src.LoadSession(context.Background(), uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, summary.ErrSessionNotFound)
}

func TestPGSummaryPromptSource(t *testing.T) {
	pool := testutil.NewPostgres(t)
	src := postgres.NewSummaryPromptSource(pool)
	_, err := src.PublishedPrompt(context.Background())
	require.ErrorIs(t, err, summary.ErrPromptNotFound, "no seed, no prompt")

	testutil.ApplySeed(t, pool)
	pr, err := src.PublishedPrompt(context.Background())
	require.NoError(t, err)
	require.Equal(t, "01920000-000d-7000-8000-000000000003", pr.ID.String())
	require.Equal(t, "SUMMARY", pr.Template.Key)
	require.Equal(t, 1, pr.Template.Version)
	require.Contains(t, pr.Template.System, "{{.MaxChars}}")
	require.Contains(t, pr.Template.User, "{{.Transcript}}")
	require.Equal(t, 400, *pr.Template.Params.MaxOutputTokens)
	require.InDelta(t, 0.3, *pr.Template.Params.Temperature, 1e-9)
	require.Empty(t, pr.Template.OutputSchema)
}

func TestPGSummaryStoreSetSummary(t *testing.T) {
	p := newPGEvaluation(t)
	_, sid := p.newSession(t, nil, nil)
	store := p.store.SummaryStore()
	read := func() (text *string, version int, updated time.Time) {
		require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT summary_text, version, updated_at FROM sessions WHERE id = $1`, sid).Scan(&text, &version, &updated))
		return
	}
	_, v0, _ := read()

	var first, second bool
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx summary.Tx) error {
		var err error
		if first, err = tx.Sessions().SetSummary(ctx, sid, "最初の要約"); err != nil {
			return err
		}
		second, err = tx.Sessions().SetSummary(ctx, sid, "二度目の要約")
		return err
	}))
	require.True(t, first)
	require.False(t, second, "summary_text is written only while NULL")
	text, v1, updated := read()
	require.Equal(t, "最初の要約", *text)
	require.Equal(t, v0, v1, "version does not move")
	require.WithinDuration(t, time.Now(), updated, time.Minute)

	ok, err := func() (bool, error) {
		var ok bool
		err := store.WithinTx(context.Background(), func(ctx context.Context, tx summary.Tx) error {
			var err error
			ok, err = tx.Sessions().SetSummary(ctx, uuid.Must(uuid.NewV7()), "x")
			return err
		})
		return ok, err
	}()
	require.NoError(t, err)
	require.False(t, ok, "unknown session")
}

func TestPGSummaryStoreRollsBack(t *testing.T) {
	p := newPGEvaluation(t)
	_, sid := p.newSession(t, nil, nil)
	env := events.Envelope{EventID: uuid.Must(uuid.NewV7()), EventType: events.SessionCompleted, EventVersion: 1}
	boom := errors.New("boom")
	err := p.store.SummaryStore().WithinTx(context.Background(), func(ctx context.Context, tx summary.Tx) error {
		if err := tx.Processed().MarkProcessed(ctx, events.QueueSummary, env); err != nil {
			return err
		}
		if _, err := tx.Sessions().SetSummary(ctx, sid, "要約"); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	var n int
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE id = $1 AND summary_text IS NOT NULL`, sid).Scan(&n))
	require.Zero(t, n)
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_events WHERE consumer = 'summary.session_completed'`).Scan(&n))
	require.Zero(t, n, "the idempotency row rolls back with the write")

	// a second MarkProcessed of the same event in a committed transaction reports the duplicate
	mark := func() error {
		return p.store.SummaryStore().WithinTx(context.Background(), func(ctx context.Context, tx summary.Tx) error {
			return tx.Processed().MarkProcessed(ctx, events.QueueSummary, env)
		})
	}
	require.NoError(t, mark())
	require.ErrorIs(t, mark(), events.ErrAlreadyProcessed)
}
