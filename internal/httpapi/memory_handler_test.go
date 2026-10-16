package httpapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

const memoriesPath = "/api/v1/profile/memories"

var (
	memSession = uuid.MustParse("0192f1aa-1111-7abc-8def-0000000000aa")
	memFirst   = time.Date(2026, 9, 20, 10, 3, 0, 123_000_000, time.UTC)
	memLast    = time.Date(2026, 10, 8, 11, 30, 0, 0, time.UTC)
)

// seedMemory stores an ACTIVE memory of the user (a fresh id) and returns the id.
func (e *apiEnv) seedMemory(uid uuid.UUID, cat, key string) uuid.UUID {
	m := memory.Memory{
		ID: uuid.Must(uuid.NewV7()), UserID: uid, Category: memory.Category(cat), Key: key,
		Content: "バックエンドエンジニアとして働いている。", DisplayText: "Bekerja sebagai backend engineer.",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.95, Importance: 0.7, ObservationCount: 3, SourceSessionID: ptrUUID(memSession),
		Embedding: []float32{0.1, 0.2}, EmbeddingModel: "text-embedding-3-small", Status: memory.StatusActive,
		FirstObservedAt: memFirst, LastObservedAt: memLast, ExpiresAt: apiT0.Add(700 * 24 * time.Hour), CreatedAt: memFirst, UpdatedAt: memLast,
	}
	e.memStore.Put(m)
	return m.ID
}

func (e *apiEnv) putMemory(m memory.Memory) { e.memStore.Put(m) }

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

func TestListMemoriesGolden(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	e.putMemory(memory.Memory{
		ID: uuid.MustParse("0192f3a4-7c1e-7b2a-9d0e-1a2b3c4d5e6f"), UserID: uid, Category: memory.CategoryProfileFact, Key: "job",
		Content: "バックエンドエンジニアとして働いている。", DisplayText: "Bekerja sebagai backend engineer.",
		EvidenceType: memory.EvidenceExplicit, Confidence: 0.95, Importance: 0.7, ObservationCount: 3, SourceSessionID: ptrUUID(memSession),
		Embedding: []float32{0.1, 0.2}, EmbeddingModel: "text-embedding-3-small", Status: memory.StatusActive,
		FirstObservedAt: memFirst, LastObservedAt: memLast, ExpiresAt: time.Date(2028, 10, 7, 11, 30, 0, 0, time.UTC), CreatedAt: memFirst, UpdatedAt: memLast,
	})
	rec := e.do(http.MethodGet, memoriesPath, "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	goldenFile(t, "memory", "list_200.json", rec.Body.Bytes())

	m := jsonOf(t, rec)
	require.Equal(t, true, m["memory_enabled"])
	require.Nil(t, m["next_cursor"])
	item := m["data"].([]any)[0].(map[string]any)
	require.Len(t, item, 10)
	for _, banned := range []string{"embedding", "embedding_model", "memory_key", "key", "status", "user_id", "importance", "evidence_type", "superseded_by"} {
		require.NotContains(t, item, banned)
	}
	require.Equal(t, 0.95, item["confidence"])
	require.Equal(t, "2026-09-20T10:03:00.123Z", item["first_observed_at"])
}

func TestListMemories_EmptyAndSwitchOff(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	rec := e.do(http.MethodGet, memoriesPath, "", "Authorization", authz)
	require.Equal(t, 200, rec.Code)
	require.JSONEq(t, `{"data":[],"next_cursor":null,"memory_enabled":true}`, rec.Body.String())

	require.Equal(t, 200, e.do(http.MethodPatch, profilePath, `{"preferences":{"memory_enabled":false}}`, "Authorization", authz).Code)
	rec = e.do(http.MethodGet, memoriesPath, "", "Authorization", authz)
	require.JSONEq(t, `{"data":[],"next_cursor":null,"memory_enabled":false}`, rec.Body.String())
}

func TestListMemories_OnlyOwn(t *testing.T) {
	e := newAPI(t)
	a, authA := e.profileUser(t, "a@example.com")
	b, authB := e.profileUser(t, "b@example.com")
	mine := e.seedMemory(a, "PROFILE_FACT", "job")
	theirs := e.seedMemory(b, "PROFILE_FACT", "job")

	ids := func(authz string) []string {
		var out []string
		for _, it := range jsonOf(t, e.do(http.MethodGet, memoriesPath, "", "Authorization", authz))["data"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return out
	}
	require.Equal(t, []string{mine.String()}, ids(authA))
	require.Equal(t, []string{theirs.String()}, ids(authB))
}

func TestListMemories_FiltersExpiredNonActiveAndCategory(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	live := e.seedMemory(uid, "PREFERENCE", "p1")
	e.seedMemory(uid, "EPISODIC", "episode:"+uuid.NewString())
	expired := e.seedMemory(uid, "PREFERENCE", "p2")
	m, _ := e.memStore.Get(uid, expired)
	m.ExpiresAt = apiT0.Add(-time.Second)
	e.putMemory(m)
	sup := e.seedMemory(uid, "PREFERENCE", "p3")
	m, _ = e.memStore.Get(uid, sup)
	m.Status = memory.StatusSuperseded
	e.putMemory(m)

	rec := e.do(http.MethodGet, memoriesPath+"?category=PREFERENCE", "", "Authorization", authz)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	data := jsonOf(t, rec)["data"].([]any)
	require.Len(t, data, 1)
	require.Equal(t, live.String(), data[0].(map[string]any)["id"])
	require.Len(t, jsonOf(t, e.do(http.MethodGet, memoriesPath, "", "Authorization", authz))["data"], 2, "category filter off: preference + episode")
}

func TestListMemories_CursorPagination(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	// 3 categories, 2 rows each with distinct observation times, plus two rows that tie on time
	var want []string
	for _, cat := range []string{"EPISODIC", "LEARNING_PATTERN", "PREFERENCE", "PROFILE_FACT"} { // text order of category
		for i := 0; i < 2; i++ {
			id := e.seedMemory(uid, cat, fmt.Sprintf("%s:k%d", cat, i))
			m, _ := e.memStore.Get(uid, id)
			m.LastObservedAt = memLast.Add(-time.Duration(i) * time.Hour)
			e.putMemory(m)
			want = append(want, id.String())
		}
	}
	tieA, tieB := e.seedMemory(uid, "PROFILE_FACT", "tie:a"), e.seedMemory(uid, "PROFILE_FACT", "tie:b")
	for _, id := range []uuid.UUID{tieA, tieB} {
		m, _ := e.memStore.Get(uid, id)
		m.LastObservedAt = memLast.Add(-10 * time.Hour)
		e.putMemory(m)
	}
	tie := []uuid.UUID{tieA, tieB}
	if tie[0].String() > tie[1].String() {
		tie[0], tie[1] = tie[1], tie[0]
	}
	want = append(want, tie[0].String(), tie[1].String())

	var got []string
	next := ""
	pages := 0
	for {
		q := "?limit=3"
		if next != "" {
			q += "&cursor=" + url.QueryEscape(next)
		}
		rec := e.do(http.MethodGet, memoriesPath+q, "", "Authorization", authz)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		m := jsonOf(t, rec)
		pages++
		for _, it := range m["data"].([]any) {
			got = append(got, it.(map[string]any)["id"].(string))
		}
		c, ok := m["next_cursor"].(string)
		if !ok {
			break
		}
		require.Len(t, m["data"], 3)
		next = c
	}
	require.Equal(t, 4, pages, "10 rows, 3 per page")
	require.Equal(t, want, got, "ordered (category, last_observed_at DESC, id) without gaps or repeats")
}

func TestListMemories_Validation(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	for _, q := range []string{"category=profile_fact", "category=NOPE", "limit=0", "limit=101", "limit=x"} {
		rec := e.do(http.MethodGet, memoriesPath+"?"+q, "", "Authorization", authz)
		require.Equal(t, 400, rec.Code, q)
		require.Equal(t, "VALIDATION_FAILED", codeOf(t, rec), q)
	}
	for name, c := range map[string]string{
		"not base64":    "%21%21",
		"two elements":  encode(`{"v":1,"k":["PREFERENCE","2026-10-08T11:30:00Z"]}`),
		"bad category":  encode(`{"v":1,"k":["NOPE","2026-10-08T11:30:00Z","` + uuid.NewString() + `"]}`),
		"bad time":      encode(`{"v":1,"k":["PREFERENCE","soon","` + uuid.NewString() + `"]}`),
		"bad id":        encode(`{"v":1,"k":["PREFERENCE","2026-10-08T11:30:00Z","x"]}`),
		"wrong version": encode(`{"v":9,"k":["PREFERENCE","2026-10-08T11:30:00Z","` + uuid.NewString() + `"]}`),
	} {
		rec := e.do(http.MethodGet, memoriesPath+"?cursor="+c, "", "Authorization", authz)
		require.Equal(t, 400, rec.Code, name+": "+rec.Body.String())
		require.Equal(t, "INVALID_CURSOR", codeOf(t, rec), name)
	}
	require.Equal(t, 200, e.do(http.MethodGet, memoriesPath+"?category=EPISODIC&limit=100", "", "Authorization", authz).Code)
}

func TestDeleteMemory_OtherUser404(t *testing.T) {
	e := newAPI(t)
	_, authA := e.profileUser(t, "a@example.com")
	b, _ := e.profileUser(t, "b@example.com")
	theirs := e.seedMemory(b, "PROFILE_FACT", "job")

	rec := e.do(http.MethodDelete, memoriesPath+"/"+theirs.String(), "", "Authorization", authA)
	require.Equal(t, 404, rec.Code, rec.Body.String())
	require.Equal(t, "MEMORY_NOT_FOUND", codeOf(t, rec))
	goldenFile(t, "memory", "delete_404.json", fixedRequestID(rec.Body.Bytes()))
	_, still := e.memStore.Get(b, theirs)
	require.True(t, still, "the row of B stays")
	require.Zero(t, e.memVer.Version(b), "and B's sessions keep their context")

	// the answer is the same for an id that does not exist at all
	missing := e.do(http.MethodDelete, memoriesPath+"/"+uuid.NewString(), "", "Authorization", authA)
	require.Equal(t, 404, missing.Code)
	require.JSONEq(t, string(fixedRequestID(rec.Body.Bytes())), string(fixedRequestID(missing.Body.Bytes())))
}

func TestMemoryIDNotUUID_404MemoryNotFound(t *testing.T) {
	e := newAPI(t)
	_, authz := e.profileUser(t, "hana@example.com")
	for _, id := range []string{"nope", "123", "0192f3a4-7c1e-7b2a-9d0e"} {
		rec := e.do(http.MethodDelete, memoriesPath+"/"+id, "", "Authorization", authz)
		require.Equal(t, 404, rec.Code, id)
		require.Equal(t, "MEMORY_NOT_FOUND", codeOf(t, rec), id)
	}
}

func TestDeleteMemory_Owner204AndTombstone(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	id := e.seedMemory(uid, "PROFILE_FACT", "job")

	rec := e.do(http.MethodDelete, memoriesPath+"/"+id.String(), "", "Authorization", authz)
	require.Equal(t, 204, rec.Code, rec.Body.String())
	require.Empty(t, rec.Body.String())
	_, ok := e.memStore.Get(uid, id)
	require.False(t, ok)
	var tombs int
	for _, m := range e.memStore.All(uid) {
		if m.Status == memory.StatusDeleted {
			tombs++
			require.Empty(t, m.Content)
			require.Equal(t, "job", m.Key)
		}
	}
	require.Equal(t, 1, tombs)
	require.Equal(t, int64(1), e.memVer.Version(uid))
	require.Empty(t, jsonOf(t, e.do(http.MethodGet, memoriesPath, "", "Authorization", authz))["data"])

	// a second delete: the memory is gone, so 404 and no second bump
	rec = e.do(http.MethodDelete, memoriesPath+"/"+id.String(), "", "Authorization", authz)
	require.Equal(t, 404, rec.Code)
	require.Equal(t, int64(1), e.memVer.Version(uid))
}

func TestDeleteMemory_NotActiveIs404(t *testing.T) {
	e := newAPI(t)
	uid, authz := e.profileUser(t, "hana@example.com")
	id := e.seedMemory(uid, "PREFERENCE", "p")
	m, _ := e.memStore.Get(uid, id)
	m.Status = memory.StatusSuperseded
	e.putMemory(m)
	rec := e.do(http.MethodDelete, memoriesPath+"/"+id.String(), "", "Authorization", authz)
	require.Equal(t, 404, rec.Code)
	require.Equal(t, "MEMORY_NOT_FOUND", codeOf(t, rec))
}

func TestDeleteAllMemories_IdempotentOnlyOwn(t *testing.T) {
	e := newAPI(t)
	a, authA := e.profileUser(t, "a@example.com")
	b, _ := e.profileUser(t, "b@example.com")
	e.seedMemory(a, "PROFILE_FACT", "job")
	e.seedMemory(a, "EPISODIC", "episode:"+uuid.NewString())
	keep := e.seedMemory(b, "PROFILE_FACT", "job")

	for i := 0; i < 2; i++ {
		rec := e.do(http.MethodDelete, memoriesPath, "", "Authorization", authA)
		require.Equal(t, 204, rec.Code, rec.Body.String())
		require.Empty(t, rec.Body.String())
	}
	require.Empty(t, e.memStore.All(a), "no tombstone either")
	_, ok := e.memStore.Get(b, keep)
	require.True(t, ok)
	require.Equal(t, int64(2), e.memVer.Version(a))
	require.Zero(t, e.memVer.Version(b))
	require.Empty(t, jsonOf(t, e.do(http.MethodGet, memoriesPath, "", "Authorization", authA))["data"])
}
