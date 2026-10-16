//go:build integration

package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/summary"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// textRouter is the TextRouter of these scenarios: reply (when set) answers call n (0-based),
// otherwise the fake LLM does, collected the way provider.Router.CompleteText collects a stream.
type textRouter struct {
	mu    sync.Mutex
	reqs  []provider.TextRequest
	reply func(n int, req provider.TextRequest) (provider.TextResult, error)
	llm   *fake.LLM
}

func newTextRouter() *textRouter {
	return &textRouter{llm: fake.NewLLM(provider.FakePrimary, fake.CapConfig{}, fake.Config{}, fake.Deps{Seed: 42})}
}

func replyWith(text string) func(int, provider.TextRequest) (provider.TextResult, error) {
	return func(int, provider.TextRequest) (provider.TextResult, error) {
		return provider.TextResult{Text: text, Provider: provider.FakePrimary, Model: "fake-llm"}, nil
	}
}

func (r *textRouter) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *textRouter) CompleteText(ctx context.Context, req provider.TextRequest) (provider.TextResult, error) {
	r.mu.Lock()
	n := len(r.reqs)
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	if r.reply != nil {
		return r.reply(n, req)
	}
	stream, err := r.llm.Stream(ctx, provider.LLMRequest{Messages: req.Messages, MaxTokens: req.MaxTokens, Temperature: req.Temperature, RequestID: req.RequestID})
	if err != nil {
		return provider.TextResult{}, err
	}
	defer func() { _ = stream.Close() }()
	var b strings.Builder
	for ev := range stream.Events() {
		switch ev.Type {
		case provider.LLMDelta:
			b.WriteString(ev.Text)
		case provider.LLMError:
			return provider.TextResult{}, ev.Err
		}
	}
	return provider.TextResult{Text: b.String(), Provider: provider.FakePrimary, Model: "fake-llm"}, nil
}

type summaryRig struct {
	pool *pgxpool.Pool
	rt   *textRouter
	cons *summary.Consumer
}

func newSummaryRig(t *testing.T, rt *textRouter) *summaryRig {
	t.Helper()
	pool := testutil.NewPostgres(t)
	r := &summaryRig{pool: pool, rt: rt}
	svc := summary.NewService(summary.Deps{
		Store: postgres.NewStore(pool).SummaryStore(), Source: postgres.NewSummarySource(pool),
		Prompts: postgres.NewSummaryPromptSource(pool), Router: rt, Cfg: summary.DefaultConfig(),
		Log: slog.New(slog.DiscardHandler),
	})
	r.cons = summary.NewConsumer(svc)
	return r
}

func (r *summaryRig) handle(fs testutil.FinishedSession) error {
	return r.cons.Handle(context.Background(), events.Delivery{Envelope: fs.Envelope, Queue: events.QueueSummary})
}

func summaryProcessed(t *testing.T, pool *pgxpool.Pool, eventID uuid.UUID) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = 'summary.session_completed' AND event_id = $1`, eventID)
}

type summaryRow struct {
	Text    *string
	Version int
}

func loadSummary(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) summaryRow {
	t.Helper()
	var r summaryRow
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT summary_text, version FROM sessions WHERE id = $1`, sessionID).Scan(&r.Text, &r.Version))
	return r
}

func TestSummaryConsumer_Normal(t *testing.T) {
	rt := newTextRouter()
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(5, 25), ActiveSeconds: 120})
	before := loadSummary(t, rig.pool, fs.ID)
	require.Nil(t, before.Text)

	require.NoError(t, rig.handle(fs))

	after := loadSummary(t, rig.pool, fs.ID)
	require.NotNil(t, after.Text)
	require.NotEmpty(t, strings.TrimSpace(*after.Text))
	require.LessOrEqual(t, utf8.RuneCountInString(*after.Text), 300)
	require.Equal(t, before.Version, after.Version, "summary_text is not a transition")
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
	require.Zero(t, count(t, rig.pool, `SELECT count(*) FROM event_outbox`), "summary emits no event")

	require.Equal(t, 1, rt.calls())
	req := rt.reqs[0]
	require.Equal(t, "SUMMARY", req.Task)
	require.Equal(t, "SUMMARY:"+fs.ID.String(), req.RequestID)
	require.Equal(t, 400, req.MaxTokens)
	require.InDelta(t, 0.3, req.Temperature, 1e-9)
	require.Len(t, req.Messages, 2)
	require.Contains(t, req.Messages[0].Content, "300文字以内")
	// the seeded template, the real topic/scenario names and the transcript reach the model
	require.Contains(t, req.Messages[1].Content, "ユーザーの発話数: 5")
	require.Contains(t, req.Messages[1].Content, "ユーザー: "+jaTurns[0])
	require.Contains(t, req.Messages[1].Content, "AI: そうですか。")
	var topic, scenario string
	require.NoError(t, rig.pool.QueryRow(context.Background(), `SELECT t.name, sc.name FROM sessions s JOIN topics t ON t.id = s.topic_id JOIN scenarios sc ON sc.id = s.scenario_id WHERE s.id = $1`, fs.ID).Scan(&topic, &scenario))
	require.Contains(t, req.Messages[1].Content, "話題: "+topic+"\n場面: "+scenario+"\n")
}

func TestSummaryConsumer_LongOutputIsCut(t *testing.T) {
	rt := newTextRouter()
	rt.reply = replyWith(strings.Repeat("あ", 289) + "。" + strings.Repeat("い", 100))
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})

	require.NoError(t, rig.handle(fs))
	require.Equal(t, strings.Repeat("あ", 289)+"。", *loadSummary(t, rig.pool, fs.ID).Text)
}

func TestSummary_FailedSessionIsSummarised(t *testing.T) {
	rig := newSummaryRig(t, newTextRouter())
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Status: "FAILED", Turns: seedTurns(2, 5), ActiveSeconds: 40})
	require.NoError(t, rig.handle(fs))
	require.NotNil(t, loadSummary(t, rig.pool, fs.ID).Text)
}

func TestSummary_ZeroUserTurns_StaysNull(t *testing.T) {
	rt := newTextRouter()
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(2, 5), ActiveSeconds: 60}).
		WithPayload(t, func(p *events.SessionCompletedV1) { p.UserTurnCount = 0 })

	require.NoError(t, rig.handle(fs))
	require.Nil(t, loadSummary(t, rig.pool, fs.ID).Text)
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
	require.Zero(t, rt.calls())
}

func TestSummary_DeletedUser(t *testing.T) {
	rt := newTextRouter()
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{UserStatus: "DELETED", Turns: seedTurns(3, 10), ActiveSeconds: 60})

	require.ErrorIs(t, rig.handle(fs), events.ErrSkippedDeletedUser)
	require.Nil(t, loadSummary(t, rig.pool, fs.ID).Text)
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
	require.Zero(t, rt.calls())
}

func TestSummary_DuplicateSequential(t *testing.T) {
	rt := newTextRouter()
	rt.reply = func(n int, _ provider.TextRequest) (provider.TextResult, error) {
		return provider.TextResult{Text: []string{"最初の要約。", "二回目の要約。", "三回目の要約。"}[n]}, nil
	}
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})

	require.NoError(t, rig.handle(fs))
	require.ErrorIs(t, rig.handle(fs), events.ErrAlreadyProcessed, "redelivery of the same event")
	require.Equal(t, "最初の要約。", *loadSummary(t, rig.pool, fs.ID).Text)

	// another event of the same session (a replayed copy with a new id) cannot overwrite either
	other := fs
	other.Envelope.EventID = uuid.New()
	require.NoError(t, rig.handle(other))
	require.Equal(t, "最初の要約。", *loadSummary(t, rig.pool, fs.ID).Text)
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
	require.Equal(t, 1, summaryProcessed(t, rig.pool, other.Envelope.EventID))
}

func TestSummary_DuplicateParallel(t *testing.T) {
	rt := newTextRouter()
	rt.reply = func(n int, _ provider.TextRequest) (provider.TextResult, error) {
		return provider.TextResult{Text: []string{"要約その一。", "要約その二。"}[n]}, nil
	}
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = rig.handle(fs)
		}()
	}
	wg.Wait()

	nils := 0
	for _, err := range errs {
		if err == nil {
			nils++
			continue
		}
		require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	}
	require.Equal(t, 1, nils, "exactly one copy writes: %v", errs)
	got := loadSummary(t, rig.pool, fs.ID)
	require.NotNil(t, got.Text)
	require.Contains(t, []string{"要約その一。", "要約その二。"}, *got.Text)
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
}

func TestSummary_ExistingSummaryUntouched(t *testing.T) {
	rig := newSummaryRig(t, newTextRouter())
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})
	_, err := rig.pool.Exec(context.Background(), `UPDATE sessions SET summary_text = '既存の要約' WHERE id = $1`, fs.ID)
	require.NoError(t, err)
	before := loadSummary(t, rig.pool, fs.ID)
	var updatedBefore string
	require.NoError(t, rig.pool.QueryRow(context.Background(), `SELECT updated_at::text FROM sessions WHERE id = $1`, fs.ID).Scan(&updatedBefore))

	require.NoError(t, rig.handle(fs))

	after := loadSummary(t, rig.pool, fs.ID)
	require.Equal(t, "既存の要約", *after.Text)
	require.Equal(t, before.Version, after.Version)
	var updatedAfter string
	require.NoError(t, rig.pool.QueryRow(context.Background(), `SELECT updated_at::text FROM sessions WHERE id = $1`, fs.ID).Scan(&updatedAfter))
	require.Equal(t, updatedBefore, updatedAfter, "a skipped write does not touch the row")
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
}

func TestSummary_ProviderFailureThenRetry(t *testing.T) {
	rt := newTextRouter()
	rt.reply = func(n int, _ provider.TextRequest) (provider.TextResult, error) {
		if n == 0 {
			e := provider.NewError(provider.FakePrimary, provider.CapLLM, provider.KindUnavailable, "down")
			e.Retryable = true
			return provider.TextResult{}, e
		}
		return provider.TextResult{Text: "ユーザーは旅行の話をした。"}, nil
	}
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})

	err := rig.handle(fs)
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	var perm *events.PermanentError
	require.False(t, errors.As(err, &perm))
	require.Nil(t, loadSummary(t, rig.pool, fs.ID).Text)
	require.Zero(t, summaryProcessed(t, rig.pool, fs.Envelope.EventID), "a failed attempt records nothing, so the retry runs")

	require.NoError(t, rig.handle(fs))
	require.Equal(t, "ユーザーは旅行の話をした。", *loadSummary(t, rig.pool, fs.ID).Text)
	require.Equal(t, 1, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
}

func TestSummary_EmptyOutputIsRetryable(t *testing.T) {
	rt := newTextRouter()
	rt.reply = replyWith("  ")
	rig := newSummaryRig(t, rt)
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})

	err := rig.handle(fs)
	require.ErrorIs(t, err, summary.ErrEmptySummary)
	var perm *events.PermanentError
	require.False(t, errors.As(err, &perm))
	require.Nil(t, loadSummary(t, rig.pool, fs.ID).Text)
	require.Zero(t, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
}

func TestSummary_PermanentFailures(t *testing.T) {
	t.Run("no published SUMMARY prompt", func(t *testing.T) {
		rt := newTextRouter()
		rig := newSummaryRig(t, rt)
		fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})
		_, err := rig.pool.Exec(context.Background(), `UPDATE prompt_versions SET status = 'RETIRED', retired_at = now()
			WHERE prompt_config_id = (SELECT id FROM prompt_configs WHERE key = 'SUMMARY')`)
		require.NoError(t, err)

		var perm *events.PermanentError
		require.ErrorAs(t, rig.handle(fs), &perm)
		require.Equal(t, "CONFIGURATION_ERROR", perm.Code)
		require.Zero(t, rt.calls())
		require.Nil(t, loadSummary(t, rig.pool, fs.ID).Text)
		require.Zero(t, summaryProcessed(t, rig.pool, fs.Envelope.EventID))
	})
	t.Run("transcript purged", func(t *testing.T) {
		rt := newTextRouter()
		rig := newSummaryRig(t, rt)
		fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60})
		_, err := rig.pool.Exec(context.Background(), `DELETE FROM session_turns WHERE session_id = $1`, fs.ID)
		require.NoError(t, err)

		var perm *events.PermanentError
		require.ErrorAs(t, rig.handle(fs), &perm)
		require.Equal(t, "TRANSCRIPT_UNAVAILABLE", perm.Code)
		require.Zero(t, rt.calls())
	})
	t.Run("unknown session", func(t *testing.T) {
		rig := newSummaryRig(t, newTextRouter())
		fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: seedTurns(3, 10), ActiveSeconds: 60}).
			WithPayload(t, func(p *events.SessionCompletedV1) { p.SessionID = uuid.New() })
		var perm *events.PermanentError
		require.ErrorAs(t, rig.handle(fs), &perm)
		require.Equal(t, "ENTITY_NOT_FOUND", perm.Code)
	})
}

func TestSummary_DiscardedTurnsAreNotSent(t *testing.T) {
	rt := newTextRouter()
	rig := newSummaryRig(t, rt)
	turns := seedTurns(3, 10)
	turns = append(turns, testutil.TurnSeed{UserText: "捨てられた発話です。", AssistantText: "捨てられた応答です。", Status: "DISCARDED", UserSeconds: 3})
	fs := testutil.SeedFinishedSession(t, rig.pool, testutil.FinishedSessionOpts{Turns: turns, ActiveSeconds: 60})

	require.NoError(t, rig.handle(fs))
	user := rt.reqs[0].Messages[1].Content
	require.NotContains(t, user, "捨てられた")
	require.Contains(t, user, "ユーザー: "+jaTurns[0])
}
