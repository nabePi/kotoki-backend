package conversation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/session"
)

type recAppender struct {
	fence int64
	ev    session.Event
	calls int
}

func (r *recAppender) AppendFenced(_ context.Context, fence int64, e session.Event) error {
	r.fence, r.ev = fence, e
	r.calls++
	return nil
}

func TestEventWriterFields(t *testing.T) {
	repo := &recAppender{}
	now := time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC)
	w := NewSessionEventWriter(repo, idgen.NewSequence(t), clock.NewFake(now))
	sid, turn := uuid.New(), uuid.New()

	payload := map[string]any{"generation": 3, "spoken_sentence_count": 2, "stop_ms": 120}
	require.NoError(t, w.Append(context.Background(), 7, sid, "BARGE_IN", &turn, payload))

	require.Equal(t, int64(7), repo.fence)
	e := repo.ev
	require.NotEqual(t, uuid.Nil, e.ID)
	require.Equal(t, sid, e.SessionID)
	require.Equal(t, session.EventBargeIn, e.Type)
	require.Equal(t, session.ActorRealtime, e.Actor)
	require.Nil(t, e.FromStatus)
	require.Nil(t, e.ToStatus)
	require.NotNil(t, e.TurnID)
	require.Equal(t, turn, *e.TurnID)
	require.Equal(t, now, e.CreatedAt)
	require.JSONEq(t, `{"generation":3,"spoken_sentence_count":2,"stop_ms":120}`, string(e.Payload))
	var m map[string]any
	require.NoError(t, json.Unmarshal(e.Payload, &m))
	for k := range m {
		require.NotContains(t, []string{"text", "transcript", "content"}, k)
	}
}

func TestEventWriterNilPayloadAndNoTurn(t *testing.T) {
	repo := &recAppender{}
	w := NewSessionEventWriter(repo, idgen.NewSequence(t), clock.NewFake(time.Now()))
	require.NoError(t, w.Append(context.Background(), 1, uuid.New(), "DIFFICULTY_ADJUSTED", nil, nil))
	require.Nil(t, repo.ev.TurnID)
	require.JSONEq(t, `{}`, string(repo.ev.Payload))
}

func TestEventWriterUnmarshalablePayload(t *testing.T) {
	repo := &recAppender{}
	w := NewSessionEventWriter(repo, idgen.NewSequence(t), clock.NewFake(time.Now()))
	require.Error(t, w.Append(context.Background(), 1, uuid.New(), "BARGE_IN", nil, make(chan int)))
	require.Zero(t, repo.calls)
}
