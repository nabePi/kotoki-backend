package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var errStoreDown = errors.New("connection reset")

func fixtureJSON(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../provider/fake/fixtures/kotoki_memory_extraction_v1.json")
	require.NoError(t, err)
	return string(raw)
}

type consumerRig struct {
	t       *testing.T
	store   *fakes.MemoryStore
	src     *fakes.MemorySource
	prof    *fakes.MemoryProfile
	prompts *fakes.MemoryPrompts
	prov    *fakes.MemoryProvider
	ver     *fakes.MemVerRecorder
	reg     *prometheus.Registry
	cons    *memory.Consumer
	user    uuid.UUID
	session uuid.UUID
	pv      uuid.UUID
}

func userTurns(texts ...string) []memory.TurnRow {
	rows := []memory.TurnRow{{ID: uuidFor(100), Seq: 0, Status: "COMPLETED", AssistantText: "こんにちは！今日はどんな一日でしたか？"}}
	for i, tx := range texts {
		rows = append(rows, memory.TurnRow{ID: uuidFor(101 + i), Seq: i + 1, Status: "COMPLETED", UserText: ptr(tx), AssistantText: "そうなんですね。"})
	}
	return rows
}

var fiveTurns = []string{"バックエンドエンジニアです。", "ゆっくり話してください。", "私は「は」と「が」を間違えます。", "来月、面接があります。", "ありがとうございました。"}

func newConsumerRig(t *testing.T, replies ...fakes.MemoryReply) *consumerRig {
	t.Helper()
	tpl, _ := seedV1(t)
	clk := clock.NewFake(t0)
	m, reg := newMetrics(t)
	r := &consumerRig{
		t: t, store: fakes.NewMemoryStore(fakes.WithMemoryClock(clk)), src: fakes.NewMemorySource(), prof: fakes.NewMemoryProfile(),
		prompts: fakes.NewMemoryPrompts(), prov: fakes.NewMemoryProvider(replies...), ver: fakes.NewMemVerRecorder(), reg: reg,
		user: uuidFor(1), session: uuidFor(2), pv: uuid.MustParse(memoryPVID),
	}
	r.src.Store, r.src.Clock = r.store, clk
	r.prompts.Set("MEMORY_EXTRACTION", r.pv, tpl)
	r.src.AddSession(r.session, memory.SessionInfo{UserID: r.user, EndedAt: t0, ScenarioName: "自己紹介"}, userTurns(fiveTurns...))
	r.cons = memory.NewConsumer(memory.ConsumerDeps{
		Store: r.store, Source: r.src, Settings: r.prof, Prompts: r.prompts, Provider: r.prov, MemVer: r.ver,
		Applier: memory.NewApplier(idgen.NewSequence(t), clk, m), Clock: clk, IDs: idgen.NewSequence(t), Metrics: m,
	})
	return r
}

func (r *consumerRig) delivery() events.Delivery {
	r.t.Helper()
	return r.deliveryFor(r.user, r.session, uuid.New())
}

func (r *consumerRig) deliveryFor(user, session, eventID uuid.UUID) events.Delivery {
	r.t.Helper()
	ev := events.NewSessionCompleted(eventID, t0, events.ProducerRealtime, user, events.SessionCompletedPayload{
		SessionID: session, FinalStatus: "COMPLETED", EndReason: "USER_ENDED", StartedAt: t0.Add(-time.Minute), EndedAt: t0,
	})
	env, err := ev.Envelope("", nil)
	require.NoError(r.t, err)
	return events.Delivery{Envelope: env, Queue: events.QueueMemory}
}

func (r *consumerRig) handle(d events.Delivery) error { return r.cons.Handle(context.Background(), d) }

func (r *consumerRig) requireNoWrites() {
	r.t.Helper()
	require.Empty(r.t, r.store.All(r.user))
	require.Zero(r.t, r.store.Outbox.Len())
	require.Empty(r.t, r.ver.Bumps())
}

func (r *consumerRig) payload(ev events.Event) events.MemoryExtractedV1 {
	r.t.Helper()
	p, ok := ev.Payload.(events.MemoryExtractedV1)
	require.True(r.t, ok, "payload is %T", ev.Payload)
	return p
}

func TestConsumer_Identity(t *testing.T) {
	c := newConsumerRig(t).cons
	require.Equal(t, events.QueueMemory, c.Queue())
	require.Equal(t, map[string]int{"session.completed": 1}, c.SupportedEvents())
	require.Equal(t, 120*time.Second, c.Timeout())
}

func TestConsumer_SkipsWhenFewerThan3UserTurns(t *testing.T) {
	r := newConsumerRig(t)
	// five user turns, but only two reach 5 runes
	r.src.AddSession(r.session, memory.SessionInfo{UserID: r.user, EndedAt: t0},
		userTurns("はい", "ええ", "そうですね、今日は仕事でした。", "天気がいいですね。", "うん"))
	require.NoError(t, r.handle(r.delivery()))
	require.Empty(t, r.prov.Requests())
	require.Equal(t, 1, r.store.Processed.Len(), "the event is recorded as processed")
	r.requireNoWrites()
}

func TestConsumer_ThreeLongUserTurnsAreEnough(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `{"memories":[],"episode":null}`})
	r.src.AddSession(r.session, memory.SessionInfo{UserID: r.user, EndedAt: t0},
		userTurns("今日は仕事でした。", "はい", "天気がいいですね。", "うん", "ありがとうございます。"))
	require.NoError(t, r.handle(r.delivery()))
	require.Len(t, r.prov.Requests(), 1, "exactly 3 user turns of 5 runes or more reach the LLM")
}

func TestConsumer_MemoryDisabled_MarksProcessedOnly(t *testing.T) {
	r := newConsumerRig(t)
	r.prof.Disable(r.user)
	require.NoError(t, r.handle(r.delivery()))
	require.Empty(t, r.prov.Requests())
	require.Equal(t, 1, r.store.Processed.Len())
	r.requireNoWrites()
}

func TestConsumer_DeletedUser_NoProviderCall(t *testing.T) {
	r := newConsumerRig(t)
	r.src.MarkDeleted(r.user)
	err := r.handle(r.delivery())
	require.ErrorIs(t, err, events.ErrSkippedDeletedUser)
	require.Empty(t, r.prov.Requests())
	require.Equal(t, 1, r.store.Processed.Len(), "only MarkProcessed is written")
	r.requireNoWrites()
}

func TestConsumer_UserMismatch_PermanentAndNoWrite(t *testing.T) {
	r := newConsumerRig(t)
	other := uuidFor(77)
	err := r.handle(r.deliveryFor(other, r.session, uuid.New())) // the session belongs to r.user
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, "INVARIANT_VIOLATION", perm.Code)
	require.Empty(t, r.prov.Requests())
	require.Zero(t, r.store.Processed.Len())
	require.Empty(t, r.store.All(other))
	r.requireNoWrites()
}

func TestConsumer_UnknownSession_Permanent(t *testing.T) {
	r := newConsumerRig(t)
	err := r.handle(r.deliveryFor(r.user, uuidFor(55), uuid.New()))
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, "ENTITY_NOT_FOUND", perm.Code)
}

func TestConsumer_MissingPromptConfig_Permanent(t *testing.T) {
	tpl, _ := seedV1(t)
	noUser, noSchema := tpl, tpl
	noUser.User = ""
	noSchema.OutputSchema = nil
	for name, set := range map[string]func(*fakes.MemoryPrompts){
		"no published version": func(*fakes.MemoryPrompts) {},
		"user_template NULL":   func(p *fakes.MemoryPrompts) { p.Set("MEMORY_EXTRACTION", uuidFor(9), noUser) },
		"output_schema NULL":   func(p *fakes.MemoryPrompts) { p.Set("MEMORY_EXTRACTION", uuidFor(9), noSchema) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newConsumerRig(t)
			r.prompts = fakes.NewMemoryPrompts()
			set(r.prompts)
			cons := memory.NewConsumer(memory.ConsumerDeps{
				Store: r.store, Source: r.src, Settings: r.prof, Prompts: r.prompts, Provider: r.prov, MemVer: r.ver,
				Applier: memory.NewApplier(idgen.NewSequence(t), clock.NewFake(t0), nil), Clock: clock.NewFake(t0), IDs: idgen.NewSequence(t),
			})
			err := cons.Handle(context.Background(), r.delivery())
			var perm *events.PermanentError
			require.ErrorAs(t, err, &perm)
			require.Equal(t, "CONFIGURATION_ERROR", perm.Code)
			require.Empty(t, r.prov.Requests())
			r.requireNoWrites()
		})
	}
}

func TestConsumer_InvalidJSONThenRepair_Succeeds(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `{"memories": [`}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	reqs := r.prov.Requests()
	require.Len(t, reqs, 2)
	require.Len(t, reqs[0].Messages, 2)
	require.Len(t, reqs[1].Messages, 4, "system, user, the rejected output, the repair instruction")
	require.Equal(t, provider.RoleAssistant, reqs[1].Messages[2].Role)
	require.Equal(t, `{"memories": [`, reqs[1].Messages[2].Content)
	rep := reqs[1].Messages[3]
	require.Equal(t, provider.RoleUser, rep.Role)
	require.True(t, strings.HasPrefix(rep.Content, "The previous output was invalid: "), rep.Content)
	require.True(t, strings.HasSuffix(rep.Content, ". Return corrected JSON only."), rep.Content)
	require.NotEmpty(t, r.store.All(r.user))
}

func TestConsumer_SchemaViolationThenRepair_Succeeds(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `{"memories":[{"category":"PROFILE_FACT"}],"episode":null}`}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	require.Len(t, r.prov.Requests(), 2)
	require.Contains(t, r.prov.Requests()[1].Messages[3].Content, "schema violation")
}

func TestConsumer_InvalidResponseKindIsRepairedWithoutAssistantEcho(t *testing.T) {
	bad := &provider.Error{Kind: provider.KindInvalidResponse, Retryable: true, Message: "empty"}
	r := newConsumerRig(t, fakes.MemoryReply{Err: bad}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	reqs := r.prov.Requests()
	require.Len(t, reqs, 2)
	require.Len(t, reqs[1].Messages, 3, "nothing to echo: system, user, repair instruction")
	require.Equal(t, provider.RoleUser, reqs[1].Messages[2].Role)
}

func TestConsumer_InvalidTwice_PermanentDLQ(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `nope`}, fakes.MemoryReply{JSON: `{"memories":"x"}`}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	err := r.handle(r.delivery())
	var perm *events.PermanentError
	require.ErrorAs(t, err, &perm)
	require.Equal(t, "INVALID_MODEL_OUTPUT", perm.Code)
	require.Len(t, r.prov.Requests(), 2, "main call plus one repair, never a third")
	require.Zero(t, r.store.Processed.Len())
	r.requireNoWrites()
}

func TestConsumer_ProviderUnavailable_NoRepairNoFallback(t *testing.T) {
	down := &provider.Error{Kind: provider.KindUnavailable, Retryable: true, Message: "503"}
	r := newConsumerRig(t, fakes.MemoryReply{Err: down}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	err := r.handle(r.delivery())
	require.Error(t, err)
	require.True(t, provider.IsRetryable(err), "left to the retry queue of the framework")
	require.Len(t, r.prov.Requests(), 1)
	require.Zero(t, r.store.Processed.Len())
	r.requireNoWrites()
}

func TestConsumer_ProviderErrorClassification(t *testing.T) {
	auth := &provider.Error{Kind: provider.KindAuth, Message: "bad key"}
	r := newConsumerRig(t, fakes.MemoryReply{Err: auth})
	err := r.handle(r.delivery())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.False(t, provider.IsRetryable(err), "a non-retryable provider error stays non-retryable for the framework")
	require.Len(t, r.prov.Requests(), 1)
}

func TestConsumer_EmbedFailure_Retryable(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	r.prov.FailNext("Embed", &provider.Error{Kind: provider.KindUnavailable, Retryable: true, Message: "embedding down"})
	err := r.handle(r.delivery())
	require.Error(t, err)
	require.True(t, provider.IsRetryable(err))
	require.Len(t, r.prov.Requests(), 1)
	require.Zero(t, r.store.Processed.Len())
	r.requireNoWrites()
}

func TestConsumer_StoreFailureLeavesNothing(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	r.store.FailNext("Insert", errStoreDown)
	require.ErrorIs(t, r.handle(r.delivery()), errStoreDown)
	require.Zero(t, r.store.Processed.Len(), "MarkProcessed rolls back with the transaction")
	r.requireNoWrites()
}

func TestConsumer_RequestShape(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	reqs := r.prov.Requests()
	require.Len(t, reqs, 1)
	q := reqs[0]
	tpl, _ := seedV1(t)
	require.Equal(t, "MEMORY_EXTRACTION", q.Task)
	require.Equal(t, "kotoki_memory_extraction_v1", q.SchemaName)
	require.Equal(t, 0.0, q.Temperature)
	require.Equal(t, 2000, q.MaxOutputTokens, "code default when llm_params has no max_output_tokens")
	require.Nil(t, q.Seed)
	require.Equal(t, "MEMORY_EXTRACTION:"+r.session.String(), q.RequestID)
	require.JSONEq(t, string(tpl.OutputSchema), string(q.JSONSchema))
	require.Len(t, q.Messages, 2)
	require.Equal(t, provider.RoleSystem, q.Messages[0].Role)
	require.Equal(t, provider.RoleUser, q.Messages[1].Role)
	require.Contains(t, q.Messages[1].Content, "SESSION DATE: 2026-10-16\n")
	require.Contains(t, q.Messages[1].Content, "SCENARIO: 自己紹介\n")
	require.Contains(t, q.Messages[1].Content, "[seq=1] USER: バックエンドエンジニアです。")

	emb := r.prov.EmbedRequests()
	require.Len(t, emb, 1, "all candidates in one batch")
	require.Equal(t, 1536, emb[0].Dimensions)
	require.Len(t, emb[0].Texts, 4)
}

func TestConsumer_RequestUsesLLMParamsOfThePromptVersion(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `{"memories":[],"episode":null}`})
	tpl, _ := seedV1(t)
	temp, maxTok, seed := 0.3, 1234, int64(7)
	tpl.Version = 3
	tpl.Params = prompt.Params{Temperature: &temp, MaxOutputTokens: &maxTok, Seed: &seed}
	r.prompts.Set("MEMORY_EXTRACTION", r.pv, tpl)
	require.NoError(t, r.handle(r.delivery()))
	q := r.prov.Requests()[0]
	require.Equal(t, "kotoki_memory_extraction_v3", q.SchemaName)
	require.InDelta(t, 0.3, q.Temperature, 1e-9)
	require.Equal(t, 1234, q.MaxOutputTokens)
	require.NotNil(t, q.Seed)
	require.Equal(t, int64(7), *q.Seed)
}

func TestConsumer_PromptContainsNoUserIdentity(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	for _, m := range r.prov.Requests()[0].Messages {
		require.NotContains(t, m.Content, r.user.String())
		require.NotContains(t, strings.ToLower(m.Content), "full_name")
	}
}

func TestConsumer_ExistingMemoriesReachThePrompt(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `{"memories":[],"episode":null}`})
	r.store.Put(memory.Memory{
		ID: uuidFor(300), UserID: r.user, Category: memory.CategoryProfileFact, Key: "occupation", Content: "エンジニア。",
		Status: memory.StatusActive, LastObservedAt: t0, ExpiresAt: t0.Add(time.Hour),
	})
	require.NoError(t, r.handle(r.delivery()))
	require.Contains(t, r.prov.Requests()[0].Messages[1].Content, "PROFILE_FACT | occupation | エンジニア。\n")
}

func TestConsumer_ExtractsAndStores(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	rows := r.store.All(r.user)
	require.Len(t, rows, 4, "3 memories and the episode")
	for _, m := range rows {
		require.Equal(t, memory.StatusActive, m.Status)
		require.NotNil(t, m.ExtractionPromptVersionID)
		require.Equal(t, r.pv, *m.ExtractionPromptVersionID)
		require.Len(t, m.Embedding, 1536)
		require.Equal(t, "fake-embedding", m.EmbeddingModel)
		require.NotEmpty(t, m.SourceTurnIDs)
		require.NotNil(t, m.SourceSessionID)
		require.Equal(t, r.session, *m.SourceSessionID)
	}
	require.Equal(t, [][]uuid.UUID{{r.user}}, r.ver.Bumps(), "memver is bumped once after the commit")
}

func TestConsumer_EmitsMemoryExtractedWithTheTouchedRows(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	evs := r.store.Outbox.Events()
	require.Len(t, evs, 1)
	require.Equal(t, events.MemoryExtracted, evs[0].EventType)
	require.Equal(t, events.ProducerWorker, evs[0].Producer)
	require.Equal(t, r.session, evs[0].AggregateID)
	require.Equal(t, r.user, evs[0].UserID)
	p := r.payload(evs[0])
	require.Equal(t, r.session, p.SessionID)
	require.Equal(t, r.pv, p.PromptVersionID)
	require.Equal(t, "FAKE", p.Provider)
	require.Equal(t, "FAKE-EVALUATION", p.Model)
	require.Empty(t, p.UpdatedMemoryIDs)
	require.Empty(t, p.SupersededMemoryIDs)
	require.Equal(t, map[string]int{"PROFILE_FACT": 2, "LEARNING_PATTERN": 1, "EPISODIC": 1}, p.CategoryCounts)
	var stored []uuid.UUID
	for _, m := range r.store.All(r.user) {
		stored = append(stored, m.ID)
	}
	require.ElementsMatch(t, stored, p.CreatedMemoryIDs)
}

func TestConsumer_EmitsMemoryExtractedEvenWhenEmpty(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: `{"memories":[],"episode":null}`})
	require.NoError(t, r.handle(r.delivery()))
	evs := r.store.Outbox.Events()
	require.Len(t, evs, 1)
	p := r.payload(evs[0])
	require.Empty(t, p.CreatedMemoryIDs)
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"created_memory_ids":[]`, "never null")
	require.Empty(t, r.prov.EmbedRequests(), "nothing to embed")
	require.Equal(t, 1, r.store.Processed.Len())
	require.Len(t, r.ver.Bumps(), 1)
}

func TestConsumer_InvalidCandidatesAreDropped(t *testing.T) {
	out := `{"memories":[
	 {"category":"PROFILE_FACT","key":"occupation","content_ja":"バックエンドエンジニアとして働いている。","display_text_id":"Backend engineer.","evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.95,"importance":0.9,"is_temporary":false,"valid_days":null,"source_turn_seqs":[1],"sensitive":false},
	 {"category":"PROFILE_FACT","key":"religion","content_ja":"イスラム教を信じている。","display_text_id":"Beragama.","evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.95,"importance":0.9,"is_temporary":false,"valid_days":null,"source_turn_seqs":[1],"sensitive":true},
	 {"category":"PROFILE_FACT","key":"not_in_registry_at_all","content_ja":"何か。","display_text_id":"Sesuatu.","evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.95,"importance":0.9,"is_temporary":false,"valid_days":null,"source_turn_seqs":[1],"sensitive":false},
	 {"category":"PREFERENCE","key":"pref:speech_speed","content_ja":"ゆっくり話してほしい。","display_text_id":"Bicara pelan.","evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.4,"importance":0.5,"is_temporary":false,"valid_days":null,"source_turn_seqs":[2],"sensitive":false},
	 {"category":"PREFERENCE","key":"pref:formality","content_ja":"丁寧語で話してほしい。","display_text_id":"Bahasa sopan.","evidence_type":"EXPLICIT_USER_STATEMENT","confidence":0.9,"importance":0.5,"is_temporary":false,"valid_days":null,"source_turn_seqs":[99],"sensitive":false}
	],"episode":null}`
	r := newConsumerRig(t, fakes.MemoryReply{JSON: out})
	require.NoError(t, r.handle(r.delivery()))
	rows := r.store.All(r.user)
	require.Len(t, rows, 1, "only the valid candidate is stored; the invalid ones are dropped, not fatal")
	require.Equal(t, "occupation", rows[0].Key)
	require.Equal(t, 1, r.store.Processed.Len())
	rej := rejectedCounts(t, r.reg)
	require.Equal(t, 1.0, rej[memory.ReasonSensitiveFlagged])
	require.Equal(t, 1.0, rej[memory.ReasonKeyNotAllowed])
	require.Equal(t, 1.0, rej[memory.ReasonLowConfidence])
	require.Equal(t, 1.0, rej[memory.ReasonNoUserEvidence])
}

func TestConsumer_RedeliveryDoesNotDoubleCount(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	d := r.delivery()
	require.NoError(t, r.handle(d))
	err := r.handle(d)
	require.ErrorIs(t, err, events.ErrAlreadyProcessed)
	for _, m := range r.store.All(r.user) {
		require.Equal(t, 1, m.ObservationCount)
	}
	require.Equal(t, 1, r.store.Outbox.Len())
	require.Len(t, r.ver.Bumps(), 1)
}

func TestConsumer_SecondSessionReinforcesAndNamesTheRows(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	second := uuidFor(3)
	r.src.AddSession(second, memory.SessionInfo{UserID: r.user, EndedAt: t0.Add(time.Hour), ScenarioName: "自己紹介"}, userTurns(fiveTurns...))
	require.NoError(t, r.handle(r.deliveryFor(r.user, second, uuid.New())))

	var active int
	for _, m := range r.store.All(r.user) {
		if m.Status != memory.StatusActive {
			continue
		}
		active++
		if m.Category != memory.CategoryEpisodic {
			require.Equal(t, 2, m.ObservationCount, m.Key)
		}
	}
	require.Equal(t, 4, active, "no duplicate slot: 3 memories reinforced, the episode superseded")
	evs := r.store.Outbox.Events()
	require.Len(t, evs, 2)
	p := r.payload(evs[1])
	require.Len(t, p.UpdatedMemoryIDs, 3)
	require.Len(t, p.SupersededMemoryIDs, 1)
	require.Len(t, p.CreatedMemoryIDs, 1, "the new episode")
	require.Equal(t, map[string]int{"EPISODIC": 1}, p.CategoryCounts)
	require.Equal(t, int64(2), r.ver.Version(r.user))
}

func TestConsumer_TombstoneBlocksReextract(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)}, fakes.MemoryReply{JSON: fixtureJSON(t)})
	require.NoError(t, r.handle(r.delivery()))
	var occ memory.Memory
	for _, m := range r.store.All(r.user) {
		if m.Key == "occupation" {
			occ = m
		}
	}
	require.NoError(t, r.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		ok, err := tx.Memories().Delete(ctx, r.user, occ.ID, t0)
		require.True(t, ok)
		return err
	}))
	second := uuidFor(3)
	r.src.AddSession(second, memory.SessionInfo{UserID: r.user, EndedAt: t0.Add(time.Hour)}, userTurns(fiveTurns...))
	require.NoError(t, r.handle(r.deliveryFor(r.user, second, uuid.New())))
	for _, m := range r.store.All(r.user) {
		if m.Key == "occupation" {
			require.Equal(t, memory.StatusDeleted, m.Status, "only the tombstone remains; the slot is not recreated")
		}
	}
}

func TestConsumer_DecodeErrorIsPermanent(t *testing.T) {
	r := newConsumerRig(t)
	d := r.delivery()
	d.Envelope.Payload = json.RawMessage(`{"session_id": 5`)
	var perm *events.PermanentError
	require.ErrorAs(t, r.handle(d), &perm)
	require.Equal(t, "DECODE_ERROR", perm.Code)
}

func TestConsumer_SourceErrorsAreReturnedForRetry(t *testing.T) {
	for _, method := range []string{"UserDeleted", "Session", "Turns", "Existing", "EnsureMemoryEnabled", "Published"} {
		t.Run(method, func(t *testing.T) {
			r := newConsumerRig(t)
			switch method {
			case "EnsureMemoryEnabled":
				r.prof.FailNext(method, errStoreDown)
			case "Published":
				r.prompts.FailNext(method, errStoreDown)
			default:
				r.src.FailNext(method, errStoreDown)
			}
			require.ErrorIs(t, r.handle(r.delivery()), errStoreDown)
			require.Empty(t, r.prov.Requests())
			r.requireNoWrites()
		})
	}
}

func TestConsumer_MemVerFailureDoesNotFailTheDelivery(t *testing.T) {
	r := newConsumerRig(t, fakes.MemoryReply{JSON: fixtureJSON(t)})
	r.ver.FailNext("Bump", errStoreDown)
	require.NoError(t, r.handle(r.delivery()), "the data is committed; a retry would find the event processed")
	require.Len(t, r.store.All(r.user), 4)
}
