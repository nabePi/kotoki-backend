package conversation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const maxSummaryFailures = 3

// SummaryResult is the SummaryReady payload of the loop (spec 7.8.5).
type SummaryResult struct {
	Text            string
	CoveredUntilSeq int
}

// SummarizerOption configures a Summarizer.
type SummarizerOption func(*Summarizer)

// WithSummaryModel sets TextRequest.Model (ROLLING_SUMMARY_LLM_MODEL; empty lets the router pick
// the active vendor's model).
func WithSummaryModel(model string) SummarizerOption { return func(s *Summarizer) { s.model = model } }

// WithSummarySession sets the session id of TextRequest.RequestID.
func WithSummarySession(id uuid.UUID) SummarizerOption {
	return func(s *Summarizer) { s.sessionID = id }
}

// WithSummaryTrigger sets ROLLING_SUMMARY_TRIGGER_TURNS (default 4).
func WithSummaryTrigger(turns int) SummarizerOption {
	return func(s *Summarizer) {
		if turns > 0 {
			s.trigger = turns
		}
	}
}

// Summarizer runs the in-session rolling summary (spec 7.8.5). Run is synchronous; the loop
// starts it in a goroutine bound to the session context and tracks "running" itself.
type Summarizer struct {
	router    provider.RealtimeRouter
	builder   PromptBuilder
	timeout   time.Duration
	model     string
	sessionID uuid.UUID
	trigger   int

	mu       sync.Mutex
	failures int
}

// NewSummarizer builds a Summarizer with the per-call timeout (ROLLING_SUMMARY_TIMEOUT).
func NewSummarizer(router provider.RealtimeRouter, b PromptBuilder, timeout time.Duration, opts ...SummarizerOption) *Summarizer {
	s := &Summarizer{router: router, builder: b, timeout: timeout, trigger: defaultSummaryTriggerTurns}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ShouldRun reports whether a summary job should start now: no job runs, fewer than three
// consecutive failures, and at least the trigger count of non-DISCARDED turns with
// covered < seq <= oldestSeqInWindow-1.
func (s *Summarizer) ShouldRun(turns []TurnContext, covered, oldestSeqInWindow int, running bool) bool {
	if running {
		return false
	}
	s.mu.Lock()
	stopped := s.failures >= maxSummaryFailures
	s.mu.Unlock()
	return !stopped && uncoveredBefore(turns, covered, oldestSeqInWindow) >= s.trigger
}

// Run summarises in.Turns on top of in.PreviousSummary. A timeout or router error counts as a
// failure (three in a row stop the summarizer for the session); the cancellation of ctx does not.
func (s *Summarizer) Run(ctx context.Context, in SummaryInput) (SummaryResult, error) {
	res, err := s.run(ctx, in)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		s.failures = 0
	case ctx.Err() == nil:
		s.failures++
	}
	return res, err
}

func (s *Summarizer) run(ctx context.Context, in SummaryInput) (SummaryResult, error) {
	built, err := s.builder.BuildRollingSummary(in)
	if err != nil {
		return SummaryResult{}, err
	}
	msgs := make([]provider.Message, len(built.Messages))
	for i, m := range built.Messages {
		msgs[i] = provider.Message{Role: provider.Role(m.Role), Content: m.Content}
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.router.CompleteText(cctx, provider.TextRequest{
		Task: "ROLLING_SUMMARY", Model: s.model, Messages: msgs,
		MaxTokens: rollingSummaryMaxOut, Temperature: rollingSummaryTemp,
		RequestID: "ROLLING_SUMMARY:" + s.sessionID.String(),
	})
	if err != nil {
		return SummaryResult{}, err
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return SummaryResult{}, errors.New("conversation: empty rolling summary")
	}
	return SummaryResult{Text: text, CoveredUntilSeq: coveredUntil(in.Turns)}, nil
}
