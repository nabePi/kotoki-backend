//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
)

func TestTurnRepoConstructors(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	s := p.build(t, nil, nil)
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error { return tx.Sessions().Insert(ctx, s) }))

	turns := postgres.NewTurnRepo(p.pool)
	err := turns.Insert(ctx, 999, session.TurnStart{ID: uuid.New(), SessionID: s.ID, Sequence: 1, Generation: 1, CreatedAt: time.Now().UTC()})
	require.ErrorIs(t, err, session.ErrFenceLost)

	events := postgres.NewEventRepo(p.pool)
	require.NotNil(t, events)
}
