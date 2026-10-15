package fakes_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

func TestFakeTurnRepoAndEvents(t *testing.T) {
	ctx := t.Context()
	repo := fakes.NewTurnRepo()
	sid := uuid.New()
	maxSeq, _, err := repo.MaxSeqAndGeneration(ctx, sid)
	require.NoError(t, err)
	require.Equal(t, -1, maxSeq)

	t1, t2 := uuid.New(), uuid.New()
	require.NoError(t, repo.Insert(ctx, 1, conversation.TurnStart{ID: t1, SessionID: sid, Sequence: 1, Generation: 1}))
	require.NoError(t, repo.Insert(ctx, 1, conversation.TurnStart{ID: t2, SessionID: sid, Sequence: 2, Generation: 3}))
	require.NoError(t, repo.Finalize(ctx, 1, t1, conversation.TurnFinal{Status: session.TurnCompleted, CompletedAt: time.Now()}))
	n, err := repo.FailInProgress(ctx, 1, sid, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	maxSeq, maxGen, _ := repo.MaxSeqAndGeneration(ctx, sid)
	require.Equal(t, 2, maxSeq)
	require.Equal(t, 3, maxGen)
	repo.SetFence(2)
	require.ErrorIs(t, repo.Insert(ctx, 1, conversation.TurnStart{ID: uuid.New(), SessionID: sid}), session.ErrFenceLost)

	w := fakes.NewEventWriter()
	tid := uuid.New()
	require.NoError(t, w.Append(ctx, 2, sid, "BARGE_IN", &tid, map[string]int{"generation": 1}))
	require.Equal(t, []string{"BARGE_IN"}, w.Types())
	require.JSONEq(t, `{"generation":1}`, string(w.Events()[0].Payload))
	w.SetFence(3)
	require.ErrorIs(t, w.Append(ctx, 2, sid, "X", nil, nil), session.ErrFenceLost)
}

func TestFakeMemoryRetrieval(t *testing.T) {
	m := &fakes.MemoryRetrieval{Context: memory.MemoryContext{CoreTokens: 7}, CoreChanged: true, NewCore: []memory.ContextItem{{Content: "x"}}}
	sid := uuid.New()
	mc, err := m.BuildSessionContext(t.Context(), memory.SessionContextInput{SessionID: sid})
	require.NoError(t, err)
	require.Equal(t, sid, mc.SessionID)
	changed, err := m.ReloadCoreIfChanged(t.Context(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, mc.Core, 1)
}
