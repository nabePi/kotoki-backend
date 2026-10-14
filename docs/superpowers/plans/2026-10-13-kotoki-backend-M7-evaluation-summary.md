# Kotoki Backend — M7 Evaluation & Summary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Setiap event `session.completed` menghasilkan evaluasi pasca-sesi (`evaluations` + `evaluation_findings`, event `evaluation.completed`/`evaluation.failed`) dan `sessions.summary_text`, dibaca lewat `GET /api/v1/sessions/{id}/evaluation`, dengan tooling kalibrasi golden set yang berjalan pada dataset contoh.
**Architecture:** Paket fitur `evaluation` (→ `content`, `prompt`, `provider`, `events`) dan `summary` (→ `prompt`, `provider`, `events`) memegang aturan, port `Store`/`Tx`/`Source`, dan `Consumer` yang memenuhi `amqp.Handler` secara struktural (hanya mengimpor `events`); `postgres` dan `httpapi` hanya mengimplementasikan port/handler. Prompt, rubric, `output_schema`, dan `llm_params` hidup di `prompt_versions` (seed `0070_prompts.sql`); kode hanya merender, memvalidasi, dan memfilter. Kalibrasi = paket murni `evaluation/golden` + subcommand `kotoki-cli eval-golden`.
**Tech Stack:** Go 1.27, chi, pgx + sqlc, errgroup (`x/sync`), `santhosh-tekuri/jsonschema/v6`, `golang.org/x/text/unicode/norm` (NFKC; dipakai juga M2), prometheus client, testify, testcontainers (postgres, rabbitmq).
**Spec:** docs/superpowers/specs/2026-10-12-kotoki-backend-design.md — Bab 10 (§10.1–§10.14), §9.9.6 (summary), §9.5.2–9.5.3 (payload yang diterbitkan), §9.7 (kontrak handler), §8.2.5/8.2.7/8.2.8 (port provider yang dipakai), §8.9.5 (fake), §6.8.1/§6.10.1 (render & fixture), §5.9.6 + §5.10 (endpoint & kode error).
**Master plan:** docs/superpowers/plans/2026-10-13-kotoki-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M6 (M6 plan: `amqp.Handler/NewConsumer/Dial/NewConfirmPublisher/OutboxPublisher`, `events.Envelope/Delivery/DecodePayload[T]/Permanent/PermanentError/ErrAlreadyProcessed/ProcessedEvents/QueueEvaluation/QueueSummary/SessionCompletedV1/EvaluationCompletedV1/EvaluationFailedV1/ProducerWorker`, builder `events.NewEvaluationCompleted(id, now, producer, userID, EvaluationCompletedV1)` dan `NewEvaluationFailed(...)`, `(*postgres.Tx).Processed()`, `testutil.NewRabbitMQ/RabbitAdmin`, `cmd/worker` `buildConsumers`/`consumerDeps` dengan case `evaluation`/`summary` = `errHandlerNotImplemented`), M4 (`provider.AsyncRouter` (`Evaluation() provider.EvaluationProvider`, `CompleteText`)/`EvaluationProvider/StructuredRequest/TextRequest`, `prompt.Render/Template/Params/RegisterFixtures`, `fake.NewEvaluation`), M3 (`content.PromptVersion/Repository/ScenarioVersion` (kolom `formality`, D-014), `session.QueryService`, `events.OutboxWriter`, `seed/0070_prompts.sql` berisi CONVERSATION+OPENING, `TestRouteTable`, helper `httpapi`), M1 (migration `000016_evaluations` + `000017_messaging`, `testutil.NewPostgres/SeedUser/SeedContent/SeedSession`).

## Global Constraints

- Master Global Constraints berlaku.
- Skor dimensi integer 0–100 absolut; `overall_score = round(mean(5 dimensi), 2)`; NULL hanya bila insufficient evidence (semua skor NULL); status `PENDING|RUNNING|COMPLETED|FAILED`; finding type 16 nilai (semua dari LLM); severity `INFO|MINOR|MAJOR` (§10.1).
- Insufficient evidence bila `user_turn_count < 3` ATAU `user_speech_seconds < 20` ATAU `active_seconds < 60` (env `EVAL_MIN_USER_TURNS/EVAL_MIN_USER_SPEECH_SECONDS/EVAL_MIN_ACTIVE_SECONDS`) → `COMPLETED`, semua skor NULL, tanpa findings/LLM, `summary_feedback` = teks tetap §10.4; user turn eligible = status `COMPLETED|INTERRUPTED|FAILED`, `user_text` ≥ 2 rune setelah trim.
- Consumer: queue `evaluation.session_completed`, `SupportedEvents={"session.completed":1}`, timeout 200 s (`EVAL_HANDLER_TIMEOUT`); summary: `summary.session_completed`, timeout 60 s; retry 5s/30s/2m/10m, total 5 percobaan (D-092).
- Maksimum 2 panggilan `GenerateJSON` per delivery (utama → satu repair; D-017: lewat `AsyncRouter.Evaluation()`, satu vendor LLM aktif (`OPENAI_LLM_ENABLED`/`DEEPSEEK_LLM_ENABLED`), 1 attempt per panggilan, tanpa fallback vendor; kolom `evaluations.used_fallback` dihapus); vendor DeepSeek memakai JSON mode tanpa `json_schema` strict (M9 Task 6) sehingga validasi skema di sisi kode (`ParseOutput`/`SchemaCache`) adalah penegakan utama dan jalur repair lebih sering terpakai; `llm_params` seed v1 `{"temperature":0,"max_output_tokens":4000,"seed":42}` (nilai DB menang; default kode hanya bila key tidak ada); `SchemaName = kotoki_evaluation_rubric_v1`; `RequestID = "EVALUATION:<session_id>"`; repair menyertakan output lama dipotong 8.000 karakter.
- Filter finding: `confidence < 0.6` dibuang; maks 15 non-STRENGTH + 3 STRENGTH; `original` harus substring ternormalisasi (NFKC, tanpa whitespace dan `、。，．！？!?「」…・`) dari `user_text`; >50 % finding ditolak aturan = output invalid (repair).
- `FAILED` final tidak menulis `processed_events`.
- `GET /api/v1/sessions/{id}/evaluation`: non-terminal → `409 SESSION_NOT_ENDED` (`details.session_status`); terminal tanpa `started_at` → `404 EVALUATION_NOT_AVAILABLE`; selain itu `200` (`PENDING` disintesis bila belum ada baris) dengan `Retry-After: 3` untuk `PENDING|RUNNING`; user lain/tidak ada → `404 SESSION_NOT_FOUND`; `failure_message = "Evaluasi untuk sesi ini gagal diproses. Tim kami sudah diberi tahu."`.
- Summary: Jepang, ≤ `SUMMARY_MAX_CHARS`=300 karakter, model LLM vendor aktif (`OPENAI_LLM_MODEL` atau `DEEPSEEK_LLM_MODEL`, tanpa profil/tier; override `TextRequest.Model` dari `ROLLING_SUMMARY_LLM_MODEL` tidak dipakai di worker), `Task "SUMMARY"`, default `max_output_tokens` 400 / `temperature` 0.3 hanya bila key tidak ada; tidak dibuat bila `payload.user_turn_count == 0`; transcript > 30.000 karakter → baris AI dipotong 150; `UPDATE … WHERE summary_text IS NULL`; tidak menaikkan `sessions.version`.
- Event tidak dikirim untuk user `DELETED` (hanya `MarkProcessed` lalu ACK); tidak ada teks user/AI/prompt di log, span, atau label metrik.

## Review Focus

(Tidak ada butir master #1–5 yang dimiliki M7; berikut kondisi tersirat spec, masing-masing diuji di task pemilik.)
1. **Event ganda / dua pod memproses salinan sama:** tepat satu evaluasi COMPLETED dan satu `evaluation.completed` — Task 13 (unit `MarkProcessed`/`status<>'COMPLETED'`), Task 15 (paralel nyata di PG).
2. **Sesi terlalu singkat (2 turn, ucapan 19 s, 59 s):** tidak boleh memanggil LLM, tetap `COMPLETED` + event terbit — Task 2 (batas), Task 13, Task 15.
3. **Evaluator berhalusinasi atau menghukum artefak STT:** finding yang `original`-nya bukan substring, confidence < 0.6, atau tipe/dimensi salah dibuang; >50 % ditolak → repair (satu kali) → `INVALID_MODEL_OUTPUT` — Task 7, 8, 9, 15.
4. **User `DELETED`:** dilewati tanpa panggilan provider dan tanpa baris evaluasi — Task 13, 15.
5. **Isolasi & matriks API:** sesi user lain = 404 tanpa bocor; `FAILED` yang pernah ACTIVE tetap dievaluasi; tiga keadaan 409/404/200 — Task 16, 19.

## File Structure

| Path | Tanggung jawab |
|---|---|
| `internal/platform/errs/{codes,messages}.go`, `internal/httpapi/errors.go` (modifikasi) | `SESSION_NOT_ENDED` 409, `EVALUATION_NOT_AVAILABLE` 404 |
| `internal/evaluation/{enums,rules,config,metrics}.go`, `internal/platform/config/worker_eval.go` | enum, finding spec, overall, insufficient, `Config` (struct biasa), metrik; grup env `Worker.Evaluation/Summary` |
| `internal/evaluation/transcript.go`, `prompt.go` | `TurnRow`, statistik, format transcript; `EvaluationPromptData`, formalitas S-17, fixture |
| `internal/evaluation/{rubric,validator,filter}.go` | `EvalOutput`, validasi schema+bahasa, filter finding per aturan |
| `internal/evaluation/llm.go` | urutan panggilan LLM (utama + repair, maks 2) |
| `internal/evaluation/{repository,service,consumer,query}.go` | port, orkestrasi §10.5, handler queue, matriks API §10.11 |
| `internal/evaluation/golden/*.go` | metrik kalibrasi, dataset, runner, report |
| `internal/summary/{prompt,transcript,service,consumer,repository}.go` | `SummaryPromptData`, transcript, orkestrasi §9.9.6 |
| `internal/postgres/{evaluation_repo,summary_repo}.go`, `db/queries/{evaluations,summary}.sql` | implementasi port; `internal/postgres/store.go` (modifikasi) memuat accessor `EvaluationStore()/SummaryStore()` |
| `internal/httpapi/evaluation_{routes,handler,dto}.go` | endpoint evaluasi |
| `internal/provider/fake/{fixtures/kotoki_evaluation_rubric_v1.json,rubric_findings.go}` | fixture + finding turunan transcript (§10.13) |
| `internal/testutil/{seed,finished_session}.go`, `fakes/evaluation_store.go`, `repotest/evaluation.go` | helper integration, fake store, contract suite |
| `seed/0070_prompts.sql` (modifikasi) | tambah version 1 EVALUATION dan SUMMARY |
| `cmd/api/main.go`, `cmd/worker/main.go`, `cmd/kotoki-cli/eval_golden.go` | wiring + subcommand `eval-golden` |
| `testdata/benchmark/evaluation/rubric-v1/{manifest.json,example.jsonl}` | dataset contoh (80 sampel nyata = prasyarat manual) |
| `test/integration/m7_*_test.go` | skenario integration (`//go:build integration`) |

---

### Task 1: Preflight kontrak M3/M4/M6 + kode error evaluasi

**Files:**
- Create: `internal/m7preflight_test.go`
- Modify: `internal/platform/errs/codes.go`, `internal/platform/errs/messages.go`, `internal/httpapi/errors.go`
- Test: `internal/platform/errs/m7codes_test.go`

**Interfaces:**
- Consumes: konvensi M2 `errs.Code<PascalCase>` (mis. `errs.CodeSessionNotFound`) dan `httpapi.StatusOf(code)`; simbol M3/M4/M6 yang dipakai plan ini (daftar di Step 1).
- Produces: `errs.CodeSessionNotEnded` (409, "Evaluasi baru tersedia setelah sesi selesai."), `errs.CodeEvaluationNotAvailable` (404, "Evaluasi tidak tersedia karena sesi ini tidak pernah dimulai.").

- [ ] **Step 1: Write the failing test** — `TestM7CodesHaveMessagesAndStatus` (table `{code,status,message}` di atas; `errs.Messages[c]` persis kolom §5.10, `httpapi.StatusOf(c)==status`). `m7preflight_test.go` hanya `var _ = ...` yang merujuk: `prompt.Render/Template/Rendered/Params/RegisterFixtures`, `provider.AsyncRouter` (method `Evaluation() provider.EvaluationProvider`)`/EvaluationProvider/StructuredRequest/StructuredResult/TextRequest/IsRetryable`, `fake.NewEvaluation`, `content.PromptVersion/ScenarioVersion`, literal interface `interface{GetScenarioVersion(context.Context, uuid.UUID)(content.ScenarioVersion, error); GetTopicByID(...); GetScenario(...); GetPublishedPromptVersion(context.Context, content.PromptKey)(content.PromptVersion, error)} = content.Repository(nil)`, `events.Envelope/Delivery/DecodePayload[events.SessionCompletedV1]/Permanent/PermanentError/ErrAlreadyProcessed/ProcessedEvents/OutboxWriter/Event/ProducerWorker/QueueEvaluation/QueueSummary/SessionCompletedV1/EvaluationCompletedV1/EvaluationFailedV1/NewEvaluationCompleted/NewEvaluationFailed` (signature M6: `(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p <Payload>) events.Event`), `amqp.Handler`, serta mekanisme M6 untuk label framework `skipped_deleted_user` (spec §9.7.2; sentinel `events.ErrSkippedDeletedUser`, nama dari plan — bila M6 memakai nama/mekanisme lain, ikuti M6 dan catat di deviations).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/errs/... ./internal/httpapi/... . -race -run 'TestM7Codes' && go vet ./internal/m7preflight_test.go`. Expected: FAIL (konstanta belum ada; simbol yang belum cocok = error kompilasi).
- [ ] **Step 3: Implement** — tambah hanya kode yang hilang (pesan dari §5.10, status ke `httpapi/errors.go`). Simbol preflight yang namanya berbeda: sesuaikan call-site plan ini dan catat di `docs/implementation-deviations.md`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go build ./...`.
- [ ] **Step 5: Commit** — `git add internal/ && git commit -m "feat(errs): evaluation error codes and M7 contract preflight"`

### Task 2: Domain evaluasi — enum, spesifikasi finding, overall, insufficient, `Config`, `Metrics`

**Files:**
- Create: `internal/evaluation/enums.go`, `rules.go`, `config.go`, `metrics.go`, `internal/platform/config/worker_eval.go`
- Modify: `internal/platform/config/worker.go` (`Worker` memuat `Evaluation`, `Summary`; `loadWorker` memanggilnya)
- Test: `internal/evaluation/rules_test.go`, `metrics_test.go`, `internal/platform/config/worker_eval_test.go`

**Interfaces:**
- Produces (§10.1 persis): `Status`, `Dimension`, `Severity`, `FindingType`, `FailureCode` (konstanta = nilai spec, mis. `StatusRunning`, `DimGrammar`, `FindingParticleError`, `FailInvalidModelOutput`); `func RubricVersionOf(pv content.PromptVersion) string`; `var FindingDimension map[FindingType]Dimension`; `type Req int` (`ReqRequired|ReqOptional|ReqNull`); `type FindingSpec struct{Dimension Dimension; TurnSeq, Original, Corrected Req}`; `func SpecOf(t FindingType) (FindingSpec, bool)` (tabel §10.1); `var LLMDimensions []Dimension` (5, urutan D-096); `type Scores struct{Grammar, Vocabulary, Fluency, Naturalness, TaskAchievement *int}`; `func ComputeOverall(s Scores) *float64`; `type Stats struct{UserTurns, UserSpeechSeconds, ActiveSeconds int}`; `type Thresholds struct{MinUserTurns, MinSpeechSeconds, MinActiveSeconds int}`; `func (s Stats) Insufficient(t Thresholds) bool`; `const InsufficientSummary = "Percakapan ini terlalu singkat untuk dinilai secara akurat. Coba bicara minimal 3 kali dengan total sekitar 1 menit agar kami bisa memberikan umpan balik yang berguna."`; `type Config struct` (struct biasa, **tanpa tag env**, tidak di-embed di `platform/config`: `MinUserTurns`, `MinUserSpeechSeconds`, `MinActiveSeconds int`, `MinFindingConfidence float64`, `TranscriptMaxChars int`, `HandlerTimeout time.Duration`) + `func DefaultConfig() Config` (3/20/60/0.6/60000/200s); di `internal/platform/config` (tidak mengimpor paket fitur): `type WorkerEvaluation struct{MinUserTurns int \`env:"EVAL_MIN_USER_TURNS" envDefault:"3"\`; MinUserSpeechSeconds \`EVAL_MIN_USER_SPEECH_SECONDS\`=20; MinActiveSeconds \`EVAL_MIN_ACTIVE_SECONDS\`=60; MinFindingConfidence float64 \`EVAL_MIN_FINDING_CONFIDENCE\`=0.6; TranscriptMaxChars \`EVAL_TRANSCRIPT_MAX_CHARS\`=60000; HandlerTimeout time.Duration \`EVAL_HANDLER_TIMEOUT\`=200s}`, `type WorkerSummary struct{MaxChars int \`SUMMARY_MAX_CHARS\`=300}` sebagai field `Worker.Evaluation/Summary` (pola M6 `Worker.Outbox`/`Worker.Jobs`); `func NewMetrics(reg prometheus.Registerer) *Metrics` dengan method nil-safe `LLMCall(provider, result string)`, `FindingDropped(reason string)`, `ObserveLatency(seconds float64)` → `kotoki_evaluation_llm_calls_total{provider,result}`, `kotoki_evaluation_findings_dropped_total{reason}`, `kotoki_evaluation_latency_seconds` (ASYNC_BUCKETS).

- [ ] **Step 1: Write the failing test**
```go
func TestComputeOverall(t *testing.T) {
	p := func(v int) *int { return &v }
	s := evaluation.Scores{Grammar: p(72), Vocabulary: p(65), Fluency: p(58), Naturalness: p(70), TaskAchievement: p(80)}
	require.Equal(t, 69.00, *evaluation.ComputeOverall(s))
	require.Equal(t, 1.33, *evaluation.ComputeOverall(evaluation.Scores{Grammar: p(1), Vocabulary: p(1), Fluency: p(2)}))
	require.Nil(t, evaluation.ComputeOverall(evaluation.Scores{}))
}
func TestInsufficientBoundaries(t *testing.T) {
	th := evaluation.Thresholds{MinUserTurns: 3, MinSpeechSeconds: 20, MinActiveSeconds: 60}
	for _, c := range []struct{ s evaluation.Stats; want bool }{
		{evaluation.Stats{2, 100, 100}, true}, {evaluation.Stats{3, 19, 100}, true},
		{evaluation.Stats{3, 20, 59}, true}, {evaluation.Stats{3, 20, 60}, false}} {
		require.Equal(t, c.want, c.s.Insufficient(th))
	}
}
```
Kasus lain: `TestSpecOf` (`PARTICLE_ERROR`→GRAMMAR semua Required; `EXCESSIVE_FILLER` Corrected Optional; `OBJECTIVE_NOT_MET` ketiganya ReqNull; `STRENGTH` tidak ada di `FindingDimension`; 16 tipe terdaftar di `SpecOf`/enum); `TestRubricVersionOf` (`Version 1` → `"rubric-v1"`); `worker_eval_test` (`loadWorker(map[string]string{})`) → default persis di atas, `EVAL_MIN_USER_TURNS=5` terbaca; `TestDefaultConfig` (evaluation) sama dengan default tersebut; `metrics_test` (counter naik, nil `*Metrics` tidak panic).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ ./internal/platform/config/ -race -run 'Overall|Insufficient|SpecOf|Rubric|Config|Metrics|WorkerEval'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** file di atas; overall = `math.Round(mean*100)/100`; `SpecOf` tabel statis dari §10.1 (STRENGTH: Dimension kosong, TurnSeq/Original Optional, Corrected Null).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ ./internal/platform/config/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal && git commit -m "feat(evaluation): domain enums, finding spec, overall, insufficient, config, metrics"`

### Task 3: Transcript, statistik, kelayakan turn

**Files:**
- Create: `internal/evaluation/transcript.go`
- Test: `internal/evaluation/transcript_test.go`

**Interfaces:**
- Produces: `type TurnRow struct{ID uuid.UUID; CreatedAt time.Time; Seq int; Status *string; UserText, AssistantText, AssistantSpokenText *string; UserStartedAt, UserEndedAt *time.Time}` (nama dari plan); `func EligibleUserTurn(t TurnRow) bool`; `func ComputeStats(turns []TurnRow, startedAt, endedAt time.Time, disconnectedSeconds int) Stats` (`UserSpeechSeconds = floor(Σ durasi eligible)`, timestamp NULL → 0; `ActiveSeconds = max(0, floor(ended−started) − disconnected)`); `func BuildTranscript(turns []TurnRow, maxChars int) string`.

- [ ] **Step 1: Write the failing test** — `TestBuildTranscript` tabel: opening T0 (user_text NULL) → hanya `[T0] AI: …`; turn `COMPLETED` dengan `user_text "今日は会社で働きます"` (10 rune non-spasi), durasi 2.0 s → `[T1] USER (2.0 s, 5.0 chars/s): 今日は会社で働きます` lalu `[T1] AI: …`; AI memakai `assistant_spoken_text` bila ada, selain itu `assistant_text` (teks Jepang polos, tanpa tag; tidak ada pembersihan ekspresi, D-014); turn `INTERRUPTED` → baris AI `[T2] AI (cut off by the user): …` dan baris USER turn berikutnya `[T3] USER (…, interrupted the AI): …` (aturan teks §10.6.2 yang dipakai; contoh transcript spec tidak konsisten dengan aturan itu); turn `FAILED` tanpa teks AI → `[T4] AI: (no reply: system error)`; turn `DISCARDED`/status NULL tidak dirender. `TestTranscriptTruncation`: > `maxChars` → setiap baris AI dipotong 200 rune + `…`, baris USER utuh; bila masih > max → 120 turn terakhir dengan baris pertama `[earlier turns omitted]`. `TestComputeStats`: durasi NULL dihitung 0; `user_text "あ"` (1 rune) dan opening tidak eligible; `ActiveSeconds` tidak negatif; status `DISCARDED` tidak eligible.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ -race -run 'Transcript|ComputeStats'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `chars/s = rune non-spasi / durasi`, 1 desimal (`%.1f`); blok per turn: baris USER lalu AI.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/evaluation && git commit -m "feat(evaluation): transcript builder and session statistics"`

### Task 4: `EvaluationPromptData`, formalitas sesi (S-17), fixture

**Files:**
- Create: `internal/evaluation/prompt.go`
- Test: `internal/evaluation/prompt_test.go`

**Interfaces:**
- Consumes: `content.ScenarioVersion` (termasuk `Formality`, D-014), `content.Formality`, `evaluation.Stats`.
- Produces: `type EvaluationPromptData struct` field persis §10.6.2 (`RubricVersion, FindingTypes, PatternKeyExamples, TopicTitle, ScenarioTitle string; DifficultyLevel int; LearnerRole, AIRole, CharacterName string; Objectives []string; ScenarioContext, ExpectedFormality string; EvaluationFocus []string; UserTurnCount, UserSpeechSeconds, ActiveSeconds int; Transcript string`); `type PromptInput struct{TopicName, ScenarioName, CharacterName string; SV content.ScenarioVersion; SessionAdaptive json.RawMessage; RubricVersion string; Stats Stats; Transcript string}`; `func BuildPromptData(in PromptInput) EvaluationPromptData`; `func ExpectedFormality(adaptive json.RawMessage, sv content.ScenarioVersion) string`; `func FindingTypesText() string`; `func PatternKeyExamplesText() string`; `func PromptFixtures() []any`; `const ExampleOutput` (JSON contoh valid §10.7.1, string Go).

- [ ] **Step 1: Write the failing test** — `TestExpectedFormality` tabel: `{"formality":"CASUAL"}` → `casual (plain form, タメ口)`; `POLITE` → `polite (です・ます)`; `KEIGO_LIGHT` → `polite (です・ます); fixed keigo phrases of the role are expected, and any correct keigo is also acceptable`; `KEIGO` → `polite/keigo (です・ます, 尊敬語・謙譲語 where appropriate)`; JSON tanpa key `formality`, `nil`, atau nilai tak dikenal → fallback `sv.Formality` (`scenario_versions.formality`; `CASUAL`/`POLITE`/`KEIGO` dipetakan ke frasa yang sama dengan nilai adaptive, D-014). `TestBuildPromptDataObjectives` (`goal` + `tasks` + `"Use: " + join(grammar_targets, ", ")`, baris grammar tidak ada bila kosong). `TestFindingTypesText`: 16 baris, memuat persis `PARTICLE_ERROR -> GRAMMAR; turn_seq, original, corrected required`, `EXCESSIVE_FILLER -> FLUENCY; turn_seq, original required; corrected optional`, `OBJECTIVE_NOT_MET -> TASK_ACHIEVEMENT; turn_seq, original, corrected must be null`. `TestPatternKeyExamples` (13 contoh §10.6.2 dipisah `, `, urutan spec). `TestPromptFixtures`: tepat 2 elemen bertipe `EvaluationPromptData`; minimal: slice kosong, transcript 3 turn; maksimal: transcript 60.000 karakter, `EvaluationFocus` 3 item, semua slice terisi.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ -race -run 'ExpectedFormality|BuildPromptData|FindingTypesText|PatternKey|PromptFixtures'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `adaptive_difficulty` di-parse hanya key `formality` (paket `evaluation` tidak mengimpor `learner`); `FindingTypesText` dibangun dari `SpecOf` (STRENGTH baris khusus: `STRENGTH -> any of GRAMMAR, VOCABULARY, FLUENCY, NATURALNESS, TASK_ACHIEVEMENT; severity must be INFO; turn_seq and original optional; corrected must be null`); `ExampleOutput` = contoh JSON §10.7.1 (dua findings) apa adanya.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/evaluation && git commit -m "feat(evaluation): prompt data, session formality mapping, fixtures"`

### Task 5: Seed prompt EVALUATION (+ `ApplySeed`) dan test render/schema (integration)

**Files:**
- Modify: `seed/0070_prompts.sql` (tambah blok sebelum `COMMIT`), `test/integration/m3_seed_test.go` (konstanta `expectedPromptVersionCount` 2 → 3 dan satu panggilan `prompt.RegisterFixtures("EVALUATION", evaluation.PromptFixtures)` di setup test; keduanya langkah eksplisit di Step 1 dan Step 3)
- Create: `internal/testutil/seed.go` (tag `integration || e2e`), `internal/evaluation/testdata/output_schema.json` (salinan skema §10.7.1 untuk unit test paket), `test/integration/m7_seed_test.go`
- Test: `test/integration/m7_seed_test.go` (`integration`)

**Interfaces:**
- Consumes: `content.Repository.GetPublishedPromptVersion`, `content.Validator.ValidateCatalog`, `evaluation.PromptFixtures/ExampleOutput/BuildPromptData`, `prompt.Render`, `jsonschema/v6`.
- Produces: `func ApplySeed(t testing.TB, pool *pgxpool.Pool)` (jalankan `seed/*.sql` berurutan nama file lewat `pool.Exec` tanpa argumen = simple protocol); prompt_versions id `01920000-000d-7000-8000-000000000004` (config `01920000-000c-7000-8000-000000000004`, status `PUBLISHED`, `ON CONFLICT (id) DO NOTHING`).

- [ ] **Step 1: Write the failing test** — `TestEvaluationSeedPrompt`: `ApplySeed` pada DB baru; `GetPublishedPromptVersion("EVALUATION")` → `Version==1`, `UserTemplate!=nil`, `LLMParams` = `{Temperature:0, MaxOutputTokens:4000, Seed:42}`; `OutputSchema` ter-compile (draft 2020-12) dan `evaluation.ExampleOutput` lolos validasi; `prompt.Render(pv.Template(), fixture)` sukses untuk kedua `PromptFixtures()` (System berisi `rubric-v1`, tidak mengandung `{{`, User berisi `## Transcript`); `ValidateCatalog` tanpa ERROR bila fixture terdaftar; seed dijalankan 2× → tetap 1 version EVALUATION; di `m3_seed_test.go` ubah `expectedPromptVersionCount` ke 3 dan tambahkan `prompt.RegisterFixtures("EVALUATION", evaluation.PromptFixtures)` (tanpa ini `TestSeedContentIsValid`/`ValidateCatalog` M3 gagal `missing_fixture` atau jumlah salah); `TestSchemaCopyMatchesSeed` (`JSONEq` antara `internal/evaluation/testdata/output_schema.json` dan `output_schema` seed).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration -race -run TestEvaluationSeedPrompt`. Expected: FAIL (version EVALUATION belum ada).
- [ ] **Step 3: Implement** — pastikan perubahan `m3_seed_test.go` (konstanta=3 + `RegisterFixtures`) sudah ada; tambahkan baris `prompt_configs` 03/04 hanya bila blok M3 belum memuat kelima key (`ON CONFLICT (id) DO NOTHING`); version EVALUATION: `system_template` dan `user_template` **verbatim §10.6.2** (dollar-quoting `$tpl$`), `output_schema` **verbatim §10.7.1 tanpa `$schema`/`$id`**, `llm_params` `{"temperature":0,"max_output_tokens":4000,"seed":42}`, `notes 'Initial version'`, `published_at now()`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go test -tags integration ./test/integration -race -run TestSeed` (M3) tetap PASS.
- [ ] **Step 5: Commit** — `git add seed internal/testutil test && git commit -m "feat(seed): EVALUATION prompt v1 with rubric schema"`

### Task 6: Fake evaluation — fixture rubric + finding turunan transcript

**Files:**
- Create: `internal/provider/fake/fixtures/kotoki_evaluation_rubric_v1.json`, `internal/provider/fake/rubric_findings.go`
- Modify: `internal/provider/fake/eval.go` (panggil hook setelah substitusi `{{score}}` bila `req.SchemaName == "kotoki_evaluation_rubric_v1"`)
- Test: `internal/provider/fake/rubric_findings_test.go`, `test/integration/m7_seed_test.go` (tambah)

**Interfaces:**
- Consumes: `fake.Evaluation.GenerateJSON` (M4 Task 13), seed schema (Task 5).
- Produces: `func RubricFindings(req provider.StructuredRequest, doc []byte) []byte` (nama dari plan; spec §8.9.5 hanya fixture statis sedangkan §10.13 menuntut finding turunan transcript — plan menggabungkan keduanya).

- [ ] **Step 1: Write the failing test** — `TestRubricFindingsDerivedFromTranscript`: `Messages[last].Content` berisi `[T1] USER (2.0 s, 5.0 chars/s): 今日は会社で働きます` … 7 baris USER (T1–T7) → tepat 2 finding (satu per 3 user turn: turn ke-3 dan ke-6 dalam urutan USER); tiap finding: `finding_type "UNNATURAL_EXPRESSION"`, `dimension "NATURALNESS"`, `severity "MINOR"`, `confidence 0.9`, `turn_seq` = nomor `[T<n>]`, `original` = ≤ 6 rune pertama `user_text` tanpa tanda baca (substring ternormalisasi), `corrected = original + "です"`, `explanation` Indonesia ≥ 10 karakter; deterministik (dua panggilan identik); < 3 USER turn → `findings: []`; skor 5 dimensi berasal dari placeholder `{{score}}` (50–90, integer). Integration: `TestFakeRubricFixtureMatchesSeedSchema` — keluaran `fake.NewEvaluation(...).GenerateJSON` untuk transcript contoh lolos `output_schema` seed (dibaca dari DB).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/fake/ -race -run RubricFindings` dan `go test -tags integration ./test/integration -race -run TestFakeRubricFixture`. Expected: FAIL.
- [ ] **Step 3: Implement** — fixture = `ExampleOutput` (Task 4) dengan `findings: []` dan lima `"score": "{{score}}"` + dua signal; hook mem-parse baris `^\[T(\d+)\] USER[^:]*: (.+)$` dari pesan user terakhir.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race` → PASS; perintah integration → PASS.
- [ ] **Step 5: Commit** — `git add internal/provider test && git commit -m "feat(provider/fake): rubric fixture with transcript-derived findings"`

### Task 7: `EvalOutput`, validasi schema + bahasa

**Files:**
- Create: `internal/evaluation/rubric.go`, `internal/evaluation/validator.go`
- Test: `internal/evaluation/validator_test.go`

**Interfaces:**
- Produces: `type EvalOutput struct{Scores struct{...}; Signals struct{...}; Summary string; Strengths, FocusAreas []string; Recommendations []Recommendation; Findings []RawFinding}` (properti persis skema v1; `DisallowUnknownFields`), `type RawFinding struct{Dimension, FindingType, Severity string; TurnSeq *int; Original, Corrected, PatternKey *string; Explanation string; Confidence float64}`; `var ErrInvalidModelOutput = errors.New("evaluation: invalid model output")`; `type OutputError struct{Problems []string}` (`errors.Is(err, ErrInvalidModelOutput)` true); `type SchemaCache`, `func NewSchemaCache() *SchemaCache`, `func (c *SchemaCache) Get(id uuid.UUID, schema json.RawMessage) (*jsonschema.Schema, error)` (compile sekali per `prompt_version_id`); `func ParseOutput(schema *jsonschema.Schema, raw json.RawMessage) (EvalOutput, error)` (bulatkan skor non-integer dengan `math.Round` sebelum validasi); `func CheckLanguage(out EvalOutput) []string`.

- [ ] **Step 1: Write the failing test** — memakai schema §10.7.1 lewat helper test `loadSchema(t)` yang membaca `internal/evaluation/testdata/output_schema.json` (salinan **hanya untuk unit test**, dijaga sama dengan seed oleh `TestSchemaCopyMatchesSeed`, Task 5): `TestParseOutputRules` tabel — `ExampleOutput` valid; field `summary` hilang → `OutputError` (problem menyebut `summary`); `scores.GRAMMAR.score` 101 → invalid; `finding_type "UNKNOWN_TYPE"` (enum) → invalid; properti tak dikenal → invalid; skor `85.4` → diterima dan menjadi `85`; `strengths: []` (minItems 1) → invalid. `TestCheckLanguage`: summary Indonesia lolos; summary Jepang murni → problem; Indonesia dengan kutipan 「会社で働く」 lolos; comment kosong setelah buang CJK → problem; `explanation` Jepang murni di finding → problem. `TestSchemaCacheCompilesOnce` (dua `Get` id sama → pointer sama).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ -race -run 'ParseOutput|CheckLanguage|SchemaCache'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `CheckLanguage`: setelah membuang segmen `「…」` dan rune Han/Hiragana/Katakana, teks valid bila ada ≥ 1 huruf Latin dan ≥ 50 % rune tersisa adalah huruf Latin/spasi/tanda baca; cek `summary`, 5 `comment`, dan setiap `explanation`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/evaluation && git commit -m "feat(evaluation): output schema validation and language check"`

### Task 8: Filter finding per aturan (§10.7.2)

**Files:**
- Create: `internal/evaluation/filter.go`
- Test: `internal/evaluation/filter_test.go`

**Interfaces:**
- Produces: `type TurnRef struct{ID uuid.UUID; Seq int; UserText string}`; `type Finding struct{Type FindingType; Dimension Dimension; Severity Severity; TurnSeq *int; TurnID *uuid.UUID; Original, Corrected, PatternKey *string; Explanation string; Confidence float64; Source string}` (`Source` = `"LLM"`); `type FilterResult struct{Kept []Finding; Dropped map[string]int; Rejected, Total int}`; `func FilterFindings(raw []RawFinding, turns []TurnRef, minConfidence float64) FilterResult`; `func (r FilterResult) TooManyRejected() bool` (`Total>0 && Rejected*2 > Total`); `func NormalizeForMatch(s string) string`.

- [ ] **Step 1: Write the failing test** — `TestNormalizeForMatch` (`"えっと…、今日は 会社"` → `"えっと今日は会社"` setelah NFKC; `"ＡＢＣ！"` → `"ABC"`). `TestFilterFindings` tabel, tiap aturan satu kasus, reason: `type_dimension_mismatch` (`PARTICLE_ERROR` + dimension `VOCABULARY`), `missing_field` (`PARTICLE_ERROR` tanpa `corrected`), `must_be_null` (`OBJECTIVE_NOT_MET` dengan `turn_seq`), `turn_not_found` (`turn_seq` tidak punya baris USER), `original_not_found`, `corrected_unchanged` (corrected = original setelah normalisasi, tipe error), `low_confidence` (0.59 dibuang, 0.60 lolos), `duplicate` (type+turn_seq+original ternormalisasi sama → simpan pertama). `STRENGTH` dengan severity `MAJOR` dan `corrected "x"` → lolos dengan severity `INFO`, `corrected nil`, dimension dari LLM. Batas: 20 error non-STRENGTH + 5 STRENGTH → kept 15 + 3 (MAJOR dahulu lalu confidence tertinggi). `TooManyRejected`: 3 dari 5 ditolak aturan validitas → true; 2 dari 5 → false; penolakan `low_confidence`/`duplicate`/batas jumlah **tidak** dihitung ke `Rejected` (keputusan plan: ambiguitas §10.7.2 poin 3, pilihan konservatif agar repair tidak terpicu oleh sekadar confidence rendah). Setiap drop menaikkan `Dropped[reason]`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ -race -run 'NormalizeForMatch|FilterFindings'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `NormalizeForMatch`: `norm.NFKC.String`, buang `unicode.IsSpace` dan rune di himpunan `、。，．,.！？!?「」…・`; `TurnID` diisi dari `TurnRef`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/evaluation && git commit -m "feat(evaluation): per-finding validation and filtering"`

### Task 9: `LLMEvaluator` — urutan utama → repair (maks 2 panggilan, tanpa fallback)

**Files:**
- Create: `internal/evaluation/llm.go`
- Modify: `internal/prompt/render.go` (tambah `func (p Params) SeedOr(def int64) int64` bila `grep -n SeedOr internal/prompt` kosong), `internal/prompt/render_test.go`
- Test: `internal/evaluation/llm_test.go`

**Interfaces:**
- Consumes: `provider.EvaluationProvider` (dari `AsyncRouter.Evaluation()`), `SchemaCache`, `ParseOutput`, `CheckLanguage`, `FilterFindings`, `Metrics`.
- Produces: `type LLMInput struct{SessionID uuid.UUID; PV content.PromptVersion; Rendered prompt.Rendered; Turns []TurnRef}`; `type LLMResult struct{Output EvalOutput; Findings []Finding; Provider provider.Name; Model string; CallCount, InputTokens, OutputTokens int; Dropped map[string]int}`; `func NewLLMEvaluator(ev provider.EvaluationProvider, schemas *SchemaCache, m *Metrics, minConfidence float64) *LLMEvaluator`; `func (e *LLMEvaluator) Evaluate(ctx context.Context, in LLMInput) (LLMResult, error)`; `func BuildStructuredRequest(sessionID uuid.UUID, pv content.PromptVersion, msgs []provider.Message) provider.StructuredRequest`; `func RepairMessages(base []provider.Message, previous string, problems []string) []provider.Message`.

- [ ] **Step 1: Write the failing test** — provider skrip `scriptedEval{steps []func(provider.StructuredRequest) (provider.StructuredResult, error)}` (memenuhi `provider.EvaluationProvider`) di `llm_test.go`. Kasus: (1) valid pada call 1 → `CallCount 1`; request: `Task "EVALUATION"`, `SchemaName "kotoki_evaluation_rubric_v1"` (dari `pv.Version`), `RequestID "EVALUATION:"+sid`, `Temperature 0`, `*Seed 42`, `MaxOutputTokens 4000` (`LLMParams` kosong → 4000/0/42; `LLMParams.MaxOutputTokens=3000` → 3000); (2) call 1 JSON gagal schema, call 2 valid → `CallCount 2`; pesan call 2 = pesan asli + `assistant`(output lama dipotong 8.000 karakter) + `user` yang diawali `Your previous output was rejected by the validator. Errors:\n- ` dan diakhiri `Return the complete corrected JSON object only. Follow the schema and all rules exactly.`; (3) call 1 & 2 invalid → `errors.Is(err, ErrInvalidModelOutput)`, `CallCount 2` (tidak ada panggilan ke-3); (4) call 1 `*provider.Error{Kind: UNAVAILABLE}` → error dikembalikan apa adanya (tanpa repair/fallback; retry lintas delivery lewat retry queue Bab 9), `CallCount 1`; (5) `CONTENT_FILTERED` → error dikembalikan; (6) `INVALID_RESPONSE` dari primary → perlakuan sama seperti output ditolak (repair); (7) DeepSeek-style: call 1 mengembalikan JSON kosong/tidak valid (`INVALID_RESPONSE`, retryable) → repair (call 2) → valid → `CallCount 2`; (8) skema dilanggar secara ketat (properti tak dikenal, enum/rentang salah) ditolak `ParseOutput` walau `json.Valid` lolos (tanpa penegakan skema vendor); (9) >50 % finding ditolak aturan (`TooManyRejected`) → diperlakukan invalid (repair); (10) `InputTokens/OutputTokens` = jumlah semua panggilan; (11) metrik `LLMCall(provider,"valid|invalid|error")` per panggilan. `render_test`: `SeedOr` default dan nilai DB.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ ./internal/prompt/ -race -run 'LLM|Evaluate|SeedOr'`. Expected: FAIL.
- [ ] **Step 3: Implement** — algoritma tabel §10.6.1 persis; error provider (`UNAVAILABLE`, `CONTENT_FILTERED`, dll.) dikembalikan apa adanya; hanya output ditolak validator (termasuk `INVALID_RESPONSE`) yang memicu satu repair.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ ./internal/prompt/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal && git commit -m "feat(evaluation): LLM evaluator with single repair (no vendor fallback)"`

### Task 10: (dihapus dari v1)

Dihapus dari v1 (pronunciation ditunda ke pasca-v1).

### Task 11: Port repository evaluasi, Postgres, fake store, contract suite

**Files:**
- Modify: `internal/postgres/store.go` (tambah `(*Store).EvaluationStore() evaluation.Store` di tempat M6 menyatukan semua accessor)
- Create: `internal/evaluation/repository.go`, `internal/postgres/evaluation_repo.go`, `db/queries/evaluations.sql`, `internal/testutil/fakes/evaluation_store.go`, `internal/testutil/repotest/evaluation.go`
- Test: `internal/testutil/fakes/evaluation_store_test.go`, `internal/postgres/evaluation_repo_integration_test.go` (`integration`)

**Interfaces:**
- Produces: `type Evaluation struct` (semua kolom §10.2, PascalCase; `Scores`, `DimensionConfidence map[string]float64`, `DimensionComments map[string]string`, `Strengths, FocusAreas []string`, `Recommendations []Recommendation`); `type FindingRow struct{ID uuid.UUID; Ordinal int; Finding; CreatedAt time.Time}`; `type ClaimInput struct{ID, SessionID, UserID, PromptVersionID, SourceEventID uuid.UUID; RubricVersion string}`; `type ClaimResult struct{ID uuid.UUID; Claimed bool; AttemptCount int}`; `type CompletedUpdate struct` (semua kolom hasil §10.5 langkah 10, termasuk `InsufficientEvidence`); `type Repository interface{Claim(ctx, ClaimInput) (ClaimResult, error); Complete(ctx, id uuid.UUID, u CompletedUpdate) (bool, error); ReplaceFindings(ctx, evaluationID uuid.UUID, userID, sessionID uuid.UUID, fs []FindingRow) error; MarkPending(ctx, sessionID uuid.UUID, lastError string) error; MarkFailed(ctx, sessionID uuid.UUID, code FailureCode, lastError string) (Evaluation, bool, error)}`; `type Tx interface{Evaluations() Repository; Processed() events.ProcessedEvents; Outbox() events.OutboxWriter}`; `type Store interface{WithinTx(ctx, fn func(ctx context.Context, tx Tx) error) error}`; `type SessionInfo struct{ID, UserID, ScenarioID, ScenarioVersionID, TopicID uuid.UUID; Status string; StartingAt, StartedAt, EndedAt *time.Time; DisconnectedSeconds int; AdaptiveDifficulty, ConfigSnapshot json.RawMessage; TranscriptPurgedAt *time.Time; UserStatus string}`; `type Source interface{LoadSession(ctx, id uuid.UUID) (SessionInfo, error); LoadTurns(ctx, sessionID uuid.UUID, from, to time.Time) ([]TurnRow, error)}` (`ErrSessionNotFound`; turns `status IS DISTINCT FROM 'DISCARDED'` urut `sequence_number`, batas `created_at` untuk partition pruning); `type Reader interface{GetBySession(ctx, sessionID uuid.UUID) (Evaluation, bool, error); ListFindings(ctx, evaluationID uuid.UUID) ([]FindingRow, error)}`; `(*postgres.Store).EvaluationStore() evaluation.Store` (di `internal/postgres/store.go`), `postgres.NewEvaluationSource(pool)`, `postgres.NewEvaluationReader(pool)`; `repotest.EvaluationRepoSuite(t, newStore func(t *testing.T) evaluation.Store)`; `fakes.NewEvaluationStore() *EvaluationStore` (rollback saat `fn` error, `FailNext`).

- [ ] **Step 1: Write the failing test** — `EvaluationRepoSuite` subtests: `claim creates RUNNING attempt 1`; `claim again from PENDING and FAILED increments attempt and clears failure_code/last_error`; `claim on COMPLETED returns Claimed=false and no change` (SQL claim §10.5 langkah 4 persis, `WHERE evaluations.status <> 'COMPLETED'`); `Complete insufficient row satisfies chk_evaluations_insufficient`; `Complete normal sets completed_at` dan `Complete again returns false`; `ReplaceFindings deletes old rows then inserts, ordinals unique`; `MarkPending only from RUNNING`; `MarkFailed from RUNNING|PENDING returns changed=true once, second call false, COMPLETED untouched`; `rollback discards claim`. `TestPGEvaluationRepo` menjalankan suite ke PG; `TestFakeEvaluationStore` ke fake. Tambahan PG: CHECK `chk_ef_type_dimension` menolak `PARTICLE_ERROR`+`VOCABULARY`; `LoadTurns` tidak mengembalikan `DISCARDED`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/testutil/fakes/ -race -run FakeEvaluationStore` dan `go test -tags integration ./internal/postgres/ -race -run PGEvaluationRepo`. Expected: FAIL.
- [ ] **Step 3: Implement** — sqlc queries (`make sqlc`, commit hasil generate); `Claim` = upsert §10.5; `MarkFailed` mengembalikan baris setelah UPDATE (butuh `AttemptCount` untuk event); `LoadSession` JOIN `users` (status).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah Step 2 → PASS; `make sqlc-check` bersih.
- [ ] **Step 5: Commit** — `git add internal db && git commit -m "feat(evaluation): repository ports, postgres implementation, fake store and contract suite"`

### Task 12: (dihapus dari v1)

Dihapus dari v1 (pronunciation ditunda ke pasca-v1).

### Task 13: `evaluation.Service` — orkestrasi §10.5 dan kegagalan

**Files:**
- Create: `internal/evaluation/service.go`
- Test: `internal/evaluation/service_test.go` (`package evaluation_test`)

**Interfaces:**
- Consumes: Task 2–12, `events.NewEvaluationCompleted/NewEvaluationFailed` (signature M6), `events.ProducerWorker`, `prompt.Render`, `clock.Clock`, `idgen.Generator`.
- Produces: `type ContentReader interface{...}` (literal interface Task 1); `type Router interface{Evaluation() provider.EvaluationProvider}` (`provider.AsyncRouter` memenuhi); `type Deps struct{Store Store; Source Source; Content ContentReader; Router Router; Cfg Config; Clock clock.Clock; IDs idgen.Generator; Metrics *Metrics; Schemas *SchemaCache; Log *slog.Logger}`; `func NewService(d Deps) *Service`; `func (s *Service) Process(ctx context.Context, env events.Envelope, p events.SessionCompletedV1) error`; `func (s *Service) RecordFailure(ctx context.Context, env events.Envelope, cause error, final bool)`; `func FailureCodeOf(err error) FailureCode`.

- [ ] **Step 1: Write the failing test** — fake: `fakes.NewEvaluationStore`, `memSource`, `memContent`, `scriptedEval` (Task 9), `fakes.Outbox`, fake clock/idgen. Kasus (`TestProcess…`): **Normal** → status `COMPLETED`, 5 skor + overall (rumus Task 2), kolom R-13 terisi (`listening_response_signal`, `interaction_complexity_signal` dari `signals.*.score`; `dimension_confidence` memuat 5 dimensi + 2 sinyal), `llm_call_count 1`, `rubric_version "rubric-v1"`, `prompt_version_id`, `provider/model`, `source_event_id = env.EventID`, findings ter-ordinal 1..n (urut MAJOR→MINOR→INFO, STRENGTH terakhir, lalu `turn_seq`, lalu dimensi) dengan `session_turn_id` dipetakan dari `turn_seq`; tepat satu event `evaluation.completed` (`aggregate EVALUATION`, `finding_counts` hanya non-STRENGTH). **Insufficient** (2 turn eligible): `COMPLETED`, `insufficient_evidence=true`, semua skor NULL, `summary_feedback == InsufficientSummary`, router TIDAK dipanggil (route yang panik bila dipanggil), event tetap terbit. **User DELETED** → hanya `MarkProcessed` lalu mengembalikan mekanisme M6 `skipped_deleted_user` (Classify = Ack, label metrik `skipped_deleted_user`); sesi bukan `COMPLETED|FAILED` / `started_at` NULL → hanya `MarkProcessed`, tanpa baris evaluasi. `sessions.user_id ≠ env.UserID` → `events.PermanentError{Code:"INVARIANT_VIOLATION"}`; sesi tidak ada → `ENTITY_NOT_FOUND`. Prompt tidak ada / `UserTemplate` nil / `OutputSchema` nil atau tak ter-compile → `CONFIGURATION_ERROR` **sebelum** claim (tidak ada baris). Claim mengembalikan sudah `COMPLETED` → `MarkProcessed` saja. Turn kosong padahal `p.UserTurnCount>0` → `TRANSCRIPT_UNAVAILABLE`. `MarkProcessed` mengembalikan `ErrAlreadyProcessed` → `Process` mengembalikannya dan tidak ada perubahan ter-commit. LLM error → dikembalikan (baris tetap `RUNNING`). **Latensi:** `ObserveLatency(completed_at − ended_at)`. **Failure:** `RecordFailure(final=false)` → `PENDING` + `last_error` dipotong 2.000; `final=true` → `FAILED` + `failure_code` + tepat satu `evaluation.failed` (`attempt_count`), tanpa `MarkProcessed`; pemanggilan `final=true` kedua tidak menerbitkan event lagi. `TestFailureCodeOf`: `PermanentError{TRANSCRIPT_UNAVAILABLE|CONFIGURATION_ERROR}` → sama; `ENTITY_NOT_FOUND`/`INVARIANT_VIOLATION` → `INTERNAL_ERROR`; `*provider.Error` → `PROVIDER_UNAVAILABLE`; `ErrInvalidModelOutput` → `INVALID_MODEL_OUTPUT`; lainnya → `INTERNAL_ERROR`. Test span (`NewTraceRecorder`): `evaluation.Process` ada dan error internal berstatus Error.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ -race -run 'TestProcess|TestFailureCodeOf|TestRecordFailure'`. Expected: FAIL.
- [ ] **Step 3: Implement** — urutan §10.5 langkah 1–12 persis: load sesi/user → cek DELETED/status → load prompt via `Content.GetPublishedPromptVersion("EVALUATION")` → `Claim` (tx singkat) → load turns+konten → statistik → bila insufficient lompat ke tx final → panggilan LLM → normalisasi findings → overall → tx final (`MarkProcessed` pertama, `Complete`, `ReplaceFindings`, `Outbox().Enqueue`); event dibangun `events.NewEvaluationCompleted(s.ids.New(), now, events.ProducerWorker, userID, events.EvaluationCompletedV1{…})` (payload §9.5.2; `Scores` 5 key dengan `nil` untuk skor null; `Signals` `*EvalSignal`; `FindingCounts` hanya non-STRENGTH) dan `events.NewEvaluationFailed(…, events.EvaluationFailedV1{EvaluationID, SessionID, FailureCode, AttemptCount, FailedAt})`. `from/to` batas `created_at` = `starting_at−1m` .. `ended_at+1m`. Transkrip memakai `Cfg.TranscriptMaxChars`; `ExpectedFormality` dari `SessionInfo.AdaptiveDifficulty` (fallback `scenario_versions.formality`); `CharacterName` dari `SessionInfo.ConfigSnapshot` (`character.name`, D-014; tanpa baca tabel `characters`).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/... -race` → PASS (coverage paket ≥ 85 %: `go test ./internal/evaluation/ -race -cover`).
- [ ] **Step 5: Commit** — `git add internal/evaluation && git commit -m "feat(evaluation): service orchestration, claim, failure handling"`

### Task 14: `evaluation.Consumer`

**Files:**
- Create: `internal/evaluation/consumer.go`
- Test: `internal/evaluation/consumer_test.go`

**Interfaces:**
- Consumes: `evaluation.Service`, `events.Delivery/Permanent/QueueEvaluation`.
- Produces: `func NewConsumer(s *Service, cfg Config) *Consumer` dengan method struktural §9.7.1: `Queue() string` (`events.QueueEvaluation`), `SupportedEvents() map[string]int` (`{"session.completed": 1}`), `Timeout() time.Duration` (`cfg.HandlerTimeout`), `Handle(ctx, d events.Delivery) error` (payload via `events.DecodePayload[events.SessionCompletedV1](d.Envelope)`; gagal → `Permanent("DECODE_ERROR")` dari M6), `OnFailure(ctx, d events.Delivery, cause error, final bool)`.

- [ ] **Step 1: Write the failing test** — `TestConsumerContract` (`Queue()=="evaluation.session_completed"`, `SupportedEvents()["session.completed"]==1`, `Timeout()==200*time.Second`); `TestHandleDecodesPayload` (payload valid → `Service.Process` menerima `SessionID`; payload JSON rusak → `*events.PermanentError{Code:"DECODE_ERROR"}`); `TestOnFailureDelegates` (`final` diteruskan ke `RecordFailure`; `d.Envelope` utuh).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ -race -run 'TestConsumer|TestHandleDecodes|TestOnFailure'`. Expected: FAIL.
- [ ] **Step 3: Implement** — tipis; paket tidak mengimpor `amqp`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/evaluation && git commit -m "feat(evaluation): queue consumer"`

### Task 15: Integration — consumer + Postgres + fake provider

**Files:**
- Create: `internal/testutil/finished_session.go` (tag `integration || e2e`), `test/integration/m7_evaluation_consumer_test.go`
- Test: `test/integration/m7_evaluation_consumer_test.go` (`integration`)

**Interfaces:**
- Consumes: `evaluation.NewService/NewConsumer`, `postgres.Store.EvaluationStore()`, `postgres.NewEvaluationSource`, `testutil.ApplySeed`, `fake.NewEvaluation`, `testutil.NewPostgres`.
- Produces: `type FinishedSessionOpts struct{UserStatus string; Status string; Turns []TurnSeed; StartedSecondsAgo, ActiveSeconds int}`, `type TurnSeed struct{UserText, AssistantText string; Status string; UserSeconds float64}`, `type FinishedSession struct{ID, UserID uuid.UUID; Envelope events.Envelope; Payload events.SessionCompletedV1; TurnIDs []uuid.UUID}`, `func SeedFinishedSession(t testing.TB, pool *pgxpool.Pool, o FinishedSessionOpts) FinishedSession` (memakai `ApplySeed` + `SeedUser/SeedSession`, mengisi `adaptive_difficulty {"formality":"POLITE",…}` dan `config_snapshot` (karakter + formalitas, D-014)).

- [ ] **Step 1: Write the failing test** — `TestEvaluationConsumer_*` (provider: `scriptedEval` membungkus `fake.NewEvaluation` untuk kasus normal, skrip untuk kasus lain): (a) `Completed` — 5 user turn ≥ 25 s, sesi 120 s → `evaluations.status COMPLETED`, 4 kolom R-13 terisi, findings tersimpan dengan `session_turn_id` valid, tepat 1 baris `event_outbox` `evaluation.completed` (`aggregate_type EVALUATION`, payload `scores`/`signals`/`finding_counts`/`overall_score`), 1 baris `processed_events(evaluation.session_completed, event_id)`; (b) `DuplicateSequential` dan `DuplicateParallel` (dua goroutine `Handle` event sama) → 1 evaluasi + 1 outbox (guard `MarkProcessed`/`status<>'COMPLETED'`; salah satu mengembalikan `ErrAlreadyProcessed`); (c) `RepairOnce` (skrip: call 1 invalid) → `llm_call_count 2` (maks 2); (d) `ProviderErrorNoFallback` (`UNAVAILABLE` pada panggilan utama) → error dikembalikan, baris tetap `RUNNING`, tidak ada panggilan ke-2; (e) `InvalidEverywhere` — jalankan siklus framework: `Handle` ×5 dengan `RetryCount 0..4`, `RecordFailure(final=false)` pada 4 pertama (status `PENDING`) dan `final=true` pada kelima → `FAILED`, `failure_code INVALID_MODEL_OUTPUT`, `attempt_count 5`, 1 outbox `evaluation.failed`, **tidak** ada `processed_events`; lalu `ReplayAfterFailed` (`Handle` lagi dengan provider valid) → `COMPLETED`, `attempt_count 6`, 1 `evaluation.completed`; (f) `DeletedUser` → tidak ada baris `evaluations`/outbox, `processed_events` ada, provider tidak dipanggil; (g) `Insufficient` (2 turn) → COMPLETED + `insufficient_evidence`, LLM tidak dipanggil, outbox terbit; (h) `FailedSessionStillEvaluated` (`sessions.status FAILED`, `started_at` ada) → COMPLETED; (i) `TranscriptUnavailable` (payload `user_turn_count 3` tapi turn nol) → `Permanent TRANSCRIPT_UNAVAILABLE` → `RecordFailure(final=true)` → `FAILED`.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration -race -run TestEvaluationConsumer`. Expected: FAIL (helper belum ada).
- [ ] **Step 3: Implement** — helper `SeedFinishedSession` (INSERT lewat `testutil.SeedSession` + SQL turns; membangun `Envelope` dengan `idgen`); tidak ada perubahan kode produksi selain perbaikan bug yang ditemukan.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git add internal/testutil test && git commit -m "test(evaluation): consumer integration scenarios with postgres and fake providers"`

### Task 16: `evaluation.QueryService` dan REST `GET /sessions/{id}/evaluation`

**Files:**
- Create: `internal/evaluation/query.go`, `internal/httpapi/evaluation_routes.go`, `evaluation_handler.go`, `evaluation_dto.go`, `testdata/api/rest/evaluation_{completed,running,pending_synth,failed,insufficient,purged,not_ended,not_available}.json`
- Modify: `internal/httpapi/router.go`, `internal/httpapi/deps.go` (`Deps.Evaluation *evaluation.QueryService`), `TestRouteTable` (tambah `GET /api/v1/sessions/{session_id}/evaluation` grup User)
- Test: `internal/evaluation/query_test.go`, `internal/httpapi/evaluation_handler_test.go`

**Interfaces:**
- Consumes: `session.QueryService.Get(ctx, userID, id) (session.View, error)` (404 `SESSION_NOT_FOUND` untuk user lain), `evaluation.Reader`, `httpapi.HandleNoBody`, `Request.SetHeader(k, v)` (M2), `errs.CodeSessionNotEnded/CodeEvaluationNotAvailable`.
- Produces: `type SessionRef struct{ID uuid.UUID; Status string; StartedAt *time.Time; TranscriptPurged bool}`; `type View struct{SessionID uuid.UUID; Status Status; Eval *Evaluation; Findings []FindingRow; TranscriptPurged bool}`; `func NewQueryService(r Reader) *QueryService`; `func (q *QueryService) Get(ctx context.Context, ref SessionRef) (View, error)`; `func registerEvaluationRoutes(user chi.Router, h *evaluationHandler)`; interface kecil di `httpapi`: `type evaluationSessionGetter interface{Get(ctx, userID, id uuid.UUID) (session.View, error)}`, `type evaluationQuery interface{Get(ctx, evaluation.SessionRef) (evaluation.View, error)}`.

- [ ] **Step 1: Write the failing test** — `query_test` (fake `Reader`) tabel matriks §10.11 berurutan: `RESERVED|STARTING|ACTIVE|ENDING` → `errs.CodeOf == errs.CodeSessionNotEnded` dan `DetailsOf == {"session_status": "<status>"}`; `CANCELLED|ABANDONED|FAILED` dengan `StartedAt nil` → `errs.CodeEvaluationNotAvailable`; `COMPLETED` tanpa baris → `View.Status == StatusPending`, `Eval nil`; `FAILED` yang `StartedAt` ada tanpa baris → `PENDING`; baris `RUNNING`/`PENDING`/`COMPLETED`/`FAILED` dikembalikan apa adanya. `evaluation_handler_test` (golden `-update`, `httptest`, fake service): `200` COMPLETED (5 elemen `dimensions` urut `GRAMMAR…TASK_ACHIEVEMENT`; `overall_score 52.4`; `findings` urut `ordinal` tanpa `provider/model/confidence/pattern_key/signals/last_error`; `created_at/completed_at` RFC 3339 milidetik); `insufficient_evidence=true` → semua skor null, `summary == InsufficientSummary`, array kosong; `details_purged_at` terisi → `details_purged true`, `summary/comments null`, `strengths/focus_areas/recommendations/findings` kosong, skor tetap; `TranscriptPurged` → `findings[].turn_id null`; `PENDING` (disintesis: `evaluation_id null`) dan `RUNNING` → header `Retry-After: 3`, array kosong, `completed_at null`; `FAILED` → `failure_message` persis kalimat Global Constraints dan tanpa `Retry-After`; `COMPLETED` tanpa `Retry-After`; `409 SESSION_NOT_ENDED` dan `404 EVALUATION_NOT_AVAILABLE` dengan body error format D-013 (`message` Indonesia tetap); sesi user lain/tidak ada/ID bukan UUID → `404 SESSION_NOT_FOUND`; tanpa Bearer → 401 (via router grup User).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/ ./internal/httpapi/ -race -run 'Query|Evaluation|RouteTable'`. Expected: FAIL.
- [ ] **Step 3: Implement** — handler: `ref` dari `session.View` (`Status`, `StartedAt`, `TranscriptPurgedAt != nil`); DTO terpisah dari domain (`evaluation_dto.go`) dan handler memanggil `req.SetHeader("Retry-After", "3")` untuk status `PENDING|RUNNING` (tanpa `Metaer`; efek ditulis pada jalur sukses); skor `overall_score` dari `NUMERIC(5,2)` dibulatkan 2 desimal; tidak ada JOIN ke `session_turns`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/ ./internal/httpapi/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal testdata && git commit -m "feat(api): GET /sessions/{id}/evaluation with R-12 matrix"`

### Task 17: Paket `summary` — domain murni, `SummaryPromptData`, seed SUMMARY

**Files:**
- Create: `internal/summary/prompt.go`, `internal/summary/transcript.go`, `internal/summary/config.go`
- Modify: `seed/0070_prompts.sql` (tambah version SUMMARY), `test/integration/m3_seed_test.go` (`expectedPromptVersionCount` 3 → 4 dan `prompt.RegisterFixtures("SUMMARY", summary.PromptFixtures)`; langkah eksplisit di Step 1 dan Step 3), `test/integration/m7_seed_test.go`
- Test: `internal/summary/{prompt,transcript}_test.go`, `test/integration/m7_seed_test.go`

**Interfaces:**
- Produces: `type SummaryPromptData struct{MaxChars int; TopicName, ScenarioName string; UserTurnCount int; Transcript string}`; `func PromptFixtures() []any` (minimal: transcript 1 baris; maksimal: 30.000 karakter); `type TurnRow struct{Seq int; Status *string; UserText, AssistantText, AssistantSpokenText *string}` (nama dari plan); `func BuildTranscript(turns []TurnRow, maxChars int) string`; `func CleanSummary(text string, maxChars int) (string, error)` + `var ErrEmptySummary = errors.New("summary: empty output")`; `type Config struct{MaxChars int}` (struct biasa; env `SUMMARY_MAX_CHARS`=300 dibaca `config.Worker.Summary`, Task 2) + `func DefaultConfig() Config`; prompt_versions id `01920000-000d-7000-8000-000000000003`.

- [ ] **Step 1: Write the failing test** — `TestBuildTranscript`: baris `ユーザー: …` / `AI: …` urut `sequence_number`; AI memakai `assistant_spoken_text` lalu `assistant_text`; `<`→`＜`, `>`→`＞`; turn `DISCARDED` dan user_text NULL tidak dirender; > 30.000 karakter → setiap baris AI dipotong 150 rune. `TestCleanSummary`: trim; kosong/hanya spasi → `ErrEmptySummary`; 350 rune dengan `。` terakhir di rune ke-290 → dipotong tepat setelah `。` (≤ 300); tanpa `。` ≤ 300 → hard-cut 300; ≤ 300 utuh. `TestPromptFixtures` (2 elemen, maks 30.000). Integration `TestSummarySeedPrompt`: version 1 `PUBLISHED`, `OutputSchema == nil`, `LLMParams {Temperature 0.3, MaxOutputTokens 400}`, render kedua fixture sukses, System memuat `300文字以内` untuk `MaxChars=300`, `ValidateCatalog` tanpa ERROR; di `m3_seed_test.go` ubah `expectedPromptVersionCount` ke 4 dan daftarkan `prompt.RegisterFixtures("SUMMARY", summary.PromptFixtures)`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/summary/ -race` dan `go test -tags integration ./test/integration -race -run TestSummarySeedPrompt`. Expected: FAIL.
- [ ] **Step 3: Implement** — pastikan perubahan `m3_seed_test.go` (konstanta=4 + `RegisterFixtures`) ada; seed SUMMARY: `system_template` dan `user_template` **verbatim §9.9.6** (dollar-quoting), `output_schema NULL`, `llm_params {"temperature":0.3,"max_output_tokens":400}`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `go test -tags integration ./test/integration -race -run TestSeed` PASS.
- [ ] **Step 5: Commit** — `git add internal/summary seed test && git commit -m "feat(summary): prompt data, transcript, output cleaning, SUMMARY seed"`

### Task 18: `summary.Service`, `Consumer`, store Postgres, integration

**Files:**
- Modify: `internal/postgres/store.go` (tambah `SummaryStore()`)
- Create: `internal/summary/repository.go`, `service.go`, `consumer.go`, `internal/postgres/summary_repo.go`, `db/queries/summary.sql`, `test/integration/m7_summary_test.go`
- Test: `internal/summary/service_test.go`, `internal/summary/consumer_test.go`, `test/integration/m7_summary_test.go` (`integration`)

**Interfaces:**
- Consumes: `provider.AsyncRouter.CompleteText`, `prompt.Render`, `events.*`.
- Produces: `type SessionInfo struct{ID, UserID uuid.UUID; UserStatus, TopicName, ScenarioName string; StartingAt, EndedAt *time.Time}`; `type Source interface{LoadSession(ctx, id uuid.UUID) (SessionInfo, error); LoadTurns(ctx, id uuid.UUID, from, to time.Time) ([]TurnRow, error)}`; `type Prompt struct{ID uuid.UUID; Template prompt.Template}`; `type PromptSource interface{PublishedPrompt(ctx context.Context) (Prompt, error)}`; `type SessionWriter interface{SetSummary(ctx, id uuid.UUID, text string) (bool, error)}`; `type Tx interface{Processed() events.ProcessedEvents; Sessions() SessionWriter}`; `type Store interface{WithinTx(ctx, fn func(ctx, tx Tx) error) error}`; `type TextRouter interface{CompleteText(ctx, provider.TextRequest) (provider.TextResult, error)}`; `type Deps struct{Store; Source; Prompts PromptSource; Router TextRouter; Cfg Config; Log *slog.Logger}`; `func NewService(d Deps) *Service`; `(*Service).Process(ctx, env events.Envelope, p events.SessionCompletedV1) error`; `func NewConsumer(s *Service) *Consumer` (`Queue()=events.QueueSummary`, `SupportedEvents {"session.completed":1}`, `Timeout()=60s`, `Handle`, `OnFailure` = log + counter `kotoki_summary_failures_total{reason}`); `(*postgres.Store).SummaryStore()` (di `internal/postgres/store.go`), `postgres.NewSummarySource(pool)`, `postgres.NewSummaryPromptSource(pool)`.

- [ ] **Step 1: Write the failing test** — unit: `TestProcess` tabel — normal → `TextRequest{Task:"SUMMARY", RequestID:"SUMMARY:<sid>", MaxTokens: 400, Temperature: 0.3}` bila `LLMParams` kosong, nilai DB menang bila ada (`MaxOutputTokens 250` → 250), pesan `[system, user]`, `SetSummary` dipanggil dengan teks bersih; `user_turn_count==0` → hanya `MarkProcessed`, router tidak dipanggil; user `DELETED` → hanya `MarkProcessed` + mekanisme `skipped_deleted_user` M6; prompt tidak ada atau `UserTemplate` kosong → `Permanent("CONFIGURATION_ERROR")`; render error → `CONFIGURATION_ERROR`; output kosong → error retryable (bukan Permanent; tidak `MarkProcessed`); `SetSummary` 0 baris (summary sudah ada) → tetap `MarkProcessed` dan sukses; `ErrAlreadyProcessed` dari `MarkProcessed` dikembalikan. `TestConsumerContract` (queue `summary.session_completed`, timeout 60 s, `DECODE_ERROR`). Integration: `TestSummaryConsumer_Normal` (fake LLM → `sessions.summary_text` terisi ≤ 300 rune, `sessions.version` tidak berubah, `processed_events` ada), `TestSummary_ZeroUserTurns_StaysNull`, `TestSummary_DeletedUser`, `TestSummary_DuplicateParallel` (satu update), `TestSummary_ExistingSummaryUntouched`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/summary/ -race` dan `go test -tags integration ./test/integration -race -run 'TestSummary(Consumer|_)'`. Expected: FAIL.
- [ ] **Step 3: Implement** — alur §9.9.6 langkah 1–8 (transcript dari `LoadTurns` dengan batas `created_at`; `SetSummary` = `UPDATE sessions SET summary_text=$2, updated_at=now() WHERE id=$1 AND summary_text IS NULL`); keputusan: summary memakai port `summary.Tx` (§9.9.6) dan **bukan** `session.Service.SetSummary` (§5.2) — catat di `docs/implementation-deviations.md`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `go test ./internal/summary/ -race -cover` ≥ 85 %.
- [ ] **Step 5: Commit** — `git add internal db test && git commit -m "feat(summary): summary worker, store and integration"`

### Task 19: Wiring `cmd/api` + `cmd/worker`, pipeline end-to-end (RabbitMQ)

**Files:**
- Modify: `cmd/api/main.go` (`Deps.Evaluation`; `prompt.RegisterFixtures("EVALUATION"/"SUMMARY", …)`), `cmd/worker/wiring.go` (`buildConsumers`: case `evaluation` → `amqp.NewConsumer(evaluation.NewConsumer(evaluation.NewService(...)), conn, pub, processed, m, log, clk)`, case `summary` serupa; pemetaan `config.Worker.Evaluation/Summary` → `evaluation.Config`/`summary.Config` dilakukan di `cmd/worker` (platform/config tidak mengimpor paket fitur); router dari M5 T25 `cmd/internal/providerwire.BuildFake(cfg, caps, deps)` dengan `Role: provider.RoleWorker` (mode fake; mode real = M9) — tambah field di `consumerDeps`: `Router provider.AsyncRouter`, `Content content.Repository`, `EvalCfg evaluation.Config`, `SummaryCfg summary.Config`), `cmd/worker/handlers_test.go` (ubah `TestBuildConsumers_UnimplementedHandlerFailsStartup`: hanya `memory`/`learner` yang tetap `errHandlerNotImplemented`, `evaluation`/`summary` kini sukses; **M8 menghapus test ini** setelah kedua handler terakhir terpasang), `.env.example` (`EVAL_*`, `SUMMARY_MAX_CHARS`), `docs/implementation-log.md`
- Create: `test/integration/m7_pipeline_test.go`
- Test: `test/integration/m7_pipeline_test.go` (`integration`), `cmd/worker/handlers_test.go`

**Interfaces:**
- Consumes: M6 `amqp.Dial`, `NewConfirmPublisher`, `OutboxPublisher.PublishBatch`, `NewConsumer(...).Run`, `testutil.NewRabbitMQ/RabbitAdmin`, router + token helper dari `test/integration/m3_lifecycle_test.go` (bila privat: ekspor ke `internal/testutil`, catat di log).

- [ ] **Step 1: Write the failing test** — `TestPipeline_SessionCompletedToEvaluationAndSummary` (mode fake): `SeedFinishedSession` (5 turn eligible, ≥ 25 s ucapan, 120 s aktif) → `events.NewSessionCompleted` ke outbox dalam tx → jalankan `OutboxPublisher` + consumer `evaluation` dan `summary` → `require.Eventually` (≤ 30 s): `evaluations.status COMPLETED` dengan finding dari fake, `sessions.summary_text` non-NULL, outbox `evaluation.completed` `PUBLISHED` (dan pesan tiba di `kotoki.events.unrouted`/`learner.evaluation_completed` sesuai topologi), `processed_events` berisi dua consumer, kedua queue utama dan DLQ kosong (`RabbitAdmin.QueueMessages`). `TestPipeline_EvaluationAPIThreeStates` (router nyata + Bearer): sesi `ACTIVE` → `409 SESSION_NOT_ENDED`; sesi `CANCELLED` → `404 EVALUATION_NOT_AVAILABLE`; sesi `COMPLETED` tanpa baris (consumer belum jalan) → `200 {"status":"PENDING"}` + `Retry-After: 3`; setelah pipeline → `200 COMPLETED` dengan `rubric_version "rubric-v1"`; user B memanggil sesi user A → `404 SESSION_NOT_FOUND`. `TestPipeline_PermanentErrorGoesToDLQ`: payload `user_turn_count 3` tanpa turn → `TRANSCRIPT_UNAVAILABLE` → 1 pesan di `evaluation.session_completed.dlq` (`x-error-class TRANSCRIPT_UNAVAILABLE`), `evaluations.status FAILED`, outbox `evaluation.failed` → `kotoki.events.unrouted`. `TestBuildConsumers_EvaluationAndSummary` (unit `cmd/worker`: flag `evaluation,summary` → 2 consumer; handler `Queue()` benar). `TestWorker_RunEvaluationFlags` (`--handlers=evaluation,summary` → `/readyz` 200). `make arch-check`: `CGO_ENABLED=0 go build ./cmd/api ./cmd/worker` bersih.
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/worker/ -race -run TestBuildConsumers` dan `go test -tags integration ./test/integration ./cmd/worker -race -run 'TestPipeline|TestWorker_RunEvaluationFlags'`. Expected: FAIL.
- [ ] **Step 3: Implement** — wiring manual D-004; tidak ada logika baru di luar wiring dan perbaikan bug yang ditemukan test.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make arch-check` exit 0.
- [ ] **Step 5: Commit** — `git add cmd internal test docs .env.example && git commit -m "feat(worker,api): wire evaluation and summary, pipeline integration test"`

### Task 20: Golden set — metrik kalibrasi (paket murni)

**Files:**
- Create: `internal/evaluation/golden/metrics.go`
- Test: `internal/evaluation/golden/metrics_test.go`

**Interfaces:**
- Produces: `func Band(score float64) int` (0–19→0, 20–39→1, 40–59→2, 60–79→3, 80–100→4); `func MAE(model, gold []float64) float64`; `func QWK(a, b []int, bands int) float64`; `func Spearman(x, y []float64) float64` (rank rata-rata untuk seri); `func StdDev(xs []float64) float64` (populasi); `func StrictlyIncreasing(means []float64) bool`; `func Recall(found, total int) float64`.

- [ ] **Step 1: Write the failing test** — `Band` pada 0/19/19.9/20/79/80/100; `MAE([50,60],[40,70])==10`; `QWK` identik → 1.0, `a=[0,1,2,3,4]`,`b=[4,3,2,1,0]` → −1.0 (±1e-9), kasus tangan 3×3 dengan nilai yang dihitung manual dari rumus (kuadrat bobot `(i−j)²/(k−1)²`); `Spearman([1,2,3],[1,2,3])==1`, dengan seri `[1,1,2]` vs `[1,2,3]` → nilai tangan (≈ 0.866); `StdDev([2,4,4,4,5,5,7,9])==2`; `StrictlyIncreasing([1,2,3])` true, `[1,2,2]` false; `Recall(0,0)==0`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/golden/ -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — hanya stdlib; pembagian nol → 0.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/evaluation/golden/ -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/evaluation/golden && git commit -m "feat(golden): calibration metrics (MAE, QWK, Spearman, stability)"`

### Task 21: Golden set — dataset, runner, report, `kotoki-cli eval-golden`

**Files:**
- Create: `internal/evaluation/golden/{dataset,runner,report}.go`, `cmd/kotoki-cli/eval_golden.go`, `testdata/benchmark/evaluation/rubric-v1/manifest.json`, `testdata/benchmark/evaluation/rubric-v1/example.jsonl` (9 sampel sintetis: 2 per level `BEGINNER|ELEMENTARY|INTERMEDIATE|ADVANCED` + 1 insufficient), `test/integration/m7_golden_test.go`
- Modify: dispatcher subcommand `cmd/kotoki-cli/main.go` milik M5 Task 28 (tambah `case "eval-golden"`), `cmd/kotoki-cli/cli_test.go` (tambah `TestEvaluationGetPrintsStatus`: server `httptest` mengembalikan JSON `PENDING` → `kotoki-cli evaluation get <session_id>` exit 0 dan mencetak `PENDING`; subcommand `evaluation get` sudah dibuat M5, diuji di sini)
- Test: `internal/evaluation/golden/{dataset,runner,report}_test.go`, `test/integration/m7_golden_test.go` (`integration`)

**Interfaces:**
- Consumes: Task 20, `evaluation.LLMEvaluator/BuildPromptData/BuildTranscript/RubricVersionOf`, `content.Repository.GetPublishedPromptVersion`.
- Produces: `type Manifest struct{RubricVersion, Status string; Expected struct{Samples, PerLevel, Insufficient, MinScenarioKinds int}}` (`Status` `EXAMPLE|COMPLETE`); `type Sample struct{ID, Level string; Insufficient bool; StartMode string; Scenario ScenarioInput; Turns []evaluation.TurnRow; Gold Gold}`; `type Gold struct{Rater1, Rater2, Rater3 map[string]int; MajorErrors []MajorError}` + `func (g Gold) Score(dim string) (float64, error)` (rata-rata rater1/2; selisih > 20 tanpa `Rater3` → `ErrAdjudicationMissing`; dengan rater3: rata-rata dua rater terdekat — keputusan plan karena §10.12 tidak merinci); `func LoadDataset(dir string) (Dataset, error)`; `func (d Dataset) CheckComplete() []string` (pelanggaran: ≠ 80 sampel non-insufficient, ≠ 20/level, < 4 jenis skenario per level, tidak ada kedua `StartMode`, ≠ 8 insufficient); `type Evaluator func(ctx context.Context, s Sample, run int) (RunResult, error)` (`RunResult{Scores map[string]int; Findings []FindingKey; FirstCallValid bool}`); `func Run(ctx, ds Dataset, ev Evaluator, runs int) (Report, error)`; `type Report struct{DatasetStatus string; Metrics map[string]MetricResult; Verdict string}` (`PASS|FAIL|NOT_EVALUABLE_EXAMPLE_DATASET|PENDING_MANUAL_METRICS`); `func ExportFindingsForRating(r Report, n int, seed int64) []RatingItem`; `func PrecisionFromRatings(valid []bool) float64`; `func Compare(cur, baseline Report) []string` (MAE memburuk > 2 poin di dimensi mana pun); `func Main(args []string, stdout, stderr io.Writer, deps MainDeps) int` (flag `--dataset --runs 3 --out report.json --baseline --export-findings`); kriteria §10.12: MAE ≤ 10, QWK ≥ 0.60, Spearman ≥ 0.70 per dimensi, monotonic mean overall per level, stabilitas rata-rata ≤ 5, recall MAJOR ≥ 0.70, validitas panggilan pertama ≥ 0.95, presisi ≥ 0.80 (manual) berstatus `PENDING_MANUAL` bila belum ada data, agreement antar-rater (QWK rater1 vs rater2) hanya dilaporkan.

- [ ] **Step 1: Write the failing test** — unit: `TestLoadDataset_Example` (9 sampel, manifest `EXAMPLE`); `TestGoldScore` (r1=60,r2=70 → 65; r1=40,r2=70 tanpa rater3 → `ErrAdjudicationMissing`; dengan rater3=65 → rata-rata dua rater terdekat = 67.5 [70,65]); `TestCheckComplete_ExampleHasViolations` (≠ 80 sampel dst. → daftar tidak kosong); `TestRun_WithPerfectEvaluator` (evaluator mengembalikan skor gold → MAE 0, QWK 1, verdict `NOT_EVALUABLE_EXAMPLE_DATASET` karena status `EXAMPLE`); dataset sintetis `COMPLETE` buatan test (80+8 sampel dibangkitkan di test) dengan evaluator sempurna → semua metrik lulus, verdict `PENDING_MANUAL_METRICS` (presisi belum ada) dan `PASS` setelah `ratings` diberikan; `TestRun_MAERegressionVsBaseline` (`Compare` melaporkan dimensi `GRAMMAR` bila MAE naik 2.5); `TestRecallMajor` (gold `(turn_seq=2, type=PARTICLE_ERROR)` cocok dengan finding tipe sama atau dimensi sama pada turn sama); `TestExportFindingsForRating` (100 item acak deterministik untuk `seed`, tanpa teks transkrip lengkap selain `original/corrected`). Integration `TestEvalGoldenExampleDataset`: `ApplySeed`, evaluator nyata atas `scriptedEval`/`fake.NewEvaluation`, jalankan `golden.Main` dengan `--dataset testdata/benchmark/evaluation/rubric-v1 --runs 3 --out <tmp>` → exit 0, `report.json` valid berisi 5 `MAE` dimensi, `dataset_status "EXAMPLE"`, `verdict "NOT_EVALUABLE_EXAMPLE_DATASET"`. CLI: `TestEvaluationGetPrintsStatus` (lihat Files).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/evaluation/golden/ ./cmd/kotoki-cli/ -race` dan `go test -tags integration ./test/integration -race -run TestEvalGolden`. Expected: FAIL.
- [ ] **Step 3: Implement** — runner membangun prompt persis seperti worker (`BuildTranscript`, `BuildPromptData`, `prompt.Render` dengan version PUBLISHED dari DB via `DATABASE_URL`) dan memanggil `LLMEvaluator.Evaluate`; `FirstCallValid = (CallCount==1)`; `cmd/kotoki-cli/eval_golden.go` hanya memanggil `golden.Main` dengan router dari konfigurasi (`PROVIDER_MODE`); manifest dan contoh JSONL ditulis tangan (data sintetis, bukan label rater sungguhan); **dataset 80+8 sampel dengan 2 rater native adalah prasyarat manual** yang wajib selesai sebelum mem-publish prompt EVALUATION baru atau sebelum M9 menetapkan model evaluasi, dan tidak dikerjakan agent.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `DATABASE_URL=<compose> PROVIDER_MODE=fake go run ./cmd/kotoki-cli eval-golden --dataset testdata/benchmark/evaluation/rubric-v1 --runs 3 --out /tmp/report.json` → exit 0, `verdict` `NOT_EVALUABLE_EXAMPLE_DATASET`.
- [ ] **Step 5: Commit** — `git add internal/evaluation/golden cmd/kotoki-cli testdata test && git commit -m "feat(golden): dataset loader, runner, report and kotoki-cli eval-golden"`

---

## Gerbang milestone

- [ ] `go build ./... && CGO_ENABLED=0 go build ./cmd/api ./cmd/worker` → sukses; `make arch-check` → exit 0 (paket `evaluation`/`summary` tidak mengimpor `amqp`, `pgx`, `chi`, ``learner`, `session`).
- [ ] `make lint` → 0 issue; `make sqlc-check` → bersih.
- [ ] `go test ./internal/evaluation/... ./internal/summary/... ./internal/httpapi/... ./internal/provider/... ./internal/prompt/... -race` → PASS; coverage `evaluation` dan `summary` ≥ 85 % (`go test ./internal/evaluation/... ./internal/summary/... -race -coverprofile=cover.out` + `go run ./test/tools/covercheck -profile cover.out -thresholds test/tools/covercheck/thresholds.txt`).
- [ ] `make test-integration` → PASS, termasuk `TestEvaluationSeedPrompt`, `TestSummarySeedPrompt`, `TestEvaluationConsumer_*` (COMPLETED + findings + 4 kolom R-13 + `evaluation.completed` di outbox; duplikat → satu evaluasi; repair → `llm_call_count=2`; provider error tanpa fallback → baris tetap `RUNNING`; lima percobaan → `FAILED` + `evaluation.failed`; replay → `COMPLETED`; user `DELETED` → tanpa baris), `TestSummaryConsumer_*`, `TestPipeline_*` (`session.completed` → evaluasi `COMPLETED` ujung-ke-ujung dengan fake provider, `summary_text` terisi, API tiga keadaan 409/404/200-PENDING lalu 200-COMPLETED, DLQ untuk error permanen), `TestEvalGoldenExampleDataset`.
- [ ] `DATABASE_URL=… PROVIDER_MODE=fake go run ./cmd/kotoki-cli eval-golden --dataset testdata/benchmark/evaluation/rubric-v1 --runs 3 --out /tmp/report.json` → exit 0 dan `report.json` ber-`verdict: NOT_EVALUABLE_EXAMPLE_DATASET`.
- [ ] Catat perintah + ringkasan keluaran di `docs/implementation-log.md`; penyimpangan di `docs/implementation-deviations.md` (lihat Self-Review). **Prasyarat manual (di luar gerbang kode):** golden set 80 + 8 sampel berlabel 2 rater (§10.12) dan `report.json` berstatus `COMPLETE` wajib ada sebelum prompt/model evaluasi dipakai di produksi; kriteria hasilnya diperiksa di M9/M10, bukan di sini.

## Self-Review

**Cakupan spec.** Tercakup: §10.1–§10.14 (enum, DDL dipakai dari M1, lifecycle, insufficient evidence, alur worker, panggilan LLM + repair (maks 2), prompt+schema+validator, publikasi event, retry/DLQ lewat kontrak §9.7, API R-12, kalibrasi sebagai tooling, env, test wajib), §9.9.6 (summary), §8.9.5/§10.13 (fake evaluasi), §6.13.4 (seed EVALUATION/SUMMARY), §5.9.6/§5.10 (kode `SESSION_NOT_ENDED`, `EVALUATION_NOT_AVAILABLE`). Tidak tercakup (milik milestone lain): job `transcript_retention`/`outbox_retention`/`worker dlq-replay` dan topologi/publisher/framework (M6), consumer learner `evaluation.completed` (M8), adapter OpenAI/DeepSeek evaluation nyata (M9), `docs/evaluation/rubric-changelog.md` (dibuat saat bump rubric pertama), pengisian dataset golden 80 sampel (manual).
**Konsistensi tipe.** `TurnRow` (Task 3) → `Source.LoadTurns`, `golden.Sample`; `Finding` (Task 8) dipakai `LLMResult`, `FindingRow`; `SchemaCache` (Task 7) → `LLMEvaluator` (Task 9) → `Service` (Task 13); `Router`/`ContentReader` didefinisikan di Task 13 dan dipenuhi `provider.AsyncRouter`/`content.Repository`; `evaluation.SessionRef/View` (Task 16) dikonsumsi handler dari `session.View` M3.
**Ambiguitas spec dan keputusan.** (1) §8.9.5 (fake berbasis fixture) vs §10.13 (finding turunan transcript): fixture + hook `RubricFindings` (Task 6). (2) Contoh transcript §10.6.2 tidak sesuai aturan teks `interrupted the AI`/`cut off`: aturan teks yang diikuti (Task 3). (3) §5.2 (`session.Service.SetSummary`) vs §9.9.6 (`summary.Tx`): port `summary.Tx` (Task 18). (4) §10.7.2 poin 3 "ditolak oleh aturan per-finding": hanya penolakan validitas yang dihitung, bukan confidence/duplikat/batas (Task 8). (5) Adjudikasi rater ketiga: rata-rata dua rater terdekat (Task 21). (6) Lokasi dataset: `testdata/benchmark/evaluation/rubric-v1/` (§2.2 + brief) menggantikan `test/golden/evaluation/` (§10.12).
