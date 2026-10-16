package summary_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/summary"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

const (
	sysTmpl  = "SYS max={{.MaxChars}}"
	userTmpl = "topic={{.TopicName}} scenario={{.ScenarioName}} n={{.UserTurnCount}}\n<t>{{.Transcript}}</t>"
)

// memSource is a summary.Source over canned data; it records the arguments of LoadTurns.
type memSource struct {
	info     summary.SessionInfo
	loadErr  error
	turns    []summary.TurnRow
	turnsErr error

	mu               sync.Mutex
	loads, turnLoads int
	from, to         time.Time
}

func (s *memSource) LoadSession(context.Context, uuid.UUID) (summary.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return s.info, s.loadErr
}

func (s *memSource) LoadTurns(_ context.Context, _ uuid.UUID, from, to time.Time) ([]summary.TurnRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnLoads++
	s.from, s.to = from, to
	return s.turns, s.turnsErr
}

type memPrompts struct {
	pr  summary.Prompt
	err error
}

func (p memPrompts) PublishedPrompt(context.Context) (summary.Prompt, error) { return p.pr, p.err }

// memStore is a summary.Store with rollback: the summaries and processed rows written by a failing
// transaction are undone.
type memStore struct {
	fakes.FailInjector
	mu        sync.Mutex
	processed *fakes.Processed
	summaries map[uuid.UUID]*string
	sets      int // SetSummary calls that changed a row
}

func newMemStore(sid uuid.UUID, existing *string) *memStore {
	return &memStore{processed: fakes.NewProcessed(), summaries: map[uuid.UUID]*string{sid: existing}}
}

func (s *memStore) text(id uuid.UUID) *string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaries[id]
}

type memTx struct {
	s      *memStore
	marked []uuid.UUID
}

func (s *memStore) WithinTx(ctx context.Context, fn func(context.Context, summary.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := map[uuid.UUID]*string{}
	for k, v := range s.summaries {
		before[k] = v
	}
	sets := s.sets
	tx := &memTx{s: s}
	if err := fn(ctx, tx); err != nil {
		s.summaries, s.sets = before, sets
		for _, id := range tx.marked {
			s.processed.Forget(events.QueueSummary, id)
		}
		return err
	}
	return nil
}

func (t *memTx) Processed() events.ProcessedEvents { return processedTx{t} }
func (t *memTx) Sessions() summary.SessionWriter   { return writer{t} }

type processedTx struct{ tx *memTx }

func (p processedTx) MarkProcessed(ctx context.Context, consumer string, ev events.Envelope) error {
	if err := p.tx.s.processed.MarkProcessed(ctx, consumer, ev); err != nil {
		return err
	}
	p.tx.marked = append(p.tx.marked, ev.EventID)
	return nil
}

type writer struct{ tx *memTx }

func (w writer) SetSummary(_ context.Context, id uuid.UUID, text string) (bool, error) {
	if err := w.tx.s.Take("SetSummary"); err != nil {
		return false, err
	}
	cur, ok := w.tx.s.summaries[id]
	if !ok || cur != nil {
		return false, nil
	}
	w.tx.s.summaries[id] = &text
	w.tx.s.sets++
	return true, nil
}

// router answers CompleteText from a script and records the requests.
type router struct {
	mu    sync.Mutex
	reqs  []provider.TextRequest
	reply func(n int) (provider.TextResult, error)
}

func (r *router) CompleteText(_ context.Context, req provider.TextRequest) (provider.TextResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	n := len(r.reqs) - 1
	r.mu.Unlock()
	return r.reply(n)
}

func (r *router) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func say(text string) func(int) (provider.TextResult, error) {
	return func(int) (provider.TextResult, error) {
		return provider.TextResult{Text: text, Provider: provider.OpenAI, Model: "m"}, nil
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent log writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type rig struct {
	t     *testing.T
	src   *memSource
	pr    memPrompts
	store *memStore
	rt    *router
	cfg   summary.Config
	logs  *syncBuffer
	sid   uuid.UUID
	env   events.Envelope
	p     events.SessionCompletedV1
	pvID  uuid.UUID
}

func newRig(t *testing.T) *rig {
	t.Helper()
	sid, uid := uuid.New(), uuid.New()
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	completed := "COMPLETED"
	r := &rig{t: t, sid: sid, pvID: uuid.New(), cfg: summary.DefaultConfig(), logs: &syncBuffer{}}
	r.src = &memSource{
		info: summary.SessionInfo{ID: sid, UserID: uid, UserStatus: "ACTIVE", TopicName: "旅行", ScenarioName: "ホテル",
			StartingAt: ptr(start.Add(-time.Second)), StartedAt: &start, EndedAt: &end},
		turns: []summary.TurnRow{
			{Seq: 1, Status: &completed, UserText: sp("京都へ行きます。"), AssistantText: sp("いいですね。")},
			{Seq: 2, Status: &completed, UserText: sp("来週です。"), AssistantText: sp("楽しんでください。")},
		},
	}
	r.pr = memPrompts{pr: summary.Prompt{ID: r.pvID, Template: prompt.Template{Key: "SUMMARY", Version: 1, System: sysTmpl, User: userTmpl}}}
	r.store = newMemStore(sid, nil)
	r.rt = &router{reply: say("ユーザーは京都旅行の予定を話した。")}
	r.p = events.SessionCompletedV1{SessionID: sid, UserTurnCount: 2, StartedAt: start, EndedAt: end}
	raw, err := json.Marshal(r.p)
	require.NoError(t, err)
	r.env = events.Envelope{EventID: uuid.New(), EventType: events.SessionCompleted, EventVersion: 1, UserID: uid, AggregateID: sid, Payload: raw}
	return r
}

func ptr[T any](v T) *T { return &v }

func (r *rig) service() *summary.Service {
	return summary.NewService(summary.Deps{
		Store: r.store, Source: r.src, Prompts: r.pr, Router: r.rt, Cfg: r.cfg,
		Log: slog.New(slog.NewTextHandler(r.logs, nil)),
	})
}

func (r *rig) process() error { return r.service().Process(context.Background(), r.env, r.p) }

func (r *rig) processed() bool {
	ok, err := r.store.processed.IsProcessed(context.Background(), events.QueueSummary, r.env.EventID)
	require.NoError(r.t, err)
	return ok
}

func requirePermanent(t *testing.T, err error, code string) {
	t.Helper()
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, code, perm.Code)
}

func requireRetryable(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var perm *events.PermanentError
	require.False(t, errors.As(err, &perm), "must not be permanent: %v", err)
	require.NotErrorIs(t, err, events.ErrAlreadyProcessed)
	require.NotErrorIs(t, err, events.ErrSkippedDeletedUser)
}

func TestProcessNormal(t *testing.T) {
	r := newRig(t)
	require.NoError(t, r.process())

	require.Equal(t, 1, r.rt.calls())
	req := r.rt.reqs[0]
	require.Equal(t, "SUMMARY", req.Task)
	require.Equal(t, "SUMMARY:"+r.sid.String(), req.RequestID)
	require.Equal(t, 400, req.MaxTokens)
	require.InDelta(t, 0.3, req.Temperature, 1e-9)
	require.Empty(t, req.Model)
	require.Equal(t, []provider.Message{
		{Role: provider.RoleSystem, Content: "SYS max=300"},
		{Role: provider.RoleUser, Content: "topic=旅行 scenario=ホテル n=2\n<t>ユーザー: 京都へ行きます。\nAI: いいですね。\nユーザー: 来週です。\nAI: 楽しんでください。</t>"},
	}, req.Messages)

	got := r.store.text(r.sid)
	require.NotNil(t, got)
	require.Equal(t, "ユーザーは京都旅行の予定を話した。", *got)
	require.True(t, r.processed())
	require.Equal(t, 1, r.store.sets)

	// the turn window is the session window padded by a minute
	require.Equal(t, r.src.info.StartingAt.Add(-time.Minute), r.src.from)
	require.Equal(t, r.src.info.EndedAt.Add(time.Minute), r.src.to)

	// the log names ids, never texts
	require.Contains(t, r.logs.String(), r.pvID.String())
	for _, secret := range []string{"京都", "ユーザーは", "SYS max"} {
		require.NotContains(t, r.logs.String(), secret)
	}
}

func TestProcessDBParamsWin(t *testing.T) {
	r := newRig(t)
	r.pr.pr.Template.Params = prompt.Params{Temperature: ptr(0.1), MaxOutputTokens: ptr(250)}
	require.NoError(t, r.process())
	require.Equal(t, 250, r.rt.reqs[0].MaxTokens)
	require.InDelta(t, 0.1, r.rt.reqs[0].Temperature, 1e-9)

	// a zero temperature stored in the database is a value, not "absent"
	r = newRig(t)
	r.pr.pr.Template.Params = prompt.Params{Temperature: ptr(0.0)}
	require.NoError(t, r.process())
	require.Zero(t, r.rt.reqs[0].Temperature)
	require.Equal(t, 400, r.rt.reqs[0].MaxTokens)
}

func TestProcessZeroUserTurns(t *testing.T) {
	r := newRig(t)
	r.p.UserTurnCount = 0
	require.NoError(t, r.process())
	require.Zero(t, r.rt.calls())
	require.Zero(t, r.src.turnLoads)
	require.Nil(t, r.store.text(r.sid))
	require.True(t, r.processed())
}

func TestProcessDeletedUser(t *testing.T) {
	r := newRig(t)
	r.src.info.UserStatus = "DELETED"
	require.ErrorIs(t, r.process(), events.ErrSkippedDeletedUser)
	require.Zero(t, r.rt.calls())
	require.Zero(t, r.src.turnLoads)
	require.Nil(t, r.store.text(r.sid))
	require.True(t, r.processed())
}

func TestProcessPermanentFailures(t *testing.T) {
	tests := []struct {
		name  string
		shape func(*rig)
		code  string
	}{
		{"session not found", func(r *rig) { r.src.loadErr = summary.ErrSessionNotFound }, "ENTITY_NOT_FOUND"},
		{"event for another user", func(r *rig) { r.src.info.UserID = uuid.New() }, "INVARIANT_VIOLATION"},
		{"no published prompt", func(r *rig) { r.pr.err = summary.ErrPromptNotFound }, "CONFIGURATION_ERROR"},
		{"prompt without user template", func(r *rig) { r.pr.pr.Template.User = "" }, "CONFIGURATION_ERROR"},
		{"user template does not render", func(r *rig) { r.pr.pr.Template.User = "{{.Nope}}" }, "CONFIGURATION_ERROR"},
		{"system template does not parse", func(r *rig) { r.pr.pr.Template.System = "{{" }, "CONFIGURATION_ERROR"},
		{"transcript gone", func(r *rig) { r.src.turns = nil }, "TRANSCRIPT_UNAVAILABLE"},
		{"only discarded turns", func(r *rig) {
			d := "DISCARDED"
			r.src.turns = []summary.TurnRow{{Status: &d, UserText: sp("x")}}
		}, "TRANSCRIPT_UNAVAILABLE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.shape(r)
			requirePermanent(t, r.process(), tc.code)
			require.Zero(t, r.rt.calls(), "no provider call on a permanent failure")
			require.Nil(t, r.store.text(r.sid))
			require.False(t, r.processed(), "a permanent failure goes to the DLQ unprocessed")
		})
	}
}

func TestProcessRetryableFailures(t *testing.T) {
	boom := errors.New("connection reset")
	unavailable := provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindUnavailable, "down")
	tests := []struct {
		name  string
		shape func(*rig)
		is    error
	}{
		{"provider error", func(r *rig) {
			r.rt.reply = func(int) (provider.TextResult, error) { return provider.TextResult{}, unavailable }
		}, unavailable},
		{"empty output", func(r *rig) { r.rt.reply = say("") }, summary.ErrEmptySummary},
		{"blank output", func(r *rig) { r.rt.reply = say("  \n ") }, summary.ErrEmptySummary},
		{"load session", func(r *rig) { r.src.loadErr = boom }, boom},
		{"load turns", func(r *rig) { r.src.turnsErr = boom }, boom},
		{"load prompt", func(r *rig) { r.pr.err = boom }, boom},
		{"set summary", func(r *rig) { r.store.FailNext("SetSummary", boom) }, boom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.shape(r)
			err := r.process()
			requireRetryable(t, err)
			require.ErrorIs(t, err, tc.is)
			require.Nil(t, r.store.text(r.sid))
			require.False(t, r.processed(), "retryable failure leaves the event unprocessed")
		})
	}
}

func TestProcessRetryAfterFailureWritesOnce(t *testing.T) {
	r := newRig(t)
	r.store.FailNext("SetSummary", errors.New("deadlock"))
	requireRetryable(t, r.process())
	require.False(t, r.processed(), "the transaction rolled back MarkProcessed")
	require.NoError(t, r.process())
	require.True(t, r.processed())
	require.NotNil(t, r.store.text(r.sid))
	require.Equal(t, 1, r.store.sets)
}

func TestProcessSummaryWrittenOnce(t *testing.T) {
	t.Run("existing summary untouched, event still processed", func(t *testing.T) {
		r := newRig(t)
		r.store = newMemStore(r.sid, sp("既存の要約"))
		require.NoError(t, r.process())
		require.Equal(t, "既存の要約", *r.store.text(r.sid))
		require.Zero(t, r.store.sets)
		require.True(t, r.processed())
	})
	t.Run("redelivery has one effect", func(t *testing.T) {
		r := newRig(t)
		require.NoError(t, r.process())
		r.rt.reply = say("別の要約。")
		require.ErrorIs(t, r.process(), events.ErrAlreadyProcessed)
		require.Equal(t, "ユーザーは京都旅行の予定を話した。", *r.store.text(r.sid))
		require.Equal(t, 1, r.store.sets)
	})
	t.Run("a second event of the same session cannot overwrite", func(t *testing.T) {
		r := newRig(t)
		require.NoError(t, r.process())
		r.env.EventID = uuid.New()
		r.rt.reply = say("別の要約。")
		require.NoError(t, r.process())
		require.Equal(t, "ユーザーは京都旅行の予定を話した。", *r.store.text(r.sid))
		require.Equal(t, 1, r.store.sets)
	})
	t.Run("parallel copies", func(t *testing.T) {
		r := newRig(t)
		svc := r.service()
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = svc.Process(context.Background(), r.env, r.p)
			}()
		}
		wg.Wait()
		nils := 0
		for _, err := range errs {
			if err == nil {
				nils++
			} else {
				require.ErrorIs(t, err, events.ErrAlreadyProcessed)
			}
		}
		require.Equal(t, 1, nils)
		require.Equal(t, 1, r.store.sets)
	})
	t.Run("MarkProcessed conflict is returned and nothing is written", func(t *testing.T) {
		r := newRig(t)
		require.NoError(t, r.store.processed.MarkProcessed(context.Background(), events.QueueSummary, r.env))
		require.ErrorIs(t, r.process(), events.ErrAlreadyProcessed)
		require.Nil(t, r.store.text(r.sid))
	})
}

func TestProcessSessionGone(t *testing.T) {
	// the row vanished between LoadSession and the write: SetSummary reports false, the event is done
	r := newRig(t)
	r.store = newMemStore(uuid.New(), nil)
	require.NoError(t, r.process())
	require.True(t, r.processed())
	require.Zero(t, r.store.sets)
}

func TestProcessLimits(t *testing.T) {
	t.Run("output longer than the limit is cut after a sentence", func(t *testing.T) {
		r := newRig(t)
		long := strings.Repeat("あ", 289) + "。" + strings.Repeat("い", 60)
		r.rt.reply = say(long)
		require.NoError(t, r.process())
		require.Equal(t, strings.Repeat("あ", 289)+"。", *r.store.text(r.sid))
	})
	t.Run("hard cut", func(t *testing.T) {
		r := newRig(t)
		r.rt.reply = say(strings.Repeat("あ", 500))
		require.NoError(t, r.process())
		require.Equal(t, 300, utf8.RuneCountInString(*r.store.text(r.sid)))
	})
	t.Run("custom limit reaches the prompt and the cleaning", func(t *testing.T) {
		r := newRig(t)
		r.cfg = summary.Config{MaxChars: 50}
		r.rt.reply = say(strings.Repeat("あ", 200))
		require.NoError(t, r.process())
		require.Equal(t, "SYS max=50", r.rt.reqs[0].Messages[0].Content)
		require.Equal(t, 50, utf8.RuneCountInString(*r.store.text(r.sid)))
	})
	t.Run("zero limit falls back to 300", func(t *testing.T) {
		r := newRig(t)
		r.cfg = summary.Config{}
		require.NoError(t, r.process())
		require.Equal(t, "SYS max=300", r.rt.reqs[0].Messages[0].Content)
	})
	t.Run("long transcript shortens AI lines", func(t *testing.T) {
		r := newRig(t)
		completed := "COMPLETED"
		var turns []summary.TurnRow
		for i := range 40 {
			turns = append(turns, summary.TurnRow{Seq: i, Status: &completed,
				UserText: sp(strings.Repeat("あ", 500)), AssistantText: sp(strings.Repeat("い", 500))})
		}
		r.src.turns = turns
		require.NoError(t, r.process())
		user := r.rt.reqs[0].Messages[1].Content
		require.Contains(t, user, "AI: "+strings.Repeat("い", 150)+"\n")
		require.NotContains(t, user, strings.Repeat("い", 151))
		require.Contains(t, user, strings.Repeat("あ", 500))
	})
}

func TestProcessTurnWindow(t *testing.T) {
	t.Run("session columns win over the payload", func(t *testing.T) {
		r := newRig(t)
		r.p.StartedAt = r.p.StartedAt.Add(-time.Hour)
		r.p.EndedAt = r.p.EndedAt.Add(time.Hour)
		require.NoError(t, r.process())
		require.Equal(t, r.src.info.StartingAt.Add(-time.Minute), r.src.from)
		require.Equal(t, r.src.info.EndedAt.Add(time.Minute), r.src.to)
	})
	t.Run("started_at when starting_at is NULL", func(t *testing.T) {
		r := newRig(t)
		r.src.info.StartingAt = nil
		r.p.StartedAt = r.p.StartedAt.Add(-time.Hour)
		require.NoError(t, r.process())
		require.Equal(t, r.src.info.StartedAt.Add(-time.Minute), r.src.from)
	})
	t.Run("payload when the session has no timestamps", func(t *testing.T) {
		r := newRig(t)
		r.src.info.StartingAt, r.src.info.StartedAt, r.src.info.EndedAt = nil, nil, nil
		require.NoError(t, r.process())
		require.Equal(t, r.p.StartedAt.Add(-time.Minute), r.src.from)
		require.Equal(t, r.p.EndedAt.Add(time.Minute), r.src.to)
	})
}
