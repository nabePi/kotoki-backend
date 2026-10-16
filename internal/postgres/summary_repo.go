package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/summary"
)

// summaryTx adapts *Tx to summary.Tx: (*Tx).Sessions already returns session.Repository, so the
// summary port gets its own handle instead of a second method of that name.
type summaryTx struct{ tx *Tx }

var _ summary.Tx = summaryTx{}

func (s summaryTx) Processed() events.ProcessedEvents { return s.tx.Processed() }
func (s summaryTx) Sessions() summary.SessionWriter {
	return &summaryWriter{q: s.tx.Queries(), clock: s.tx.Clock()}
}

type summaryWriter struct {
	q     *sqlcgen.Queries
	clock clock.Clock
}

var _ summary.SessionWriter = (*summaryWriter)(nil)

// SetSummary sets sessions.summary_text only while it is NULL; version and runtime_fence stay.
func (w *summaryWriter) SetSummary(ctx context.Context, id uuid.UUID, text string) (bool, error) {
	n, err := w.q.SetSessionSummaryIfNull(ctx, sqlcgen.SetSessionSummaryIfNullParams{ID: id, Text: pgtype.Text{String: text, Valid: true}, At: tsz(w.clock.Now())})
	if err != nil {
		return false, internalErr(err, "set session summary failed")
	}
	return n > 0, nil
}

type summaryStoreView struct{ s *Store }

// SummaryStore returns the summary.Store view of s.
func (s *Store) SummaryStore() summary.Store { return summaryStoreView{s: s} }

func (v summaryStoreView) WithinTx(ctx context.Context, fn func(ctx context.Context, tx summary.Tx) error) error {
	return v.s.WithinTx(ctx, func(ctx context.Context, tx *Tx) error { return fn(ctx, summaryTx{tx: tx}) })
}

type summarySource struct{ q *sqlcgen.Queries }

// NewSummarySource returns a summary.Source over pool.
func NewSummarySource(pool *pgxpool.Pool) summary.Source { return summarySource{q: sqlcgen.New(pool)} }

func (s summarySource) LoadSession(ctx context.Context, id uuid.UUID) (summary.SessionInfo, error) {
	r, err := s.q.GetSessionForSummary(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return summary.SessionInfo{}, summary.ErrSessionNotFound
	}
	if err != nil {
		return summary.SessionInfo{}, internalErr(err, "load session for summary failed")
	}
	return summary.SessionInfo{
		ID: r.ID, UserID: r.UserID, UserStatus: r.UserStatus, TopicName: r.TopicName, ScenarioName: r.ScenarioName,
		StartingAt: fromTszPtr(r.StartingAt), StartedAt: fromTszPtr(r.StartedAt), EndedAt: fromTszPtr(r.EndedAt),
	}, nil
}

func (s summarySource) LoadTurns(ctx context.Context, id uuid.UUID, from, to time.Time) ([]summary.TurnRow, error) {
	rows, err := s.q.ListTurnsForSummary(ctx, sqlcgen.ListTurnsForSummaryParams{SessionID: id, CreatedFrom: tsz(from), CreatedTo: tsz(to)})
	if err != nil {
		return nil, internalErr(err, "load turns for summary failed")
	}
	out := make([]summary.TurnRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, summary.TurnRow{
			Seq: int(r.SequenceNumber), Status: fromText(r.Status), UserText: fromText(r.UserText),
			AssistantText: fromText(r.AssistantText), AssistantSpokenText: fromText(r.AssistantSpokenText),
		})
	}
	return out, nil
}

type summaryPromptSource struct{ repo content.Repository }

// NewSummaryPromptSource returns a summary.PromptSource that reads the PUBLISHED SUMMARY version.
func NewSummaryPromptSource(pool *pgxpool.Pool) summary.PromptSource {
	return summaryPromptSource{repo: NewContentRepo(pool)}
}

func (p summaryPromptSource) PublishedPrompt(ctx context.Context) (summary.Prompt, error) {
	pv, err := p.repo.GetPublishedPromptVersion(ctx, content.PromptSummary)
	if errors.Is(err, content.ErrNotFound) {
		return summary.Prompt{}, summary.ErrPromptNotFound
	}
	if err != nil {
		return summary.Prompt{}, err
	}
	return summary.Prompt{ID: pv.ID, Template: pv.Template()}, nil
}
