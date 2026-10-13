# Kaiwa Backend — M3 Content, Balance & Session Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sesi percakapan dapat dibuat (konten ter-resolve, saldo ter-reservasi secara idempotent), dijalankan lewat `session.RuntimeService` (tanpa runtime nyata), diakhiri/diselesaikan dengan settlement yang benar, dan dibaca lewat REST, dengan ledger yang lolos rekonsiliasi §4.8.
**Architecture:** Tiga paket fitur baru/dilengkapi: `content` (→ `prompt`), `balance`, `session` (→ `balance`, `content`, `events`); semua akses DB lewat port yang didefinisikan di sisi pemakai dan diimplementasikan `postgres` (`postgres.Store` satu `*Tx`). `session.Service` satu-satunya penulis `sessions.status`; transisi yang menyentuh saldo memakai satu transaksi dengan urutan lock §4.6.0. Redis hanya lewat port kecil (`HeartbeatReader`, `ControlPublisher`) agar paket fitur bebas `go-redis`.
**Tech Stack:** Go 1.25, chi, pgx + sqlc, testify, testcontainers (postgres, redis), `text/template` (stdlib), `github.com/santhosh-tekuri/jsonschema/v6` (disebut spec §6.10.1 sebagai [builder call]; tambahkan ke register D-003 sebelum `go get`).
**Spec:** docs/superpowers/specs/2026-10-12-kaiwa-backend-design.md — Bab 4 (§4.2, §4.4–§4.9), Bab 5 (§5.2–§5.7, §5.8.3–§5.8.6, §5.9.3–§5.9.5, §5.10, §5.11), Bab 6 (§6.2–§6.5, §6.8.1–§6.8.3, §6.9–§6.14), §7.9.3 & §7.13.1 (signature runtime), §9.3.1 (OutboxWriter).
**Master plan:** docs/superpowers/plans/2026-10-13-kaiwa-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M1 (semua migration/sqlc/`postgres.Store`/`repotest`, `errs`, `clock`, `idgen`, `config`), M2 (`auth.Principal`, `auth.TicketService`, `balance.Store/Tx` + `Repository` bagian grant/adjust + `OpenAccount`, `httpapi` `Handle/HandleNoBody/Page/Request/router.go/deps.go/errors.go`, `admin_audit_logs` writer, ticket store Redis).

## Global Constraints

- Master Global Constraints berlaku.
- Durasi sah `300, 600, 900, 1800, 2700, 3600` detik; `start_deadline_at = created_at + 5 menit`; start timeout `STARTING→ACTIVE` ≤ 10 s (D-030, D-035, D-053).
- `billable_end = LEAST(COALESCE(ending_at, ended_at), expires_at)`; `charged = min(reserved, ceil(active_ms/1000))`; `PROVIDER_FAILURE`/`SERVER_ERROR`: `active_ms < 60_000` → 0, selain itu `max(0, base − 30)`; semua integer milidetik (D-036, §4.5).
- Urutan lock `balance_accounts → users → sessions → balance_reservations → session_turns → insert-only`; `lock_timeout 3s`, `statement_timeout 5s`, retry 3× untuk 40P01/40001; tanpa Redis/provider/RabbitMQ di dalam transaksi (§4.6.0).
- Satu sesi non-terminal per user; `Idempotency-Key` wajib `^[A-Za-z0-9_-]{8,128}$`; replay → `200` + `Idempotent-Replayed: true`; key sama payload beda → `409 IDEMPOTENCY_KEY_REUSED` (§5.8.4).
- Sweeper: (e) handoff > 30 s, (a) `expires_at + 15 s`, (c) `ENDING` > 60 s, (d) `STARTING` > 30 s, (b) heartbeat > 45 s; batas 200/aturan/tick (2000 untuk kandidat (b), MGET batch 500); guard (b): Redis down + 45 s pulih, dan skip bila kandidat ≥ 20 **dan** ≥ 50 % sesi `ACTIVE` non-handoff (§5.6.4).
- `session.completed` hanya untuk terminal `COMPLETED`/`FAILED` dengan `started_at IS NOT NULL` (D-091); runtime_fence naik pada setiap transisi sweeper/cleanup/admin ke `ENDING` dan setiap transisi terminal.
- Cache konten in-process TTL 30 s (`CONTENT_CACHE_TTL_SECONDS`); load by-ID tanpa filter status (§6.9, D-NEW-6-16).
- Transcript: `410 TRANSCRIPT_EXPIRED` bila `transcript_purged_at IS NOT NULL` atau `ended_at < now − RETENTION_TRANSCRIPT_DAYS` (180); turns hanya `COMPLETED|INTERRUPTED|FAILED`, limit default 50/max 100.
- Seed ID literal `01920000-FFFF-7000-8000-0000000000NN`, idempoten `ON CONFLICT (id) DO NOTHING` (§6.13.2).

## Review Focus

1. **Klik ganda / retry `POST /sessions`** (master #1): satu sesi + satu `RESERVE`, replay = sesi sama, key sama payload beda → 409 `IDEMPOTENCY_KEY_REUSED`, key berbeda saat sesi terbuka → 409 `SESSION_ALREADY_OPEN` — Task 17 (unit + 20-goroutine), Task 24.
2. **Pod mati / client putus saat sesi berjalan** (master #4 bagian M3): tagihan berhenti di `ENDING`, finalize tepat sekali, tidak ada reservasi yatim, `session.completed` sekali — Task 19 (race Finalize), Task 22 (sweeper), Task 24 (rekonsiliasi).
3. **Runtime lama tidak boleh menulis setelah takeover/terminal** (implisit §7.9.3, tidak ada test lintas-bab): fence — Task 16 (tulis turn 0 baris), Task 19.
4. **Saldo tidak pernah bocor** (§4.4 I1–I10): R1–R5 = 0 baris setelah setiap skenario integration — helper Task 5, dipakai Task 17–24.
5. **Versi konten immutable & tepat satu `PUBLISHED`; katalog tidak menampilkan entitas yang gagal resolver** — Task 11 (publish konkuren), Task 13.

## File Structure

| Path | Tanggung jawab |
|---|---|
| `internal/platform/errs/codes.go`, `messages.go` (modifikasi) | Kode §5.10 milik M3 + pesan Indonesia |
| `internal/httpapi/errors.go` (modifikasi) | Map kode → HTTP status |
| `internal/balance/charge.go` | `BillableEnd`, `ComputeCharge` (pure) |
| `internal/balance/balance.go`, `service.go`, `reconcile.go` | Reservation/Movement, `Reserve/ReleaseReservation/SettleReservation`, `GetBalance/ListTransactions`, `Reconcile` |
| `internal/prompt/types.go`, `render.go`, `funcs.go`, `fixtures.go` | Tipe & mekanisme §6.8 yang dibutuhkan validator (lihat Task 6) |
| `internal/content/{types,character,expression,ports,repository,resolver,config_loader,cache,validator,catalog,admin}.go` | Domain konten, resolver §6.9, validator §6.10, katalog publik, `AdminService` |
| `internal/events/outbox.go`, `types.go` | `Event`, `OutboxWriter`, `NewSessionCompleted` (M6 menambah publisher) |
| `internal/session/{session,tx,transition,service,lifecycle,runtime,query,sweeper,guard}.go` | Domain, state machine, `Service`, `RuntimeService`, `QueryService`, `Sweeper` |
| `internal/postgres/{balance_repo,content_repo,session_repo,session_turn_repo,session_event_repo,outbox_writer}.go` + `db/queries/{balance,content,sessions,session_turns,session_events,outbox}.sql` | Implementasi port |
| `internal/redisstore/{heartbeat_reader,session_control}.go` | `session.HeartbeatReader`, `session.ControlPublisher` |
| `internal/httpapi/{content,admin_content,session,balance}_{routes,handler,dto}.go` | REST grup User/Admin |
| `internal/testutil/fakes/*.go`, `internal/testutil/repotest/{balance,session}.go`, `internal/testutil/reconcile.go` | Fake tulisan tangan, contract suite, `AssertReconciled` |
| `seed/0010…0070_*.sql`, `Makefile` (target `seed`) | Seed konten |
| `test/integration/m3_*_test.go` | Skenario lifecycle lintas paket (`//go:build integration`) |

---

### Task 1: Preflight kontrak M2 + kode error M3

**Files:**
- Create: `internal/m3preflight_test.go`
- Modify: `internal/platform/errs/codes.go`, `internal/platform/errs/messages.go`, `internal/httpapi/errors.go`
- Test: `internal/platform/errs/m3codes_test.go`

**Interfaces:**
- Consumes: M1 `errs.Code`, `errs.Messages`; M2 `httpapi.Handle/HandleNoBody/Page/Request`, `auth.Principal`, `balance.Store/Tx`, `clock.Clock`, `idgen.Generator`.
- Produces: konstanta `errs` untuk kode di tabel bawah (nama = kode dalam PascalCase, mis. `errs.BalanceInsufficient`).

- [ ] **Step 1: Write the failing test** — `TestM3CodesHaveMessagesAndStatus` (table `{code, status}`): `BALANCE_INSUFFICIENT 402`, `SESSION_NOT_FOUND 404`, `SESSION_ALREADY_OPEN 409`, `SESSION_NOT_CANCELLABLE 409`, `SESSION_NOT_CONNECTABLE 409`, `SESSION_START_DEADLINE_PASSED 409`, `IDEMPOTENCY_KEY_REQUIRED 400`, `IDEMPOTENCY_KEY_INVALID 400`, `IDEMPOTENCY_KEY_REUSED 409`, `INVALID_CURSOR 400`, `TRANSCRIPT_EXPIRED 410`, `TOPIC_NOT_AVAILABLE|SCENARIO_NOT_AVAILABLE|SCENARIO_TOPIC_MISMATCH|CHARACTER_NOT_AVAILABLE|CONVERSATION_STYLE_NOT_AVAILABLE|EXPRESSION_PROFILE_NOT_AVAILABLE|CONTENT_VALIDATION_FAILED 422`, `CONTENT_MISCONFIGURED 500`, `CONTENT_INVALID_TRANSITION|CONTENT_IN_USE_AS_DEFAULT|CONTENT_VERSION_REQUIRED 409`, `NOT_FOUND 404`, `TERMS_NOT_ACCEPTED 403`, `ACCOUNT_SUSPENDED 403`, `SERVICE_UNAVAILABLE 503`. Assert `errs.Messages[c]` sama persis dengan kolom `message` §5.10 dan `httpapi.StatusOf(c) == status`. `m3preflight_test.go` hanya `var _ = ...` yang mereferensikan simbol M2 di atas (gagal kompilasi = kontrak M2 tidak sesuai; sesuaikan nama, catat di `docs/implementation-deviations.md`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/errs/... ./internal/httpapi/... -race -run TestM3Codes` → FAIL (konstanta/pesan belum ada, atau sebagian sudah ada dari M1/M2: hanya yang hilang yang gagal).
- [ ] **Step 3: Implement** — tambahkan hanya kode yang hilang, pesan disalin dari §5.10, status ke `httpapi/errors.go`. Jika `Handle` belum mendukung header/status respons dinamis, tambahkan `type ResponseMeta struct{Status int; Header http.Header}` dan interface `Metaer{ Meta() ResponseMeta }` yang diperiksa `Handle` (dipakai `Idempotent-Replayed` dan `Location`).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go build ./...`.
- [ ] **Step 5: Commit** — `git add internal/ && git commit -m "feat(errs): add M3 error codes and HTTP status map"`

### Task 2: `BillableEnd` & `ComputeCharge`

**Files:**
- Create: `internal/balance/charge.go`
- Test: `internal/balance/charge_test.go`

**Interfaces:**
- Produces (persis §4.5): `func BillableEnd(endingAt *time.Time, endedAt, expiresAt time.Time) time.Time`; `func ComputeCharge(reserved int64, startedAt, billableEnd time.Time, disconnectedSeconds int64, serviceFault bool) int64`.

- [ ] **Step 1: Write the failing test** — `TestComputeChargeExamples`: 11 baris tabel §4.5 (kolom reserved, started, ending, ended, expires, disconnected, serviceFault, wantBillableEnd, wantActiveMs, wantCharged): ex1 `421`, ex2 `900`, ex3 `588`, ex4 `301`, ex5 `0`, ex6 `271`, ex7 `280`, ex8 `0`, ex9 `270` (`endingAt=nil`), ex10 `180`, ex11 `420`. `TestComputeChargeBoundaries`: failure `active_ms` 59_999 → 0, 60_000 → `60−30=30`; `active_ms = reserved*1000 ± 1` → charge `reserved` / `reserved` (clamp) / `reserved` ; `endingAt = expiresAt ± 1ms` → billable = yang lebih awal; `endingAt nil` memakai `endedAt`; disconnected > aktif → 0.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/balance/... -race -run TestComputeCharge` → FAIL (undefined).
- [ ] **Step 3: Implement** — salin blok Go §4.5 apa adanya (satu-satunya tempat rumus).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(balance): BillableEnd and ComputeCharge with spec examples"`

### Task 3: Domain reservasi & `balance.Service` operasi tx-bound

**Files:**
- Create: `internal/balance/balance.go` (jika M2 sudah punya, modifikasi), `internal/balance/service.go` (modifikasi dari M2), `internal/testutil/fakes/balance_repo.go`
- Test: `internal/balance/service_test.go`

**Interfaces:**
- Consumes: `balance.Repository`, `Movement`, `Account` (spec §4.2; M2 sudah punya `LockAccountByUserID`, `ApplyMovement`, `FindLedgerByIdempotencyKey`).
- Produces (nama dari plan; menerima `acc Account` yang SUDAH di-lock pemanggil):
```go
type Reservation struct{ ID, UserID, AccountID, SessionID uuid.UUID; ReservedSeconds int64; ConsumedSeconds, ReleasedSeconds *int64; Status ReservationStatus; StartDeadlineAt, CreatedAt time.Time; SettledAt, ReleasedAt *time.Time }
type ReserveInput struct{ ReservationID, SessionID uuid.UUID; Seconds int64; At time.Time }
type SettleInput struct{ SessionID uuid.UUID; StartedAt, EndedAt, ExpiresAt time.Time; EndingAt *time.Time; DisconnectedSeconds int64; ServiceFault bool; At time.Time }
type SettleResult struct{ Charged, Released, ActiveMs int64; BillableEnd time.Time }
func (s *Service) Reserve(ctx, tx Tx, acc Account, in ReserveInput) (Reservation, error)          // INSERT reservation HELD (+5 menit) lalu ApplyMovement RESERVE
func (s *Service) ReleaseReservation(ctx, tx Tx, acc Account, sessionID uuid.UUID, at time.Time) (int64, error) // §4.6.2 langkah 3-5
func (s *Service) SettleReservation(ctx, tx Tx, acc Account, in SettleInput) (SettleResult, error) // §4.6.3 langkah 3-6
var ErrReservationState = errors.New("reservation state mismatch")
```
Repository tambahan: `LockReservationBySessionID`, `InsertReservation`, `MarkReservationSettled`, `MarkReservationReleased`, `ListLedger`, `GetAccountByUserID` (baca tanpa lock).

- [ ] **Step 1: Write the failing test** — memakai fake repo: `TestReserve_MovesAvailableToReserved` (available 1200 → 600/600, entry `RESERVE` key `reserve:{rid}`, deadline = at+5m); `TestSettle_Example1` (reserved 600, charged 421 → entries `CONSUME(0,-421)` lalu `RELEASE(+179,-179)`, account 779/0, reservation `SETTLED` consumed 421 released 179); `TestSettle_ChargedZero_OnlyRelease`; `TestSettle_ChargedEqualsReserved_OnlyConsume` (tanpa RELEASE); `TestRelease_FullReturn` (consumed 0, `RELEASED`); `TestSettle_ReservationNotHeld_ErrReservationState`; `TestSettle_FailureUnder60s_Charge0`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/balance/... -race -run 'TestReserve|TestSettle|TestRelease'` → FAIL.
- [ ] **Step 3: Implement** — service memanggil `ComputeCharge`; entry bernilai delta 0 tidak ditulis (D-NEW-4-1); fake repo meniru `ApplyMovement` (update saldo + ledger + version).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(balance): reservation reserve/release/settle operations"`

### Task 4: Baca saldo, transaksi user, & `Reconcile`

**Files:**
- Create: `internal/balance/reconcile.go`
- Modify: `internal/balance/service.go`
- Test: `internal/balance/read_test.go`

**Interfaces:**
- Produces:
```go
type BalanceView struct{ AvailableSeconds, ReservedSeconds int64; AllowedDurations []int; UpdatedAt time.Time }
type TxView struct{ ID uuid.UUID; EntryType EntryType; AvailableDelta, ReservedDelta, AvailableAfter, ReservedAfter int64; SessionID *uuid.UUID; Description string; CreatedAt time.Time }
func (s *Service) GetBalance(ctx, userID uuid.UUID) (BalanceView, error)
func (s *Service) ListTransactions(ctx, userID uuid.UUID, types []EntryType, after *uuid.UUID, limit int) ([]TxView, error) // limit+1 → pemanggil memotong
type ReconcileRepository interface{ RunCheck(ctx context.Context, check string) ([]string, error) } // check "R1".."R6"
type Violation struct{ Check string; Rows []string }
func (s *Service) Reconcile(ctx context.Context) ([]Violation, error)
```

- [ ] **Step 1: Write the failing test** — `TestAllowedDurations` (available 700 → `[300,600]`; 299 → `[]`); `TestDescriptions` (tabel §4.7: GRANT/REGISTRATION `Welcome balance`, GRANT/ADMIN `Balance granted`, ADJUSTMENT `Balance adjustment`, RESERVE `Reserved for session`, CONSUME `Used in session`, RELEASE `Returned from session`); `TestListTransactions_FilterRejectsUnknownEntryType` (→ `VALIDATION_FAILED`); `TestReconcile_ReportsRowsPerCheck` (fake `RunCheck` mengembalikan baris untuk R3 → satu `Violation{Check:"R3"}`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/balance/... -race -run 'TestAllowed|TestDescriptions|TestList|TestReconcile'` → FAIL.
- [ ] **Step 3: Implement** — view tidak membawa `reason`/`admin_user_id`/`idempotency_key` (D-NEW-4-9). `Reconcile` menjalankan R1–R6 berurutan (SQL di postgres, Task 5).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(balance): balance read views and reconcile service"`

### Task 5: Repository balance di Postgres + `AssertReconciled` (integration)

**Files:**
- Create: `internal/postgres/balance_repo.go` (lengkapi dari M2), `db/queries/balance.sql` (tambah), `internal/testutil/repotest/balance.go`, `internal/testutil/reconcile.go`
- Test: `internal/postgres/balance_repo_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: `balance.Repository` (Task 3), `balance.ReconcileRepository` (Task 4), `postgres.Store.BalanceStore()`.
- Produces: `func RunBalanceRepoSuite(t *testing.T, mk func(t *testing.T) (balance.Store, func()))` (contract suite fake & postgres); `func AssertReconciled(t *testing.T, pool *pgxpool.Pool)` — menjalankan R1–R5 dari §4.8 dan `require.Empty` tiap hasil (R6 dilewati: butuh waktu).

- [ ] **Step 1: Write the failing test** — suite: (a) reserve+settle ex.1 → saldo 779/0, `AssertReconciled` kosong; (b) `UPDATE balance_ledger SET reason='x'` → error `restrict_violation`; `DELETE` → error; (c) `ApplyMovement` yang melanggar `chk_ledger_shape` → error; (d) `ListLedger` keyset `id DESC` limit+1; (e) `RunCheck("R1")` mendeteksi baris saat `balance_accounts.available_seconds` diubah manual (tes sengaja merusak lalu memeriksa R1 dan R3 tidak kosong); (f) `RunCheck("R4")` mendeteksi reservasi `HELD` untuk sesi `COMPLETED`.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres/... -race -run TestBalanceRepo` → FAIL.
- [ ] **Step 3: Implement** — query sqlc (`sqlc generate`), SQL R1–R6 disalin dari §4.8 ke `db/queries/balance.sql` (`-- name: ReconcileR1 :many`, dst.); `*postgres.Tx.Balance()` mengembalikan repo; mapping unique violation `ux_balance_ledger_idempotency` → `balance.ErrDuplicateMovement` (jika belum dari M2).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go test ./internal/balance/... -race` tetap PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(postgres): balance repository, reconcile queries, AssertReconciled helper"`

### Task 6: Mekanisme prompt yang dibutuhkan validator (§6.8)

> Catatan koordinasi: spec §6.8 menyebut mekanisme render/FuncMap/fixture "dimiliki Bab 6", master memberi `prompt.Render` ke M4. Task ini membuat **hanya** yang dibutuhkan M3. Bila `internal/prompt/render.go` sudah ada (M4 lebih dulu), lewati Step 3 file yang sudah ada dan jalankan test saja; bila belum, M4 MUST memakai file ini dan hanya menambah `EstimateTokens`, `Budget`, `SentenceParser`, `core.go`, `phrases.go`.

**Files:**
- Create: `internal/prompt/types.go`, `funcs.go`, `render.go`, `fixtures.go`
- Test: `internal/prompt/render_test.go`

**Interfaces:**
- Produces (persis §6.8.1–§6.8.2): `Template{Key string; Version int; System, User string; Params Params; OutputSchema json.RawMessage}`, `Params{Temperature *float64; MaxOutputTokens *int; TopP *float64; Seed *int64}` (tag `omitempty`), `Rendered{System, User string}`, `VocabHint`, `LearnerContext`, `PromptData` + `type OpeningPromptData = PromptData`, `func Render(t Template, data any) (Rendered, error)`, `func RegisterFixtures(key string, fn func() []any)`, `func Fixtures(key string) []any`, `FuncMap` (`join`, `bullets`, `quote`, `vocab`).

- [ ] **Step 1: Write the failing test** — `TestRender_MissingKeyErrors` (`{{.Nope}}` → error); `TestRender_SnakeCaseFails` (`{{.character_name}}`); `TestFuncMap` (`bullets []string{"a","b"}` → `"- a\n- b"`, `quote "私"` → `「私」`, `vocab` → `"- 経験（けいけん）: experience"`, `join`); `TestRender_UserEmptyWhenNoUserTemplate`; `TestConversationFixtures` (`Fixtures("CONVERSATION")` memuat tepat 2 instance bertipe `PromptData`: minimal dan maksimal; render template seed §6.8.3 dengan keduanya sukses, hasil maksimal ≤ 12.000 karakter — template diambil dari `seed/0070_prompts.sql` di Task 12, jadi test ini memakai string template kecil lokal dan test penuh dipindah ke Task 12).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/... -race` → FAIL (undefined).
- [ ] **Step 3: Implement** — `text/template` dengan `Option("missingkey=error")`; fixtures `CONVERSATION`/`OPENING` terdaftar di `init()` milik paket `prompt`; `RegisterFixtures` dengan key sama dua kali → panic (programmer error).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(prompt): template types, render, funcmap, fixtures for content validation"`

### Task 7: Domain konten, `EffectiveFormality`, `SpeakingRate`, katalog ekspresi

**Files:**
- Create: `internal/content/types.go`, `character.go`, `expression.go`
- Test: `internal/content/types_test.go`

**Interfaces:**
- Produces: tipe §6.5 persis (`VersionStatus`, `EntityStatus`, `Formality`, `PromptKey`, `Family`, `ScenarioVersion`, `ConversationStyleVersion`, `ExpressionProfileVersion`, `ExpressionLimit`, `PromptVersion` + `Template()`, `CharacterVersion`, `VoiceProfile`, `VoiceProviderConfig`, `Topic`, `Scenario`, `Character`, `ConversationStyle`, `ExpressionProfile`); `func EffectiveFormality(sv ScenarioVersion, stv ConversationStyleVersion) Formality`; `func SpeakingRate(voiceDefaultSpeed, styleSpeechRate, adaptiveRate float64) float64`; `var Expressions` dengan `Lookup(expr string) (degradeTo string, nonVerbal bool, ok bool)`; `func DifficultyBand(level int) (band, jlpt string)`.

- [ ] **Step 1: Write the failing test** — `TestEffectiveFormality` (override `KEIGO` menang atas `CASUAL`; nil → style); `TestSpeakingRate` (`1.00×0.95×0.95` → `0.9025`; adaptive 0 → 1.0; hasil di-clamp `0.80`/`1.05`); `TestExpressionsCatalog` (12 nama; `laugh`→`amused` nonVerbal, `amused`→`happy`, `happy`→`neutral`, `sigh`→`concerned`, `concerned`→`empathetic`, `sad`→`empathetic`, `neutral` degrade kosong; nama tak dikenal `ok=false`); `TestDifficultyBand` (1 `BEGINNER/N5` … 5 `ADVANCED/N1`); `TestPromptVersionTemplate` (UserTemplate nil → `Template.User == ""`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/content/... -race -run 'TestEffective|TestSpeaking|TestExpressions|TestDifficulty|TestPromptVersion'` → FAIL.
- [ ] **Step 3: Implement** — salin tipe & fungsi dari §6.5/§6.3.7; `content` hanya mengimpor `prompt`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make arch-check` (depguard) bersih.
- [ ] **Step 5: Commit** — `git commit -m "feat(content): domain types, effective formality, speaking rate, expression catalog"`

### Task 8: `content.Validator` (§6.10)

**Files:**
- Create: `internal/content/validator.go`
- Test: `internal/content/validator_test.go`

**Interfaces:**
- Consumes: `prompt.Render`, `prompt.Fixtures`, `content.Repository` (Task 9 mendefinisikan; validator hanya memakai subset `ValidatorRepo` yang didefinisikan di file ini: `GetCharacterDefaults(ctx, versionID) (CharacterRefs, error)` dst. — fake di test).
- Produces: `type Issue struct{Severity, Entity, Field, Rule, Message string}`; `type Validator interface{ValidateDraft(ctx, Family, any) []Issue; ValidatePublish(ctx, Family, uuid.UUID) []Issue; ValidateCatalog(ctx) []Issue}`; `func NewValidator(repo ValidatorRepo, cfg ValidatorConfig) Validator` dengan `ValidatorConfig{PrimaryTTS string; ProviderModeReal bool}`.

- [ ] **Step 1: Write the failing test** — table-driven per family (satu kasus per aturan §6.10.1; `Rule` string = nama dari plan, ditulis di test): scenario (`evaluation_focus` 4 item → `max_items`; skill tak dikenal → `invalid_skill`; `opening_line` tanpa karakter Jepang → `japanese_required`; `opening_line` memuat `<e:` → `tag_not_allowed`; `recommended_level_min > max` → `range`; vocab `term_ja` ganda → `duplicate`); character (dialect KANSAI + voice accent KANTO → `dialect_accent_mismatch` ERROR; tier≠quality_tier → `tier_mismatch`; tanpa voice_provider_config primary → `missing_primary_voice_config`; placeholder + real → ERROR, fake → WARNING); style (`behavior_flags` kurang 1 key → `required`; `correction_policy` ≠ NONE); expression (`allowed` tanpa `neutral`; `expression_limits` key `neutral`; `tts_mappings` param tak dikenal); prompt — tabel §6.14 #8 lengkap: `user_template` NULL untuk SUMMARY/EVALUATION/MEMORY_EXTRACTION → `required`; `user_template`/`output_schema` terisi untuk CONVERSATION/OPENING → `must_be_null`; `output_schema` NULL untuk EVALUATION/MEMORY_EXTRACTION; schema invalid → `invalid_json_schema`; `{{.snake_case}}` → `template_render`; `{{.CoreRules}}` dua kali → `placeholder_count`; key tanpa fixture → `missing_fixture`; `llm_params` key `model` → ERROR; `temperature` absen → WARNING.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/content/... -race -run TestValidator` → FAIL.
- [ ] **Step 3: Implement** — `output_schema` dikompilasi dengan `jsonschema/v6` draft 2020-12 dan root `"type":"object"`; dry-run render tiap fixture dari `prompt.Fixtures(key)`; `ValidateCatalog` memeriksa seluruh entitas `ACTIVE` + voice settings/capabilities §6.10.1; **keputusan M3:** key prompt tanpa version `PUBLISHED` = ERROR hanya untuk `CONVERSATION`, WARNING `missing_published_prompt` untuk key lain (versi SUMMARY/EVALUATION/MEMORY_EXTRACTION ditambahkan M7/M8 bersama fixture-nya; catat di deviations).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(content): content validator with prompt dry-run rules"`

### Task 9: `Repository` konten, cache, `Resolver` (§6.9), `SessionConfigLoader`

**Files:**
- Create: `internal/content/ports.go`, `repository.go`, `cache.go`, `resolver.go`, `config_loader.go`, `internal/testutil/fakes/content_repo.go`
- Test: `internal/content/resolver_test.go`, `internal/content/cache_test.go`

**Interfaces:**
- Consumes: tipe Task 7, `clock.Clock`.
- Produces (§6.5 persis): `ResolveInput`, `ResolvedConfig`, `Resolver`, `SessionConfig`, `SessionConfigLoader`; `type Repository interface` dengan method baca: `GetTopic`, `GetScenario`, `GetPublishedScenarioVersion`, `GetCharacter`, `GetPublishedCharacterVersion`, `GetConversationStyle`, `GetPublishedConversationStyleVersion`, `GetExpressionProfile`, `GetPublishedExpressionProfileVersion`, `GetDefaultVoiceProfile(characterID)`, `GetVoiceProviderConfig(voiceProfileID, provider)`, `GetPublishedPromptVersion(key)`, plus by-ID `GetScenarioVersion/GetCharacterVersion/…/GetPromptVersion/GetTopicByID/GetVoiceProfile` dan `ListVoiceConfigs(voiceProfileID)`; `type ResolverConfig struct{TTSMode, TTSPrimary, TTSFallback string; ProviderProfiles []string}`; `type Observer interface{ResolveError(code string); VoiceFallbackMissing(voiceProfile string)}`; `func NewCachedRepository(r Repository, ttl time.Duration, c clock.Clock) Repository`; `func NewResolver(r Repository, cfg ResolverConfig, obs Observer) Resolver`; `func NewSessionConfigLoader(r Repository) SessionConfigLoader`.

- [ ] **Step 1: Write the failing test** — `TestResolve` table-driven, satu kasus per baris pseudocode §6.9 dengan kode yang diharapkan: topic nil/non-ACTIVE → `TOPIC_NOT_AVAILABLE`; scenario nil/non-ACTIVE/tanpa PUBLISHED → `SCENARIO_NOT_AVAILABLE`; topic beda → `SCENARIO_TOPIC_MISMATCH`; character nil/non-ACTIVE/tanpa PUBLISHED → `CHARACTER_NOT_AVAILABLE`; style override rusak → `CONVERSATION_STYLE_NOT_AVAILABLE` tetapi default karakter rusak → `CONTENT_MISCONFIGURED`; hal sama untuk expression; voice nil/non-ACTIVE, dialek tak cocok, tier tak cocok → `CONTENT_MISCONFIGURED`; mode real: config primary nil/DISABLED/`PLACEHOLDER_` → `CONTENT_MISCONFIGURED`; mode fake: tanpa baris vendor tetap sukses; fallback hilang → sukses + `Observer.VoiceFallbackMissing`; tanpa prompt CONVERSATION → `CONTENT_MISCONFIGURED`; `ProviderProfiles` tidak memuat tier → `CONTENT_MISCONFIGURED`; `AI_FIRST` tanpa OPENING PUBLISHED → **sukses**. Urutan error: input dengan topic DAN scenario rusak mengembalikan `TOPIC_NOT_AVAILABLE`. `TestCache`: dua baca dalam 30 s → 1 hit ke repo; setelah fake clock +31 s → 2; method by-ID tidak kedaluwarsa. `TestLoadIgnoresStatus`: version `RETIRED` + induk `DISABLED` tetap dimuat.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/content/... -race -run 'TestResolve|TestCache|TestLoad'` → FAIL.
- [ ] **Step 3: Implement** — `CONTENT_MISCONFIGURED` selalu `slog.Error` dengan `entity`,`rule` + `obs.ResolveError(code)`; cache hanya membungkus method "published/active" (TTL), by-ID di-LRU 1.000 tanpa TTL; `ListVoiceConfigs` tidak di-cache.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(content): repository port, cache, resolver and session config loader"`

### Task 10: Repository konten di Postgres (integration)

**Files:**
- Create: `internal/postgres/content_repo.go`, `db/queries/content.sql` (termasuk `GetUserVoiceExposure` §6.11 dan `ListPublic*` §6.12)
- Test: `internal/postgres/content_repo_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: `content.Repository` (Task 9). Produces: `postgres.NewContentRepo(pool) content.Repository` + `(*postgres.Tx).Content() content.TxRepository` (tambah method tulis: `InsertDraft`, `LockParent`, `LockVersion`, `RetirePublished`, `SetPublished`, `DeleteDraft`, `SetEntityStatus`, `NextVersionNumber`).

- [ ] **Step 1: Write the failing test** — setelah migrate + seed fixture kecil lewat SQL: `TestGetPublished_ReturnsOnlyPublished`; `TestLoadByIDIgnoresStatus`; `TestPublishTwiceSameParent_UniqueViolation` (insert langsung dua PUBLISHED → error `23505`); `TestVersionImmutable` (UPDATE `situation` → error trigger `content_version_guard`; DELETE PUBLISHED → error); `TestNextVersionNumber_UnderParentLock` (dua tx paralel menunggu lock → nomor 2 dan 3); `TestGetUserVoiceExposure`.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres/... -race -run TestContentRepo` → FAIL.
- [ ] **Step 3: Implement** — query sqlc; `GetDefaultVoiceProfile` JOIN `character_voice_profiles WHERE is_default`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(postgres): content repository and sqlc queries"`

### Task 11: `CatalogService` & `AdminService` konten

**Files:**
- Create: `internal/content/catalog.go`, `admin.go`, `tx.go`
- Test: `internal/content/admin_test.go`, `internal/content/catalog_test.go`, `test/integration/m3_content_publish_test.go` (`integration`)

**Interfaces:**
- Consumes: `Repository`, `Validator`, `Resolver` (usable-filter katalog memakai aturan resolver).
- Produces:
```go
type Store interface{ WithinTx(ctx, fn func(ctx, tx Tx) error) error }
type Tx interface{ Content() TxRepository; Audit() AuditWriter }
type AuditWriter interface{ Record(ctx, AuditEntry) error }   // action, target_type, target_id, metadata, admin_user_id
func (s *CatalogService) Topics(ctx) ([]TopicView, error); Scenarios(ctx, topicID *uuid.UUID); Scenario(ctx, id); Characters(ctx); Character(ctx, id); Styles(ctx); Expressions(ctx)   // semua hanya entitas usable; detail tak usable → errs NOT_FOUND
func (s *AdminService) CreateDraft(ctx, actor uuid.UUID, fam Family, parent string, body json.RawMessage, copyFrom *uuid.UUID) (VersionView, error)
func (s *AdminService) Publish(ctx, actor uuid.UUID, fam Family, parent string, versionID uuid.UUID) (PublishResult, error) // {Version, RetiredVersionID *uuid.UUID, Warnings []Issue}
func (s *AdminService) Retire(...) (RetireResult, error); DeleteDraft(...) error; SetStatus(ctx, actor, entity string, id uuid.UUID, to EntityStatus) (EntityView, error); ListVersions/GetVersion; ValidationReport(ctx) ([]Issue, []Issue, error)
```
`TopicView/ScenarioView/CharacterView/StyleView/ExpressionView` = field persis §6.12.1–§6.12.5 (`jlpt_label`, `difficulty_band`, `scenario_count`, `voice{voice_profile_id,gender,accent}`; tanpa `opening_line`, `constraints`, `persona`, dst.).

- [ ] **Step 1: Write the failing test** — unit (fake Store): `TestPublish_RetiresPreviousInSameTx`; `TestPublish_Idempotent` (sudah PUBLISHED → no-op, tanpa audit baru); `TestPublish_ValidationError_422WithIssueArray`; `TestPublish_WrongParent_404`; `TestRollbackRetiredToPublished`; `TestRetireStyleUsedAsDefault_409` (`CONTENT_IN_USE_AS_DEFAULT`); `TestSetVoiceStatusDisabledWhenDefault_409`; `TestRetirePromptWithoutReplacement_409` (`CONTENT_VERSION_REQUIRED`); `TestDeleteNonDraft_409` (`CONTENT_INVALID_TRANSITION`); `TestCreateDraftCopyFrom` (field tak dikirim disalin); audit action `CONTENT_VERSION_CREATED|PUBLISHED|RETIRED|DELETED|CONTENT_STATUS_CHANGED`; katalog: `TestCharacterHiddenWhenDefaultVoiceDisabled`, `TestCatalogOmitsPrivateFields` (reflect pada JSON hasil). Integration `TestPublishConcurrent`: 2 goroutine publish dua version DRAFT berbeda untuk induk yang sama (`-count=20`) → akhir tepat satu `PUBLISHED`, tanpa error 500/`23505` yang bocor.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/content/... -race -run 'TestPublish|TestRetire|TestSetVoice|TestDelete|TestCreateDraft|TestCharacter|TestCatalog|TestRollback'` → FAIL.
- [ ] **Step 3: Implement** — algoritma publish §6.13.1 (lock induk → lock version → validasi → retire lama → publish → audit → invalidasi cache setelah commit); nomor version baru dihitung di bawah lock induk; `postgres` menyediakan `postgres.Store.ContentStore()` dan `Audit()` yang menulis `admin_audit_logs` memakai query M2.
- [ ] **Step 4: Run tests to verify they pass** — unit PASS; `go test -tags integration ./test/integration -race -run TestPublishConcurrent -count=20` PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(content): catalog and admin lifecycle services"`

### Task 12: Seed SQL, `make seed`, `TestSeedContentIsValid`

**Files:**
- Create: `seed/0010_topics.sql`, `0020_conversation_styles.sql`, `0030_expression_profiles.sql`, `0040_voice_profiles.sql`, `0050_characters.sql`, `0060_scenarios.sql`, `0070_prompts.sql`
- Modify: `Makefile` (target `seed`)
- Test: `test/integration/m3_seed_test.go` (`integration`)

**Interfaces:**
- Consumes: validator (Task 8), resolver (Task 9), repo (Task 10), `prompt.Render`. Produces: konten seed §6.13.4 (5 topic, 13 scenario, 3 style, 3 expression profile, 4 voice profile × 3 vendor config, 4 karakter HARUKA/KENJI/AOI/RIKU, 5 `prompt_configs` + version 1 `PUBLISHED` untuk `CONVERSATION` dan `OPENING`).

- [ ] **Step 1: Write the failing test** — `TestSeedContentIsValid`: migrate + jalankan semua `seed/*.sql` (psql `ON_ERROR_STOP`) dengan `PROVIDER_MODE=fake` → `ValidateCatalog` tanpa ERROR; `TestSeedIdempotent` (jalankan 2× → jumlah baris tiap tabel konten identik); `TestSeedCounts` (5/13/3/3/4/12/4/5 + 2 prompt versions); `TestSeedResolveHarukaBackend`: `Resolve{Topic JOB_INTERVIEW, Scenario BACKEND_ENGINEER_INTERVIEW (01920000-0002-7000-8000-000000000003), Character HARUKA (…000a…01)}` → `ProviderProfile=="PREMIUM"`, style `FRIENDLY_POLITE`, `Dialect STANDARD`, `ScenarioVersion.FormalityOverride == KEIGO`; `TestSeedPromptsRender`: render `CONVERSATION` & `OPENING` seed terhadap kedua fixture (minimal: tanpa kalimat nama learner; maksimal ≤ 12.000 char); `TestSeedFullNameNeverRendered` (fixture dengan `Learner.DisplayName=""` dan string `"FULLNAME-SENTINEL"` tidak ada di output — struct tidak punya field full name).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration -race -run 'TestSeed'` → FAIL (file seed belum ada).
- [ ] **Step 3: Implement** — salin contoh HARUKA/voice/BACKEND_ENGINEER_INTERVIEW dari §6.13.5 apa adanya; sisanya mengikuti tabel §6.13.4 dengan ID literal skema §6.13.2; `0070_prompts.sql` memuat 5 baris `prompt_configs` + version `CONVERSATION`/`OPENING` dari §6.8.3 (dollar-quoting `$tpl$`); versi SUMMARY/EVALUATION/MEMORY_EXTRACTION ditambahkan M7/M8 ke file ini. `Makefile`: target `seed` persis §6.13.2.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make seed` dua kali di DB lokal → sukses.
- [ ] **Step 5: Commit** — `git commit -m "feat(seed): content seed, make seed target, seed validity tests"`

### Task 13: REST konten (katalog publik + admin content) & `TestRouteTable` bagian konten

**Files:**
- Create: `internal/httpapi/content_{routes,handler,dto}.go`, `admin_content_{routes,handler,dto}.go`, `testdata/api/rest/content/*.json`
- Modify: `internal/httpapi/router.go`, `deps.go`
- Test: `internal/httpapi/content_handler_test.go`, `internal/httpapi/route_table_test.go`

**Interfaces:**
- Consumes: `content.CatalogService`, `content.AdminService` (interface kecil di `httpapi`), M2 `Handle*`.
- Produces: `registerContentRoutes(user chi.Router, h *contentHandler)`, `registerAdminContentRoutes(admin chi.Router, h *adminContentHandler)`; `Deps.Content *content.CatalogService`, `Deps.ContentAdmin *content.AdminService`; route: 7 GET katalog (§5.8.5) + admin: `GET|POST /admin/content/{family}/{parent}/versions`, `GET …/versions/{version_id}`, `POST …/publish`, `POST …/retire`, `DELETE …/versions/{version_id}`, `PATCH /admin/content/{entity}/{id}/status`, `GET /admin/content/validation-report`. Header `Cache-Control: private, max-age=60` untuk katalog.

- [ ] **Step 1: Write the failing test** — golden JSON per endpoint katalog (bentuk §6.12.1–§6.12.5 persis, `{"items":[…]}`); `GET /scenarios?topic_id=bukan-uuid` → 400 `VALIDATION_FAILED`; detail tak usable → 404 `NOT_FOUND`; admin POST dengan field `template` → 400 (unknown field); publish gagal validasi → 422 dengan `details` array `Issue`; `TestRouteTable_ContentRoutes`: `chi.Walk` membandingkan (method, path, grup) milik prefix `/topics /scenarios /characters /conversation-styles /expression-profiles` (grup User) dan `/admin/content` (grup Admin) dengan daftar harapan; route berlebih/hilang/salah grup → gagal. (M7/M8/M2 menambah prefix mereka ke tabel yang sama.)
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/httpapi/... -race -run 'TestContent|TestRouteTable'` → FAIL.
- [ ] **Step 3: Implement** — handler hanya decode → service → DTO; semua lewat `Handle/HandleNoBody`; DTO terpisah dari domain.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(httpapi): content catalog and admin content endpoints"`

### Task 14: Domain `session` — status, transisi, tipe, port

**Files:**
- Create: `internal/session/session.go`, `tx.go`, `transition.go`
- Test: `internal/session/transition_test.go`

**Interfaces:**
- Produces (spec §5.2 persis): `Status`, `EndReason`, `Actor`, `StartMode`, `Fence = *int64`, `CanTransition(from, to Status) bool`, `TerminalStatusFor(r EndReason) Status`, `ErrNotFound/ErrInvalidTransition/ErrStartDeadlinePassed/ErrStartTimeout/ErrAlreadyOpen/ErrFenceLost`, `Session` (semua kolom §5.3.1), `Turn`, `Event`, `RuntimeRecord`, `Settlement` (§7.13.1), `Store`/`Tx` (§4.2; `Tx` = `balance.Tx` + `Sessions() Repository`, `Turns() TurnRepository`, `Events() EventRepository`, `Outbox() events.OutboxWriter`). Konstanta `StartDeadline = 5*time.Minute`, `StartTimeout = 10*time.Second`. Port tambahan (nama dari plan): `UserGate{Gate(ctx, userID) (UserGateInfo{Status, TermsVersion string}, error)}`, `AdaptiveProvider{Adaptive(ctx, AdaptiveRequest{UserID uuid.UUID; Config content.ResolvedConfig}) (json.RawMessage, error)}` (M8 menyediakan adapter nyata di atas `learner.Service`; M3 pakai fake), `OpeningPromptSource{PublishedPromptVersionID(ctx, key string) (*uuid.UUID, error)}`, `ControlPublisher{Publish(ctx, sessionID uuid.UUID, msg ControlMessage) error}`, `HeartbeatReader{Read(ctx, ids []uuid.UUID) (map[uuid.UUID]time.Time, error)}`.
- `Repository` (consumer-defined): `Insert`, `GetByID`, `GetForUpdate`, `OwnerOf`, `FindByIdempotencyKey(userID,key)`, `FindOpenByUser`, `CAS(ctx, id, from Status, fence Fence, p Patch) (Session, bool, error)`, `Claim(ctx, id, allowed []Status) (Session, error)`, `MarkHandoff(ctx, id, fence int64, at time.Time) (bool, error)`, `AddDisconnected(ctx, id, fence int64, n int, at time.Time) (Session, bool, error)` (SQL §5.6.5), `ListByUser`, `GetRuntimeRecord`, `SetSummary`, kandidat sweeper (`ListExpired`, `ListStuckEnding`, `ListStuckStarting`, `ListHandoffStale`, `ListActiveForHeartbeat`, `CountActiveNonHandoff`), `ListAbandonCandidates`. `TurnRepository`: `Insert/UpdateUser/Finalize/SetUserAudioKey/FailInProgress/ListRecent/MaxSeqAndGeneration` (signature §7.13.1, tipe `TurnStart/TurnUser/TurnFinal/TurnRecord` didefinisikan di `session`; M5 membuat alias di `conversation`), plus `FailDangling(ctx, sessionID, startingAt, at) (int, error)`, `Stats(ctx, sessionID, startingAt) (turnCount, userTurnCount int, hasAudio bool, err error)`, `ListForUser(ctx, sessionID, startingAt, endedAt *time.Time, afterSeq *int, limit int) ([]TurnRecord, error)`. `EventRepository`: `Append(ctx, Event)`, `AppendFenced(ctx, fence int64, Event)`.

- [ ] **Step 1: Write the failing test** — `TestCanTransition_Matrix`: iterasi 8×8; true hanya untuk `RESERVED→{STARTING,CANCELLED,ABANDONED}`, `STARTING→{ACTIVE,FAILED}`, `ACTIVE→{ENDING,FAILED}`, `ENDING→{COMPLETED,FAILED}`; `TestTerminalStatusFor`: `USER_ENDED|DURATION_ELAPSED|RECONNECT_TIMEOUT→COMPLETED`, `PROVIDER_FAILURE|SERVER_ERROR→FAILED`, `USER_CANCELLED→CANCELLED`, `NOT_STARTED→ABANDONED`; `TestStatusFromStringRejectsUnknown`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run 'TestCanTransition|TestTerminalStatusFor|TestStatusFrom'` → FAIL.
- [ ] **Step 3: Implement** — tabel transisi sebagai `map[Status]map[Status]bool` privat; semua tipe/port di atas (tanpa implementasi service).
- [ ] **Step 4: Run tests to verify they pass** — PASS; `go build ./...`.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): domain types, state machine, ports"`

### Task 15: Paket `events` + outbox writer minimal

**Files:**
- Create: `internal/events/outbox.go`, `internal/events/types.go`, `internal/postgres/outbox_writer.go`, `db/queries/outbox.sql`, `internal/testutil/fakes/outbox.go`
- Test: `internal/events/session_completed_test.go`, `internal/postgres/outbox_writer_integration_test.go`

**Interfaces:**
- Produces (§9.3.1 persis): `Event`, `OutboxWriter`; `const SessionCompleted = "session.completed"`; `type SessionCompletedPayload struct` (field & tag JSON persis §5.7.2: `session_id, final_status, end_reason, conversation_start_mode, topic_id, scenario_id, scenario_version_id, character_id, character_version_id, prompt_version_id, provider_profile, duration_seconds, started_at, ended_at, active_seconds, charged_seconds, turn_count, user_turn_count, has_user_audio`); `func NewSessionCompleted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p SessionCompletedPayload) Event` (`EventVersion 1`, `AggregateType "SESSION"`, `AggregateID = p.SessionID`); fake `fakes.Outbox` (merekam event).
- **Keputusan:** tabel `event_outbox` sudah dibuat M1 (`000020_messaging`), jadi M3 mengimplementasikan writer INSERT-only sekarang (kolom `traceparent/tracestate` diisi dari propagator ctx). Publisher/topologi/consumer = M6.

- [ ] **Step 1: Write the failing test** — `TestNewSessionCompleted_Fields` (JSON golden: `final_status`, `active_seconds=floor(active_ms/1000)`); integration `TestOutboxWriter_InsertsPendingRow` (status `PENDING`, attempts 0, payload JSON sama, `event_type` `session.completed`) dan `TestOutboxWriter_RollsBackWithTx` (error di fn → tidak ada baris).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/events/... -race` dan `go test -tags integration ./internal/postgres/... -race -run TestOutboxWriter` → FAIL.
- [ ] **Step 3: Implement** — `postgres.Tx.Outbox()` mengembalikan writer terikat tx; kolom sesuai DDL §9.2.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(events): outbox port, session.completed builder, postgres writer"`

### Task 16: Repository session/turn/event di Postgres + fencing (integration)

**Files:**
- Create: `internal/postgres/session_repo.go`, `session_turn_repo.go`, `session_event_repo.go`, `db/queries/{sessions,session_turns,session_events}.sql`, `internal/testutil/repotest/session.go`
- Test: `internal/postgres/session_repo_integration_test.go` (`integration`)

**Interfaces:**
- Consumes: port Task 14. Produces: `postgres.NewSessionRepo(pool)` (non-tx reader) dan `(*Tx).Sessions()/Turns()/Events()`; `Store.SessionStore() session.Store`; `func RunSessionRepoSuite(t, mk)`.

- [ ] **Step 1: Write the failing test** — (a) CHECK §5.11: `COMPLETED` dengan `PROVIDER_FAILURE` ditolak; `ACTIVE` tanpa `started_at` ditolak; `opening_prompt_version_id` pada `USER_FIRST` ditolak; sesi terbuka kedua ditolak (`ux_sessions_one_open_per_user` → `session.ErrAlreadyOpen`); `chk_turn_status` menerima NULL; `handoff_at < started_at` ditolak; (b) partisi: `session_turns_pYYYYMM`/`session_events_pYYYYMM` ada untuk bulan berjalan+3; insert `created_at` bulan depan masuk partisi benar; di luar rentang gagal; tidak ada FK menunjuk ke keduanya (`pg_constraint`); `EXPLAIN` turns per sesi dengan batas `created_at` hanya menyentuh partisi relevan; (c) fence: `Turns().Insert(fence=1)` sukses saat `runtime_fence=1`, `ClaimRuntime` kedua (fence 2) → `Insert(fence=1)` mengembalikan `ErrFenceLost` (0 baris) dan `Events().AppendFenced` sama; status terminal → `ErrFenceLost`; (d) `Claim` menaikkan `runtime_fence` tanpa menaikkan `version` dan me-NULL `handoff_at`; (e) `AddDisconnected` SQL §5.6.5: n=12 → 12; episode melewati `expires_at` 5 s → `n−5`; fence salah → `ErrFenceLost`; (f) `FailInProgress` mengubah semua `status IS NULL` menjadi `FAILED`.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres/... -race -run TestSessionRepo` → FAIL.
- [ ] **Step 3: Implement** — query fenced persis pola §7.9.3 (`INSERT … SELECT … WHERE EXISTS`); UPDATE turn memakai `WHERE id=$1 AND created_at=$2 AND status IS NULL`; query turns per sesi selalu membawa batas `created_at`; translasi `23505` pada dua index unik ke `ErrAlreadyOpen` / `ErrDuplicateIdempotency` (sentinel baru di `session`, dipakai Task 17). Perbaikan DDL (jika ditemukan celah) dilakukan di migration M1 yang bersangkutan.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(postgres): session, turn, event repositories with fencing"`

### Task 17: `session.Service.Create` (idempotency, satu sesi terbuka, reservasi)

**Files:**
- Create: `internal/session/service.go`, `internal/session/create.go`, `internal/testutil/fakes/{session_repo,adaptive,user_gate}.go`
- Test: `internal/session/create_test.go`, `test/integration/m3_create_test.go` (`integration`)

**Interfaces:**
- Consumes: Task 3 (`Reserve`), Task 9 (`Resolver`), port Task 14.
- Produces: `type Deps struct{Store Store; Reader Repository; Balance *balance.Service; Resolver content.Resolver; Adaptive AdaptiveProvider; Users UserGate; OpeningPrompts OpeningPromptSource; Control ControlPublisher; Clock clock.Clock; IDs idgen.Generator; Log *slog.Logger; TermsVersion string; Producer func(Fence) string}`; `func NewService(d Deps) *Service`; `CreateSessionInput`, `CreateSessionResult{Session; Replayed bool}` (§5.2); `func (s *Service) Create(ctx, in CreateSessionInput) (CreateSessionResult, error)`; `func HashCreateRequest(in CreateSessionInput) []byte`.

- [ ] **Step 1: Write the failing test** — unit (fakes): `TestCreate_HappyPath` (status `RESERVED`, reservasi 900, event `SESSION_RESERVED` payload `{"duration_seconds":900,"reservation_id":…}`, `adaptive_difficulty` tersalin dari fake); `TestCreate_Replay_SameKeySamePayload` (`Replayed=true`, tetap 1 `RESERVE`); `TestCreate_KeyReused_DifferentPayload` → `IDEMPOTENCY_KEY_REUSED`; `TestCreate_SecondSessionDifferentKey_SessionAlreadyOpen` (`details.session_id`); `TestCreate_InvalidKeyFormat` (`IDEMPOTENCY_KEY_INVALID`; kosong → `_REQUIRED`); `TestCreate_BalanceInsufficient` (available 600, durasi 900 → `BALANCE_INSUFFICIENT` details `{"available_seconds":600,"requested_seconds":900}`); `TestCreate_DurationNotAllowed` (`VALIDATION_FAILED`); `TestCreate_UserSuspended` (`ACCOUNT_SUSPENDED`) dan `TestCreate_TermsStale` (`TERMS_NOT_ACCEPTED`, `details.current_version`) — keduanya dicek **sebelum** resolve; `TestCreate_ResolveErrorPropagates` (`SCENARIO_TOPIC_MISMATCH` apa adanya); `TestCreate_AdaptiveFailure_503` (tanpa fallback default); `TestHashCreateRequest` (deterministik; style nil ≠ style terisi; mode beda → hash beda). Integration `TestCreate_20Parallel_DifferentKeys` (available 600, durasi 300 → tepat 1 sukses, 19 `SESSION_ALREADY_OPEN`; setelah `Cancel` lagi 20 → tepat 1), `TestCreate_20Parallel_SameKey` (semua mengembalikan sesi yang sama, tepat 1 entry `RESERVE`), `TestCreate_UniqueViolationBackstop` (paksa tabrakan di INSERT → respons ditentukan ulang di luar tx); `AssertReconciled` setelah tiap test.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run 'TestCreate|TestHash'` → FAIL.
- [ ] **Step 3: Implement** — urutan §4.6.1: pre-tx (gate → resolve → adaptive → hash), lalu tx: lock account → cek idempotency → cek open → cek saldo → `Insert` → `Balance.Reserve` → `Events().Append`; `ErrAlreadyOpen`/`ErrDuplicateIdempotency` dari backstop → rollback lalu ulangi pemeriksaan di luar tx.
- [ ] **Step 4: Run tests to verify they pass** — unit PASS; `go test -tags integration ./test/integration -race -run TestCreate` PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): Create with idempotency, single-open guard and reservation"`

### Task 18: Jalur release — `Cancel`, `Abandon`, `CancelInTx`, `BeginStart`, `Activate`, `FailStart`

**Files:**
- Create: `internal/session/lifecycle.go` (bagian release/start), `internal/session/control.go`
- Test: `internal/session/lifecycle_start_test.go`, `test/integration/m3_start_test.go` (`integration`)

**Interfaces:**
- Produces (signature §5.2 persis): `Cancel(ctx, userID, sessionID uuid.UUID) (Session, error)`; `Abandon(ctx, sessionID, actor Actor) (Session, error)`; `CancelInTx(ctx, tx Tx, sessionID uuid.UUID, actor Actor) (Session, error)`; `BeginStart(ctx, sessionID, fence int64) (Session, error)`; `Activate(ctx, sessionID, fence int64) (Session, error)`; `FailStart(ctx, sessionID, fence Fence, r EndReason, detail string, actor Actor) (Session, error)`; `type ControlMessage struct{Type, SessionID, Status, EndReason string; Version int64}` (JSON `status_changed`, §5.6.6).

- [ ] **Step 1: Write the failing test** — `TestCancel_ReleasesFullReservation` (saldo kembali, sesi `CANCELLED`, `charged 0`, event `SESSION_CANCELLED`, fence +1, control `status_changed` terkirim setelah commit); `TestCancel_Idempotent` (kedua kali sama, satu `RELEASE`); `TestCancel_NotReservedStatus_SessionNotCancellable` (`details.status`); `TestCancel_OtherUsersSession_404`; `TestAbandon_BeforeDeadline_Skips`, `TestAbandon_AfterDeadline_Releases` (`NOT_STARTED`, actor `CLEANUP`); `TestBeginStart_AIFirst_SelectsPublishedOpening` (event `SESSION_STARTING` payload berisi id), `TestBeginStart_AIFirst_NoPublishedOpening_NullAndWarn` (sesi `STARTING`, kolom NULL, payload `null`, log WARN `session.opening_prompt_missing`, **tidak** `ABANDONED`), `TestBeginStart_UserFirst_AlwaysNull`; `TestBeginStart_AfterDeadline` (fake clock +5m1s → `ErrStartDeadlinePassed` dan sesi `ABANDONED`, saldo kembali); `TestActivate_SetsStartedAndExpires` (`expires_at = started_at + duration`); `TestActivate_After10s` (→ `ErrStartTimeout`, sesi `FAILED`, charge 0, `PROVIDER_FAILURE`, tanpa outbox); `TestFailStart_ReleasesAndBumpsFence`. Integration `TestCancelVsBeginStart_Race` (paralel 50×: tepat satu menang, `AssertReconciled`) dan `TestCancelInTx_UsedByAdminTx`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run 'TestCancel|TestAbandon|TestBeginStart|TestActivate|TestFailStart'` → FAIL.
- [ ] **Step 3: Implement** — rutin privat `releaseAndClose` untuk T4/T5/T6 (urutan lock: baca `OwnerOf` pra-tx → lock account → `GetForUpdate` → lock reservasi → `ReleaseReservation` → CAS; idempoten bila sudah di status tujuan); `BeginStart` mengunci hanya sesi lalu reservasi (rank naik), membaca OPENING lewat `OpeningPromptSource` sebelum tx (deviasi kecil dari "dalam T2", catat); `ControlPublisher.Publish` dipanggil setelah commit, error di-log saja.
- [ ] **Step 4: Run tests to verify they pass** — unit PASS; `go test -tags integration ./test/integration -race -run 'TestCancelVsBeginStart|TestCancelInTx'` PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): cancel, abandon, start, activate and fail-start transitions"`

### Task 19: Transisi runtime — `ClaimRuntime`, `AddDisconnected`, `MarkHandoff`, `BeginEnding`, `Finalize`, `FailActive`

**Files:**
- Create: `internal/session/runtime_ops.go` (di `lifecycle.go` bila lebih ringkas)
- Test: `internal/session/lifecycle_end_test.go`, `test/integration/m3_end_test.go` (`integration`)

**Interfaces:**
- Produces (§5.2 persis): `ClaimRuntime(ctx, sessionID uuid.UUID, allowed []Status) (fence int64, sess Session, err error)`; `AddDisconnected(ctx, id, fence int64, seconds int) (Session, error)`; `MarkHandoff(ctx, id, fence int64) (Session, error)`; `BeginEnding(ctx, id, fence Fence, r EndReason, detail string, actor Actor) (Session, error)`; `Finalize(ctx, id, fence Fence, actor Actor) (Session, error)`; `FailActive(ctx, id, estimatedEnd time.Time, detail string) (Session, error)`; `SetSummary(ctx, id, text string) error`.

- [ ] **Step 1: Write the failing test** — unit (fake clock): `TestBeginEnding_SetsEndingAtCoalesceHandoff` (handoff_at 10:03:00 → `ending_at` 10:03:00); `TestBeginEnding_Noop_WhenAlreadyEnding_FirstReasonWins`; `TestBeginEnding_SweeperBumpsFence_RuntimeDoesNot`; `TestFinalize_USER_ENDED_Example11` (BeginEnding 10:07:00, Finalize 10:08:05 → `charged 420`, `ended_at` 10:08:05, `COMPLETED`); `TestFinalize_Example1` (421 / released 179); `TestFinalize_PROVIDER_FAILURE_Under60s_Charge0_FAILED_OutboxFinalStatusFailed`; `TestFinalize_TerminalIsIdempotent_BeforeFenceCheck`; `TestFinalize_StaleFenceInEnding_ErrFenceLost`; `TestFinalize_FailsDanglingTurns`; `TestFinalize_OutboxPayload` (`turn_count` mengabaikan `DISCARDED`, `user_turn_count`, `has_user_audio`, `producer REALTIME` vs `WORKER`); `TestFailActive_Example9` (estimatedEnd 10:05:00 → `ended_at` 10:05:00, `ending_at NULL`, charge 270, outbox `FAILED`); `TestFailActive_EstimatedEndAfterExpiry_ClampsToExpires`; `TestMarkHandoff` (+ `ClaimRuntime([ACTIVE])` → `handoff_at NULL`; `MarkHandoff` fence lama → `ErrFenceLost`; `version` tak berubah); `TestClaimRuntime_DisallowedStatus_ErrInvalidTransition`; `TestAddDisconnected` (12; episode melewati expiry 5 s → `n−5`; fence salah → `ErrFenceLost`; status RESERVED → `ErrInvalidTransition`). Integration: `TestFinalize_RuntimeVsSweeper_Race` (100 iterasi, dua goroutine: tepat satu `CONSUME` + satu `RELEASE`, tepat satu outbox `session.completed`, kedua pemanggil tidak error kecuali `ErrFenceLost` untuk runtime), `TestHandoffThenSweeperRuleE_Example10` (handoff_at 10:03:00; 10:03:31 → `RECONNECT_TIMEOUT`, charge 180, `ending_at = handoff_at`), `TestTakeover_OldFenceCannotWriteTurn`, `AssertReconciled` setelah semua.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run 'TestBeginEnding|TestFinalize|TestFailActive|TestMarkHandoff|TestClaim|TestAddDisconnected'` → FAIL.
- [ ] **Step 3: Implement** — settle di satu tx sesuai §4.6.3 langkah 1–10 (urutan lock account → sesi → reservasi → turns); `to_status = TerminalStatusFor(end_reason)`; `serviceFault = end_reason ∈ {PROVIDER_FAILURE, SERVER_ERROR}`; `Finalize` mengisi `ended_at = now`, `FailActive` mengisi `LEAST(estimatedEnd, expires_at)`; outbox via `tx.Outbox().Enqueue(events.NewSessionCompleted(...))`; payload event `SESSION_COMPLETED|FAILED` berisi `charged_seconds, released_seconds, active_ms, billable_end, disconnected_seconds` (+`estimated_end` untuk `FailActive`); control publish setelah commit.
- [ ] **Step 4: Run tests to verify they pass** — unit PASS; `go test -tags integration ./test/integration -race -run 'TestFinalize|TestHandoff|TestTakeover' -count=3` PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): runtime transitions, settlement and outbox"`

### Task 20: `session.RuntimeService` (fasad §7.13.1)

**Files:**
- Create: `internal/session/runtime.go`
- Test: `internal/session/runtime_test.go`

**Interfaces:**
- Produces (signature persis §7.13.1, dipenuhi `conversation.SessionRuntime` di M5):
```go
func NewRuntimeService(s *Service, r Repository) *RuntimeService
func (r *RuntimeService) GetForAttach(ctx, sessionID uuid.UUID) (RuntimeRecord, error)
func (r *RuntimeService) ClaimRuntime(ctx, sessionID uuid.UUID, allowed []Status) (fence int64, rec RuntimeRecord, err error)
func (r *RuntimeService) MarkStarting(ctx, sessionID uuid.UUID, fence int64) (RuntimeRecord, error)
func (r *RuntimeService) Activate(ctx, sessionID uuid.UUID, fence int64) (startedAt, expiresAt time.Time, err error)
func (r *RuntimeService) FailStart(ctx, sessionID uuid.UUID, fence int64, reason EndReason) (Settlement, error)
func (r *RuntimeService) AddDisconnected(ctx, sessionID uuid.UUID, fence int64, seconds int) error
func (r *RuntimeService) MarkHandoff(ctx, sessionID uuid.UUID, fence int64) error
func (r *RuntimeService) BeginEnding(ctx, sessionID uuid.UUID, fence int64, reason EndReason) error
func (r *RuntimeService) Complete(ctx, sessionID uuid.UUID, fence int64) (Settlement, error)
func (r *RuntimeService) Fail(ctx, sessionID uuid.UUID, fence int64, reason EndReason) (Settlement, error)
```
`RuntimeRecord` memuat: status, `UserID`, mode, `StartedAt/ExpiresAt *time.Time`, semua version ID, `VoiceProfileID`, `ProviderProfile`, `AdaptiveDifficulty json.RawMessage`, `OpeningPromptVersionID *uuid.UUID`, `RuntimeFence`, `AudioConsent bool` (JOIN users).

- [ ] **Step 1: Write the failing test** — `TestRuntimeService_SatisfiesConversationPort` (assertion kompilasi terhadap interface lokal salinan §7.13.1 di file test); `TestMarkStarting_ReturnsOpeningPromptID`; `TestComplete_ReturnsSettlement` (Charged/Disconnected/StartedAt/EndedAt/EndReason sama dengan baris sesi); `TestFail_ReasonMismatch_UsesStoredReasonAndLogsWarn`; `TestBeginEnding_NoopWhenAlreadyEnding`; `TestGetForAttach_AudioConsentFromUser`; `TestRuntimeCalls_WithStaleFence_ErrFenceLost` untuk `MarkStarting/Activate/AddDisconnected/MarkHandoff`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run TestRuntimeService|TestMarkStarting|TestComplete|TestFail_|TestGetForAttach|TestRuntimeCalls` → FAIL.
- [ ] **Step 3: Implement** — fasad tipis: `Complete/Fail` → `Finalize(id, &fence, REALTIME)` lalu bentuk `Settlement`; `FailStart` → `Service.FailStart(id,&fence,reason,"",REALTIME)`.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): RuntimeService facade per spec 7.13.1"`

### Task 21: `session.QueryService` (Get/List/Turns + 410)

**Files:**
- Create: `internal/session/query.go`, `internal/postgres/session_view_repo.go` (JOIN topics/scenarios/character_versions/reservations; sqlc), `db/queries/session_views.sql`
- Test: `internal/session/query_test.go`, `internal/postgres/session_view_integration_test.go`

**Interfaces:**
- Produces: `type View struct{Session; TopicName, ScenarioName, CharacterName string; AvatarKey *string; ConversationStyleID, ExpressionProfileID uuid.UUID; StartDeadlineAt time.Time}`; `type ViewRepository interface{GetView(ctx, userID, id uuid.UUID) (View, error); ListViews(ctx, userID uuid.UUID, statuses []Status, after *uuid.UUID, limit int) ([]View, error)}`; `NewQueryService(v ViewRepository, r Repository, c clock.Clock, retentionDays int) *QueryService`; `Get(ctx, userID, id) (View, error)`; `List(ctx, userID, statuses []Status, after *uuid.UUID, limit int) ([]View, error)`; `Turns(ctx, userID, id uuid.UUID, afterSeq *int, limit int) ([]TurnRecord, error)`.

- [ ] **Step 1: Write the failing test** — `TestGet_OtherUser_404SessionNotFound`; `TestGet_NameSources` (`ScenarioName`/`TopicName` dari induk, `CharacterName`+`AvatarKey` dari version snapshot walau version terbaru berbeda); `TestList_StatusFilterAndKeyset` (`id DESC`, limit+1); `TestTurns_OnlyCompletedInterruptedFailed` (turn `DISCARDED` dan berjalan tidak muncul); `TestTurns_ActiveSessionAllowed`; `TestTurns_410_WhenPurgedAt`; `TestTurns_410_WhenEndedBeforeRetention` (fake clock; `ended_at = now−181d` → 410, 179d → 200); `TestTurns_ReservedSession_Empty` (tanpa `starting_at` → daftar kosong tanpa query); `TestTurns_PruningBounds` (fake repo menangkap `startingAt−1m` dan `endedAt+1m`); integration: `TestListViews_UsesOwnerFilter`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run 'TestGet|TestList|TestTurns'` → FAIL.
- [ ] **Step 3: Implement** — pembatasan turns per sesi memakai `created_at` bounds (§5.3.2); invalid UUID path ditangani handler (404).
- [ ] **Step 4: Run tests to verify they pass** — unit PASS; `go test -tags integration ./internal/postgres/... -race -run TestListViews` PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): query service for session views and transcript"`

### Task 22: `session.Sweeper` (aturan a–e + guard) + adapter Redis

**Files:**
- Create: `internal/session/sweeper.go`, `internal/session/guard.go`, `internal/redisstore/heartbeat_reader.go`, `internal/redisstore/session_control.go`
- Test: `internal/session/sweeper_test.go`, `internal/session/guard_test.go`, `internal/redisstore/heartbeat_integration_test.go`, `test/integration/m3_sweeper_test.go` (`integration`)

**Interfaces:**
- Produces: `type Sweeper struct`; `func NewSweeper(s *Service, r Repository, hb HeartbeatReader, c clock.Clock, log *slog.Logger) *Sweeper`; `func (w *Sweeper) Tick(ctx) (SweepReport, error)` dengan `SweepReport{Rule map[string]RuleResult}` (`RuleResult{OK, Noop, Error int; Skipped bool}`); `type HeartbeatGuard struct`; `func (g *HeartbeatGuard) Observe(ok bool, now time.Time)`; `func (g *HeartbeatGuard) Enabled(now time.Time) bool`; `redisstore.NewHeartbeatReader(rdb) session.HeartbeatReader` (MGET `kaiwa:session:{id}:hb`, nilai epoch ms, key hilang → tidak ada di map); `redisstore.NewSessionControl(rdb) session.ControlPublisher` (PUBLISH `kaiwa:session:{id}:control`). Urutan per tick: (e) (a) (c) (d) (b). Metrik `kaiwa_sweeper_actions_total{rule,result}` lewat `metrics` M1 bila API-nya tersedia; selain itu hitung di `SweepReport` (job M6 yang mencatat metrik).

- [ ] **Step 1: Write the failing test** — fake clock, tiap aturan: (a) `expires_at+14s` tidak diproses, `+16s` → `DURATION_ELAPSED`+`COMPLETED`, charge dipotong di `expires_at` (contoh 7: disconnected 20 → 280); (c) `ENDING` 59 s tidak, 61 s → Finalize; (d) `STARTING` 29 s tidak, 31 s → `FAILED` `SERVER_ERROR`, charge 0, tanpa outbox; (e) `handoff_at` 29 s tidak, 31 s → `ending_at = handoff_at`, `RECONNECT_TIMEOUT`, charge contoh 10; (b) heartbeat 10:05:00 → `FailActive(estimatedEnd=10:05:00)` contoh 9; key hilang → `now−45s` minimal `started_at`; sesi `handoff_at NOT NULL` tanpa heartbeat tidak pernah kena (b); guard massal: 30 kandidat dari 40 `ACTIVE` non-handoff → (b) dilewati; 30 sesi handoff tidak ikut hitung; 19 kandidat dari 20 → tetap diproses; `HeartbeatGuard`: Redis error → disabled; pulih 30 s → disabled, 46 s → enabled, error lagi → reset; saat reader error aturan (a)(c)(d)(e) tetap jalan; error satu sesi tidak menghentikan loop; batas 200/aturan. Integration Redis: `TestHeartbeatReader_MGet` (key ada/hilang), `TestSessionControl_Publish` (subscriber menerima JSON `status_changed`); skenario Postgres+Redis: runtime "mati" (heartbeat berhenti) → (b) menyelesaikan, `AssertReconciled`, satu outbox.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/session/... -race -run 'TestSweeper|TestGuard'` → FAIL.
- [ ] **Step 3: Implement** — query kandidat di `Repository` (Task 14) memakai SQL §5.6.4; aksi lewat `Service` (fence nil, actor `SWEEPER`); (e) `BeginEnding(RECONNECT_TIMEOUT,"handoff_not_resumed")` lalu `Finalize`; (b) `FailActive(est,"heartbeat_lost")`; guard tidak berbasis wall-clock (selalu `clock.Clock`).
- [ ] **Step 4: Run tests to verify they pass** — unit PASS; `go test -tags integration ./internal/redisstore/... ./test/integration -race -run 'TestHeartbeat|TestSessionControl|TestSweeper'` PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(session): sweeper rules a-e with heartbeat guard and redis adapters"`

### Task 23: REST sesi & balance + ticket + `TestRouteTable` grup User

**Files:**
- Create: `internal/httpapi/session_{routes,handler,dto}.go`, `balance_{routes,handler,dto}.go`, `testdata/api/rest/{sessions,balance}/*.json`
- Modify: `internal/httpapi/router.go`, `deps.go`
- Test: `internal/httpapi/session_handler_test.go`, `balance_handler_test.go`, `route_table_test.go`

**Interfaces:**
- Consumes: `session.Service`, `session.QueryService`, `balance.Service`, M2 `auth.TicketService` (nama method dari M2: `Issue(ctx, principal, sessionID)`; adaptasi bila berbeda).
- Produces: `registerSessionRoutes(user, h)` (6 route §5.8.6), `registerBalanceRoutes(user, h)` (2 route); `Deps.Session`, `Deps.SessionQuery`, `Deps.Balance`, `Deps.Tickets`; DTO `sessionResponse` (field persis §5.9.4: `topic{id,name}`, `scenario{id,name}`, `character{id,name,avatar_key}`, `remaining_seconds = max(0, ceil(expires_at − server_time))` hanya `ACTIVE`, `summary`, `server_time`), `createSessionRequest` (`Validate()`; field asing → `unknown_field`), `Location` header, `Idempotent-Replayed: true` pada replay (status 200 vs 201).

- [ ] **Step 1: Write the failing test** — (a) golden JSON: `POST /sessions` 201 + `Location: /api/v1/sessions/{id}` (status `RESERVED`), replay 200 + `Idempotent-Replayed: true` + body sesi saat ini; `GET /sessions/{id}` memuat `scenario.name`, `character.avatar_key`, tanpa `title`/`avatar_url`/`adaptive_difficulty`/`runtime_fence`; list `{"data":[…],"next_cursor":…}`; turns `{"data":…}` + 410; balance & transactions (field §5.9.5, tanpa `reason`); (b) error: tanpa header → 400 `IDEMPOTENCY_KEY_REQUIRED`; body memuat `version_id` → 400 `VALIDATION_FAILED` (`unknown_field`); UUID path invalid → 404 `SESSION_NOT_FOUND`; sesi user lain → 404; `limit=101` → 400; cursor rusak → 400 `INVALID_CURSOR`; `entry_type=FOO` → 400; cancel idempotent 200 dan 409 `SESSION_NOT_CANCELLABLE`; ticket: 200 `{session_id, ticket, expires_in:30, realtime_url}` dan 409 `SESSION_NOT_CONNECTABLE`/`SESSION_START_DEADLINE_PASSED` dipetakan dari `auth.TicketService`; (c) `TestRouteTable_UserGroup`: (method, path, grup) seluruh route milik M3 = `POST|GET /sessions`, `GET /sessions/{session_id}`, `POST …/cancel`, `POST …/realtime-ticket`, `GET …/turns`, `GET /balance`, `GET /balance/transactions` + 7 route katalog (Task 13) — semua grup User; tidak ada route berlebih pada prefix tsb (evaluation/profile/me ditambahkan milik M2/M7/M8 ke tabel harapan yang sama).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/httpapi/... -race -run 'TestSession|TestBalance|TestRouteTable'` → FAIL.
- [ ] **Step 3: Implement** — handler memanggil `Create` lalu `QueryService.Get` untuk DTO; rate rule `session_create_user`/`ticket_user`/`api_user_default` dipasang di `router.go` per route sesuai §5.8.5.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(httpapi): session and balance endpoints with route table test"`

### Task 24: Skenario lifecycle lintas paket + wiring + rekonsiliasi akhir

**Files:**
- Create: `test/integration/m3_lifecycle_test.go` (`integration`)
- Modify: `cmd/api/main.go` (wiring: `content.*`, `balance.Service`, `session.Service`, `QueryService`, `postgres.Store`, `redisstore` control/heartbeat, `httpapi.Deps`; `cmd/worker/main.go` tidak diubah — job M6), `internal/platform/config/*.go` (tambah `CONTENT_CACHE_TTL_SECONDS`=30, `RETENTION_TRANSCRIPT_DAYS`=180, `PRIVACY_POLICY_VERSION`, `REALTIME_PUBLIC_URL` bila belum ada)
- Test: file di atas

**Interfaces:**
- Consumes: seluruh Task 1–23 melalui HTTP (httptest server dengan wiring nyata, Postgres+Redis testcontainers, fake clock, fake `AdaptiveProvider`, seed konten) dan `session.RuntimeService` langsung.

- [ ] **Step 1: Write the failing test** — (1) `TestLifecycle_HappyPath_AIFirst` (§5.11): register-grant saldo 1200 → `POST /sessions` (HARUKA + BACKEND_ENGINEER_INTERVIEW, 600 s) → ticket → `ClaimRuntime([RESERVED])` → `MarkStarting` (opening id terisi) → `Activate` → `BeginEnding(USER_ENDED)` pada +420,4 s → `Complete` → `COMPLETED`, `charged=421`, saldo 779/0, tepat 1 outbox `session.completed final_status=COMPLETED`, event berurutan `SESSION_RESERVED, SESSION_STARTING, SESSION_STARTED, SESSION_ENDING, SESSION_COMPLETED`; (2) `TestLifecycle_OpeningFallback` (hapus/retire OPENING → `STARTING`, `opening_prompt_version_id NULL`); (3) `TestLifecycle_ProviderFailure` (`BeginEnding(PROVIDER_FAILURE)` pada 45 s → `Fail` → `FAILED`, charge 0, outbox `FAILED`; `STARTING→FAILED` tanpa outbox); (4) `TestLifecycle_DoubleClick` (Review Focus 1 lewat HTTP: 2× POST kunci sama paralel → 201 + 200 replay, sesi sama, 1 `RESERVE`; kunci sama + `duration_seconds` beda → 409 `IDEMPOTENCY_KEY_REUSED`; kunci berbeda → 409 `SESSION_ALREADY_OPEN`); (5) `TestLifecycle_PodDies_SweeperSettlesOnce` (ACTIVE tanpa heartbeat → sweeper (b); `session.completed` sekali; runtime lama `Complete` → idempotent/`ErrFenceLost`); (6) `TestLifecycle_AbandonedReservation` (deadline +5m → `Abandon` → saldo kembali); (7) `TestLifecycle_TranscriptAccess` (turns 200 saat `ACTIVE`, 410 setelah 181 hari); (8) `TestNoOrphanReservation_AfterAllScenarios`: setelah semua subtest di DB yang sama → `AssertReconciled` (R1–R5 kosong) **dan** R6 kosong dengan `now()` normal, serta tidak ada reservasi `HELD` untuk sesi terminal.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration -race -run 'TestLifecycle|TestNoOrphan'` → FAIL (wiring/config belum ada).
- [ ] **Step 3: Implement** — hanya wiring `cmd/api/main.go` dan config; perbaikan bug yang ditemukan dicatat di `docs/implementation-log.md` dan diperbaiki di task pemilik (commit terpisah `fix(...)`).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make test-integration` penuh PASS.
- [ ] **Step 5: Commit** — `git commit -m "test(session): lifecycle integration scenarios and api wiring"`

---

## Gerbang milestone

- [ ] `go build ./... && CGO_ENABLED=0 go build ./cmd/api ./cmd/worker` → sukses.
- [ ] `make lint && make arch-check` → 0 issue (depguard: `content → prompt` saja; `session` tanpa `pgx/go-redis`; `balance` tanpa impor `session`).
- [ ] `make test` (`go test ./... -race`) → PASS; coverage `balance/charge.go` dan `session/transition.go` 100 %.
- [ ] `make test-integration` → PASS, termasuk `TestLifecycle_*`, `TestCreate_20Parallel_*`, `TestFinalize_RuntimeVsSweeper_Race -count=3`, `TestSweeper*`, `TestSeedContentIsValid`, `TestPublishConcurrent -count=20`.
- [ ] `make seed && make seed` (DB lokal) → keduanya sukses, jumlah baris tabel konten tidak berubah.
- [ ] `AssertReconciled` (R1–R5) kosong di akhir setiap integration test; `TestNoOrphanReservation_AfterAllScenarios` PASS (Gerbang "rekonsiliasi §4.8 bersih").
- [ ] `go test ./internal/httpapi/... -race -run TestRouteTable` → PASS untuk grup User (+ Admin `/admin/content`).
- [ ] Review Focus 1 dibuktikan oleh `TestCreate_20Parallel_SameKey` dan `TestLifecycle_DoubleClick`.
- [ ] Catat perintah + ringkasan output di `docs/implementation-log.md`; deviasi di `docs/implementation-deviations.md`.

## Self-Review

**Cakupan spec.** Dicakup: §4.2, §4.4–§4.9; §5.2–§5.7, §5.8.3–§5.8.6 (route/handler sesi & balance), §5.9.3–§5.9.5, §5.10 (kode M3), §5.11; §6.2–§6.5, §6.8.1–§6.8.3 (mekanisme + seed template), §6.9–§6.14 (kecuali tes DB §6.14 #1–2 milik M1); §7.13.1; §9.3.1 (port + writer minimal). **Tidak dicakup (milik lain):** grant/adjust/registrasi/`/me` (M2), publisher/topologi/job scheduler `session_sweeper`/`reservation_cleanup`/`balance_reconcile` + metrik job (M6; M3 menyediakan `Sweeper.Tick`, `Service.Abandon`, `Balance.Reconcile`), `ResolveTTSParams` & provider (M4), `EstimateTokens/SentenceParser/phrases/core.go` (M4/M5), runtime WS (M5), evaluation/profile endpoint (M7/M8), template SUMMARY/EVALUATION/MEMORY_EXTRACTION (M7/M8), endpoint admin sesi `/admin/users/{id}/sessions`, `/admin/sessions/*` (§5.9.8 — **tidak ada pemilik di master; usulan: ditambahkan ke M2 atau M10 memakai `session.QueryService`**), `GetUserVoiceExposure` hanya query (dipakai M8).
**Konsistensi tipe.** `balance.SettleInput/SettleResult` (Task 3) dipakai Task 19; `session.Deps` (Task 17) dipakai Task 18–22; `events.NewSessionCompleted` (Task 15) dipakai Task 19; `RuntimeRecord/Settlement` (Task 14) dipakai Task 20; `ControlPublisher/HeartbeatReader` (Task 14) diimplementasikan Task 22; `AssertReconciled` (Task 5) dipakai Task 17–24; `Fixtures/Render` (Task 6) dipakai Task 8 & 12.
**Deviasi yang harus dicatat:** (1) `OpeningPromptSource` dibaca sebelum tx `BeginStart`; (2) `ValidateCatalog` memperlakukan prompt non-CONVERSATION tanpa PUBLISHED sebagai WARNING; (3) spec §6.8 vs master M4 untuk `prompt.Render` (Task 6); (4) master Review Focus #1 menyebut "sesi kedua tanpa key → 409" sedangkan spec §5.8.4 mewajibkan key (tanpa key = 400 `IDEMPOTENCY_KEY_REQUIRED`; key berbeda = 409 `SESSION_ALREADY_OPEN`) — plan mengikuti spec.
