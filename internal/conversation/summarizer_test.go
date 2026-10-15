package conversation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// textRouter is a RealtimeRouter whose CompleteText is scripted; the rest is unused.
type textRouter struct {
	provider.RealtimeRouter
	mu   sync.Mutex
	reqs []provider.TextRequest
	fn   func(context.Context, provider.TextRequest) (provider.TextResult, error)
}

func (r *textRouter) CompleteText(ctx context.Context, req provider.TextRequest) (provider.TextResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return r.fn(ctx, req)
}

func seqTurns(seqs ...int) []TurnContext {
	out := make([]TurnContext, len(seqs))
	for i, s := range seqs {
		u := "こんにちは"
		out[i] = TurnContext{Seq: s, Status: "COMPLETED", UserText: &u, AssistantText: "はい"}
	}
	return out
}

func TestSummarizerTrigger(t *testing.T) {
	s := NewSummarizer(&textRouter{}, NewPromptBuilder(), time.Second, uuid.New())
	turns := seqTurns(1, 2, 3, 4, 5, 6)
	// oldest=5: seq<=4 and >0 → 4 turns
	if !s.ShouldRun(turns, 0, 5, false) {
		t.Error("4 uncovered turns must trigger")
	}
	if s.ShouldRun(turns, 0, 5, true) {
		t.Error("running job blocks")
	}
	if s.ShouldRun(turns, 1, 5, false) {
		t.Error("covered=1 leaves 3 turns")
	}
	if s.ShouldRun(turns, 0, 4, false) {
		t.Error("oldest=4 leaves 3 turns")
	}
	d := seqTurns(1, 2, 3, 4, 5)
	d[1].Status = "DISCARDED"
	if s.ShouldRun(d, 0, 5, false) {
		t.Error("DISCARDED turns do not count")
	}
	if got := NewSummarizer(&textRouter{}, NewPromptBuilder(), time.Second, uuid.New(), WithSummaryTrigger(2)); !got.ShouldRun(seqTurns(1, 2), 0, 3, false) {
		t.Error("trigger option ignored")
	}
}

func TestSummarizerRequest(t *testing.T) {
	noLeak(t)
	sid := uuid.New()
	r := &textRouter{fn: func(context.Context, provider.TextRequest) (provider.TextResult, error) {
		return provider.TextResult{Text: "  要約です。\n"}, nil
	}}
	s := NewSummarizer(r, NewPromptBuilder(), time.Second, sid, WithSummaryModel("m-1"))
	turns := seqTurns(2, 3, 4)
	turns[2].Status = "DISCARDED"
	in := SummaryInput{PreviousSummary: "前", Turns: turns, MaxRunes: 300}
	res, err := s.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "要約です。" || res.CoveredUntilSeq != 3 {
		t.Fatalf("got %+v", res)
	}
	q := r.reqs[0]
	if q.Task != "ROLLING_SUMMARY" || q.Model != "m-1" || q.MaxTokens != 400 || q.Temperature != 0.3 ||
		q.RequestID != "ROLLING_SUMMARY:"+sid.String() {
		t.Fatalf("bad request %+v", q)
	}
	if len(q.Messages) != 2 || q.Messages[0].Role != provider.RoleSystem || q.Messages[1].Role != provider.RoleUser {
		t.Fatalf("bad messages %+v", q.Messages)
	}
	built, _ := NewPromptBuilder().BuildRollingSummary(in)
	for i, m := range built.Messages {
		if q.Messages[i].Role != provider.Role(m.Role) || q.Messages[i].Content != m.Content || m.Content == "" {
			t.Fatalf("message %d differs from BuildRollingSummary: %+v", i, q.Messages[i])
		}
	}
	if !strings.Contains(q.Messages[1].Content, "これまでの要約:\n前") {
		t.Fatalf("user message lacks the previous summary: %q", q.Messages[1].Content)
	}
}

func TestSummarizerTimeoutAndStop(t *testing.T) {
	noLeak(t)
	r := &textRouter{fn: func(ctx context.Context, _ provider.TextRequest) (provider.TextResult, error) {
		<-ctx.Done()
		return provider.TextResult{}, ctx.Err()
	}}
	s := NewSummarizer(r, NewPromptBuilder(), 20*time.Millisecond, uuid.New())
	parent, cancelParent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelParent()
	turns := seqTurns(1, 2, 3, 4, 5)
	in := SummaryInput{Turns: turns}
	for i := 0; i < 3; i++ {
		if !s.ShouldRun(turns, 0, 5, false) {
			t.Fatalf("must still run before failure %d", i+1)
		}
		_, err := s.Run(parent, in)
		if !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
			t.Fatalf("want timeout, got %v", err)
		}
	}
	if s.ShouldRun(turns, 0, 5, false) {
		t.Fatal("3 consecutive failures must stop the summarizer")
	}
}

func TestSummarizerSuccessResetsFailures(t *testing.T) {
	fail := true
	r := &textRouter{fn: func(context.Context, provider.TextRequest) (provider.TextResult, error) {
		if fail {
			return provider.TextResult{}, errors.New("boom")
		}
		return provider.TextResult{Text: "ok"}, nil
	}}
	s := NewSummarizer(r, NewPromptBuilder(), time.Second, uuid.New())
	turns := seqTurns(1, 2, 3, 4, 5)
	for i := 0; i < 2; i++ {
		_, _ = s.Run(context.Background(), SummaryInput{Turns: turns})
	}
	fail = false
	if _, err := s.Run(context.Background(), SummaryInput{Turns: turns}); err != nil {
		t.Fatal(err)
	}
	fail = true
	for i := 0; i < 2; i++ {
		_, _ = s.Run(context.Background(), SummaryInput{Turns: turns})
	}
	if !s.ShouldRun(turns, 0, 5, false) {
		t.Fatal("failures must reset after a success")
	}
}

func TestSummarizerCancelNotCounted(t *testing.T) {
	r := &textRouter{fn: func(ctx context.Context, _ provider.TextRequest) (provider.TextResult, error) {
		<-ctx.Done()
		return provider.TextResult{}, ctx.Err()
	}}
	s := NewSummarizer(r, NewPromptBuilder(), time.Minute, uuid.New())
	turns := seqTurns(1, 2, 3, 4, 5)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Run(ctx, SummaryInput{Turns: turns}); err == nil {
			t.Fatal("want error")
		}
	}
	if !s.ShouldRun(turns, 0, 5, false) {
		t.Fatal("session cancellation must not count as failure")
	}
}

func TestSummarizerEmptyTextIsFailure(t *testing.T) {
	r := &textRouter{fn: func(context.Context, provider.TextRequest) (provider.TextResult, error) {
		return provider.TextResult{Text: " \n"}, nil
	}}
	s := NewSummarizer(r, NewPromptBuilder(), time.Second, uuid.New())
	if _, err := s.Run(context.Background(), SummaryInput{Turns: seqTurns(1)}); err == nil {
		t.Fatal("empty summary must fail")
	}
}

func TestSummarizerNilSessionRejected(t *testing.T) {
	r := &textRouter{fn: func(context.Context, provider.TextRequest) (provider.TextResult, error) {
		return provider.TextResult{Text: "ok"}, nil
	}}
	s := NewSummarizer(r, NewPromptBuilder(), time.Second, uuid.Nil)
	if _, err := s.Run(context.Background(), SummaryInput{Turns: seqTurns(1)}); err == nil || len(r.reqs) != 0 {
		t.Fatalf("nil session id must fail before any LLM call: %v", err)
	}
}

func TestSummarizerNonPositiveTimeoutDefaults(t *testing.T) {
	r := &textRouter{fn: func(ctx context.Context, _ provider.TextRequest) (provider.TextResult, error) {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) < 5*time.Second {
			return provider.TextResult{}, errors.New("deadline too short")
		}
		return provider.TextResult{Text: "ok"}, nil
	}}
	for _, d := range []time.Duration{0, -time.Second} {
		s := NewSummarizer(r, NewPromptBuilder(), d, uuid.New())
		if _, err := s.Run(context.Background(), SummaryInput{Turns: seqTurns(1)}); err != nil {
			t.Fatalf("timeout %v: %v", d, err)
		}
	}
}
