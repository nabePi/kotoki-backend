package fakes_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func TestFakeEvaluationStore(t *testing.T) {
	repotest.EvaluationRepoSuite(t, func(*testing.T) repotest.EvaluationHarness {
		s := fakes.NewEvaluationStore()
		return repotest.EvaluationHarness{
			Store: s, Reader: s, PromptVersionID: uuid.Must(uuid.NewV7()),
			NewSession: func(*testing.T) (uuid.UUID, uuid.UUID) { return uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()) },
		}
	})
}

func TestEvaluationStoreRollsBackOutboxAndProcessed(t *testing.T) {
	s := fakes.NewEvaluationStore()
	sid, uid, eid := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	boom := errors.New("boom")
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		_, err := tx.Evaluations().Claim(ctx, evaluation.ClaimInput{ID: uuid.Must(uuid.NewV7()), SessionID: sid, UserID: uid, SourceEventID: eid, RubricVersion: "rubric-v1"})
		require.NoError(t, err)
		require.NoError(t, tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, events.Envelope{EventID: eid}))
		require.NoError(t, tx.Outbox().Enqueue(ctx, events.Event{}))
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, ok := s.Evaluation(sid)
	require.False(t, ok)
	require.Zero(t, s.Outbox.Len(), "enqueued events are dropped")
	require.Zero(t, s.Processed.Len(), "processed rows are dropped")

	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		require.NoError(t, tx.Processed().MarkProcessed(ctx, events.QueueEvaluation, events.Envelope{EventID: eid}))
		return tx.Outbox().Enqueue(ctx, events.Event{})
	}))
	require.Equal(t, 1, s.Outbox.Len())
	require.Equal(t, 1, s.Processed.Len())
}

func TestEvaluationStoreFailNext(t *testing.T) {
	s := fakes.NewEvaluationStore()
	s.FailNext("Claim", errors.New("disk full"))
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx evaluation.Tx) error {
		_, err := tx.Evaluations().Claim(ctx, evaluation.ClaimInput{SessionID: uuid.Must(uuid.NewV7())})
		return err
	})
	require.EqualError(t, err, "disk full")
	_, ok := s.Evaluation(uuid.Nil)
	require.False(t, ok)
}
