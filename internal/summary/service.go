package summary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	// consumerName is the consumer column of processed_events: the queue name.
	consumerName = events.QueueSummary
	// turnWindowPad widens the created_at window around the session for partition pruning.
	turnWindowPad = time.Minute

	// Code defaults used only when the prompt version has no such llm_params key (spec 9.9.6 step 5).
	defaultMaxTokens   = 400
	defaultTemperature = 0.3
)

// Permanent error codes the service raises (spec 9.7.3).
const (
	codeConfigurationError    = "CONFIGURATION_ERROR"
	codeEntityNotFound        = "ENTITY_NOT_FOUND"
	codeInvariantViolation    = "INVARIANT_VIOLATION"
	codeTranscriptUnavailable = "TRANSCRIPT_UNAVAILABLE"
)

// Deps are the collaborators of Service. Metrics and Log may be nil.
type Deps struct {
	Store   Store
	Source  Source
	Prompts PromptSource
	Router  TextRouter
	Cfg     Config
	Metrics *Metrics
	Log     *slog.Logger
}

// Service writes sessions.summary_text once per session.completed delivery (spec 9.9.6).
type Service struct {
	d Deps
}

// NewService returns a Service over d; a non-positive Cfg.MaxChars becomes the default 300.
func NewService(d Deps) *Service {
	if d.Cfg.MaxChars <= 0 {
		d.Cfg.MaxChars = DefaultMaxChars
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

// Process runs spec 9.9.6 steps 1-7 for one session.completed delivery. Permanent failures are
// *events.PermanentError; ErrAlreadyProcessed and ErrSkippedDeletedUser are acknowledged by the
// framework; any other error (provider failure, empty output, database) is retried.
func (s *Service) Process(ctx context.Context, env events.Envelope, p events.SessionCompletedV1) (err error) {
	ctx, span := otel.Start(ctx, "summary.Process")
	defer otel.End(span, &err)

	info, err := s.d.Source.LoadSession(ctx, p.SessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return events.Permanent(codeEntityNotFound, err)
	}
	if err != nil {
		return err
	}
	if info.UserID != env.UserID {
		return events.Permanent(codeInvariantViolation, errors.New("summary: session user differs from event user"))
	}
	if info.UserStatus == "DELETED" {
		if err := s.markProcessedOnly(ctx, env); err != nil {
			return err
		}
		return events.ErrSkippedDeletedUser
	}
	if p.UserTurnCount == 0 {
		return s.markProcessedOnly(ctx, env)
	}

	pr, err := s.loadPrompt(ctx)
	if err != nil {
		return err
	}

	endedAt := p.EndedAt
	if info.EndedAt != nil {
		endedAt = *info.EndedAt
	}
	from := p.StartedAt
	switch {
	case info.StartingAt != nil:
		from = *info.StartingAt
	case info.StartedAt != nil:
		from = *info.StartedAt
	}
	turns, err := s.d.Source.LoadTurns(ctx, info.ID, from.Add(-turnWindowPad), endedAt.Add(turnWindowPad))
	if err != nil {
		return err
	}
	transcript := BuildTranscript(turns, TranscriptMaxChars)
	if transcript == "" {
		return events.Permanent(codeTranscriptUnavailable, errors.New("summary: no transcript although the session had user turns"))
	}

	rendered, err := prompt.Render(pr.Template, SummaryPromptData{
		MaxChars: s.d.Cfg.MaxChars, TopicName: info.TopicName, ScenarioName: info.ScenarioName,
		UserTurnCount: p.UserTurnCount, Transcript: transcript,
	})
	if err != nil {
		return events.Permanent(codeConfigurationError, err)
	}

	res, err := s.d.Router.CompleteText(ctx, provider.TextRequest{
		Task: "SUMMARY", RequestID: "SUMMARY:" + info.ID.String(),
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: rendered.System},
			{Role: provider.RoleUser, Content: rendered.User},
		},
		MaxTokens:   pr.Template.Params.MaxOutputTokensOr(defaultMaxTokens),
		Temperature: pr.Template.Params.TemperatureOr(defaultTemperature),
	})
	if err != nil {
		return err
	}
	text, err := CleanSummary(res.Text, s.d.Cfg.MaxChars)
	if err != nil {
		return err
	}

	var written bool
	err = s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Processed().MarkProcessed(ctx, consumerName, env); err != nil {
			return err
		}
		var err error
		written, err = tx.Sessions().SetSummary(ctx, info.ID, text)
		return err
	})
	if err != nil {
		return err
	}
	// No user, AI or prompt text in the log (spec Global Constraints).
	s.d.Log.InfoContext(ctx, "session summary stored", "session_id", info.ID, "prompt_version_id", pr.ID,
		"provider", res.Provider, "model", res.Model, "written", written)
	return nil
}

// markProcessedOnly records the event as processed without any other effect.
func (s *Service) markProcessedOnly(ctx context.Context, env events.Envelope) error {
	return s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.Processed().MarkProcessed(ctx, consumerName, env)
	})
}

// loadPrompt reads the PUBLISHED SUMMARY version; a missing version or user template is a
// configuration error.
func (s *Service) loadPrompt(ctx context.Context) (Prompt, error) {
	pr, err := s.d.Prompts.PublishedPrompt(ctx)
	if errors.Is(err, ErrPromptNotFound) {
		return pr, events.Permanent(codeConfigurationError, err)
	}
	if err != nil {
		return pr, err
	}
	if pr.Template.User == "" {
		return pr, events.Permanent(codeConfigurationError, errors.New("summary: SUMMARY prompt lacks user_template"))
	}
	return pr, nil
}

// RecordFailure is the OnFailure side (spec 9.9.6 step 8): a log line and a counter, no database
// state. It never logs user text.
func (s *Service) RecordFailure(ctx context.Context, env events.Envelope, cause error, final bool) {
	reason := FailureReason(cause)
	s.d.Metrics.Failure(reason)
	s.d.Log.WarnContext(ctx, "summary delivery failed", "event_id", env.EventID, "session_id", env.AggregateID,
		"reason", reason, "final", final, "error", fmt.Sprint(cause))
}
