package fakes_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/repotest"
)

func TestSessionStoreContract(t *testing.T) {
	repotest.RunSessionRepoSuite(t, func(*testing.T) repotest.SessionHarness {
		store := fakes.NewSessionStore(fakes.NewBalanceStore())
		return repotest.SessionHarness{
			Store: store,
			Build: func(_ *testing.T, owner *uuid.UUID) session.Session {
				uid := uuid.Must(uuid.NewV7())
				if owner != nil {
					uid = *owner
				}
				id := uuid.Must(uuid.NewV7())
				return session.Session{
					ID: id, UserID: uid, Status: session.StatusReserved, ConversationStartMode: session.StartUserFirst, DurationSeconds: 300, ReservedSeconds: 300,
					TopicID: uuid.New(), ScenarioID: uuid.New(), ScenarioVersionID: uuid.New(), CharacterID: uuid.New(), PromptVersionID: uuid.New(),
					ConfigSnapshot: json.RawMessage(`{"character":{"code":"HARUKA"},"formality":"POLITE"}`), AdaptiveDifficulty: json.RawMessage(`{}`),
					CreateIdempotencyKey: "idem-" + id.String(), CreateRequestHash: make([]byte, 32), CreatedAt: time.Now().UTC(),
				}
			},
		}
	})
}
