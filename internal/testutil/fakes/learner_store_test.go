package fakes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func TestFakeLearnerStore(t *testing.T) {
	repotest.LearnerRepoSuite(t, func(*testing.T) repotest.LearnerHarness {
		return repotest.LearnerHarness{
			Store:   fakes.NewLearnerStore(),
			NewUser: func(*testing.T) uuid.UUID { return uuid.Must(uuid.NewV7()) },
			NewEvaluation: func(*testing.T, uuid.UUID) (uuid.UUID, uuid.UUID) {
				return uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			},
		}
	})
}

func TestLearnerStoreRollsBackProcessed(t *testing.T) {
	s := fakes.NewLearnerStore()
	eid := uuid.Must(uuid.NewV7())
	boom := errors.New("boom")
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		require.NoError(t, tx.Processed().MarkProcessed(ctx, events.QueueLearner, events.Envelope{EventID: eid}))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, s.Processed.Len(), "processed rows are dropped")
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueLearner, events.Envelope{EventID: eid})
	}))
	require.Equal(t, 1, s.Processed.Len())
}

func TestLearnerStoreEvidenceMismatchIsNotFound(t *testing.T) {
	s := fakes.NewLearnerStore()
	owner, other, ev := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	s.SetEvidence(learner.EvaluationEvidence{EvaluationID: ev, UserID: owner})
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx learner.Tx) error {
		_, err := tx.Evaluations().Evidence(ctx, other, ev)
		require.ErrorIs(t, err, learner.ErrNotFound)
		_, err = tx.Evaluations().Evidence(ctx, owner, uuid.Must(uuid.NewV7()))
		require.ErrorIs(t, err, learner.ErrEvaluationNotVisible)
		got, err := tx.Evaluations().Evidence(ctx, owner, ev)
		require.NoError(t, err)
		require.Equal(t, owner, got.UserID)
		return nil
	}))
}
