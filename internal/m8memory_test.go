package internal_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// *memory.Consumer must satisfy amqp.Handler structurally (memory never imports amqp).
var _ amqp.Handler = (*memory.Consumer)(nil)

// *memory.Retriever must satisfy memory.RetrievalService (the port of the realtime runtime).
var _ memory.RetrievalService = (*memory.Retriever)(nil)

// TestMemoryConsumerClassification drives the real Consumer and feeds each outcome to
// amqp.Classify, the way the AMQP consumer does after Handle (spec 9.7.3).
func TestMemoryConsumerClassification(t *testing.T) {
	const emptyOut = `{"memories":[],"episode":null}`
	down := &provider.Error{Kind: provider.KindUnavailable, Retryable: true, Message: "503"}
	auth := &provider.Error{Kind: provider.KindAuth, Message: "bad key"}

	type rig struct {
		store *fakes.MemoryStore
		src   *fakes.MemorySource
		prof  *fakes.MemoryProfile
		prov  *fakes.MemoryProvider
		env   *events.Envelope
		user  uuid.UUID
		sess  uuid.UUID
	}
	type scenario struct {
		name      string
		replies   []fakes.MemoryReply
		shape     func(r *rig)
		processed bool
		want      amqp.Class
		check     func(t *testing.T, err error)
	}
	scenarios := []scenario{
		{name: "success", replies: []fakes.MemoryReply{{JSON: emptyOut}}, want: amqp.ClassAck},
		{name: "memory disabled", want: amqp.ClassAck, shape: func(r *rig) { r.prof.Disable(r.user) }},
		{name: "deleted user", want: amqp.ClassAck, shape: func(r *rig) { r.src.MarkDeleted(r.user) },
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, events.ErrSkippedDeletedUser) }},
		{name: "already processed", want: amqp.ClassAck, processed: true, replies: []fakes.MemoryReply{{JSON: emptyOut}},
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, events.ErrAlreadyProcessed) }},
		{name: "too few user turns", want: amqp.ClassAck, shape: func(r *rig) {
			r.src.AddSession(r.sess, memory.SessionInfo{UserID: r.user}, []memory.TurnRow{{Seq: 1, Status: "COMPLETED", UserText: ptrTo("はい")}})
		}},
		{name: "broken payload", want: amqp.ClassDead, shape: func(r *rig) { r.env.Payload = json.RawMessage(`{"session_id": 5`) }},
		{name: "session of another user", want: amqp.ClassDead, shape: func(r *rig) { r.env.UserID = uuid.New() }},
		{name: "unknown session", want: amqp.ClassDead, shape: func(r *rig) { r.env.Payload = sessionPayload(t, uuid.New()) }},
		{name: "invalid output twice", want: amqp.ClassDead, replies: []fakes.MemoryReply{{JSON: "x"}, {JSON: "y"}}},
		{name: "provider down", want: amqp.ClassRetry, replies: []fakes.MemoryReply{{Err: down}}},
		{name: "provider auth", want: amqp.ClassDead, replies: []fakes.MemoryReply{{Err: auth}}},
		{name: "embedding down", want: amqp.ClassRetry, replies: []fakes.MemoryReply{{JSON: validOut}}, shape: func(r *rig) {
			r.prov.FailNext("Embed", down)
		}},
		{name: "database error", want: amqp.ClassRetry, replies: []fakes.MemoryReply{{JSON: emptyOut}}, shape: func(r *rig) {
			r.store.FailNext("LockUser", errors.New("connection reset"))
		}},
		{name: "user lookup error", want: amqp.ClassRetry, shape: func(r *rig) { r.src.FailNext("UserDeleted", errors.New("connection reset")) }},
		{name: "handler timeout", want: amqp.ClassRetry, replies: []fakes.MemoryReply{{JSON: emptyOut}}, shape: func(r *rig) {
			r.store.FailNext("LockUser", context.DeadlineExceeded)
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			clk := clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
			r := &rig{store: fakes.NewMemoryStore(fakes.WithMemoryClock(clk)), src: fakes.NewMemorySource(), prof: fakes.NewMemoryProfile(),
				prov: fakes.NewMemoryProvider(sc.replies...), user: uuid.New(), sess: uuid.New()}
			turns := []memory.TurnRow{
				{ID: uuid.New(), Seq: 1, Status: "COMPLETED", UserText: ptrTo("今日は仕事でした。"), AssistantText: "そうですか。"},
				{ID: uuid.New(), Seq: 2, Status: "COMPLETED", UserText: ptrTo("天気がいいですね。"), AssistantText: "はい。"},
				{ID: uuid.New(), Seq: 3, Status: "COMPLETED", UserText: ptrTo("ありがとうございます。"), AssistantText: "いいえ。"},
			}
			r.src.AddSession(r.sess, memory.SessionInfo{UserID: r.user, EndedAt: clk.Now(), ScenarioName: "自己紹介"}, turns)
			prompts := fakes.NewMemoryPrompts()
			prompts.Set("MEMORY_EXTRACTION", uuid.New(), prompt.Template{Key: "MEMORY_EXTRACTION", Version: 1, System: "s", User: "{{.Transcript}}",
				OutputSchema: json.RawMessage(`{"type":"object"}`)})
			env := events.Envelope{EventID: uuid.New(), EventType: events.SessionCompleted, EventVersion: 1, UserID: r.user,
				Payload: sessionPayload(t, r.sess)}
			r.env = &env
			if sc.shape != nil {
				sc.shape(r)
			}
			if sc.processed {
				require.NoError(t, r.store.Processed.MarkProcessed(t.Context(), events.QueueMemory, env))
			}
			c := memory.NewConsumer(memory.ConsumerDeps{
				Store: r.store, Source: r.src, Settings: r.prof, Prompts: prompts, Provider: r.prov, MemVer: fakes.NewMemVerRecorder(),
				Applier: memory.NewApplier(idgen.NewSequence(t), clk, nil), Clock: clk, IDs: idgen.NewSequence(t),
			})
			err := c.Handle(t.Context(), events.Delivery{Envelope: env, Queue: events.QueueMemory})
			require.Equal(t, sc.want, amqp.Classify(err, false), "err=%v", err)
			if sc.check != nil {
				sc.check(t, err)
			}
		})
	}
}

const validOut = `{"memories":[{"category":"PROFILE_FACT","key":"occupation","content_ja":"エンジニアとして働いている。","display_text_id":"Engineer.","evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.95,"importance":0.9,"is_temporary":false,"valid_days":null,"source_turn_seqs":[1],"sensitive":false}],"episode":null}`

func ptrTo[T any](v T) *T { return &v }

func sessionPayload(t *testing.T, sid uuid.UUID) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(events.SessionCompletedV1{SessionID: sid, FinalStatus: "COMPLETED"})
	require.NoError(t, err)
	return raw
}
