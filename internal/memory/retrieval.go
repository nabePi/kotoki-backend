package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Retrieval constants (spec 11.7).
const (
	// MemCtxTTL is the lifetime of kotoki:session:{id}:memctx: the longest session (3600 s) plus 900 s.
	MemCtxTTL = 4500 * time.Second

	buildDeadline   = 2000 * time.Millisecond
	embedQueryLimit = 1500 * time.Millisecond
	refreshLimit    = 1500 * time.Millisecond

	maxCoreItems     = 20
	maxEpisodicItems = 3
	minPriority      = 0.45
	episodicMinSim   = 0.75
	recencyDays      = 90.0
	scenarioMaxRunes = 500
	refreshMaxRunes  = 600
	refreshTurns     = 3

	qvecCacheMax = 2048
	qvecCacheTTL = 2 * time.Hour

	// LearningPatternPrefix marks a LEARNING_PATTERN line so that the LLM only uses it to create
	// practice opportunities, never to correct (spec 11.7.6).
	LearningPatternPrefix = "（学習の傾向）"

	stageStart = "start"
	stageTurn  = "turn"
)

// ContextCache is the Redis side of retrieval: the per-user memory version and the per-session
// MemoryContext (spec 11.7.3, 11.7.5).
type ContextCache interface {
	// GetMemVer returns kotoki:user:{id}:memver; 0 when it does not exist.
	GetMemVer(ctx context.Context, userID uuid.UUID) (int64, error)
	// SetContext stores mc under kotoki:session:{id}:memctx with ttl. A ttl <= 0 keeps the TTL the
	// key has and writes only an existing key.
	SetContext(ctx context.Context, sessionID uuid.UUID, mc MemoryContext, ttl time.Duration) error
	// GetContext returns the stored context; nil, nil when it does not exist or is unreadable.
	GetContext(ctx context.Context, sessionID uuid.UUID) (*MemoryContext, error)
}

// Embedder embeds texts (provider.AsyncRouter).
type Embedder interface {
	Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error)
}

// RetrieverDeps are the collaborators of Retriever. Log and Metrics may be nil.
type RetrieverDeps struct {
	Reader   Reader
	Settings ProfileSettings
	Embedder Embedder
	Cache    ContextCache
	Clock    clock.Clock
	Log      *slog.Logger
	Metrics  *Metrics
}

// Retriever implements RetrievalService (spec 11.7).
type Retriever struct {
	d RetrieverDeps

	mu       sync.Mutex
	sessions map[uuid.UUID]sessionState
}

var _ RetrievalService = (*Retriever)(nil)

// sessionState is what the retriever remembers of a session in memory: its owner and the query
// vector of the start (relevance of the core reload). It is lost on restart: relevance is then 0.
type sessionState struct {
	user uuid.UUID
	qvec []float32
	at   time.Time
}

// NewRetriever returns a Retriever.
func NewRetriever(d RetrieverDeps) *Retriever {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Retriever{d: d, sessions: map[uuid.UUID]sessionState{}}
}

// ContextItemSource is what RenderItem needs of an item.
type ContextItemSource struct {
	Category   Category
	Content    string
	ObservedAt time.Time
}

var itemSanitizer = strings.NewReplacer("<", "＜", ">", "＞", "\r\n", "", "\n", "", "\r", "")

// RenderItem is the text of one item as it goes into the prompt (spec 11.7.6): the sanitised
// content, LEARNING_PATTERN with its prefix, an episode with its date (Asia/Jakarta) in front.
func RenderItem(c ContextItemSource) string {
	text := itemSanitizer.Replace(c.Content)
	switch c.Category {
	case CategoryLearningPattern:
		return LearningPatternPrefix + text
	case CategoryEpisodic:
		return c.ObservedAt.In(jakarta()).Format("2006-01-02") + ": " + text
	}
	return text
}

// itemTokens is the contribution of an item to its prompt component: the estimate of the final
// line, bullet and newline included (spec 11.7.2).
func itemTokens(c ContextItemSource) int { return prompt.EstimateTokens("- " + RenderItem(c) + "\n") }

// Priority is the retrieval priority of a core row (spec 11.7.3).
func Priority(r CoreRow, rel float64, now time.Time) float64 {
	days := max(0, now.Sub(r.LastObservedAt).Hours()/24)
	rec := math.Exp(-days / recencyDays)
	imp := math.Min(1, r.Importance+0.05*float64(r.ObservationCount-1))
	return 0.40*imp + 0.25*r.Confidence + 0.15*rec + 0.20*rel
}

// BuildSessionContext implements RetrievalService. It never returns an error.
func (r *Retriever) BuildSessionContext(ctx context.Context, in SessionContextInput) (MemoryContext, error) {
	start := r.d.Clock.Now()
	ctx, cancel := context.WithTimeout(ctx, buildDeadline)
	defer cancel()
	defer func() { r.d.Metrics.Retrieval(stageStart, r.d.Clock.Now().Sub(start)) }()

	mc := MemoryContext{SessionID: in.SessionID, UserID: in.UserID, BuiltAt: start}
	degrade := func(why string, err error) {
		mc.Degraded = true
		r.d.Metrics.Degraded(stageStart)
		r.d.Log.WarnContext(ctx, "memory retrieval degraded", slog.String("stage", stageStart), slog.String("why", why), slog.String("error", err.Error()))
	}

	enabled, err := r.d.Settings.EnsureMemoryEnabled(ctx, in.UserID)
	if err != nil {
		degrade("settings", err)
		return mc, nil
	}
	if !enabled {
		return mc, nil
	}

	if mc.UserMemVer, err = r.d.Cache.GetMemVer(ctx, in.UserID); err != nil {
		mc.UserMemVer = 0
		degrade("memver", err)
	}

	var qvec []float32
	if text := truncateRunes(strings.TrimSpace(in.ScenarioText), scenarioMaxRunes); text != "" {
		if qvec, err = r.embed(ctx, embedQueryLimit, in.SessionID, text); err != nil {
			qvec = nil
			degrade("embedding", err)
		}
	}
	r.remember(in.SessionID, sessionState{user: in.UserID, qvec: qvec})

	rows, err := r.d.Reader.ListCore(ctx, in.UserID, start)
	if err != nil {
		degrade("core", err)
		return mc, nil
	}
	mc.Core, mc.CoreTokens = selectCore(rows, qvec, start)

	if qvec != nil {
		ep, err := r.d.Reader.SearchEpisodic(ctx, in.UserID, qvec, episodicMinSim, maxEpisodicItems, &in.SessionID, start)
		if err != nil {
			degrade("episodic", err)
		} else {
			mc.Episodic, mc.EpisodicTokens = trimEpisodic(ep)
		}
	}
	r.d.Metrics.ContextTokens("core", mc.CoreTokens)
	r.d.Metrics.ContextTokens("episodic", mc.EpisodicTokens)

	if err := r.d.Cache.SetContext(ctx, in.SessionID, mc, MemCtxTTL); err != nil {
		degrade("memctx", err)
	}
	return mc, nil
}

// RefreshEpisodic implements RetrievalService. It never returns an error: on failure the episodes
// of the stored context are returned (nil when there is none).
func (r *Retriever) RefreshEpisodic(ctx context.Context, userID, sessionID uuid.UUID, recent []TurnText) ([]ContextItem, error) {
	start := r.d.Clock.Now()
	ctx, cancel := context.WithTimeout(ctx, refreshLimit)
	defer cancel()
	defer func() { r.d.Metrics.Retrieval(stageTurn, r.d.Clock.Now().Sub(start)) }()

	stored, _ := r.d.Cache.GetContext(ctx, sessionID) // an unreadable cache only loses the fallback
	old := func() []ContextItem {
		if stored == nil {
			return nil
		}
		return slices.Clone(stored.Episodic)
	}
	degrade := func(why string, err error) []ContextItem {
		r.d.Metrics.Degraded(stageTurn)
		r.d.Log.WarnContext(ctx, "memory retrieval degraded", slog.String("stage", stageTurn), slog.String("why", why), slog.String("error", err.Error()))
		return old()
	}

	q := recentText(recent)
	if q == "" {
		return old(), nil
	}
	qvec, err := r.embed(ctx, refreshLimit, sessionID, q)
	if err != nil {
		return degrade("embedding", err), nil
	}
	rows, err := r.d.Reader.SearchEpisodic(ctx, userID, qvec, episodicMinSim, maxEpisodicItems, &sessionID, start)
	if err != nil {
		return degrade("episodic", err), nil
	}
	items, tokens := trimEpisodic(rows)
	if stored != nil {
		stored.Episodic, stored.EpisodicTokens = items, tokens
		if err := r.d.Cache.SetContext(ctx, sessionID, *stored, 0); err != nil {
			r.d.Log.WarnContext(ctx, "memctx update failed", slog.String("error", err.Error()))
		}
	}
	r.d.Metrics.ContextTokens("episodic", tokens)
	return items, nil
}

// ReloadCoreIfChanged implements RetrievalService: when the memver of the user moved past
// mc.UserMemVer the core is selected again (with the query vector of the start when this process
// still has it), episodes that are no longer ACTIVE are dropped, and mc.UserMemVer is set.
func (r *Retriever) ReloadCoreIfChanged(ctx context.Context, mc *MemoryContext) (bool, error) {
	state, known := r.state(mc.SessionID)
	user := mc.UserID
	if user == uuid.Nil {
		if !known {
			return false, nil // nobody to reload for
		}
		user = state.user
	}
	ver, err := r.d.Cache.GetMemVer(ctx, user)
	if err != nil {
		return false, fmt.Errorf("memory: read memver: %w", err)
	}
	if ver == mc.UserMemVer {
		return false, nil
	}
	now := r.d.Clock.Now()
	rows, err := r.d.Reader.ListCore(ctx, user, now)
	if err != nil {
		return false, fmt.Errorf("memory: reload core: %w", err)
	}
	var episodic []ContextItem
	if len(mc.Episodic) > 0 {
		cat := CategoryEpisodic
		active, err := r.d.Reader.List(ctx, user, &cat, nil, 200, now)
		if err != nil {
			return false, fmt.Errorf("memory: reload episodes: %w", err)
		}
		alive := make(map[uuid.UUID]bool, len(active))
		for _, m := range active {
			alive[m.ID] = true
		}
		for _, it := range mc.Episodic {
			if alive[it.ID] {
				episodic = append(episodic, it)
			}
		}
	}
	mc.Core, mc.CoreTokens = selectCore(rows, state.qvec, now)
	mc.Episodic = episodic
	mc.EpisodicTokens = 0
	for _, it := range episodic {
		mc.EpisodicTokens += it.Tokens
	}
	mc.UserMemVer = ver
	if err := r.d.Cache.SetContext(ctx, mc.SessionID, *mc, 0); err != nil {
		r.d.Log.WarnContext(ctx, "memctx update failed", slog.String("error", err.Error()))
	}
	return true, nil
}

func (r *Retriever) embed(ctx context.Context, limit time.Duration, sessionID uuid.UUID, text string) ([]float32, error) {
	ectx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	res, err := r.d.Embedder.Embed(ectx, provider.EmbeddingRequest{
		Texts: []string{text}, Dimensions: embedDimensions, RequestID: "MEMORY_RETRIEVAL:" + sessionID.String(),
	})
	if err != nil {
		return nil, err
	}
	if len(res.Vectors) != 1 || len(res.Vectors[0]) == 0 {
		return nil, fmt.Errorf("memory: embedding has an unexpected shape")
	}
	return res.Vectors[0], nil
}

// selectCore scores rows, keeps priority >= 0.45, sorts (priority, then newest observation) and
// takes items greedily while they fit the core budget and the item cap.
func selectCore(rows []CoreRow, qvec []float32, now time.Time) ([]ContextItem, int) {
	type scored struct {
		row  CoreRow
		prio float64
	}
	var kept []scored
	for _, row := range rows {
		rel := 0.0
		if qvec != nil && len(row.Embedding) > 0 {
			rel = max(0, Cosine(qvec, row.Embedding))
		}
		if p := Priority(row, rel, now); p >= minPriority {
			kept = append(kept, scored{row, p})
		}
	}
	slices.SortStableFunc(kept, func(a, b scored) int {
		if a.prio != b.prio {
			if a.prio > b.prio {
				return -1
			}
			return 1
		}
		return b.row.LastObservedAt.Compare(a.row.LastObservedAt)
	})
	var items []ContextItem
	total := 0
	for _, k := range kept {
		if len(items) >= maxCoreItems {
			break
		}
		src := ContextItemSource{Category: k.row.Category, Content: k.row.Content, ObservedAt: k.row.LastObservedAt}
		tok := itemTokens(src)
		if total+tok > prompt.DefaultBudget.MemoryCore {
			continue
		}
		items = append(items, ContextItem{
			ID: k.row.ID, Category: k.row.Category, Content: itemSanitizer.Replace(k.row.Content),
			ObservedAt: k.row.LastObservedAt, Score: k.prio, Tokens: tok,
		})
		total += tok
	}
	return items, total
}

// trimEpisodic converts hits (nearest first) into items and keeps the longest prefix that fits the
// episodic budget.
func trimEpisodic(rows []EpisodicRow) ([]ContextItem, int) {
	var items []ContextItem
	total := 0
	for _, e := range rows {
		if len(items) >= maxEpisodicItems {
			break
		}
		tok := itemTokens(ContextItemSource{Category: CategoryEpisodic, Content: e.Content, ObservedAt: e.LastObservedAt})
		if total+tok > prompt.DefaultBudget.MemoryEpisodic {
			break
		}
		items = append(items, ContextItem{
			ID: e.ID, Category: CategoryEpisodic, Content: itemSanitizer.Replace(e.Content),
			ObservedAt: e.LastObservedAt, Score: e.Similarity, Tokens: tok,
		})
		total += tok
	}
	return items, total
}

// recentText joins the last 3 turns (user text and spoken assistant text, newline separated) and
// keeps the last 600 runes.
func recentText(recent []TurnText) string {
	if len(recent) > refreshTurns {
		recent = recent[len(recent)-refreshTurns:]
	}
	var parts []string
	for _, t := range recent {
		for _, s := range []string{t.UserText, t.AssistantSpokenText} {
			if s = strings.TrimSpace(s); s != "" {
				parts = append(parts, s)
			}
		}
	}
	s := strings.Join(parts, "\n")
	if n := utf8.RuneCountInString(s); n > refreshMaxRunes {
		s = string([]rune(s)[n-refreshMaxRunes:])
	}
	return s
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (r *Retriever) remember(id uuid.UUID, s sessionState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.d.Clock.Now()
	s.at = now
	for k, v := range r.sessions { // drop what outlived its TTL
		if now.Sub(v.at) > qvecCacheTTL {
			delete(r.sessions, k)
		}
	}
	for len(r.sessions) >= qvecCacheMax {
		var oldest uuid.UUID
		var oldestAt time.Time
		first := true
		for k, v := range r.sessions {
			if first || v.at.Before(oldestAt) {
				oldest, oldestAt, first = k, v.at, false
			}
		}
		delete(r.sessions, oldest)
	}
	r.sessions[id] = s
}

func (r *Retriever) state(id uuid.UUID) (sessionState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if ok && r.d.Clock.Now().Sub(s.at) > qvecCacheTTL {
		delete(r.sessions, id)
		return sessionState{}, false
	}
	return s, ok
}
