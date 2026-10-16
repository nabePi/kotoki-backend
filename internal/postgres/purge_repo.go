package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

// PurgeRepo runs the steps of the account purge (spec 13.9.3). Every method is idempotent and
// commits independently, so the job can start a crashed purge again from the first step. Balance
// accounts, reservations, the ledger and the sessions rows are never touched, except that
// ClearSummaries NULLs sessions.summary_text.
type PurgeRepo struct{ pool *pgxpool.Pool }

// NewPurgeRepo returns the repository over pool.
func NewPurgeRepo(pool *pgxpool.Pool) *PurgeRepo { return &PurgeRepo{pool: pool} }

// ListCandidates returns up to limit users with status DELETED, purged_at NULL and deleted_at
// before deletedBefore, oldest deleted_at first.
func (r *PurgeRepo) ListCandidates(ctx context.Context, deletedBefore time.Time, limit int) ([]uuid.UUID, error) {
	ids, err := sqlcgen.New(r.pool).ListPurgeCandidates(ctx, sqlcgen.ListPurgeCandidatesParams{
		DeletedBefore: ts(deletedBefore), MaxUsers: int32(limit), //nolint:gosec // G115: limit is a small constant
	})
	if err != nil {
		return nil, internalErr(err, "list purge candidates failed")
	}
	return ids, nil
}

// inTx runs fn in one transaction, rolled back on error.
func (r *PurgeRepo) inTx(ctx context.Context, what string, fn func(*sqlcgen.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return internalErr(err, what+": begin failed")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(sqlcgen.New(tx)); err != nil {
		return internalErr(err, what+" failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return internalErr(err, what+": commit failed")
	}
	return nil
}

// DeleteMemories deletes every memory of the user, tombstones included.
func (r *PurgeRepo) DeleteMemories(ctx context.Context, userID uuid.UUID) error {
	_, err := sqlcgen.New(r.pool).PurgeDeleteMemories(ctx, userID)
	if err != nil {
		return internalErr(err, "purge memories failed")
	}
	return nil
}

// DeleteEvaluations deletes the findings, then the evaluations, of the user. Learner rows that
// point at an evaluation follow their foreign keys (last_evaluation_id is nulled, estimates cascade).
func (r *PurgeRepo) DeleteEvaluations(ctx context.Context, userID uuid.UUID) error {
	return r.inTx(ctx, "purge evaluations", func(q *sqlcgen.Queries) error {
		if _, err := q.PurgeDeleteFindings(ctx, userID); err != nil {
			return err
		}
		_, err := q.PurgeDeleteEvaluations(ctx, userID)
		return err
	})
}

// DeleteLearner deletes proficiency estimates, skill states and the learner profile.
func (r *PurgeRepo) DeleteLearner(ctx context.Context, userID uuid.UUID) error {
	return r.inTx(ctx, "purge learner", func(q *sqlcgen.Queries) error {
		if _, err := q.PurgeDeleteProficiencyEstimates(ctx, userID); err != nil {
			return err
		}
		if _, err := q.PurgeDeleteSkillStates(ctx, userID); err != nil {
			return err
		}
		_, err := q.PurgeDeleteLearnerProfile(ctx, userID)
		return err
	})
}

// sessionIDs lists the ids of every session of the user.
func (r *PurgeRepo) sessionIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := sqlcgen.New(r.pool).PurgeSessionIDs(ctx, userID)
	if err != nil {
		return nil, internalErr(err, "list sessions of user failed")
	}
	return ids, nil
}

// DeleteTranscripts deletes the session_turns, then the session_events, of every session of the
// user, batch rows per transaction. The sessions rows stay.
func (r *PurgeRepo) DeleteTranscripts(ctx context.Context, userID uuid.UUID, batch int) error {
	ids, err := r.sessionIDs(ctx, userID)
	if err != nil || len(ids) == 0 {
		return err
	}
	for _, del := range []struct {
		what string
		run  func(*sqlcgen.Queries) (int64, error)
	}{
		{"purge session turns", func(q *sqlcgen.Queries) (int64, error) {
			return q.PurgeDeleteTurns(ctx, sqlcgen.PurgeDeleteTurnsParams{SessionIds: ids, Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
		}},
		{"purge session events", func(q *sqlcgen.Queries) (int64, error) {
			return q.PurgeDeleteSessionEvents(ctx, sqlcgen.PurgeDeleteSessionEventsParams{SessionIds: ids, Batch: int32(batch)}) //nolint:gosec // G115: batch is a small constant
		}},
	} {
		for {
			var n int64
			if err := r.inTx(ctx, del.what, func(q *sqlcgen.Queries) (err error) { n, err = del.run(q); return err }); err != nil {
				return err
			}
			if n < int64(batch) {
				break
			}
		}
	}
	return nil
}

// ClearSummaries NULLs sessions.summary_text of every session of the user.
func (r *PurgeRepo) ClearSummaries(ctx context.Context, userID uuid.UUID) error {
	if _, err := sqlcgen.New(r.pool).PurgeClearSummaries(ctx, userID); err != nil {
		return internalErr(err, "purge session summaries failed")
	}
	return nil
}

// Anonymize rewrites the user row (email deleted+<id>@deleted.invalid, phone NULL, full_name
// 'deleted', password_hash '!') and sets purged_at = now. It does nothing for a user that is
// already purged and fails for a user that is not in the DELETED state.
func (r *PurgeRepo) Anonymize(ctx context.Context, userID uuid.UUID, now time.Time) error {
	q := sqlcgen.New(r.pool)
	n, err := q.PurgeAnonymizeUser(ctx, sqlcgen.PurgeAnonymizeUserParams{UserID: userID, PurgedAt: ts(now)})
	if err != nil {
		return internalErr(err, "anonymize user failed")
	}
	if n > 0 {
		return nil
	}
	purged, err := q.PurgeUserIsPurged(ctx, userID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return internalErr(err, "anonymize user: lookup failed")
	}
	if err == nil && purged {
		return nil
	}
	return internalErr(errors.New("user is not in DELETED state or does not exist"), "anonymize user failed")
}
