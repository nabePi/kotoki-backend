# Kaiwa Backend — M8 Memory & Learner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Setelah sesi selesai, percakapan menghasilkan memory jangka panjang (diekstrak, divalidasi, di-dedup, dipakai lagi di sesi berikutnya) dan hasil evaluasi memperbarui profil kemampuan per skill yang menentukan tingkat kesulitan adaptif sesi berikutnya; semuanya terisolasi per user dan terhubung ke runtime realtime menggantikan fake M5.
**Architecture:** Dua paket fitur baru `internal/memory` (`→ prompt, provider, events`) dan `internal/learner` (`→ content, prompt, events`) berisi logika murni (validasi, dedup, algoritma proficiency, adaptive) yang diuji tanpa I/O, port repository kecil di sisi pemakai, dan consumer yang memenuhi `amqp.Handler` M6 secara struktural. `postgres` (sqlc `memories.sql`/`learner.sql`) dan `redisstore` (`memctx`) mengimplementasikan port; `httpapi` hanya decode → service → encode. Wiring manual: `cmd/worker` (consumer), `cmd/api` (service profile/memory + adapter `session.AdaptiveProvider`), `cmd/realtime` (ganti fake `RetrievalService`/`LearnerContextProvider`).
**Tech Stack:** pgx v5 + sqlc (override `vector` → `pgvector-go`), pgvector (exact scan per user, tanpa HNSW), go-redis v9, chi + `httpapi.Handle[B,R]`, testcontainers (`pgvector/pgvector:pg17`, redis, rabbitmq), testify, `golang.org/x/text/unicode/norm` (NFKC; sudah dependensi transitif `x/crypto` family — tambahkan ke allowlist D-003 dan catat di `docs/implementation-deviations.md`).
**Spec:** docs/superpowers/specs/2026-10-12-kaiwa-backend-design.md — Bab 11 (§11.1–§11.12), Bab 12 (§12.1–§12.10), §5.9.7 (kontrak REST profile/memory), §7.8.6 (in-session), §9.9.9, §14.3
**Master plan:** docs/superpowers/plans/2026-10-13-kaiwa-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M1 (migration `000017_learner`, `000018_memories`, `000019_evaluations`, `000011`/`000012`; sqlc; `postgres.Store/Tx`; `testutil.NewPostgres/NewRedis/SeedUser/SeedContent/SeedSession/MustInsert`; `errs`, `clock`, `idgen`, `otel.Start/End`). M2 (`httpapi.Handle/HandleNoBody/Page/Request/WriteError/NewRouter/Deps`, `FieldError`, `EncodeCursor/DecodeCursor/ParseLimit`, `auth.Principal`, `auth.UserService.RequireActive`, `route_table_test.go`). M3 (`session.AdaptiveProvider{Adaptive(ctx, AdaptiveRequest{UserID; Config content.ResolvedConfig}) (json.RawMessage, error)}` + `session.Service.Create` yang memanggilnya, `content.SessionConfigLoader`/`SessionConfig`, `content.EffectiveFormality`, seed `0070_prompts.sql` dengan 5 baris `prompt_configs`, `testutil.AssertReconciled`). M4 (`prompt.Render/Template/Params/RegisterFixtures/EstimateTokens/ItemOverheadTokens/DefaultBudget/LearnerContext`, `provider.AsyncRouter`/`EmbeddingRequest/Result`/`StructuredRequest/Result`, `fake.NewEvaluation`/`fake.NewEmbedding`, `fake/fixtures/`). M5 (runtime memakai port `memory.RetrievalService` + `learner.LearnerContextProvider` dengan fake; `conversation.MaybeAdjust`, `conversation.PromptBuilder`; e2e harness `test/e2e`). M6 (`events.*` payload/queue/`Permanent`/`ProcessedEvents`, `amqp.Handler`, `cmd/worker buildConsumers/consumerDeps`, job `memory_cleanup` + `redisstore.MemVerBumper`, `account_purge`). M7 (event `evaluation.completed`, tabel `evaluations` terisi worker evaluasi; untuk e2e saja).

## Global Constraints

- Master Global Constraints berlaku.
- Skala skill 0.00–5.00 (2 desimal); 7 skill urut D-097: `GRAMMAR, VOCABULARY, FLUENCY, NATURALNESS, PRONUNCIATION, LISTENING_RESPONSE, INTERACTION_COMPLEXITY`; `AlgorithmVersion = 1`; konstanta di `internal/learner/params.go`: `MinTurnsForEvidence 4`, `PivotScore 70`, `UpperSpan 30`, `LowerSpan 35`, `EvidenceBandBelow 2.0`, `EvidenceBandAbove 1.0`, `AlphaFloor 0.15`, `MaxRisePerEval 0.40`, `MaxDropPerEval 0.30`, `ConfGainRate 0.25`, `ConfPenaltyRate 0.15`, `ConfPenaltyGap 1.0`, `ConfMax 0.95`, `ConfHalfLifeDays 90`, `DefaultEvalDimConfidence 0.70`, `TrendWindow 5`, `TrendMinPoints 3`, `TrendMaxAgeDays 120`, `TrendSlope 0.05`; skip skill bila `c_obs < 0.05`.
- Prior onboarding (reseptif/produksi/confidence): `NULL` 1.00/1.00/0.050; `BEGINNER` 0.50/0.50/0.100; `N5` 1.00/0.50/0.100; `N4` 2.00/1.50/0.100; `N3` 3.00/2.50/0.100; `N2` 4.00/3.50/0.100; `N1` 4.50/4.00/0.100 (reseptif = GRAMMAR, VOCABULARY, LISTENING_RESPONSE).
- FocusSkills: maks 2, ambang `P_k ≥ 0.30`, bobot `c_k×relgap + 0.5×f_k + trend(DECLINING +0.30 / IMPROVING −0.10) + 0.2×focus`; tie → level lebih rendah. `Stretch(x) = clamp(R(x+0.3), max(1,B−2), min(5,B+1))`; `R` = round half up. Dialect `FULL` hanya bila `Ld ≥ 3.5` dan `DialectSessions90d ≥ 2`; `NONE` bila `Ld < 2.0`. Penyesuaian in-session: ≥ 2 sinyal klarifikasi dari 6 turn, jeda ≥ 4 turn, maks 2/sesi.
- Memory: `SIM_DEDUP 0.90`; ambang simpan confidence `PROFILE_FACT 0.75`, `PREFERENCE 0.70`, `LEARNING_PATTERN 0.70` (+ ≥ 2 turn), `EPISODIC 0.60`; `content` ≤ 60 rune (EPISODIC 150), `display_text` ≤ 160 (EPISODIC 300); maks 12 kandidat + 1 episode; skip bila < 3 turn user ≥ 5 rune; transcript ≤ 40.000 rune; existing memory ≤ 150; REINFORCE `confidence = LEAST(0.990, GREATEST(c, c_new) + 0.050)`; TTL: `PROFILE_FACT` 730 hari (temporer `valid_days`, default 60, maks 120), `PREFERENCE` 365, `LEARNING_PATTERN` 180, `EPISODIC` 180, `SUPERSEDED` 30, tombstone 90; cap `ACTIVE` per user: `PROFILE_FACT+PREFERENCE` 100, `LEARNING_PATTERN` 40, `EPISODIC` 200.
- Retrieval: core ≤ 600 token & ≤ 20 item (`prompt.DefaultBudget.MemoryCore`), episodik ≤ 3 item, cosine ≥ 0.75, ≤ 500 token (`MemoryEpisodic`); prioritas `0.40×imp + 0.25×conf + 0.15×rec + 0.20×rel`, ambang 0.45, `rec = exp(−days/90)`, `imp = min(1, importance + 0.05×(obs−1))`; deadline build 2.000 ms, embedding query 1.500 ms, refresh episodik 1.500 ms; Redis `kaiwa:session:{id}:memctx` (TTL **4500 s** = durasi maks 3600 + 900; nama dari plan karena `SessionContextInput` tidak membawa durasi) dan `kaiwa:user:{id}:memver`.
- Prompt: `MEMORY_EXTRACTION` di DB (`system_template`, `user_template`, `output_schema`, `llm_params` `{"temperature":0,"max_output_tokens":2000}`); `SchemaName = kaiwa_memory_extraction_v{version}`; `RequestID = "MEMORY_EXTRACTION:{session_id}"`; `users.full_name` MUST NOT masuk prompt apa pun; `learning_goal_note` tidak masuk prompt.
- Endpoint (semua `api_user_default`, Bearer; tulis butuh `users.status=ACTIVE` → `403 ACCOUNT_SUSPENDED`): `GET/PATCH /profile`, `GET /profile/skills`, `GET /profile/skills/{skill}/history`, `DELETE /profile/skills`, `GET/DELETE /profile/memories`, `DELETE /profile/memories/{memory_id}`; `404 MEMORY_NOT_FOUND` tidak membedakan sebab; limit default 20/maks 100.
- Metrik memory: `kaiwa_memory_extraction_duration_seconds`, `kaiwa_memory_candidates_total{category}`, `kaiwa_memory_candidates_rejected_total{reason}`, `kaiwa_memory_apply_total{action}`, `kaiwa_memory_retrieval_duration_seconds{stage}`, `kaiwa_memory_retrieval_degraded_total{stage}`, `kaiwa_memory_context_tokens{part}`, `kaiwa_memory_cleanup_deleted_total`; learner `kaiwa_learner_replays_total{skill}`. Log memory tidak memuat `content`/`display_text`.

## Review Focus

Butir master 1–5 dimiliki M3/M5/M6, bukan M8. Butir M8 (tersirat spec, paling mungkin menggigit user):
1. **Isolasi antar user** (memory & learner; embedding identik tidak boleh bocor, id milik user lain → 404, event dengan `user_id` tidak cocok → DLQ) — Task 7, 14 (`TestSearchEpisodic_Isolation`, `TestFindNearestActive_Isolation`, static `TestMemoryQueriesAreUserScoped`), 15 (`TestMemoryWorker_UserMismatch`), 17 (`TestDeleteMemory_OtherUser404`, `TestListMemories_OnlyOwn`).
2. **Memory tidak menyimpan data sensitif/asumsi/injection dan re-delivery tidak menggandakan** — Task 11 (filter sensitif, ≥ 2 term/kategori), 12 (12 aturan, satu test per `reason`), 15 (`TestMemoryWorker_RedeliveryDoesNotDoubleCount`).
3. **`evaluation.completed` duplikat / tidak berurutan menghasilkan state skill identik** (replay S-19) — Task 3 (permutasi murni), Task 9 (`TestLearner_OutOfOrder_*` lewat consumer).
4. **Hapus memory / `memory_enabled=false` benar-benar berlaku** di sesi berikutnya (tombstone mencegah re-extract 90 hari, retrieval kosong, `memver` naik sehingga sesi aktif membuangnya) — Task 13, 15, 16, 17.
5. **Kegagalan embedding/DB/Redis tidak menggagalkan sesi** (`Degraded=true`, tanpa error) dan **`full_name` tidak pernah sampai ke prompt** — Task 16 (`TestBuildSessionContext_EmbedFailureDegrades`), Task 6 (`TestPromptContext_NeverContainsFullName`).

## File Structure

| Path | Tanggung jawab |
|---|---|
| `internal/learner/{types,skill,params,proficiency,trend,priors,focus,adaptive,render,repository,service,profile,skills_view,consumer}.go` | Domain learner: tipe, algoritma §12.5 (murni), prior, FocusSkills, `ComputeAdaptiveDifficulty` murni, render prompt, port, service, consumer |
| `internal/prompt/adaptive_descriptors.go` | Tabel descriptor Jepang §12.8.4 (kunci primitif; `prompt` tidak mengimpor `learner`) |
| `internal/memory/{types,registry,sensitive,prompt,extraction,validate,apply,repository,consumer,retrieval,service}.go` | Domain memory: tipe, registry key, filter sensitif, prompt data, 12 aturan, dedup, port, consumer, `RetrievalService`, service REST |
| `db/queries/{memories,learner}.sql`, `internal/postgres/{memory_repo,learner_repo}.go` | Query sqlc user-scoped + implementasi port (`Store.MemoryStore()`, `Store.LearnerStore()`) |
| `internal/redisstore/memctx.go` | `memctx` (JSON `MemoryContext`) + baca `memver` (menambah `MemVerBumper` M6) |
| `internal/httpapi/{profile,memory}_{routes,handler,dto}.go` | 8 endpoint §5.9.7 |
| `internal/testutil/fakes/{memory_store,learner_store}.go` | Fake in-memory tulisan tangan (cosine nyata) |
| `seed/0070_prompts.sql`, `internal/provider/fake/fixtures/kaiwa_memory_extraction_v1.json` | Seed `MEMORY_EXTRACTION` v1; fixture fake |
| `cmd/worker/wiring.go`, `cmd/api/{main,adaptive_adapter}.go`, `cmd/realtime/{main,providers}.go` | Registrasi consumer, adapter `session.AdaptiveProvider`, ganti `noopMemory`/`staticLearner` M5 |
| `test/integration/m8_*_test.go`, `test/e2e/m8_memory_learner_test.go` | Gerbang M8 |

---

### Task 1: Kontrak tipe `learner` yang belum ada (rekonsiliasi dengan M5)

**Files:**
- Create: `internal/learner/skill.go`, `internal/learner/types.go`
- Test: `internal/learner/types_test.go`, `internal/memory/types_test.go` (karakterisasi tipe milik M5)

**Interfaces:**
- Consumes (sudah dibuat M5, **tidak diubah**): `memory.Category` + 4 konstanta, `ContextItem`, `MemoryContext`, `SessionContextInput`, `TurnText`, `RetrievalService` (§11.7.1 persis); `learner.AdaptiveDifficulty`, `Formality`, `DialectExposure`, `Skill` (string), `TTSSpeakingRate(speed int) float64`, `MaxAISentences(speed int) int` (`internal/learner/difficulty.go`); `conversation.LearnerContextProvider{PromptContext(ctx, userID uuid.UUID, ad learner.AdaptiveDifficulty) (prompt.LearnerContext, error); Rerender(prev prompt.LearnerContext, ad learner.AdaptiveDifficulty) prompt.LearnerContext}` (`internal/conversation/ports.go`); `rtstate.Store.ReadMemCtx`. Verifikasi dulu: `grep -rn 'type AdaptiveDifficulty\|type MemoryContext\|SkillGrammar' internal/` ; bila konstanta `Skill` sudah ada, jangan definisikan ulang.
- Produces: konstanta `SkillGrammar, SkillVocabulary, SkillFluency, SkillNaturalness, SkillPronunciation, SkillListeningResponse, SkillInteractionComplexity` (nilai string = nama D-097), `AllSkills() []Skill` (urutan D-097), `ParseSkill(s string) (Skill, bool)`, `FindingStat{Major, Minor int}`, `AdaptiveInput` (§12.8.2 persis, ditambah `Focus []Skill` — hasil `FocusSkills`; nama dari plan karena §12.8.3 membutuhkannya tetapi §12.8.2 tidak memuatnya; field `Findings` pada §12.8.2 bernama `FindingStats`).

- [ ] **Step 1: Write the failing test**
```go
func TestMemoryContextJSONKeys(t *testing.T) {
	b, _ := json.Marshal(memory.MemoryContext{})
	var m map[string]any; require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"session_id","user_mem_ver","core","core_tokens","episodic","episodic_tokens","built_at","degraded"} { require.Contains(t, m, k) }
}
func TestAllSkillsOrderD097(t *testing.T) {
	require.Equal(t, []string{"GRAMMAR","VOCABULARY","FLUENCY","NATURALNESS","PRONUNCIATION","LISTENING_RESPONSE","INTERACTION_COMPLEXITY"}, skillNames(learner.AllSkills()))
	_, ok := learner.ParseSkill("TASK_ACHIEVEMENT"); require.False(t, ok)
}
func TestAdaptiveDifficultyJSONKeys(t *testing.T) { /* marshal AdaptiveDifficulty{} (tipe M5): 20 key persis daftar §12.8.2; bila focus_skills marshal sebagai null, tambahkan AdaptiveDifficulty.MarshalJSON yang menormalkan menjadi [] (perubahan aditif, tanpa mengubah field) */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run 'TestAllSkills|TestAdaptiveDifficultyJSONKeys'`. Expected: FAIL (`AllSkills`/`ParseSkill` belum ada); `go test ./internal/memory/ -race -run TestMemoryContextJSONKeys` langsung PASS (karakterisasi; bila FAIL, M5 menyimpang dari §11.7.1 — perbaiki tag JSON, catat di deviations).
- [ ] **Step 3: Implement** `skill.go`/`types.go` persis spec.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/learner/ ./internal/memory/ -race` → PASS; `go build ./...` hijau (M5 tetap kompilasi).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory,learner): contract types per spec chapters 11-12"`

### Task 2: Algoritma proficiency murni — evidence, step, confidence (+ golden)

**Files:**
- Create: `internal/learner/params.go`, `internal/learner/proficiency.go`
- Test: `internal/learner/proficiency_test.go`

**Interfaces:**
- Produces: konstanta Global Constraints; `type SkillState struct{ Level, Confidence float64; Count int; Ceiling *float64; LastEvidenceAt *time.Time }`; `type Estimate struct{ EvaluationID uuid.UUID; AssessedAt time.Time; RawScore int; DEff *int; LObs, CObs float64 }`; `type Applied struct{ Previous, Level, Confidence float64; Count int; Ceiling float64; Trend Trend }` (`Trend` didefinisikan Task 3); `func EvidenceLevel(skill Skill, score, dEff int) float64` (langkah 1, hasil `round2`); `func EvidenceConfidence(qEval, qSample float64) float64` (`round3`); `func ShouldSkip(cObs float64) bool` (`cObs < 0.05`); `func Step(st SkillState, e Estimate, hist []TrendPoint) (SkillState, Applied)` (langkah 3–7); `func round2(x float64) float64`, `round3` (half away from zero, `math.Round`). Keputusan (ambigu di spec): `L_obs` dibulatkan 2 desimal dan `c_obs` 3 desimal **sebelum** dipakai, agar jalur normal identik dengan replay yang membaca kolom tersimpan `NUMERIC(3,2)/(4,3)` (§12.2.3); angka golden spec tidak berubah — catat di deviations.

- [ ] **Step 1: Write the failing test**
```go
func TestProficiencyGolden_Grammar(t *testing.T) { // §12.5.4, N4: L=2.00 c=0.100 n=0
	st := learner.SkillState{Level: 2.00, Confidence: 0.100}
	d := func(v int) *int { return &v }
	t0 := time.Date(2026,10,1,10,0,0,0,time.UTC)
	e1 := learner.Estimate{AssessedAt: t0, RawScore: 82, DEff: d(2)}
	e1.LObs, e1.CObs = learner.EvidenceLevel(learner.SkillGrammar, 82, 2), learner.EvidenceConfidence(0.80, 1.0)
	require.InDelta(t, 2.40, e1.LObs, 1e-9)
	st, a := learner.Step(st, e1, nil)
	require.Equal(t, 2.16, st.Level); require.Equal(t, 0.280, a.Confidence); require.Equal(t, 1, st.Count); require.Equal(t, learner.TrendInsufficient, a.Trend)
	e2 := learner.Estimate{AssessedAt: t0.AddDate(0,0,3), RawScore: 91, DEff: d(2)}
	e2.LObs, e2.CObs = learner.EvidenceLevel(learner.SkillGrammar, 91, 2), learner.EvidenceConfidence(0.85, 0.75) // 2.70, 0.638
	st, a = learner.Step(st, e2, []learner.TrendPoint{{At: t0, Level: 2.16}})
	require.Equal(t, 2.27, st.Level); require.Equal(t, 0.389, a.Confidence); require.Equal(t, learner.TrendInsufficient, a.Trend)
	e3 := learner.Estimate{AssessedAt: t0.AddDate(0,0,7), RawScore: 58, DEff: d(3)}
	e3.LObs, e3.CObs = learner.EvidenceLevel(learner.SkillGrammar, 58, 3), learner.EvidenceConfidence(0.80, 1.0) // 2.66, 0.80
	st, a = learner.Step(st, e3, []learner.TrendPoint{{At: t0, Level: 2.16}, {At: e2.AssessedAt, Level: 2.27}})
	require.Equal(t, 2.35, st.Level); require.Equal(t, 0.502, a.Confidence); require.Equal(t, learner.TrendImproving, a.Trend)
	require.NotNil(t, st.Ceiling); require.Equal(t, 2.35, *st.Ceiling)
}
func TestStepRiseCap(t *testing.T)   { /* L_old 2.00,n=0,c_obs 1.0,L_obs 3.00 → 2.40 */ }
func TestStepDropCap(t *testing.T)   { /* L_old 4.00, n=0, c_obs 1.0, L_obs 0.0 → 3.70 (−0.30) */ }
func TestStepConfidencePenalty(t *testing.T) { /* L_old 3.50 c_old 0.70 n=6 L_obs 2.20 c_obs 0.80, LastEvidenceAt==AssessedAt → L 3.34, c 0.676 */ }
func TestEvidenceLevelBand(t *testing.T) { /* D_eff=2: s=100 → 3.00 (band atas D+1); s=0 → 0.00 (band bawah max(0,D−2)); s=70 → 2.00; D_eff=5,s=100 → 5.00 */ }
func TestEvidenceLevelPronunciation(t *testing.T) { require.Equal(t, 2.80, learner.EvidenceLevel(learner.SkillPronunciation, 78, 0)); /* s=40 → 0; s=100 → 5 */ }
func TestStepSkipsWhenCObsBelow005(t *testing.T) { /* Step tidak dipanggil: helper learner.ShouldSkip(cObs) true untuk 0.049, false untuk 0.05 */ }
func TestStepDecaysConfidenceByHalfLife(t *testing.T) { /* 90 hari sejak last → c_old×0.5 sebelum gain */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run 'TestProficiencyGolden|TestStep|TestEvidence'`. Expected: FAIL.
- [ ] **Step 3: Implement** langkah 1–6 §12.5.1 (`α = max(0.15, 1/(n+2)) × c_obs`, `Δ = clamp(α(L_obs−L_old), −0.30, +0.40)`, `gain/penalty`, `c ≤ 0.95`, `ceiling = max`); decay memakai `t − LastEvidenceAt` dalam hari; `Step` memanggil `Trend` (Task 3) — sementara stub yang mengembalikan `TrendInsufficient`, diganti di Task 3.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS (kecuali assertion trend `Improving` yang lulus di Task 3; tandai `t.Skip("task 3")` hanya pada baris itu sampai Task 3).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): proficiency algorithm v1 with golden values"`

### Task 3: Trend OLS + `Replay` out-of-order (S-19), murni

**Files:**
- Create: `internal/learner/trend.go`
- Modify: `internal/learner/proficiency.go` (lepas stub trend)
- Test: `internal/learner/trend_test.go`, `internal/learner/replay_test.go`

**Interfaces:**
- Produces: `type Trend string` (`TrendInsufficient="INSUFFICIENT_DATA"`, `TrendImproving`, `TrendStable`, `TrendDeclining`); `type TrendPoint struct{ At time.Time; Level float64 }`; `func ComputeTrend(hist []TrendPoint, newPoint TrendPoint, nNew int) Trend` (ambil titik `At > t−120 hari`, ≤ 4 terakhir + titik baru; `nNew < 3` atau < 3 titik → `INSUFFICIENT_DATA`; slope OLS `≥ +0.05` IMPROVING, `≤ −0.05` DECLINING); `func Replay(prior SkillState, ests []Estimate) (final SkillState, rows []Applied)` (urut `(AssessedAt, EvaluationID)`, mulai dari `prior` tanpa `LastEvidenceAt`, trend memakai `hist` yang dikumpulkan; menghitung ulang kolom turunan; `final.LastEvidenceAt` = max).

- [ ] **Step 1: Write the failing test** — `TestTrend`: `[2.0,2.0,2.0]`→STABLE; `[2.4,2.2,2.0]`→DECLINING; dua titik→INSUFFICIENT_DATA; titik > 120 hari diabaikan (hist 3 titik lama + baru → INSUFFICIENT). `TestReplay_OutOfOrderEqualsInOrder`: tiga estimate golden Task 2 (`LObs/CObs` tersimpan 2.40/0.800, 2.70/0.638, 2.66/0.800; DEff 2,2,3) dikirim ke `Replay` dalam semua 6 permutasi urutan → `final` identik dengan urutan 1,2,3: `Level 2.35`, `Confidence 0.502`, trend `IMPROVING`, `Count 3`, `LastEvidenceAt = tanggal sesi 3`; `TestReplayIdempotent` (Replay dua kali pada hasilnya → sama); `TestReplayTieBreakByEvaluationID` (`AssessedAt` sama → urutan menurut UUID); `TestStepEqualsReplayAppend` (hasil `Step` berantai == `Replay` untuk 5 estimate acak seed tetap).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run 'TestTrend|TestReplay|TestStepEqualsReplay'`. Expected: FAIL.
- [ ] **Step 3: Implement** OLS: `b = Σ(x−x̄)(y−ȳ)/Σ(x−x̄)²`, `x = 0..k−1`; `Replay` memanggil `Step` berulang.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/learner/ -race` → PASS (lepas `t.Skip` Task 2).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): trend and deterministic replay for out-of-order evaluations"`

### Task 4: Prior onboarding + `FocusSkills`, murni

**Files:**
- Create: `internal/learner/priors.go`, `internal/learner/focus.go`
- Test: `internal/learner/priors_test.go`, `internal/learner/focus_test.go`

**Interfaces:**
- Produces: `type Prior struct{ Level, Confidence float64 }`; `func PriorFor(level *string, skill Skill) Prior` (tabel Global Constraints); `func EffectiveLevel(level float64, ceiling *float64) float64` (`max(L, ceiling−0.5)`); `func EffectiveConfidence(conf float64, lastEvidence *time.Time, now time.Time) float64` (`conf × 0.5^(days/90)`; nil → apa adanya); `type FocusInput struct{ Levels, Confidences map[Skill]float64; Trends map[Skill]Trend; Findings map[Skill]FindingStat; Evaluations int; ScenarioFocus []Skill }`; `func FocusSkills(in FocusInput) []Skill`; `func LevelLabel(l float64) string` (<0.50 `BEGINNER`; <1.50 `N5`; <2.50 `N4`; <3.50 `N3`; <4.50 `N2`; else `N1`); `func ConfidenceLabel(c float64) string` (<0.30 `LOW`; <0.60 `MEDIUM`; else `HIGH`).

- [ ] **Step 1: Write the failing test** — `TestPriorTable` (tabel penuh 7 level × produksi/reseptif: `N4` GRAMMAR 2.00 & FLUENCY 1.50 conf 0.100; `nil` semua 1.00/0.050; `N1` PRONUNCIATION 4.00). `TestFocusSkills` contoh §12.7: levels `GRAMMAR 2.35, VOCABULARY 3.10, FLUENCY 2.40, NATURALNESS 2.20, PRONUNCIATION 2.80, LISTENING_RESPONSE 2.90, INTERACTION_COMPLEXITY 2.00`, conf `0.50/–/0.45/0.45/–/–/0.10` (lain 0.30), trend GRAMMAR IMPROVING, findings (3 evaluasi) GRAMMAR `{2,4}` NATURALNESS `{0,3}` → `[GRAMMAR, NATURALNESS]` dan `P(GRAMMAR)≈0.437`, `P(NATURALNESS)≈0.318` (InDelta 0.001 lewat helper `priority`); `TestFocusSkillsNoneBelowThreshold` (semua P < 0.30 → `[]`, bukan nil); `TestFocusSkillsScenarioFocusAddsPoint2`; `TestFocusSkillsTieLowerLevelWins`; `TestEffectiveLevelUsesCeiling` (L 2.0, ceiling 3.0 → 2.5); `TestLabels`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run 'TestPrior|TestFocus|TestEffective|TestLabels'`. Expected: FAIL.
- [ ] **Step 3: Implement** rumus §12.7 (`f_k = min(1, (2×MAJOR+MINOR)/(3×E))`, `E=0 → 0`).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): onboarding priors and focus skills"`

### Task 5: `ComputeAdaptiveDifficulty` murni (13 dimensi) + turunan tempo

**Files:**
- Create: `internal/learner/adaptive.go`
- Test: `internal/learner/adaptive_test.go`

**Interfaces:**
- Consumes: M5 `learner.TTSSpeakingRate(speed int) float64` dan `learner.MaxAISentences(speed int) int` (sudah ada + ditest di M5; **dipakai, tidak didefinisikan ulang**).
- Produces: `func ComputeAdaptiveDifficulty(in AdaptiveInput) AdaptiveDifficulty` (fungsi paket, murni; berdampingan dengan method `(*Service).ComputeAdaptiveDifficulty` Task 10); `func Stretch(x float64, b int) int`; `func MaxFollowUps(depth int) int` (1→0, 2→1, 3→2, 4→3, 5→3); `func roundHalfUp(x float64) int`.

- [ ] **Step 1: Write the failing test** — `TestComputeAdaptiveDifficulty_Spec1283Example`: `B=4`, level §12.7, `TopicSessions90d=1`, `EffectiveFormality=KEIGO`, `VoiceAccent=KANTO`, pref `NORMAL/AUTO/STANDARD_ONLY`, focus `[GRAMMAR, NATURALNESS]` (dari `FocusSkills`) → `json.Marshal` `JSONEq` ke objek contoh §12.8.3 (`vocabulary_complexity 3, grammar_complexity 3, speech_speed 3, tts_speaking_rate 0.95, max_ai_sentences_per_turn 3, topic_novelty 4, topic_abstraction 3, question_complexity 3, follow_up_depth 2, max_follow_ups 1, expected_response_length 3, spontaneity 3, scaffolding 3, formality "KEIGO_LIGHT", dialect_exposure "NONE", turn_taking_complexity 3, focus_skills ["GRAMMAR","NATURALNESS"], in_session_adjustments 0, algorithm_version 1`). Table test: `TestAdaptive_LowGrammarOnlyAffectsGrammarScaffoldingFormality` (turunkan L(GRAMMAR) → hanya `grammar_complexity`, `scaffolding`, `formality` berubah; sisa dimensi identik dengan baseline), `TestAdaptive_StretchBounds` (`B=1` → ≥1, ≤2; `B=5` → ≥3), `TestAdaptive_PrefSlowerLowersSpeedByOne` (dan FASTER +1; clamp 1..5), `TestAdaptive_DialectRules` (accent KANTO→NONE; `STANDARD_ONLY`→NONE; `Ld=1.9`→NONE; `Ld=3.5`+`DialectSessions90d=2`→FULL; `Ld=3.5`+1 sesi→LIGHT; `ALLOW_DIALECT`+`Ld=2.5`→LIGHT), `TestAdaptive_Formality` (efektif KEIGO + L(GRAMMAR) 2.4 → KEIGO_LIGHT; 2.5 → KEIGO; efektif POLITE + `PrefFormality=CASUAL` → CASUAL; `AUTO` → POLITE), `TestAdaptive_ScaffoldingBands` (gap 1.5→3, 0.75→2, 0→1, <0→0), `TestAdaptive_TopicNovelty` (`B=3`, sesi 0..6 → `R(2+0.5×min(n,4))` = 2,3(`R(2.5)`),3,4(`R(3.5)`),4,4,4 clamp 1..5), `TestRoundHalfUp`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run 'TestComputeAdaptive|TestAdaptive|TestRoundHalfUp'`. Expected: FAIL.
- [ ] **Step 3: Implement** tabel §12.8.3 baris 1–13; `FocusSkills` diisi pemanggil (input `FocusSkills []Skill` ditambahkan ke `AdaptiveInput` sebagai hasil Task 4 — nama field `Focus` (nama dari plan) karena spec §12.8.2 tidak memuatnya padahal §12.8.3 membutuhkannya); `R(x)=floor(x+0.5)`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): adaptive difficulty across 13 dimensions"`

### Task 6: Descriptor prompt, `RenderAdaptation`, `NewPromptContext`

**Files:**
- Create: `internal/prompt/adaptive_descriptors.go`, `internal/learner/render.go`
- Test: `internal/prompt/adaptive_descriptors_test.go`, `internal/learner/render_test.go`

**Interfaces:**
- Consumes: `prompt.LearnerContext`, `prompt.EstimateTokens` (M4).
- Produces (`prompt`): `func DimensionDescriptor(dim string, level int) string` (`dim` ∈ `vocabulary, grammar, speech_speed, topic_novelty, topic_abstraction, question_complexity, follow_up_depth, expected_response_length, spontaneity, turn_taking`; level 1–5; teks verbatim tabel §12.8.4), `ScaffoldingDescriptor(n int) string` (0–3), `FormalityDescriptor(f string) string`, `DialectDescriptor(d string) string`, `FocusDescriptor(skill string) string`, `MaxSentencesLine(n int) string` (`一回の発話は{N}文以内にする。`), `GoalDescriptor(goal string) string` (descriptor per tabel §12.8.4, mis. `JOB_INTERVIEW` → `日本での就職面接に向けた練習`; `OTHER`/`""` → `""`; prefix `学習者の目標: ` dan akhiran `。` ditambahkan `RenderAdaptation`), `const ClosingLine = "学習者の間違いを指摘・訂正しないでください。"`. Produces (`learner`): `type ProfileSnapshot struct{ Nickname, LearningGoal *string }`; `func RenderAdaptation(goalDescriptor string, ad AdaptiveDifficulty) string`; `func NewPromptContext(p ProfileSnapshot, ad AdaptiveDifficulty) prompt.LearnerContext` (`DisplayName` = nickname atau `""`; `GoalLine` = descriptor goal; `FocusSkills` string; `Adaptation`); `func Rerender(prev prompt.LearnerContext, ad AdaptiveDifficulty) prompt.LearnerContext` (mempertahankan `DisplayName` dan `GoalLine`, menghitung ulang `FocusSkills`/`Adaptation` dari `ad`; untuk `conversation.LearnerContextProvider.Rerender` M5).

- [ ] **Step 1: Write the failing test**
```go
func TestRenderAdaptation_Layout(t *testing.T) { // contoh §12.8.3 + goal JOB_INTERVIEW
	out := learner.RenderAdaptation(prompt.GoalDescriptor("JOB_INTERVIEW"), exampleAD())
	lines := strings.Split(out, "\n")
	require.Equal(t, "<conversation_difficulty>", lines[0])
	require.Equal(t, "学習者の目標: 日本での就職面接に向けた練習。", lines[1])
	require.Equal(t, "学習のねらい:", lines[len(lines)-5]) // 2 descriptor + ClosingLine + </conversation_difficulty>
	require.Equal(t, prompt.ClosingLine, lines[len(lines)-2]); require.Equal(t, "</conversation_difficulty>", lines[len(lines)-1])
	require.Contains(t, out, "一回の発話は3文以内にする。")
}
func TestRenderAdaptation_NoFocusNoGoal_OmitsHeaders(t *testing.T) { /* tidak ada "学習者の目標" dan tidak ada "学習のねらい:" */ }
func TestLearnerContextWorstCaseBudget(t *testing.T) { /* descriptor terpanjang tiap dimensi, 2 focus terpanjang, goal terpanjang, DisplayName 30 rune */ 
	require.LessOrEqual(t, prompt.EstimateTokens(ctx.Adaptation+ctx.DisplayName), prompt.DefaultBudget.Learner) // 700
	require.LessOrEqual(t, utf8.RuneCountInString(ctx.Adaptation), 800) }
func TestPromptContext_NeverContainsFullName(t *testing.T) { /* ProfileSnapshot hanya Nickname; nickname nil → DisplayName == ""; NewPromptContext tidak punya parameter full_name (tanda tangan diuji lewat reflect: 2 argumen) */ }
func TestRerender_KeepsDisplayNameAndGoal_ReplacesAdaptation(t *testing.T) { /* ad.Scaffolding 3 → 2 → Adaptation berubah, DisplayName/GoalLine identik dengan prev */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/ ./internal/learner/ -race -run 'Descriptor|RenderAdaptation|WorstCase|PromptContext'`. Expected: FAIL.
- [ ] **Step 3: Implement** — salin descriptor **apa adanya** dari tabel §12.8.4 (jangan parafrase); `RenderAdaptation` urutan tetap: header, goal (opsional), 10 baris dimensi (vocabulary, grammar, speech_speed, topic_novelty, topic_abstraction, question_complexity, follow_up_depth, expected_response_length, spontaneity, turn_taking), scaffolding, formality, dialect, `MaxSentencesLine`, `学習のねらい:` + descriptor focus (hanya bila ada), `ClosingLine`, footer. **Risiko spec**: klaim "±654 rune ≈ 630 token" bertentangan dengan estimator (1,1 token/rune CJK → ≈ 720 token). Bila `TestLearnerContextWorstCaseBudget` gagal pada baris token: jangan ubah teks descriptor; catat di `docs/implementation-deviations.md`, pertahankan asersi ≤ 800 rune, dan ubah asersi token menjadi `≤ DefaultBudget.Learner + prompt.EstimateTokens(focusBlock)` (pemangkasan baris `学習のねらい` oleh builder M5, §7.8.1, sudah menutup selisih) — verifikasi pemangkasan itu di Task 20.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner,prompt): adaptation descriptors and prompt context rendering"`

### Task 7: Query & repository learner (Postgres) + isolasi

**Files:**
- Create: `db/queries/learner.sql`, `internal/learner/repository.go`, `internal/postgres/learner_repo.go`, `internal/testutil/fakes/learner_store.go`
- Modify: `internal/postgres/store.go` (aksesor `LearnerStore()`), `sqlc.yaml` (bila perlu regenerasi), `internal/postgres/sqlcgen/` (hasil `make sqlc`)
- Test: `internal/learner/queries_test.go` (unit), `internal/postgres/learner_repo_integration_test.go` (`integration`)

**Interfaces:**
- Produces (port di `learner/repository.go`; semua method menerima `userID` wajib): tipe baris `Profile{UserID; Nickname, SelfDeclaredLevel, TargetLevel, LearningGoal, LearningGoalNote *string; PreferredSpeechSpeed, PreferredFormality, DialectPreference string; MemoryEnabled bool; OnboardingCompletedAt *time.Time; CreatedAt, UpdatedAt time.Time}`, `StateRow{Skill Skill; PriorLevel, PriorConfidence, Level, Confidence float64; Ceiling *float64; Trend Trend; EvidenceCount int; LastEvaluationID *uuid.UUID; LastEvidenceAt *time.Time}`, `EstimateRow{ID, EvaluationID, SessionID uuid.UUID; Skill Skill; AlgorithmVersion int; RawScore float64; DEff *int; LObs, CObs float64; AssessedAt time.Time; Previous, Level, Confidence float64; Count int; Trend Trend}`, `EvaluationEvidence{EvaluationID, SessionID, UserID uuid.UUID; Status string; Scores map[Skill]*int; ListeningSignal, InteractionSignal *int; DimensionConfidence map[string]float64; PronunciationStatus string; PronunciationUtteranceCount int; SessionEndedAt time.Time; AdaptiveDifficulty json.RawMessage; ScenarioDifficulty int; NTurns int}`; port `ProfileRepository{Ensure(ctx, userID) error; Get; Update(ctx, userID, ProfileRow) (Profile, error); LockUser(ctx, userID) error; ResetPriors(ctx, userID, level *string, now) error; Stats(ctx, userID) (ProfileStats{CompletedSessions int; PracticeSeconds int64; LastSessionAt *time.Time}, error)}`, `SkillStateRepository{List(ctx, userID, forUpdate bool) ([]StateRow, error); Save(ctx, userID, StateRow, now) error; Reset(ctx, userID, now) error}`, `EstimateRepository{Insert(ctx, userID, EstimateRow) (inserted bool, err error) /* ON CONFLICT (source_evaluation_id, skill) DO NOTHING */; ListBySkill(ctx, userID, skill) ([]EstimateRow, error) /* assessed_at ASC, source_evaluation_id ASC */; UpdateDerived(ctx, userID, EstimateRow) error; DeleteAll(ctx, userID) error; History(ctx, userID, skill, after *HistoryCursor, limit int) ([]EstimateRow, error)}`, `EvaluationReader{Evidence(ctx, userID, evaluationID uuid.UUID) (EvaluationEvidence, error)}`, `Reader` (non-tx: `RecentFindings(ctx, userID) (map[Skill]FindingStat, int, error)` = `CountRecentFindingsBySkill` §12.7; `VoiceExposure(ctx, userID, now) ([]ExposureRow, error)` = `GetVoiceExposure` §12.8.6; `TopicSessions90d(ctx, userID, topicID, now) (int, error)`; `DialectSessions90d`), `Store{WithinTx(ctx, func(ctx, Tx) error) error; Reader() Reader}`, `Tx{Profiles(); SkillStates(); Estimates(); Evaluations() EvaluationReader; Processed() events.ProcessedEvents}`; `ErrEvaluationNotVisible` (retryable). `postgres.NewLearnerRepo` + `(*Store).LearnerStore() learner.Store`; `fakes.NewLearnerStore()` (in-memory, mengimplementasi port yang sama; `repotest`-style suite `learnertest.RunSuite(t, mk)` dijalankan ke fake & Postgres).

- [ ] **Step 1: Write the failing test** — unit `TestLearnerQueriesAreUserScoped` (parse `db/queries/learner.sql`: tiap blok `-- name:` non-`Sys` memuat `user_id = `). Integration (`testutil.NewPostgres`): `TestEnsure_Idempotent_SevenStates` (dua kali → 1 profil, 7 state, prior `NULL` → 1.00/0.050), `TestEstimateInsert_ConflictNoop` (insert kedua `(evaluation, skill)` sama → `inserted=false`; insert langsung SQL mentah → `RequirePgErr(23505, "ux_proficiency_estimates_eval_skill")`), `TestSkillStateEvidenceConsistencyCheck` (`evidence_count>0` tanpa `last_evidence_at` → `23514`), `TestLearnerIsolation_OtherUserRowsInvisible` (user A & B; `List/History/Get/Stats` untuk A tidak memuat B; `Update` userID A pada baris B tidak mengubah apa pun), `TestHistory_OrderAndCursor` (`assessed_at DESC, id DESC`, `limit+1`), `TestEvidence_ReadsNTurns` (turn `COMPLETED`/`INTERRUPTED` dengan `char_length(user_text) ≥ 5`; `DISCARDED`/4-rune tidak dihitung), `TestEvidence_UserMismatch` (`evaluations.user_id ≠ userID` → `errs` NOT found), `TestRecentFindings_Last5CompletedEvaluations` (spec query), `TestVoiceExposure_Window90d_CompletedOnly`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run Queries`; `go test -tags integration ./internal/postgres/... -race -run 'TestEnsure|TestEstimate|TestSkillState|TestLearnerIsolation|TestHistory|TestEvidence|TestRecentFindings|TestVoiceExposure'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `make sqlc` setelah menulis `learner.sql` (SQL §12.3 `EnsureProfile` dua statement; `CountRecentFindingsBySkill`, `GetVoiceExposure` verbatim spec); `advisory lock` = `SELECT pg_advisory_xact_lock(hashtextextended('learner:' || $1, 0))`; `Evidence` join `evaluations`, `sessions`, `scenario_versions` dan `COUNT` turn valid.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `make sqlc-check` bersih.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner,postgres): learner repositories with user-scoped queries"`

### Task 8: `learner.Service` — profil, onboarding, skills, history, reset

**Files:**
- Create: `internal/learner/service.go`, `internal/learner/profile.go`, `internal/learner/skills_view.go`
- Test: `internal/learner/service_test.go` (unit, `fakes.NewLearnerStore`), `internal/learner/service_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: Task 7 port, `clock.Clock`, `idgen.Generator`.
- Produces: `type Deps struct{ Store Store; Content content.SessionConfigLoader; Clock clock.Clock; IDs idgen.Generator; Log *slog.Logger }`; `func NewService(d Deps) *Service`; `type Field[T any] struct{ Set, Null bool; Value T }` dengan `UnmarshalJSON` (kunci absen → `Set=false`; `null` → `Set,Null`; nilai → `Set`); `type PatchInput struct{ Nickname, SelfDeclaredLevel, TargetLevel, LearningGoal, LearningGoalNote Field[string]; SpeechSpeed, Formality, Dialect Field[string]; MemoryEnabled Field[bool] }`; `type ProfileView struct{ Profile; OverallLevel float64; OverallLevelLabel string; CompletedSessionCount int; TotalPracticeSeconds int64; LastSessionAt *time.Time }`; `type SkillView struct{ Skill Skill; EstimatedLevel float64; LevelLabel string; DemonstratedCeiling *float64; Confidence float64; ConfidenceLabel string; Trend Trend; EvidenceCount int; IsProvisional bool; LastAssessedAt *time.Time }`; `type SkillsView struct{ Skills []SkillView; FocusSkills []Skill; VoiceExposure VoiceExposureView; AlgorithmVersion int }`; `type VoiceExposureView struct{ WindowDays int; Items []ExposureItem; ExposureSuggestion *string }`; method: `EnsureProfile(ctx, tx Tx, userID) error`, `GetProfile(ctx, userID) (ProfileView, error)`, `PatchProfile(ctx, userID, PatchInput) (ProfileView, error)`, `Skills(ctx, userID) (SkillsView, error)`, `History(ctx, userID, skill Skill, after *HistoryCursor, limit int) ([]EstimateRow, *HistoryCursor, error)`, `ResetSkills(ctx, userID) error`. Validasi nickname (keputusan atas konflik §5.9.7 vs §12.9.2; §5.8.1: §5.9 menang): NFKC + `TrimSpace` + tanpa kontrol/newline; `null` → NULL; string kosong setelah trim → `errs` `VALIDATION_FAILED` (`out_of_range`); 1–30 rune; mengandung `<`, `>`, `{`, `}` → `VALIDATION_FAILED` (`invalid_format`).

- [ ] **Step 1: Write the failing test** — unit: `TestPatch_EnumValidation` (`self_declared_level:"N9"` → `VALIDATION_FAILED` `invalid_format`; `learning_goal_note` 301 rune → `out_of_range`), `TestPatch_NicknameRules` (`" Budi "`→`"Budi"`; `""`→400; `null`→NULL; 31 rune→400; `"a{b"`→400; `"ｈａｎａ"`→NFKC `"hana"`), `TestPatch_OnboardingResetsPriorsWhenNoEvidence` (`N4` pada profil baru → 7 state prior sesuai tabel, `onboarding_completed_at` terisi; dipanggil lagi `N2` sebelum evidence → prior diganti, `onboarding_completed_at` tidak berubah), `TestPatch_OnboardingAfterEvidenceKeepsStates` (satu state `evidence_count=1` → state tidak disentuh, hanya kolom profil), `TestPatch_NullDeletesNullableFields`, `TestSkills_SevenInD097Order_ProvisionalFlags` (evidence 0 → `is_provisional true`, `trend INSUFFICIENT_DATA`, `last_assessed_at null`; `evidence_count 3` + conf 0.50 → false; conf 0.29 → true), `TestSkills_ConfidenceIsDecayedEffective`, `TestSkills_FocusSkillsWithoutScenarioFocus`, `TestVoiceExposure_ShareAndSuggestion` (total sesi ≥ 5 dan satu kombinasi gender+accent > 80% detik → `TRY_OTHER_VOICE`; share 2 desimal), `TestOverallLevelWeighted` (rata-rata terbobot `max(0.05, EffectiveConfidence)`, 2 desimal), `TestResetSkills_RestoresPriorsDeletesEstimates` (`processed_events` tidak disentuh, idempotent), `TestHistory_InvalidCursor` (`INVALID_CURSOR`). Integration `TestService_ProfileLifecycle_Postgres` (alur Patch→Skills→Reset di atas Postgres).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run 'TestPatch|TestSkills|TestVoiceExposure|TestOverall|TestResetSkills|TestHistory'`; `go test -tags integration ./internal/learner/... -race -run TestService_`. Expected: FAIL.
- [ ] **Step 3: Implement** — semua operasi baca/tulis memanggil `EnsureProfile` lebih dulu dalam tx yang sama; `Reset` = SQL §12.9.5 satu transaksi.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): profile, onboarding, skills view, history and reset service"`

### Task 9: Consumer `evaluation.completed` (learner worker)

**Files:**
- Create: `internal/learner/consumer.go`
- Test: `internal/learner/consumer_test.go` (unit), `internal/learner/consumer_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: `events.EvaluationCompletedV1`, `events.Delivery`, `events.Permanent`, `events.ErrAlreadyProcessed`, `events.QueueLearner`, Task 7 `Store/Tx`, `Reader`, `UserStatus` (`UserDeleted(ctx, userID) (bool, error)` — port kecil `learner.UserGate`, implementasi postgres), Task 2–3 fungsi murni.
- Produces: `func NewConsumer(s Store, g UserGate, clk clock.Clock, ids idgen.Generator, log *slog.Logger, m *Metrics) *Consumer`; `Queue() = events.QueueLearner`; `SupportedEvents() = {"evaluation.completed": 1}`; `Timeout() = 30*time.Second`; `Handle(ctx, d events.Delivery) error`; `OnFailure(...)` (log saja). Alur §12.5.3: user `DELETED` → tx `MarkProcessed` saja (`skipped_deleted_user`); tx: `MarkProcessed` (`ErrAlreadyProcessed` → dikembalikan) → `LockUser` → `Evidence` (tidak ada → `ErrEvaluationNotVisible` retryable; `status ≠ COMPLETED` → `Permanent("INVARIANT_VIOLATION")`; `evaluations.user_id`/`sessions.user_id` ≠ `Envelope.UserID` → `Permanent("INVARIANT_VIOLATION")` + log `security_mismatch`) → `Ensure` → `NTurns < 4` → commit tanpa update → tiap skill: skor NULL / `PronunciationStatus=UNAVAILABLE` → lewati; `c_obs < 0.05` → lewati; `D_eff` dari field JSON adaptive (§12.4; field hilang → `ScenarioDifficulty`; LISTENING = `round_half_up((speech_speed+question_complexity)/2)`, INTERACTION = `round_half_up((follow_up_depth+turn_taking_complexity)/2)`); `q_eval = DimensionConfidence[dim]` (default 0.70); `q_sample = min(1, NTurns/12)` (PRONUNCIATION `min(1, utterances/8)`); jalur normal bila `LastEvidenceAt == nil || ended_at > LastEvidenceAt` (`Insert` → bila `inserted` lanjut `Step` + `Save`), selain itu `Insert` (turunan sementara) lalu `Replay` + `UpdateDerived` + `Save` + `kaiwa_learner_replays_total{skill}`.

- [ ] **Step 1: Write the failing test** — unit (fake store, event dibangun `events.NewEvaluationCompleted`): `TestLearner_NTurnsBelow4_NoUpdateButProcessed`, `TestLearner_PronunciationNullOrUnavailable_NoRow` (hanya 6 estimate), `TestLearner_InsufficientEvidenceEvaluation_NoRows`, `TestLearner_DeletedUserSkipped`, `TestLearner_EvaluationNotVisible_Retryable` (`events.Classify` → Retry), `TestLearner_StatusNotCompleted_Permanent`, `TestLearner_UserMismatch_Permanent_DLQ`, `TestLearner_DEffFallbackToScenarioDifficulty`, `TestLearner_DEffListeningAndInteractionAveraging` (`round_half_up((2+3)/2)=3`). Integration (Postgres + seed evaluations/sesi lewat `testutil.MustInsert`): `TestLearner_EvaluationCompleted_ChangesSkillState` (gerbang: onboarding `N4`, evaluasi golden sesi 1 → `learner_skill_states(GRAMMAR)` `estimated_level 2.16`, `confidence 0.280`, `evidence_count 1`, `last_evaluation_id` terisi; 1 baris `proficiency_estimates` per skill ber-skor), `TestLearner_GoldenThreeSessions_ViaConsumer` (sesi 1–3 §12.5.4 → `2.16/0.280`, `2.27/0.389`, `2.35/0.502/IMPROVING`), `TestLearner_OutOfOrder_AllPermutations` (6 permutasi urutan pengiriman `{1,2,3}` → `learner_skill_states` dan kolom turunan `proficiency_estimates` identik dengan urutan 1,2,3; `last_evidence_at` = `ended_at` sesi 3; Review Focus 3), `TestLearner_ReplayTwice_NoChange`, `TestLearner_DuplicateEvent_SingleEffect` (event sama 2× → `evidence_count` naik sekali, 1 baris `processed_events`), `TestLearner_ConcurrentSameUser_Serialized` (dua event user sama paralel → hasil sama dengan serial).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ -race -run TestLearner_`; `go test -tags integration ./internal/learner/... -race -run TestLearner_`. Expected: FAIL.
- [ ] **Step 3: Implement** sesuai alur di atas; semua tulis dalam satu `WithinTx`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): evaluation.completed consumer with replay and idempotency"`

### Task 10: `Service.ComputeAdaptiveDifficulty`, `PromptContext`, adapter `session.AdaptiveProvider`

**Files:**
- Create: `internal/learner/adaptive_service.go`, `cmd/api/adaptive_adapter.go`
- Test: `internal/learner/adaptive_service_test.go`, `cmd/api/adaptive_adapter_test.go`, `internal/learner/adaptive_service_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: Task 5, 7, 8; `content.SessionConfigLoader.Load(ctx, content.ResolvedConfig) (content.SessionConfig, error)`; `content.EffectiveFormality`; `session.AdaptiveProvider` (M3).
- Produces: `type AdaptiveRequest struct{ UserID uuid.UUID; Config content.ResolvedConfig }`; `(*Service).ComputeAdaptiveDifficulty(ctx, AdaptiveRequest) (AdaptiveDifficulty, error)` (≤ 4 query kecil di luar tx reservasi: `EnsureProfile`, states + profil, `RecentFindings`, `VoiceExposure`/`TopicSessions90d`; memuat `SessionConfig` untuk `difficulty_level`, `evaluation_focus`, `EffectiveFormality`, `voice_profiles.accent`; `Levels = EffectiveLevel`, `Confidences = EffectiveConfidence`; `Focus = FocusSkills(...)` dengan `ScenarioFocus`); `(*Service).PromptContext(ctx, userID uuid.UUID, ad AdaptiveDifficulty) (prompt.LearnerContext, error)` (baca `ProfileSnapshot`, panggil `NewPromptContext`); method `(*Service).Rerender(prev prompt.LearnerContext, ad AdaptiveDifficulty) prompt.LearnerContext` (= `Rerender` Task 6) sehingga `*Service` memenuhi `conversation.LearnerContextProvider` M5 (asersi `var _` di `cmd/realtime`, Task 20; `learner` tidak boleh mengimpor `conversation`); di `cmd/api`: `type adaptiveAdapter struct{ svc *learner.Service }` dengan `Adaptive(ctx, session.AdaptiveRequest) (json.RawMessage, error)` (adapter berada di `cmd/api` karena `learner` tidak boleh mengimpor `session`; error → dikembalikan apa adanya agar `session.Service.Create` memetakan ke `503 SERVICE_UNAVAILABLE`, tanpa fallback default).

- [ ] **Step 1: Write the failing test** — unit (fake store + fake `SessionConfigLoader`): `TestComputeAdaptive_NewUserUsesPriors` (profil baru tanpa onboarding: semua level 1.00, `B=3` → vocab `Stretch(1.0)=clamp(R(1.3)=1, 1, 4)=1`), `TestComputeAdaptive_UsesPreferencesAndScenarioFocus` (`SLOWER` → speed−1; focus scenario `[FLUENCY]` menambah 0.2), `TestComputeAdaptive_DialectUsesVoiceAccent` (accent KANSAI + `ALLOW_DIALECT` + `Ld≥2` → `LIGHT`), `TestComputeAdaptive_DBErrorPropagates`, `TestPromptContext_ReadsNicknameOnly`, `TestAdapter_MarshalsAdaptiveJSON` (keluaran `JSONEq` ke `AdaptiveDifficulty` ber-`algorithm_version 1`). Integration: `TestComputeAdaptive_ReflectsSkillStateAfterEvaluation` (setelah Task 9 menurunkan GRAMMAR → `grammar_complexity` sesi baru ≤ sebelumnya; `scaffolding` naik) dan `TestComputeAdaptive_Under20msP95OnFixture` (benchmark ringan: 50 panggilan, p95 < 100 ms pada CI — target 20 ms hanya dicatat sebagai metrik, bukan gagal).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/ ./cmd/api/ -race -run 'TestComputeAdaptive|TestPromptContext|TestAdapter'`; `go test -tags integration ./internal/learner/... -race -run TestComputeAdaptive`. Expected: FAIL.
- [ ] **Step 3: Implement** — `ComputeAdaptiveDifficulty` method memakai fungsi murni Task 5 (nama method = nama fungsi paket; Go mengizinkan).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(learner): adaptive difficulty service, prompt context and session adapter"`

### Task 11: Memory — normalisasi key, registry, filter sensitif

**Files:**
- Create: `internal/memory/registry.go`, `internal/memory/sensitive.go`
- Test: `internal/memory/registry_test.go`, `internal/memory/sensitive_test.go`

**Interfaces:**
- Produces: `func NormalizeKey(k string) string` (lowercase; spasi/`-`/`.` → `_` kecuali setelah `:` terakhir boleh `-`; buang karakter lain); `func KeyAllowed(c Category, key string) bool` (single-slot persis atau prefix multi-slot + slug `[a-z0-9_-]{1,40}`; regex DB `^[a-z0-9_]+(:[a-z0-9_-]{0,2})…` dan ≤ 100 karakter; `EPISODIC` hanya `episode:{uuid}` server-side); `type SensitiveHit struct{ Reason string }`; `func CheckSensitive(c Category, key, contentJA, displayID string) (reason string, hit bool)` (NFKC + case-insensitive substring per kategori §11.5.4 — 9 kategori + regex email/telepon/≥6 digit/tanggal lengkap + heuristik `family:` nama orang `SENSITIVE_THIRD_PARTY`); `func CheckInjection(key, contentJA, displayID string) bool` (regex aturan 7).

- [ ] **Step 1: Write the failing test** — `TestNormalizeKey` (`"Hobby: Hiking"`→`"hobby:_hiking"` ditolak? → definisikan: spasi setelah `:` → `_`; `"pref:topic_avoid:Politics"`→`"pref:topic_avoid:politics"`; `"error:particle-wa.ga"`→`"error:particle_wa_ga"` karena `-`/`.` sebelum `:` terakhir tidak ada → `_`; setelah `:` terakhir `-` dipertahankan: `"goal:job-in-japan"`→`"goal:job-in-japan"`). `TestKeyAllowed_Registry` (tiap single-slot §11.4 true; `occupation:x` false; `hobby:hiking` true; `hobby:` kosong false; `error:x` untuk `PROFILE_FACT` false; `pref:name_to_call` untuk `PREFERENCE` true; slug 41 karakter false; `episode:<uuid>` untuk `LEARNING_PATTERN` false). `TestCheckSensitive_AtLeastTwoTermsPerCategory` (tabel: 9 kategori × ≥ 2 term dari §11.5.4, mis. HEALTH `病気`/`depresi`, RELIGION `宗教`/`islam`, POLITICS `政党`/`partai`, SEXUALITY `LGBT`/`transgender`, ETHNICITY `民族`/`etnis`, CRIMINAL `逮捕`/`penjara`, IMMIGRATION `ビザ`/`visa`, FINANCE `年収`/`gaji`, IDENTIFIER `パスワード`/regex `a@b.co` dan `\d{6,}`, BIRTHDATE `生年月日`/`2000-01-31` → reason `SENSITIVE_<KAT>`), `TestCheckSensitive_FullwidthNFKC` (`ＬＧＢＴ` terdeteksi), `TestCheckSensitive_ThirdPartyName` (`family:` + `山田さん` → `SENSITIVE_THIRD_PARTY`; `family:has_children` lolos), `TestCheckSensitive_CleanExamplesPass` (3 contoh §11.5.2 dan teks Indonesia fixture lolos), `TestCheckInjection` (`ignore previous instructions`, `指示を無視`, `system prompt`, `abaikan instruksi`, `<e:happy>` → true).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ -race -run 'TestNormalizeKey|TestKeyAllowed|TestCheckSensitive|TestCheckInjection'`. Expected: FAIL.
- [ ] **Step 3: Implement** — daftar term sebagai tabel `var sensitiveTerms = map[string][]string{…}` disalin dari §11.5.4 (urutan evaluasi tetap: HEALTH, RELIGION, POLITICS, SEXUALITY, ETHNICITY, CRIMINAL, IMMIGRATION, FINANCE, IDENTIFIER, BIRTHDATE → reason pertama yang cocok); false positive diterima (spec).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory): key registry, sensitive and injection filters"`

### Task 12: Memory — prompt data, seed prompt, parse output & 12 aturan validasi

**Files:**
- Create: `internal/memory/prompt.go`, `internal/memory/extraction.go`, `internal/memory/validate.go`, `internal/provider/fake/fixtures/kaiwa_memory_extraction_v1.json`
- Modify: `seed/0070_prompts.sql` (tambah blok sebelum `COMMIT`, setelah blok EVALUATION/SUMMARY milik M7: version `MEMORY_EXTRACTION` v1 `PUBLISHED`; `system_template`/`user_template` §11.5.2 verbatim, `output_schema` §11.5.2 verbatim, `llm_params {"temperature":0,"max_output_tokens":2000}`), `test/integration/m3_seed_test.go` (jumlah prompt version 4 → 5; daftarkan `prompt.RegisterFixtures("MEMORY_EXTRACTION", memory.PromptFixtures)` di setup test)
- Test: `internal/memory/prompt_test.go`, `internal/memory/validate_test.go`, `internal/memory/seed_test.go`

**Interfaces:**
- Produces: `MemoryExtractionPromptData{SessionDate, ScenarioTitle string; ExistingMemories []ExistingMemoryLine; Transcript string}`, `ExistingMemoryLine{Category, Key, Content string}` (§11.5.2); `func BuildTranscript(turns []TurnRow, maxRunes int) (text string, validSeqs map[int]uuid.UUID, userSeqs int)` (format `[seq=N] USER:`/`AI:` per §11.5.1; `<`→`＜`, `>`→`＞`; buang turn tertua sampai ≤ 40.000 rune; AI = `assistant_spoken_text` untuk `INTERRUPTED`); `type TurnRow struct{ ID uuid.UUID; Seq int; Status string; UserText *string; AssistantText, AssistantSpokenText string }`; `func PromptFixtures() []any` (minimal: tanpa existing, 3 turn; maksimal: 150 existing + transcript 40.000 rune) dan `func ExampleOutput() []byte` (contoh §11.5.2); `type ExtractionOutput struct{ Memories []RawCandidate; Episode *RawEpisode }` (+ tag JSON schema); `func ParseOutput(raw []byte, schema json.RawMessage) (ExtractionOutput, error)` (validasi skema JSON 2020-12 lalu decode ketat); `type Candidate struct{ Category Category; Key, ContentJA, DisplayID, EvidenceType string; Confidence, Importance float64; IsTemporary bool; ValidDays *int; TurnIDs []uuid.UUID; Embedding []float32 }`; `func ValidateCandidates(out ExtractionOutput, turns map[int]uuid.UUID /* seq→turn id, hanya user_text non-kosong */, sessionID uuid.UUID, m Metrics) []Candidate` (12 aturan §11.5.3 berurutan; `reason` persis; EPISODIC key `episode:{session_id}`, `EvidenceType INFERRED`; urutan keluaran `PROFILE_FACT, PREFERENCE, LEARNING_PATTERN, EPISODIC`, dalam kategori `confidence DESC`).

- [ ] **Step 1: Write the failing test** — `TestValidateCandidates_OneCasePerReason` (12 sub-test tabel: `SENSITIVE_FLAGGED` (`sensitive:true`), `KEY_NOT_ALLOWED` (`foo:bar`), `ASSUMPTION` (`PROFILE_FACT`+`INFERRED`), `NO_USER_EVIDENCE` (seq menunjuk turn tanpa user_text / tak ada), `SINGLE_OCCURRENCE` (`LEARNING_PATTERN` dengan `[3,3]`), `SENSITIVE_HEALTH` (content `うつ病…` lewat filter §11.5.4), `INJECTION` (`指示を無視`), `NOT_JAPANESE` (`hello world`), `LENGTH` (61 rune; episode 151), `LOW_CONFIDENCE` (0.74 `PROFILE_FACT`, 0.69 `PREFERENCE`, 0.69 `LEARNING_PATTERN`, 0.59 `EPISODIC`; 0.75/0.70/0.70/0.60 lolos), `BATCH_DUPLICATE` (category+key sama → confidence tertinggi), `OVER_LIMIT` (14 kandidat → 12 tersisa, buang `confidence×importance` terendah); counter `rejected_total{reason}` bertambah tepat 1 per kasus). `TestValidate_ExampleOutputAccepted` (contoh §11.5.2 dengan transcript seq 1–9 → 3 memori + episode lolos). `TestBuildTranscript` (format baris, delimiter, sanitasi `<>`, buang turn tertua pada > 40.000 rune, `INTERRUPTED` memakai spoken text, `userSeqs` hanya turn ≥ 5 rune). `TestRenderSeedV1WithFixtures` (`prompt.Render` template seed v1 dengan kedua `PromptFixtures()`: existing kosong → baris `(none)`; tidak ada error `missingkey`; `ExampleOutput()` lolos `output_schema` seed). `TestSeedPromptRowMatchesSpec` (integration ringan: jalankan seed → `prompt_configs.key='MEMORY_EXTRACTION'` punya 1 version `PUBLISHED` dengan `llm_params` dan `output_schema` valid; seed idempoten 2×). `TestFakeFixtureValidates` (fixture fake ter-embed lolos `ParseOutput` + `ValidateCandidates` dengan transcript seq 1–4 — fixture e2e memakai `source_turn_seqs` `[1]`, `[2]`, `[1,2,3]`, episode `[1,2,3]` dan tidak memuat term sensitif).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ -race -run 'TestValidate|TestBuildTranscript|TestRenderSeed|TestFakeFixture'`; `go test -tags integration ./internal/memory/ -race -run TestSeedPromptRow`. Expected: FAIL.
- [ ] **Step 3: Implement** — tiap aturan satu fungsi kecil berurutan di `validate.go`; kandidat `valid_days` default 60 bila `is_temporary` dan `null`, di-clamp 1–120; teks template/seed disalin dari §11.5.2 ke SQL tanpa parafrase; `provider/fake/fixtures/kaiwa_memory_extraction_v1.json` = contoh §11.5.2 dengan `source_turn_seqs` di atas.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory): extraction prompt, schema parsing and 12 validation rules"`

### Task 13: Memory — `ApplyCandidate` (reinforce/supersede/insert), TTL, cap, tombstone

**Files:**
- Create: `internal/memory/apply.go`, `internal/memory/repository.go`, `internal/testutil/fakes/memory_store.go`
- Test: `internal/memory/apply_test.go`

**Interfaces:**
- Produces: port `Repository` (semua method menerima `userID`; dipakai dalam `Tx`): `TombstoneActive(ctx, userID, cat Category, key string, now time.Time) (bool, error)`, `GetActiveSlotForUpdate(ctx, userID, cat, key) (*Memory, error)`, `FindNearestActive(ctx, userID, cat, qvec []float32) (*Nearest, error)` (`Nearest{ID uuid.UUID; Key string; Similarity float64}`), `Insert(ctx, userID, Memory) error`, `Reinforce(ctx, userID, id uuid.UUID, ReinforceInput) error`, `Supersede(ctx, userID, oldID, newID uuid.UUID, expiresAt, now time.Time) error`, `CountActive(ctx, userID, group CapGroup) (int, error)`, `DeleteLowest(ctx, userID, group CapGroup, n int) error`, `ListCore(ctx, userID, now) ([]CoreRow, error)`, `SearchEpisodic(ctx, userID, qvec, minSim float64, k int, exclude *uuid.UUID, now) ([]EpisodicRow, error)`, `List(ctx, userID, cat *Category, after *ListCursor, limit int, now) ([]Memory, error)`, `Delete(ctx, userID, id uuid.UUID, now) (bool, error)` (rantai supersede + baris + tombstone untuk non-episodik; `false` → tidak ditemukan/bukan ACTIVE), `DeleteAll(ctx, userID) error`, `LockUser(ctx, userID) error` (`pg_advisory_xact_lock(hashtextextended('memory:'||$1,0))`); `Store{WithinTx(ctx, func(ctx, Tx) error) error; Reader() Reader}`, `Tx{Memories() Repository; Processed() events.ProcessedEvents; Outbox() events.OutboxWriter}`; `type Applier struct{…}`; `func NewApplier(ids idgen.Generator, clk clock.Clock, m Metrics) *Applier`; `(*Applier).Apply(ctx, tx Tx, userID uuid.UUID, c Candidate) (Action, error)` (`ActionCreated|Reinforced|Superseded|Rejected`; urutan §11.6: tombstone → slot → semantik lintas key → insert); `func TTLFor(c Candidate, now time.Time) time.Time`; `(*Applier).EnforceCaps(ctx, tx, userID) error`; `fakes.NewMemoryStore()` (cosine nyata, `ux_memories_active_slot` ditegakkan).

- [ ] **Step 1: Write the failing test** (embedding vektor eksplisit, bukan hash)
```go
func TestApply_SameSlotSim095_Reinforces(t *testing.T) { // memory: obs=1, conf 0.90; kandidat conf 0.95 vec cos 0.95
	// → ActionReinforced; obs 2; confidence = min(0.99, max(0.90,0.95)+0.05) = 0.99 (LEAST 0.990); content & embedding lama tak berubah;
	// expires_at = max(lama, now+730d); source_session_id/turn_ids diganti
}
func TestApply_SameSlotSim05_Supersedes(t *testing.T) { /* lama status SUPERSEDED, superseded_by=baru, expires_at=now+30d; baru ACTIVE obs=1 first_observed_at=now; UPDATE terjadi sebelum INSERT (fake mencatat urutan) */ }
func TestApply_CrossKeySim092_ReinforcesOldKey(t *testing.T) { /* key beda, kategori sama, sim 0.92 → Reinforce baris lama, key lama dipertahankan */ }
func TestApply_CrossKeyEpisodicSim092_Supersedes(t *testing.T) {}
func TestApply_CrossKeyBelow090_Inserts(t *testing.T) {}
func TestApply_ActiveTombstone_Rejected_USER_DELETED_SLOT(t *testing.T) { /* tombstone expires_at > now → reject; tombstone kedaluwarsa (now > expires_at) → boleh insert */ }
func TestApply_OldEmbeddingNil_TreatsSimAsZero_Supersedes(t *testing.T) {}
func TestTTLFor(t *testing.T) { /* PROFILE_FACT now+730d; temporer valid_days 45 → now+45d, nil → 60; PREFERENCE 365; LEARNING_PATTERN 180; EPISODIC 180 */ }
func TestEnforceCaps(t *testing.T) { /* 101 PROFILE_FACT+PREFERENCE → hapus 1 dengan 0.6*imp+0.4*conf terendah (tie: last_observed_at terlama); 41 LEARNING_PATTERN; 201 EPISODIC → hapus last_observed_at terlama */ }
func TestApply_ProcessOrderAndMetrics(t *testing.T) { /* urutan kategori PROFILE_FACT,PREFERENCE,LEARNING_PATTERN,EPISODIC; kaiwa_memory_apply_total{action} */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ -race -run 'TestApply|TestTTLFor|TestEnforceCaps'`. Expected: FAIL.
- [ ] **Step 3: Implement** pseudokode §11.6 verbatim; `similarity = 1 − cosine distance`; id baru dari `idgen.Generator`; waktu dari `clock.Clock`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory): dedup, supersede and reinforce with retention TTLs and caps"`

### Task 14: Query & repository memory (pgvector exact scan) + tes isolasi §11.10

**Files:**
- Create: `db/queries/memories.sql`, `internal/postgres/memory_repo.go`
- Modify: `internal/postgres/store.go` (`MemoryStore()`), `sqlc.yaml` (override `vector` → `github.com/pgvector/pgvector-go.Vector` bila belum ada), `go.mod` (`pgvector-go`)
- Test: `internal/memory/queries_test.go`, `internal/postgres/memory_repo_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: Task 13 port `Repository`/`Store`/`Tx`.
- Produces: `postgres.NewMemoryRepo`, `(*Store).MemoryStore() memory.Store`; query sqlc (§11.7.4 verbatim kecuali satu keputusan: `now()` pada `ListCoreCandidates`/`SearchEpisodic` menjadi parameter `@now` dari `clock.Clock` agar test deterministik — catat di deviations): `ListCoreCandidates`, `SearchEpisodic` (CTE `MATERIALIZED`), `FindNearestActive` (CTE `MATERIALIZED`), `GetActiveSlotForUpdate`, plus `InsertMemory`, `ReinforceMemory`, `SupersedeMemory`, `HasActiveTombstone`, `CountActiveByGroup`, `DeleteLowestInGroup`, `ListMemories`, `GetActiveForUpdate`, `DeleteSupersedeChain` (rekursif §11.9), `DeleteMemoryByID`, `UpsertTombstone` (`ON CONFLICT … WHERE status='DELETED' DO UPDATE`), `DeleteAllMemories`, `ListExistingForPrompt`. `SysDeleteExpiredMemories` milik M6 tidak disentuh.

- [ ] **Step 1: Write the failing test** — unit `TestMemoryQueriesAreUserScoped` (parse `memories.sql`: tiap blok `-- name:` yang tak berawalan `Sys` memuat `user_id = `). Integration (`pgvector/pgvector:pg17`): `TestSearchEpisodic_Isolation` (A & B memori EPISODIC embedding **identik**; search sebagai A hanya milik A; sebagai C kosong; Review Focus 1), `TestFindNearestActive_Isolation` (kandidat user A tak pernah mereinforce/supersede memory B walau embedding identik — dijalankan lewat `Applier` + repo nyata), `TestSearchEpisodic_ThresholdAndExclude` (sim 0.74 tidak muncul, 0.75 muncul; `exclude` sesi sendiri tidak muncul; `k=3`; urut similarity), `TestChkMemoriesContent_RejectsTombstoneWithContent` (`23514` `chk_memories_content`), `TestActiveSlotUnique` (`23505` `ux_memories_active_slot`), `TestSupersedeDeferredFK_UpdateBeforeInsertInOneTx` (valid saat COMMIT), `TestDeleteRepo_ChainAndTombstone` (Delete: baris + rantai `superseded_by` terhapus; tombstone dibuat untuk `PROFILE_FACT`, tidak untuk `EPISODIC`; tombstone kedua pada slot sama → upsert `expires_at` +90 hari, 1 baris), `TestDeleteAll_KeepsOtherUsers`, `TestListMemories_CursorOrder` (urut `category, last_observed_at DESC, id`; cursor tuple `(category > c) OR (category = c AND (last_observed_at < t OR (last_observed_at = t AND id > i)))`; tanpa tombstone/SUPERSEDED/kedaluwarsa), `TestVectorRoundTrip1536`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ -race -run Queries`; `go test -tags integration ./internal/postgres/... -race -run 'TestSearchEpisodic|TestFindNearest|TestChkMemories|TestActiveSlot|TestSupersedeDeferred|TestDeleteRepo|TestDeleteAll|TestListMemories|TestVectorRoundTrip'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `make sqlc`; tanpa index HNSW (§11.3.1); embedding `NULL` untuk tombstone.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `make sqlc-check` bersih.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory,postgres): user-scoped memory queries with exact vector scan"`

### Task 15: Consumer ekstraksi memory (`session.completed`)

**Files:**
- Create: `internal/memory/consumer.go`
- Test: `internal/memory/consumer_test.go` (unit), `internal/memory/consumer_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: `events.SessionCompletedPayload`/`events.QueueMemory`/`Permanent`/`NewMemoryExtracted`/`MemoryExtractedV1` (M6), `provider.AsyncRouter` (subset di bawah), `prompt.Render`, Task 11–13.
- Produces: port sisi pemakai: `type Provider interface{ GenerateJSON(ctx, provider.StructuredRequest) (provider.StructuredResult, error); Embed(ctx, provider.EmbeddingRequest) (provider.EmbeddingResult, error) }` (dipenuhi `provider.AsyncRouter`); `type Source interface{ UserDeleted(ctx, userID uuid.UUID) (bool, error); Session(ctx, sessionID uuid.UUID) (SessionInfo{UserID uuid.UUID; EndedAt time.Time; ScenarioName string}, error); Turns(ctx, sessionID uuid.UUID) ([]TurnRow, error); Existing(ctx, userID uuid.UUID, limit int) ([]ExistingMemoryLine, error)}`; `type ProfileSettings interface{ EnsureMemoryEnabled(ctx, userID uuid.UUID) (bool, error) }` (postgres: SQL `EnsureProfile` §12.3); `type PromptSource interface{ Published(ctx, key string) (prompt.Template, uuid.UUID, error) }`; `type MemVer interface{ Bump(ctx, userIDs []uuid.UUID) error }` (= `redisstore.MemVerBumper` M6); `func NewConsumer(d ConsumerDeps) *Consumer` (`Queue() = events.QueueMemory`, `SupportedEvents() = {"session.completed": 1}`, `Timeout() = 120*time.Second`, `Handle`, `OnFailure`). `Handle` = alur §11.5.1 langkah 1–13: user `DELETED` → tx `MarkProcessed` saja; `memory_enabled=false` → `MarkProcessed`; `Session.UserID ≠ Envelope.UserID` → `Permanent("INVARIANT_VIOLATION")` + log `security_mismatch` (sesi tak ada → `Permanent("ENTITY_NOT_FOUND")`); turn user ≥ 5 rune < 3 → `MarkProcessed`; prompt tidak ada / `user_template`/`output_schema` NULL → `Permanent("CONFIGURATION_ERROR")`; `GenerateJSON` (`Task "MEMORY_EXTRACTION"`, `SchemaName fmt.Sprintf("kaiwa_memory_extraction_v%d", Version)`, `Temperature`/`MaxOutputTokens`/`Seed` dari `Params` (default 0 / 2000 / nil), `RequestID "MEMORY_EXTRACTION:{session_id}"`; pesan = `system` + `user`); output tak valid → 1× re-ask (assistant: output lama; user: `"The previous output was invalid: <error>. Return corrected JSON only."`), tetap gagal → `Permanent("INVALID_MODEL_OUTPUT")`; validasi → embed batch (`Dimensions 1536`, timeout 5 s; gagal → error retryable apa adanya) → tx: `LockUser`, `MarkProcessed`, `Apply` per kandidat, `EnforceCaps`, `Outbox().Enqueue(events.NewMemoryExtracted(...))` (juga bila tidak ada kandidat; `created/updated/superseded_memory_ids`, `category_counts`, `provider`/`model` UPPERCASE dari `StructuredResult`) → di luar tx `MemVer.Bump([userID])`.

- [ ] **Step 1: Write the failing test** — unit (stub `Provider`, fake store): `TestConsumer_SkipsWhenFewerThan3UserTurns`, `TestConsumer_MemoryDisabled_MarksProcessedOnly`, `TestConsumer_DeletedUser_NoProviderCall`, `TestConsumer_UserMismatch_PermanentAndNoWrite`, `TestConsumer_MissingPromptConfig_Permanent`, `TestConsumer_InvalidJSONThenRepair_Succeeds` (2 panggilan; panggilan ke-2 berisi pesan repair), `TestConsumer_InvalidTwice_PermanentDLQ`, `TestConsumer_EmbedFailure_Retryable` (`events.Classify`→Retry; tidak ada tulisan), `TestConsumer_ProviderErrorClassification` (non-retryable `provider.Error` → Dead), `TestConsumer_RequestShape` (Task/SchemaName/Temperature 0/MaxOutputTokens 2000/RequestID persis), `TestConsumer_PromptContainsNoFullName`, `TestConsumer_EmitsMemoryExtractedEvenWhenEmpty`. Integration (Postgres + provider fake M4 dibungkus tipis: `GenerateJSON` → `fake.NewEvaluation` fixture `kaiwa_memory_extraction_v1`, `Embed` → `fake.NewEmbedding`): `TestMemoryWorker_ExtractsFromTranscript` (sesi 5 turn → ≥ 3 baris `ACTIVE` + 1 episode, `source_turn_ids` terisi, `extraction_prompt_version_id`, 1 baris outbox `memory.extracted`), `TestMemoryWorker_RedeliveryDoesNotDoubleCount` (event sama 2× → `observation_count` tetap 1, `processed_events` 1 baris; Review Focus 2), `TestMemoryWorker_SecondSessionReinforces` (sesi kedua dengan fixture sama → `observation_count 2`, tidak ada duplikat slot), `TestMemoryWorker_UserMismatch` (§11.10: event `user_id` ≠ `sessions.user_id` → `Permanent`, tidak ada tulisan; lewat framework `amqp` M6 → pesan di `memory.session_completed.dlq`), `TestMemoryWorker_TombstoneBlocksReextract` (hapus via repo → worker tidak membuat ulang slot), `TestMemoryWorker_BumpsMemver`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ -race -run TestConsumer_`; `go test -tags integration ./internal/memory/... -race -run TestMemoryWorker_`. Expected: FAIL.
- [ ] **Step 3: Implement** — kerja mahal (provider, embedding) di luar transaksi; transaksi akhir singkat (pola §9.7.2); log tanpa konten memory.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory): session.completed extraction consumer"`

### Task 16: `RetrievalService` + `memctx`/`memver` Redis

**Files:**
- Create: `internal/memory/retrieval.go`, `internal/redisstore/memctx.go`
- Test: `internal/memory/retrieval_test.go` (unit), `internal/redisstore/memctx_integration_test.go`, `internal/memory/retrieval_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: Task 13 `Repository`, `prompt.EstimateTokens/ItemOverheadTokens/DefaultBudget`, `Provider.Embed`, `ProfileSettings`, `clock.Clock`.
- Produces: `type ContextCache interface{ GetMemVer(ctx, userID uuid.UUID) (int64, error) /* missing → 0 */; SetContext(ctx, sessionID uuid.UUID, mc MemoryContext, ttl time.Duration) error; GetContext(ctx, sessionID uuid.UUID) (*MemoryContext, error) /* tidak ada/rusak → nil,nil */ }` (`redisstore.NewMemCtxStore(rdb redis.Cmdable)`; key `kaiwa:session:{id}:memctx`, `kaiwa:user:{id}:memver`); `type Retriever struct{…}`; `func NewRetriever(d RetrieverDeps) *Retriever` (`var _ RetrievalService = (*Retriever)(nil)`); `const MemCtxTTL = 4500 * time.Second`; `func RenderItem(c ContextItemSource) string` (core: `content` disanitasi; `LEARNING_PATTERN` diawali `（学習の傾向）`; episodik `YYYY-MM-DD: content` zona `Asia/Jakarta`; `<`→`＜`, `>`→`＞`, newline dihapus); `func Priority(r CoreRow, rel float64, now time.Time) float64`. Perilaku: `BuildSessionContext` (deadline 2000 ms, embed 1500 ms; `memory_enabled=false` → `MemoryContext{}` kosong tanpa degraded; embed gagal → `qvec=nil`, `Degraded=true`; DB gagal → konteks kosong `Degraded=true`; tidak pernah mengembalikan error; menyimpan `qvec` di cache in-memory per sesi: map ber-mutex, maks 2048 entri (buang yang tertua), TTL 2 jam — dipakai `ReloadCoreIfChanged`; greedy core ≤ `DefaultBudget.MemoryCore` & ≤ 20 item; `ContextItem.Tokens = prompt.EstimateTokens("- " + RenderItem(...) + "\n")` persis §11.7.2 (teks item final termasuk prefix `- ` dan newline; `ItemOverheadTokens` tidak ditambahkan lagi), `CoreTokens`/`EpisodicTokens` = jumlah `Tokens`);
  `RefreshEpisodic` (embed 3 turn terakhir 600 rune terakhir, `SearchEpisodic` 0.75/3/exclude sesi, trim 500 token, tulis `memctx` TTL dipertahankan; gagal → hasil lama + `Degraded`); `ReloadCoreIfChanged` (`memver` ≠ `UserMemVer` → ulang `ListCore` + scoring dengan qvec cache, buang episodik yang tidak lagi `ACTIVE`, set `UserMemVer`, `changed=true`).

- [ ] **Step 1: Write the failing test** — unit (fake store, embedder stub): `TestPriorityFormula` (importance 0.8, obs 3, conf 0.9, 90 hari, rel 0.5 → `0.40×0.9 + 0.25×0.9 + 0.15×e^-1 + 0.20×0.5`), `TestBuildSessionContext_ThresholdAndSort` (< 0.45 dibuang; urut `priority DESC, last_observed_at DESC`), `TestBuildSessionContext_CoreBudgetNeverExceeds600` (200 item panjang → `CoreTokens ≤ 600`, ≤ 20 item), `TestBuildSessionContext_EpisodicMax3And500Tokens`, `TestContextItemTokensEqualEstimateOfRenderedLine` (`Tokens == prompt.EstimateTokens("- "+RenderItem(...)+"\n")`; `CoreTokens` = jumlah), `TestRenderItem_Sanitizes_AndLearningPatternPrefix_AndEpisodeDate`, `TestBuildSessionContext_MemoryDisabled_EmptyNotDegraded` (Review Focus 4), `TestBuildSessionContext_EmbedFailureDegrades` (core tetap dimuat, `Degraded true`, episodik kosong, `err == nil`; metrik `kaiwa_memory_retrieval_degraded_total{stage="start"}`; Review Focus 5), `TestBuildSessionContext_DBFailureDegrades`, `TestBuildSessionContext_FreeConversationEmptyScenarioText_NoEmbed`, `TestRefreshEpisodic_KeepsOldOnError`, `TestReloadCoreIfChanged_DeletedMemoryDroppedOnNextCall` (memver naik → item hilang; `changed=true`; memver sama → `changed=false`, tanpa query DB), `TestReloadCoreIfChanged_NoQvecAfterRestart_RelevanceZero`. Integration: `TestMemCtxStore_RoundTripAndTTL` (`SetContext` TTL ≈ 4500 s; `GetContext` rusak → nil), `TestRetrieval_E2EOnPostgres` (memori hasil `Applier` + `fake.NewEmbedding`: `BuildSessionContext` sesi baru memuat core + episode sesi lama dan **tidak** memuat episode sesi sendiri; `memctx` tertulis), `TestRetrieval_UserIsolation` (user B tidak melihat memori A walau embedding identik), `TestRetrieval_AfterUserDeleteAll_NextReloadEmpty` (`DeleteAll` + `Bump` → `ReloadCoreIfChanged` → `Core` kosong).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ -race -run 'TestPriority|TestBuildSession|TestContextItem|TestRenderItem|TestRefresh|TestReload'`; `go test -tags integration ./internal/redisstore/... ./internal/memory/... -race -run 'TestMemCtxStore|TestRetrieval_'`. Expected: FAIL.
- [ ] **Step 3: Implement** pseudokode §11.7.3 & §11.7.5; metrik `kaiwa_memory_retrieval_duration_seconds{stage}`, `kaiwa_memory_context_tokens{part}`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(memory): retrieval service with memctx and memver invalidation"`

### Task 17: `memory.Service` + endpoint profile & memory (`httpapi`)

**Files:**
- Create: `internal/memory/service.go`, `internal/httpapi/{profile,memory}_{routes,handler,dto}.go`, `testdata/api/rest/profile/*.json`, `testdata/api/rest/memory/*.json`
- Modify: `internal/httpapi/deps.go`, `router.go` (tambah `Profile profileService`, `Memories memoryService` pada `Deps`; aditif), `route_table_test.go` (8 baris `Ready=true`)
- Test: `internal/memory/service_test.go`, `internal/httpapi/profile_handler_test.go`, `internal/httpapi/memory_handler_test.go`, `internal/httpapi/m8_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: M2 `Handle[B,R]`, `HandleNoBody`, `Page[T]`, `NewPage`, `EncodeCursor/DecodeCursor`, `ParseLimit`, `FieldError`, `auth.PrincipalFrom`, `auth.UserService.RequireActive` (signature persis dari M2), Task 8 `learner.Service`.
- Produces: `memory.NewService(s Store, bump MemVer, clk clock.Clock, log *slog.Logger) *Service`; `(*Service).List(ctx, userID, cat *Category, after *ListCursor, limit int) (ListResult{Items []Memory; Next *ListCursor; MemoryEnabled bool}, error)`; `Delete(ctx, userID, id uuid.UUID) error` (tak ditemukan/milik user lain/bukan ACTIVE → `errs` `MEMORY_NOT_FOUND`; setelah commit `Bump`); `DeleteAll(ctx, userID) error` (idempotent, `Bump`). DTO: `profileResponse` (§5.9.7: 16 field termasuk `completed_session_count`, `total_practice_seconds`, `last_session_at`; bentuk §5.9.7 menang atas §12.9.1 — catat), `patchProfileRequest` (memakai `learner.Field[T]`; `preferences` objek bersarang; field tak dikenal → `VALIDATION_FAILED`), `skillsResponse`, `historyResponse`, `memoryListResponse{data, next_cursor, memory_enabled}` (tanpa embedding/tombstone). Route (grup User, `api_user_default`): `GET /profile` 200, `PATCH /profile` 200, `GET /profile/skills` 200, `GET /profile/skills/{skill}/history` 200, `DELETE /profile/skills` 204, `GET /profile/memories` 200, `DELETE /profile/memories` 204, `DELETE /profile/memories/{memory_id}` 204; metode tulis memanggil `RequireActive` (→ `403 ACCOUNT_SUSPENDED`); `memory_id` bukan UUID → `404 MEMORY_NOT_FOUND`; `skill` di luar enum / `limit` di luar rentang → `400 VALIDATION_FAILED`; cursor rusak → `400 INVALID_CURSOR`; `category` tak dikenal → `400 VALIDATION_FAILED`.

- [ ] **Step 1: Write the failing test** — unit service: `TestDelete_BumpsMemverAndTombstones`, `TestDeleteAll_Idempotent_NoTombstone`, `TestList_FiltersExpiredAndNonActive`. Handler (httptest + fake service): golden JSON per endpoint (`testdata/api/rest/profile/get_profile.json` memuat nilai contoh §5.9.7; field nullable selalu ada bernilai `null`; timestamp `...123Z`), `TestPatchProfile_UnknownFieldRejected`, `TestPatchProfile_NullClearsNickname`, `TestWriteEndpoints_Suspended403`, `TestMemoryIDNotUUID_404MemoryNotFound`, `TestHistory_InvalidSkill400`, `TestRouteTable` (8 baris sesuai §5.8.5). Integration (Postgres + Redis, router nyata, dua user A/B): `TestDeleteMemory_OtherUser404` (A menghapus id milik B → 404 `MEMORY_NOT_FOUND`; baris B tetap), `TestListMemories_OnlyOwn`, `TestDeleteMemory_Owner204ThenSlotTombstoned` (worker berikutnya tidak membuat ulang; Review Focus 4), `TestDeleteAllMemories_ThenListEmpty`, `TestPatchMemoryEnabledFalse_WorkerSkipsAndRetrievalEmpty`, `TestProfileEndpoints_OnlyOwnData` (A tak melihat skill/history B), `TestDeleteSkills_ResetsOnlyOwn`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/memory/ ./internal/httpapi/ -race -run 'TestDelete|TestList|TestPatchProfile|TestWriteEndpoints|TestMemoryID|TestHistory|TestRouteTable|Golden'`; `go test -tags integration ./internal/httpapi/... -race -run 'TestDeleteMemory|TestListMemories|TestDeleteAllMemories|TestPatchMemoryEnabled|TestProfileEndpoints|TestDeleteSkills'`. Expected: FAIL.
- [ ] **Step 3: Implement** — handler tipis; `GET /profile` dst. memanggil `EnsureProfile` lewat service; cursor memories `[category, last_observed_at, id]`, history `[assessed_at, id]` (`EncodeCursor`).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; golden diperiksa manual terhadap §5.9.7.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(httpapi,memory): profile, skills and memory endpoints"`

### Task 18: Retensi memory (`memory_cleanup` terisi) + verifikasi `account_purge`

**Files:**
- Create: `internal/memory/retention_test.go` (test pemakai), `test/integration/m8_retention_test.go` (`integration`)
- Modify: `internal/jobs/memory_cleanup.go` hanya bila test menunjukkan `Bump` tidak dipanggil per user terdampak (kontrak M6 sudah ada); `internal/postgres/purge_repo.go` hanya bila `DeleteMemories`/`DeleteLearner` tidak menghapus tabel M8 (kontrak M6 sudah ada)

**Interfaces:**
- Consumes: M6 `jobs.NewMemoryCleanupJob`, `redisstore.MemVerBumper`, `jobs.NewAccountPurgeJob`, `postgres.NewPurgeRepo`; Task 13 `TTLFor`, `Applier`.
- Produces: tidak ada tipe baru; test lintas-milestone yang memastikan kebijakan §11.8 hidup end-to-end.

- [ ] **Step 1: Write the failing test** — `TestRetention_TTLsEndToEnd` (kandidat tiap kategori di-`Apply` pada `T`; baris tersimpan `expires_at` = `T+730d` (permanen) / `T+45d` (temporer) / `T+365d` / `T+180d` / `T+180d`; memori `SUPERSEDED` `T+30d`; tombstone `T+90d`), `TestMemoryCleanup_DeletesExpiredAndBumpsMemver` (maju ke `T+181d` pada baris dengan `expires_at` lampau: `LEARNING_PATTERN` dan `EPISODIC` terhapus, `PROFILE_FACT` tetap; `INCR memver` tepat sekali per user terdampak; `kaiwa_memory_cleanup_deleted_total` naik), `TestRetrieval_NeverReturnsExpiredEvenBeforeCleanup` (`expires_at < now` tidak masuk `BuildSessionContext`), `TestAccountPurge_RemovesMemoriesAndLearnerRows` (user `DELETED` dengan memories, 7 state, estimates → semua terhapus; ledger/`sessions` utuh; `AssertReconciled`; user lain utuh), `TestAccountPurge_DeletedUserEventsSkipped` (memory & learner consumer untuk user `DELETED` → hanya `processed_events`).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration/... -race -run 'TestRetention_|TestMemoryCleanup_|TestAccountPurge_'`. Expected: FAIL (hanya bila ada celah kontrak; bila langsung PASS karena M6 sudah benar, tambahkan satu test negatif yang menegaskan perilaku — mis. user aktif tak tersentuh — hingga gagal pada implementasi kosong, lalu lanjut).
- [ ] **Step 3: Implement** perbaikan minimal di `jobs`/`postgres` hanya untuk kegagalan yang terbukti; catat di `docs/implementation-log.md`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test(memory,learner): retention and purge verification across milestones"`

### Task 19: Wiring `cmd/worker` — consumer memory & learner

**Files:**
- Modify: `cmd/worker/wiring.go` (`consumerDeps` aditif: `MemoryProvider memory.Provider` (diisi `Router provider.AsyncRouter` yang sudah ditambahkan M7), `PromptSource memory.PromptSource`, `MemorySource memory.Source`, `MemoryStore memory.Store`, `LearnerStore learner.Store`, `MemVer memory.MemVer`; case `memory` dan `learner` di `buildConsumers` — case `evaluation`/`summary` milik M7 tidak diubah, rebase bila konflik), `cmd/worker/main.go` (`prompt.RegisterFixtures("MEMORY_EXTRACTION", memory.PromptFixtures)`)
- Create: `internal/postgres/memory_source.go` (implementasi `memory.Source`, `memory.ProfileSettings`, `memory.PromptSource`; `learner.UserGate`)
- Test: `cmd/worker/handlers_test.go` (ubah/tambah), `internal/postgres/memory_source_integration_test.go`

**Interfaces:**
- Consumes: M6 `buildConsumers(flags []string, d consumerDeps) ([]*amqp.Consumer, error)`, `errHandlerNotImplemented`; M7 mengisi case `evaluation`/`summary` pada fungsi yang sama — gabungkan tanpa mengubah case milik M7 (rebase bila konflik).
- Produces: `buildConsumers` mengembalikan consumer `memory.session_completed` (prefetch 4) dan `learner.evaluation_completed` (prefetch 16) untuk flag `memory`/`learner`; Deployment `worker-memory` memakai `AsyncRouter` dari `provider.NewRouter(... Role: RoleWorker)`.

- [ ] **Step 1: Write the failing test** — ubah `TestBuildConsumers_UnimplementedHandlerFailsStartup` agar hanya `evaluation`/`summary` (jika M7 belum merge) yang gagal; `TestBuildConsumers_MemoryAndLearner` (flag `memory` → 1 consumer dengan `Queue()==events.QueueMemory`; `learner` → `events.QueueLearner`; `learner,summary` → 2), `TestWorker_MemoryLearner_EndToEndOnBroker` (integration: RabbitMQ + Postgres + provider fake; publish `session.completed` lewat outbox untuk sesi ber-transcript → baris `memories` muncul ≤ 15 s dan `processed_events(consumer='memory.session_completed')` 1 baris; publish `evaluation.completed` → `learner_skill_states` berubah dan `processed_events(consumer='learner.evaluation_completed')`), `TestMemorySource_Queries` (session owner, turns `COMPLETED/INTERRUPTED` urut `sequence_number`, `Existing` ≤ 150 non-episodik urut `last_observed_at DESC`, `Published('MEMORY_EXTRACTION')` mengembalikan `prompt.Template` dengan `OutputSchema`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/worker/... -race -run 'TestBuildConsumers'`; `go test -tags integration ./cmd/worker/... ./internal/postgres/... -race -run 'TestWorker_MemoryLearner|TestMemorySource'`. Expected: FAIL.
- [ ] **Step 3: Implement** wiring manual (D-004); `Timeout()`/prefetch dari tabel §9.6.2 (sudah di topologi M6).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `make arch-check` (`CGO_ENABLED=0 go build ./cmd/worker ./cmd/api`) exit 0; `golangci-lint run ./internal/memory/... ./internal/learner/...` 0 issue (depguard: tidak ada impor adapter).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(worker): register memory and learner consumers"`

### Task 20: Wiring `cmd/api` & `cmd/realtime` — ganti fake M5, context builder, in-session

**Files:**
- Modify: `cmd/api/main.go` (bangun `learner.Service`, `memory.Service`, `adaptiveAdapter` → `session.Deps.Adaptive` menggantikan fake M3; isi `httpapi.Deps.Profile/Memories`; `prompt.RegisterFixtures("MEMORY_EXTRACTION", memory.PromptFixtures)`), `cmd/realtime/providers.go` dan `main.go` (hapus `noopMemory` dan `staticLearner` M5; isi `conversation.Deps.Memory` dengan `memory.NewRetriever(...)` dan `Deps.Learner` dengan `*learner.Service` — memenuhi `PromptContext` + `Rerender`)
- Create: `test/integration/m8_wiring_test.go` (`integration`)
- Test: `cmd/realtime/wiring_test.go`, `cmd/api/wiring_test.go`

**Interfaces:**
- Consumes: Task 10 adapter, Task 16 `Retriever`, M5 `conversation.MaybeAdjust(s AdaptiveState, recentUserTexts []string) (AdaptiveState, bool)`, `conversation.PromptBuilder.BuildConversation(ConversationInput) (BuiltPrompt, error)`, `prompt.ConversationV1`, `prompt.Render` (nama dari spec §7.8.3/§7.8.6; verifikasi dengan `go doc` sebelum menulis test, signature M5 tidak diubah).
- Produces: wiring nyata; tidak ada port berubah.

- [ ] **Step 1: Write the failing test** — `TestRealtimeWiring_UsesRealMemoryAndLearner` (`var _ memory.RetrievalService = (*memory.Retriever)(nil)` dan `var _ conversation.LearnerContextProvider = (*learner.Service)(nil)` di `cmd/realtime`; tipe dinamis `deps.Memory` = `*memory.Retriever`, bukan `noopMemory`; `deps.Learner` = `*learner.Service`, bukan `staticLearner`), `TestAPIWiring_AdaptiveIsLearnerBacked` (`session.Deps.Adaptive` bertipe `*adaptiveAdapter`), `TestPublishValidatorKnowsMemoryExtractionFixtures` (`prompt.Fixtures("MEMORY_EXTRACTION")` ≥ 2 instance di `cmd/api` dan `cmd/worker`). Integration `TestPostSessions_StoresLearnerAdaptiveSnapshot` (stack `httpapi` nyata + seed konten + saldo; user onboarding `N4`, `POST /sessions` → kolom `sessions.adaptive_difficulty` JSON valid ber-`algorithm_version 1`, `scenario_difficulty` = `difficulty_level` scenario, `formality` mengikuti efektif/preferensi; DB learner error disimulasikan → `503 SERVICE_UNAVAILABLE` dan **tidak** ada sesi/reservasi). `TestAdaptiveInPrompt_NewSessionAfterEvaluation` (gerbang: evaluasi memperburuk GRAMMAR lewat consumer Task 9 → sesi baru: `adaptive_difficulty.grammar_complexity` lebih rendah/`scaffolding` lebih tinggi dari sesi sebelumnya; `learner.Service.PromptContext` → `Adaptation`; `PromptBuilder.BuildConversation` (`ConversationInput.Config` = `content.SessionConfig` hasil `SessionConfigLoader.Load` atas `testutil.SeedContent`) dengan `prompt.ConversationV1` menghasilkan system message yang memuat `<conversation_difficulty>`, baris `学習のねらい:` dan descriptor grammar level baru, serta `DisplayName` = nickname, tidak memuat `users.full_name`). `TestInSessionAdjustment_ViaContextBuilder` (§12.8.5 lewat M5: 6 turn user dengan `もう一度` pada turn ke-3 dan ke-5 dan `TurnsSinceAdjust ≥ 4` → `MaybeAdjust` mengembalikan `true`; `SpeechSpeed −1`, `QuestionComplexity −1`, `Scaffolding +1`, `InSessionAdjustments 1`; `learner.NewPromptContext` lalu `BuildConversation` mencerminkan descriptor baru; sinyal berikutnya < 4 turn → `false`; tiga kali bergantian → maksimal 2 penyesuaian; `learner.TTSSpeakingRate/MaxAISentences` dipakai nilai baru; snapshot DB tidak berubah).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/api/... ./cmd/realtime/... -race -run 'Wiring|Fixtures'` (cgo untuk realtime); `go test -tags integration ./test/integration/... -race -run 'TestPostSessions_StoresLearnerAdaptiveSnapshot|TestAdaptiveInPrompt|TestInSessionAdjustment'`. Expected: FAIL.
- [ ] **Step 3: Implement** wiring manual; hapus pembuatan fake dari wiring produksi (fake tetap di `testutil/fakes` untuk test M5).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `make test-e2e` untuk E1–E7 M5 tetap hijau (pengganti fake tidak mengubah port); `make arch-check` exit 0.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(wiring): real memory retrieval and learner adaptive replace M5 fakes"`

### Task 21: Gerbang integrasi M8 (tanpa e2e)

**Files:**
- Create: `test/integration/m8_gate_test.go` (`integration`)

**Interfaces:**
- Consumes: semua task sebelumnya; harness `testutil.NewPostgres/NewRedis` + RabbitMQ M6 (`testutil.NewRabbitMQ`).

- [ ] **Step 1: Write the failing test** — `TestM8Gate_EvaluationCompletedChangesSkillState` (event `evaluation.completed` nyata lewat outbox→RabbitMQ→`learner.Consumer`: `GET /profile/skills` (httpapi) menampilkan `estimated_level` dan `evidence_count` berubah, `trend INSUFFICIENT_DATA` sampai sesi ke-3 lalu `IMPROVING`); `TestM8Gate_MemoryExtractedThenRetrievedNextSession` (user mengobrol 4 turn → `session.completed` → worker memory → `GET /profile/memories` memuat `occupation` (`content` Jepang, `display_text` Indonesia) → `BuildSessionContext` untuk sesi baru memuat item itu dan episode sesi lama; `memctx` Redis terisi); `TestM8Gate_UserIsolationAcrossWholePipeline` (dua user, embedding & transcript identik: memories, skill states, `memctx`, endpoint list/history/delete saling tak terlihat; event dengan `user_id` palsu → DLQ); `TestM8Gate_MemoryDisabledAndDeleteTakeEffectNextTurn` (sesi aktif disimulasikan: `Retriever` + `ReloadCoreIfChanged` setelah `DELETE /profile/memories/{id}` → item hilang); `TestM8Gate_EmbeddingProviderDown_SessionStillStarts` (provider embedding fault → `BuildSessionContext` `Degraded` tanpa error dan `POST /sessions` tidak terpengaruh).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration/... -race -run TestM8Gate`. Expected: FAIL sebelum wiring lengkap; setelah Task 1–20 harus mulai PASS — bila ada yang tidak, perbaiki di task pemiliknya (bukan di file gerbang).
- [ ] **Step 3: Implement** — hanya harness test (tanpa kode produksi baru).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test(m8): integration gate for memory and learner pipeline"`

### Task 22: E2E — dua sesi berturut-turut dengan provider fake

**Files:**
- Create: `test/e2e/m8_memory_learner_test.go` (`//go:build e2e && m5`, konvensi harness M5/M6)
- Modify: `test/e2e/main_test.go` (`TestMain` M5: tambah proses `worker` kedua dengan `WORKER_HANDLERS=evaluation,memory,learner,summary` bila M7 belum menambahkannya; env eksplisit sama dengan worker M6, `PROVIDER_MODE=fake`)

**Interfaces:**
- Consumes: harness e2e M5 (binary `api`/`realtime` sebagai subprocess, `kaiwaclient`, fake provider), binary `worker` (`--handlers=outbox,evaluation,memory,learner,summary,sweeper,cleanup`; M7 menyediakan evaluasi), endpoint admin `GET /admin/sessions/{id}` (M3) untuk `adaptive_difficulty`.

- [ ] **Step 1: Write the failing test** — `TestE2E_MemoryExtractedAndRetrievedOnNextSession`: user A register + `PATCH /profile {"nickname":"Budi","self_declared_level":"N4"}`; sesi 1 `USER_FIRST` 300 s dengan ≥ 4 ucapan user (≥ 5 rune; fixture audio M5) lalu `session.end` → tunggu ≤ 30 s: `GET /sessions/{id}/evaluation` `COMPLETED`, `GET /profile/memories` memuat ≥ 1 item (`occupation`), `GET /profile/skills` `GRAMMAR.evidence_count == 1`, `processed_events` memuat consumer `memory.session_completed` dan `learner.evaluation_completed`; sesi 2: `POST /sessions` → `GET /admin/sessions/{id}` `adaptive_difficulty.algorithm_version == 1` dan `scenario_difficulty` terisi; connect + `session.start` → Redis `kaiwa:session:{id2}:memctx` memuat item `occupation` dan episode sesi 1 (`Core` ≥ 1, `Episodic` ≥ 1); sesi 2 selesai normal (saldo & `AssertReconciled` utuh). `TestE2E_MemoryIsolationBetweenUsers`: user B (tanpa sesi sebelumnya) memulai sesi → `memctx` `Core` & `Episodic` kosong; `GET /profile/memories` B kosong. `TestE2E_DeleteMemoryTakesEffect`: A `DELETE /profile/memories/{id}` → `memver` naik; sesi 3 tidak memuat item itu dan worker setelah sesi 3 tidak membuat ulang slotnya (tombstone). `TestE2E_PipelineWithin30s` (E8 §14.5: evaluasi COMPLETED, memory terekstrak, learner terupdate dalam ≤ 30 s).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags 'e2e m5' ./test/e2e/... -race -run 'TestE2E_Memory|TestE2E_Delete|TestE2E_Pipeline' -timeout 10m`. Expected: FAIL.
- [ ] **Step 3: Implement** — hanya harness; fixture fake `kaiwa_memory_extraction_v1.json` (Task 12) menjamin deterministik; bila turn user < 4 karena VAD fixture, perpanjang fixture ucapan (bukan nilai ambang).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS; `make test-e2e` seluruhnya hijau.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test(e2e): memory extraction and retrieval across consecutive sessions"`

---

## Gerbang milestone M8

Jalankan berurutan; catat perintah + ringkasan keluaran di `docs/implementation-log.md`.

- [ ] `make lint` → 0 issue (depguard: `memory`/`learner` tidak mengimpor adapter; `forbidigo`: tanpa `time.Now`/`uuid.New*`).
- [ ] `go test ./internal/learner/... ./internal/memory/... ./internal/prompt/... -race` → PASS (golden proficiency `2.16/0.280`, `2.27/0.389`, `2.35/0.502/IMPROVING`; 6 permutasi replay identik; `ComputeAdaptiveDifficulty` JSON contoh §12.8.3 persis; 12 aturan validasi satu test per `reason`; 9 kategori sensitif ≥ 2 term; `ApplyCandidate` REINFORCE/SUPERSEDE/lintas-key/tombstone; cakupan `internal/memory` & `internal/learner` memenuhi `test/tools/covercheck` (§14.8)).
- [ ] `make test-integration` → PASS, termasuk `TestMemoryQueriesAreUserScoped`, `TestLearnerQueriesAreUserScoped`, `TestSearchEpisodic_Isolation`, `TestFindNearestActive_Isolation`, `TestDeleteMemory_OtherUser404`, `TestListMemories_OnlyOwn`, `TestMemoryWorker_UserMismatch`, `TestLearner_EvaluationCompleted_ChangesSkillState`, `TestLearner_OutOfOrder_AllPermutations`, `TestPostSessions_StoresLearnerAdaptiveSnapshot`, `TestAdaptiveInPrompt_NewSessionAfterEvaluation`, `TestM8Gate_*` (gerbang: evaluation.completed → skill state berubah; adaptive difficulty tampil di prompt sesi baru; isolasi antar user terbukti).
- [ ] `make test-e2e` → PASS, termasuk `TestE2E_MemoryExtractedAndRetrievedOnNextSession` (gerbang: memory diekstrak & di-retrieve pada sesi berikutnya dengan fake provider) dan E1–E8 M5/M7 tetap hijau setelah fake diganti implementasi nyata.
- [ ] `make arch-check` → exit 0 (`CGO_ENABLED=0` build `cmd/api`, `cmd/worker`); `make sqlc-check` bersih; `go run ./test/tools/migcheck -dir migrations` tidak berubah (M8 tidak menambah migration).
- [ ] `grep -rn 'full_name' internal/memory internal/learner internal/prompt` → tidak ada pemakaian nilai `users.full_name` ke prompt (hanya komentar/test negatif).

## Self-Review

**Cakupan spec.** Bab 11: §11.1–§11.3 (DDL milik M1; query & exact scan Task 14), §11.4 (Task 11), §11.5.1–§11.5.4 (Task 12, 15), §11.6 (Task 13), §11.7 (Task 16), §11.8 (Task 13 TTL/cap, Task 18 cleanup M6), §11.9 (Task 14, 17), §11.10 (Task 14, 15, 17, 21), §11.11 (Task 11–16), §11.12 (metrik di Task 13/15/16, nama persis). Bab 12: §12.2–§12.3 (Task 7, 8), §12.4 (Task 9), §12.5.1–§12.5.5 (Task 2, 3, 9), §12.6–§12.7 (Task 4), §12.8.1–§12.8.6 (Task 5, 6, 8, 10, 20), §12.9 (Task 8, 17), §12.10 (Task 7–9, 18). §5.9.7 (Task 17), §7.8.6 verifikasi lewat builder M5 (Task 20), §9.9.9 (Task 18). **Tidak dicakup (milik lain):** DDL/migration (M1), job `memory_cleanup`/`account_purge` kode (M6; M8 hanya memverifikasi), `MaybeAdjust` dan `PromptBuilder` (M5; M8 memverifikasi), worker evaluasi dan event `evaluation.completed` (M7), adapter embedding vendor nyata (M9), manifest/KEDA worker memory/learner (M10).

**Konsistensi tipe.** `Skill/Trend/AdaptiveDifficulty` (Task 1) dipakai Task 2–10; `SkillState/Estimate/Applied` (Task 2) → `Replay` (Task 3) → consumer (Task 9); `Repository/Store/Tx` memory (Task 13) diimplementasikan Task 14 dan fake Task 13; `Provider/Source/ProfileSettings/PromptSource/MemVer` (Task 15) diimplementasikan Task 19; `Retriever` (Task 16) dipasang Task 20; `Field[T]` (Task 8) dipakai DTO Task 17. Nama berlabel "(nama dari plan)" tidak ada di spec.

**Ambiguitas spec & keputusan (catat di `docs/implementation-deviations.md`).** (1) Nickname kosong: §5.9.7 → `400`, §12.9.2 → NULL; diikuti §5.9.7 (§5.8.1: §5.9 menang), plus penolakan `<>{}` dari §12.9.2. (2) Bentuk `GET /profile` mengikuti §5.9.7 (ada `completed_session_count`, `total_practice_seconds`, `last_session_at`). (3) `L_obs`/`c_obs` dibulatkan (2/3 desimal) sebelum dipakai agar jalur normal ≡ replay dari kolom tersimpan; angka golden tidak berubah. (4) Klaim anggaran learner "±654 rune ≈ 630 token" bertentangan dengan estimator 1,1 token/rune (≈ 720); test + fallback didefinisikan di Task 6. (5) `ComputeAdaptiveDifficulty` ada sebagai fungsi murni dan method service (nama sama, signature berbeda); `AdaptiveInput` perlu field `Focus` yang tak tercantum §12.8.2. (6) `memctx` TTL = 4500 s karena `SessionContextInput` tak membawa durasi; `memver` memakai `MemVerBumper` M6 (TTL 30 hari, bukan 7). (7) Query retrieval memakai parameter `@now` (bukan `now()`) agar test deterministik. (8) `ReloadCoreIfChanged` butuh `qvec` awal: disimpan in-memory di `Retriever` (hilang saat takeover/restart → relevansi 0). (9) Urutan purge: learner dihapus sebelum evaluasi karena FK (sudah di M6). (10) `learner` tidak mengimpor `session`, sehingga adapter `session.AdaptiveProvider` berada di `cmd/api`. (11) `golang.org/x/text/unicode/norm` ditambahkan ke allowlist D-003 untuk NFKC. (12) M5 sudah menambahkan `LearnerContextProvider.Rerender` (tak ada di spec); `*learner.Service` mengimplementasikannya lewat `learner.Rerender`. (13) M5 sudah memiliki `learner.TTSSpeakingRate/MaxAISentences` dan tipe `memory`; M8 hanya menambah yang belum ada (Task 1, 5). (14) Tag e2e mengikuti konvensi `e2e && m5` dari M6.
