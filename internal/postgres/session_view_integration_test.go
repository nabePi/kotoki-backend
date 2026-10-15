//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestListViewsUsesOwnerFilter(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	mine := p.build(t, nil, nil)
	theirs := p.build(t, nil, nil)
	require.NoError(t, p.store.SessionStore().WithinTx(ctx, func(ctx context.Context, tx session.Tx) error {
		require.NoError(t, tx.Sessions().Insert(ctx, mine))
		return tx.Sessions().Insert(ctx, theirs)
	}))
	views := postgres.NewSessionViewRepo(p.pool)

	got, err := views.ListViews(ctx, mine.UserID, nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, mine.ID, got[0].ID)

	_, err = views.GetView(ctx, mine.UserID, theirs.ID)
	require.ErrorIs(t, err, session.ErrNotFound, "another user's session does not exist for the caller")
	v, err := views.GetView(ctx, mine.UserID, mine.ID)
	require.NoError(t, err)
	require.Equal(t, "Topic", v.TopicName)
	require.Equal(t, "Scenario", v.ScenarioName)
	require.Equal(t, "Haruka", v.CharacterName, "from config_snapshot.character")
	require.Nil(t, v.AvatarKey, "the snapshot has avatar_key null")
	require.True(t, v.StartDeadlineAt.IsZero(), "this seeded session has no reservation (LEFT JOIN)")
}

func TestSessionViewKeysetAndStatusFilter(t *testing.T) {
	p := newPGSession(t)
	ctx := context.Background()
	user := testutil.SeedUser(t, p.pool, nil)
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		status := testutil.Row{}
		if i < 2 {
			status = testutil.Row{"status": "CANCELLED", "end_reason": "USER_CANCELLED", "ended_at": p.build(t, &user, nil).CreatedAt}
		}
		ids = append(ids, testutil.SeedSession(t, p.pool, user, p.content, status))
	}
	views := postgres.NewSessionViewRepo(p.pool)
	all, err := views.ListViews(ctx, user, nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for i := 1; i < len(all); i++ {
		require.Greater(t, all[i-1].ID.String(), all[i].ID.String())
	}
	cancelled, err := views.ListViews(ctx, user, []session.Status{session.StatusCancelled}, nil, 10)
	require.NoError(t, err)
	require.Len(t, cancelled, 2)
	next, err := views.ListViews(ctx, user, nil, &all[0].ID, 10)
	require.NoError(t, err)
	require.Len(t, next, 2)
	require.Equal(t, all[1].ID, next[0].ID)
	_ = ids
}
