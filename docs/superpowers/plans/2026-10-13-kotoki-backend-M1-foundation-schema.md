# Kotoki Backend — M1 Foundation & Schema Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Repo Go ter-bootstrap penuh (tooling, CI, compose, Docker), paket `internal/platform/*`, `postgres.Store`, kerangka tiga binary yang `/healthz` `/readyz` `/metrics`-nya hidup, dan seluruh 18 migration (24 tabel) yang teruji up/down/up.
**Architecture:** `internal/platform/*` hanya bergantung ke stdlib + library infrastruktur (bukan paket fitur); `internal/postgres` dan `internal/redisstore` adalah adapter yang hanya diimpor `cmd/*` dan test. Semua error lewat `errs.New/Wrap` (lokasi asal terekam), semua span lewat `otel.Start/End`, semua waktu/ID lewat `clock`/`idgen`. Skema DB adalah satu-satunya sumber kebenaran sqlc; M1 hanya membuat struktur `db/queries` + satu query `Ping`, repository fitur dibuat milestone fiturnya.
**Tech Stack:** Go 1.25, caarlos0/env v11, log/slog, OpenTelemetry SDK + OTLP gRPC (trace), prometheus/client_golang, pgx v5 + pgxpool, sqlc (directive `tool`), golang-migrate (CLI di Makefile, library di test), go-redis v9, google/uuid (v7), testify, testcontainers-go (modul postgres, redis), golangci-lint v2.
**Spec:** docs/superpowers/specs/2026-10-12-kotoki-backend-design.md — §2.1–2.3 & D-001..D-013, §2.2 (layout/aturan paket), §3.3, §4.2–4.3, §4.6.0, §5.3, §5.10, §5.11, §6.4, §7.4.8, §9.2, §9.9.10, §10.2, §11.3, §12.2, §13.2, §13.3, §13.7, §13.10.4/8/9, §14, §15.1
**Master plan:** docs/superpowers/plans/2026-10-13-kotoki-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** — (milestone pertama). Branch: `feat/m1-foundation-schema` dari `main`.

## Global Constraints

- Master Global Constraints berlaku. Setiap Step 5 (Commit) juga menambah entri `docs/implementation-log.md` (format §14.9.4); penyimpangan → `docs/implementation-deviations.md`.
- Pin versi: golangci-lint `v2.1.6`, migrate image `migrate/migrate:v4.18.3`, sqlc `v1.29.0` (`go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0`), govulncheck `v1.1.4`; image compose `pgvector/pgvector:pg17`, `redis:7.4`, `rabbitmq:4-management`, `minio/minio:RELEASE.2025-04-22T22-12-26Z`, `otel/opentelemetry-collector-contrib:0.120.0`, `grafana/otel-lgtm:<PIN>` (resolve + catat di implementation-log).
- Migration `000001`–`000018` nama & urutan §15.1 (D-014: konten disederhanakan, 24 tabel); `reject_mutation()` dibuat di `000013` (bukan `000018`, §3.3.3); `content_version_guard()` dibuat di `000006` dan hanya dipasang pada `scenario_versions` dan `prompt_versions`; `characters` (TTS utama = kolom `provider_name`/`model_name`/`voice_id`, atribut non-vendor inline `voice JSONB`, D-015) tidak berversi dan tanpa trigger guard; tiap `.down.sql` membalik `.up.sql` sepenuhnya.
- `session_turns`/`session_events`: PARTITION BY RANGE (`created_at`), nama `{tabel}_pYYYYMM`, batas bulan UTC, 4 partisi awal (bulan berjalan + 3), tanpa `DEFAULT`, PK `(id, created_at)`, tanpa FK yang menunjuk ke keduanya (§5.3.2, §9.9.10).
- Transaksi `postgres.Store`: `READ COMMITTED`, `SET LOCAL lock_timeout='3s'`, `SET LOCAL statement_timeout='5s'`; retry maks 3× untuk SQLSTATE `40P01`/`40001` dengan backoff 20 ms, 50 ms, 100 ms ± 50% jitter; `55P03` → `SERVICE_UNAVAILABLE` (§4.6.0).
- Pool default (§13.2.3): `DB_MAX_CONNS` api 10 / realtime 6 / worker 4; `DB_MIN_CONNS` 2/2/1; `DB_STATEMENT_TIMEOUT` api,realtime `5s` / worker `30s`; `DB_LOCK_TIMEOUT 3s`; `DB_IDLE_IN_TX_TIMEOUT 15s`; `DB_MAX_CONN_LIFETIME 30m`; `DB_MAX_CONN_IDLE_TIME 5m`; `DB_CONNECT_TIMEOUT 5s`; `application_name` = `kotoki-api|kotoki-realtime|kotoki-worker`.
- Listener ops (§13.2.2/§14.10.5): `OPS_ADDR` api `:9090`, realtime `:9091`; `WORKER_HTTP_ADDR` worker `:8081` (Makefile `run-worker` → `:9092`); `HTTP_ADDR` api `:8080`, realtime `:8081`. `.env.example` tidak boleh men-set variabel yang default-nya beda per binary.
- Redaksi kunci (case-insensitive, nested): `password,new_password,token,access_token,refresh_token,ticket,authorization,cookie,set-cookie,api_key,secret,email,phone,user_text,assistant_text,assistant_spoken_text,transcript,partial,text,prompt,audio`; `LOG_TRANSCRIPTS=true` hanya membebaskan `user_text,assistant_text,assistant_spoken_text,transcript,partial,text,prompt`.
- Bucket: `HTTP_BUCKETS=[0.005,0.01,0.025,0.05,0.1,0.2,0.3,0.5,1,2.5,5]`, `TURN_BUCKETS=[0.1,0.2,0.3,0.4,0.5,0.6,0.8,1,1.25,1.5,2,3,5,10]`, `BARGE_BUCKETS=[0.01,0.025,0.05,0.075,0.1,0.15,0.2,0.3,0.5,0.6,1]`, `ASYNC_BUCKETS=[0.1,0.5,1,2,5,10,20,30,60,120,300,600]`, acquire-wait `[0.001,0.005,0.01,0.05,0.1,0.5,1]`.
- `/readyz` (§13.10.4): `starting` sampai pemeriksaan startup (Ping PG, Redis) sukses sekali; setelah ready gangguan dependency tidak membuat unready; tidak pernah memanggil provider.
- Kind error: kode REST 4xx = `KindBusiness`, 5xx = `KindInternal`; kode WS-saja: `PROVIDER_FAILURE`, `AI_GENERATION_FAILED` = internal, sisanya business (nama dari plan).

## Review Focus

1. **Down migration benar-benar membalik up** (fungsi, trigger, partisi, FK `ALTER` di `000016`): tidak ada tabel/fungsi/extension tersisa setelah full down; step `+1/-1/+1` per versi — Task 11 (harness), Task 16 (`000016` FK), Task 19 (stepwise + golden).
2. **Partisi aman**: INSERT bulan depan masuk partisi benar, di luar rentang gagal, tanpa `DEFAULT`, tanpa FK menuju turns/events — Task 14.
3. **Backstop DB untuk klik ganda & saldo** (master #1, #4): `ux_sessions_one_open_per_user`, `ux_sessions_user_idempotency`, `ux_balance_ledger_idempotency`, CHECK saldo ≥ 0, reservasi satu per sesi, ledger/audit append-only — Task 12, 14, 15, 17.
4. **Tidak ada teks user/AI/secret di log & span**: `RedactingHandler`, redaksi atribut `otel.Start/End`, `config.Secret`, `LOG_TRANSCRIPTS` ditolak di staging/prod — Task 4, 5, 7.
5. **`WithinTx` retry & timeout**: 40P01/40001 diulang tepat 3×, lock timeout → `SERVICE_UNAVAILABLE`, panic me-rollback — Task 21.

## File Structure

| Path | Tanggung jawab |
|---|---|
| `go.mod`, `Makefile`, `.env.example`, `.gitignore`, `.golangci.yml`, `sqlc.yaml`, `docker-compose.yml` | tooling & dev env (D-006, §14.9.5, §14.10) |
| `.github/workflows/ci.yml` | CI §14.12.1 |
| `deploy/docker/{api,realtime,worker,migrate}.Dockerfile`, `deploy/docker/otel-collector.yaml` | image & collector lokal |
| `internal/platform/buildinfo` | `Version`, `GitCommit` (ldflags) |
| `internal/platform/errs` | `Code`, `Kind`, `Location`, `New/Wrap`, `Messages`, `WithDetails/DetailsOf` |
| `internal/platform/{clock,idgen}` | port waktu & UUIDv7 + fake |
| `internal/platform/config` | struct config per binary + `Validate()` + `Secret` |
| `internal/platform/logging` | slog JSON + `RedactingHandler` (direktori `logging`, bukan `log`, lihat Self-Review) |
| `internal/platform/otel` | `Start/End`, `Setup` tracer provider |
| `internal/platform/metrics` | registry, bucket, metrik DB/Redis |
| `internal/platform/health` | listener ops `/healthz /readyz /metrics /debug/pprof` |
| `internal/postgres` (+`sqlcgen/`) | `NewPool`, pgx tracer, `Store`/`Tx` |
| `internal/redisstore` | `NewClient`, hook tracing |
| `internal/testutil` (+`fakes/`, `repotest/`) | container helper, `TestdataPath`, `NewTraceRecorder`, fixture DB |
| `migrations/` | 18 migration + `migrations_integration_test.go` + `testdata/schema.golden.sql` |
| `db/queries/system.sql` | query `Ping` |
| `cmd/{api,realtime,worker}/main.go` | kerangka wiring |
| `test/tools/{modelcheck,covercheck,migcheck}`, `test/arch` | alat CI & tes arsitektur |

---

### Task 1: Bootstrap repo, Makefile, buildinfo

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`, `internal/platform/buildinfo/buildinfo.go`, `docs/implementation-log.md`, `docs/implementation-deviations.md`, `.gitkeep` di `seed/`, `testdata/audio/`, `testdata/api/rest/`, `deploy/k8s/`
- Test: `internal/platform/buildinfo/buildinfo_test.go`, `test/arch/make_targets_test.go`

**Interfaces:**
- Produces: `buildinfo.Version string` (default `"dev"`), `buildinfo.GitCommit string` (default `"unknown"`), `func String() string` → `"<Version> (<GitCommit>)"`.

- [ ] **Step 1: Write the failing test** — `TestDefaults` (`Version=="dev"`, `GitCommit=="unknown"`, `String()=="dev (unknown)"`). `TestMakeTargetsExist` di `test/arch` menjalankan `make -n <t>` untuk setiap target D-006 (`up down migrate-up migrate-down sqlc test test-integration lint run-api run-realtime run-worker`) + §14.10.4 (`env up-obs reset migrate-create seed promote-admin sqlc-check tidy-check migrations-check modelname-check arch-check test-cover test-e2e test-live vuln build cli docker-build load`) dan `require.NoError` untuk tiap target.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/buildinfo/... ./test/arch/... -race -run 'TestDefaults|TestMakeTargetsExist'`. Expected: FAIL (paket/Makefile belum ada).
- [ ] **Step 3: Implement** — `go mod init github.com/nabePi/kotoki-backend`, `go 1.25`; `go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc@v1.29.0`. `Makefile` disalin **persis** dari §14.10.4 (blok ```make```), `.gitignore`: `.env`, `bin/`, `out/`, `coverage.out`. Dua file docs berisi judul + format entri §14.9.4. Dependensi lain di-`go get` oleh task pemiliknya.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go build ./... && go vet ./...` bersih.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "chore(repo): bootstrap go module, Makefile, buildinfo"`.

### Task 2: `internal/platform/errs`

**Files:**
- Create: `internal/platform/errs/{codes.go,messages.go,errs.go,location.go}`
- Test: `internal/platform/errs/errs_test.go`

**Interfaces:**
- Produces:
  `type Code string`; konstanta untuk **70 kode** dengan aturan nama kanonik mekanis **`Code` + PascalCase dari string kode, tiap segmen `_` diawali huruf kapital dan sisanya huruf kecil, TANPA pengecualian** (mis. `INVALID_JSON`→`CodeInvalidJson`, `AI_GENERATION_FAILED`→`CodeAiGenerationFailed`, `SESSION_NOT_ENDED`→`CodeSessionNotEnded`, `EVALUATION_NOT_AVAILABLE`→`CodeEvaluationNotAvailable`, `ORIGIN_NOT_ALLOWED`→`CodeOriginNotAllowed`, `IDEMPOTENCY_KEY_REUSED`→`CodeIdempotencyKeyReused`, `SESSION_ALREADY_OPEN`→`CodeSessionAlreadyOpen`, `RATE_LIMITED`→`CodeRateLimited`); plan milestone lain MUST memakai aturan ini (tidak ada nama tak beraturan). Daftar kode: seluruh 53 baris §5.10 + 17 kode WS-saja §7.4.8 (`TICKET_INVALID, HANDSHAKE_TIMEOUT, SESSION_STATE_CONFLICT, SESSION_NOT_RESUMABLE, SESSION_TAKEN_OVER, SERVER_DRAINING, SLOW_CONSUMER, HEARTBEAT_TIMEOUT, PROVIDER_FAILURE, SERVER_AT_CAPACITY, PROTOCOL_VIOLATION, UNSUPPORTED_PROTOCOL_VERSION, UNSUPPORTED_AUDIO_FORMAT, INVALID_MESSAGE, COMMAND_REJECTED, AUDIO_FRAME_INVALID, AI_GENERATION_FAILED`); contoh `CodeBalanceInsufficient`, `CodeInternalError`, `CodeServiceUnavailable`, `CodeValidationFailed`.
  `func AllCodes() []Code` (terurut). `var Messages map[Code]string` (teks Indonesia verbatim §5.10/§7.4.8).
  `type Kind int` (`KindBusiness`, `KindInternal`); `type Location struct{ Function, File string; Line int }` (File relatif root modul).
  `func New(code Code, msg string) error`; `func Wrap(err error, code Code, msg string) error` (mengimplementasikan `Unwrap`); `func CodeOf(err error) Code` (`""` bila tak ada); `func KindOf(err error) Kind` (tanpa kode/tak dikenal → `KindInternal`); `func LocationOf(err error) (Location, bool)`; `func WithDetails(err error, details any) error`; `func DetailsOf(err error) any`.

- [ ] **Step 1: Write the failing test** — table-driven (§14.2):
  - `TestNewRecordsCallerLocation`: `loc, ok := LocationOf(New(CodeBalanceInsufficient,"x"))` → `ok`, `loc.File=="internal/platform/errs/errs_test.go"`, `loc.Line` = baris pemanggil, `loc.Function` memuat `TestNewRecordsCallerLocation`.
  - `TestLocationThroughWrapLayers`: `e1 := errors.New("pgx")`; `w := Wrap(e1, CodeInternalError, "insert")` (baris A); `fmt.Errorf("svc: %w", fmt.Errorf("repo: %w", w))` → `LocationOf` = baris A; `Wrap(w, CodeEmailAlreadyRegistered, "y")` kedua **tidak** menimpa lokasi; `errors.Is(w, e1)`; `CodeOf` melewati `%w`.
  - `TestKindOfAllCodes`: untuk `AllCodes()` → `KindInternal` hanya untuk `{INTERNAL_ERROR, CONTENT_MISCONFIGURED, SERVICE_UNAVAILABLE, PROVIDER_FAILURE, AI_GENERATION_FAILED}`, selain itu `KindBusiness`; `KindOf(errors.New("x")) == KindInternal`; `CodeOf(errors.New("x")) == ""`.
  - `TestCodeConstantNames`: tabel **70 pasang** `{const, string}` ditulis eksplisit (mis. `{CodeInvalidJson,"INVALID_JSON"}`, `{CodeAiGenerationFailed,"AI_GENERATION_FAILED"}`, `{CodeOriginNotAllowed,"ORIGIN_NOT_ALLOWED"}`) dan `string(const)==kode` + nama konstanta (via `go/ast` parse `codes.go`) == `"Code"+PascalCase(kode)`; himpunan pasangan = `AllCodes()`.
  - `TestMessagesComplete`: `len(AllCodes())==70`, `len(Messages)==70`, tiap pesan non-kosong; `Messages[CodeBalanceInsufficient]=="Saldo menit Anda tidak cukup untuk durasi yang dipilih."`, `Messages[CodeInternalError]=="Terjadi kesalahan pada sistem. Silakan coba lagi."`, `Messages[CodeValidationFailed]=="Data yang dikirim tidak valid. Periksa kembali isian Anda."`, `Messages[CodeTicketInvalid]=="Koneksi tidak valid atau sudah kedaluwarsa. Menyambungkan ulang…"`.
  - `TestDetails`: `DetailsOf(WithDetails(New(...), map[string]any{"available_seconds":600}))` melewati wrap; `DetailsOf(nil)==nil`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/errs/... -race`. Expected: FAIL (compile error, paket belum ada).
- [ ] **Step 3: Implement** — `location.go`: root modul dihitung dari `runtime.Caller(0)` (buang suffix `internal/platform/errs/location.go`); `New/Wrap` memakai `runtime.Caller(1)`; `Wrap` merekam lokasi hanya bila `LocationOf(err)` belum ada. Tipe `*Error{code, msg, loc, details, cause}`; `Error()` = `msg` (+ `": "+cause`). `messages.go` salin teks dari §5.10 dan §7.4.8 (kode yang ada di keduanya memakai teks identik).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/errs/... -race -cover` → PASS, coverage ≥ 85%.
- [ ] **Step 5: Commit** — `git commit -m "feat(errs): error codes, kinds, origin location, messages"`.

### Task 3: `clock` dan `idgen`

**Files:**
- Create: `internal/platform/clock/clock.go`, `internal/platform/idgen/idgen.go`
- Test: `internal/platform/clock/clock_test.go`, `internal/platform/idgen/idgen_test.go`

**Interfaces:**
- Produces: `clock.Clock interface{ Now() time.Time }`; `clock.System{}` (UTC); `clock.NewFake(t0 time.Time) *clock.Fake` dengan `Now() time.Time`, `Advance(d time.Duration)`, `Set(t time.Time)` (ketiganya aman konkuren; nama ini kanonik untuk plan lain).
  `idgen.Generator interface{ New() uuid.UUID }`; `idgen.NewV7() Generator` (UUIDv7 acak, `github.com/google/uuid`); `idgen.NewSequence(t testing.TB) Generator` (deterministik, naik, UUIDv7 valid).

- [ ] **Step 1: Write the failing test** — `TestFakeClockAdvance` (`NewFake(t0).Advance(90*time.Second).Now() == t0+90s`, lokasi UTC), `TestFakeClockConcurrent` (`-race`, 50 goroutine `Advance`). `TestSequenceDeterministic` (dua `NewSequence` → 100 ID pertama identik), `TestSequenceMonotonic` (`id[i].String() < id[i+1].String()`, `Version()==7`, `Variant()==uuid.RFC4122`), `TestV7MonotonicByTime` (1000 ID unik, `Version()==7`), `TestSequenceConcurrent` (semua unik).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/clock/... ./internal/platform/idgen/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — fake sequence: counter atomik `n` mulai 1; byte 0–5 = `1760000000000+n` (ms, big-endian), nibble versi 7, varian RFC4122, 8 byte terakhir = `n`. `System.Now` = `time.Now().UTC()` (diizinkan: `internal/platform` dikecualikan `forbidigo`).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(platform): clock and idgen ports with fakes"`.

### Task 4: `internal/platform/config`

**Files:**
- Create: `internal/platform/config/{secret.go,config.go,validate.go,handlers.go}`, `internal/testutil/env.go` (tanpa build tag, stdlib saja; paket `testutil` dibuat di sini, file lain di Task 6)
- Test: `internal/platform/config/config_test.go`

**Interfaces:**
- Produces: `testutil.MinimalEnv(t testing.TB, overrides ...map[string]string) map[string]string` (env minimal valid untuk **semua** binary: `APP_ENV=local`, `DB_URL=postgres://u:p@localhost:5432/kotoki?sslmode=disable`, `REDIS_URL=redis://localhost:6379/0`; override digabung berurutan, nilai `""` menghapus kunci) dan `testutil.ApplyEnv(t testing.TB, env map[string]string)` (`t.Setenv` tiap kunci). **Satu-satunya tempat** daftar env wajib; milestone yang membuat env baru menjadi wajib (mis. `AUTH_JWT_SECRET`, `PRIVACY_POLICY_VERSION`) hanya menambah entri di helper ini, bukan di test config/cmd.
- Produces: `type Secret string` (`LogValue()` & `String()` → `"[REDACTED]"`, `Reveal() string`).
  Struct bersama `Common{AppEnv, LogLevel, LogFormat string; LogTranscripts, PprofEnabled, MetricsEnabled bool}`, `DB{URL Secret; MaxConns, MinConns int32; MaxConnLifetime, MaxConnIdleTime, ConnectTimeout, StatementTimeout, LockTimeout, IdleInTxTimeout time.Duration}`, `Redis{URL Secret; SentinelMaster string; SentinelAddrs []string; Password Secret; PoolSize int; DialTimeout, ReadTimeout, WriteTimeout time.Duration}`, `OTel{Endpoint, ServiceName, ResourceAttributes, TracesSampler string}`.
  `API{Common; DB; Redis; OTel; HTTPAddr, OpsAddr string; ShutdownTimeout time.Duration; HTTPMaxBodyBytes int64; CORSAllowedOrigins []string}`, `Realtime{Common; DB; Redis; OTel; HTTPAddr, OpsAddr string; WSAllowedOrigins []string; WSAllowEmptyOrigin bool; ReadyMaxSessions, MaxSessions, HardMaxSessions int}`, `Worker{Common; DB; Redis; OTel; HTTPAddr string /*env WORKER_HTTP_ADDR*/; ShutdownGrace time.Duration; Handlers []string}`.
  `LoadAPI() (API, error)`, `LoadRealtime() (Realtime, error)`, `LoadWorker() (Worker, error)` (prefill default per binary → `env.Parse` → `Validate()`); varian internal `loadAPI(environ map[string]string)` (nil = `os` env) untuk test. `func ParseWorkerHandlers(flagValue, envValue string) ([]string, error)` (flag menang; kosong atau nama ∉ `{outbox,evaluation,memory,learner,summary,sweeper,cleanup}` → error).
  Default selain di Global Constraints: `APP_ENV=local`, `LOG_LEVEL=info`, `LOG_FORMAT=json`, `METRICS_ENABLED=true`, `OTEL_SERVICE_NAME=kotoki-api|kotoki-realtime|kotoki-worker`, `OTEL_TRACES_SAMPLER=always_on`, `HTTP_MAX_BODY_BYTES=65536`, `SHUTDOWN_TIMEOUT=20s`, `WORKER_SHUTDOWN_GRACE=25s`, `REDIS_POOL_SIZE` api 20/realtime 50/worker 10, `REDIS_DIAL_TIMEOUT=2s`, `REDIS_READ_TIMEOUT=REDIS_WRITE_TIMEOUT=500ms`, `CORS_ALLOWED_ORIGINS=http://localhost:3000`, `REALTIME_READY_MAX_SESSIONS=95`, `REALTIME_MAX_SESSIONS=100`, `REALTIME_HARD_MAX_SESSIONS=120`. `DB_URL` wajib; `REDIS_URL` wajib kecuali `REDIS_SENTINEL_MASTER` diisi.
  Grup env lain (AUTH_, S3_, RABBITMQ_, PROVIDER_, dst.) ditambahkan milestone pemiliknya sebagai file `config/<grup>.go` + baris validasi §13.2.13 miliknya.

- [ ] **Step 1: Write the failing test** — `TestDefaultsPerBinary` (env = `testutil.MinimalEnv(t)`; api: `HTTPAddr==":8080"`, `OpsAddr==":9090"`, `DB.MaxConns==10`, `MinConns==2`, `StatementTimeout==5*time.Second`; realtime: `":8081"`, `":9091"`, `6`, `2`; worker: `HTTPAddr==":8081"`, `MaxConns==4`, `MinConns==1`, `StatementTimeout==30*time.Second`). `TestSecretRedacted` (`slog` JSON handler mencetak `[REDACTED]`, `fmt.Sprintf("%v")` juga). `TestValidate` table (env dasar tiap baris = `testutil.MinimalEnv(t, <override baris>)`; setiap baris `require.ErrorContains(err, <NAMA_VAR>)`):

| APP_ENV | override | error menyebut |
|---|---|---|
| prod | `LOG_TRANSCRIPTS=true` | `LOG_TRANSCRIPTS` |
| staging | `LOG_FORMAT=text` | `LOG_FORMAT` |
| local | `LOG_FORMAT=text` | (lolos) |
| staging | `DB_URL=...sslmode=disable` | `DB_URL` |
| prod | `REDIS_URL=redis://...` (non-sentinel) | `REDIS_URL` |
| prod | `CORS_ALLOWED_ORIGINS=http://x.com` | `CORS_ALLOWED_ORIGINS` |
| prod | `CORS_ALLOWED_ORIGINS=` | `CORS_ALLOWED_ORIGINS` |
| local | `CORS_ALLOWED_ORIGINS=*` | `CORS_ALLOWED_ORIGINS` |
| local | `CORS_ALLOWED_ORIGINS=http://localhost:3000` | (lolos) |
| prod (realtime) | `WS_ALLOW_EMPTY_ORIGIN=true` | `WS_ALLOW_EMPTY_ORIGIN` |
| local (realtime) | `REALTIME_READY_MAX_SESSIONS=101` | `REALTIME_READY_MAX_SESSIONS` |
| local | `DB_MIN_CONNS=11` (api max 10) | `DB_MIN_CONNS` |
| local | `DB_LOCK_TIMEOUT=0s` | `DB_LOCK_TIMEOUT` |
| local | `APP_ENV=qa` / `LOG_LEVEL=trace` | `APP_ENV` / `LOG_LEVEL` |
| local | `DB_URL` kosong | `DB_URL` |

  `TestParseWorkerHandlers`: `("outbox,sweeper","")→[outbox sweeper]`, `("","learner,summary")→[learner summary]`, flag menang atas env, `("","")` error, `("outbox,bogus","")` error menyebut `bogus`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/config/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `go get github.com/caarlos0/env/v11`; tag `env:"NAME"` + `envSeparator:","`; default via fungsi `defaultAPI()/defaultRealtime()/defaultWorker()` (prefill, §13.2.1 butir 5). Origin: tolak `*`, tolak kosong bila ≠ local/dev; di staging/prod wajib `https://`; di local/dev `http://localhost[:port]` boleh (keputusan plan atas ambiguitas §13.2.13). Pesan error selalu diawali nama variabel. Tidak ada `os.Getenv` (gunakan `env.Options{Environment}` hanya bila map non-nil).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/config/... -race -cover` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(config): per-binary env config with startup validation"`.

### Task 5: `internal/platform/logging`

**Files:**
- Create: `internal/platform/logging/{logging.go,redact.go,errattrs.go}`
- Test: `internal/platform/logging/logging_test.go`

**Interfaces:**
- Consumes: `errs.CodeOf/LocationOf`.
- Produces: `type Options struct{ Level slog.Level; Format string /*json|text*/; Service, Version, GitCommit, Env string; LogTranscripts bool }`; `func New(w io.Writer, o Options) *slog.Logger` (field wajib `time` UTC RFC3339Nano, `level`, `msg`, `service`, `version`, `git_commit`, `env`, `trace_id`/`span_id` bila ada span); `func NewRedactingHandler(inner slog.Handler, logTranscripts bool) slog.Handler`; `func IsSensitiveKey(key string, logTranscripts bool) bool`; `func EmailHash(email string) string` (sha256, 32 hex pertama); `func ErrAttrs(err error) []slog.Attr` (`error_code`, `error`, `error_location` = `"<file>:<line> <function>"`).

- [ ] **Step 1: Write the failing test** — `TestRedactAllKeys` (untuk setiap 21 kunci, juga `PASSWORD` & `Set-Cookie`: nilai → `"[REDACTED]"`); `TestRedactNestedGroup` (`slog.Group("req", slog.String("email","a@b.c"))` dan `With(...).WithGroup("g")` ter-redaksi); `TestLogTranscriptsExempt` (`LogTranscripts=true`: `text`,`prompt`,`user_text` tampil apa adanya, tetapi `password`,`token`,`email`,`audio` tetap `[REDACTED]`); `TestRequiredFields` (JSON memuat `service=kotoki-api`, `version`, `git_commit`, `env`, `level=="INFO"`, `time` berakhiran `Z`); `TestTraceIDs` (ctx dengan span dari `sdktrace.NewTracerProvider` lokal (tanpa testutil, Task 6 belum ada) → `trace_id`/`span_id` 32/16 hex); `TestEmailHash` (panjang 32, deterministik, tidak sama dengan input); `TestErrAttrs` (`error_location` cocok `^internal/platform/logging/logging_test.go:\d+ `).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/logging/... -race`. Expected: FAIL (paket belum ada).
- [ ] **Step 3: Implement** — `redactHandler` membungkus `slog.JSONHandler`/`TextHandler` (`ReplaceAttr` menormalkan `time` ke UTC); `Handle` menambah `trace_id`/`span_id` dari `trace.SpanContextFromContext`; `WithAttrs/WithGroup` meredaksi rekursif. Daftar kunci disalin dari Global Constraints sebagai `var sensitiveKeys` (dipakai juga Task 7).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/logging/... -race -cover` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(logging): slog JSON with RedactingHandler"`.

### Task 6: `internal/testutil` dasar (tanpa container)

**Files:**
- Create: `internal/testutil/{testdata.go,trace.go}`, `internal/testutil/fakes/failinjector.go`, `internal/testutil/repotest/repotest.go`, `testdata/api/rest/.gitkeep`
- Test: `internal/testutil/testdata_test.go`, `internal/testutil/fakes/failinjector_test.go`, `internal/testutil/repotest/repotest_test.go`

**Interfaces:**
- Produces: `testutil.ModuleRoot(t testing.TB) string` (cari `go.mod` naik dari cwd); `testutil.TestdataPath(t testing.TB, rel string) string` (`<root>/testdata/<rel>`, `t.Fatalf` bila tak ada; tanpa build tag, hanya stdlib); `testutil.NewTraceRecorder(t testing.TB) *TraceRecorder` dengan `Spans() []tracetest.SpanStub`, `Find(name string) tracetest.SpanStub` (memasang `sdktrace.NewTracerProvider` + `tracetest.NewInMemoryExporter()` sebagai provider global, dipulihkan di `t.Cleanup`).
  `fakes.FailInjector` (`FailNext(method string, err error)`, `Take(method string) error`, aman konkuren, sekali pakai).
  `repotest.Case[T any]{Name string; Run func(t *testing.T, repo T)}`; `repotest.Each[T any](t *testing.T, newRepo func(t *testing.T) T, cases ...Case[T])` (repo baru per case, `t.Run`). Suite per port ditulis milestone fitur.

- [ ] **Step 1: Write the failing test** — `TestTestdataPathFindsRoot` (`TestdataPath(t,"api/rest/.gitkeep")` ada & absolut); `TestNewTraceRecorderCapturesSpan` (`otel.Tracer("x").Start` → `Find("a").Name=="a"`); `TestFailInjectorOnce` (`FailNext("Get",e)`; `Take("Get")==e` lalu `nil`); `TestEachFreshRepo` (tiga case mengubah state, tiap case melihat repo baru).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/testutil/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `go get go.opentelemetry.io/otel go.opentelemetry.io/otel/sdk github.com/stretchr/testify`. Paket `testutil` hanya boleh diimpor `*_test.go` (lint `no-test-deps-in-prod`).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; ulangi `go test ./internal/platform/logging/... -race`.
- [ ] **Step 5: Commit** — `git commit -m "test(testutil): testdata path, trace recorder, fakes and repotest skeleton"`.

### Task 7: `internal/platform/otel`

**Files:**
- Create: `internal/platform/otel/{span.go,setup.go}`
- Test: `internal/platform/otel/span_test.go`, `internal/platform/otel/setup_test.go`

**Interfaces:**
- Consumes: `errs.*`, `logging.IsSensitiveKey`, `testutil.NewTraceRecorder`.
- Produces: `func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span)`; `func End(span trace.Span, errp *error)`; `type Config struct{ Endpoint, ServiceName, ResourceAttributes, Version, Env string }`; `func Setup(ctx context.Context, c Config) (shutdown func(context.Context) error, err error)`. Impor SDK memakai alias `otelsdk "go.opentelemetry.io/otel"` (nama paket kita `otel`).

- [ ] **Step 1: Write the failing test** — (§14.2 b/c) memakai `NewTraceRecorder`:
  - `TestEndInternalError`: `ctx, sp := Start(ctx,"balance.Settle"); err := errs.Wrap(errors.New("pgx boom"), errs.CodeInternalError, "x"); End(sp,&err)` → span `Status().Code==codes.Error`, ada event `exception` dengan `exception.stacktrace`, atribut `code.file.path=="internal/platform/otel/span_test.go"`, `code.line.number`, `code.function.name`, `kotoki.error_code=="INTERNAL_ERROR"`.
  - `TestEndBusinessError`: `errs.New(errs.CodeBalanceInsufficient,..)` → status **bukan** `Error`, `kotoki.error_code=="BALANCE_INSUFFICIENT"`, atribut lokasi tetap ada, tanpa event exception.
  - `TestEndPlainErrorIsInternal` (error tanpa kode → status Error); `TestEndNilError` (tidak ada atribut `kotoki.error_code`).
  - `TestParentChild` (`Start` bersarang → `ParentSpanID` cocok).
  - `TestAttrRedaction` (`Start(ctx,"x", attribute.String("text","halo"), attribute.String("user.id","u1"))` → `text=="[REDACTED]"`, `user.id=="u1"`).
  - `TestSetupNoEndpointIsNoop` (`Endpoint==""` → `shutdown` non-nil, `Start` tidak panik, propagator `TraceContext` terpasang); `TestSetupWithEndpoint` (`localhost:1` tidak error saat setup, `shutdown(ctx)` mengembalikan dalam < 5 s).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/otel/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc go.opentelemetry.io/otel/sdk go.opentelemetry.io/otel/semconv`; tracer global bernama modul; `End` sesuai kontrak §13.7.2.1 (internal → `RecordError(err, trace.WithStackTrace(true))` + `SetStatus(codes.Error, string(CodeOf))`; bisnis → hanya atribut). Sampler hanya `always_on`; resource `service.name`, `service.version`, `deployment.environment.name`; batch processor.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/otel/... -race -cover` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(otel): Start/End span wrappers with error location and tracer setup"`.

### Task 8: `internal/platform/metrics`

**Files:**
- Create: `internal/platform/metrics/{metrics.go,db.go,redis.go}`
- Test: `internal/platform/metrics/metrics_test.go`

**Interfaces:**
- Produces: `var HTTPBuckets, TurnBuckets, BargeBuckets, AsyncBuckets []float64`; `func NewRegistry() *prometheus.Registry` (collector Go + process, tanpa `init()`/registry global); `func Handler(reg *prometheus.Registry) http.Handler`; `type PoolStat struct{ Acquired, Idle, Max int32 }`; `func NewDB(reg prometheus.Registerer, stat func() PoolStat) *DB` dengan `(*DB).ObserveQuery(name string, d time.Duration)`, `(*DB).ObserveAcquireWait(d time.Duration)`; `func NewRedis(reg prometheus.Registerer) *Redis` dengan `(*Redis).CommandError(op string)`, `(*Redis).PoolTimeout()`.
  Metrik: `kotoki_db_pool_acquired_conns|idle_conns|max_conns` (GaugeFunc), `kotoki_db_pool_acquire_wait_seconds`, `kotoki_db_query_duration_seconds{query}` (HTTPBuckets), `kotoki_redis_command_errors_total{op}`, `kotoki_redis_pool_timeouts_total`.

- [ ] **Step 1: Write the failing test** — `TestBuckets` (nilai persis Global Constraints, `reflect.DeepEqual`); `TestRegistryHasRuntimeMetrics` (scrape `Handler` → memuat `go_goroutines`, `process_cpu_seconds_total`); `TestDBMetrics` (stat→`{3,2,10}` muncul `kotoki_db_pool_acquired_conns 3`, `..._max_conns 10`; `ObserveQuery("InsertSession", 30ms)` → baris `kotoki_db_query_duration_seconds_bucket{query="InsertSession",le="0.05"} 1`); `TestRedisMetrics` (counter naik); `TestNoForbiddenLabels` (tidak ada label `user_id|session_id|turn_id|email`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/metrics/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `go get github.com/prometheus/client_golang`; `promhttp.HandlerFor(reg, Opts{})`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/metrics/... -race -cover` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(metrics): registry, buckets, db and redis client metrics"`.

### Task 9: `internal/platform/health`

**Files:**
- Create: `internal/platform/health/ops.go`
- Test: `internal/platform/health/ops_test.go`

**Interfaces:**
- Consumes: `metrics.Handler`.
- Produces: `type Check struct{ Name string; Fn func(ctx context.Context) error }`; `type OpsOptions struct{ Registry *prometheus.Registry; MetricsEnabled, PprofEnabled bool; Checks []Check; Logger *slog.Logger }`; `func NewOps(o OpsOptions) *Ops`; `(*Ops).Handler() http.Handler`; `(*Ops).RunStartupChecks(ctx context.Context) error` (jalankan semua `Checks`; semua sukses → ready); `(*Ops).SetDraining()`; `(*Ops).Serve(ctx context.Context, ln net.Listener) error` (menutup dengan `Shutdown` 5 s saat `ctx` selesai, mengembalikan `nil` pada shutdown normal).

- [ ] **Step 1: Write the failing test** — `httptest`: `TestHealthzAlways200` (`{"status":"ok"}`, juga saat draining); `TestReadyzStartingThenReady` (awal `503 {"status":"starting","checks":{}}`; setelah `RunStartupChecks` sukses `200 {"status":"ready"}`); `TestReadyzFailedCheck` (check `postgres` error → `RunStartupChecks` mengembalikan error, `/readyz` 503 dengan `"checks":{"postgres":"fail"}`, body **tidak** memuat teks error); `TestReadyStaysAfterDependencyFails` (setelah ready, `Fn` mulai gagal → tetap 200); `TestDraining503` (`{"status":"draining"}`); `TestMetricsToggle` (`MetricsEnabled=true` → body memuat `go_goroutines`; `false` → 404); `TestPprofToggle` (`/debug/pprof/` 200 vs 404); `TestServeShutsDown` (listener `127.0.0.1:0`, cancel ctx → `Serve` mengembalikan `nil` ≤ 2 s).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/health/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `http.ServeMux`, `net/http/pprof` didaftarkan manual (bukan `DefaultServeMux`); state ready/draining `atomic`; `ReadHeaderTimeout` 5 s.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/health/... -race -cover` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(health): ops listener with healthz, readyz, metrics, pprof"`.

### Task 10: Lint (`.golangci.yml`) dan `make arch-check`

**Files:**
- Create: `.golangci.yml`
- Test: `test/arch/arch_test.go`

**Interfaces:**
- Produces: `func forbiddenImports(pkgPath string, imports []string) []string` (test-internal; mengembalikan impor terlarang menurut aturan `feature-no-infra`, `handler-thin`, `adapter-no-handler`, `cgo-libs-only-in-audio`, `audio-only-realtime-graph`).

- [ ] **Step 1: Write the failing test** — `TestForbiddenImportsTable` (contoh: `internal/session` mengimpor `github.com/jackc/pgx/v5` → terlarang; `internal/httpapi` mengimpor `internal/postgres` → terlarang; `internal/postgres` mengimpor `internal/httpapi` → terlarang; `internal/conversation/audio` mengimpor `gopkg.in/hraban/opus.v2` → boleh; `internal/provider/openai` mengimpor `github.com/coder/websocket` → boleh; `internal/postgres` mengimpor `pgx` → boleh). `TestCmdAPIWorkerGraphHasNoCgo` menjalankan `go list -deps ./cmd/api ./cmd/worker` (skip bila paket belum ada) dan menolak `internal/conversation/audio`, `hraban/opus`, `go-webrtcvad`. `TestFeaturePackagesRespectLayout` memuat `go list -json ./internal/...` dan memanggil `forbiddenImports` untuk setiap paket fitur yang ada.
- [ ] **Step 2: Run it to verify it fails** — `go test ./test/arch/... -race -run 'Forbidden|Cgo|Layout'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `.golangci.yml` disalin **persis** dari §14.9.5 (blok yaml). Instal: `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6`. Perbaiki temuan lint pada kode Task 1–9 (tanpa `//nolint` kecuali `gosec` dengan alasan).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./test/arch/... -race` PASS; `make lint` → 0 issue; `make arch-check` → exit 0 (build `cmd/api`/`cmd/worker` belum ada → target boleh gagal sampai Task 23, jalankan ulang di sana). **Uji negatif manual:** buat `internal/session/tmp_violation.go` yang mengimpor `github.com/jackc/pgx/v5` (dan `time.Now()`), `make lint` HARUS gagal dengan `depguard` dan `forbidigo`; hapus file.
- [ ] **Step 5: Commit** — `git commit -m "chore(lint): golangci-lint config with layout depguard rules and arch tests"`.

### Task 10b: Follow-up `errs` — hapus 3 kode error konten (D-014)

**Files:**
- Modify: `internal/platform/errs/{codes.go,messages.go,errs_test.go}`

**Interfaces:**
- Menghapus konstanta `CodeConversationStyleNotAvailable`, `CodeExpressionProfileNotAvailable`, `CodeContentInUseAsDefault` (`CONVERSATION_STYLE_NOT_AVAILABLE`, `EXPRESSION_PROFILE_NOT_AVAILABLE`, `CONTENT_IN_USE_AS_DEFAULT`) beserta entri `Messages` dan `AllCodes()`. Setelah task ini `len(AllCodes())==67` = **50** baris REST §5.10 + 17 kode WS-saja §7.4.8. Aturan nama kanonik dan API lain (`New/Wrap/CodeOf/KindOf/...`) tidak berubah. Himpunan `KindInternal` tidak berubah (`INTERNAL_ERROR, CONTENT_MISCONFIGURED, SERVICE_UNAVAILABLE, PROVIDER_FAILURE, AI_GENERATION_FAILED`); sisanya `KindBusiness`.

- [ ] **Step 1: Write the failing test** — ubah `errs_test.go` (test Task 2) mengikuti spec baru: `TestMessagesComplete` kini `len(AllCodes())==67` dan `len(Messages)==67`; tabel `TestCodeConstantNames` menjadi **67 pasang** (tiga pasang di atas dihapus; himpunan pasangan = `AllCodes()`); `TestKindOfAllCodes` tetap (himpunan `KindInternal` sama, 67 kode). Tambah `TestRemovedContentCodesAbsent`: `AllCodes()` dan `Messages` tidak memuat string ketiga kode itu, dan parse `go/ast` pada `codes.go` tidak menemukan konstanta bernama `CodeConversationStyleNotAvailable|CodeExpressionProfileNotAvailable|CodeContentInUseAsDefault`. Tambah `TestRestAndWSCounts`: kode yang ada di tabel REST (ditandai di tabel test) = 50, kode WS-saja = 17.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/errs/... -race`. Expected: FAIL (masih 70 kode; tiga kode masih ada).
- [ ] **Step 3: Implement** — hapus tiga konstanta dari `codes.go`, tiga entri dari `messages.go` dan dari daftar `AllCodes()`. `grep -rn 'ConversationStyleNotAvailable\|ExpressionProfileNotAvailable\|ContentInUseAsDefault' --include=*.go .` MUST kosong (paket lain belum memakainya di M1; bila ada, hapus rujukannya).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/platform/errs/... -race -cover` → PASS, coverage ≥ 85%; `go build ./... && go vet ./...` bersih; `make lint` 0 issue.
- [ ] **Step 5: Commit** — `git commit -m "refactor(errs): drop style, expression and default-in-use error codes (D-014)"`.

### Task 11: Harness migration, `000001_extensions`, helper container Postgres/Redis

**Files:**
- Create: `migrations/000001_extensions.{up,down}.sql`, `internal/testutil/{postgres.go,redis.go,pgassert.go}` (tag `//go:build integration || e2e`)
- Test: `migrations/migrations_integration_test.go` (tag `integration`)

**Interfaces:**
- Produces: `testutil.NewPostgres(t testing.TB) *pgxpool.Pool` (container `pgvector/pgvector:pg17` sekali per paket via `sync.Once`, DB `kotoki_template` dimigrasi dengan golang-migrate dari `<ModuleRoot>/migrations`, tiap test `CREATE DATABASE t_<uuidhex> TEMPLATE kotoki_template`, `DROP … WITH (FORCE)` di `t.Cleanup`; override `TEST_DATABASE_URL`); `testutil.NewEmptyPostgresDSN(t testing.TB) string` (DB kosong tanpa migrasi, nama dari plan); `testutil.NewRedis(t testing.TB) *redis.Client` (`redis:7.4`, `FLUSHALL`; override `TEST_REDIS_URL`); `testutil.RequirePgErr(t testing.TB, err error, sqlstate, constraint string)` (cek `*pgconn.PgError`; `constraint==""` = abaikan); `type Row map[string]any`, `testutil.TryInsert(ctx context.Context, db DBTX, table string, r Row) error`, `testutil.MustInsert(...)`; `type DBTX interface{ Exec(...); Query(...); QueryRow(...) }` (subset pgx). Helper RabbitMQ/MinIO ditambah M6.
  Migration test: `func newMigrator(t *testing.T, dsn string) *migrate.Migrate` (driver `database/pgx/v5`, source `file://<root>/migrations`).

- [ ] **Step 1: Write the failing test** — `TestMigrationsUpDownUp`: DB kosong → `Up()` → `Down()` → di schema `public` tidak ada tabel selain `schema_migrations`, tidak ada fungsi non-extension (`pg_proc` + `pg_depend deptype='e'` dikecualikan), `pg_extension` hanya `plpgsql` → `Up()` sukses lagi. `TestMigrationsStepwise`: untuk versi `v=1..N` (N = jumlah file `*.up.sql`): `Steps(+1)`, `Steps(-1)`, `Steps(+1)` tanpa error. `TestExtensions`: `SELECT extname FROM pg_extension` memuat `vector` dan `citext`.
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run 'Migrations|Extensions'`. Expected: FAIL (tidak ada file migration / helper).
- [ ] **Step 3: Implement** — `000001` dari §15.1 (dua baris `CREATE EXTENSION IF NOT EXISTS`; down `DROP EXTENSION IF EXISTS citext; … vector`). `go get github.com/golang-migrate/migrate/v4 github.com/testcontainers/testcontainers-go github.com/testcontainers/testcontainers-go/modules/postgres github.com/testcontainers/testcontainers-go/modules/redis github.com/jackc/pgx/v5 github.com/redis/go-redis/v9`. Container Postgres dengan `-c timezone=UTC`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS (Docker diperlukan). Harness ini otomatis mencakup tiap migration yang ditambah Task 12–17.
- [ ] **Step 5: Commit** — `git commit -m "test(migrations): up/down/up harness, extensions migration, postgres/redis helpers"`.

### Task 12: Migration `000002`–`000004` (users, auth_tokens, balance_accounts)

**Files:**
- Create: `migrations/000002_users`, `000003_auth_tokens`, `000004_balance_accounts` (`.up.sql`/`.down.sql`), `internal/testutil/fixtures_identity.go` (tag integration)
- Test: `migrations/identity_integration_test.go` (tag `integration`)

**Interfaces:**
- Produces: `testutil.UserRow(o Row) Row` (baris `users` valid minimal), `testutil.SeedUser(t testing.TB, db DBTX, o Row) uuid.UUID`, `testutil.BalanceAccountRow(userID uuid.UUID, o Row) Row`.
- DDL: salin dari §3.3.1, §3.3.2, §4.3.1 (down: `DROP TABLE` urutan terbalik FK).

- [ ] **Step 1: Write the failing test** — tabel pelanggaran `{nama, aksi, sqlstate, constraint}` melalui `TryInsert`/`Exec` pada `NewPostgres(t)`:
  `users`: `role='SUPERUSER'`→`23514 chk_users_role`; `status='DELETED'` tanpa `deleted_at`→`chk_users_deleted_at`; `purged_at` untuk user `ACTIVE`→`chk_users_purged_at`; `email` `"ab"`→`chk_users_email_len`; `phone_number='0812'`→`chk_users_phone_fmt`; `terms_version='bad version'`→`chk_users_terms_version`; email beda huruf (`A@x.id` vs `a@x.id`)→`23505 ux_users_email` (CITEXT); dua user `phone_number` sama→`ux_users_phone`, dua `NULL` boleh.
  `refresh_tokens`: `token_hash` 31 byte→`chk_refresh_hash_len`; `revoked_at` tanpa `revoke_reason`→`chk_refresh_revoke`; `revoke_reason='X'`→`chk_refresh_revoke_reason`; `token_hash` duplikat→`ux_refresh_tokens_hash`. `password_reset_tokens`: `expires_at<=created_at`→`chk_reset_expiry`.
  `balance_accounts`: `available_seconds=-1`→`chk_balance_available_non_negative`; `reserved_seconds=-1`→`chk_balance_reserved_non_negative`; dua account satu user→`ux_balance_accounts_user`. FK: hapus `users` yang punya account→`23503` (RESTRICT).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run Identity`. Expected: FAIL (tabel belum ada).
- [ ] **Step 3: Implement** — tulis tiga migration dari DDL spec (tanpa perubahan semantik).
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./migrations/...` → PASS termasuk `TestMigrationsUpDownUp`/`Stepwise` pada 4 versi.
- [ ] **Step 5: Commit** — `git commit -m "feat(migrations): users, auth tokens, balance accounts"`.

### Task 13: Migration `000005`–`000008` (konten: topics, scenarios, characters, prompts)

**Files:**
- Create: `migrations/000005_topics`, `000006_scenarios` (+`content_version_guard()`), `000007_characters`, `000008_prompts` (`.up/.down.sql`), `internal/testutil/fixtures_content.go` (tag integration)
- Test: `migrations/content_integration_test.go`

**Interfaces:**
- Produces: `testutil.ContentIDs struct{ TopicID, ScenarioID, ScenarioVersionID, CharacterID, PromptVersionID uuid.UUID }`; `testutil.SeedContent(t testing.TB, db DBTX) ContentIDs` (satu baris `PUBLISHED` per keluarga berversi + satu `characters` `ACTIVE` dengan `voice` valid + prompt key `CONVERSATION`, baris minimal valid menurut DDL §6.4); `testutil.ValidVoiceJSON(o ...map[string]any) []byte` (objek `voice` minimal valid §6.3.4, D-015: `gender`, `accent`, `speaking_rate` 1.0, tanpa `providers`; `fallback` opsional; override per-key untuk test CHECK); `SeedContent` mengisi `provider_name='ELEVENLABS'`, `model_name='PLACEHOLDER_MODEL_TEST'`, `voice_id='PLACEHOLDER_VOICE_TEST'` (nilai nyata hanya sebagai komentar di seed M3).
- DDL: §6.4.1–§6.4.4 (guard `content_version_guard` dibuat di `000006` dan dipakai trigger `scenario_versions` (000006) dan `prompt_versions` (000008) saja; `characters` tidak berversi dan **tanpa** trigger; down `000006` men-drop fungsi **setelah** `000008` di-drop — periksa urutan down 000008→000005).

- [ ] **Step 1: Write the failing test** — untuk tiap keluarga versi (`scenario_versions`, `prompt_versions`) loop:
  - `TestVersionOnePublished`: dua baris `PUBLISHED` untuk induk sama → `23505 ux_<tabel>_published`.
  - `TestVersionImmutableContent`: `UPDATE … SET <kolom isi>` pada baris `PUBLISHED` → `23514` pesan memuat `immutable`.
  - `TestVersionTransitions`: `DRAFT→RETIRED` ditolak (`invalid content version transition`); `DRAFT→PUBLISHED`, `PUBLISHED→RETIRED`, `RETIRED→PUBLISHED` (rollback, setelah mem-`RETIRE` pengganti) diterima; `DELETE` baris `PUBLISHED` ditolak, `DELETE` `DRAFT` diterima.
  - `TestStatusTimestamps`: `status='PUBLISHED'` dengan `published_at IS NULL` → `chk_scenario_versions_status_ts`, `chk_pv_status_ts`.
  - `TestGuardNotOnCharacters`: `pg_trigger` untuk tabel `characters` = 0 baris; `UPDATE characters SET persona=…`, `name=…`, `voice=…` dan `DELETE` diterima (karakter diedit langsung, D-014).
  - Pelanggaran lain: `topics.code='lower'`→`chk_topics_code`; `scenario_versions.difficulty_level=6`→`chk_scenario_versions_difficulty`; `recommended_level_min>max`→`chk_scenario_versions_levels`; **`scenario_versions.formality='LOUD'`→`chk_scenario_versions_formality`, INSERT tanpa `formality` → nilai `POLITE` (NOT NULL DEFAULT)**; `prompt_configs.key='X'`→`chk_prompt_configs_key`.
  - `characters` (D-014, §6.4.3): `code='haruka'`→`chk_characters_code`; kode ganda→`23505 ux_characters_code`; `status='GONE'`→`chk_characters_status`; `persona` 10 karakter→`chk_characters_persona`; `name` 51 karakter→`chk_characters_name`; `voice` array/bukan objek→`chk_characters_voice_obj`; `voice.gender='X'`→`chk_characters_voice_gender`; `voice.accent='OSAKA'`→`chk_characters_voice_accent`; `speaking_rate` 0.79 dan 1.21→`chk_characters_voice_rate`, 0.80 dan 1.20 diterima; `speaking_rate` string→`chk_characters_voice_rate`; `provider_name='GOOGLE'` dan `'elevenlabs'` (huruf kecil)→`chk_characters_provider` (DB tidak menormalkan; normalisasi di validator M3), `'CARTESIA'` diterima; `model_name=''`, `'   '`, dan 201 karakter→`chk_characters_model` (spasi di dalam seperti `Eleven v3 Conversational` diterima); `voice_id=''`/201 karakter→`chk_characters_voice_id`, UUID dan `21m00Tcm4TlvDq8ikWAM` diterima; INSERT tanpa `provider_name`/`model_name`/`voice_id`→`23502`; `voice.fallback` bukan objek→`chk_characters_voice_fallback` (objek diterima); key asing seperti `providers` tidak ditolak DB (ditolak validator M3, bukan CHECK); `voice` NULL→`23502`. `TestCharactersTTSColumns`: `information_schema.columns` untuk `characters` memuat `provider_name`, `model_name`, `voice_id` (`text`, `NOT NULL`).
  - `TestNoLegacyContentTables` (kemudian dicakup golden Task 19): `to_regclass` untuk `conversation_styles`, `conversation_style_versions`, `expression_profiles`, `expression_profile_versions`, `character_versions`, `voice_profiles`, `voice_provider_configs`, `character_voice_profiles` semuanya NULL.
  - `TestSeedContent` (fixture itu sendiri valid, membuktikan semua FK/CHECK terpenuhi).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run Content`. Expected: FAIL.
- [ ] **Step 3: Implement** — empat migration dari DDL §6.4 (urutan FK: `topics` → `scenarios` → `characters` → `prompts`).
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./migrations/...` → PASS (termasuk up/down/up & stepwise 8 versi).
- [ ] **Step 5: Commit** — `git commit -m "feat(migrations): topics, scenarios, characters with inline voice, prompts and version guard"`.

### Task 14: Migration `000009`–`000011` (sessions, session_turns, session_events)

**Files:**
- Create: `migrations/000009_sessions`, `000010_session_turns`, `000011_session_events` (`.up/.down.sql`), `internal/testutil/fixtures_session.go` (tag integration)
- Test: `migrations/sessions_integration_test.go`

**Interfaces:**
- Produces: `testutil.SessionRow(userID uuid.UUID, c ContentIDs, o Row) Row` (sesi `RESERVED` valid; `config_snapshot` berbentuk `{"character":{id,code,name,avatar_key,persona,greeting_hint,provider_name,model_name,voice_id,voice{gender,accent,speaking_rate}},"formality":"POLITE"}` dari `SeedContent`; `Row` tanpa kolom `provider_profile`), `testutil.SeedSession(t testing.TB, db DBTX, userID uuid.UUID, c ContentIDs, o Row) uuid.UUID`.
- DDL: §5.3.1–§5.3.3 (DO block partisi untuk `session_turns` dan `session_events` disalin; down = `DROP TABLE` tabel induk).

- [ ] **Step 1: Write the failing test** — §5.11 integration DB:
  - `TestSessionStateChecks`: `status='COMPLETED'`+`end_reason='PROVIDER_FAILURE'`→`chk_sessions_state`; `status='ACTIVE'` tanpa `started_at`→`chk_sessions_state`; `CANCELLED` dengan `charged_seconds=5`→`chk_sessions_state`; `duration_seconds=450`→`chk_sessions_duration`; `reserved_seconds<>duration_seconds`→`chk_sessions_reserved`; `create_idempotency_key='short'`→`chk_sessions_idem_key`; `expires_at ≠ started_at+duration`→`chk_sessions_expires`.
  - `TestConfigSnapshotRequired` (D-014, §5.3.1): `config_snapshot` NULL→`23502`; bukan objek (`'[]'`)→`chk_sessions_config_snapshot`; objek valid diterima. `TestSessionsHaveNoLegacyContentColumns`: `information_schema.columns` untuk `sessions` tidak memuat `character_version_id`, `voice_profile_id`, `conversation_style_version_id`, `expression_profile_version_id`, dan `session_turns` tidak memuat `expressions`; kolom `provider_profile` dan constraint `chk_sessions_provider_profile` tidak ada (D-015, D-016: tanpa kolom `tier`); kolom `character_id`, `scenario_version_id`, `prompt_version_id`, `opening_prompt_version_id`, `adaptive_difficulty`, `runtime_fence`, `config_snapshot` ada; `session_turns.tts_voice_id` ada. FK `sessions.character_id → characters` `ON DELETE RESTRICT`.
  - `TestOpeningPromptOnlyAIFirstAfterReserved`: `opening_prompt_version_id` pada `USER_FIRST`→`chk_sessions_opening_prompt`.
  - `TestHandoffCheck`: `handoff_at < started_at`→`chk_sessions_handoff`.
  - `TestOneOpenSessionPerUser`: dua sesi `RESERVED` user sama→`23505 ux_sessions_one_open_per_user`; sesi pertama `CANCELLED` lalu sesi kedua diterima. `TestIdempotencyKeyUnique`: `(user_id, create_idempotency_key)` sama→`ux_sessions_user_idempotency`.
  - `TestTurnStatusNullable`: INSERT `session_turns` `status NULL, completed_at NULL` diterima; `status='COMPLETED'` tanpa `completed_at`→`chk_turn_completed`; `status='X'`→`chk_turn_status`; `sequence_number=0` dengan `user_text` non-NULL→`chk_turn_opening`; `stt_provider='stt'`→`chk_turn_provider_upper`.
  - `TestEventChecks`: `event_type='BOGUS'`→`chk_session_events_type`; `actor='X'`→`chk_session_events_actor`; `SESSION_RESERVED` dengan `from_status NULL,to_status 'RESERVED'` diterima.
  - **Partisi (Review Focus 2):** `TestPartitionsExist` (untuk kedua tabel: tepat 4 anak di `pg_inherits` bernama `{tabel}_pYYYYMM` bulan berjalan..+3 UTC, hitung dari `clock.System{}`; tidak ada partisi `DEFAULT` (`pg_get_expr(relpartbound)='DEFAULT'` kosong)); `TestInsertRoutesToMonthPartition` (INSERT dengan `created_at` bulan depan → `tableoid::regclass` = partisi bulan depan); `TestInsertOutsideRangeFails` (`created_at` = bulan+6 → SQLSTATE `23514`, pesan `no partition`); `TestNoFKPointsToPartitioned` (`pg_constraint` `contype='f'` dengan `confrelid IN ('session_turns'::regclass,'session_events'::regclass)` = 0 baris); `TestDeleteTurnKeepsEvents` (hapus turn tidak mengubah `session_events.turn_id`); `TestTurnPruning` (`EXPLAIN (FORMAT JSON)` query `WHERE session_id=$1 AND created_at >= $2 AND created_at < $3` hanya menyentuh 1 partisi).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run 'Session|Partition|Turn|Event'`. Expected: FAIL.
- [ ] **Step 3: Implement** — tiga migration dari DDL §5.3.
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./migrations/...` → PASS (full down men-drop semua partisi).
- [ ] **Step 5: Commit** — `git commit -m "feat(migrations): sessions and monthly-partitioned turns/events"`.

### Task 15: Migration `000012`–`000013` (balance_reservations, balance_ledger)

**Files:**
- Create: `migrations/000012_balance_reservations`, `000013_balance_ledger` (+`reject_mutation()`) (`.up/.down.sql`)
- Test: `migrations/balance_integration_test.go`

**Interfaces:**
- Produces: `testutil.ReservationRow(acc, userID, sessionID uuid.UUID, o Row) Row`, `testutil.LedgerRow(kind string, ...)` helper per `entry_type` (di file test; tidak diekspor lintas paket).
- DDL: §4.3.2, §4.3.3. Down `000013`: drop trigger, tabel, lalu fungsi `reject_mutation()` (`000018` hanya menambah trigger; urutan down 000018 sebelum 000013 sudah aman).

- [ ] **Step 1: Write the failing test** — `balance_reservations`: `reserved_seconds=450`→`chk_reservation_reserved`; `status='HELD'` dengan `consumed_seconds=0`→`chk_reservation_state`; `SETTLED` dengan `consumed=100, released=100, reserved=300`→`chk_reservation_state`; `SETTLED` `consumed=0, released=300, reserved=300` diterima; `RELEASED` dengan `consumed=10`→`chk_reservation_state`; `start_deadline_at<=created_at`→`chk_reservation_deadline`; dua reservasi satu sesi→`ux_balance_reservations_session`. `balance_ledger` (tiap bentuk §4.3.3): `RESERVE` `reserved_delta=300, available_delta=-300, idempotency_key='reserve:'||reservation_id` diterima; key `reserve:other`→`chk_ledger_shape`; `GRANT` `reference_type='ADMIN'` tanpa `admin_user_id`→`chk_ledger_shape`; `CONSUME` dengan `available_delta<>0`→`chk_ledger_shape`; `ADJUSTMENT` delta 0→`chk_ledger_shape`; `available_seconds_after=-1`→`chk_ledger_after_non_negative`; `reason` 2 karakter→`chk_ledger_reason_len`; `idempotency_key` duplikat→`23505 ux_balance_ledger_idempotency`; `(account_id, account_version)` duplikat→`ux_balance_ledger_account_version`; `entry_type='BONUS'`→`chk_ledger_entry_type`. **Append-only:** `UPDATE balance_ledger …` dan `DELETE …` → SQLSTATE `23001` pesan `append-only`.
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run Balance`. Expected: FAIL.
- [ ] **Step 3: Implement** — dua migration dari DDL §4.3.2–4.3.3 (GRANT/REVOKE bersyarat role §13.3.2: `DO $$ … IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='kotoki_api') …` dibungkus agar lokal dilewati).
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./migrations/...` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(migrations): balance reservations and append-only ledger"`.

### Task 16: Migration `000014`–`000016` (learner, memories, evaluations)

**Files:**
- Create: `migrations/000014_learner`, `000015_memories`, `000016_evaluations` (`.up/.down.sql`)
- Test: `migrations/learning_integration_test.go`

**Interfaces:**
- DDL: §12.2.1–12.2.3 (tanpa FK ke `evaluations`), §11.3, §10.2 + `ALTER TABLE` §12.2.4 (`fk_lss_last_evaluation`, `fk_pe_source_evaluation`, `idx_lss_last_evaluation`) di `000016`; down `000016` men-drop FK itu **sebelum** `DROP TABLE evaluations`.

- [ ] **Step 1: Write the failing test** — `learner_profiles.nickname` 31 karakter→`chk_lp_nickname`, 30 diterima; `preferred_speech_speed='FAST'`→`chk_lp_speed`; `learner_skill_states`: skill `'READING'`→`chk_lss_skill`, `(user_id, skill)` ganda→`ux_learner_skill_states_user_skill`, `estimated_level=5.5`→`chk_lss_level`, `trend='UP'`→`chk_lss_trend`; `proficiency_estimates.raw_score=101`→`chk_pe_raw`; `memories`: `category='MISC'`→`chk_memories_category`, `memory_key='Bad Key'`→`chk_memories_key_fmt`, `confidence=1.2`→`chk_memories_confidence`, `status='ACTIVE'` dengan `superseded_by` non-NULL→`chk_memories_superseded_by`, embedding berdimensi 3→SQLSTATE `22000` (kolom `vector(1536)`), dua baris `ACTIVE` di slot yang sama→`23505 ux_memories_active_slot`; `evaluations`: `status='DONE'`→`chk_evaluations_status`, dua evaluasi satu sesi→`uq_evaluations_session`, `FAILED` tanpa `failure_code`→`chk_evaluations_failed`, `COMPLETED` tanpa `completed_at`→`chk_evaluations_completed_at`; `evaluation_findings.severity='HIGH'`→`chk_ef_severity`, `(evaluation_id, ordinal)` ganda→`uq_evaluation_findings_ordinal`. **FK cross-migration:** `TestEvaluationFKsAdded` (`pg_constraint` memuat `fk_lss_last_evaluation` dan `fk_pe_source_evaluation`); `TestDownEvaluationsDropsFKs` (`Steps(-1)` dari `000016` → kedua FK hilang dan `learner_skill_states`/`proficiency_estimates` tetap ada; `Steps(+1)` sukses).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run Learning`. Expected: FAIL.
- [ ] **Step 3: Implement** — tiga migration; gunakan fixture `SeedUser`/`SeedContent`/`SeedSession` dari Task 12/13/14 (tambah `SeedEvaluation` lokal di test bila perlu).
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./migrations/...` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(migrations): learner, memories, evaluations with deferred FKs"`.

### Task 17: Migration `000017`–`000018` (messaging, admin_audit)

**Files:**
- Create: `migrations/000017_messaging`, `000018_admin_audit` (`.up/.down.sql`)
- Test: `migrations/messaging_integration_test.go`

**Interfaces:**
- DDL: §9.2.1 (`event_outbox` termasuk `traceparent`, `tracestate`), §9.2.2 (`processed_events`), §3.3.3 (`admin_audit_logs` + trigger `trg_admin_audit_logs_append_only`; fungsi `reject_mutation()` sudah ada dari `000013`).

- [ ] **Step 1: Write the failing test** — `event_outbox`: `status='PUBLISHED'` tanpa `published_at`→`chk_event_outbox_published_at`; `producer='CRON'`→`chk_event_outbox_producer`; `aggregate_type='USER'`→`chk_event_outbox_aggregate_type`; `status='DONE'`→`chk_event_outbox_status`; payload > 65536 byte→`chk_event_outbox_payload_size`; `attempts=-1`→`chk_event_outbox_attempts`; INSERT dengan `traceparent`/`tracestate` terisi diterima; baris `PENDING` tampil di `idx_event_outbox_ready` (`EXPLAIN` memakai index itu untuk `WHERE status='PENDING' ORDER BY …`). `processed_events`: `(consumer, event_id)` duplikat→`23505` (PK); `consumer='nobody'`→`chk_processed_events_consumer`. `admin_audit_logs`: `action='HACK'`→`chk_admin_audit_action`; `target_type='X'`→`chk_admin_audit_target_type`; `UPDATE`/`DELETE`→`23001`. `TestDownAuditKeepsRejectMutation` (`Steps(-1)` dari 18: tabel hilang, fungsi `reject_mutation` **masih** ada karena milik `000013`; `Steps(-5)` lagi sampai 000013 turun → fungsi hilang). `admin_audit_logs`: `action='CHARACTER_UPDATED'` dan `target_type='CHARACTER'` diterima (D-014).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run 'Outbox|Processed|Audit'`. Expected: FAIL.
- [ ] **Step 3: Implement** — dua migration dari DDL spec.
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./migrations/...` → PASS (18 versi up/down/up & stepwise).
- [ ] **Step 5: Commit** — `git commit -m "feat(migrations): messaging outbox and admin audit"`.

### Task 18: Alat CI `test/tools/{modelcheck,covercheck,migcheck}`

**Files:**
- Create: `test/tools/modelcheck/main.go`, `test/tools/covercheck/{main.go,thresholds.txt}`, `test/tools/migcheck/main.go`
- Test: `test/tools/modelcheck/main_test.go`, `test/tools/covercheck/main_test.go`, `test/tools/migcheck/main_test.go`

**Interfaces:**
- Produces: CLI `go run ./test/tools/modelcheck ./internal ./cmd` (exit 1 + `file:line` bila literal string non-test cocok `(gpt-|o[0-9]-|gemini-|claude-|eleven_|scribe|nova-|aura-|text-embedding-)`, kecuali `internal/platform/config` dan `testdata/`); `go run ./test/tools/covercheck -profile coverage.out -thresholds test/tools/covercheck/thresholds.txt [-baseline coverage.main.out]` (baris threshold `<prefix paket> <min%>`; paket tanpa data diabaikan; turun > 1 poin dari baseline → gagal; baris `TOTAL` mengecualikan `internal/postgres/sqlcgen` & `internal/testutil`); `go run ./test/tools/migcheck -dir migrations -base origin/main` (pasangan up/down, 6 digit, berurutan tanpa celah, nama = daftar kanonik §15.1, modifikasi file di `main` hanya dicek bila tag `v0.1.0` ada).
  `thresholds.txt` memuat baris §14.8 (fitur 85, `balance`/`session` 90, `conversation` 80, `httpapi`/`wsgateway` 70, `provider` 70, adapter 70, `TOTAL internal 75`).

- [ ] **Step 1: Write the failing test** — modelcheck: file temp berisi `"gpt-4.1"` → 1 temuan `file:line`; sama di `_test.go` / path `config` → 0 temuan. covercheck: profil sintetis (paket `internal/balance` 80% vs threshold 90) → exit 1 menyebut `internal/balance`; 95% → exit 0; baseline turun 1,5 poin → gagal, 0,5 → lulus; paket tak ada di profil → lulus. migcheck: `checkFiles` menolak nomor `000005` hilang, `.up` tanpa `.down`, nama `000003_foo`; menerima daftar kanonik 18 nama; `modifiedGuard(tagExists=false, modified=[...])` → ok, `tagExists=true` → error.
- [ ] **Step 2: Run it to verify it fails** — `go test ./test/tools/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — modelcheck pakai `go/parser` (literal `*ast.BasicLit` string); covercheck parse format `mode: atomic` baris `file:startline.col,endline.col numstmt count`; migcheck menanam daftar 18 nama kanonik sebagai `var canonical = []string{"extensions","users","auth_tokens","balance_accounts","topics","scenarios","characters","prompts","sessions","session_turns","session_events","balance_reservations","balance_ledger","learner","memories","evaluations","messaging","admin_audit"}`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./test/tools/... -race` PASS; `make modelname-check` exit 0; `make migrations-check` exit 0 (tanpa `origin/main` → guard dilewati dengan pesan); `make test-cover` exit 0.
- [ ] **Step 5: Commit** — `git commit -m "chore(tools): modelcheck, covercheck, migcheck"`.

### Task 19: Snapshot skema `schema.golden.sql` + verifikasi down penuh

**Files:**
- Create: `migrations/testdata/schema.golden.sql`
- Modify: `migrations/migrations_integration_test.go` (tambah test)

**Interfaces:**
- Produces: flag test `-update` (regenerasi golden); fungsi `normalizeSchemaDump(dump string) string` (membuang `CREATE TABLE … session_(turns|events)_pYYYYMM`, `ATTACH PARTITION` dan komentar bergantung tanggal).

- [ ] **Step 1: Write the failing test** — `TestSchemaSnapshot`: setelah `Up()` pada DB kosong (golden memuat tepat **24 tabel** aplikasi + `schema_migrations`; `characters` memuat `provider_name`, `model_name`, `voice_id` + `chk_characters_provider/model/voice_id`, `sessions` tanpa `provider_profile` (D-015); `TestTableCount24` menghitung `pg_class relkind IN ('r','p')` di `public` tanpa partisi anak = 24 + 1; tidak ada tabel konten lama D-014), `pg_dump --schema-only` (dieksekusi di container lewat `Exec`, atau `pg_dump` lokal bila `TEST_DATABASE_URL`) → `normalizeSchemaDump` == isi `testdata/schema.golden.sql`; `TestPartitionNormalizationChecks` (tepat 4 partisi per tabel, tanpa `DEFAULT` — sudah di Task 14, di sini dipanggil lagi pada DB hasil `Up()` penuh); `TestFullDownLeavesNothing` (setelah `Down()` dari 000018: `pg_class` relkind `r|p|v|S|i` di `public` hanya `schema_migrations`; `pg_proc` fungsi kita = 0; `pg_trigger` non-internal = 0); `TestNormalizeDump` (unit murni, tanpa tag: baris partisi tanggal 202610 dan 202701 sama-sama dibuang).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./migrations/... -run 'Snapshot|FullDown'` dan `go test -race ./migrations/... -run TestNormalizeDump`. Expected: FAIL (golden belum ada).
- [ ] **Step 3: Implement** — jalankan sekali `go test -tags=integration ./migrations -run TestSchemaSnapshot -update`, review diff golden terhadap DDL spec (24 tabel), commit.
- [ ] **Step 4: Run tests to verify they pass** — `make test-integration` (seluruh repo) → PASS; hasil identik pada dua run berurutan (deterministik).
- [ ] **Step 5: Commit** — `git commit -m "test(migrations): schema golden snapshot and full-down check"`.

### Task 20: sqlc, `NewPool`, pgx tracer

**Files:**
- Create: `sqlc.yaml`, `db/queries/system.sql`, `internal/postgres/sqlcgen/*` (generated), `internal/postgres/{pool.go,tracer.go}`
- Test: `internal/postgres/tracer_test.go`, `internal/postgres/pool_integration_test.go` (tag integration)

**Interfaces:**
- Consumes: `config.DB`, `metrics.DB`, `otel`, `errs`.
- Produces: `sqlc.yaml` (engine postgresql, `schema: migrations`, `queries: db/queries`, `sql_package: pgx/v5`, out `internal/postgres/sqlcgen`, package `sqlcgen`, override `uuid` → `github.com/google/uuid.UUID`); query `-- name: Ping :one` (`SELECT 1::int`) → `sqlcgen.Queries.Ping(ctx) (int32, error)`.
  `type PoolOptions struct{ ServiceName string; Metrics *metrics.DB }`; `func NewPool(ctx context.Context, cfg config.DB, o PoolOptions) (*pgxpool.Pool, error)` (runtime params `statement_timeout`, `lock_timeout`, `idle_in_transaction_session_timeout`, `application_name`; `MaxConns/MinConns/MaxConnLifetime/MaxConnIdleTime`; `ConnectTimeout`; tracer terpasang; error dibungkus `errs.Wrap(..., CodeServiceUnavailable, ...)`); `func PoolStat(p *pgxpool.Pool) metrics.PoolStat`; `func queryName(sql string) string` (parse `-- name: X` → `"X"`, tanpa komentar → `"query"`).

- [ ] **Step 1: Write the failing test** — unit `TestQueryName` (`"-- name: InsertSession :one\nINSERT…"→"InsertSession"`, `"SELECT 1"→"query"`). Integration: `TestPoolRuntimeParams` (`SHOW statement_timeout`=`5s`, `lock_timeout`=`3s`, `idle_in_transaction_session_timeout`=`15s`, `application_name`=`kotoki-test`); `TestQuerySpanAndMetric` (`sqlcgen.New(pool).Ping` → span `db.Ping` atribut `db.system=postgresql`, `db.operation.name=Ping`, **tanpa** atribut berisi `SELECT`; `kotoki_db_query_duration_seconds_count{query="Ping"}=1`); `TestQueryErrorSpan` (SQL salah → span status Error); `TestPoolBadURL` (error `errs.CodeOf==SERVICE_UNAVAILABLE`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/postgres/... -race -run TestQueryName` dan `go test -race -tags=integration ./internal/postgres/...`. Expected: FAIL.
- [ ] **Step 3: Implement** — `pgx.QueryTracer` (`TraceQueryStart/End`) memulai span `db.<queryName>` lewat `otel.Start` (SQL text tidak dicatat), mengamati durasi ke `metrics.DB`. Jalankan `make sqlc` dan commit hasilnya; `make sqlc-check` bersih. Jika sqlc menolak sebuah statement migration (mis. blok `DO`), gunakan `sqlc.yaml` `schema:` daftar eksplisit file `*.up.sql` yang diparse dan catat di deviations.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make sqlc-check` → exit 0.
- [ ] **Step 5: Commit** — `git commit -m "feat(postgres): sqlc layout, pgxpool factory and query tracer"`.

### Task 21: `postgres.Store` / `Tx` (`WithinTx`)

**Files:**
- Create: `internal/postgres/store.go`
- Test: `internal/postgres/store_integration_test.go` (tag integration)

**Interfaces:**
- Consumes: `NewPool`, `sqlcgen`, `errs`, `otel`.
- Produces: `type Store struct{…}`; `type StoreOption func(*Store)`; `func NewStore(pool *pgxpool.Pool, opts ...StoreOption) *Store`; `WithIDs(g idgen.Generator) StoreOption` (default `idgen.NewV7()`) dan `WithClock(c clock.Clock) StoreOption` (default `clock.System{}`) — Store meneruskan keduanya ke repository/`Tx` (M2 `ApplyMovement`/`OpenAccount` butuh); accessor `(*Store).IDs() idgen.Generator`, `(*Store).Clock() clock.Clock`, dan `(*Tx).IDs()`, `(*Tx).Clock()`; `WithTimeouts(lock, statement time.Duration)` (default 3s/5s; worker boleh 30s); `WithBackoff(d []time.Duration)` (default `20ms,50ms,100ms`, jitter ±50%) dan `WithSleep(func(ctx context.Context, d time.Duration) error)` (untuk test); `type Tx struct{…}` (**didefinisikan di `internal/postgres/store.go`**, file yang dimodifikasi M2/M3/M6 untuk menambah accessor repository/outbox) dengan `(*Tx).Queries() *sqlcgen.Queries` dan `(*Tx).DBTX() sqlcgen.DBTX`; `(*Store).Queries() *sqlcgen.Queries` (non-tx); `(*Store).WithinTx(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error`; `(*Store).Pool() *pgxpool.Pool`. View per fitur (`BalanceStore()`, `SessionStore()`, `AuthStore()`, `AdminStore()`) ditambahkan milestone fitur di atas tipe ini (§4.2); `*Tx` kelak mengimplementasikan `balance.Tx`/`session.Tx`/`auth.Tx`/`admin.Tx`.

- [ ] **Step 1: Write the failing test** — `TestCommit` (insert via `tx.DBTX()` terlihat setelah return); `TestStoreOptions` (default `IDs()` menghasilkan UUIDv7 valid dan `Clock()` bertipe `clock.System`; `WithIDs(idgen.NewSequence(t))`/`WithClock(clock.NewFake(t0))` terlihat dari `Store` dan dari `Tx` di dalam `WithinTx`); `TestRollbackOnFnError` (fn error → baris tak ada, error dikembalikan apa adanya); `TestRollbackOnPanic` (panic di-propagate, baris tak ada); `TestSetLocalTimeouts` (di dalam fn `SHOW lock_timeout`=`3s`, `statement_timeout`=`5s`; isolasi `SHOW transaction_isolation`=`read committed`); `TestRetryOn40001` (fn mengembalikan `&pgconn.PgError{Code:"40001"}` 2× lalu sukses → fn dipanggil 3×, `WithSleep` fake merekam jeda `[20ms,50ms]` ±50%); `TestRetryOn40P01Exhausted` (selalu `40P01` → fn dipanggil tepat **4×** (1 + 3 retry), error terakhir dibungkus `errs.CodeInternalError` dengan `errors.As(&PgError)` tetap bisa); `TestNoRetryOnOtherError` (`23505` → 1×); `TestLockTimeout` (dengan `WithTimeouts(200ms, 5s)`: koneksi lain `SELECT … FOR UPDATE` pada satu baris; WithinTx `FOR UPDATE` baris yang sama → error `errs.CodeOf == CodeServiceUnavailable`, SQLSTATE asal `55P03`, tidak di-retry); `TestTxSpan` (`NewTraceRecorder`: span `db.tx` ada, anak dari span pemanggil, status Error bila fn gagal dengan error internal); `TestContextCancel` (ctx dibatalkan → return cepat, tanpa kebocoran koneksi: `pool.Stat().AcquiredConns()==0`).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./internal/postgres/... -run 'Commit|Rollback|Retry|Lock|TxSpan|SetLocal|Context'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})`, `SET LOCAL` dua statement, `defer` rollback + `recover` re-panic; loop retry hanya untuk `40P01`/`40001` (cek `errors.As(*pgconn.PgError)` menembus `errs`); `55P03` → `errs.Wrap(err, CodeServiceUnavailable, "lock timeout")`; error lain `errs.Wrap(err, CodeInternalError, …)` hanya bila belum membawa kode.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make lint` 0 issue.
- [ ] **Step 5: Commit** — `git commit -m "feat(postgres): Store.WithinTx with retry, timeouts and tracing"`.

### Task 22: `internal/redisstore` (client + hook tracing)

**Files:**
- Create: `internal/redisstore/{client.go,hook.go}`
- Test: `internal/redisstore/hook_test.go`, `internal/redisstore/client_integration_test.go` (tag integration)

**Interfaces:**
- Consumes: `config.Redis`, `metrics.Redis`, `otel`, `errs`.
- Produces: `func NewClient(cfg config.Redis, m *metrics.Redis) (redis.UniversalClient, error)` — **tipe kembalian kanonik `redis.UniversalClient`** (`github.com/redis/go-redis/v9`); konsumen di milestone lain MUST menerima `redis.UniversalClient` (atau `redis.Cmdable` bila hanya butuh perintah) (`Sentinel` bila `SentinelMaster != ""` → `redis.NewFailoverClient`, selain itu `redis.ParseURL`; `PoolSize`, timeout; hook terpasang); `func NewTraceHook(m *metrics.Redis) redis.Hook` (`ProcessHook`, `ProcessPipelineHook`; span `redis.<CMD>` / `redis.pipeline`, atribut `db.system=redis`, `kotoki.redis.key_prefix`; `redis.Nil` bukan error; error dibungkus `errs.Wrap(CodeServiceUnavailable)`; `redis.ErrPoolTimeout` menaikkan `PoolTimeout()`); `func KeyPrefix(key string) string` (segmen `:`-terpisah yang berupa UUID, hex ≥ 16 karakter, atau angka diganti `*`; nama dari plan); `func Ping(ctx context.Context, c redis.UniversalClient) error`.

- [ ] **Step 1: Write the failing test** — unit `TestKeyPrefix`: `kotoki:session:0192f1a0-1111-7000-8000-000000000001:rt`→`kotoki:session:*:rt`; `kotoki:ws_ticket:`+64 hex→`kotoki:ws_ticket:*`; `kotoki:fake:faults`→sama. Integration: `TestHookSpan` (`SET kotoki:session:<uuid>:rt v` → span `redis.SET`, `db.system=redis`, `kotoki.redis.key_prefix=kotoki:session:*:rt`; **tidak ada** atribut yang nilainya memuat UUID/`v`); `TestGetMissIsNotError` (`GET` kunci tak ada → span status bukan Error); `TestCommandErrorCounted` (perintah salah → `kotoki_redis_command_errors_total{op="EVAL"}`/op perintah naik, error `CodeOf==SERVICE_UNAVAILABLE`); `TestPipelineSpan`; `TestPing`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/redisstore/... -race -run TestKeyPrefix` dan `go test -race -tags=integration ./internal/redisstore/...`. Expected: FAIL.
- [ ] **Step 3: Implement** — hook buatan sendiri (tanpa library tambahan, §13.7.2.1); `DialHook` pass-through.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(redisstore): client factory and tracing hook"`.

### Task 23: Kerangka `cmd/api`, `cmd/realtime`, `cmd/worker`

**Files:**
- Create: `cmd/api/main.go`, `cmd/realtime/main.go`, `cmd/worker/main.go`
- Test: `cmd/api/main_integration_test.go`, `cmd/realtime/main_integration_test.go`, `cmd/worker/main_integration_test.go` (tag `integration`, `package main`)

**Interfaces:**
- Consumes: `config.Load*`, `logging.New`, `otel.Setup`, `metrics.NewRegistry/NewDB/NewRedis`, `postgres.NewPool`, `redisstore.NewClient/Ping`, `health.NewOps`, `buildinfo`.
- Produces: tiap `main` memanggil `func run(ctx context.Context) error` (worker: `run(ctx context.Context, args []string) error` dengan flag `--handlers`). Urutan wiring: `signal.NotifyContext(SIGINT,SIGTERM)` → config (gagal → stderr + exit 1 sebelum koneksi apa pun) → logger → `otel.Setup` → registry/metrics → `NewPool` → `NewClient` → `health.NewOps` dengan `Checks{postgres: Ping, redis: Ping}` → `net.ListenConfig.Listen(OpsAddr)` → `RunStartupChecks` → `Serve` sampai ctx selesai → `ops`/pool/redis/otel shutdown berurutan. Worker: `ParseWorkerHandlers(flag, cfg.Handlers env)` lalu log handler aktif (belum menjalankan apa pun); listener = `WORKER_HTTP_ADDR`. Tidak ada listener publik/handler fitur di M1.

- [ ] **Step 1: Write the failing test** — per binary (bukan paralel): `testutil.ApplyEnv(t, testutil.MinimalEnv(t, map[string]string{"DB_URL": <DSN NewPostgres>, "REDIS_URL": <addr NewRedis>, "OPS_ADDR": "127.0.0.1:<port bebas>" /* worker: WORKER_HTTP_ADDR */, "LOG_FORMAT": "text"}))`; `go run(ctx)`; `require.Eventually(≤10s, 50ms)` `GET /readyz`==200; `/healthz`==200 `{"status":"ok"}`; `/metrics` memuat `go_goroutines` dan `kotoki_db_pool_max_conns`; `cancel()` → `run` mengembalikan `nil` ≤ 5 s. Tes tambahan: `TestRunFailsFastOnBadConfig` (`MinimalEnv` dengan override `DB_URL:""` → error menyebut `DB_URL`, tidak ada port terbuka); worker: `TestWorkerRejectsUnknownHandler` (`--handlers=outbox,bogus` → error menyebut `bogus`) dan `TestWorkerRequiresHandlers` (kosong → error); `TestReadyzBeforeDependencies` tidak diperlukan (sudah di Task 9).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -tags=integration ./cmd/...`. Expected: FAIL (`run` belum ada).
- [ ] **Step 3: Implement** — tiga `main.go` (~70 baris tiap file, tanpa helper bersama karena wiring hanya di `cmd`, D-004); `fmt.Fprintln(os.Stderr, …)` diperbolehkan di `cmd/`. Pool: `ServiceName` `kotoki-api|kotoki-realtime|kotoki-worker`. Worker `Store` timeouts belum dipakai.
- [ ] **Step 4: Run tests to verify they pass** — `go test -race -tags=integration ./cmd/...` PASS; `make arch-check` exit 0 (`CGO_ENABLED=0 go build ./cmd/api ./cmd/worker` + `go list -deps` bersih); `go build ./cmd/realtime` sukses.
- [ ] **Step 5: Commit** — `git commit -m "feat(cmd): api, realtime, worker skeletons wired to config, log, otel, health"`.

### Task 24: docker-compose, `.env.example`, collector, Dockerfile per binary

**Files:**
- Create: `docker-compose.yml`, `.env.example`, `deploy/docker/{otel-collector.yaml,api.Dockerfile,realtime.Dockerfile,worker.Dockerfile,migrate.Dockerfile}`
- Test: `internal/platform/config/envexample_test.go`, `test/arch/docker_test.go` (tag `integration`)

**Interfaces:**
- `docker-compose.yml`, `deploy/docker/otel-collector.yaml`, `.env.example`, dan keempat Dockerfile disalin **persis** dari §14.10.2, §14.10.3, §13.10.9 (blok kode); `worker.Dockerfile` = `api.Dockerfile` dengan `./cmd/worker` dan `EXPOSE 8081 9090`... (sesuai §13.10.9 "ganti target"). `<PIN>` untuk `grafana/otel-lgtm` di-resolve ke rilis terbaru dan dicatat.
- Keputusan plan (ambiguitas `--wait` dengan service sekali-jalan `minio-init`): bila `docker compose up -d --wait` gagal hanya karena `minio-init` exit, ubah target `up` menjadi `docker compose up -d --wait postgres redis rabbitmq minio && docker compose up minio-init` dan catat di `docs/implementation-deviations.md`.

- [ ] **Step 1: Write the failing test** — `TestEnvExampleLoads` (parse `.env.example` dengan `testutil.ModuleRoot`, komentar `#` diabaikan; untuk `loadAPI`, `loadRealtime`, `loadWorker` dengan map itu → `Validate()` lolos); `TestEnvExampleOmitsPerBinaryVars` (tidak mengandung `HTTP_ADDR`, `OPS_ADDR`, `WORKER_HTTP_ADDR`, `WORKER_SHUTDOWN_GRACE`, `DB_MAX_CONNS`, `DB_MIN_CONNS`, `REDIS_POOL_SIZE`, `SHUTDOWN_TIMEOUT`, `DB_STATEMENT_TIMEOUT`). `TestDockerImagesBuild` (integration): `docker build -f deploy/docker/<b>.Dockerfile` untuk `api realtime worker migrate`; `docker run --rm kotoki/api:test` exit ≠ 0 dan stderr memuat `DB_URL`; image `api`/`worker` tanpa shell (`docker run --entrypoint sh` gagal) dan `docker inspect` `Config.User=="65532:65532"`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/config/... -race -run EnvExample` dan `go test -race -tags=integration ./test/arch/... -run Docker`. Expected: FAIL.
- [ ] **Step 3: Implement** — salin berkas; `make env`; jalankan `docker compose config -q` dan `docker compose --profile observability config -q`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make up` → semua service `healthy` (postgres, redis, rabbitmq, minio) dan `minio-init` selesai; `make up-obs` → Grafana `http://localhost:3000` 200; `make down`.
- [ ] **Step 5: Commit** — `git commit -m "chore(dev): docker-compose, env example, collector config and Dockerfiles"`.

### Task 25: CI GitHub Actions

**Files:**
- Create: `.github/workflows/ci.yml`
- Test: `test/arch/ci_test.go`

**Interfaces:**
- `ci.yml` = kerangka §14.12.1 (job `lint`, `unit`, `integration`, `e2e`, `build-images`; trigger, `concurrency`, `permissions: contents: read`, matrix `[api, realtime, worker, migrate]`). Setiap `uses:` dipin ke commit SHA (resolve dengan `gh api repos/<owner>/<repo>/commits/<tag> --jq .sha`, komentar versi). Job `e2e` memanggil `make test-e2e`; target ini ada di Makefile dan lulus kosong (`go test ./test/e2e/...` tanpa paket → gunakan `-run` aman) sampai M5 — tambahkan `test/e2e/doc.go` (`//go:build e2e`) agar paket ada. `nightly.yml`/`live-providers.yml`/`deploy-*.yml` milik M6/M9/M10, bukan M1.

- [ ] **Step 1: Write the failing test** — `TestCIWorkflowShape` (parse YAML tanpa library baru: `go get gopkg.in/yaml.v3` dilarang → gunakan cek teks): berisi lima nama job; setiap baris `uses:` cocok `@[0-9a-f]{40}`; memanggil `make tidy-check sqlc-check migrations-check modelname-check`, `make vuln`, `make arch-check`, `make test-cover`, `make test-integration`, `make test-e2e`; `permissions:` tingkat atas `contents: read`; `packages: write` hanya di job `build-images`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./test/arch/... -race -run CIWorkflow`. Expected: FAIL.
- [ ] **Step 3: Implement** — tulis `ci.yml`; validasi `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7` → 0 temuan (alat dev, bukan dependensi modul).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./test/arch/... -race` PASS; `actionlint` bersih.
- [ ] **Step 5: Commit** — `git commit -m "ci: github actions workflow for lint, unit, integration, e2e, images"`.

---

## Gerbang milestone

Jalankan berurutan di repo bersih; catat perintah + ringkasan output di `docs/implementation-log.md`.

- [ ] `make env && make up` → semua service `healthy`, `minio-init` selesai.
- [ ] `make migrate-up` → `18/u` … tanpa error; `docker compose exec -T postgres psql -U kotoki -d kotoki -c '\dt'` memuat 24 tabel aplikasi (+ partisi `session_turns_p*`/`session_events_p*` masing-masing 4) dan `schema_migrations`.
- [ ] `make migrate-down && make migrate-up` (literal gerbang; turun 1 langkah) lalu `make migrate-down N=18 && make migrate-up` (keputusan plan: bukti reversibilitas penuh) → keduanya exit 0; setelah `N=18` hanya `schema_migrations` yang tersisa.
- [ ] `make lint` → `0 issues.`; `make test` → semua paket `ok` (`-race -short`); `make test-integration` → semua paket `ok` termasuk `migrations`, `internal/postgres`, `internal/redisstore`, `cmd/*`; `make arch-check` → exit 0; `make sqlc-check migrations-check modelname-check tidy-check` → exit 0; `make test-cover` → covercheck exit 0.
- [ ] Tiga binary hidup: `make run-api`, `make run-realtime`, `make run-worker` (tiga terminal) lalu `curl -fsS localhost:9090/healthz localhost:9090/readyz localhost:9090/metrics`, `:9091/…`, `:9092/…` → `healthz`=`{"status":"ok"}`, `readyz`=`{"status":"ready"}`, `/metrics` memuat `go_goroutines` dan `kotoki_db_pool_max_conns`.
- [ ] `make docker-build` → empat image ter-build; `docker run --rm kotoki/api:<VERSION>` exit 1 dengan pesan `DB_URL`.
- [ ] `make up-obs` dengan `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` pada `make run-api` → trace `db.Ping`/`db.tx` bisa dicari di Grafana → Explore → Tempo.
- [ ] Review Focus 1–5 masing-masing punya test hijau di task pemiliknya (daftar di bagian Review Focus).

## Self-Review

**Cakupan spec:** §2.1–2.3/D-001–D-013 (Task 1–4), §2.2 aturan 5–8 (Task 3, 10, 23), §3.3 (Task 12, 17), §4.2 port transaksi dasar + §4.6.0 (Task 21), §4.3 (Task 12, 15), §5.3 (Task 14), §5.10/§7.4.8 (Task 2), §5.11 bagian DB (Task 14), §6.4 (Task 13), §9.2 (Task 17), §9.9.10 hanya DDL partisi awal (Task 14), §10.2/§11.3/§12.2 (Task 16), §13.2 (Task 4 untuk grup yang dipakai M1), §13.3.3 timeout (Task 20–21), §13.7.1–13.7.3 (Task 5, 7, 8), §13.10.4 (Task 9, 23), §13.10.8–9 (Task 24), §14.1–14.3, 14.7, 14.9.5, 14.10, 14.12.1 (Task 6, 10, 11, 18, 19, 24, 25), §15.1 (Task 11–17).
**Tidak dicakup (sengaja):** grup env `AUTH_/S3_/RABBITMQ_/OUTBOX_/PROVIDER_/FAKE_/REALTIME_(selain batas sesi)/VAD_/TURN_/…` dan baris validasi §13.2.13 miliknya (milestone pemilik menambah `config/<grup>.go`); helper `NewRabbitMQ`/`NewMinIO` (M6); `nightly/live/deploy` workflow (M6/M9/M10); `deploy/k8s` (M10); view `BalanceStore()/SessionStore()/…` pada `postgres.Store` (M2/M3); seed konten (M3); job `partition_maintenance` (M6).
**Konsistensi tipe:** `config.DB` → `postgres.NewPool` → `Store`; `metrics.DB`/`metrics.Redis` dipakai `postgres`/`redisstore`; `testutil.Row/TryInsert` dipakai semua test migration; `ContentIDs` (Task 13) dikonsumsi Task 14 & 16.
**Ambiguitas spec & keputusan:**
1. §2.2 menamai direktori `platform/log/`, §13.7.1 menyebut `platform/logging.RedactingHandler`, dan aturan paket 4 melarang menimpa stdlib → dipakai `internal/platform/logging` (catat di deviations).
2. Spec final menaruh `reject_mutation()` di `000013` (`balance_ledger`, dipakai pertama) dan `000018` hanya trigger audit → ikut spec.
7. Task 1–10 sudah selesai sebelum D-014; penyederhanaan konten ditambahkan sebagai Task 10b (hapus 3 kode error) dan penomoran/isi migration Task 13–17 diubah (18 migration, 24 tabel).
3. Hook Redis: brief menyebutnya di `otel`, §13.7.2.1 menaruhnya di `redisstore` → `internal/redisstore/hook.go`.
4. §4.6.0 mewajibkan `statement_timeout 5s` per transaksi sementara worker punya 30 s → `WithTimeouts` (default 3s/5s, worker menimpa).
5. Gerbang `make migrate-down` default `N=1` hanya menguji satu langkah → ditambah `N=18`.
6. §13.2.13 baris origin untuk lokal ambigu → `*` selalu ditolak, `http://localhost[:port]` hanya diizinkan di `local`/`dev`.
