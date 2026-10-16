package memory_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

// stubEmbedder answers every text with vec (or err) and records the texts.
type stubEmbedder struct {
	mu    sync.Mutex
	vec   []float32
	err   error
	texts []string
}

func (e *stubEmbedder) Embed(_ context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, req.Texts...)
	if e.err != nil {
		return provider.EmbeddingResult{}, e.err
	}
	return provider.EmbeddingResult{Vectors: [][]float32{e.vec}}, nil
}

func (e *stubEmbedder) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.texts)
}

type retrievalRig struct {
	t      *testing.T
	store  *fakes.MemoryStore
	prof   *fakes.MemoryProfile
	cache  *fakes.MemoryContextCache
	emb    *stubEmbedder
	clk    *clock.Fake
	reg    *prometheus.Registry
	ret    *memory.Retriever
	user   uuid.UUID
	sess   uuid.UUID
	nextID int
}

func newRetrievalRig(t *testing.T) *retrievalRig {
	t.Helper()
	clk := clock.NewFake(t0)
	m, reg := newMetrics(t)
	r := &retrievalRig{
		t: t, store: fakes.NewMemoryStore(fakes.WithMemoryClock(clk)), prof: fakes.NewMemoryProfile(), cache: fakes.NewMemoryContextCache(),
		emb: &stubEmbedder{vec: vec(0, 100)}, clk: clk, reg: reg, user: uuidFor(1), sess: uuidFor(2),
	}
	r.rebuild(m)
	return r
}

func (r *retrievalRig) rebuild(m *memory.Metrics) {
	r.ret = memory.NewRetriever(memory.RetrieverDeps{
		Reader: r.store.Reader(), Settings: r.prof, Embedder: r.emb, Cache: r.cache, Clock: r.clk, Metrics: m,
	})
}

// put stores an ACTIVE memory of user with sensible defaults.
func (r *retrievalRig) put(user uuid.UUID, cat memory.Category, key, content string, mod ...func(*memory.Memory)) memory.Memory {
	r.t.Helper()
	r.nextID++
	m := memory.Memory{
		ID: uuidFor(1000 + r.nextID), UserID: user, Category: cat, Key: key, Content: content, DisplayText: "x",
		Confidence: 0.9, Importance: 0.8, ObservationCount: 1, Embedding: vec(0, 100), Status: memory.StatusActive,
		FirstObservedAt: t0, LastObservedAt: t0, ExpiresAt: t0.Add(24 * time.Hour), CreatedAt: t0, UpdatedAt: t0,
	}
	for _, f := range mod {
		f(&m)
	}
	r.store.Put(m)
	return m
}

func (r *retrievalRig) build(scenario string) memory.MemoryContext {
	r.t.Helper()
	mc, err := r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: r.sess, ScenarioText: scenario})
	require.NoError(r.t, err)
	return mc
}

func coreIDs(mc memory.MemoryContext) []uuid.UUID {
	var out []uuid.UUID
	for _, it := range mc.Core {
		out = append(out, it.ID)
	}
	return out
}

func TestPriorityFormula(t *testing.T) {
	row := memory.CoreRow{Importance: 0.8, ObservationCount: 3, Confidence: 0.9, LastObservedAt: t0.Add(-90 * 24 * time.Hour)}
	want := 0.40*0.9 + 0.25*0.9 + 0.15*math.Exp(-1) + 0.20*0.5
	require.InDelta(t, want, memory.Priority(row, 0.5, t0), 1e-9)

	// importance saturates at 1 and a future observation does not raise recency above 1
	sat := memory.CoreRow{Importance: 0.99, ObservationCount: 10, Confidence: 0, LastObservedAt: t0.Add(time.Hour)}
	require.InDelta(t, 0.40*1+0.15*1, memory.Priority(sat, 0, t0), 1e-9)
}

func TestRenderItem_Sanitizes_AndLearningPatternPrefix_AndEpisodeDate(t *testing.T) {
	obs := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC) // 2026-10-02 01:00 in Asia/Jakarta
	require.Equal(t, "ゆっくり＜b＞話して＞ほしい。", memory.RenderItem(memory.ContextItemSource{
		Category: memory.CategoryPreference, Content: "ゆっくり<b>話して>\nほしい。", ObservedAt: obs}))
	require.Equal(t, "（学習の傾向）「は」と「が」。", memory.RenderItem(memory.ContextItemSource{
		Category: memory.CategoryLearningPattern, Content: "「は」と\r\n「が」。"}))
	require.Equal(t, "2026-10-02: 面接練習＜x＞。", memory.RenderItem(memory.ContextItemSource{
		Category: memory.CategoryEpisodic, Content: "面接練習<x>。", ObservedAt: obs}), "the date is in Asia/Jakarta")
	require.Equal(t, "エンジニア。", memory.RenderItem(memory.ContextItemSource{Category: memory.CategoryProfileFact, Content: "エンジニア。"}))
}

func TestBuildSessionContext_ThresholdAndSort(t *testing.T) {
	r := newRetrievalRig(t)
	// priority = 0.40*imp + 0.25*conf + 0.15*rec + 0.20*rel; the query vector equals every embedding (rel 1)
	hi := r.put(r.user, memory.CategoryProfileFact, "a", "高い。", func(m *memory.Memory) { m.Importance, m.Confidence = 1, 1 })
	mid := r.put(r.user, memory.CategoryPreference, "b", "中。", func(m *memory.Memory) { m.Importance, m.Confidence = 0.6, 0.8 })
	tie1 := r.put(r.user, memory.CategoryPreference, "c", "同点一。", func(m *memory.Memory) { m.Importance, m.Confidence = 0.6, 0.8 })
	tie2 := r.put(r.user, memory.CategoryPreference, "d", "同点二。", func(m *memory.Memory) { m.Importance, m.Confidence = 0.6, 0.8 })
	// below the threshold: no importance, no confidence, a year old, unrelated to the query
	low := r.put(r.user, memory.CategoryProfileFact, "low", "低い。", func(m *memory.Memory) {
		m.Importance, m.Confidence, m.LastObservedAt, m.Embedding = 0, 0, t0.Add(-365*24*time.Hour), vec(0, 0)
	})
	// just under the threshold: 0.40*0.3 + 0.25*0.4 + 0.15*~0 + 0.20*1 = 0.42
	under := r.put(r.user, memory.CategoryProfileFact, "under", "下。", func(m *memory.Memory) {
		m.Importance, m.Confidence, m.LastObservedAt, m.Embedding = 0.3, 0.4, t0.Add(-900*24*time.Hour), vec(0, 100)
	})
	mc := r.build("面接の練習")
	got := coreIDs(mc)
	require.NotContains(t, got, low.ID)
	require.NotContains(t, got, under.ID, "priority below 0.45 is dropped")
	require.Equal(t, hi.ID, got[0], "highest priority first")
	// the three equal priorities follow the high one
	require.ElementsMatch(t, []uuid.UUID{mid.ID, tie1.ID, tie2.ID}, got[1:])
	for i := 1; i < len(mc.Core); i++ {
		require.GreaterOrEqual(t, mc.Core[i-1].Score, mc.Core[i].Score, "priority descending")
	}
	require.InDelta(t, 0.40*1+0.25*1+0.15*1+0.20*1, mc.Core[0].Score, 1e-6)
}

func TestBuildSessionContext_SortsEqualPriorityByNewestObservation(t *testing.T) {
	r := newRetrievalRig(t)
	// identical rows observed at the same instant: the order of the repository (id) is kept
	a := r.put(r.user, memory.CategoryPreference, "a", "一。")
	b := r.put(r.user, memory.CategoryPreference, "b", "二。")
	mc := r.build("x")
	require.Equal(t, []uuid.UUID{a.ID, b.ID}, coreIDs(mc), "equal priority and time: by id (as the repository orders rows)")
}

func TestBuildSessionContext_CoreBudgetNeverExceeds600(t *testing.T) {
	r := newRetrievalRig(t)
	long := strings.Repeat("あ", 59) + "。"
	for i := range 200 {
		r.put(r.user, memory.CategoryPreference, "k"+string(rune('a'+i%26))+string(rune('a'+i/26)), long)
	}
	mc := r.build("x")
	require.LessOrEqual(t, mc.CoreTokens, prompt.DefaultBudget.MemoryCore)
	require.LessOrEqual(t, len(mc.Core), 20)
	require.NotEmpty(t, mc.Core)
	sum := 0
	for _, it := range mc.Core {
		sum += it.Tokens
	}
	require.Equal(t, sum, mc.CoreTokens)

	// the item cap binds before the budget for short items
	r2 := newRetrievalRig(t)
	for i := range 40 {
		r2.put(r2.user, memory.CategoryPreference, "k"+string(rune('a'+i%26))+string(rune('a'+i/26)), "短い。")
	}
	mc2 := r2.build("x")
	require.Len(t, mc2.Core, 20)
	require.Less(t, mc2.CoreTokens, prompt.DefaultBudget.MemoryCore)
}

func TestBuildSessionContext_SkipsAnItemThatDoesNotFitAndTakesSmallerOnes(t *testing.T) {
	r := newRetrievalRig(t)
	long := strings.Repeat("あ", 59) + "。" // about 67 tokens
	for i := range 9 {
		r.put(r.user, memory.CategoryProfileFact, "long"+string(rune('a'+i)), long, func(m *memory.Memory) { m.Importance = 1 })
	}
	short := r.put(r.user, memory.CategoryPreference, "short", "短い。", func(m *memory.Memory) { m.Importance, m.Confidence = 0.1, 0.9 })
	mc := r.build("x")
	require.LessOrEqual(t, mc.CoreTokens, 600)
	require.Contains(t, coreIDs(mc), short.ID, "after nine long items (603 tokens) only a short one still fits")
}

func TestBuildSessionContext_EpisodicMax3And500Tokens(t *testing.T) {
	r := newRetrievalRig(t)
	for i := range 5 {
		r.put(r.user, memory.CategoryEpisodic, "episode:"+uuidFor(500+i).String(), "短い会話。", func(m *memory.Memory) {
			m.Embedding = vec(0, 100-i) // all above 0.75, nearest first
			s := uuidFor(700 + i)
			m.SourceSessionID = &s
		})
	}
	mc := r.build("x")
	require.Len(t, mc.Episodic, 3, "at most 3 episodes")
	for i := 1; i < len(mc.Episodic); i++ {
		require.GreaterOrEqual(t, mc.Episodic[i-1].Score, mc.Episodic[i].Score, "nearest first")
	}

	// long episodes: the prefix that fits 500 tokens
	r2 := newRetrievalRig(t)
	long := strings.Repeat("あ", 149) + "。"
	for i := range 3 {
		r2.put(r2.user, memory.CategoryEpisodic, "episode:"+uuidFor(600+i).String(), long, func(m *memory.Memory) {
			m.Embedding = vec(0, 100-i)
			s := uuidFor(800 + i)
			m.SourceSessionID = &s
		})
	}
	mc2 := r2.build("x")
	require.LessOrEqual(t, mc2.EpisodicTokens, prompt.DefaultBudget.MemoryEpisodic)
	require.Len(t, mc2.Episodic, 2, "the third episode would pass 500 tokens")
	sum := 0
	for _, it := range mc2.Episodic {
		sum += it.Tokens
	}
	require.Equal(t, sum, mc2.EpisodicTokens)
}

func TestContextItemTokensEqualEstimateOfRenderedLine(t *testing.T) {
	r := newRetrievalRig(t)
	r.put(r.user, memory.CategoryLearningPattern, "error:x", "「は」と「が」を混同する。")
	r.put(r.user, memory.CategoryPreference, "pref", "ゆっくり<話>して\nほしい。")
	r.put(r.user, memory.CategoryEpisodic, "episode:"+uuidFor(9).String(), "面接練習。", func(m *memory.Memory) {
		s := uuidFor(99)
		m.SourceSessionID = &s
	})
	mc := r.build("x")
	require.Len(t, mc.Core, 2)
	require.Len(t, mc.Episodic, 1)
	sum := 0
	for _, it := range append(append([]memory.ContextItem{}, mc.Core...), mc.Episodic...) {
		line := "- " + memory.RenderItem(memory.ContextItemSource{Category: it.Category, Content: it.Content, ObservedAt: it.ObservedAt}) + "\n"
		require.Equal(t, prompt.EstimateTokens(line), it.Tokens, line)
		if it.Category != memory.CategoryEpisodic {
			sum += it.Tokens
		}
	}
	require.Equal(t, sum, mc.CoreTokens)
	for _, it := range mc.Core {
		require.NotContains(t, it.Content, "<")
		require.NotContains(t, it.Content, "\n")
	}
}

func TestBuildSessionContext_MemoryDisabled_EmptyNotDegraded(t *testing.T) {
	r := newRetrievalRig(t)
	r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
	r.prof.Disable(r.user)
	mc := r.build("x")
	require.Empty(t, mc.Core)
	require.Empty(t, mc.Episodic)
	require.False(t, mc.Degraded)
	require.Zero(t, r.emb.calls(), "no embedding for a disabled memory")
	cached, err := r.cache.GetContext(context.Background(), r.sess)
	require.NoError(t, err)
	require.Nil(t, cached)
}

func TestBuildSessionContext_EmbedFailureDegrades(t *testing.T) {
	r := newRetrievalRig(t)
	m, reg := newMetrics(t)
	r.rebuild(m)
	r.emb.err = errors.New("embedding down")
	core := r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
	r.put(r.user, memory.CategoryEpisodic, "episode:"+uuidFor(9).String(), "面接練習。")
	mc, err := r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: r.sess, ScenarioText: "面接"})
	require.NoError(t, err)
	require.True(t, mc.Degraded)
	require.Equal(t, []uuid.UUID{core.ID}, coreIDs(mc), "core is still loaded")
	require.Empty(t, mc.Episodic)
	require.Equal(t, 1.0, counterValues(t, reg, "kotoki_memory_retrieval_degraded_total", "stage")["start"])
}

func TestBuildSessionContext_DBFailureDegrades(t *testing.T) {
	for _, method := range []string{"ListCore", "SearchEpisodic"} {
		t.Run(method, func(t *testing.T) {
			r := newRetrievalRig(t)
			r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
			r.store.FailNext(method, errors.New("connection reset"))
			mc, err := r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: r.sess, ScenarioText: "面接"})
			require.NoError(t, err)
			require.True(t, mc.Degraded)
			if method == "ListCore" {
				require.Empty(t, mc.Core)
			} else {
				require.NotEmpty(t, mc.Core)
			}
		})
	}
	t.Run("profile lookup", func(t *testing.T) {
		r := newRetrievalRig(t)
		r.prof.FailNext("EnsureMemoryEnabled", errors.New("connection reset"))
		mc, err := r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: r.sess})
		require.NoError(t, err)
		require.True(t, mc.Degraded)
		require.Empty(t, mc.Core)
	})
}

func TestBuildSessionContext_RedisDownDegradesButKeepsMemory(t *testing.T) {
	r := newRetrievalRig(t)
	core := r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
	r.cache.FailAlways(errors.New("redis down"))
	mc := r.build("x")
	require.True(t, mc.Degraded)
	require.Equal(t, []uuid.UUID{core.ID}, coreIDs(mc))
	require.Zero(t, mc.UserMemVer)
}

func TestBuildSessionContext_FreeConversationEmptyScenarioText_NoEmbed(t *testing.T) {
	r := newRetrievalRig(t)
	core := r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
	r.put(r.user, memory.CategoryEpisodic, "episode:"+uuidFor(9).String(), "面接練習。")
	mc := r.build("  \n ")
	require.Zero(t, r.emb.calls())
	require.False(t, mc.Degraded)
	require.Equal(t, []uuid.UUID{core.ID}, coreIDs(mc), "core loaded with relevance 0")
	require.Empty(t, mc.Episodic)
}

func TestBuildSessionContext_CachesMemctxAndExcludesOwnEpisode(t *testing.T) {
	r := newRetrievalRig(t)
	own := r.sess
	r.put(r.user, memory.CategoryEpisodic, "episode:"+own.String(), "この会話。", func(m *memory.Memory) { m.SourceSessionID = &own })
	other := uuidFor(50)
	old := r.put(r.user, memory.CategoryEpisodic, "episode:"+other.String(), "前の会話。", func(m *memory.Memory) { m.SourceSessionID = &other })
	r.cache.Bump(r.user)
	mc := r.build("x")
	require.Len(t, mc.Episodic, 1)
	require.Equal(t, old.ID, mc.Episodic[0].ID, "an episode of the session itself is never recalled")
	require.Equal(t, int64(1), mc.UserMemVer)
	require.Equal(t, memory.MemCtxTTL, r.cache.TTL(r.sess))
	require.Equal(t, 4500*time.Second, memory.MemCtxTTL)
	cached, err := r.cache.GetContext(context.Background(), r.sess)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, mc.Episodic[0].ID, cached.Episodic[0].ID)
	require.Equal(t, r.user, cached.UserID)
	require.Equal(t, t0, cached.BuiltAt.UTC())
}

func TestBuildSessionContext_EpisodeBelowSimilarityThresholdIsNotRecalled(t *testing.T) {
	r := newRetrievalRig(t)
	s1, s2 := uuidFor(61), uuidFor(62)
	// cosine with the query (ones in [0,100)) of a vector with k ones inside it is sqrt(k/100)
	r.put(r.user, memory.CategoryEpisodic, "episode:"+s1.String(), "遠い。", func(m *memory.Memory) { m.Embedding, m.SourceSessionID = vec(0, 56), &s1 })         // 0.748
	near := r.put(r.user, memory.CategoryEpisodic, "episode:"+s2.String(), "近い。", func(m *memory.Memory) { m.Embedding, m.SourceSessionID = vec(0, 57), &s2 }) // 0.755
	mc := r.build("x")
	require.Len(t, mc.Episodic, 1)
	require.Equal(t, near.ID, mc.Episodic[0].ID, "0.748 stays out, 0.755 is in")
}

func TestBuildSessionContext_ScenarioTextIsCutAt500Runes(t *testing.T) {
	r := newRetrievalRig(t)
	r.build(strings.Repeat("あ", 800))
	require.Len(t, r.emb.texts, 1)
	require.Equal(t, 500, len([]rune(r.emb.texts[0])))
}

func TestBuildSessionContext_UserIsolation(t *testing.T) {
	r := newRetrievalRig(t)
	other := uuidFor(77)
	r.put(other, memory.CategoryProfileFact, "occupation", "他人。")
	otherSess := uuidFor(78)
	r.put(other, memory.CategoryEpisodic, "episode:"+otherSess.String(), "他人の会話。", func(m *memory.Memory) { m.SourceSessionID = &otherSess })
	mc := r.build("x")
	require.Empty(t, mc.Core)
	require.Empty(t, mc.Episodic)
}

func TestRefreshEpisodic_FindsEpisodesFromTheLastThreeTurns(t *testing.T) {
	r := newRetrievalRig(t)
	r.put(r.user, memory.CategoryEpisodic, "episode:"+uuidFor(9).String(), "面接練習。", func(m *memory.Memory) {
		s := uuidFor(99)
		m.SourceSessionID = &s
	})
	mc := r.build("") // memctx is stored (no embed, no episode yet)
	require.Empty(t, mc.Episodic)
	long := strings.Repeat("あ", 560)
	recent := []memory.TurnText{
		{UserText: "古い", AssistantSpokenText: "古い返事"},
		{UserText: long, AssistantSpokenText: "返事2"},
		{UserText: "三", AssistantSpokenText: "返事3"},
		{UserText: "四", AssistantSpokenText: "返事4"},
	}
	items, err := r.ret.RefreshEpisodic(context.Background(), r.user, r.sess, recent)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Len(t, r.emb.texts, 1)
	q := r.emb.texts[0]
	require.Equal(t, long+"\n返事2\n三\n返事3\n四\n返事4", q, "the last 3 turns, user text then spoken text, newline separated")
	require.NotContains(t, q, "古い")

	// a longer text keeps its last 600 runes
	recent[1].UserText = strings.Repeat("あ", 900)
	_, err = r.ret.RefreshEpisodic(context.Background(), r.user, r.sess, recent)
	require.NoError(t, err)
	q = r.emb.texts[1]
	require.Equal(t, 600, len([]rune(q)))
	require.True(t, strings.HasSuffix(q, "三\n返事3\n四\n返事4"), q)
	cached, _ := r.cache.GetContext(context.Background(), r.sess)
	require.Len(t, cached.Episodic, 1, "memctx is updated")
	require.Equal(t, memory.MemCtxTTL, r.cache.TTL(r.sess), "the TTL is kept")
	require.LessOrEqual(t, r.cache.LastTTLArg(r.sess), time.Duration(0), "the update asks the store to keep the TTL")
}

func TestRefreshEpisodic_KeepsOldOnError(t *testing.T) {
	r := newRetrievalRig(t)
	m, reg := newMetrics(t)
	r.rebuild(m)
	s := uuidFor(99)
	old := r.put(r.user, memory.CategoryEpisodic, "episode:"+s.String(), "面接練習。", func(mm *memory.Memory) { mm.SourceSessionID = &s })
	require.Len(t, r.build("x").Episodic, 1)

	r.emb.err = errors.New("embedding down")
	items, err := r.ret.RefreshEpisodic(context.Background(), r.user, r.sess, []memory.TurnText{{UserText: "こんにちは"}})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, old.ID, items[0].ID, "the previous episodes stay")
	require.Equal(t, 1.0, counterValues(t, reg, "kotoki_memory_retrieval_degraded_total", "stage")["turn"])

	r.emb.err = nil
	r.store.FailNext("SearchEpisodic", errors.New("connection reset"))
	items, err = r.ret.RefreshEpisodic(context.Background(), r.user, r.sess, []memory.TurnText{{UserText: "こんにちは"}})
	require.NoError(t, err)
	require.Len(t, items, 1)

	// no stored context and a failure: nothing, still no error
	items, err = r.ret.RefreshEpisodic(context.Background(), r.user, uuidFor(1234), nil)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestReloadCoreIfChanged_DeletedMemoryDroppedOnNextCall(t *testing.T) {
	r := newRetrievalRig(t)
	keep := r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
	gone := r.put(r.user, memory.CategoryPreference, "pref", "ゆっくり。")
	s := uuidFor(99)
	ep := r.put(r.user, memory.CategoryEpisodic, "episode:"+s.String(), "面接練習。", func(m *memory.Memory) { m.SourceSessionID = &s })
	mc := r.build("x")
	require.ElementsMatch(t, []uuid.UUID{keep.ID, gone.ID}, coreIDs(mc))
	require.Len(t, mc.Episodic, 1)

	// unchanged memver: no change and no query at all (a failure armed on ListCore is not hit)
	r.store.FailNext("ListCore", errors.New("must not be called"))
	reads := r.cache.MemVerReads()
	changed, err := r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, reads+1, r.cache.MemVerReads(), "one memver read per call")
	_, _ = r.store.Reader().ListCore(context.Background(), r.user, t0) // consume the armed failure

	// the user deletes two memories and the version moves
	require.NoError(t, r.store.WithinTx(context.Background(), func(ctx context.Context, tx memory.Tx) error {
		for _, id := range []uuid.UUID{gone.ID, ep.ID} {
			if ok, err := tx.Memories().Delete(ctx, r.user, id, t0); err != nil || !ok {
				return errors.New("delete failed")
			}
		}
		return nil
	}))
	r.cache.Bump(r.user)
	changed, err = r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []uuid.UUID{keep.ID}, coreIDs(mc))
	require.Empty(t, mc.Episodic, "an episode that is no longer ACTIVE is dropped")
	require.Zero(t, mc.EpisodicTokens)
	require.Equal(t, int64(1), mc.UserMemVer)
	require.Equal(t, mc.Core[0].Tokens, mc.CoreTokens)
	cached, _ := r.cache.GetContext(context.Background(), r.sess)
	require.Equal(t, []uuid.UUID{keep.ID}, coreIDs(*cached), "memctx follows")

	changed, err = r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.False(t, changed, "the version is now current")
}

func TestReloadCoreIfChanged_Errors(t *testing.T) {
	r := newRetrievalRig(t)
	r.put(r.user, memory.CategoryProfileFact, "occupation", "エンジニア。")
	mc := r.build("x")
	r.cache.Bump(r.user)

	r.cache.FailNext("GetMemVer", errors.New("redis down"))
	changed, err := r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.Error(t, err)
	require.False(t, changed)

	r.store.FailNext("ListCore", errors.New("connection reset"))
	before := mc
	changed, err = r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.Error(t, err)
	require.False(t, changed)
	require.Equal(t, before.UserMemVer, mc.UserMemVer, "a failed reload leaves the context as it was")
}

func TestReloadCoreIfChanged_NoQvecAfterRestart_RelevanceZero(t *testing.T) {
	r := newRetrievalRig(t)
	// priority with relevance 1 = 0.4*0.4 + 0.25*0.4 + 0.15 + 0.2 = 0.61; without relevance 0.41 (< 0.45)
	item := r.put(r.user, memory.CategoryPreference, "pref", "ゆっくり。", func(m *memory.Memory) { m.Importance, m.Confidence = 0.4, 0.4 })
	mc := r.build("面接")
	require.Equal(t, []uuid.UUID{item.ID}, coreIDs(mc), "relevant to the scenario: kept")

	r.cache.Bump(r.user)
	changed, err := r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []uuid.UUID{item.ID}, coreIDs(mc), "the start vector is still in memory")

	// a new process: the vector is gone, relevance is 0 and the item falls below the threshold
	r.rebuild(nil)
	r.cache.Bump(r.user)
	changed, err = r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, mc.Core)
}

func TestReloadCoreIfChanged_WithoutUserOrState(t *testing.T) {
	r := newRetrievalRig(t)
	mc := memory.MemoryContext{SessionID: uuidFor(404)}
	changed, err := r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.False(t, changed, "nobody to reload for")
}

func TestSessionStateCacheIsBounded(t *testing.T) {
	r := newRetrievalRig(t)
	r.put(r.user, memory.CategoryPreference, "pref", "ゆっくり。", func(m *memory.Memory) { m.Importance, m.Confidence = 0.4, 0.4 })
	first := uuidFor(5000)
	_, err := r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: first, ScenarioText: "面接"})
	require.NoError(t, err)
	for i := range 2100 {
		r.clk.Advance(time.Millisecond)
		_, err := r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: uuidFor(6000 + i)})
		require.NoError(t, err)
	}
	// the oldest entry was evicted: its context reloads without the start vector (relevance 0)
	mc := memory.MemoryContext{SessionID: first, UserID: r.user}
	r.cache.Bump(r.user)
	changed, err := r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, mc.Core, "oldest of 2048 entries evicted: no vector, relevance 0")

	// the TTL: an entry older than 2 hours is gone too
	recent := uuidFor(9000)
	_, err = r.ret.BuildSessionContext(context.Background(), memory.SessionContextInput{UserID: r.user, SessionID: recent, ScenarioText: "面接"})
	require.NoError(t, err)
	r.clk.Advance(2*time.Hour + time.Minute)
	mc = memory.MemoryContext{SessionID: recent, UserID: r.user}
	r.cache.Bump(r.user)
	_, err = r.ret.ReloadCoreIfChanged(context.Background(), &mc)
	require.NoError(t, err)
	require.Empty(t, mc.Core, "entry older than 2 hours dropped")
}

func TestRefreshEpisodic_BelowSimilarityThresholdIsNotRecalled(t *testing.T) {
	r := newRetrievalRig(t)
	s1, s2 := uuidFor(61), uuidFor(62)
	r.put(r.user, memory.CategoryEpisodic, "episode:"+s1.String(), "遠い。", func(m *memory.Memory) { m.Embedding, m.SourceSessionID = vec(0, 56), &s1 })         // 0.748
	near := r.put(r.user, memory.CategoryEpisodic, "episode:"+s2.String(), "近い。", func(m *memory.Memory) { m.Embedding, m.SourceSessionID = vec(0, 57), &s2 }) // 0.755
	items, err := r.ret.RefreshEpisodic(context.Background(), r.user, r.sess, []memory.TurnText{{UserText: "こんにちは"}})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, near.ID, items[0].ID)
}
