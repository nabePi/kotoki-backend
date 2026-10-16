package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// passwordResetTokenRetention is how long a password reset token row is kept (spec 9.9.4 step 5).
const passwordResetTokenRetention = 24 * time.Hour

// RetentionRepo runs the batch deletes of the transcript_retention job (spec 9.9.4). Every method
// is one batch in one transaction and returns the number of rows it removed or purged; the caller
// repeats until a batch comes back short. Cutoffs are computed by the caller: nothing here reads
// the clock, so a cutoff is the exact boundary a row has to be older than.
type RetentionRepo struct{ pool *pgxpool.Pool }

// NewRetentionRepo returns the repository over pool.
func NewRetentionRepo(pool *pgxpool.Pool) *RetentionRepo { return &RetentionRepo{pool: pool} }

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

// inTx runs fn in a transaction that is rolled back on error.
func (r *RetentionRepo) inTx(ctx context.Context, what string, fn func(*sqlcgen.Queries) (int64, error)) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, internalErr(err, what+": begin failed")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	n, err := fn(sqlcgen.New(tx))
	if err != nil {
		return 0, internalErr(err, what+" failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, internalErr(err, what+": commit failed")
	}
	return int(n), nil
}

// PurgeEvaluationDetails deletes the findings and blanks the feedback text of up to batch
// evaluations completed before the given instant; scores and signals are kept. Returns the number
// of evaluations purged.
func (r *RetentionRepo) PurgeEvaluationDetails(ctx context.Context, before time.Time, batch int) (int, error) {
	return r.inTx(ctx, "purge evaluation details", func(q *sqlcgen.Queries) (int64, error) {
		return q.PurgeEvaluationDetails(ctx, sqlcgen.PurgeEvaluationDetailsParams{Before: ts(before), Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
	})
}

// DeleteTurnsBefore deletes up to batch session_turns rows with created_at < cutoff.
func (r *RetentionRepo) DeleteTurnsBefore(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.inTx(ctx, "delete turns", func(q *sqlcgen.Queries) (int64, error) {
		return q.DeleteTurnsBefore(ctx, sqlcgen.DeleteTurnsBeforeParams{Cutoff: ts(cutoff), Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
	})
}

// DeleteEventsBefore deletes up to batch session_events rows with created_at < cutoff.
func (r *RetentionRepo) DeleteEventsBefore(ctx context.Context, cutoff time.Time, batch int) (int, error) {
	return r.inTx(ctx, "delete session events", func(q *sqlcgen.Queries) (int64, error) {
		return q.DeleteEventsBefore(ctx, sqlcgen.DeleteEventsBeforeParams{Cutoff: ts(cutoff), Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
	})
}

// PurgeSessionSummaries NULLs summary_text and sets transcript_purged_at on up to batch sessions
// that ended before the given instant and were not purged yet.
func (r *RetentionRepo) PurgeSessionSummaries(ctx context.Context, before time.Time, batch int) (int, error) {
	return r.inTx(ctx, "purge session summaries", func(q *sqlcgen.Queries) (int64, error) {
		return q.PurgeSessionSummaries(ctx, sqlcgen.PurgeSessionSummariesParams{Before: ts(before), Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
	})
}

// DeleteExpiredTokens deletes up to batch refresh-token families whose every token expired or was
// revoked more than tokenRetention before now, and up to batch password reset tokens older than
// 24 hours. Returns the number of rows deleted from both tables together.
func (r *RetentionRepo) DeleteExpiredTokens(ctx context.Context, now time.Time, tokenRetention time.Duration, batch int) (int, error) {
	return r.inTx(ctx, "delete expired tokens", func(q *sqlcgen.Queries) (int64, error) {
		fam, err := q.DeleteExpiredRefreshTokenFamilies(ctx, sqlcgen.DeleteExpiredRefreshTokenFamiliesParams{
			Cutoff: ts(now.Add(-tokenRetention)), Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
		if err != nil {
			return 0, err
		}
		reset, err := q.DeleteOldPasswordResetTokens(ctx, sqlcgen.DeleteOldPasswordResetTokensParams{
			Cutoff: ts(now.Add(-passwordResetTokenRetention)), Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
		if err != nil {
			return 0, err
		}
		return fam + reset, nil
	})
}
