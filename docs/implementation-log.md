# Implementation Log

Format entri (spec §14.9.4):

```markdown
## <TASK-ID> — <judul>
Status: DONE | IN PROGRESS
Tanggal: YYYY-MM-DD
### Implementasi
- ...
### Test
- make test (race) — PASS
- make test-integration — PASS
### Penyimpangan
- Tidak ada | lihat docs/implementation-deviations.md#<TASK-ID>
### Sisa / blocker
- ...
```

## M1-T1 — Bootstrap go module, Makefile, buildinfo
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- go.mod, Makefile, `internal/buildinfo` (commit 0e22fb5)
### Test
- TestDefaults (buildinfo) — PASS
- TestMakeTargetsExist — PASS
### Penyimpangan
- Tidak ada
### Sisa / blocker
- Tidak ada

## M1-T2 — internal/platform/errs
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `codes.go` (70 kode: 53 REST §5.10 + 17 WS-saja §7.4.8, nama konstanta kanonik mekanis), `messages.go` (teks Indonesia verbatim), `errs.go` (New/Wrap/CodeOf/KindOf/WithDetails/DetailsOf), `location.go` (lokasi asal, relatif root modul)
### Test
- go test ./internal/platform/errs/... -race -cover — PASS, coverage 97.9%
- (make test-integration belum relevan: tanpa DB)
### Penyimpangan
- Tidak ada
### Sisa / blocker
- Tidak ada

## M1-T3 — internal/platform/clock dan idgen
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `clock`: `Clock`, `System` (UTC), `Fake` (NewFake/Now/Advance/Set, aman konkuren)
- `idgen`: `Generator`, `NewV7` (UUIDv7 acak), `NewSequence(t)` (deterministik, naik, UUIDv7 valid); `github.com/google/uuid` dijadikan dependensi langsung di go.mod
### Test
- go test ./internal/platform/clock/... ./internal/platform/idgen/... -race — PASS
### Penyimpangan
- Tidak ada
### Sisa / blocker
- Tidak ada

## M1-T4 — internal/platform/config
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `secret.go` (`Secret` redacted di String/LogValue), `config.go` (struct Common/DB/Redis/OTel + API/Realtime/Worker, default per binary via prefill, `LoadAPI/LoadRealtime/LoadWorker` + varian `loadX(map)`), `validate.go` (aturan §13.2.13, error digabung `errors.Join`, tiap pesan diawali nama variabel), `handlers.go` (`ParseWorkerHandlers`)
- `internal/testutil/env.go`: `MinimalEnv`, `ApplyEnv` (satu-satunya daftar env wajib); dependensi `github.com/caarlos0/env/v11`
### Test
- go test ./internal/platform/config/... -race -cover — PASS, coverage 88.7%
### Penyimpangan
- Tidak ada (default `WS_ALLOWED_ORIGINS=http://localhost:3000` dipilih agar realtime lolos MinimalEnv; spec menandainya wajib di prod, yang ditegakkan oleh validasi https)
### Sisa / blocker
- Tidak ada

## M1-T5 — internal/platform/logging
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `logging.go` (`Options`, `New`: JSON/text handler, `time` UTC, field service/version/git_commit/env), `redact.go` (`sensitiveKeys`, `IsSensitiveKey`, `EmailHash`, `NewRedactingHandler` redaksi rekursif + `trace_id`/`span_id`), `errattrs.go` (`ErrAttrs`: error_code, error, error_location)
- Dependensi langsung baru: otel/sdk, otel/trace v1.38.0, testify; `go 1.25` dipertahankan, x/sys dipin v0.35.0 (lihat implementation-deviations.md#M1-T5)
- Redaksi deny-by-default: nilai komposit (map/struct/slice/RawMessage) -> `[REDACTED_COMPLEX]`; grup/WithGroup bernama sensitif meredaksi seluruh isi
### Test
- go test ./internal/platform/logging/... -race -cover — PASS, coverage 100%
### Penyimpangan
- Tidak ada
### Sisa / blocker
- `trace_id`/`span_id` ditambahkan di Handle sehingga bila logger memakai `WithGroup`, keduanya berada di dalam grup tersebut (perilaku slog standar)

## M1-T6 — internal/testutil dasar
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `testdata.go` (`ModuleRoot`, `TestdataPath`, stdlib saja), `trace.go` (`NewTraceRecorder` dengan `tracetest` in-memory exporter, provider global dipulihkan di cleanup), `fakes/failinjector.go` (`FailInjector` sekali pakai, aman konkuren), `repotest/repotest.go` (`Case[T]`, `Each[T]`), `testdata/api/rest/.gitkeep`
- `go.opentelemetry.io/otel` menjadi dependensi langsung (versi v1.38.0 tetap)
### Test
- go test ./internal/testutil/... ./internal/platform/logging/... -race — PASS
### Penyimpangan
- Tidak ada
### Sisa / blocker
- Tidak ada

## M1-T7 — internal/platform/otel
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `span.go` (`Start` dengan redaksi atribut via `logging.IsSensitiveKey`; `End`: atribut `code.function.name`/`code.file.path`/`code.line.number` (semconv v1.37.0) + `kotoki.error_code`; error internal/tanpa kode -> `RecordError` + stack + status Error, error bisnis -> hanya atribut), `setup.go` (`Config`, `Setup`: propagator TraceContext+Baggage, tanpa endpoint = no-op, OTLP gRPC + batcher + AlwaysSample + resource service.name/version/deployment.environment.name, shutdown dibatasi 3 s)
- Dependensi langsung baru: otlptracegrpc v1.38.0; `go 1.25` dan x/sys v0.35.0 dipertahankan (grpc v1.75.0, protobuf v1.36.8 dll. naik sebagai transitif)
### Test
- go test ./internal/platform/otel/... -race -cover — PASS, coverage 92.3%
### Penyimpangan
- Tidak ada
### Sisa / blocker
- Tidak ada

## M1-T8 — internal/platform/metrics
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `metrics.go` (`HTTPBuckets`/`TurnBuckets`/`BargeBuckets`/`AsyncBuckets`, `NewRegistry` dengan collector Go + process tanpa registry global, `Handler`), `db.go` (`PoolStat`, `NewDB`: GaugeFunc acquired/idle/max, `kotoki_db_pool_acquire_wait_seconds`, `kotoki_db_query_duration_seconds{query}`), `redis.go` (`NewRedis`: `kotoki_redis_command_errors_total{op}`, `kotoki_redis_pool_timeouts_total`)
- Dependensi langsung baru: prometheus/client_golang v1.23.2 (v1.25.0 memaksa `go 1.26`, jadi dipin ke v1.23.2); `go 1.25` dan x/sys v0.35.0 dipertahankan
### Test
- go test ./internal/platform/metrics/... -race -cover — PASS (5 test)
### Penyimpangan
- client_golang dipin v1.23.2, bukan versi terbaru, demi direktif `go 1.25`
### Sisa / blocker
- Tidak ada

## M1-T9 — internal/platform/health
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `ops.go` (`Check`, `OpsOptions`, `Ops`: `/healthz` selalu 200, `/readyz` starting sampai `RunStartupChecks` sukses sekali lalu tetap ready meski dependency gagal, draining 503, body tidak memuat teks error; `/metrics` via `metrics.Handler` hanya jika `MetricsEnabled`; pprof didaftarkan manual hanya jika `PprofEnabled`; `Serve` dengan `ReadHeaderTimeout` 5 s dan `Shutdown` 5 s)
### Test
- go test ./internal/platform/health/... -race -cover — PASS (8 test)
### Penyimpangan
- Tidak ada
### Sisa / blocker
- Tidak ada

## M1-T10 — Lint (.golangci.yml) dan make arch-check
Status: DONE
Tanggal: 2026-10-13
### Implementasi
- `.golangci.yml` disalin persis dari spec §14.9.5 (enam aturan depguard termasuk `audio-only-realtime-graph` dan `no-test-deps-in-prod`, forbidigo, nolintlint); golangci-lint v2.1.6 dipasang ke `bin/tools` (gitignored)
- `test/arch`: `forbiddenImports` + `TestForbiddenImportsTable`, `TestCmdAPIWorkerGraphHasNoCgo` (skip bila `cmd/api`/`cmd/worker` belum ada), `TestFeaturePackagesRespectLayout`
- Makefile: `arch-check` melewati paket `cmd/*` yang belum ada (Task 23); `lint` memakai `bin/tools` bila ada
- Perbaikan temuan lint Task 1–9 (18 isu): errcheck, errorlint, gocritic ifElseChain, revive, staticcheck QF1008, `//nolint:gosec` G101 (3 kode error) dan `//nolint:revive` untuk `CodeInvalidJson` (nama dari spec/plan)
### Test
- go test ./test/arch/... -race — PASS; `make lint` 0 issue; uji negatif manual: depguard (feature-no-infra, no-test-deps-in-prod, audio-only-realtime-graph) dan forbidigo time.Now terbukti gagal
### Penyimpangan
- golangci-lint v2.1.6 tidak bisa membaca export data Go 1.27 (toolchain lokal): lint dijalankan/dipasang dengan `GOTOOLCHAIN=go1.25.5`
- `//nolint:revive` untuk `CodeInvalidJson` (brief hanya mengizinkan gosec) karena rename memutus nama di plan
### Sisa / blocker
- `make arch-check` penuh baru bermakna setelah Task 23 membuat cmd/api dan cmd/worker

## M1-T10b — errs: hapus 3 kode konten (D-014)
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- Hapus `CodeConversationStyleNotAvailable`, `CodeExpressionProfileNotAvailable`, `CodeContentInUseAsDefault` dari `codes.go`/`messages.go`/`AllCodes()`; 67 kode = 50 REST + 17 WS-saja
### Test
- go test ./internal/platform/errs/... -race -cover — PASS (97,9%); `TestRemovedContentCodesAbsent`, `TestRestAndWSCounts`
### Penyimpangan
- Tidak ada

## M1-T11 — Harness migration, 000001, helper Postgres/Redis
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `000001_extensions`; `testutil.{NewPostgres,NewEmptyPostgresDSN,NewRedis,RequirePgErr,TryInsert,MustInsert}` (container `pgvector/pgvector:pg17` sekali per paket, template DB `kotoki_template`); dijalankan di **podman rootless** via `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- Pin dependensi demi direktif `go 1.25`: migrate v4.18.3, testcontainers v0.38.0, pgx v5.7.5, go-redis v9.11.0
### Test
- `go test -race -tags=integration ./migrations/...` — PASS
### Penyimpangan
- Lihat docs/implementation-deviations.md (podman, `MustInsert(ctx, t, …)` agar lolos revive context-as-argument)

## M1-T12–T17 — migration 000002–000018
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- DDL disalin dari spec (§3.3, §4.3, §5.3, §6.4, §9.2, §10.2, §11.3, §12.2); fixture `testutil.{UserRow,SeedUser,BalanceAccountRow,TopicRow,…,SeedContent,SessionRow,SeedSession,ReservationRow}`
- `REVOKE UPDATE, DELETE` bersyarat untuk role `kotoki_api/realtime/worker` di 000013 dan 000018 (§13.3.2)
### Test
- `migrations/{identity,content,sessions,balance,learning,messaging}_integration_test.go` — PASS (up/down/up dan stepwise 18 versi)
### Penyimpangan
- `chk_characters_voice_obj` tidak pernah jadi CHECK pertama yang gagal (urutan alfabetis Postgres); test hanya menegaskan 23514

## M1-T18 — test/tools/{modelcheck,covercheck,migcheck}
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- Tiga CLI + `thresholds.txt` (run unit) dan `thresholds.integration.txt` (adapter); sintaks `TOTAL <prefix> <min> [except=a,b]`; `make test-cover-integration`
### Test
- go test ./test/tools/... -race — PASS; `make test-cover` dan `make test-cover-integration` exit 0
### Penyimpangan
- Ambang adapter (postgres/redisstore/amqp) dipindah ke run integration (spec §14.8 memang mengukurnya di sana)

## M1-T19 — Snapshot skema golden
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `migrations/testdata/schema.golden.sql` (24 tabel + `schema_migrations`), `normalizeSchemaDump` membuang partisi bertanggal dan baris `\restrict`/`Dumped`; `testutil.PgDumpSchema` (pg_dump di container)
### Test
- `TestSchemaSnapshot`, `TestTableCount24`, `TestPartitionNormalizationChecks`, `TestFullDownLeavesNothing`, `TestNormalizeDump` — PASS, deterministik pada run berulang

## M1-T20 — sqlc, NewPool, pgx tracer
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `sqlc.yaml` (v1.29.0 via `go tool`), `db/queries/system.sql` (`Ping`), `internal/postgres/{pool.go,tracer.go,sqlcgen}`; override `vector` → `pgvector.Vector`
### Penyimpangan
- `github.com/pgvector/pgvector-go v0.3.0` ditambahkan (allowlist D-003/R-30) karena sqlc mengimpornya untuk `memories.embedding`

## M1-T21 — postgres.Store / WithinTx
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `Store`, `Tx`, opsi `WithIDs/WithClock/WithTimeouts/WithBackoff/WithSleep`; retry 40P01/40001 (4 percobaan), `55P03` → `SERVICE_UNAVAILABLE`, span `db.tx`
### Test
- 11 test integrasi (commit, rollback error/panic, SET LOCAL, retry, lock timeout, span, ctx cancel) — PASS

## M1-T22 — internal/redisstore
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `NewClient` (`redis.UniversalClient`, Sentinel/URL), `NewTraceHook` (span per perintah/pipeline, `redis.Nil` bukan error), `KeyPrefix`, `Ping`
### Test
- `TestKeyPrefix` (unit) + 5 test integrasi — PASS

## M1-T23 — cmd/api, cmd/realtime, cmd/worker
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- Tiga kerangka `run(ctx)` (worker: `run(ctx, args)` + `--handlers`); listener ops di-`Serve` **sebelum** `RunStartupChecks` agar `/readyz` bisa melapor `starting`
### Test
- `go test -race -p 1 -tags=integration ./cmd/...` — PASS (8 test); `make arch-check` exit 0
### Penyimpangan
- Integration test harus `-p 1` di podman (race nama reaper testcontainers antar paket paralel)

## M1-T24 — compose, .env.example, collector, Dockerfile
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- Disalin dari §14.10.2/§14.10.3/§13.10.9; `grafana/otel-lgtm` dipin ke **0.36.0** (rilis terbaru 2026-10-09); `worker.Dockerfile` = api dengan target worker
### Test
- `TestEnvExampleLoads`, `TestEnvExampleOmitsPerBinaryVars`, `TestDockerImagesBuild` (build 4 image dengan podman, user 65532, tanpa shell) — PASS
### Penyimpangan
- compose: `rabbitmq` diberi `user: "999:999"` (podman rootless: `.erlang.cookie` eacces); `MIGRATE_IMAGE` fully qualified `docker.io/…`; Makefile memakai `CONTAINER_ENGINE`/`COMPOSE` (podman bila docker tidak ada)

## M1-T25 — CI GitHub Actions
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `.github/workflows/ci.yml` (lint, unit, integration, e2e, build-images; semua `uses:` dipin SHA: checkout v4.4.0, setup-go v5.6.0, golangci-lint-action v8.0.0, gitleaks-action v2.3.9, upload-artifact v4.6.2, setup-qemu v3.7.0, setup-buildx v3.12.0, login v3.7.0, build-push v6.19.2, trivy-action v0.36.0); `test/e2e/doc.go`
### Test
- `TestCIWorkflowShape` — PASS; actionlint v1.7.7 0 temuan

## Gerbang milestone M1 (2026-10-14)
- `make reset; make env && make up` → postgres/redis/rabbitmq healthy (podman compose)
- `make migrate-up` → `18/u admin_audit`; 24 tabel aplikasi + 4 partisi `session_turns_p*` + 4 `session_events_p*` + `schema_migrations`
- `make migrate-down && make migrate-up` OK; `make migrate-down N=18` → hanya `schema_migrations`; `make migrate-up` OK
- `make lint` → `0 issues.` (GOTOOLCHAIN=go1.25.5); `make test` ok; `make test-integration` ok; `make arch-check sqlc-check migrations-check modelname-check tidy-check` exit 0; `make test-cover` dan `make test-cover-integration` exit 0
- Tiga binary hidup: `:9090/:9091/:9092` `/healthz`=`{"status":"ok"}`, `/readyz`=`{"status":"ready"}`, `/metrics` memuat `go_goroutines` dan `kotoki_db_pool_max_conns` (10/6/4); SIGTERM → `stopped`
- `make docker-build` → api/realtime/worker/migrate ter-build; `podman run --rm kotoki/api:<VERSION>` exit 1 dengan `DB_URL: required`
- `make up-obs` + `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317`: trace `redis.PING` (service `kotoki-api`) tercari di Tempo via Grafana (span `db.Ping`/`db.tx` baru muncul saat fitur memanggilnya)

## Go 1.27
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- Direktif `go 1.27` (toolchain lokal go1.27.2); `go get -u ./...` menaikkan semua dependensi (pin lama untuk `go 1.25` dicabut: otel, x/sys, client_golang, migrate, testcontainers, pgx, go-redis, …)
- golangci-lint v2.14.0 (dibangun dengan go1.27.2; `GOTOOLCHAIN=go1.25.5` tidak diperlukan lagi), govulncheck v1.9.0 (v1.1.4 crash di go1.27), Dockerfile `golang:1.27-trixie`, CI `go-version: "1.27"`
- Perbaikan lint baru (noctx, gosec G101/G204/G703, staticcheck `Value.Emit` → `Value.String`)
- `make vuln` → "No vulnerabilities found" (menutup GO-2026-6629 x/text dan temuan stdlib net/http)
### Test
- make lint 0 issue, make test, make test-integration (podman, -p 1), make test-cover, arch/sqlc/migrations/modelname/tidy check — PASS
## M2-T1 — Auth domain, port, normalisasi input, password policy
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/auth`: `user.go` (User, Role, status, repo port), `auth.go` (Principal, port hasher/token/mailer/ticket/opener/deleter, `AccessSubject`, `Claims`), `tx.go`, `errors.go`, `normalize.go`, `password.go`
- `common_passwords.txt`: 10.000 password umum dari danielmiessler/SecLists `Passwords/Common-Credentials/10k-most-common.txt` (MIT), lowercase + dedupe, di-embed
### Test
- go test ./internal/auth/... -race -cover — PASS (93%)
### Penyimpangan
- `AccessSubject`/`Claims` dideklarasikan di `auth.go` (bukan `jwt.go`) agar port `TokenIssuer` bisa dikompilasi di Task 1

## M2-T2 — Argon2id hasher
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/auth/argon2.go`: `Argon2Hasher` (PHC `$argon2id$v=19$m,t,p$salt$hash`, salt 16 B, key 32 B), semaphore `MaxConcurrent` dengan tunggu `MaxWait` lalu `SERVICE_UNAVAILABLE`, `NeedsRehash`; hash dengan parameter berbeda tetap diverifikasi memakai parameter yang tersimpan
### Test
- go test ./internal/auth -race -run Argon2 — PASS (4 test)

## M2-T3 — JWT TokenIssuer
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/auth/jwt.go`: `JWTIssuer` HS256 dengan header `kid` (8 hex pertama sha256 secret), claims iss/aud/sub/role/fam/jti/iat/nbf/exp, leeway 30 s, `PreviousSecret` hanya untuk verifikasi, secret < 32 byte ditolak; `exp`/`ExpiresAt` dikembalikan dalam UTC
- Dependensi baru: `github.com/golang-jwt/jwt/v5 v5.3.0` (go 1.25 tetap)
### Test
- go test ./internal/auth -race -cover — PASS (21 test)

## M2-T4 — Mailer adapters
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/mailer`: `New(Config, *slog.Logger)` (log ditolak di staging/prod, smtp wajib host), `LogMailer` (`mail.sent`), `SMTPMailer` (STARTTLS wajib, PlainAuth bila username, header RFC 2047), `FakeMailer`
### Test
- go test ./internal/mailer/... -race (+ `-tags integration`: `TestSMTPRequiresSTARTTLS` dengan server SMTP loopback tanpa STARTTLS, body tidak terkirim) — PASS
- Pengiriman SMTP nyata tidak diuji otomatis

## M2-T5 — Mail dispatcher & template reset password
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/auth/dispatcher.go`: `MailDispatcher` (buffer 1000, 4 worker, timeout kirim 10 s, retry 3× 1 s/5 s/15 s, `Enqueue` tidak pernah memblok; buffer penuh → ERROR `mail.queue_full`; gagal permanen → ERROR `mail.dropped` dengan email di-mask), `Close(ctx)` menguras antrean, `RenderPasswordReset` + `templates/password_reset.txt` (go:embed)
### Test
- go test ./internal/auth -race — PASS (dispatcher: deliver, retry, timeout, buffer penuh; render)
### Penyimpangan
- `TestDispatcherBufferFull`: pesan 1 diambil worker, pesan 2 masuk buffer, pesan 3 ditolak (brief menyebut "kedua"; urutan ini deterministik)

## M2-T6 — Balance domain, port, OpenAccount, pembacaan saldo (+ fake & contract suite)
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/balance`: `Account`, `LedgerEntry`, `Movement`, `EntryType`, `ReferenceType`, port `Repository` (Lock/Get/InsertAccount, `ApplyMovement`, `FindLedgerByIdempotencyKey`, `ListLedger` dengan filter tipe + cursor), `Store`/`Tx`, `OpenAccount`, `Service.GetBalance` (`AllowedDurations` ≤ saldo), `Service.ListTransactions` (limit+1), `Description` (§4.7)
- `fakes.BalanceStore` (rollback snapshot saat fn error, `FailNext`, `WithBalanceIDs`), `repotest.BalanceRepoSuite`
### Test
- go test ./internal/balance/... ./internal/testutil/... -race — PASS
### Penyimpangan
- Suite menerima `BalanceHarness{Store, SeedUser}` (bukan hanya `newStore`) karena store PostgreSQL butuh baris `users` nyata (Task 17)
- `GetBalance` untuk user tanpa account → `USER_NOT_FOUND`

## M2-T7 — Grant & Adjust (idempotent)
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `balance.Service`: `GrantTx`/`AdjustTx` (lock account → cek key `grant:`/`adjust:` → replay bila user, tipe, delta sama, selain itu `IDEMPOTENCY_KEY_REUSED` → `ApplyMovement`), `Grant`/`Adjust` mandiri (retry sekali bila `ErrDuplicateMovement`), `ValidateAdminIdempotencyKey`, `MovementResult`
- Validasi: `seconds` 1–360000, `|seconds_delta|` 1–360000, `reason` 3–500 rune; adjustment negatif → `BALANCE_ADJUSTMENT_NEGATIVE` dengan `details {available_seconds, seconds_delta}`; audit sengaja bukan di sini (Task 14)
### Test
- go test ./internal/balance -race — PASS

## M2-T8 — Fake & contract suite repository auth
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `fakes.AuthStore` (`auth.Store`): transaksi menyarang `BalanceStore.WithinTx` sehingga rollback membatalkan user + account sekaligus; `FailNext`, accessor `User`/`RefreshTokens`; `Accounts()` memanggil `balance.OpenAccount`
- `repotest.{UserRepoSuite,RefreshTokenRepoSuite,PasswordResetRepoSuite}` (+ `NewUser`); hanya mengimpor `auth` dan testify, siap dipakai ulang oleh repo PostgreSQL (Task 16)
### Test
- go test ./internal/testutil/... -race — PASS (3 suite + rollback/commit/FailNext)

## M2-T9 — auth.Service: Register & Login
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/auth/service.go`: `Config`, `Events`, `NewService` (dummy hash untuk menyamakan timing login), `Register` (hash di luar tx; tx: Insert user → `OpenAccount` → refresh family; `ErrEmailTaken`/`ErrPhoneTaken` → 409), `Login` (email tak dikenal / DELETED → `Verify(dummy)` tepat 1× → `INVALID_CREDENTIALS`; SUSPENDED hanya setelah password benar; rehash best-effort tanpa mengubah `password_changed_at`), audit log `audit=true` (`register`, `login_succeeded`, `login_failed`) tanpa email/password mentah
- `internal/platform/metrics/auth.go`: `kotoki_auth_events_total{event,result}`; nilai di luar daftar di-ignore
### Test
- go test ./internal/auth ./internal/platform/metrics -race — PASS

## M2-T10 — auth.Service: Refresh rotation, reuse detection, Logout
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `Refresh` (spec 3.8.3): `LockByHash` → grace 10 s inklusif (`revoked_at >= now − grace`, penggantinya belum revoked → 409 `REFRESH_TOKEN_ROTATED`, rollback) → selain itu reuse: `RevokeFamily(REUSE_DETECTED)` **di-commit** lalu 401 (pola commit-then-error), log WARN `refresh_reuse_detected` tanpa token; expired (`expires_at <= now`) → 401; user non-ACTIVE → revoke `USER_STATUS_CHANGED` di-commit + 401; sukses → token baru (family sama, `ParentID`), role dari DB
- `Logout`: idempoten, token tak valid → nil; audit `logout`
- Event metrik: `refresh` success/failure; reuse di luar grace → `refresh_reuse` failure
- `fakes.AuthStore.MutateUser` untuk setup test
### Test
- go test ./internal/auth -race — PASS (12 test refresh/logout termasuk konkurensi 2 goroutine)

## M2-T11 — auth.Service: Forgot & Reset password
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `ForgotPassword`: email invalid → `VALIDATION_FAILED`; selain itu selalu nil (limiter email, user tak dikenal/non-ACTIVE → tanpa token & mail); user ACTIVE → tx (invalidate token terbuka + insert token TTL 15 m, hash SHA-256) lalu `Enqueue` mail; audit `password_reset_requested`
- `ResetPassword`: decode token → `FindByHash` → `ValidatePassword` (token tidak terpakai bila lemah) → `Hash` di luar tx → tx `LockByHash` re-check → ganti password (`password_changed_at = now`), `MarkUsed`, `InvalidateOthers`, revoke semua refresh `PASSWORD_RESET`; `expires_at <= now` → invalid; event metrik `password_reset`; audit `password_reset_completed`
- `fakes.AuthStore.ResetTokens` untuk test
### Test
- go test ./internal/auth -race — PASS (12 test forgot/reset)

## M2-T12 — auth.UserService (Me, consents, hapus akun)
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/auth/user_service.go`: `Me`, `RequireActive` (SUSPENDED dan DELETED → `ACCOUNT_SUSPENDED`), `UpdateConsents` (versi ≠ terkini → `VALIDATION_FAILED not_current`; versi sama tidak mengubah `terms_accepted_at`; non-ACTIVE → 403; audit `consent_updated`), `DeleteAccount` (langkah 1 & 5 §3.13.3: status ACTIVE, `Verify` password, lalu `AccountDeleter`; error `ErrAccountHasActiveSession` diteruskan; audit `account_deletion_requested`), `CurrentTermsVersion`
- Kontrak repo diperketat: `UpdateConsents` hanya menggeser `terms_accepted_at` bila versi berubah (`repotest` kasus baru; fake diperbaiki)
### Test
- go test ./internal/auth ./internal/testutil/... -race — PASS

## M2-T13 — TicketService + redisstore.TicketStore
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `auth.TicketService.Issue` (§3.11): user ACTIVE (terms usang tetap boleh), sesi RESERVED sebelum deadline / STARTING / ACTIVE, selain itu `SESSION_START_DEADLINE_PASSED` / `SESSION_NOT_CONNECTABLE {status}`; error store → `SERVICE_UNAVAILABLE`; `realtime_url` = `REALTIME_PUBLIC_URL` + `/api/v1/realtime`
- `redisstore.TicketStore`: key `kotoki:ws_ticket:{hex sha256(ticket)}`, `SET NX EX`, ulang sekali bila tabrakan, `GETDEL` untuk Consume; `fakes.TicketSessionReader`/`fakes.TicketStore`
### Test
- go test ./internal/auth -run Ticket; `-tags integration ./internal/redisstore -run Ticket` (Podman) — PASS (single-use konkuren, expiry, Redis down)

## M2-T14 — admin: port, Grant/Adjust dengan audit, daftar audit
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/admin`: `AuditLog`/`AuditFilter`/`AuditRepository`, `RequestMeta`, `SessionGate`, `Tx` (= `auth.Tx` + `balance.Tx` + `Audit()` + `SessionGate()`), `Store`; `Service.Grant`/`Adjust` (satu tx: user ada → `GrantTx`/`AdjustTx` → audit `BALANCE_GRANT`/`BALANCE_ADJUSTMENT` hanya bila bukan replay; retry sekali bila `ErrDuplicateMovement`; span `admin.Grant`/`admin.Adjust`), `ListTransactions`, `ListAudit`, `IsActiveAdmin`
- Fake: `fakes.AdminStore` (tx bersarang di AuthStore → BalanceStore; rollback juga untuk audit & gate), `fakes.SessionGate`, `fakes.CallRecorder` (urutan lock lintas repo); `repotest.AuditRepoSuite`
### Test
- go test ./internal/admin ./internal/testutil/... -race — PASS (audit gagal → saldo & ledger kembali)

## M2-T15 — admin: daftar/detail user, ubah status, DeleteAccount
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `admin.Service`: `ListUsers`/`GetUser` (`UserView`, `UserDetail` dengan sesi terbuka & jumlah refresh token aktif), `ChangeStatus` (ACTIVE↔SUSPENDED, → DELETED final; DELETED→apa pun = `INVALID_USER_STATUS_TRANSITION`; diri sendiri = `CANNOT_MODIFY_SELF`; status sama = 200 tanpa audit; SUSPENDED merevoke refresh `USER_STATUS_CHANGED`; audit `USER_STATUS_CHANGE {from,to,revoked_refresh_tokens}`), `DeleteAccount` (memenuhi `auth.AccountDeleter`; urutan lock account → user → sesi; STARTING/ACTIVE/ENDING → 409 `ACCOUNT_HAS_ACTIVE_SESSION {session_id}` tanpa perubahan; RESERVED dibatalkan lewat `SessionGate`; refresh & reset token dihapus) lewat `deleteAccountTx`
### Test
- go test ./internal/admin -race — PASS
### Penyimpangan
- `auth.PasswordResetRepository` mendapat method `DeleteAllForUser` (Task 1 tidak memuatnya padahal §3.13.3 langkah 4 menghapus `password_reset_tokens`); fake + `repotest` ditambah

## M2-T17 — postgres: repo balance (account & ledger) + BalanceStore
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `db/queries/balance.sql` + sqlc; `internal/postgres/balance_repo.go` (`(*Tx).Balance()`; `ApplyMovement` = `UPDATE balance_accounts … RETURNING` lalu `INSERT balance_ledger` dengan `*_after`; `ux_balance_ledger_idempotency` → `ErrDuplicateMovement`; lainnya `INTERNAL_ERROR`), `convert.go`, `store_views.go` (`BalanceStore()`); `testutil.AssertReconciled` (R1–R5 §4.8 verbatim)
### Test
- go test -tags integration ./internal/postgres -run 'PGBalance|PGLedger|PGGrant|PGAdjust' (Podman) — PASS: suite repo, ledger append-only (23001), 20 goroutine key sama → 1 ledger + 19 replay, 20 user key sama → 1 sukses + 19 `IDEMPOTENCY_KEY_REUSED`, adjust negatif ditolak; reconcile R1–R5 kosong di akhir tiap test
### Penyimpangan
- Dikerjakan sebelum Task 16 (Task 16 membutuhkan `Balance()` untuk `Accounts()`)
- Suite kontrak diperbaiki agar bentuk movement sah menurut `chk_ledger_shape` (adjustment membawa admin & reason) — ketahuan oleh PostgreSQL, fake tidak menegakkannya

## M2-T16 — postgres: repo users, refresh token, password reset, AuthStore
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `db/queries/{users,refresh_tokens,password_resets}.sql` + sqlc; `internal/postgres/{user_repo,refresh_token_repo,password_reset_repo}.go` (`ux_users_email`/`ux_users_phone` → `ErrEmailTaken`/`ErrPhoneTaken`, `ErrNoRows` → sentinel, `FOR UPDATE` untuk `LockByID`/`LockByHash`, waktu selalu dari parameter, filter list memakai `starts_with(lower(email), …)`); `(*Tx).Accounts()` menjembatani ke `balance.OpenAccount`; `(*Store).AuthStore()`
### Test
- go test -tags integration ./internal/postgres -run 'PGUser|PGRefresh|PGPassword|PGRegister|PGCount' (Podman) — PASS: ketiga suite `repotest` di PostgreSQL, DELETE satu statement untuk rantai self-FK, register atomik (rollback user+account+ledger), reuse refresh di PG commit revoke, `CountActive` batas `expires_at == now`

## M2-T18 — postgres: audit repo, SessionGate, TicketSessionReader, AdminStore
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `db/queries/{admin_audit,session_gate}.sql`; `internal/postgres/{admin_audit_repo,session_gate}.go`: `(*Tx).Audit()` (details JSONB), `(*Tx).SessionGate()` (`OpenSessions` = sesi RESERVED/STARTING/ACTIVE/ENDING; **`CancelReserved` stub** → `INTERNAL_ERROR "session cancel not wired before M3"`), `postgres.NewTicketSessionReader` (JOIN sessions × balance_reservations), `(*Store).AdminStore()`; `*Tx` memenuhi `admin.Tx`
### Test
- go test -tags integration ./internal/postgres -run 'PGAudit|PGAdmin|PGSessionGate|PGTicket' (Podman) — PASS: suite audit, append-only (23001), grant + audit atomik (audit ditolak DB → saldo kembali), alur hapus akun (status DELETED, token terhapus, account & ledger utuh, reconcile kosong), gate tanpa sesi, stub CancelReserved, reader `ErrSessionNotFound`
### Sisa (M3)
- Task M3 wajib mengganti isi `CancelReserved` dengan `session.Service.CancelInTx` dan membalik `TestPGSessionGateCancelReservedNotWired`

## M2-T19 — platform/ratelimit: Policy, Catalog, limiter lokal
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/platform/ratelimit`: `Policy` (+ `RatePerMs`), `Decision`, port `Limiter`, `Catalog` 15 policy (13 dari M2 + `ws_connect_ip`, `ws_attach_user` untuk M5, semuanya dari §13.4.3), `Get` (panic bila nama tak dikenal), `SubjectEmail`/`SubjectIP`, `Local` (token bucket in-memory + LRU, jam dari `clock.Clock`)
### Test
- go test ./internal/platform/ratelimit -race — PASS

## M2-T20 — redisstore.RateLimiter (Lua + fallback)
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `redisstore.RateLimiter`: script Lua §13.4.3 verbatim (`redis.NewScript` → EVALSHA, fallback EVAL otomatis), key `kotoki:ratelimit:{policy}:{subject}` dengan `PEXPIRE`, waktu dari `redis.call('TIME')`; Redis error → limiter lokal (fail-open), `kotoki_ratelimit_backend_errors_total`, log warn maksimal 1×/10 s (jam dari `clock.Clock`); `metrics.RateLimit` (`kotoki_ratelimit_rejected_total{policy}`, `kotoki_ratelimit_backend_errors_total`)
### Test
- go test ./internal/platform/metrics -run RateLimit; go test -tags integration ./internal/redisstore -run RedisLimiter (Podman) — PASS (burst, refill, TTL, isolasi subject, fallback saat Redis mati)

## M2-T21 — httpapi fondasi: Handle, Page, WriteError, cursor, golden error
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/httpapi`: `Handle`/`HandleNoBody` (Content-Type wajib `application/json` → 415, `DisallowUnknownFields` → `unknown_field`, tipe salah → `invalid_type`, JSON rusak/sisa data → `INVALID_JSON`, > 64 KiB → 413, `Validate()`), `Request[B]` (+ `PathUUID`, `Query`, `Cookie`, `SetCookie`, `SetHeader`, `SetStatus`, `ClientIP`, `UserAgent`; efek diterapkan di jalur sukses dan error), `NoContent` → 204, `WriteError` (kode dikenal → status §5.10; kode tak dikenal → 500 `INTERNAL_ERROR` tanpa bocor details; error internal di-log ERROR sekali dengan `error_code` + `error_location`; `Retry-After: 1` untuk 503 kecuali sudah diset), `WriteJSON`, `StatusOf`, `FieldError`, `Page`/`NewPage`, `EncodeCursor`/`DecodeCursor`, `ParseLimit`; konteks `WithRequestID`/`WithLogger`/`WithClientIP`
- Golden `testdata/api/rest/errors/<KODE>.json` untuk 50 kode katalog REST
- Dependensi baru: `github.com/go-chi/chi/v5`
### Test
- go test ./internal/httpapi -race — PASS (74 test)
### Penyimpangan
- 17 kode yang hanya ada di katalog WebSocket tidak punya status REST; bila bocor ke `WriteError` diperlakukan seperti kode tak dikenal (500 `INTERNAL_ERROR`) — test `TestWriteErrorUnknownAndWSOnlyCodes`

## M2-T22 — httpapi middleware
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/httpapi/middleware*.go`: `RequestID` (UUID valid dipakai, selain itu UUIDv7), `Recoverer` (panic → 500, satu log ERROR dengan `error_location`), `AccessLog` (route pattern, status, `duration_ms`; tanpa path mentah/query sehingga `?ticket=` tak bocor), `CORS` (tanpa library), `BodyLimit` (413), `SecurityHeaders` (+ `no-store` untuk `/auth/*` dan `/me*`), `ClientIP`/`ClientIPMiddleware` (XFF dari kanan melewati proxy tepercaya), `RateLimit` (fail-open, `RateLimit-Remaining`, `Retry-After` dibulatkan ke atas) dengan `IPSubject`/`UserSubject`, `Authn`, `RequireAdmin` (claim lalu DB; audit `admin_access_denied`), `OriginCheck`, `Telemetry` (span `HTTP {method} {route}`, 5xx = Error), `HTTPMetrics`
- `metrics.NewHTTP`: `kotoki_http_requests_total` dan `kotoki_http_request_duration_seconds`; rute tak cocok → `route="unmatched"`; `/healthz`, `/readyz`, `/metrics` tidak dihitung
### Test
- go test ./internal/httpapi ./internal/platform/metrics -race — PASS
### Penyimpangan
- `Telemetry` ditulis sendiri di atas `platform/otel` (bukan `otelhttp`) supaya nama span memakai pola chi yang baru diketahui setelah routing; propagasi W3C tetap di-extract dari header

## M2-T23 — Router, empat grup, rute auth & me, TestRouteTable
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/httpapi/{router,deps}.go`: `NewRouter(Deps)` (RequestID → ClientIP → Recoverer → AccessLog → HTTPMetrics → Telemetry → SecurityHeaders → CORS → BodyLimit; grup publik/user; 404 `NOT_FOUND`, 405 `METHOD_NOT_ALLOWED` lewat `WriteError`; `/healthz` dst. tidak dilayani), interface layanan kecil (`authService`, `userService`)
- `auth_{routes,handler,dto}.go`: register 201, login 200, refresh (cookie; hapus cookie hanya pada `REFRESH_TOKEN_INVALID`, 409 tidak menyentuh cookie), logout 204 (selalu hapus cookie), forgot 202 generik, reset 204, terms 200; policy IP di rute, policy email (`auth_login_email`, `auth_forgot_email`) di handler setelah decode; `me_{routes,handler,dto}.go`: `GET /me`, `PUT /me/consents`, `DELETE /me` 202 + hapus cookie
- Golden: `testdata/api/rest/auth/{register_201,terms_200}.json`, `me/me_200.json`
### Test
- go test ./internal/httpapi -race — PASS: `TestRouteTable` (rute terdaftar == baris Ready; grup & policy dibuktikan lewat perilaku), `TestInternalPathsNotServed`, alur register→login→refresh→409→logout→401, origin check, rate limit email, forgot identik, reset, terms, cookie `Secure=false`
### Penyimpangan
- Atribut `Set-Cookie` diverifikasi sebagai himpunan (Path, HttpOnly, Secure, SameSite=Strict, Max-Age=2592000), bukan urutan teks: `net/http` selalu menulis `Max-Age` sebelum `HttpOnly`

## M2-T24 — Rute admin & balance user
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/httpapi/{balance,admin}_{routes,handler,dto}.go`: `GET /balance` (`allowed_durations`), `GET /balance/transactions` (filter `entry_type`, cursor UUID, tepat 9 field user: tanpa `reason`/`admin_user_id`/`idempotency_key`); admin: `GET /admin/users` (validasi `email` ≥ 3, `status`, `role`), `GET|PATCH /admin/users/{user_id}`, `POST …/balance-grants`, `POST …/balance-adjustments` (header `Idempotency-Key` UUID; replay → 200 + `Idempotent-Replayed: true`), `GET …/balance/transactions` (bentuk ledger admin), `GET /admin/audit-logs`; grup admin = `Authn` → rate limit `admin_user` → `RequireAdmin`; `PATCH`/grant/adjust meneruskan `RequestMeta{RequestID, ClientIP, UserAgent}`
- `TestRouteTable`: baris balance + admin M2 berstatus `Ready`; golden `testdata/api/rest/{admin/grant_201,balance/balance_200}.json`
### Test
- go test ./internal/httpapi -race — PASS
### Penyimpangan
- Rate limit `admin_user` dipasang sebelum `RequireAdmin` (bukan sesudahnya) agar token USER tidak memicu query DB per request

## M2-T25 — Konfigurasi auth, wiring `cmd/api`, gerbang integrasi M2
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/platform/config/auth.go`: struct `Auth` (JWT, TTL, argon2, mail, cookie, trusted proxy) di-embed ke `API`; default §13.2.7; rahasia JWT tidak pernah tercetak; validasi lintas-field
- `cmd/api/main.go`: `buildAPI` merakit store, JWT, argon2, mailer + dispatcher, balance/admin/auth/user service, rate limiter Redis+Local, router; listener publik di-bind sebelum startup check, `Serve` sesudahnya; `http.Server` dengan timeout; shutdown mengikuti `SHUTDOWN_TIMEOUT`
- `Makefile promote-admin` + `test/arch/promote_admin_test.go`
### Test
- `cmd/api/m2_gate_integration_test.go` (komponen nyata, PG+Redis, jam palsu): `TestM2GateAuthFlow`, `RefreshReuse`, `ResetFlow`, `Consent`, `AdminGrant`, `DeleteAccount`, `TicketTTL`; tiap tes diakhiri `AssertReconciled`
- `go test -tags integration -p 1 ./cmd/api`, `go test ./internal/platform/config -race`, `go test ./test/arch` — PASS
### Penyimpangan
- Tes gerbang ditempatkan di `cmd/api` (bukan `internal/httpapi`) karena depguard `handler-thin` melarang paket httpapi mengimpor postgres/redisstore/pgx

## M2-Final — Perbaikan hasil review seluruh branch
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `auth.Service.Login`: transaksi penerbitan sesi mengunci baris user (`LockByID`) dan memeriksa ulang `password_hash` + status; rehash dipindah ke dalam transaksi itu dan hanya berlaku bila hash tidak berubah (reset/suspend/hapus yang commit di antara verifikasi dan sesi kini menang)
- `cmd/api`: penutupan dispatcher dibatasi `SHUTDOWN_TIMEOUT`
- `config`: `PASSWORD_RESET_URL_BASE` wajib https mutlak (bukan default localhost) di staging/prod
- `httpapi.ClientIP`: semua baris header `X-Forwarded-For` digabung sebelum ditelusuri dari kanan
### Test
- RED→GREEN: `TestLoginRacingPasswordReset`, `TestLoginRacingSuspension`, `TestAuthConfigValidate` (2 kasus prod), `TestClientIP` (multi-baris), `TestDispatcherCloseHonoursDeadline`

## M1-minors — Catatan minor M1 yang ditunda
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `health.Ops.RunStartupChecks`: cek yang gagal diulang dengan backoff (200 ms → 2 s) sampai ctx habis; `cmd/api`: listener ops memakai konteks sendiri dan berhenti paling akhir (`/readyz` = draining terlihat), error `Serve` ops ikut dipantau
- `config`: staging/prod hanya menerima `sslmode=verify-full|verify-ca` (hilang/prefer/require ditolak)
- `migcheck`: deteksi migrasi yang dihapus/di-rename (`--diff-filter=MD --no-renames`)
- `redisstore`: ctx cancel diteruskan apa adanya; error server (WRONGTYPE, Lua) → `INTERNAL_ERROR`, hanya LOADING/READONLY/dst. → `SERVICE_UNAVAILABLE`; `key_prefix` EVAL/EVALSHA dari key pertama; `DisableIdentity`
- `postgres`: pelanggaran constraint (23xxx) tidak lagi jadi span error; retry habis, `lock_timeout`, `statement_timeout` → `SERVICE_UNAVAILABLE`; panic menandai span `db.tx`; dokumentasi bahwa `fn` bisa dijalankan ulang; `WithTimeouts` kini dipasang dari `DB_LOCK_TIMEOUT`/`DB_STATEMENT_TIMEOUT` di `cmd/api`
- Migrasi 000013/000018: trigger `BEFORE TRUNCATE` pada `balance_ledger` dan `admin_audit_logs`
### Penyimpangan
- Migrasi 000013/000018 diubah di tempat (belum ada tag v0.1.0, `migcheck` lulus); golden skema diperbarui
- Tidak dikerjakan: REVOKE ulang untuk role yang dibuat setelah migrasi (sudah dijadwalkan M10) dan penggantian error 40001 saat ctx dibatalkan di tengah backoff (mengembalikan error ctx dinilai benar)
- Tanpa tes otomatis: urutan shutdown ops (draining tidak teramati pada shutdown cepat) dan span panic

## M4-T1 — Tipe umum, port, error taxonomy, guard aturan impor
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/{provider,errors,stt,llm,tts,evaluation,embedding}.go`: tipe dan port §8.2, `ErrorKind`/`*Error`, `KindOf`, `IsRetryable`, `FallbackEligible`, `CountsForBreaker`, `ClassifyHTTPStatus`, `NewError` (pesan dipotong 512 byte di batas rune), `NewCircuitOpen`
### Test
- `errors_test.go` (tabel taksonomi, 404, local_concurrency, kuota, KindOf, status HTTP, string, potong 512, Unwrap) dan `importrules_test.go` (aturan impor provider/fake/prompt) — PASS
### Penyimpangan
- `NewCircuitOpen` diekspor (test di paket eksternal)

## M4-T2 — `NormalizeJA` dan util PCM
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/text.go`: `NormalizeJA` (komposisi kana + dakuten/handakuten tanpa `x/text`, spasi antar rune non-ASCII dibuang, trim)
- `internal/provider/pcm.go`: `EncodeWAV`/`DecodeWAV`/`WAVDurationMs`, `ResamplePCM16` (linear), `RMSdBFS`, `pcmRing` (posisi logis absolut)
### Test
- `text_test.go`, `pcm_test.go`, `pcm_internal_test.go` — PASS (race)

## M4-T3 — `RouterConfig` dan `Validate()`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/config.go`: `RouterConfig` (tag `env`/`envDefault` sesuai §8.5.1, D-017), `ConcurrencyConfig` + `Limit`, `ModeFor`, `ActiveSTT`/`ActiveLLM`/`TTSVendors`/`STTModel`/`LLMModel`/`EvaluationModel`, `Validate` (flag/key/model hanya untuk kapabilitas mode `real`; semua pesan menyebut nama env), `Secret` untuk API key (tidak tercetak)
### Test
- `config_test.go`, `config_env_test.go`: satu kasus per aturan §8.5.1, default env, override env, tidak ada seleksi vendor/profil TTS — PASS
### Penyimpangan
- Galat parse bool menyebut nama field Go (bukan nama env); pemetaan nama env dilakukan saat `platform/config` menanam `RouterConfig`

## M4-T4 — Circuit breaker
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/breaker.go`: `Breaker`/`Ticket`/`BreakerConfig`, 9 transisi §8.5.4, window bergeser, probe half-open (tiket usang diabaikan lewat epoch), `onTransition` dipanggil di luar lock
### Test
- `breaker_test.go`: `TestBreakerStateMachine` (8 subtest), `TestBreakerConcurrent` (50 goroutine) — PASS `-race -count=20`

## M4-T5 — Semaphore konkurensi dan `HealthTracker`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/semaphore.go`: `semaphoreSlot` (try atau tunggu `wait`; penuh → `RATE_LIMITED{local_concurrency}` yang tidak dihitung breaker; ctx habis → `CANCELLED`/`TIMEOUT`; release idempoten)
- `internal/provider/health.go`: `HealthTracker` (60 bucket × 1 s, p95 latensi, `last_error` bertahan di luar window), `ProviderHealth` dengan bentuk JSON §8.5.5, `BreakerState.String()`
### Test
- `semaphore_test.go`, `health_test.go` — PASS (race)

## M4-T6 — Metrik Prometheus provider
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/metrics.go`: `Metrics` dengan seluruh metrik §8.8.1 + §8.7.4 (nama/label persis), helper per metrik yang aman untuk receiver nil, `vendor_code` tak dikenal → `other`, `kotoki_session_cost_usd`/`_per_minute_usd` tanpa label, `SetDefaultMetrics` + `ObserveLLMFirstSentence`
### Test
- `metrics_test.go` — PASS (race)
### Penyimpangan
- Bucket `kotoki_session_cost_per_minute_usd` tidak dinyatakan spec: dipilih 0,001…0,5; `final_timeout` ditambahkan ke daftar vendor_code yang dikenal

## M4-T7 — `PriceTable` port dan `pricing.Table`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/pricer.go`: port `PriceTable`, `ModelRef`; `internal/provider/pricing/{pricing.go,prices.yaml}`: loader YAML ketat (`KnownFields`, UPPERCASE, duplikat/negatif ditolak), `LLM`/`TTS`/`Audio` (wildcard `*`, harga cache-hit), `Missing` (tanpa entri atau harga 0); semua harga 0 sampai benchmark
### Test
- `pricing/pricing_test.go` — PASS (race)

## M4-T8 — Fake: konfigurasi, keputusan gagal, fault set, poller
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/fake/config.go`: `Config` (env `FAKE_*`), `CapConfig` + `LoadCapConfig` (`FAKE_<PRIMARY|FALLBACK>_<CAP>_*`, FALLBACK hanya TTS, galat menyebut nama env), `Deps`
- `internal/provider/fake/faults.go`: `Fault`/`FaultSet`/`FaultSource`/`NoFaults`, `ParseFaultSet` (until, kombinasi mode/kapabilitas, status→kind), `decider` deterministik (every-N atau hash seed|instance|capability|counter), `Poller` (snapshot atomik; galat fetch → pertahankan ≤ 10 s lalu kosongkan; WARN ≤ 1×/menit)
### Test
- `faults_test.go`, `config_env_test.go` — PASS (race); termasuk `TestPollerClearsAfterStaleWindow` (review focus 5)
### Penyimpangan
- Hash keputusan = FNV-64a + finalizer splitmix64 (FNV saja bias pada bit tinggi untuk counter berurutan)

## M4-T9 — Fake LLM + `providertest.LLMSuite`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/fake/llm.go` (+ `errors.go`): `LLM` deterministik (template opening/regular, `#long` ×5, delta 3 rune, `MaxTokens`/`Stop`, usage nyata, fase gagal start/before_first/mid/hang, fault runtime error/slow/timeout, override template), `internal/provider/providertest/llm.go`: `LLMSuite` (7 subtest) untuk fake dan adapter M9
### Test
- `fake/llm_test.go` — PASS (race)
### Penyimpangan
- Lihat ledger: pembagian template (4 reguler + opening) dan `SyncErrorIsProviderError` memakai ctx yang sudah dibatalkan

## M4-T10 — Fake TTS + `providertest.TTSSuite`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/fake/tts.go`: PCM16 24 kHz, durasi `max(300 ms, rune×120)/SpeakingRate` (0 → 1), chunk 20 ms, nada 220 Hz −20 dBFS atau senyap, fault start/before_first/mid/hang + runtime `fail_after_first_audio`, mengabaikan voice/model karakter; `providertest/tts.go`: `TTSSuite` (5 subtest)
### Test
- `fake/tts_test.go` — PASS (race)

## M4-T11 — Fake STT (korelasi envelope) + `providertest.STTSuite`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/fake/stt.go`, `stt_match.go`: segmentasi energi (mulai ≥ 200 ms bicara, tutup 600 ms senyap atau `Finalize`), PARTIAL tiap 200 ms waktu audio, FINAL setelah `LatencyMS`, korelasi envelope fixture (±10 frame, skor diskalakan overlap/panjang, tie → nama file terkecil) atau frasa energi, antrean event (PARTIAL baru menggantikan yang antre; FINAL/ERROR/CLOSED tidak pernah dibuang), buffer audio 2 s, fault timeout/disconnect/phase env; `providertest/stt.go`: `STTSuite` (6 subtest)
### Test
- `fake/stt_test.go` — PASS (race, count=3)
### Penyimpangan
- Skor kecocokan dikalikan overlap/panjang terpanjang supaya input pendek tidak cocok dengan fixture panjang; `CapConfig` dengan phase/kind kosong diperlakukan sebagai `start`/`UNAVAILABLE`

## M4-T12 — Fake Evaluation dan Embedding + suite
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/fake/{eval,embedding,oneshot}.go` + `fixtures/kotoki_fake_example.json`: fixture JSON ter-embed (`FAKE_EVAL_FIXTURE_DIR` menimpa), `"{{score}}"` → `50 + fnv32(RequestID) % 41`, `SchemaName` divalidasi `[a-z0-9_]{1,64}` (tak bisa keluar dari direktori), embedding 1536 dim via trigram hashing + L2, fault error/timeout/slow lewat `oneShot`; `providertest/{eval,embedding}.go`: `EvaluationSuite`, `EmbeddingSuite`
### Test
- `fake/eval_test.go`, `fake/embedding_test.go` — PASS (race)

## M4-T13 — `redisstore.FakeFaults`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/redisstore/fakefaults.go`: `FakeFaults` membungkus `fake.Poller` dengan fetch `GET` (`redis.Nil` → tanpa fault); snapshot bertahan ≤ 10 s saat Redis mati lalu dikosongkan
### Test
- `fakefaults_integration_test.go` (integration, Podman): polling set/del key, key tak ada, outage dengan jam palsu, dokumen rusak — PASS

## M4-T14 — Router inti
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/router.go`: interface `RealtimeRouter`/`AsyncRouter`/`RoutedSTTSession`/`Generation`/`GenerationSummary`/`TextRequest`/`TextResult`, `ProcessRole`, `RouterDeps`, `NewRouter` (validasi config + adapter), breaker & semaphore per (vendor, kapabilitas) (wait STT 0, LLM/TTS 100 ms realtime atau 30 s worker, EVALUATION/EMBEDDING 30 s), `admit` (breaker lalu semaphore, fail-fast `circuit_open`), `ttsCandidates` (primary + fallback karakter, `voice_binding_missing`, circuit-open), `HealthSnapshot`, `llmModel`/`evaluationModel`/`textModel`/`ttsVendors`; `Breaker.WouldAllow`, `HealthTracker.Touch`
### Test
- `router_test.go`, `router_helpers_test.go` (+ `export_test.go`) — PASS (race)
### Penyimpangan
- Asersi `var _ RealtimeRouter/AsyncRouter` dipindah ke Task 18 (method lain menyusul di T15–T18)

## M4-T15 — `Generation`: LLM satu vendor
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/generation.go`: `Router.NewGeneration`, `StreamLLM` (breaker + semaphore, model/default dari config, first-token timer, `LLM_TOTAL`, `DONE{TRUNCATED}` setelah `MarkAudioSent` (deadline sesi tidak dihitung breaker, `LLM_TOTAL` dihitung), `CONTENT_FILTERED` tidak dihitung breaker dan diteruskan, cancel → `DONE{CANCELLED}`, usage estimasi saat gagal, metrik/biaya/health, log DEBUG tanpa teks), `Summary`, `MarkAudioSent`, `TTSEpochChanged`; `generation_tts.go` berisi stub `Synthesize` sampai T16
- `fake`: `HangAfter` (`FAKE_<INST>_<CAP>_HANG_AFTER`, fault `after_events`) agar stream bisa macet setelah N delta
### Test
- `generation_llm_test.go` (13 tes) — PASS (race, count=5)

## M4-T16 — `Generation`: TTS (epoch, voice dipin, VoiceID/model per vendor)
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/generation_tts.go`: `Synthesize` (validasi `SpeakingRate` 0 → 1,0 dan 0,80–1,05; slot primary/fallback dipin per generasi, awal di fallback bila breaker primary terbuka; mode fake memakai `FAKE_PRIMARY`/`FAKE_FALLBACK` dengan `VoiceID="FAKE"`, `Model="fake-tts"`; mode real memakai `VoiceRef` karakter apa adanya + `voice_binding_missing` yang tidak dihitung breaker; breaker + semaphore per vendor; timer first-audio dan total per kalimat), fallback hanya sebelum `MarkAudioSent`, sekali per generasi, `tts_provider_switched` + epoch naik + stream epoch lama dibatalkan, `Epoch` pada setiap chunk, ringkasan TTS (provider/model/voice/karakter/first audio), metrik/health/log
### Test
- `generation_tts_test.go` (17 tes) — PASS (race, count=5)

## M4-T17 — `rotatingSTT`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/stt_session.go`: `Router.OpenSTT` (breaker → semaphore tanpa tunggu → `Open` dengan timeout connect), `rotatingSTT` (offset logis, rotation buffer 15 s = 480.000 byte, rotasi ke vendor yang sama untuk `session_time_limit_exceeded`/`insufficient_audio_activity` tanpa dihitung breaker, replay potongan 3.200 byte, `Finalize` ditunda selama replay, FINAL{FromFinalize} koneksi lama dibuang, ERROR lain/penutupan tak terduga diteruskan sebagai `STTError` tanpa pindah vendor, `ReportFinalTimeout` ×3 → `final_timeout`), antrean event (PARTIAL digantikan), metrik rotasi/replay; `Breaker.Record` untuk kegagalan sesi tanpa tiket; `pcmRing.readFromBytes`
### Test
- `stt_session_test.go` (17 tes) — PASS (race, count=5)

## M4-T18 — Router async
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/async.go`: `doAsync` (breaker + semaphore per attempt, retry ke vendor yang sama, `Retry-After` ≤ 30 s, non-retryable/cancelled langsung gagal), `GenerateJSON` (3 attempt, backoff 1 s/4 s, temperatur negatif → 0,2, body non-JSON → `INVALID_RESPONSE`), `Evaluation()` (satu attempt), `CompleteText` (worker 3 / realtime 1 attempt, `CONTENT_FILTERED` non-retryable), `Embed` (3 attempt, 500 ms/2 s), metrik/biaya/health per attempt; asersi `RealtimeRouter`/`AsyncRouter`; `RouterDeps.AsyncWait`
### Test
- `async_test.go` (16 tes) — PASS (race, count=3)

## M4-T19 — `provider/httpx`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/provider/httpx/{client,classify,sse}.go`: `NewClient` (HTTP/2, pool 256/host, idle 90 s, TLS 3 s, tanpa timeout respons), `ClassifyHTTP` (tabel §8.3, `Retry-After` detik atau HTTP-date relatif header `Date`, kuota habis tidak retryable, pesan ≤ 512 byte dan disamarkan), `Redact`, `ClassifyBody`, `ClassifyError`, `ClassifyWSClose`, `SSEReader`
### Test
- `httpx_test.go` — PASS (race)

## M4-T20 — `prompt`: estimator, budget, tipe data
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/prompt/{tokens,budget,learner_context,data,text}.go`: `EstimateTokens` (aritmetika integer `(77·cjk + 20·other)/70` dibulatkan ke atas, bebas drift float), `ItemOverheadTokens`, `Budget`/`DefaultBudget` (total 6.450), `LearnerContext`, `VocabHint`, `PromptData`/`OpeningPromptData` (tanpa style/ekspresi/identitas user), `NormalizeSpeakable` (diekspor di `prompt`, dipakai M5)
### Test
- `tokens_test.go`, `budget_test.go`, `data_test.go` — PASS (race); aturan impor `prompt` (tanpa paket fitur) lulus

## M4-T21 — `prompt.Render`, `Template`, `Params`, FuncMap, registry fixture
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/prompt/{render,funcs,fixtures}.go`: `Template`/`Rendered`/`Render` (`text/template`, `missingkey=error`, struct bertipe; galat menyebut template dan placeholder, bukan nilai data), `Params` (+`TemperatureOr`/`MaxOutputTokensOr`; nilai DB menang), FuncMap tetap `join`/`bullets`/`quote`/`vocab`, registry fixture (`RegisterFixtures` ganda → panic menyebut key)
### Test
- `render_test.go`, `funcs_test.go` — PASS (race)

## M4-T22 — `CoreRules`, `OutputContract`, frasa, template v1, fixture
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/prompt/{core,phrases,templates_v1,fixtures_conversation}.go`: `CoreRules`, `OutputContract(closing)`, `FormalityInstruction`, `ConversationV1`/`OpeningV1` (teks §6.8.3 verbatim tanpa rujukan silang), `FixtureMinimal`/`FixtureMaximal`, fixture CONVERSATION/OPENING terdaftar saat init
### Test
- `templates_test.go`, `phrases_test.go` — PASS (race); render maksimum ≤ 12.000 karakter

## M4-T23 — `SentenceParser`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/prompt/sentence_parser.go`: `NewSentenceParser(maxSentences)`, `Feed`/`Close`, sanitasi (buang `* # ` _`, emoji `So`, ringkas spasi lintas delta; `<`/`>` apa adanya), pemotongan kalimat (terminator + closer, tunggu bila terminator rune terakhir, soft boundary ≥ 20, potong keras 60), kalimat tanpa ucapan ditempel ke sebelumnya, kalimat 1 rune digabung ke berikutnya (kecuali di akhir stream), `Limited` setelah kalimat ke-`MaxSentences` (sisa delta dibuang dari `clean`), hasil identik untuk feed per rune dan sekaligus
### Test
- `sentence_parser_test.go` — PASS (race), cakupan paket 97 %

## Gerbang milestone M4
Status: LULUS
Tanggal: 2026-10-14
- `go build ./... && go vet ./internal/provider/... ./internal/prompt/...` — bersih
- `go test ./internal/provider/... ./internal/prompt/... -race -count=5` — semua `ok`; suite `providertest` (LLM, TTS, STT, Evaluation, Embedding) dijalankan terhadap `fake`
- Cakupan (`covercheck`): `internal/provider` (+ sub-paket, kecuali `providertest`) ≥ 85 %, `internal/prompt` 97 % — `covercheck` kini mendukung `except=` pada baris prefix
- `make arch-check` memuat `TestImportRules` provider/prompt; `make lint` 0 issue; `make test`, `make test-cover`, `migrations-check`, `sqlc-check`, `tidy-check`, `vuln` hijau
- `go test -tags integration ./internal/redisstore -run FakeFaults` — PASS
- Review Focus 1–5 lulus (`ContentFiltered`, `NoResetEventExists`, `NoEpochSwitchAfterAudioSent`, `Truncated`, `DeadlineBeforeAudio`, `BreakerStateMachine`, `TestRotation*`, `VendorErrorForwarded`, `PollerClears`)
- grep D-014/D-015/D-016/D-017: hanya komentar sah dan sakelar `PROVIDER_TTS_FALLBACK_ENABLED`

## M4-Final — Perbaikan hasil review seluruh branch
Status: DONE
Tanggal: 2026-10-14
- STT: ERROR vendor tidak lagi dihitung/diteruskan dua kali dan `Close` normal tidak menghitung kegagalan (flag `ending`); rotasi diset sinkron di `handle` (audio tidak pernah ditolak), Finalize yang belum dijawab diulang di koneksi baru, FINAL koneksi lama dibuang, trigger saat replay memicu rotasi ulang, `AudioMsSent` tidak ganda, titik replay dijepit ke audio yang tertulis, `ReportFinalTimeout` setelah close diabaikan
- TTS: slot yang tak terdaftar tidak lagi menyebabkan nil-pointer; `audioSent` dicek ulang di bawah `g.mu` saat switch; memilih voice + mendaftarkan stream satu critical section; stream yang ditutup/deadline pemanggil tidak memicu switch
- Router: menunggu slot semaphore bukan vonis breaker; `INVALID_RESPONSE` diulang sekali; `httpx` tidak lagi menggaungkan body mentah dan menyamarkan `sk_…`; `ttsCandidates` (kode mati) dihapus

## M3-T1 — Preflight kontrak M1/M2/M4 dan kode error
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- Tes karakterisasi: `internal/platform/errs/m3codes_test.go` (23 kode M3: pesan tidak kosong + status HTTP; 3 kode yang dihapus tidak ada; total 67), `internal/m3preflight_test.go` (kompilasi simbol M1/M2/M4 yang dipakai M3)
### Test
- `go test ./internal/... -run 'TestM3|TestPreflight'` — PASS langsung (kontrak sesuai)

## M3-T2 — `BillableEnd` dan `ComputeCharge`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/balance/charge.go`: rumus §4.5 apa adanya (satu-satunya tempat)
### Test
- `charge_test.go`: 11 contoh §4.5 (billable end + charged) dan batas (59.999/60.000 ms untuk kegagalan, clamp ke reservasi, ending_at ± expires_at, disconnected > aktif) — PASS

## M3-T3 — Reservasi dan operasi `balance.Service` tx-bound
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `balance.Reservation`/`ReservationRepository` (di-embed ke `Repository`), `Service.Reserve` (validasi durasi, `BALANCE_INSUFFICIENT`, deadline +5 menit, RESERVE `reserve:{rid}`), `ReleaseReservation` (RELEASE `release:{rid}`), `SettleReservation` (CONSUME lalu RELEASE; entry nol tidak ditulis); `ErrReservationNotFound`/`ErrReservationState`
- `fakes.BalanceStore`: reservasi + `chk_reservation_state` setara, rollback-copy ikut menyalin reservasi; `repotest.BalanceRepoSuite` + kasus reservasi dan hook `SeedSession`
### Test
- `internal/balance/reservation_test.go` (10 tes) — PASS
### Penyimpangan
- Repo Postgres untuk reservasi baru di Task 5; sementara method menolak (lihat ledger)

## M3-T4 — `balance.Service.Reconcile`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/balance/reconcile.go`: `ReconcileRepository.RunCheck`, `Violation`, `Reconcile` (R1–R6 berurutan, berhenti pada error yang menyebut check); SQL di Postgres (Task 5)
### Test
- `reconcile_test.go` — PASS

## M3-T5 — Repository reservasi dan reconcile di Postgres
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `db/queries/balance.sql`: `LockReservationBySession`, `InsertReservation`, `MarkReservationSettled/Released` (hanya dari `HELD`), `ReconcileR1..R6` (§4.8, satu kolom teks tanpa data pribadi); `postgres.balanceRepo` kini memenuhi `ReservationRepository` (stub T3 dihapus); `postgres.NewReconcileRepo` (`RunCheck`)
- `BalanceRepoSuite`: hook `Unreconciled`; harness Postgres menyediakan `SeedSession` (sesi per user baru)
### Test
- `balance_repo_integration_test.go`: suite kontrak (fake + Postgres), reserve+settle contoh 1 bersih, `chk_ledger_shape`, `chk_reservation_state`, R1/R3/R4/R6 mendeteksi pelanggaran — PASS (Podman)

## M3-T6 — Domain konten, `ScenarioFormality`, `SpeakingRate`, voice/snapshot
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/content/{types,character}.go`: tipe §6.5 (Topic, Scenario, ScenarioVersion, PromptVersion + `Template()`, Character/Voice/TTSBinding, CharacterSnapshot/SessionSnapshot), `ScenarioFormality`, `DifficultyBand`, `DefaultStyle`, `SpeakingRate` (clamp 0,80–1,05), `NormalizeProviderName`, `MarshalJSONB`/`ParseSnapshot` (key tak dikenal ditolak); `content` hanya mengimpor `prompt`
### Test
- `types_test.go` — PASS; `make arch-check` bersih

## M3-T7 — `content.Validator`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/content/validator.go`: `Issue`, `Validator` (`ValidateDraft`/`ValidatePublish`/`ValidateCharacter`/`ValidateCatalog`), aturan scenario/karakter/prompt §6.10.1 (dry-run render semua fixture, JSON Schema 2020-12 via `jsonschema/v6`, batas render 12.000 karakter), `NormalizeCharacter`, `ParseVoice`, `ParseLLMParams`, `Observer`/`NopObserver`, `ReportCatalog`
- `internal/platform/metrics/content.go`: `metrics.Content` (`kotoki_content_resolve_errors_total{code}`, `kotoki_content_voice_fallback_missing_total{character}`, `kotoki_content_validation_issues{severity}`)
### Test
- `validator_test.go` (satu kasus per aturan), `metrics/content_test.go` — PASS

## M3-T8 — Repository konten, cache, `Resolver`, `SessionConfigLoader`
Status: DONE
Tanggal: 2026-10-14
### Implementasi
- `internal/content/{ports,cache,resolver,config_loader}.go`: `Repository` (baca; keluarga "live" ber-TTL dan keluarga by-id LRU 1.000 tanpa TTL), `CachedRepository` (+`Invalidate`), `Resolver` §6.9 (urutan topic → scenario → mismatch → versi → karakter → TTS → prompt → LLM; vendor TTS tidak enabled → `CHARACTER_NOT_AVAILABLE` di kedua mode; mode real: model/voice kosong atau placeholder → `CONTENT_MISCONFIGURED`; fallback hilang → WARN + `VoiceFallbackMissing`; snapshot salinan dalam), `SessionConfigLoader` (tanpa filter status, snapshot dari sesi bukan baris karakter hidup)
- `fakes.ContentRepo` (pencacah panggilan untuk tes cache)
### Test
- `resolver_test.go`, `cache_test.go` — PASS (race)

## M3-T9 — Repository konten di Postgres
- `db/queries/content.sql` + sqlc; `postgres.NewContentRepo` (`content.Repository`) dan `(*Tx).Content()` (`content.TxRepository`, `internal/content/tx.go`): lock parent → versi, `NextVersionNumber`, insert draft, publish/retire/delete, status topik/skenario, lock+update karakter.
- Tes integrasi: published-only, load by id, unik satu PUBLISHED, versi immutable (trigger), nomor versi di bawah lock parent (2 tx paralel → 2 dan 3), round-trip karakter + `voice.fallback`, patch karakter paralel serial, `GetUserVoiceExposure` (hanya COMPLETED, stabil setelah voice karakter diubah).
- Ruling: `ListPublic*` (§6.12) dilewati — katalog publik disusun dari `content.Repository` di Task berikutnya; `GetUserVoiceExposure` dipakai lewat sqlc langsung sampai ada konsumennya.

## M3-T10 — CatalogService & AdminService konten
- `content.Store/Tx/AuditWriter/AuditEntry` (`tx.go`), `CatalogService` (usable-filter = langkah 1–3 resolver; karakter tersembunyi bila vendor mati, atau placeholder/kosong di mode real), `AdminService` (CreateDraft/copy_from, Publish idempoten + rollback RETIRED→PUBLISHED, Retire, DeleteDraft, SetStatus, ListVersions/GetVersion, ValidationReport, GetCharacter, UpdateCharacter) dan `postgres.Store.ContentStore()` (audit ke `admin_audit_logs`). Fake: `fakes.ContentStore` (snapshot/rollback + log audit), `ContentRepo` mengimplementasi `TxRepository`.
- Tes unit (admin_test, catalog_test) + integrasi `test/integration` (`TestPublishConcurrent -count=20`, publish prompt, `TestCharacterEditDoesNotChangeExistingSnapshot`).
- Ruling: retire versi prompt `PUBLISHED` selalu 409 `CONTENT_VERSION_REQUIRED` (spec §6.13.1 baris 4269: hanya publish pengganti yang meretire). Ini berbenturan dengan skenario e2e E12 (retire OPENING lewat endpoint retire) — E12 perlu menyesuaikan (publish pengganti / SQL) saat M-e2e dikerjakan.
- Ruling: `content.Issue` diberi tag JSON (`entity` omitempty) agar `details` 422 memakai bentuk spec; `Entity` tetap tampil di validation-report.
- Ruling: `SetStatus` tidak membatasi transisi antar `ACTIVE/DISABLED/ARCHIVED` (spec tidak mendefinisikannya); status yang sama = no-op tanpa audit/invalidasi.
- Ruling: `AuditEntry.RequestID` diisi lewat `AdminDeps.RequestID` (content tidak boleh mengimpor httpapi); kosong → "unknown".
- Catatan deviations: teks §6.13.1 menulis `metadata`/`CONTENT_CHARACTER_UPDATED`; implementasi memakai kolom `details` dan action `CHARACTER_UPDATED` sesuai CHECK `admin_audit_logs`.

## M3-T11 — Seed SQL & `make seed`
- `seed/0010_topics.sql` (5), `0050_characters.sql` (4, inline voice, placeholder TTS ids), `0060_scenarios.sql` (13 scenario + v1 PUBLISHED), `0070_prompts.sql` (5 `prompt_configs` + CONVERSATION/OPENING v1 dari `prompt.ConversationV1/OpeningV1` byte-per-byte), `seed/env/example/0900_voice_ids.sql`; `make seed` menjalankan juga `seed/env/$KOTOKI_ENV/*.sql` bila ada.
- Tes `test/integration/m3_seed_test.go` (valid, idempoten, jumlah, formality, karakter, placeholder real/fake, override suara, resolve HARUKA×BACKEND, render, konstanta M4, nama lengkap tak pernah dirender). Konstanta `expectedPromptVersionCount = 2` satu-satunya tempat angka itu.
- Ruling: 12 scenario selain `BACKEND_ENGINEER_INTERVIEW` ditulis penulis (spec hanya memberi satu contoh, tabel §6.13.4 memberi kode/level/formality); isi dapat dituning produk lewat versi baru. Dipakai `ValidatorConfig.TTSModeReal` (bukan `ProviderModeReal` seperti tertulis di brief).

## M3-T12 — REST konten (katalog + admin content)
- `content_{routes,handler,dto}.go` (5 GET katalog, `{"items":[…]}`, `Cache-Control: private, max-age=60` hanya pada sukses, limiter `api_user_default`) dan `admin_content_{routes,handler,dto}.go` (versions CRUD/publish/retire, status topics|scenarios, GET/PATCH karakter, validation-report). `Request.PathParam` ditambahkan; `Deps.Content`/`Deps.ContentAdmin` bertipe interface kecil.
- Tes: golden `testdata/api/rest/content/*`, `TestCharacterResponseHasNoVoiceIDs`, error/403/PATCH karakter/lifecycle, `TestRouteTable` (5 baris katalog Ready + 10 baris admin content).
- Ruling: `POST …/versions` memakai body `json.RawMessage` yang di-decode ketat oleh `AdminService` (field per family berbeda); field tak dikenal → 400 `VALIDATION_FAILED`. Objek `voice` PATCH yang cacat (key tak dikenal) → 422 `CONTENT_VALIDATION_FAILED` dengan issue `unknown_key`.
- Catatan: wiring `cmd/api` (Deps.Content/ContentAdmin) ditunda ke Task 23 sesuai plan; sampai saat itu route konten di binary `cmd/api` belum berfungsi.

## M3-T13 — Domain `session`
- `internal/session/{session,tx,transition,ports}.go`: `Status/EndReason/Actor/StartMode/Fence`, `CanTransition` (tabel privat, matriks 8×8 teruji), `TerminalStatusFor`, error domain, `Session`/`RuntimeRecord`/`Settlement`/`Event`/`Turn*`, `Patch`, `Repository`/`TurnRepository`/`EventRepository`, `Store/Tx`, port `UserGate/AdaptiveProvider/OpeningPromptSource/ControlPublisher/HeartbeatReader`.
- Ruling: `TurnRepository` tidak memuat `SetUserAudioKey` (signature §7.13.1 menyebutnya, tetapi §5.3.2/D-099: audio user tidak pernah disimpan dan `session_turns` tidak punya kolom referensi audio). Biaya bila salah: satu method tambahan di M5.
- Ruling: `internal/events/outbox.go` (`Event`, `OutboxWriter`) dimajukan dari Task 14 karena `session.Tx.Outbox()` membutuhkannya; Task 14 menambah `types.go`, builder dan writer Postgres.
- Ruling: bentuk field `TurnStart/TurnUser/TurnFinal/TurnRecord/Patch/ListFilter/ActiveRef` ditetapkan dari kolom DDL §5.3.2 dan transisi §5.4–5.6 (spec hanya memberi nama tipe); `CreatedAt` turn ikut dibawa untuk partition pruning.

## M3-T14 — Paket `events` + outbox writer
- `internal/events/types.go` (`SessionCompletedPayload` v1 dengan timestamp milidetik UTC, `NewSessionCompleted`, `ActiveSeconds`), `postgres.Tx.Outbox()` (INSERT `event_outbox`, traceparent/tracestate dari propagator ctx), `db/queries/outbox.sql`, `fakes.Outbox`.
- Tes: golden payload, floor active_seconds, 16 key; integrasi `TestOutboxWriterInsertsPendingRow`, `TestOutboxWriterRollsBackWithTx`.

## M3-T15 — Repository session/turn/event di Postgres
- `db/queries/{sessions,session_turns,session_events}.sql`; `postgres.NewSessionRepo/NewTurnRepo/NewEventRepo` dan `(*Tx).Sessions()/Turns()/Events()`; `Store.SessionStore()`. Tulis runtime ber-fence dengan pola `INSERT … SELECT … WHERE EXISTS` / `UPDATE … AND EXISTS` (0 baris → `ErrFenceLost`); `FailInProgress` memeriksa fence terpisah bila tidak ada turn yang diubah; 23505 diterjemahkan ke `ErrAlreadyOpen`/`ErrDuplicateIdempotency` lewat nama index.
- `repotest.RunSessionRepoSuite` (kontrak yang nanti juga dijalankan terhadap fake session store) + tes integrasi: CHECK, partisi, tanpa FK ke tabel partisi, pruning, snapshot immutable (`TestConfigSnapshotImmutable` mem-parse sqlc), kandidat abandon.
- Ruling: port sweeper memakai cutoff yang dihitung pemanggil (`ListExpired(expiredBefore)`, `ListActiveForHeartbeat(startedBefore, limit)`), bukan `now`; `MaxSeqAndGeneration` mengembalikan `(-1, 0)` bila belum ada turn (AI_FIRST mulai dari 0, USER_FIRST dari 1); `Stats.hasAudio` = ada turn dengan `stt_audio_ms` atau `tts_first_audio_ms` (D-099: tak ada referensi audio tersimpan); `ListForUser` mengecualikan turn `DISCARDED`; `ListRecent` mengembalikan semua status, urut naik — penyaringan milik konsumen (M5).

## M3-T16 — `session.Service.Create`
- `internal/session/{service,create}.go`: `Deps`, `Metrics`/`NopMetrics`, `NewService`, `CreateSessionInput/Result`, `HashCreateRequest`, `Create` mengikuti §4.6.1 (gate → resolve → adaptive → hash di luar tx; di tx: lock account → replay/reuse → satu sesi terbuka → Insert → `Balance.Reserve` → `SESSION_RESERVED`); `ErrAlreadyOpen/ErrDuplicateIdempotency` dari backstop → rollback lalu keputusan diulang lewat `Reader`. `metrics.NewSession` (`kotoki_sessions_created_total`, `kotoki_sessions_ended_total`).
- Fakes: `fakes.SessionStore` (mengikuti semantik repo Postgres, lulus `RunSessionRepoSuite`; satu transaksi bersama balance dan outbox; hook `BeforeInsert` + `PutSession` mensimulasikan baris yang di-commit pihak lain), `fakes.UserGate`, `fakes.Adaptive`.
- Tes unit (create_test) + integrasi `TestCreate20ParallelDifferentKeys/SameKey`, `TestCreateUniqueViolationBackstop` (+`AssertReconciled`).
- Ruling: metrik `created` menghitung `error` untuk 5xx (INTERNAL_ERROR, SERVICE_UNAVAILABLE, CONTENT_MISCONFIGURED, error non-errs); replay dan 4xx lain tidak dihitung.
- Ruling: separuh kedua `TestCreate_20Parallel_DifferentKeys` ("setelah Cancel lagi 20 → tepat 1") ditunda ke Task 17 karena `Cancel` baru ada di sana.

## M3-T17 — Jalur release & start
- `internal/session/{lifecycle,control}.go`: `releaseInTx` (spec 4.6.2: pra-tx `OwnerOf` → lock account → `GetForUpdate` → lock reservasi → `ReleaseReservation` → CAS dengan `BumpFence` → event), dipakai `Cancel`, `Abandon`, `FailStart`, `CancelInTx`; `BeginStart` (lock sesi → reservasi, OPENING dibaca sebelum tx; terlambat → `Abandon` inline lalu `ErrStartDeadlinePassed`), `Activate` (>10 s → `FailStart(PROVIDER_FAILURE,"start_timeout")` + `ErrStartTimeout`). `TransitionError` (membungkus `ErrInvalidTransition` dengan From/To); control `status_changed` dan metrik `Ended` hanya setelah commit transisi yang benar-benar mengubah sesuatu.
- Postgres: stub `SessionGate.CancelReserved` diganti `CancelInTx` lewat `Store.SetSessionCanceller(svc)`; tes M2 `TestPGSessionGateCancelReservedNotWired` dibalik menjadi `TestPGSessionGateCancelReserved*` dan `TestPGDeleteAccountCancelsReservedSession`. `var _ session.Tx = (*Tx)(nil)`.
- Fakes: `fakes.ControlPublisher`, `fakes.OpeningSource`. Tes integrasi: `TestCancelVsBeginStartRace` (50×, jitter, kedua urutan terjadi), `TestCancelInTxUsedByAdminTx`, babak kedua `TestCreate20ParallelDifferentKeys` setelah Cancel.
- Ruling: `Abandon` sebelum deadline mengembalikan sesi apa adanya tanpa error (job melewati); `CancelInTx` tidak mem-publish control (commit milik pemanggil) tetapi menghitung metrik; `CancelReserved(…, at)` memakai jam service, argumen `at` diabaikan; `cmd/api/main.go` perlu memanggil `store.SetSessionCanceller(svc)` — dikerjakan di Task 23 (wiring).

## M3-T18 — Transisi runtime & settlement
- `internal/session/runtime_ops.go`: `ClaimRuntime`, `AddDisconnected`, `MarkHandoff`, `SetSummary`, `BeginEnding` (ending_at = COALESCE(handoff_at, now), alasan pertama menang, non-runtime menaikkan fence, control `ENDING` setelah commit), `Finalize` dan `FailActive` lewat `settleTx/settleInTx` (§4.6.3: lock account → sesi (terminal → idempoten sebelum cek fence) → `SettleReservation` → `FailDangling` → CAS `BumpFence` → event → outbox `session.completed` dengan producer REALTIME/WORKER; control + metrik `Ended` hanya sekali).
- Tes unit (`lifecycle_end_test`: contoh 1, 9, 11, handoff, fence, idempotensi, payload outbox, metrik) dan integrasi (`TestFinalizeRuntimeVsSweeperRace` 100×, `TestHandoffThenSweeperRuleEExample10`, `TestTakeoverOldFenceCannotWriteTurn`, count=3).
- Ruling: `FailActive` memotong `ended_at` ke [started_at, expires_at]; `SetSummary` tidak menimpa ringkasan yang sudah ada (no-op) dan menolak sesi yang tidak pernah ACTIVE; `whyNoRow` memeriksa status lebih dulu lalu fence (urutan error spec: `ErrInvalidTransition` sebelum `ErrFenceLost`).

## M3-T19 — `session.RuntimeService`
- `internal/session/runtime.go`: fasad ber-fence dengan signature §7.13.1 (`GetForAttach/ClaimRuntime/MarkStarting/Activate/FailStart/AddDisconnected/MarkHandoff/BeginEnding/Complete/Fail`) di atas `Service`; `Settlement` dari baris sesi (waktu nol bila sesi tak pernah mulai). `RuntimeRecord` mendapat `CharacterID`; `session.RuntimeRecordOf(Session)` dipakai postgres dan fake.
- Tes: assertion kompilasi terhadap salinan `conversation.SessionRuntime`, opening id, snapshot dapat di-`ParseSnapshot`, settlement, alasan `Fail` berbeda → alasan tersimpan + WARN, fence basi.

## M3-T20 — `session.QueryService`
- `internal/session/query.go` (`View`, `ViewRepository`, `QueryService.Get/List/Turns`), `postgres.NewSessionViewRepo` + `db/queries/session_views.sql` (join topics/scenarios, LEFT JOIN reservasi; nama/avatar karakter dari `config_snapshot`), `fakes.SessionViews`, `SessionStore.TurnReader()`.
- `Turns`: 404 untuk sesi orang lain; 410 `TRANSCRIPT_EXPIRED` bila `transcript_purged_at` atau `ended_at` > retensi (180 hari); sesi tanpa `starting_at` → daftar kosong tanpa query; hanya turn `COMPLETED/INTERRUPTED/FAILED` (SQL `status IN (…)`, jadi paginasi tetap benar).
- Ruling: `NewQueryService` menerima `TurnRepository` tambahan (brief tidak menyebutnya tetapi `Turns` membutuhkannya); batas pruning (`starting_at − 1 menit`, `ended_at + 1 menit`) diterapkan di repo Postgres, jadi tes unit menegaskan bahwa service meneruskan `starting_at`/`ended_at` apa adanya.

## M3-T21 — Sweeper & adapter Redis
- `internal/session/{sweeper,guard}.go` (`Sweeper.Tick` urutan e,a,b,c,d; `HeartbeatGuard` 45 s sehat; skip massal ≥20 kandidat dan ≥50% dari ACTIVE non-handoff), `Service.EndStaleHandoff`, `redisstore.NewHeartbeatReader` (MGET, `Read(nil)` = PING) dan `NewSessionControl` (PUBLISH), `fakes.Heartbeats`. `ListActiveForHeartbeat(ctx, limit)` mengembalikan `id, started_at, expires_at`.
- Tes unit: tiap aturan dengan batas waktu (14/16 s, 59/61 s, 29/31 s), contoh 7, 9, 10, urutan aturan, guard, skip massal, handoff tak dihitung, 19 dari 20, Redis mati, error satu sesi, kalah dari owner; integrasi Redis (`TestHeartbeatReaderMGet`, `TestSessionControlPublish`) dan Postgres+Redis (`TestSweeperRuleBWithRealPostgresAndRedis`, Redis mati).

## M3-T22 — REST sesi
- `internal/httpapi/session_{routes,handler,dto}.go`: `POST|GET /sessions`, `GET /sessions/{id}`, `POST …/cancel`, `POST …/realtime-ticket`, `GET …/turns` dengan rate policy per route (`session_create_user`, `ticket_user`, `api_user_default`); `Deps.Session/SessionQuery/Tickets` (interface kecil); `Request.PathUUIDOr` (id sesi tak valid → `SESSION_NOT_FOUND`). `createSessionRequest` memakai string id agar id rusak menjadi field error (`VALIDATION_FAILED`), field asing (`version_id`, `conversation_style_id`, …) ditolak `unknown_field`. Replay → 200 + `Idempotent-Replayed`, baru → 201 + `Location`. `remaining_seconds` hanya untuk `ACTIVE` (`ceil`, min 0); nama topik/skenario dari induk, karakter dari snapshot; tidak ada `adaptive_difficulty/runtime_fence/config_snapshot`.
- Tes: golden `testdata/api/rest/sessions/*` (create, get, list, turns), error/404/409/410, paginasi, tiket (`auth.TicketService` dengan fake reader), `TestRouteTable` (6 baris sesi Ready) dan `TestRouteTableUserGroupPrefixes` (11 route di prefix sesi + katalog, tanpa `/conversation-styles`/`/expression-profiles`).
- Ruling: cursor turns = `sequence_number`; limit turns default 50 (maks 100) berbeda dari default list umum (20).

## M3-T23 — Lifecycle lintas paket, wiring, rekonsiliasi akhir
- `internal/session/adaptive_stub.go` (`NeutralAdaptiveProvider`, snapshot netral; hanya `formality` dari snapshot), `cmd/api/adapters.go` (`userGate` di atas `auth.UserService.RequireActive`, `openingPromptSource`, `resolverConfig/validatorConfig` dari variabel `PROVIDER_*` yang sama dengan router — `provider.RouterConfig` di-parse dengan `env`, tanpa validasi kunci), wiring `cmd/api/main.go` (cache konten 30 s, `ReportCatalog` saat start, catalog/admin, session service, `SetSessionCanceller`, query service, ticket service, `Deps.Content/ContentAdmin/Session/SessionQuery/Tickets`), config `CONTENT_CACHE_TTL_SECONDS` (30) dan `RETENTION_TRANSCRIPT_DAYS` (180) + `.env.example`.
- `test/integration/m3_lifecycle_test.go` (`TestLifecycle`): happy path AI_FIRST (421 s), karakter diubah/di-DISABLE saat sesi aktif, fallback OPENING, kegagalan provider (+STARTING tanpa outbox), double-click lewat HTTP, pod mati → sweeper (b) sekali, reservasi terbengkalai, akses transcript (410 setelah 181 hari), lalu `AssertReconciled` + `balance.Reconcile` R1–R6 kosong dan tidak ada reservasi yatim.
- `go test -race -tags=integration -p 1 ./...` hijau, `golangci-lint` bersih.
- Ruling: `userGate` memakai `RequireActive` saja (mengembalikan `User.TermsVersion`), tidak memanggil `Me` terpisah; `NewAdminQueryService` dan wiring `RuntimeService/Sweeper` bukan bagian api (T24, M5, M6).

## M3-T24 — Endpoint admin sesi + audit `SESSION_TRANSCRIPT_VIEW`
- `internal/session/admin_query.go` (`AdminView`, `AdminRepository`, `AdminQueryService.ListByUser/Get/Turns/Events`, `RedactSnapshot`), `postgres.NewSessionAdminRepo` + `db/queries/session_admin.sql` (JOIN `balance_reservations`, semua turn termasuk `DISCARDED`/berjalan, events keyset `(created_at,id)` tanpa subquery by id), `fakes.SessionAdmin`.
- `admin.Service.RecordTranscriptView` (action `SESSION_TRANSCRIPT_VIEW`, target `SESSION`, `target_user_id` = pemilik); `internal/httpapi/admin_session_{routes,handler,dto}.go` dengan 4 route di grup Admin (`admin_user`), `Deps.AdminSessions/AdminAudit`, wiring `cmd/api/main.go`.
- Tes: unit `admin_query_test` (redaksi, pemilik bebas, turn DISCARDED/berjalan, 410, cursor events, filter status + keyset), `admin/service_audit_test`, handler `admin_session_handler_test` + golden `testdata/api/rest/admin_sessions/*` (tepat satu audit untuk turns, 404/410 tanpa audit, audit gagal → 500 tanpa transcript), `TestRouteTable` (4 baris Ready) + `TestRouteTable_AdminSessions`, integrasi `TestAdminSessions` (reservasi `SETTLED`, `charged_seconds == consumed_seconds == 421`, audit di DB, events keyset, 410).
- Catatan proses: kode `admin_query.go` dan repo Postgres pertama kali ditulis sebelum tesnya (sesi terputus); `admin_query.go` ditulis ulang dari tes, sedangkan repo Postgres dipulihkan dari versi awal lalu dibuktikan oleh tes integrasi.

## M5-T1 — Preflight kontrak, kode error WS, `RealtimeTuning`
- `internal/m5preflight_test.go` (`TestM5Preflight` kompilasi-saja: `*session.RuntimeService` memenuhi sepuluh method §7.13.1, `Event.TurnID *uuid.UUID`, `provider.RealtimeRouter`, `prompt.Render`, `content.SessionConfigLoader`, `auth.TicketStore`, `ratelimit.Limiter`; `TestWSCodesExist`). Semua kode WS dan pesannya sudah ada di `errs` (M1), tidak ada kode/pesan yang ditambah.
- `config.RealtimeTuning` (`realtime_tuning.go`) tertanam anonim di `config.Realtime`; default diambil dari tag `envDefault` (satu sumber), `PodID` (`REALTIME_POD_ID`) default hostname karena belum ada di `Realtime` M1; `Validate()` dipanggil dari `Realtime.Validate()`. `PROVIDER_SESSION_GRACE`/`PROVIDER_TIMEOUT_STT_FINAL` tetap milik `provider.RouterConfig`. Env baru ditambahkan ke `.env.example`.
- Deviasi kecil: `httpapi.StatusOf` memetakan `SERVER_DRAINING` dan `SERVER_AT_CAPACITY` ke 503 (sebelumnya 500 sebagai "WS-only"), karena gateway M5 menolak upgrade dengan HTTP 503 (§7.3.1 langkah 3). `TestWriteErrorUnknownAndWSOnlyCodes` disesuaikan (17 → 15 kode WS-only murni).
- `postgres.NewTurnRepo`/`NewEventRepo` sudah ada sejak M3 (non-tx); hanya ditambah tes integrasi `TestTurnRepoConstructors` (fence salah → `ErrFenceLost`). `session.TurnStart/TurnUser/TurnFinal` sudah memuat `SpokenText`/`TTSVoiceID`/`ErrorCode`, tidak ada perubahan.

## M5-T2 — Port learner/memory minimal, `rtstate`, port `conversation`, fake, helper leak
- `internal/learner/difficulty.go` (`AdaptiveDifficulty` dengan tag json §12.8.2, `Skill/Formality/DialectExposure`, `TTSSpeakingRate`, `MaxAISentences`; kecepatan di luar 1..5 di-clamp), `internal/memory/port.go` (§11.7.1 persis), `internal/conversation/rtstate/rtstate.go` (`Store` 17 method, `RT/TurnSnap/EventEntry/Summary/Takeover/PodInfo`; field epoch dalam ms), `internal/conversation/ports.go` (`SessionRuntime` 10 method, `TurnRepository`, `SessionEventWriter` dengan `turnID`, `RuntimeStore = rtstate.Store`, `ConfigLoader/ConfigSnapshot`, `LearnerContextProvider` dengan `Rerender`, alias `TurnStart/TurnUser/TurnFinal/TurnRecord`).
- Fake di `internal/testutil/fakes`: `RuntimeStore` (owner Lua §7.9.2, stream dipangkas 500, pub/sub per pod), `SessionRuntime` (`session.CanTransition`, fence naik di `ClaimRuntime`, `ErrFenceLost`, `Complete` idempotent, settlement `min(duration, ceil(endingAt-startedAt) - disconnected)` dengan fake clock), `TurnRepo`, `EventWriter`, `MemoryRetrieval`, `LearnerContext`, `AudioStore`; semuanya aman konkuren dengan hook `FailNext`.
- `repotest.RuntimeStoreSuite` (dipakai ulang Task 16 untuk Redis), `testutil.NoGoroutineLeak(t, pkgPrefix)`.
- Tes: `TestSpeedTables`, `TestAdaptiveDifficultyJSONTags`, `TestRuntimeStoreContract`, `TestFakeSessionRuntime`, `TestFakeTurnRepoAndEvents`, `TestFakeMemoryRetrieval`, `TestNoGoroutineLeakPasses`; lint bersih.

## M5-T3 — Protokol WS v1: envelope, payload, registry, golden
- `internal/conversation/protocol/{envelope,client,server,registry,codec}.go`: `ServerEvent`/`ClientEvent`, payload per §7.4.4/§7.4.5 (tanpa `expression`, D-014), `AudioParams.Valid()` (opus/48000/1/20; decode tidak menolak, pemanggil yang memberi `UNSUPPORTED_AUDIO_FORMAT`), `Timestamp` (RFC 3339 UTC dengan tepat 3 digit ms, juga untuk `ts` envelope), registry `map[string]reflect.Type` untuk client (6) dan server (21), `IsDurable` (12 tipe §7.4.3), `MarshalServer/MarshalClient/DecodeServerEvent/DecodeClientEvent`. `turn_id/turn_seq/generation` dihilangkan bila nil, `replay` hanya bila true. Paket cgo-free dan tidak mengimpor `conversation/audio`.
- Golden `testdata/api/ws/{client,server}/*.json` (27 file, indent 2, UUID/waktu tetap) dibuat dengan `go test ./internal/conversation/protocol -update`.
- Tes: `TestGoldenServer/TestGoldenClient` (marshal JSONEq, decode→re-marshal, kelengkapan dua arah), `TestDurableSet`, `TestEnvelopeOmitsEmpty`, `TestDecodeClient`.
- Keputusan: field tak dikenal diabaikan (§7.4.2 menang atas §14.4), lihat deviations.

## M5-T4 — Protokol: frame biner, katalog error, close code
- `internal/conversation/protocol/binary.go`: `EncodeUpstream/DecodeUpstream/EncodeDownstream/DecodeDownstream` (big-endian), `MaxBinary=4096`, `ErrFrameTooShort/ErrFrameTooLarge/ErrBadType` (pemanggil memetakan semuanya ke `PROTOCOL_VIOLATION` 1008). Payload kosong dianggap terlalu pendek (1 ≤ N).
- `errors.go`: `Spec{Fatal, Close, Retryable}`, `Catalog` (tabel §7.4.8), `CloseCode`, `NewError(code, requestID, ref, details, ...ErrorOption)` + `WithRetryable`. `request_id` = `ref` bila ada; pesan dari `errs.Messages`. `SESSION_STATE_CONFLICT.Retryable=false` di katalog, pemanggil menimpa `WithRetryable(true)` hanya untuk status `STARTING`.
- `Catalog[code].Fatal` adalah penentu durable-nya event `error` (durable hanya bila non-fatal, §7.4.3); `IsDurable("error")` tetap false.
- Golden `testdata/api/ws/frames/{upstream_audio,downstream_audio}.hex`. Tes: `TestBinaryFrameGolden`, `TestUpstreamValidation`, `TestDownstreamValidation`, `TestCatalog`, `TestNewErrorFormat`; lint bersih.

## M5-T5 — Audio: Opus, framer, PCM, WAV (cgo)
- `internal/conversation/audio/{opus,framer,pcm,wav}.go` — satu-satunya paket cgo M5 (`gopkg.in/hraban/opus.v2`, libopus 1.6.1). `Decoder` 16 kHz mono (`Decode` hanya valid 320 sampel selain itu `ErrBadFrame`, `DecodePLC` 320 sampel), `Encoder` (`AppVoIP`, DTX/FEC off, panjang blok `sampleRate/50` atau `ErrBlockSize`), `Framer` (menahan byte ganjil, `Flush` pad nol), `Int16ToBytes/BytesToInt16/Decimate48to16/Resample`, `ReadWAV` (hanya PCM16 mono), `OutFrame`.
- Tes: `TestEncodeDecodeRoundTrip`, `TestDecodeInvalid`, `TestPLC`, `TestEncoderBlockSizeError`, `TestFramerCarriesOddByte`, `TestFramerFlushPads`, `TestDecimate`, `TestResampleAndBytes`, `TestReadWAV`; `-race` hijau, `make lint` dan `make arch-check` hijau (aturan depguard/arch untuk paket cgo sudah ada, tidak diubah).

## M5-T6 — Audio: VAD dan aset APOLOGY
- `audio/vad.go`: `NewVAD(mode)` (mode di luar 0..3 ditolak) membungkus `go-webrtcvad` (cgo); `Process(pcm []int16) bool` menerima tepat 320 sampel (20 ms @ 16 kHz, dikonversi ke 640 byte LE), panjang lain atau error → `false`. Tidak thread-safe (buffer internal dipakai ulang).
- `audio/apology.go`: `ApologyText` dan `ApologyFrames()` mem-parse embed `assets/apology_ja.opus` (`[uint16 BE len][packet]`); aset rusak → `ErrBadApologyAsset`.
- `test/tools/genapology`: sintesis nada naik-turun 2,4 s (220–330 Hz per suku kata, 24 kHz) → `Framer` → `Encoder(24000,32000,5)`; `make assets-apology` menjalankannya, file hasil (120 paket, ±9 KB) di-commit. Hanya mode fake; `-tts` menyusul di M9.
- Tes: `TestVADSilenceAndTone`, `TestVADRejectsBadMode`, `TestApologyFrames` (paket 24 kHz didekode `Decoder` 16 kHz Task 5 → 320 sampel); `-race`, `make arch-check`, `make lint` hijau. Aturan depguard tidak berubah (`**/test/**` sudah dikecualikan dari `audio-only-realtime-graph`).

## M5-T7 — Fixture audio Jepang (sintetis) + validator
- `test/tools/genaudio` (deterministik, seed tetap `"kotoki"` + FNV nama fixture, PCG `math/rand/v2`): sumber harmonik F0 120 Hz (kontur ±4 % @0,7 Hz, `ja_question` naik +25 % di 400 ms terakhir) sampai 3,8 kHz, kemiringan −6 dB/oktaf, 3 resonansi formant orde-2 per suku kata (vokal a/i/u/e/o pria Jepang dipilih acak), modulasi amplitudo 4 Hz (`0,35 + 0,65·sin²`), RMS per suku kata −20 dBFS (hasil ≈ −23 dBFS, puncak ≤ 10 929, tanpa clipping), fade 15 ms; jeda = nol digital; `noise_only` = derau merah (integrator bocor 0,995, tanpa DC) −45 dBFS RMS; `silence_5s` = nol. `make fixtures` menulis `testdata/audio/synth/` (10 WAV PCM16 mono 48 kHz header kanonik 44 byte, `.txt` satu baris, `.partials.json` untuk 8 fixture ucapan, `manifest.json` dengan `name/duration_ms/tags/expect{turns}/segments`), total 98,5 s = 9 476 367 byte; regenerasi idempoten (sha256 sama). `-to16k <dir> -src <dir>` (`make fixtures16k` → `out/fixtures16k`, 20 file) menulis salinan `Decimate48to16` (WAV kanonik 16 kHz, terbaca `provider.DecodeWAV`) + `.txt`.
- `internal/testutil/audiofx`: `FixtureManifest`, `LoadFixture` (mencari `testdata/audio/` dulu lalu `synth/`), `Path`, `PartialsPath/LoadPartials`, `SynthDir`, tipe `Entry/Segment/Expect/Partial` (`Entry.InSegment/SegmentCovers`).
- Transcript/partial mengikuti peran §14.5 untuk Task 9: `ja_filler_pause` teks selama jeda = `えっと`, `ja_conjunction_pause` = `昨日は雨だったけど`, `ja_question` diakhiri final `趣味は何ですか？`, `ja_greeting_short` tanpa final (ambang default 800), `ja_barge_in` ≥ 2 rune pada onset+500 ms; `ja_long_utterance`/`ja_max_utterance` partial per klausa.
- `TestFixturesVADSanity` (VAD asli mode 2 pada `Decimate48to16`) lulus tanpa tuning: frame voiced pada segmen bicara 99,2–100 %, `noise_only` 1,6 %, `silence_5s` 0 %, `ja_filler_pause` tepat 2 run. Ini sekaligus kasus true-positive wrapper VAD Task 6 (wrapper benar). Probe sementara (tidak di-commit) pada fake STT mode fixture: skor silang antar fixture ≤ 0,67 (< ambang 0,8), jadi fixture saling terbedakan.
- Lint/arch: pengecualian depguard aturan 5 `!**/internal/testutil/audiofx/**`; tabel `test/arch` (`audioAllowed` + kasus `audiofx` boleh, root `internal/testutil` dan `testutil/fakes` tetap dilarang); `TestMakeTargetsExist` + `fixtures`, `fixtures16k`; `.env.example` `FAKE_STT_FIXTURE_DIR=out/fixtures16k` (belum ada wiring produksi yang membacanya). Tes: `TestAudioFixtures`, `TestFixturePartialRoles`, `TestSynthDirSize`, `TestFixturesVADSanity`, `TestLoadFixtureUnknown`, `TestFixtureTableTotals`, `TestRenderDeterministic`, `TestRenderMatchesCommitted` (±1 LSB untuk perbedaan FMA lintas CPU), `TestWriteAndTo16k`.

## M5-T8 — Detector turn + aturan teks Jepang (murni)
- `internal/conversation/detector.go`: `TurnState` (string, `StateListening/StateUserSpeaking/StateProcessing/StateAISpeaking` = nilai spec), `EventKind` (`EventSpeechConfirmed` … `EventBargeInConfirmed`, nilai string = nama spec `SPEECH_CONFIRMED` dst.), `Event{Kind, OnsetMs, ThresholdMs}`, `TextView` (4 method), `DetectorConfig` + `DetectorConfigFrom(config.RealtimeTuning)` (10/15 dari `VAD_SPEECH_START_MS/_WINDOW_MS`, 15/20 dari `BARGEIN_VAD_MS/_WINDOW_MS`, jendela tidak pernah lebih pendek dari hitungannya; suffix disalin), `Detector` (`Push`, `Threshold`, `InjectSilence(ms, st, tx)`, `ResetForNewTurn`, `NowMs`) dan `BoolRing` (`Push/Count/SpanFromFirstTrue`). Algoritma §7.6.2 disalin apa adanya: cluster 3/5 (lastVoiced), urutan cabang USER_SPEAKING resume → max utterance → pause → silence, onset kandidat barge-in `now − 60`, batal pada silence ≥ 300 ms, barge-in butuh `win20 ≥ 15` dan `RuneLen(NormalizeSpeakable(TextSince(onset))) ≥ 2`. Tanpa goroutine/clock (waktu = frame × 20); paket tetap cgo-free.
- `internal/conversation/japanese.go`: `NormalizeTail` (whitespace termasuk `　` dan `、,…ー〜～` di akhir), `HasSuffixAny` (literal setelah `NormalizeTail`, sufiks kosong tidak pernah cocok), `IsFillerOnly` (token filler panjang §7.6.2 dihapus satu lintasan, lalu tanda baca; teks hanya tanda baca = filler-only), `RuneLen`, `isDiscard` (memakai `prompt.NormalizeSpeakable`), `HasJapanese` (Hiragana/Katakana/Han).
- Konvensi onset: `OnsetMs` = waktu **awal** frame voiced pertama (frame ke-i mencakup `[(i−1)·20, i·20)`), sama dengan `now − 60` barge-in; `SpanFromFirstTrue` = jumlah slot dari true tertua sampai terbaru (inklusif) × 20.
- Tes: `TestDetectorConfigFrom`, `TestSpeechStart`, `TestSilenceThresholds` (tabel brief + `SILENCE_REACHED` tepat di frame `k+2+N`), `TestMaxUtterance` (event pertama tepat di `onset+45000`), `TestPauseResume`, `TestBargeInRules` (15/20 + `あの`, `あ`, tanda baca, 14/20, gema `TextSince=""` 40 frame, pembatalan 300 ms dan onset kandidat baru), `TestInjectSilence`, `TestResetForNewTurn`, `TestBoolRing`, tabel `japanese_test.go`; `-race`, `go build ./...`, `make arch-check`, `make lint` hijau.
- Perbaikan review (fix 1): `IsFillerOnly` kini harfiah §7.6.2 (`"。"`/`"？"`/`"「」"` → ambang 2000, `"あのー、えっと"` → 1400); tes ditambah untuk teks hanya tanda baca, urutan token-lalu-tanda-baca, VAD voiced tersebar < 10/15, `HasJapanese("ｶ")`/`("々")`; loop tes kini berbatas frame.

## M5-T9 — `sttState`, `ScriptedSTT`, component test detector dengan VAD asli
- `internal/conversation/stt_state.go`: `sttState` (implementasi `TextView`) mengikuti blok §7.6.2: `segments []Segment{Text, Final, RecvAudioMs}` (partial selalu segmen terakhir dan menimpa partial sebelumnya; FINAL menggantikan partial berjalan), FIFO `pending` (`Commit(CommitTurnEnd|CommitDiscard)`, satu entri per `stt.Finalize`), `commitIdx`, `sttMark`. `OnFinal` mengembalikan `FinalOutcome` (`FinalSegment`, `FinalTurnEnd` = STT_COMMITTED_FINAL, `FinalTurnEndLate` = TURN_END yang sudah timeout → dibuang, `FinalDiscarded`); setiap FINAL commit membuang partial berjalan (audio sebelum commit) dan memajukan `commitIdx`; turn yang dimulai selagi commit DISCARD pending memakai `sttMark = commitIdx` baru. `ExpireTurnEnd()` untuk STT_FINAL_TIMEOUT, `PendingCommits()`, `MarkTurn(onsetMs)`, `TurnText`, `TextSince` ("" selama ada DISCARD pending, S-8), `FinalCoversLatest`, `CommitIdx`, `TurnOnsetMs`. Waktu = waktu audio (`newSTTState(audioNow)`, runtime memberi `det.NowMs`).
- `internal/testutil/replay` (cgo-free): `Step{AtMs, Text, Final}`, `Timeline`, `LoadTimeline(path)` (format `<fixture>.partials.json`, `at_ms` tidak boleh mundur); `ScriptedSTT` memenuhi `provider.RoutedSTTSession`: waktu = byte audio kumulatif / 32 (640 B = 20 ms), step yang tercapai dikirim sinkron sebelum `SendAudio` kembali (PARTIAL, atau FINAL untuk step final), `Finalize` → tepat satu `FINAL{FromFinalize:true}` (teks = `SetFinalizeText` sekali pakai atau partial berjalan), `HoldFinalize(bool)` menahan lalu melepas berurutan, `Barrier()` + `BarrierID(ev)` (sentinel bertipe `EventBarrier="BARRIER"`), `FinalizeCalls`, `FinalTimeoutReports`, `AudioMs`, `Close` idempoten (CLOSED lalu channel ditutup). Event lewat channel ber-buffer (1024, `WithEventBuffer`) tanpa goroutine; overflow = panic yang jelas.
- `detector_component_test.go` (`TestDetectorFixtures`): fixture 48 kHz → `Decimate48to16` → VAD asli mode 2 per frame 20 ms + tail silence 3000 ms; STT dari `.partials.json` lewat `ScriptedSTT`; state machine mini baris 1/3/4/5/6/9 (bertindak pada event pertama tiap jenis). Hasil: `ja_filler_pause` onset 200, ambang 2000 sepanjang jeda, `SILENCE_REACHED` 4440 ms (500); `ja_conjunction_pause` ambang 1400 sepanjang jeda 1,1 s, satu turn; `ja_question` 500; `ja_greeting_short` 800; `ja_max_utterance` satu segmen > 45 s, `MAX_UTTERANCE` tepat 45 000 ms sejak onset, tanpa `SILENCE_REACHED`; `noise_only`/`silence_5s` nol event; potongan 100 ms di dalam ujaran → nol event, potongan 150 ms → `SPEECH_CONFIRMED` (overhang VAD, lihat deviasi). Mutasi (filler-only, ambang 1400, cabang 500, max utterance, ambang start) masing-masing membuat subtes terkait gagal.
- Tes unit: `TestTurnTextJoinsFinalPlusPartial`, `TestMarkTurnKeepsRunningPartial`, `TestCommitFIFO`, `TestCommitDiscardMovesMarkOfTurnStartedWhilePending`, `TestTextSinceEmptyWhilePendingDiscard`, `TestFinalCoversLatestAcrossCommits`, `TestTimedOutTurnEndCommitDropped`, `TestOrphanCommitFinalIsSegment`, `TestScriptedSTTDeterministic`, `TestScriptedSTTFinalizeTextHoldAndBarrier`, `TestScriptedSTTClose`, `TestLoadTimeline`; `-race`, `go build ./...`, `make arch-check`, `make lint` hijau. Tidak ada perubahan aturan arsitektur (`internal/conversation` sudah boleh mengimpor `conversation/audio`; `replay` tidak mengimpornya).
- Perbaikan review (fix 1): identitas per commit di `sttState` — `Commit` mengembalikan `CommitID`, `OnFinal` mengembalikan `FinalResult{Outcome, Commit, Purpose}`, `ExpireTurnEnd(id)`, `ResumeTurn()` untuk baris 9 dengan outcome baru `FinalTurnEndSuperseded` (teks FINAL tetap di turn yang berlanjut); tes `TestDoubleTurnEndAfterResume`, `TestResumedTurnEndFinalReplacesPreResumePartial`, `TestTimeoutThenResumeKeepsEarlierText`, `TestResumeTurnIgnoresOtherTurnsAndDiscards`. `ScriptedSTT` mengikuti kontrak port (buffer 64, buang PARTIAL tertua, FINAL/CLOSED tidak pernah dibuang, `Close` tidak panic/blok; `DroppedPartials`), `Barrier()` setelah `Close` → 0, assertion `RoutedSTTSession` dipindah ke `scripted_stt.go`; `LoadTimeline` memakai `DisallowUnknownFields`, menolak data sisa dan teks partial kosong. Kasus potongan 100 ms → nol event dipatok di 3 offset dalam segmen (0/300/600 ms).

## M5-T10 — `Conn`, `Outbound`, reader, writer, heartbeat, limit
- `internal/conversation/conn.go`: `MessageKind` (`MessageText=1`, `MessageBinary=2`) dan `Conn{Read, Write, Ping, Close}` §7.13.1; `TimeoutFunc`, `Ticker` + `NewRealTicker` (seam waktu untuk fake clock).
- `outbound.go`: `Outbound` (mutex) dengan antrean `control` (prioritas) dan `audio`; `NewOutbound(cap, m)`, `EnqueueControl(env, durable)` (penuh + durable → `ErrSlowConsumer`, `len(control) > 128` → transient menggantikan transient sejenis/dibuang), `EnqueueEvent(ev)` + `EventDurable(ev)` (`error` non-fatal durable), `EnqueueAudio`, `EnqueueAudioEnd(gen)`, `PurgeAudio(activeGen)`, `AudioLen`, `ControlLen`, `Next(ctx)` (semua control lalu satu item audio, blok dengan `ctx.Done()`), `Drained()` (antrean kosong dan tidak ada item di tangan writer).
- `writer.go`: `Writer.Run(ctx, conn, out, activeGen, acks)` — satu-satunya penulis; frame dengan `Gen != activeGen.Load()` dibuang sebelum ditulis (`audio_frames_out_total{dropped_stale}`, Review Focus 2); frame `LastOfSentence` menaikkan hitungan kalimat (`SentencesWritten(gen)`) dan ack `SENTENCE_WRITTEN`; penanda akhir generation → ack `AUDIO_WRITTEN_ALL`; tiap tulis dibatasi 5 s (`ErrWriteTimeout`).
- `reader.go`: `Reader.Run(ctx, conn, cmdCh, audioIn, notices)` — token bucket biner 100/s burst 200 dan JSON 20/s burst 40, JSON > 16384 B / biner > 4096 B / < 6 B / byte 0 ≠ 0x01, `session_id` salah → `*ProtocolError{PROTOCOL_VIOLATION}` (kind `rate|frame_size|header|session_id`); `version ≠ 1` → `UNSUPPORTED_PROTOCOL_VERSION` (kind `version`); JSON rusak / tipe tak dikenal / tanpa `event_id` → `ReaderNotice{INVALID_MESSAGE, RefEventID}`, > 10 dalam 60 s → `PROTOCOL_VIOLATION` (`invalid_burst`); `event_id` duplikat (LRU 64, `EventIDCache`) diabaikan diam-diam; `audioIn` penuh → frame terbaru dibuang (`audio_frames_in_total{overflow}`); setiap frame menyentuh `Activity`.
- `ratebucket.go`: `bucket` (token bucket di atas `clock.Clock`) dan `burstWindow` (jendela geser, aman konkuren; konstanta `INVALID_MESSAGE` 10/60 s dan `AUDIO_FRAME_INVALID` 50/10 s).
- `heartbeat.go`: `Activity` (lastActivity atomic, monoton), `Heartbeat.Run(ctx, conn, act)` — ping tiap 15 s (timeout 15 s), ping sukses = aktivitas, `now − lastActivity > 45 s` → `Close(4102, "HEARTBEAT_TIMEOUT")` + `ErrHeartbeatTimeout`.
- `metrics.go`: `Metrics` minimal nil-safe (`NewMetrics(reg)`), diperluas Task 18.
- Tes: `TestOutboundPriority`, `TestOutboundNextBlocksAndHonoursCtx`, `TestPurgeAudioDropsStale`, `TestSlowConsumer`, `TestSlowConsumerCountsAudio`, `TestTransientCoalescing`, `TestEventDurable`, `TestEnqueueEventNonFatalErrorIsDurable`, `TestWriterWritesControlAndAudio`, `TestWriterDropsStaleGen`, `TestWriterTimeout` (fake 4 s → masih blok, 5 s → `ErrWriteTimeout`), `TestWriterStopsOnCtx`, `TestWriterNilAcksAndMetrics`, `TestReaderLimits` (tabel 11 kasus termasuk burst 200/201 biner dan 40/41 JSON dengan fake clock), `TestReaderBinaryRateRefills`, `TestReaderForwardsCommandsAndAudio`, `TestReaderAudioOverflowDropsNewest`, `TestInvalidMessageWindow`, `TestBurstWindowSlides`, `TestDuplicateEventIDIgnored`, `TestReaderStopsOnCtx`, `TestTokenBucket`, `TestHeartbeatPingKeepsConnectionAlive`, `TestHeartbeatIdleTimeout`, `TestActivityTouchIsMonotonic`; semua memakai `testutil.NoGoroutineLeak` bila ada goroutine. `-race` (×10 untuk tes baru), `go build ./...`, `make arch-check`, `make lint`, `go test ./test/arch/` hijau. Deviasi: lihat `docs/implementation-deviations.md` § M5-T10.

## M5-T11 — Pacer downstream
- `internal/conversation/pacer.go`: `Pacer` (`NewPacer(clk, sleep, maxLeadMS)`), `Run(ctx, frames, out, PacerHooks)` mengikuti blok kode §7.5.4: `lead = sentMs − played`, underrun (`lead < 0`) mereset `start`, frame di-enqueue bila `lead < AUDIO_MAX_LEAD_MS` dan `out.AudioLen() < 25`; selain itu `sleep(min(20 ms, lead−max+1))`. Waktu hanya dari `clk` (tanpa `time.Now()`). `EstimatedPlaybackEnd()` = `start + sentMs` (aman konkuren, zero sebelum frame pertama).
- `PacerHooks`: urutan frame pertama `BeforeFirst` → `EnqueueAudio` → `OnFirst` → `OnSentenceStart`; `OnSentenceStart` pada frame pertama tiap `SentenceIdx`; `OnDone(frameCount, totalMS)` hanya pada akhir normal.
- Penanda akhir: `out.EnqueueAudioEnd(gen)` dipanggil setelah channel `frames` ditutup, ctx masih hidup, dan ≥ 1 frame terkirim. Dibatalkan via ctx (Run mengembalikan `ctx.Err()`) atau generation tanpa audio → tanpa penanda.
- Tes: `TestPacerBurstThenRealtime`, `TestPacerUnderrunResetsClock`, `TestPacerBackpressure`, `TestPacerHookOrder`, `TestPacerCancel`, `TestPacerCancelledBeforeStartEnqueuesNothing`, `TestPlaybackEnd`, `TestPacerEndMarkerOnlyOnNormalEnd`; `-race`, `go build ./...`, `make arch-check`, `make lint` hijau. Deviasi: lihat `docs/implementation-deviations.md` § M5-T11.

## M5-T12 — Pipeline generation (LLM → parser → TTS → Opus → pacer)
- `internal/conversation/generation.go`: `GenKind` (`GenOpening/GenReply/GenApology`, nilai = `ai.generation_started.kind`), `GenCause` (`CauseProvider`, `CauseContentFiltered`), `GenEvent` (`GenNumber()`) dengan varian `EvTextDelta{Clean}`, `EvSentence{Sentence}`, `EvFirstAudio`, `EvSentenceAudio{Index, FirstFrameSeq}`, `EvAudioDone{LLMFinished, FrameCount, TotalMS, SentenceCount}`, `EvGenFailed{Cause}` (semua membawa `Gen`). `genParams` + `newGeneration(p)` (ctx/cancel dari `Parent` + `Deadline` opsional, `llmCtx/llmCancel`, `router.NewGeneration(sid, gen, p.Voice)` untuk OPENING/REPLY, `speakingRate = content.SpeakingRate(voice, adaptive)` sekali per generation, `Pacer` per generation). Goroutine: `runLLM` / `runStatic(openingLine)` → `sentences(8)` → `runTTS` → `frames(250)` → `runPacer` → `Outbound`; APOLOGY: `playAsset` → `frames` → `runPacer`. Semua produsen `defer close(...)`, semua tunggu `select` pada ctx; `Cancel`, `LLMCancel`, `Wait`, `PlaybackEnd`, `Summary`.
- `runLLM`: `StreamLLM(llmCtx, {Messages, MaxTokens, Temperature, RequestID "{sid}:{gen}"})`, `prompt.NewSentenceParser(MaxSentences)`; DELTA → `Feed` → `EvTextDelta` + `EvSentence` + kirim ke TTS; `Limited` → `ai_sentence_limit_total{kind}`, `llmFinished=true`, `llmCancel()`; `DONE{STOP|MAX_TOKENS}` → `Close`; `DONE{TRUNCATED}` → tanpa `Close`, `llm_truncated_total`, `llm_finished=false`; `DONE{CONTENT_FILTERED}`/`ERROR{CONTENT_FILTERED}` → `EvGenFailed{CauseContentFiltered}`; ERROR lain → `EvGenFailed{CauseProvider}` (tanpa pindah vendor/RESET). `runStatic`: parser tanpa batas (`math.MaxInt`), `Feed`+`Close`, tanpa `StreamLLM`.
- `runTTS`: satu `Synthesize` aktif, berurutan; `TTSRequest{Text, Language "ja", Voice, SpeakingRate, PreviousText, RequestID "{sid}:{gen}:{idx}"}` (VoiceID/Model diisi router); `Framer` 24 kHz → `Encoder(24000, OPUS_BITRATE, OPUS_COMPLEXITY)` baru per generation/epoch → `OutFrame{Gen, Seq dari 0, SentenceIdx, LastOfSentence pada frame terakhir kalimat, Epoch}`. Epoch: nilai `TTSEpochChanged()` atau error `tts_provider_switched` → epoch baru, encoder baru, `Seq=0`, semua kalimat yang sudah diparse disintesis ulang dari 0; chunk ber-`Epoch` lama diabaikan; frame epoch lama dibuang oleh hook pacer `Admit` (`audio_frames_out_total{dropped_stale}`). Error TTS lain (tanpa `voice.fallback`) → `EvGenFailed{CauseProvider}`.
- Pacer: `BeforeFirst` → `pg.MarkAudioSent()` (S-12), `OnFirst` → `EvFirstAudio`, `OnSentenceStart` → `EvSentenceAudio`, `OnDone(0)` → generation senyap → `EvGenFailed{CauseProvider}`. `AUDIO_DONE` hanya dari ack writer: `(*Generation).OnWriterAck(WriterAck)` mengembalikan `EvAudioDone` untuk `AUDIO_WRITTEN_ALL` generation ini, sekali, dan tidak pernah untuk generation yang gagal (loop Task 19/20 memanggilnya untuk tiap ack). `playAsset`: paket `ApologyFrames()` sebagai satu kalimat (`EvSentence{ApologyText}`), `LastOfSentence` pada frame terakhir, `llm_finished=true`.
- `voiceSelection(content.CharacterSnapshot)` (D-015): `Primary` dari `provider_name/model_name/voice_id`, `Fallback` dari `voice.fallback` atau nil, nilai apa adanya. `sleepCtx(ctx, d)` (timer nyata; seam `genParams.Sleep` untuk fake clock).
- Tambahan kecil di luar file brief: `audio.OutFrame.Epoch` (internal, tidak dikirim), `PacerHooks.Admit` (dipanggil setelah tunggu pacing, sebelum `BeforeFirst`; frame yang ditolak tidak di-enqueue/dihitung), `Metrics` + `kotoki_realtime_ai_sentence_limit_total{kind}` dan `kotoki_realtime_llm_truncated_total`.
- `internal/testutil/fakes/scripted_router.go`: `ScriptedRouter` (`provider.RealtimeRouter`) + `Script{Deltas, Finish, LLMErrAfter, LLMErr, PCMPerSentenceMS, Gate, TTSEpochAfterSentence, TTSErr, TTSErrSentence}`; `ScriptedGeneration` mencatat `LLMRequests`, `TTSCalls{Req, Index, Epoch}`, `Log` berurutan, `MarkAudioSentCalls`, `Epoch`, `LLMCancelled` (lewat `context.AfterFunc`); switch epoch meniru router (hanya bila ada `voice.fallback`, error eligible, dan belum `MarkAudioSent`). `SetOnMarkAudioSent` untuk tes S-12.
- Tes (`generation_test.go`, paket eksternal `conversation_test` + `export_test.go` karena `fakes` mengimpor `conversation`): `TestPipeliningOrder`, `TestSentenceLimit`, `TestTextDeltaSanitized`, `TestSpeakingRateFormula`, `TestVoiceSelectionFromSnapshot`, `TestNoTTSFallbackFailsToApology`, `TestLLMErrorFailsGeneration`, `TestTTSEpochRestart`, `TestTruncated`, `TestContentFiltered`, `TestStaticOpening`, `TestApologyAsset`, `TestMarkAudioSentBeforeFirstFrame`, `TestCancelNoLeak`, `TestSilentGenerationFails` (6 kasus), `TestAudioDoneFromWriterAck`, `TestSleepCtx`; `TestPacerAdmitDropsFrames` di `pacer_test.go`. Writer asli + `Outbound` asli + fake clock; `-race` ×50 stabil; mutasi (filter epoch, pemetaan senyap, `llmCancel` di limit, `MarkAudioSent` terlambat, `Close` pada TRUNCATED, CF sebagai provider, ack generation lain) masing-masing menggagalkan tes terkait. `go build ./...`, `make arch-check`, `make lint` hijau.
- Perbaikan review (fix 1): `deliver` tidak lagi membuang kalimat yang sudah diparse saat `LLMCancel` (menunggu pada `g.ctx` + `ttsStopped`); `runLLM` mengutamakan `llmCtx` yang sudah dibatalkan; mutex `{ttsEpoch, firstAdmitted}` menggantikan dua atomic pada gerbang epoch; chunk ber-epoch lebih baru → restart; limit R-17 di `parser.Close()` dihitung; jam pacer mulai pada frame pertama yang di-admit; `fakes.ScriptedGeneration.ForceEpoch(n)`. Tes baru: `TestLLMCancelKeepsParsedSentences`, `TestSentenceLimitReachedInClose`, `TestTTSSwitchAfterFirstFrameFails`, `TestTTSSwitchBeforeFirstAdmitRestarts`, `TestPacerClockStartsAtFirstAdmittedFrame` (+ cek jam di `TestPacerAdmitDropsFrames`); asersi `LLMCancelled` memakai `Eventually`.

## M5-T13 — Context builder (§7.8)
- `internal/conversation/context_builder.go`: `Mode`, `TurnContext`, `ConversationInput`, `Message`, `BuiltPrompt`, `SummaryInput`, `PromptBuilder`, `NewPromptBuilder()` (§7.8.3; karakter dari `Config.Snapshot.Character`). `BuildConversation` murni dan deterministik: `buildPromptData` memetakan `content.SessionConfig` → `prompt.PromptData` (§6.8.2, tanpa I/O, tanpa `Style`/`Expression*`), lalu `prompt.Render(in.Prompt.Template(), data)` menjadi satu-satunya pesan `system`; pesan setelahnya = recent turns lalu input user (CONVERSATION), atau hanya `system` (OPENING).
- Budget per komponen dengan `prompt.EstimateTokens` + `ItemOverheadTokens`: persona dipotong di akhir paragraf (`character`), situasi dipotong lalu butir daftar dari belakang (`scenario`), input user dipotong dari belakang (`current_input`); urutan pemangkasan 8 → 7 → 10 → 6 → 11 (`memory_episodic` → `memory_core` → `summary` → `learner` (hanya baris `学習のねらい`) → `recent_turns` sampai minimum 4 turn, lalu `user_text` > 300 rune), dicatat di `Truncations`. Item memory dibuang dari skor terendah; `ComponentTokens` per komponen (`system`, `character`, `scenario`, `learner`, `memory_core`, `memory_episodic`, `summary`, `session_state`, `recent_turns`, `current_input`).
- Bentuk pesan: INTERRUPTED → `assistant_spoken_text + "（ユーザーが割り込みました）"`, FAILED → hanya `user`, DISCARDED dilewati, opening (tanpa `user_text`) → hanya `assistant`. Speech: `Formality` dari `adaptive.Formality` (fallback `Snapshot.Formality`), `MaxSentencesPerTurn` dari `adaptive.MaxAISentencesPerTurn` (fallback `content.DefaultStyle`). `RemainingMinutes` dibulatkan ke atas; `OutputContract(closing = remaining ≤ 60 s)`. `max_output_tokens`/`temperature` dari `llm_params` DB, default 300/200/0,8.
- `SelectRecentWindow(turns, coveredUntilSeq)`: window 8 non-DISCARDED, diperluas mundur ≤ 12 selama turn sebelumnya belum tercakup summary (§7.8.4). `BuildRollingSummary`: template §7.8.5 persis (`MaxRunes` default 300), `MaxOutputTokens 400`, `Temperature 0,3`.
- Tes: `TestBuildMaxFixtureWithinBudget` (golden `testdata/prompt/conversation_max.golden.json`, `-update`; 4.904 token), `TestTrimOrder`, `TestTrimDropsLowestScoreFirst`, `TestRecentTurnsKeepMinimumThenCutUserText`, `TestCurrentInputTruncatedKeepsFront`, `TestMessageShape`, `TestOpeningOnlySystem`, `TestSystemIsRender`, `TestFullNameNeverInPrompt`, `TestCharacterFromSnapshot`, `TestSpeechFromAdaptive`, `TestDeterministic`, `TestMaxOutputFromParams`, `TestRollingSummaryPrompt`, `TestRecentWindow`; `-race`, `go build ./...`, `make arch-check`, `make lint` hijau. Deviasi: lihat `docs/implementation-deviations.md` § M5-T13.

## M5-T14 — Adaptive in-sesi, rolling summary, summarizer
- `adaptive.go`: `AdaptiveState`, `ClarificationRegex`, `MaybeAdjust` (murni; hanya 6 teks terakhir dihitung; `TTSSpeakingRate`/`MaxAISentencesPerTurn` dari `learner.TTSSpeakingRate`/`MaxAISentences`; masukan tidak dimutasi). `rolling_summary.go`: aritmetika trigger (`seq ≤ oldest−1`, `> covered`, non-DISCARDED) dan `coveredUntil`. `summarizer.go`: `Summarizer` (`NewSummarizer(router, builder, timeout, opts...)`, `ShouldRun`, `Run`) memanggil `RealtimeRouter.CompleteText` dengan `TextRequest{Task:"ROLLING_SUMMARY", MaxTokens:400, Temperature:0.3, RequestID:"ROLLING_SUMMARY:{sid}"}`, teks di-trim, `CoveredUntilSeq` = seq tertinggi turn non-DISCARDED masukan (kontrak `SelectRecentWindow`). 3 gagal berturut-turut (timeout/error router/teks kosong) menghentikan summarizer; sukses mereset; pembatalan ctx sesi tidak dihitung gagal.
- Metrik `kotoki_realtime_difficulty_adjustments_total` (nil-safe, tanpa label); dipanggil loop (Task 15+).
- Tes: `TestMaybeAdjust`, `TestClarificationRegexAlternatives`, `TestSummarizerTrigger`, `TestSummarizerRequest`, `TestSummarizerTimeoutAndStop`, `TestSummarizerSuccessResetsFailures`, `TestSummarizerCancelNotCounted`, `TestSummarizerEmptyTextIsFailure`; `-race`, build, arch-check, lint hijau.

## M5-T15 — Persister & redisSync
- `internal/conversation/persister.go`: `Persister` (`NewPersister(sid, turns, events, sess DisconnectedAdder, fence, m, log, opts...)`, `Start(ctx)`, `Submit(job) error`, `Pending()`, `Flush(ctx)`, `Close(ctx)`), job `InsertTurn`, `UpdateUser`, `FinalizeTurn`, `SessionEvent`, `AddDisconnected` (interface tertutup `PersistJob`). Satu goroutine menulis berurutan lewat port ber-fence; `Submit` tidak pernah memblok loop, `UpdateUser` antre untuk turn sama ditimpa yang terbaru di posisi lama (§7.12.3). `session.ErrFenceLost` → antrean dibuang, tidak ada tulis lagi, `OnFenceLost` sekali, `fence_lost_total`+1, `Submit/Flush/Close` → `ErrFenceLost`. Error lain → job yang sama diulang (backoff 100 ms ×2 s/d 2 s, 5 s per percobaan); antrean > 64 atau job kepala gagal terus > 30 s → `OnStuck(ErrPersisterStuck)` sekali (§7.11.4 `SERVER_ERROR`). Callback dipanggil dari goroutine persister dan tidak boleh memblok.
- `internal/conversation/redissync.go`: `RedisSync` (`NewRedisSync(store, clk, sid, pod, fence, m, log, opts...)`, `Start`, `Acquire`, `Update`, `UpdateTurn`, `ClearTurn`, `UpdateSummary`, `Event(ev, env)`, `Flush`, `Release`, `Abort`). Setter hanya menyentuh memori; satu goroutine (tick 50 ms) menulis: owner Lua tiap 5 s TTL 15 s, hb tiap 5 s TTL 60 s, rt ≤ 1 HSET/100 ms (PodID/Fence/UpdatedAtMS diisi saat tulis), turn 250 ms (DEL segera saat final), summary segera, event durable (`EventDurable`, bukan `IsDurable`; replay diabaikan) ke stream dengan `prev_seq` berantai. TTL_S = `max(300 s, expires_at − now + 300 s)`, sebelum ACTIVE `duration + 600 s`. Tiap operasi dibatasi 200 ms. Redis gagal → tandai down, sesi jalan terus, snapshot terbaru disimpan, event dibuang (`prev_seq` tetap maju sehingga resume melihat celah → `UNAVAILABLE`), probe tiap `HEARTBEAT_INTERVAL` berupa tulis ulang penuh (owner via Lua, hb, rt, turn, summary). Owner ditolak → `OnFenceLost` sekali, `fence_lost_total`+1, berhenti menulis. Opsi: `RedisSyncTuning`, `RedisSyncSessionDuration`, `RedisSyncLastDurableSeq` (takeover), `RedisSyncOnFenceLost`, `RedisSyncTicker`, `RedisSyncTimeoutFunc`.
- Metrik `kotoki_realtime_fence_lost_total` (nil-safe, tanpa label). Error Redis hanya log WARN (metrik klien Redis milik Bab 13, §7.14).
- Pemakaian loop (Task 18–23) didokumentasikan di komentar tipe: persister `Start` → `Submit` → `Flush` (≤ 5 s) sebelum `Complete/Fail/MarkHandoff` → `Close`; redisSync `Acquire` → `Start` → setter/`Event` per event terkirim → drain `Flush` lalu `Release`; `finish()` `Update(rt COMPLETED)` lalu `Release`; fence lost dari persister → `Abort()`; hasil Summarizer yang `coveredUntilSeq`-nya naik → `UpdateSummary`.
- Tes: `TestPersisterOrdered`, `TestPersisterCoalesce`, `TestPersisterCoalesceKeepsOtherTurnsAndOrder`, `TestPersisterFenceLost`, `TestPersisterFenceLostFromEvents`, `TestPersisterRetryThenSuccess`, `TestPersisterStuckWhenFailingOver30s`, `TestPersisterStuckWhenQueueOver64For30s`, `TestPersisterCloseStopsOnContext`, `TestPersisterStopsWithContext`, `TestRedisSyncCoalesce`, `TestRedisSyncTurnCoalesce250ms`, `TestRedisSyncTTL`, `TestRedisSyncHeartbeatAndOwnerEvery5s`, `TestEventsOnlyDurable`, `TestRedisSyncLastDurableSeqOnTakeover`, `TestRedisDownKeepsRunning`, `TestRedisSyncOpTimeout200ms`, `TestOwnerRefreshLost`, `TestRedisSyncFlushAndRelease`, `TestRedisSyncFlushWhileDownFails`, `TestRedisSyncAcquire`, `TestRedisSyncTuningOverrides` (fake clock + ticker manual, tanpa sleep nyata; leak helper). Dicek dengan mutasi (IsDurable saja, summary tidak ditulis ulang, event disimpan saat down, `prev_seq` dibekukan saat down, tanpa coalesce rt/persister, fence lost diulang) — semuanya RED. `-race`, build, arch-check, lint hijau.
- **Fix review 1 (T15).** Persister: klasifikasi error per percobaan (fence / permanen / transien pasti / ambigu), retry idempoten (`InsertTurn` 23505 setelah ambigu = sukses; `FinalizeTurn` 0 baris setelah ambigu dicek ulang via `GetForAttach`; `AddDisconnected` baca-ulang `disconnected_seconds` terhadap baseline), error permanen gagal cepat (`ErrPersistRejected`, `PersisterOnFatal`), `Abort()`, `FenceLatch` bersama (`fence_lost_total` sekali per runtime). redisSync: setelah owner ditolak goroutine berhenti dan tidak ada tulis apa pun (dipatok tes), `Release`/`Abort` menunggu pass berjalan, `RedisSyncFenceLatch`. `session.RuntimeRecord.DisconnectedSeconds`. Leak helper kini mencetak stack yang bocor. Tes baru: `TestPersisterQueueOf64IsNotStuck`, `TestPersisterQueueAbove64ForExactly30sIsNotStuck`, `TestPersisterQueueAbove64ForOver30sIsStuck`, `TestPersisterFailingForOver30sIsStuck`, `TestPersisterTransientServerErrorRetried`, `TestPersisterAmbiguousInsertDuplicateIsSuccess`, `TestPersisterDuplicateInsertWithoutAmbiguityIsRejected`, `TestPersisterDuplicateAfterDefiniteFailureIsRejected`, `TestPersisterPermanentErrorsFailFast`, `TestPersisterAmbiguousFinalizeIsNotFenceLoss`, `TestPersisterAmbiguousFinalizeThenFenceReallyLost`, `TestPersisterAddDisconnectedAmbiguousCommittedCountsOnce`, `TestPersisterAddDisconnectedAmbiguousNotCommittedRetried`, `TestPersisterUpdateUserMergeStaysBeforeFinalize`, `TestPersisterAbortDropsQueueWithoutWriting`, `TestRedisSyncReleaseKeepsInFlightEvents`, `TestRedisSyncPendingEventsCap500`, `TestFenceLatchCountsOnce`, `TestLeakReportNamesTheGoroutine`; `TestOwnerRefreshLost` diperluas. Mutasi m5, m1, m6 dan 17 lainnya → RED.

## M5-T16 — `redisstore` state realtime (integration)
- `internal/redisstore/realtime_state.go`: `NewRealtimeState(rdb redis.UniversalClient, log *slog.Logger) *RealtimeState` (`var _ rtstate.Store`). Key persis §7.9.1 (UUID kanonik via `uuid.String()`); owner acquire/refresh/takeover dan release memakai skrip Lua §7.9.2 verbatim lewat `redis.Script.Run` (EVALSHA, fallback EVAL pada NOSCRIPT; nilai `pod|fence`, `PX` = ttl); hash `rt`/`turn`/`summary`/`podinfo` ditulis `HSET` + `PEXPIRE` dalam satu MULTI (key tak pernah hidup tanpa TTL; `disconnected_at` kosong untuk 0, `adaptive` JSON); stream `XADD MAXLEN ~ 500` + `EXPIRE 120` dalam satu MULTI; `memctx` dibaca mentah; pub/sub `kotoki:realtime:pod:{id}` JSON `{"type":"TAKEOVER","session_id","fence"}`, `SubscribePod` mengonfirmasi langganan sebelum kembali, resubscribe dengan backoff 100 ms→1 s setelah koneksi putus, channel ditutup saat ctx selesai. Setiap operasi berbatas 200 ms; error dibungkus `errs.Wrap(CodeInternalError)`; tidak mengimpor `conversation`.
- `redisstore.NewClient` dan `testutil.NewRedis` kini `ContextTimeoutEnabled: true` (lihat deviasi).
- Tes (`realtime_state_integration_test.go`, tag `integration`): `TestRealtimeStateRuntimeStoreSuite` (suite `repotest.RuntimeStoreSuite` ke Redis nyata), `TestOwnerLua` (fence 3 lalu 2 → ditolak, nilai `pod|3`, PX ≈ ttl, fallback NOSCRIPT, release), `TestEveryKeyHasTTL`, `TestStreamMaxLen` (1000 append → 500..600), `TestRTHashFields` (himpunan field persis §7.9.1, tanpa `stt_vendor`), `TestTurnDeleted`, `TestPubSubTakeover` (format kawat, kanal pod lain, resubscribe setelah `CLIENT KILL TYPE pubsub`, channel tertutup), `TestKeysUseCanonicalUUID`, `TestRedisTimeout` (< 250 ms).
- Verifikasi: perintah Step 2 plan → PASS (11 tes); seluruh `go test -tags integration -race ./internal/redisstore/...` PASS; `make arch-check` hijau; `make lint` tidak melaporkan isu pada kode task (satu-satunya isu typecheck: `opusfile.pc` tidak ada di mesin ini, memblokir paket `conversation/audio`, sehingga `go build ./...` dan paket yang bergantung pada audio, termasuk `testutil/fakes`, tidak bisa dibangun di sini).

## M5-T17 — Event writer adapter & config loader
- **`internal/conversation/event_writer.go`**: `NewSessionEventWriter(repo, ids, clk)` memetakan `Append` ke `session.EventRepository.AppendFenced`: `actor=REALTIME`, `from/to_status` NULL, `turn_id` dan `fence` diteruskan, payload di-marshal JSON (nil → `{}`; payload yang tidak bisa di-marshal → error tanpa memanggil repo). Pemanggil bertanggung jawab agar payload tanpa teks.
- **`internal/conversation/config_loader.go`**: `NewConfigLoader(l, prompts, size)`. Snapshot karakter di-decode per sesi (`content.ParseSnapshot`, rusak → `CONTENT_MISCONFIGURED`), versi topic/scenario/prompt di-cache LRU per ID, OPENING dimuat by ID dari `rec.OpeningPromptVersionID` (cache LRU terpisah). Tidak ada query `PUBLISHED`.
- Tes: `event_writer_test.go`, `config_loader_test.go` (11 tes lulus, `-race`).
- Perbaikan lint yang sudah ada dari T16: konstanta `subBackoffMin/Max` di `redisstore/realtime_state.go` diganti nama (`time-naming` revive).

## M5-T18 — Metrik §7.14, `Runtime` kerangka, harness, alur start
- **`internal/conversation/metrics.go`**: `Metrics` diperluas (tipe yang sama dari T10) dengan seluruh tabel §7.14 (`kotoki_realtime_` + 37 nama, bucket `metrics.TurnBuckets`/`BargeBuckets` dari `platform/metrics`, `frame_processing_seconds` dan `prompt_tokens` memakai bucket yang disebut §7.14) ditambah `kotoki_llm_content_filtered_total{provider}` dan `kotoki_content_opening_fallback_total`. Method baru diekspor dan nil-safe (`SessionState`, `Connection`, `WSClose`, `TurnEndToFirstAudio`, `Turn`, `STTDiscarded`, `BargeInStop`, `Reconnect`, `PromptTokens`, dst.): tiap komentar menyebut baris spec yang diisinya sehingga task 19–23 tinggal memanggil.
- **`runtime.go`**: `Deps` (+`Limits`), `Runtime` (aktor): `loop` memilih dari `cmdCh/notices` (milik koneksi), `frameEvents`, `sttEvents`, `genEvents`, `writerAcks`, `connExits`, `ctl`, `startDone`, `fenceLost`, `fatalCh` dan ticker 100 ms; semua state sesi/turn/generation hanya disentuh loop (tanpa mutex). Semua deadline adalah `time.Time` di `deadlines` yang dicek tiap tick terhadap `Deps.Clock` (`startBy`, `warn[]`, `expiry`, `balance`, `playback`); context yang harus habis menurut clock itu dibuat lewat `ctxWithDeadline`/`cancelAt` (learner 1 s, memori 2 s, connect STT 3 s, start 10 s, deadline generation `expires_at + PROVIDER_SESSION_GRACE`). `emit` mengalokasikan `server_seq`, mengirim ke `RedisSync.Event` dan `Outbound` (durable penuh → slow consumer → close 4101). `View`/`Barrier`/`call`/`post` adalah seam baca-state dan sinkronisasi untuk tes. Titik perluasan Task 19–23 ditulis di komentar tipe `Runtime`, `deadlines`, `Manager`, `frameEvent`.
- **`connstate.go`**: `connState` = `Conn` + `Outbound` + reader/writer/heartbeat + kanal per koneksi; dibuat `Manager` sebelum `Runtime` ada (handshake berjalan di atasnya) lalu diadopsi runtime; `closeAfterDrain` (event terakhir tertulis sebelum close frame) dan `closeNow`; panic goroutine koneksi dipulihkan.
- **`manager.go`**: `NewManager(Deps)`, `Attach(ctx, conn, AttachTicket)`: `GetForAttach` → pemilik (4003/4004) → tabel status §7.3.2 → `session.ready` (server_seq 1) → command pertama ≤ 10 s (4008, deadline di clock, ticker `Deps.NewTicker`) → `session.start`/`resume` (audio D-071 salah → 1008, command lain → 1008) → `startRuntime` (kapasitas ≥ `Limits.Max` → 4503, `ClaimRuntime([RESERVED])`, `MarkStarting`, `ErrStartDeadlinePassed` → 4010). `Active()`, `Runtime(sid)`, `Close()`. Jalur resume/takeover berupa seam `Manager.resume` (Task 21).
- **`runtime_start.go`**: §7.6.3 langkah 3–9: goroutine startup (owner `Acquire`, rt, `OWNER_CHANGED{reason:"START"}`, errgroup: learner 1 s/memori 2 s degrade, snapshot → (memori ‖ `OpenSTT` 3 s, wajib), `Activate`), hasilnya diserahkan ke loop (`startDone`); gagal/timeout 10 s → `FailStart` (`PROVIDER_FAILURE` bila STT belum terbuka, `SERVER_ERROR` selain itu), `session.failed` + `error`, close 4500/1011; sukses → `session.started` (generation 0), timer (warning T-60/T-10, expiry, `balance.tick`), `pendingEnd`, opening AI_FIRST. Span `session.start` (`session.id`, `fence`, `result`).
- **`runtime_gen.go`**: inti turn/generation yang dibutuhkan opening: turn 0 (INSERT status NULL, `turn.started{AI}`), `startGeneration` (prompt dari cache runtime, `generation++`, `Generation`, `ai.generation_started`; opening statis S-2 bila `OpeningPromptVersionID` NULL: `kotoki_content_opening_fallback_total`, WARN, `llm_provider` NULL), event generation → `ai.text_delta/sentence/audio_started/audio_completed`, baris 10 (`AI_SPEAKING` + `commitBoundary`), 14, 15 (`playback_finished` atau `playbackDeadline` + `playback_finished_missing_total`), baris 12, `cancelGeneration`/`interruptGeneration`, kolom turn final (§7.6.6).
- **`runtime_end.go`**: bentuk akhir sesi yang dibutuhkan task ini: `userEnd`/`onExpiry` (`BeginEnding` → `session.ending` → turn ditutup → `finish`: flush persister ≤ 5 s → `Complete` → `session.completed` → close 1000), `failStart`, `fatal` (§7.11.4), `onFenceLost` (berhenti tanpa tulis, 4011), `exit` (persister drain → `RedisSync.Release` → stop goroutine → registry). Task 22 menyempurnakan `endWith` (grace, USER_SPEAKING/PROCESSING).
- **`internal/testutil/rtharness/{harness.go,pipeconn.go,ticker.go}`**: `New(t, opts...) *H` (fake clock, `Tickers`/`ManualTicker`, `PipeConn`, `ScriptedRouter`+`ScriptedSTT`, fake port, `ContentLoader`, `Manager`), `Connect/Attach/Send`, `StartCmd/ResumeCmd/EndCmd/PlaybackFinishedCmd`, `Speak(fixture)` (Opus asli 48 kHz per 20 ms fake time via `audio.Encoder`), `Silence`, `Sync` (barrier audio→loop, STT→loop, drain loop, `Outbound.Drained`, dua putaran stabil), `Events/WaitEvents/Types/Frames`, `AdvanceClock`, `Tick/TickPing`, `WaitClosed`, `Close` (+ leak check). Opsi: `WithStartMode/Status/Duration/WithoutOpeningPrompt/WithVoiceFallback/WithScript/WithTimeline/WithSTTOpenError/WithTuning/WithLimits/WithGenerationWrapper`; `Gates` (menggantung load config/learner/memori/STT), `OnLearner`.
- `.golangci.yml` (aturan 5: `!**/internal/testutil/rtharness/**`) dan cermin tabelnya di `test/arch/rules_test.go` + `arch_test.go`.
- Perubahan kecil di kode T10–T12/fakes: `AudioIn.Barrier` (seam tes), `genParams.OnPanic` (panic goroutine generation → loop), `fakes.SessionRuntime.Recorder`/`Claims()`.
- Tes (`metrics_test.go`, `runtime_start_test.go`, paket `conversation_test` karena `rtharness`/`fakes` mengimpor `conversation`): `TestMetricsNamesAndBuckets`, `TestMetricsNilSafe`, `TestSessionStateGauge`, `TestStartUserFirst`, `TestStartLoadsCharacterFromSnapshot`, `TestStartAIFirst`, `TestOpeningFallbackStatic`, `TestStartDeadlinePassed`, `TestStartFailsWhenSTTUnavailable`, `TestStartDegrades` (5 sub-kasus termasuk learner menggantung 1 s di fake clock), `TestStartTimeout10s` (config menggantung → 1011 `SERVER_ERROR`; STT menggantung → 4500 `PROVIDER_FAILURE` pada 3 s), `TestSessionEndBeforeActive`, `TestHandshakeFirstCommand` (5 sub-kasus), `TestStartWhileActive`; tambahan: `TestOpeningPlaybackDeadline`, `TestOpeningFailureFailsTurn`, `TestAttachRefusals`, `TestStartRefusedAtCapacity`, `TestStartErrorsMapToRefusals`, `TestFenceLostWhileStarting`, `TestSpeakIsDiscardedUntilTheAudioPipeline`, `TestProtocolViolationAfterStart`, `TestInvalidMessageIsNonFatal`, `TestConnectionLossDetaches`, `TestTimersWarningsBalanceExpiry`, `TestPanicInStartupFailsTheStart`, `TestPanicInGenerationEndsTheSessionServerError`.
- Verifikasi: lihat laporan commit; `-race -count=15` pada tes baru, `-count=5` pada seluruh paket `conversation`, `make arch-check`, `make lint` (0 isu).

## M5-T19 — Turn engine: user turn, discard, generation, penutupan turn; audio pipeline
- **`internal/conversation/runtime_audio.go`** (baru): `audioPipeline` per koneksi (goroutine `audio` milik `connState`): aturan `seq` §7.5.2 (`dup` dibuang, gap ≤ 5 → `DecodePLC` per frame hilang, ≤ 50 → silence, > 50 → 50 silence + WARN `upstream discontinuity`; `expected` mulai 0 per koneksi), decode langsung 16 kHz (`audio.Decoder`), paket tak valid → frame silence 320 sampel diteruskan + `AUDIO_FRAME_INVALID` ≤ 1× / 5 s (lewat `frameEvent.invalid`, dikirim loop) dan > 50 dalam 10 s → `*ProtocolError` `PROTOCOL_VIOLATION` (loop menjawab 1008), VAD (`audio.VAD`, mode `VAD_MODE`), `stt.SendAudio` (selalu, juga saat `AI_SPEAKING`; STT dibaca lewat `Runtime.sttPipe` `atomic.Pointer` yang diisi loop saat sesi ACTIVE), `frameEvent{voiced, invalid, at}` ke loop; metrik `audio_frames_in_total{ok|gap|dup|invalid}` dan `frame_processing_seconds{opus_decode|vad}`. Sisi loop: `onFrameEvent` → `pushFrame` (detector, `audioMs = det.NowMs()`, menghitung ms voiced turn), watchdog starvation (`checkStarvation` di tick: > 200 ms tanpa frame saat `USER_SPEAKING` → silence yang berlalu disuntikkan ke detector).
- **`internal/conversation/runtime_turn.go`** (baru): `turnRun` (dipindah dari `runtime_gen.go`, + sisi user), `onDetector` = tabel §7.6.1: baris 1 (`beginUserTurn`: INSERT `status` NULL, `MarkTurn`, `speech.started`, `turn.started{USER}`), 2 (`!accepting` diabaikan), 3/4 (`endUserTurn`: `Finalize` commit `TURN_END`, timer `STT_FINAL_TIMEOUT`, `turn_end_reason_total`, `noMerge` setelah `MAX_UTTERANCE`), 5/6 (`speech.ended`/`speech.started`), 7/8 (`resolveUserText`: FINAL commit atau timeout → `isDiscard` → `discardTurn` atau `UpdateUser` + `stt.final` + `startGeneration(REPLY)`), 9 (`resumeUserTurn`: `ai.interrupted{USER_RESUMED}`, `ResumeTurn`, `turn_resumed_total`, guard `noMerge` dan utterance < 45 s), 9b (`interruptOpening`: turn 0 `INTERRUPTED`, lalu baris 1); `onSTTEvent` (dipindah dari `runtime.go`), `stt.partial` (throttle 100 ms + sisa yang tertahan keluar di tick), `commitBoundary` (dipindah), `postTurn` (§7.7.5: `recentTurns` maks 12, `MaybeAdjust` + `Learner.Rerender` + `DIFFICULTY_ADJUSTED` + `difficulty_adjustments_total` hanya `COMPLETED`, rolling summary lewat `Summarizer` (`SummaryReady` kembali ke loop lewat `post`), refresh memori `RefreshEpisodic` + `ReloadCoreIfChanged` dalam satu goroutine, maks 1 in-flight, `RefreshEpisodic` dilewati bila `user_text` < 6 rune).
- **`runtime_gen.go`**: `closeTurn` menutup turn user (payload `user_text`, `latency`), `assistant_spoken_text` = kalimat yang frame terakhirnya ditulis writer (`SentencesWritten`), bukan jumlah kalimat yang di-parse; `postTurn`; `ai.text_delta` di-coalesce 100 ms (`flushText` sebelum event generation lain, di `interruptGeneration`/`closeTurn`/`failTurn`, dan di tick); `turn_end_to_first_audio` berpatokan pada deteksi akhir turn (`turnRun.endAt`) untuk REPLY; `llm_first_sentence_seconds`, `server_overhead_seconds`, `stt_final_seconds` diisi; prompt REPLY memuat `RecentTurns` (`SelectRecentWindow`), `RollingSummary`, `CurrentUserText`.
- **`runtime.go`**: field baru (`det`, `lastFrameAt`, `recent`, `summary`, `memBusy`, `failedTurns`, …), `deadlines.sttFinal`, `Deps.STTFinalTimeout` (default 1,5 s = `PROVIDER_TIMEOUT_STT_FINAL`), `onTick` memanggil `checkStarvation`/`flushPartial`/`flushDueText`.
- **Harness/fake**: `rtharness.H.SpeakSlice`, `SendRawUpstream`, `SkipUpstream`, `Gates.Refresh`; `Speak`/`Silence` kini menunggu loop mengejar audio+STT tiap 500 ms audio (`Runtime.Barrier`) agar FINAL dari timeline diproses sebelum frame yang mengikutinya; `rtharness.Router.CompleteText/SetSummary/TextRequests` (rolling summary), `fakes.MemoryRetrieval.Refreshed()`; aksesor metrik di `export_test.go`.
- **Tes** (`runtime_turn_test.go`, `runtime_audio_test.go` internal untuk pipeline): `TestSeqGapPLC`, `TestInvalidFrameSilenceInserted`, `TestUserTurnHappyPath` (AI_FIRST, jadi balasan = generation 2 seperti di plan), `TestFinalizeCountIsThree`, `TestDiscardEchoCommit`, `TestNoiseOnlyDiscarded`, `TestShortNoiseDiscarded`, `TestUserResumesInProcessing`, `TestNoMergeAfterMaxUtterance`, `TestSTTFinalTimeout`, `TestPlaybackDeadline`, `TestOpeningUserResumes9b`, `TestPostTurnTasks`, `TestSpokenTextAfterAudioOnly`; tambahan: `TestCommitFinalsAreFIFO`, `TestResumeAfterFinalTimeoutKeepsText`, `TestRollingSummaryAfterTurns`, `TestPlaybackFinishedOutsideAISpeaking`, `TestAdaptiveAdjustsAfterClarifications`, `TestTextDeltasAreCoalesced`, `TestStarvationEndsTurn`, `TestInvalidFrameBurstViaRuntime`. `TestSpeakIsDiscardedUntilTheAudioPipeline` (T18, sementara) dihapus: frame kini bermakna dan dicakup tes di atas.
- Verifikasi: lihat laporan commit (`-race -count=5` pada tes Step 2, seluruh paket `conversation/...`, `make arch-check`, `make lint`; mutasi, semua gagal merah: discard kosong, discard pendek tanpa durasi voiced, seluruh guard merge baris 9, urutan FIFO commit, commit `listening` setelah discard, timeout STT 500 ms, batal tanpa `ai.interrupted`, tanpa `ResumeTurn`, 9b diabaikan, turn DISCARDED ikut `turn_count`, PLC→silence, duplikat tidak dibuang, audio tidak ke STT, refresh pendek tidak dilewati, in-flight memori tak terbatas. Yang selamat dan alasannya: `noMerge` saja tanpa guard 45 s (redundan: setelah `MAX_UTTERANCE` audio sudah ≥ 45 s dari onset), `min(…, SentencesWritten)` di spoken text (setara selama turn ditutup setelah semua frame tertulis; menjadi berarti di Task 20), reset `failedTurns` (belum ada yang menaikkan sebelum Task 20). Menulis `TestRollingSummaryAfterTurns` menemukan bug: pemicu summary salah memakai jendela yang sudah diperluas, bukan jendela dasar 8 turn; diperbaiki).

## M5-T20 — Barge-in, kegagalan generation, APOLOGY, penghitung gagal
- **`internal/conversation/runtime_bargein.go`** (baru): `onBargeInConfirmed` (baris 13; guard `accepting`, `AI_SPEAKING`, turn dan generation ada) dan `bargeIn` §7.6.5: `interruptGeneration("BARGE_IN")` (cancel, `generation++`, `activeGen`, `PurgeAudio`, `ai.interrupted{BARGE_IN, active_generation, spoken_sentence_count}`), turn lama `INTERRUPTED` (`assistant_spoken_text` = tepat k kalimat yang frame terakhirnya ditulis writer, `assistant_text` penuh; turn yang sedang APOLOGY tetap `FAILED`), `postTurn`, lalu `beginUserTurn(onset, duringAI=true)` (`speech.started{during_ai_speech:true}`, `turn.started{USER}`, teks STT dimulai dari onset lewat `sttState.MarkTurnFrom`). `client.barge_in` (baris 18) hanya hint: `bargein_total{hint_confirmed}` bila barge-in server menyusul ≤ 1 s, `hint_unconfirmed` di tick sesudahnya; `bargein_total{server}` per barge-in. `bargein_stop_seconds` dan baris `BARGE_IN` (`generation`, `spoken_sentence_count`, `stop_ms`) diselesaikan di tick berikutnya dari `Writer.LastAudioWrite()`.
- **`internal/conversation/runtime_failure.go`** (baru; `onGenFailed` dan `failTurn` dipindah dari `runtime_gen.go`): baris 11 dan 16 (`failReply`: `ai.interrupted{GENERATION_FAILED}` hanya bila sudah ada audio, `error{AI_GENERATION_FAILED}` non-fatal durable dengan turn dan generation, `startGeneration(APOLOGY)`; baris turn `FAILED` ditunda di `turnRun.failed` sampai APOLOGY selesai/terpotong: `completeFailedTurn`), baris 12 (`failTurn`, opening), turn non-Jepang (`failNonJapanese`: `stt.final`, lalu APOLOGY tanpa LLM, `error_code="STT_NON_JAPANESE"`), penghitung (`countFailure`: `failedTurns` naik per turn `FAILED`, `CONTENT_FILTERED` hanya `contentFilteredTurns` + `kotoki_llm_content_filtered_total`, non-Jepang tidak dihitung; batas `MAX_CONSECUTIVE_FAILED_TURNS` → `fatal(PROVIDER_FAILURE)`), `sttFailed` (`STTError` → `PROVIDER_FAILURE`, `BeginEnding` → `Fail`, close 4500; tanpa vendor lain, tanpa `PROVIDER_FALLBACK`). Baris `session_events`: `TURN_FAILED` (`turn_seq`, `stage` STT/LLM/TTS, `error_class`), `PROVIDER_FALLBACK` (saat audio pertama bila `UsedFallback`).
- **Perubahan di kode T10–T19:** `Generation` mencatat teks kalimat dan teks penuh di bawah mutex sebelum event dikirim (`SentenceText`, `Text`) dan stage/kelas kegagalan (`FailInfo`); `genRun.spokenText` dan semua `assistant_text` final memakainya, `spokenCount` = hitungan writer (menggantikan `min(len(sentences), writer)`); `Writer` mencatat `LastAudioWrite` (jam `Deps.Clock`); `cancelGeneration` memulai pengamat `generation_goroutine_exit_seconds`; `postTurn` memasukkan turn `FAILED` ke `recentTurns` dan hanya turn `COMPLETED`/`INTERRUPTED` mereset `failedTurns`; `closeTurn` baris 15 menutup turn `FAILED` bila APOLOGY selesai; `closeTurnForEnd`/`failTurnSilently` mempertahankan status `FAILED`; `beginUserTurn(onset, duringAI)`; `View` + `FailedTurns`/`ContentFilteredTurns`.
- **Harness/fake:** `ScriptedRouter.SetScript` (generation membawa script saat dibuat), `ScriptedSTT.EmitPartial/EmitFinal/Fail`, `rtharness.WithSleep` (tahan pacer di tengah generation), `PipeConn.StallWrites/ReleaseWrites` (klien lambat), `PipeConn.Idle()` benar bila koneksi sudah ditutup; `export_test.go` (`BargeInTotal`, `BargeInStopSamples`, `GenerationExitSamples`, `LLMContentFilteredTotal`, `SetAccepting`, `BargeInBehindTheLoop`); `go.mod`: `prometheus/client_model` jadi dependensi langsung (dipakai tes untuk menghitung sampel histogram).
- **Tes** (`runtime_bargein_test.go`, `runtime_failure_test.go`, plus `stt_state_test.go`, `generation_test.go`): semua tes Step 1 — `TestBargeInConfirmed`, `TestNoStaleFramesAfterInterrupt` (writer macet: 25 frame antre + 1 di tangan; tak ada frame gen lama sesudah `ai.interrupted`), `TestEchoDoesNotBargeIn` (3 sub: tanpa kata sejak onset; partial dari sebelum commit; commit tertunda; masing-masing dengan kontrol positif), `TestClientBargeInHintOnly`, `TestBargeInDuringApology`, `TestBargeInDisabledAfterExpiry`, `TestReplyFailureApology` (LLM dan TTS), `TestFailureAfterFirstAudio16` (dua kalimat terparse, satu tertulis), `TestOpeningFailure12`, `TestThreeFailedTurnsEndSession` (+ reset oleh turn `COMPLETED`), `TestContentFilteredNotCounted` (DONE dan ERROR, ×3, plus tidak mereset streak), `TestNonJapaneseSTTFailsTurn`, `TestTruncatedIsCompleted`, `TestFailedTurnDoesNotChangeCharge`, `TestSTTErrorEndsSessionProviderFailure`; tambahan: `TestFailedTurnEntersRecentTurns`, `TestTTSFallbackWritesProviderFallback`, `TestBargeInKeepsTheFinalWordsSinceTheOnset`, `TestSpokenTextDoesNotWaitForTheLoop` (loop sengaja tertinggal dari generation), `TestMarkTurnFromKeepsTheWordsSinceTheOnset`, `TestSentenceTextIsRecordedBeforeTheLoopReadsIt`.
- **Verifikasi:** `go build ./...`; tes Step 2 + tambahan `-race -count=5` (220 lulus); `go test -race -count=5 ./internal/conversation/...` (2135 lulus); `go test -race ./internal/...` hijau; `make arch-check`; `make lint` (0 isu). Mutasi, semua gagal merah: barge-in tanpa syarat partial (`TestBargeInRules`, `TestEchoDoesNotBargeIn`), frame gen lama lolos (tanpa `activeGen`+`PurgeAudio`), `CONTENT_FILTERED` dihitung, `CONTENT_FILTERED` mereset streak, non-Jepang dihitung, non-Jepang jadi DISCARDED, turn `FAILED` mereset streak, batas off-by-one, barge-in mengabaikan `accepting`, APOLOGY yang dipotong jadi `INTERRUPTED`, `STTError` diabaikan, baris 16 tanpa `ai.interrupted`, hint tak pernah terkonfirmasi, `MarkTurn` menggantikan `MarkTurnFrom`, turn `FAILED` tak masuk `recentTurns`, spoken text dari kalimat terparse (bukan hitungan writer), spoken text/`assistant_text` dari salinan loop, `bargein_stop_seconds` tak diamati. Mutasi `min(…, SentencesWritten)` dari T19 tidak ada lagi (diganti teks dari `Generation` + hitungan writer); tes yang membuatnya bermakna adalah `TestSpokenTextDoesNotWaitForTheLoop` (writer menulis 3 kalimat, loop belum membaca satu pun) dan `TestFailureAfterFirstAudio16`/`TestBargeInConfirmed` (parsed > tertulis).

## M5-T21 — Disconnect, resume pod sama, takeover, replay
- **`internal/conversation/replay.go`** (baru): `replay(entries, lastSeq, knownLast) (missed, mode)` = algoritma §7.10.4 murni (COMPLETE/UNAVAILABLE, deteksi celah lewat `prev_seq`), `withReplayFlag` (envelope asli + `"replay":true`, `server_seq` tetap), `replayMaxEvents=128` (lebih panjang → snapshot saja).
- **`runtime_conn.go`** (baru): `detachConn/detach` = §7.10.1 (koneksi ditutup, generation dibatalkan `ai.interrupted{DISCONNECTED}` dan turn berjalan `FAILED` + `turn.completed{FAILED}` hanya masuk replay stream, `TURN_FAILED{stage:DISCONNECT}`, stream STT ditutup, detector/sttState baru, `reconnectTimer` 30 s dari saat putus terdeteksi, `SESSION_DISCONNECTED{at}`); `settleDisconnected` (floor detik terputus + flush persister sebelum `BeginEnding`, dipakai `endWith`/`fatal` sehingga expiry/RECONNECT_TIMEOUT/fatal saat putus tidak menagih waktu terputus); `onAttach` = §7.10.2 (koneksi lama masih ada → `error` + close 4011 dan diperlakukan putus dari `lastActivity`, `AddDisconnected`, STT baru, replay, `session.snapshot`, `SESSION_RECONNECTED{same_pod:true}`); `takeoverInput`/`beginTakeover`/`runTakeover`/`onTakenOver` = §7.10.3 langkah 6–11 (`FailInProgress` + `TURN_FAILED{TAKEOVER}`, load paralel dalam budget 3 s, `generation=max(rt.generation,maxGen)+1`, `nextTurnSeq=max(maxSeq+1,1)`, `server_seq` lanjut dari `max(rt.last_server_seq, ujung stream)`, adaptive dari `rt`, memctx dari Redis, ring `recentTurns` dan `turn_count` dari Postgres, timer dari `expires_at`).
- **`manager_resume.go`** (baru): `attachResume` (runtime lokal → `resumeSamePod`, selain itu `takeover`), `takeover` (reservasi s.d. `HardMax`, `ClaimRuntime([ACTIVE])`, baca rt/turn/summary/stream/hb paralel 200 ms, `PublishTakeover`, jendela 30 s → `takeoverExpired`: `FailInProgress`, `AddDisconnected`, `BeginEnding(RECONNECT_TIMEOUT)`, `Complete`, `session.ending`+`session.completed`, close 1000), subscriber kanal pod (subscribe sinkron pertama, resubscribe backoff `Deps.PodSubscribeBackoff`=1 s, pesan dengan fence lebih baru → `onFenceLost`). Redis tak terbaca saat takeover → §7.9.4 (waktu terputus 0, generation dari Postgres, replay UNAVAILABLE).
- **Masalah terbuka T18–T20 yang ditutup:** `Manager.resume` bukan seam lagi; `STTClosed` yang tak diminta → `PROVIDER_FAILURE` (runtime berhenti membaca stream sebelum menutupnya sendiri: `dropSTT`, epoch per stream); stage `TURN_FAILED` DISCONNECT/TAKEOVER ada; kolom `session_turns.generation` kini tertulis (`session.TurnFinal.Generation`, `FinalizeTurnFenced` COALESCE, sqlc dibuat ulang, `Runtime.finalize`), jadi takeover tanpa Redis tetap di atas generation pemilik lama.
- **Perubahan di kode lama:** `sttRecv` per stream (Barrier dan event tak lagi bocor lintas stream), `frameEvent.cs` (frame koneksi yang sudah diganti dibuang), `RedisSync.Seq` (rt.last_server_seq/last_durable_seq mengikuti tiap emit), `onFenceLost` menghitung `fence_lost_total` sekali untuk semua jalur, `interruptGenerationWith`, `recentTurn.id`, `View.LastDurable`, opening AI_FIRST yang tertunda saat klien pergi dimainkan setelah resume, `session.end` saat takeover berjalan ditunda seperti saat STARTING.
- **Harness/fake:** `rtharness.H.Reconnect/Peer/Prev` (koneksi baru; pod kedua berbagi clock, fake, Postgres, Redis), `Sync` tidak lagi berhenti di Attach yang selesai; `FailInjector.FailAlways`, `TurnRepo.FenceOf/Hold/Attempts`, `EventWriter.FenceOf/Attempts`, `SessionRuntime.Fence/Status`, `RuntimeStore.Subscribers`; `export_test.go`: `FlushPersister`, `PersisterPending`, `AdaptiveForTest`, `SetEnding`, `HandleTakeover`. Suite repotest sesi memeriksa `Generation` di fake dan PG.
- **Tes:** semua tes Step 1 (`TestReplayAlgorithm`, `TestDisconnectMidTurn`, `TestResumeSamePod`, `TestReconnectTimeout30s`, `TestOldConnClosed4011`, `TestTakeoverFencesOldOwner`, `TestTakeoverBeyondWindow`, `TestTakeoverRedisDown`, `TestResumeRejectedWhenEnding`, `TestResumeAdaptiveFromRT`) plus `TestOldOwnerWritesNothingAfterTakeover` (DB lambat, antrean persister, tulis Redis), `TestTakeoverBaselines`, `TestTakeoverRebuildsTheSessionState`, `TestTakeoverFailsTheTurnLeftInProgress`, `TestDisconnectDuringUserSpeech`, `TestTimersRunWhileDetached`, `TestExpiryWhileDetachedCountsAwayTime`, `TestDisconnectedSecondsAreWholeSeconds`, `TestReplayUnavailableCases`, `TestSTTClosedUnrequestedFailsSession`, `TestResumePlaysThePostponedOpening`, `TestReconnectWindowStartsWhenTheLossIsNoticed`, `TestTakeoverRedisDownAfterARealSession`, dll.
- **Verifikasi:** `go build ./...`; tes T21 `-race -count=5`; `go test -race -count=3 ./internal/conversation/...`; `go test -race ./internal/...`; suite sesi PG (`-tags integration`) hijau; `make arch-check`; `make lint` (0 isu). Mutasi, semua merah: replay tanpa cek celah / `<` / tanpa guard `lastSeq>known` / stream kosong selalu COMPLETE / filter `>=` / tanpa flag replay / tanpa cap; `AddDisconnected` ceil, tanpa AddDisconnected di attach/expiry/RECONNECT_TIMEOUT/takeover lewat jendela; TAKEOVER diabaikan / `>=`; fence lost tanpa `persister.Abort` / tanpa `sync.Abort`; jendela `>=`; generation tanpa maxGen / tanpa rt / tanpa +1; `nextTurnSeq=maxSeq`; known tanpa tip / tanpa rt; putus tanpa gagalkan turn / tanpa tutup STT / tanpa reset / tanpa timer / timer dari lastActivity / disconnected_at=now / generation tak dibatalkan / dihitung ke failedTurns; koneksi lama tak ditutup; resume tanpa STT / tanpa snapshot / tanpa cek ENDING; STTClosed diabaikan; takeover tanpa FailInProgress / adaptive dari snapshot / memori selalu dibangun ulang / opening tak ditunda. Selamat (setara): menjaga `dl.reconnect` setelah resume (`onReconnectTimeout` sudah menolak saat terhubung); `closeTurnForEnd` sebelum `Abort` (antrean langsung dibuang).

## M5-T22 — Akhir sesi: expiry, user end, kegagalan fatal
- **`runtime_end.go`** ditulis ulang. `endWith(reason)` = ACTIVE → ENDING seketika (`BeginEnding` di awal: billing berhenti di sini, apa pun yang dimakan grace/flush), `session.ending{ENDING}` (grace 2 untuk USER_ENDED; expiry 0 bila LISTENING, selain itu `ceil(expires_at+10 s − now)`), lalu `driveEnd` memberi turn berjalan yang diizinkan §7.11.2/§7.11.3: USER_SPEAKING (expiry: detector jalan sampai turn berakhir; user end: `forceEndUser` = Finalize + tunggu final ≤ min(1,5 s, sisa grace), `stt.final.end_reason=SESSION_ENDING`), menunggu final (timer STT_FINAL), PROCESSING (generation dibatalkan `ai.interrupted{SESSION_ENDING}`, turn COMPLETED user-saja / turn 0 INTERRUPTED), AI_SPEAKING (expiry: `LLMCancel`, kalimat terparse tetap diputar, turn ditutup oleh playback_finished/deadline playback: COMPLETED bila LLM selesai normal, selain itu INTERRUPTED; user end: dipotong), APOLOGY (expiry: diputar sampai habis, selain itu dipotong, tetap FAILED). `dl.grace` (`expires_at+SESSION_EXPIRY_GRACE`, atau now+2 s) memotong sisanya (`cutTurnForEnd`). `advanceEnd` (dipanggil loop setelah tiap input) memulai `finish` begitu tak ada turn. `finish`/`fatal` memakai `flushThen`: flush persister di goroutine dengan deadline clock runtime (`ioTimeout` 5 s, digerakkan tick/fake clock, loop tak terblokir), lalu `Complete`/`Fail` di loop; flush gagal/timeout tetap settle. `fatal` mengikuti urutan §7.11.4 dan mengirim `session.ending{ENDING}`; fatal setelah ekor akhir dimulai diabaikan. `checkDeadlines` dipecah per status (ENDING: hanya grace, playback, sttFinal; tanpa `balance.tick`/peringatan).
- **Perubahan kode lain:** `resolveUserText` (akhir sesi → `completeUserOnly`, tanpa jawaban/`ReportFinalTimeout`), `onGenFailed` saat berakhir → `cutTurnForEnd` (tanpa APOLOGY/hitungan gagal), `STTError` saat berakhir diabaikan, `closeTurn/discardTurn/completeFailedTurn` tanpa commit S-8/`postTurn` saat berakhir, `playedOutStatus`, watchdog starvation aktif juga saat grace (`hearsUser`), `onFenceLost` → 4010 `SESSION_NOT_RESUMABLE` bila status sesi terminal (baca `GetForAttach`; pesan TAKEOVER tetap 4011), `settleBargeIn` dipanggil sebelum flush akhir (baris `BARGE_IN` + `bargein_stop_seconds` tak hilang), `recoverLoop` menyelesaikan akhir secara sinkron (`syncEnd`).
- **Seam tes dihapus:** `SetAccepting`, `SetEnding`. `TestBargeInDisabledAfterExpiry` (kini dengan kontrol sebelum expiry) dan `TestResumeRejectedWhenEnding` memakai grace expiry nyata. Seam baru: `Runtime.PanicOnLoop` (export_test).
- **Tes (`runtime_end_test.go`):** semua tes Step 1 (`TestWarnings`, `TestBalanceTick10s`, `TestExpiryListening`, `TestExpiryAISpeaking`, `TestExpiryUserSpeaking`, `TestGraceDeadlineHard`, `TestUserEnd`, `TestBillingStopsAtEnding`, `TestFatalSTTFailure`, `TestPanicInGoroutine`, `TestPersisterStuckFatal`, `TestNoGoroutineLeakAfterSession`) plus `TestExpiryProcessing`, `TestExpiryWaitingForFinal`, `TestNoBalanceTickInTheGrace`, `TestPanicInTheLoop`, `TestBargeInRowSurvivesTheEnd`, `TestFenceLostTellsTheTerminalStatus`, `TestDetachedInTheGraceSettlesAtTheDeadline`.
- **Verifikasi:** `go build ./...`; tes T22 `-race -count=5`; `go test -race -count=2 ./internal/conversation/...`; `go test -race ./internal/...` (1746); `make arch-check`; `make lint` (0 isu). Mutasi, semua merah: balance.tick di ENDING, tanpa tunggu flush, grace ±1 s, `Complete` ganda, exit tanpa cancel (leak), accepting tetap on, grace deadline diabaikan, LLM tak dipotong saat expiry, tanpa settle BARGE_IN, selalu 4011, status akhir selalu COMPLETED, jawab user setelah expiry, `ReportFinalTimeout` saat berakhir, grace user end 3, `session.ending` fatal hilang, USER_SPEAKING dipotong saat expiry, tanpa starvation di ENDING, AI dipotong saat expiry, Fail tanpa BeginEnding, tanpa `syncEnd`. Selamat: klem `sttFinal` ke grace (setara; dihapus, deadline grace sudah memotong) dan `onGenFailed` saat berakhir (tak ada skenario alami: expiry membatalkan LLM, kegagalan TTS tak bisa dipicu di jendela itu tanpa fake baru).

## M5-T23 — Manager: admission, kapasitas, drain
- **`manager.go`** (registry, kapasitas, podinfo) dan **`manager_attach.go`** (Attach langkah 7–11, tabel §7.3.2, `startRuntime`) berasal dari pemecahan `manager.go` T18 (kode dipindah apa adanya). Baru: `ActiveSessions()` (ganti nama `Active()`; runtime registry + slot start berjalan = `sessions_active` dijumlah), `Ready()` (`!draining && active < ReadyMax`), `Admission()` (`SERVER_DRAINING` lebih dulu, lalu `SERVER_AT_CAPACITY` pada `≥ HardMax`; hanya membaca registry, tanpa Redis/Postgres), `Draining()`, `PodInfoLoop(ctx)` (podinfo sekarang dan tiap 5 s, TTL 15 s; gagal tulis hanya WARN). `reserveUpTo` kini mengembalikan kode penolakan (draining/capacity) dan menolak saat draining; `register` melaporkan bila drain mulai di antara reserve dan register sehingga runtime itu tetap diberi tahu. `Attach` menolak 4100 pada pod draining tanpa runtime lokal sebelum membaca sesi; start/takeover yang lolos ke `reserve` ditolak 4100 (`connections_total{rejected_draining}`); resume pada runtime yang sedang handoff dijawab 4100, bukan 4010.
- **`manager_resume.go`**: subscriber pod kini `StartSubscriber(ctx)` (langganan pertama sinkron, sekali jalan, berakhir dengan `ctx` atau `Close`), bukan efek samping `NewManager`.
- **`drain.go`**: `Manager.Drain(ctx)` (draining=true, podinfo `draining=1`, `SetDraining`, ctl DRAIN ke tiap runtime, menunggu pod kosong termasuk slot start berjalan lewat `changed`, `drain_duration_seconds`, `ctx` habis → error). Sisi runtime: `onDrain`, `advanceDrain` (dipanggil loop setelah tiap input: ACTIVE, tidak ending/takeover; terlepas → handoff langsung; terhubung → handoff di safepoint `LISTENING` tanpa turn/gen; `dl.safepoint` dipasang pada pemeriksaan ACTIVE pertama), `onSafepointTimeout` (20 s: `failTurnForDetach` + `resetListening`, turn `FAILED` `SERVER_DRAINING`), `beginHandoff` (`accepting=false`, flush persister di goroutine dengan deadline clock `DRAIN_FLUSH_TIMEOUT`), `onHandoffFlushed` (langkah 2–6 §7.12.1: `redisSync.Flush` dengan rt `connected=0, disconnected_at`, `MarkHandoff` (gagal → WARN), `OWNER_CHANGED{DRAIN_RELEASE, to_pod:null}`, `Release`, `error{SERVER_DRAINING}`, close 4100; sesi tetap ACTIVE). STARTING menyelesaikan start dulu; ENDING selesai normal lewat `finish` (jawaban catatan T22: drain menunggunya, tidak men-handoff); end yang mulai saat flush handoff membatalkan handoff.
- **Perubahan kecil lain:** `Deps.Build` (podinfo), default `DrainSafepointTimeout`/`DrainFlushTimeout`, `ownerChanged.ToPod` menjadi `*string`, `View.Draining/Handoff`, `exit()` tidak me-`Release` ulang setelah handoff, `resumable()` false saat handoff.
- **Harness/fake:** `rtharness.WithApology`, `Mgr.StartSubscriber` dipanggil di `New`/`Peer`; `fakes.SessionRuntime.Hold/Blocked` (GetForAttach, ClaimRuntime, MarkStarting, Activate, MarkHandoff); `export_test`: `Manager.HoldSlots`, `Metrics.SessionsActiveTotal/DrainingGauge/DrainDurationSamples`.
- **Tes:** `manager_test.go` (`TestAttachStatusTable`, `TestCapacity`, `TestAdmissionBeforeTicket`, `TestOneConnectionPerSession`, `TestRedisDownAttach`, plus `TestActiveSessionsMatchesTheGauge`, `TestStartSubscriber`, `TestPodInfoLoop`), `drain_test.go` (`TestDrainLISTENING`, `TestDrainWaitsSafepoint`, `TestDrainDetachedImmediate`, `TestDrainStartingFinishesFirst`, `TestDrainEndingFinishesNormally`, `TestResumeAfterDrainOnOtherManager`, plus `TestDrainHandsOffWhenTheTurnEnds`, `TestDrainFlushesBeforeHandoff`, `TestDrainFlushTimeout`, `TestDrainWaitsForAPodThatIsStillEnding`, `TestResumeAfterDrainFailsOnlyTheTurnThatRan`, `TestDrainingPodRefusesNewWork`, `TestResumeDuringHandoffIsToldToGoElsewhere`, `TestDrainCoversAStartInFlight`, `TestDrainStopsWithItsContext`), dan `TestGenerationFailsWhileEnding` (jalur `onGenFailed` saat berakhir, catatan T22) di `runtime_end_test.go`.
- **Verifikasi:** `go build ./...`; tes T23 `-race -count=5` (275 lulus); `go test -race ./internal/...` (1800); `make arch-check`; `make lint` (0 isu). Mutasi, semua merah: ambang start `>`/ready `<=`/hard max `>`, admission mengabaikan draining / kapasitas didahulukan, ready mengabaikan draining, reserve mengabaikan draining, register tak memberi tahu drain, Drain tak menunggu slot pending, handoff tanpa safepoint, onDrain langsung memotong turn, handoff menandai turn FAILED (juga saat LISTENING), handoff sebelum flush, safepoint 19 s, flush timeout 4 s, tanpa MarkHandoff, alasan OWNER_CHANGED salah, owner tak dilepas, close 4101, sesi ENDING di-handoff, `disconnected_at` ditimpa untuk sesi terlepas, rt tetap connected, tanpa `error{SERVER_DRAINING}`, tanpa podinfo draining, resumable saat handoff, resume saat handoff dijawab 4010, `ctx` Drain diabaikan, STARTING tidak retryable, `onGenFailed` saat berakhir lewat jalur biasa, podinfo salah (draining/jumlah), subscriber tidak idempoten. Tidak ada yang selamat.

## M5-T24 — `wsgateway`: handshake, adapter Conn, redaksi ticket
- **`internal/wsgateway/handler.go`**: `Handler` (`http.Handler` mentah) menjalankan langkah 1–6 §7.3.1 berurutan. 1 Origin (exact scheme+host+port, huruf kecil, `/` akhir dibuang; tanpa Origin hanya bila `AllowEmptyOrigin`) → 403 `ORIGIN_NOT_ALLOWED`; 2 `ws_connect_ip` (subjek `httpapi.ClientIP` dengan `TrustedProxies`) → 429 + `Retry-After` (dibulatkan ke atas, minimal 1) + `RATE_LIMITED`; 3 `Mgr.Admission()` → 503 `SERVER_DRAINING`/`SERVER_AT_CAPACITY`, `Retry-After: 1`, `Set-Cookie: kotoki_rt=; Max-Age=0; Path=/api/v1/realtime; Secure; HttpOnly` (string persis), metrik `connections_total{rejected_draining|rejected_capacity}`; 4 `websocket.Accept` (subprotocol `kotoki.v1`, kompresi mati, `OriginPatterns` dari host allowlist, `SetReadLimit(WS_MAX_JSON_BYTES)`); 5 `Tickets.Consume` → `auth.ErrTicketInvalid` = error event + close 4001, error lain (Redis) = langkah 5b, 4503 `SERVICE_UNAVAILABLE` (`rejected_unavailable`); 6 `ws_attach_user` (subjek user id dari ticket) → 4029; lalu `connections_total{accepted}` dan `Mgr.Attach`. Error HTTP lewat `httpapi.WriteError` (header ditulis dulu ke `w`). Limiter yang error = fail-open (seperti `httpapi.RateLimit`). Penolakan setelah upgrade: event `error` (server_seq 1) lalu close dengan reason = code, dibatasi `WS_WRITE_TIMEOUT`, metrik `ws_close_total`. Gateway tidak mengimpor go-redis/redisstore (depguard `handler-thin` lolos).
- **`conn.go`**: `NewConn(*websocket.Conn) conversation.Conn` (kind teks/biner, `Ping` library, `Close(code, reason)` dengan reason dipotong ≤ 123 byte di batas rune). **`redact.go`**: `RedactQuery` mengganti nilai tiap parameter `ticket` (kunci dibandingkan setelah percent-decode, tanpa beda huruf) menjadi `REDACTED`, parameter lain utuh; log handler selalu memakainya (`path`, `query`).
- **`go.mod`**: `github.com/coder/websocket v1.8.15` (langsung).
- **Tes** (`handler_test.go`, `conn_test.go`, `redact_test.go`, `helpers_test.go`; `Manager` asli dari `rtharness`, `httptest`, klien `coder/websocket`): `TestOriginRejected` (7 varian, ticket utuh lalu dipakai), `TestOriginAllowed`, `TestEmptyOriginPolicy`, `TestRateLimitIP`, `TestRateLimitIPPolicyAndSubject`, `TestLimiterErrorFailsOpen`, `TestDrainingBeforeTicket` (ticket yang sama dipakai di gateway lain → upgrade sukses), `TestAtCapacityBeforeTicket`, `TestStepOrder`, `TestTicketInvalidCloses4001`, `TestTicketRedisError4503`, `TestAttachRateLimit4029`, `TestAttachRateLimitNeedsTheTicketFirst`, `TestHandshakeReachesAttach`, `TestSubprotocolEcho`, `TestReadLimit`, `TestRequestLogNeverHoldsTheTicket`, `TestNilOptionalDependenciesHaveDefaults`, `TestRedactTicket`, `TestConnAdapter` (7 subtes).
- **Verifikasi:** `go build ./...`; `go test ./internal/wsgateway/... -race -count=5` (coverage 96,9%); `go test -race ./internal/...` (1842 lulus); `make arch-check`; `make lint` (0 isu). Mutasi, semua merah: ticket dikonsumsi sebelum origin / limiter IP / admission / upgrade, limiter sebelum origin, admission sebelum limiter, admission dilewati, empty origin selalu/tidak pernah diizinkan, origin tanpa scheme / prefix match, cookie tanpa `Secure` / hilang, `Retry-After` salah / hilang / tanpa pembulatan / bisa 0 / admission 2, kapasitas dihitung sebagai draining, tanpa/`+1` read limit, kompresi aktif, tanpa subprotocol, error Redis dipetakan 4001, ticket tak valid dipetakan 4503, limit attach dilewati / per sesi / policy salah, policy IP salah, limiter error fail-closed, ticket cacat sampai store, log query mentah, redaksi no-op / hanya pertama / peka huruf / prefix, tanpa event error, close code salah, tanpa metrik `accepted`, reason tidak dipotong / dipotong di tengah rune, kind biner jadi teks, tulis selalu teks, close tanpa code / reason, ping no-op. Tidak ada yang selamat.

## M5-T25 — Wiring `cmd/realtime`, `/readyz`, tracing
- **`cmd/realtime/main.go`**: `run` dirangkai manual (D-004), mengikuti `cmd/api`: `config.LoadRealtime` → log/otel → `pgxpool` (default 6 koneksi) → Redis → `buildRealtime` → ops listener (`OPS_ADDR`, `:9091`) → listener publik (`HTTP_ADDR`, `:8081`, diikat sebelum readiness) → startup check → `Serve`. `buildRealtime`: `postgres.NewStore` + `balance.NewService` + `session.NewService` (`Control: redisstore.NewSessionControl`, `ServiceName: "realtime"`) + `session.NewRuntimeService`, `postgres.NewTurnRepo/NewEventRepo` (+ `conversation.NewSessionEventWriter`), `content.NewSessionConfigLoader(postgres.NewContentRepo)` di balik `conversation.NewConfigLoader`, `redisstore.NewRealtimeState/NewRateLimiter/NewTicketStore/NewFakeFaults`, router provider, `conversation.NewManager` (Limits dari `REALTIME_*_MAX_SESSIONS`, `ProviderSessionGrace`/`STTFinalTimeout` dari `RouterConfig`, `Build` = versi), lalu `StartSubscriber`, `go PodInfoLoop`, `go faults.Run` pada `podCtx` (hidup sampai Manager tertutup) dan `wsgateway.Handler` di `GET /api/v1/realtime` (Origins, AllowEmptyOrigin, TrustedProxies, Limiter, Tickets, Metrics, IDs, Clock).
- **SIGTERM** (spec §7.12.1): `ops.SetDraining` → `Mgr.Drain` (≤ 30 s; gagal/kehabisan waktu hanya WARN) → `Mgr.Close` → `wsSrv.Shutdown` (5 s, `Close` bila gagal) → stop pod ctx dan ops → exit 0. Listener publik tetap menjawab selama drain agar koneksi baru mendapat 503 `SERVER_DRAINING` (bukan connection refused); ops hidup sampai terakhir seperti `cmd/api`.
- **`cmd/realtime/ops.go`**: `opsWrap(pod)` (dipasang lewat `health.OpsOptions.Wrap`): `/readyz` 503 `{"status":"draining"|"at_capacity"}` selama `!Mgr.Ready()` (di atas state starting/draining milik `health.Ops`), semua request ops dibungkus `otelhttp.NewHandler` (`go.opentelemetry.io/contrib/.../otelhttp` kini dependensi langsung di `go.mod`).
- **`cmd/realtime/providers.go`**: `buildRouter` (parse env → `providerwire.BuildFake` dengan `caps = {STT, LLM, TTS, EMBEDDING}`, `RoleRealtime`, `redisstore.NewFakeFaults` → `provider.NewRouter`), `noopMemory` (`memory.RetrievalService` kosong) dan `staticLearner` (`LearnerContext{}`).
- **`cmd/internal/providerwire/fake.go`**: `ParseConfigs()` (`provider.RouterConfig` + `fake.Config` dari env), `BuildFake(cfg, fakeCfg, caps, Deps)` mengisi `RouterDeps` penuh (`FAKE_PRIMARY` per kapabilitas, `FAKE_FALLBACK` hanya TTS, `Prices` via `pricing.Load(cfg.PricesFile)`, `Metrics`, `Role`, `Clock`, `Log`, `Faults`); `FAKE_<INST>_<CAP>_*` dibaca lewat `fake.LoadCapConfig`; mode `real` untuk kapabilitas yang diminta → `ErrRealModeUnavailable` ("adapter vendor tersedia di M9").
- **Tracing `ws.connection`** (`internal/wsgateway/handler.go`): span dibuka di awal `ServeHTTP` lewat `otel.Start` (penolakan sebelum upgrade pun terlacak) dan ditutup `otel.End`; atribut `session.id` (setelah ticket), `conn.id` dan `resume` (diisi `Manager.Attach`/`dispatchFirst` lewat `otel.SetString/SetBool` pada span di ctx), `close.code` (diisi adapter `conn`: close code pertama yang dikirim server atau diterima dari peer). `platform/otel` mendapat `SetString/SetBool/SetInt` (atribut lewat redaksi yang sama dengan `Start`). Ticket tidak pernah masuk atribut (tes).
- **Perubahan pendukung:** `health.OpsOptions.Wrap` dan `ShutdownTimeout`; `Ops.Serve` tidak lagi gagal bila `Shutdown` kehabisan waktu karena koneksi yang tidak pernah mengirim request (dial cadangan klien HTTP): koneksi sisa ditutup dan `Serve` mengembalikan nil. Bug ini muncul sebagai exit 1 `context deadline exceeded` sesudah SIGTERM pada 1 dari ~10 run `TestM5BootIntegration`, dan sama-sama ada di `cmd/api`/`cmd/worker` (kini ikut tertolong). `config.Realtime.TrustedProxyCIDRs` (`TRUSTED_PROXY_CIDRS`, divalidasi seperti di API) untuk subjek `ws_connect_ip`. `Makefile`: `run-realtime` bergantung `fixtures16k` dan `CGO_ENABLED=1`; `build` hanya membangun `cmd/kotoki-cli` bila direktorinya ada (baru ada di Task 28). `.env.example`: dokumentasi env listener/kapasitas realtime (komentar). `deploy/docker/realtime.Dockerfile`: `EXPOSE 8081 9091`.
- **Tes:** `cmd/internal/providerwire/fake_test.go` (`TestBuildFakeFillsFullRouterDeps` untuk `RoleRealtime` dan `RoleWorker` dengan `router.Embed` 1536 dim, kapabilitas tak diminta nil, mode real ditolak per kapabilitas, variabel `FAKE_*` salah menyebut namanya); `cmd/realtime/ops_test.go` (`TestReadyzThresholds`, `TestOnlyReadyzIsGatedByThePod`, `TestReadyzFollowsTheManager` dengan Manager asli yang di-drain, `TestReadyzStillStartsBeforeTheChecksPass`, `TestRealtimeRejectsRealModeInM5`, `TestBuildRouterInFakeMode`); `cmd/realtime/main_integration_test.go` (env fake, `TestRunRefusesRealModeBeforeListening`); `test/integration/m5_realtime_boot_test.go` (`TestM5BootIntegration`: binary asli dibangun cgo dan dijalankan dengan fixture 16 kHz, `/readyz`/`/healthz`/`/metrics`, Origin salah → 403 `ORIGIN_NOT_ALLOWED`, ticket palsu → event error + close 4001 lewat Redis, podinfo `draining=0` lalu SIGTERM → exit 0 < 30 s dan podinfo `draining=1`; `TestM5RealtimeRefusesRealMode`); `internal/wsgateway/trace_test.go` (span penolakan, penolakan sebelum upgrade, start lalu resume dengan close code 4321 dari peer, tanpa ticket di atribut); `platform/otel` (`SetString/SetBool/SetInt`), `health` (`TestServeClosesAConnectionThatNeverSentARequest`), `config` (CIDR proxy realtime, origin kosong di prod).
- **Verifikasi:** `go build ./...`; `go test ./cmd/realtime/... -race`; `go test -tags integration ./test/integration -race -run M5Boot` (juga 6 proses paralel dan 8 run berurutan tanpa gagal setelah perbaikan `Serve`); `go test -tags integration ./cmd/... ./internal/postgres/... -race`; `make build`; `make arch-check`; `go test -race ./internal/... ./cmd/...`; `make lint` (0 isu, juga dengan `-tags integration`). Mutasi, semua merah: gerbang `/readyz` dilepas, `conn.id`/`session.id`/`resume`(true dan false) tak diisi, close code dari `Close` dan dari peer tak dicatat, tanpa `FAKE_FALLBACK` TTS, tanpa Evaluation, `Drain` dilewati (podinfo `draining=1` hilang), toleransi `DeadlineExceeded` di `Serve` dilepas. Tidak ada yang selamat setelah dua tes ditegaskan (`resume` wajib ada, close code peer 4321).

## M5-T26 — `kotokiclient`: REST, WS, audio, pacer
- **`internal/kotokiclient/client.go`, `types.go`**: `Client` (`New(baseURL, opts...)`; opsi `WithHTTPClient/WithClock/WithIDs`; cookie jar untuk cookie refresh, access token di memori + `AccessToken/SetAccessToken`), `Register/Login/Refresh/Catalog/CreateSession/CancelSession/RealtimeTicket/GetSession/GetEvaluation/Balance/Turns`. `CreateSession` selalu mengirim `Idempotency-Key` (UUID dari `idgen`, atau `CreateSessionRequest.IdempotencyKey`). Galat non-2xx menjadi `*APIError{Status, Code, Message, RequestID, Details}` (body D-013; body non-JSON tetap `APIError` dengan status). DTO disalin dari `httpapi` (tanpa impor); `Catalog` = tiga GET `/topics`, `/scenarios`, `/characters`.
- **`realtime.go`**: `Dial(ctx, wsURL, ticket, origin, ...DialOption)` (ticket di query, subprotocol `kotoki.v1`, kompresi mati, error dial tidak memuat URL/ticket; penolakan HTTP pra-upgrade menjadi `*DialError`), `Send` (isi `version/event_id/ts` bila kosong), `SendAudio` (`protocol.EncodeUpstream`, seq otomatis dari 0 di bawah mutex, paket 1..4091 B), `Events()/Audio()`, `Done()/Err()` (close code lewat `websocket.CloseError`), `Close`. Satu goroutine pembaca memutuskan frame lama dalam urutan stream: generasi aktif mengikuti `ai.generation_started` (envelope), `ai.interrupted.active_generation`, `session.snapshot.active_generation`; frame `gen <` aktif dibuang (`StaleDropped()`). Event bertipe tak dikenal/cacat diabaikan (forward compat).
- **`pacer.go`**: `NewPacer(start, now, sleep)` (jadwal absolut `start + n×20 ms`, n = 1, 2, …; tertinggal = tidak tidur sampai terkejar; tanpa `time.Now`), `NewClockPacer(start, clock.Clock)`, `SleepContext`. **`report.go`**: tipe `Report`/`ServerLatency` (JSON = `summary.json`); logikanya di Task 27.
- **`internal/kotokiclient/audio`**: `ReadWAV` (PCM16, downmix rata-rata, resample linear ke 48 kHz), `WriteWAV`, `PreEncode` (Opus 48 kHz, 960 sampel, VoIP, 32 kbps, complexity 5, sisa dipad nol), `SilencePacket`, `Decoder` (48 kHz, hanya 20 ms). **`internal/conversation/audio/playback.go`** (baru): `PlaybackDecoder` 48 kHz, satu-satunya cara klien men-decode tanpa melanggar depguard cgo.
- **Tes**: `pacer_test.go` (`TestPacerNoDrift`, `TestPacerExactSchedule`, catch-up, ctx, `SleepContext`, `NewClockPacer`), `client_test.go` (`TestRESTRoundTrip`, `TestCreateSessionKeys`, `TestErrorDecoding`, `TestBadBaseURL`, `TestCatalogAndReads`, `TestTurnsPagination`), `realtime_test.go` (`TestDialHandshake`, `TestDialRefusedBeforeUpgradeKeepsTheTicketOut`, `TestSendAudioHeader` (seq 0,1,2 dan 256 big-endian), `TestSendAudioPacketBounds`, `TestSendFillsEnvelope`, `TestDropsStaleGeneration`, `TestFrameOrderAgainstEventsIsTheServers`, `TestReceivedTimestampsFollowTheClock`, `TestUnknownEventsAreIgnored`, `TestCloseSendsCodeAndStopsTheReader`, `TestCloseUnblocksAReaderStuckOnAFullBuffer`), `report_test.go`, `audio/audio_test.go` (`TestReadWAVResamples`, `TestPreEncodeShape`, downmix, round-trip WAV, silence, decoder), `conversation/audio/playback_test.go`.
- **Verifikasi:** `go test ./internal/kotokiclient/... -race -count=5`; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint`. Mutasi, semua merah (22): pacer relatif (drift)/tanpa maju/geser satu slot, frame lama lolos, gen sama dibuang, `interrupted`/`snapshot`/`generation_started` diabaikan, seq tak naik/geser satu, batas paket, ticket salah, Origin hilang, tanpa `Idempotency-Key`, tanpa cookie jar, kunci cursor salah, token login tak disimpan, downmix tanpa pembagian, tanpa resample, frame parsial dibuang, silence tidak senyap, header rate WAV salah. Tidak ada yang selamat.


## M5-T27 — `kotokiclient`: skrip percakapan & Report
- **`steps.go`**: `Step` (tertutup) + `Speak{Packets, Label}`, `WaitAIDone{Timeout}`, `Think{D}`, `BargeIn{After, Packets, Timeout}`, `Drop{For}`, `End{Timeout}`. **`sink.go`**: antarmuka `Sink{OnEvent(Received[protocol.ServerEvent]); OnAudio(AudioFrame)}`, `NopSink`, `SinkFuncs`, `MultiSink`, dan `AudioCollector` (mengelompokkan frame Opus per generation berlabel `turn_seq` dari `ai.generation_started`; `Generations()` terurut; dipakai CLI untuk `turn-<seq>-gen-<generation>.wav`). **`converse.go`**: `SessionSpec{SessionID, WSURL, Origin, Client, Clock, Sleep, IDs, HandshakeTimeout}`, `RunConversation`, `ServerError`, `ErrAITimeout/ErrNoAIAudio/ErrEndTimeout`, `Default*Timeout`. **`realtime.go`**: `RealtimeConn.Abort()` (tutup transport tanpa close handshake).
- **Alur:** tiket → `Dial` → tunggu `session.ready` → `session.start` → tunggu `session.started`/`snapshot`; sejak itu satu frame tiap 20 ms (jadwal absolut `Pacer`; ucapan atau `SilencePacket`) dari goroutine pemanggil, satu goroutine konsumen per koneksi (event + frame, dipanggilkan ke `Sink` berurutan). Tenggat langkah (`Think`, timeout `WaitAIDone`, `After`) memakai `Clock` yang disuntik, bukan timer nyata. `Drop`: `Abort` → tunggu konsumen selesai (semua yang sudah diterima terhitung) → `Sleep(For)` → tiket baru → `session.resume{last_server_seq}` → tunggu `session.snapshot` → pacer baru; urutan seq upstream mulai 0 lagi. Sesi selesai sendiri (`session.completed/failed`, mis. `wait-expiry`) mengakhiri skrip tanpa galat; `error` fatal → `*ServerError`; koneksi putus di luar `Drop` → galat. `Report` selalu dikembalikan (juga bila galat); slice tidak pernah `null`.
- **Pengukuran (cap waktu `Received.At`/`AudioFrame.At` dari jam yang sama):** `connect_ms` (sekitar `Dial`, koneksi pertama), `start_ms` (`session.start` dikirim → `session.started`), `turn_latency_ms` (frame ucapan terakhir dikirim → frame pertama generation > generation tertinggi yang terlihat saat itu; frame generation lama tidak dihitung), `barge_in_stop_ms` (frame pertama penyela dikirim → frame terakhir generation lama; final saat `ai.interrupted` live dengan `active_generation` lebih besar; frame yang ditulis server sebelum event diproses lebih dulu), `resume_ms` (sejak rekoneksi dimulai, setelah `For`, termasuk tiket+dial → `session.snapshot`), `server_latencies` (dari `turn.completed.latency`), `charged_seconds`/`status` (`COMPLETED`/`FAILED` dari event terminal, selain itu `ACTIVE`).
- **`last_server_seq`:** terbesar yang terlihat (event durable maupun transient); `session.snapshot` menggantinya dengan `server_seq_baseline` meski lebih kecil (§7.4.3). Event `replay:true` dengan `server_seq ≤ last_server_seq` dianggap duplikat (ke `Sink` tetap, ke `Report`/state tidak); yang belum pernah dilihat dihitung seperti event live.
- **Tes (`converse_test.go`, jam fake, tanpa tidur nyata):** server palsu in-process bergerak serempak (lock step): setelah tiap frame upstream server menjawab sesuai skrip lalu mengirim penanda `pong` (seq 0); sleep Pacer menunggu penanda itu sebelum memajukan jam 20 ms, jadi seluruh waktu terima tepat. `TestRunConversationHappy`, `TestBargeInStepMeasured`, `TestBargeInWithoutAudio`, `TestTurnLatencyUsesTheNewGenerationOnly`, `TestDropAndResume`, `TestSnapshotResetsBaseline`, `TestAlwaysSendsFramesEvery20ms` (50 frame untuk `Think{1s}`, tiap frame persis +20 ms, seq berurutan, paket silence), `TestWaitAIDoneTimesOut`, `TestSessionEndingOnItsOwnEndsTheScript`, `TestFatalServerErrorIsReturned`, `TestResumeRefusedFails`, `TestRunConversationRejectsBadInput`, `TestConsumeHandlesFramesBeforeALaterEvent` (urutan frame vs event), `TestAudioCollectorGroupsByGeneration`, `TestSinkHelpers`; `TestAbortDropsTheTransportWithoutCloseHandshake` di `realtime_test.go`.
- **Verifikasi:** `go test ./internal/kotokiclient/... -race -count=5` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` (1926 lulus); `make arch-check`; `make lint` (0 isu). Mutasi, semua merah (21): latensi dari frame ucapan pertama, baseline generation 0, snapshot tidak mereset baseline, duplikat replay terhitung, semua replay diabaikan, interval 30 ms, tanpa menguras frame sebelum event, `resume_ms` dari sebelum `For`, `Drop` dengan close handshake, lantai `turn_seq` 0, tanpa `playback_finished`, t0 barge-in di frame terakhir, tanpa `After`, resume mengirim 0, `start_ms`/`connect_ms` bergeser, idle tanpa silence, `last_server_seq` tidak maju, event terminal diabaikan. Tidak ada yang selamat.


## M5-T28 — `cmd/kotoki-cli`
- **`cmd/kotoki-cli/main.go`**: `run(ctx, args, out, errw, getenv)` (exit 0 sukses, 1 gagal, 2 salah pakai), flag standar `flag` tanpa cobra. Flag global `--api-url/--ws-url/--origin/--json/--timeout` (env `KOTOKI_CLI_*`; default persis §14.11.2) dapat diberikan sebelum maupun sesudah subcommand. `cliClient` membungkus `kotokiclient.Client` dengan kredensial tersimpan; `call` me-refresh sekali pada 401 lalu mengulang, `persist` menyimpan token yang berotasi.
- **`creds.go`**: `$XDG_CONFIG_HOME/kotoki-cli/credentials.json` (fallback `~/.config`), dir 0700, file 0600 (ditulis atomik lewat temp+rename; file yang lebih terbuka dikencangkan saat dipakai), satu entri per `api-url` (garis miring akhir diabaikan), isi `{access_token, refresh_token, email}`. JSON rusak dilaporkan dan tidak ditimpa.
- **`cmd_auth.go`**: `register` (terms dari `GET /auth/terms` bila `--terms-version` kosong), `login`, `catalog`, `balance`. **`cmd_session.go`**: `session create|get|cancel|transcript|end`, `ticket`, `evaluation get --wait`. `session create` mencetak `session_id` saja (mode `--json`: objek sesi). `session end` = WS singkat `session.resume` → `session.end{USER_ENDED}` → tunggu `session.completed/failed` (menolak sesi RESERVED dengan petunjuk `session cancel`). **`cmd_converse.go`** (`converse`, alias `chat`): semua flag tabel §14.11.2; `buildSteps` (murni, diuji) menyusun skrip; keluaran `turn-<seq 3 digit>-gen-<gen>.wav` (PCM16 48 kHz), `events.jsonl` (`{received_at, event}` per baris), `summary.json` (`session_id` + `Report`); mode `--json` mencetak event (`kind:"event"`) dan ringkasan (`kind:"summary"`) sebagai JSON lines.
- **`kotokiclient`** (tambahan minimal): `Client.RefreshToken()/SetRefreshToken()` (cookie refresh direkam dari header `Set-Cookie` dan dapat dipasang kembali ke jar), `Client.Terms()`. Tes: `TestRefreshTokenExportAndImport`, `TestTerms`.
- **Tes (`cli_test.go`, server palsu REST+WS in-process, waktu nyata dengan WAV 200-400 ms)**: `TestFlagDefaults`, `TestFlagEnvAndPrecedence`, `TestGlobalFlagsWork...`, `TestCredentialsFileMode0600`, `TestCredentialsKeyedByAPIURL`, `TestRefreshesOn401AndStoresTheRotation`, `TestRefreshFailureAsksForLogin`, `TestCorruptCredentials...`, `TestConverseRequiresWav`, `TestConverseOutputs` (tiga keluaran, WAV 48 kHz valid, events.jsonl per baris ber-timestamp, summary memuat `turn_latency_ms`, mode file 0600), `TestJSONOutputMode`, `TestRegisterUsesTermsVersion`, `TestConverseAIFirstWaitsForTheGreeting`, `TestConverseDropAndResume`, `TestSessionEndOverWebSocket`, `TestSessionEndRefusesAReservedSession`, `TestEvaluationGetWaitsUntilCompleted`, `TestSessionTranscriptPages`, `TestBuildSteps*`, `TestInsertDrop`, `TestParseEnd`, dll. (45 tes).
- **Verifikasi:** `go test ./cmd/kotoki-cli/... -race -count=3` hijau; `make cli && ./bin/kotoki-cli --help` sukses; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` (0 isu). `make build` sudah membangun CLI bila direktorinya ada (T25). Mutasi (33), semua merah setelah dua tes tambahan: mode file kredensial/direktori/pengencangan, kunci per `api-url` (konstan, tanpa normalisasi), tiap default flag global dan converse (termasuk nama env), format `turn-%03d`, laju WAV, `received_at`, mode file keluaran, `AI_FIRST` tanpa `WaitAIDone`, terms tanpa lookup, tanpa retry 401, `session end` tanpa cek `expected_command`/alasan salah, validasi flag sesi dilewati, `--wav` wajib dilewati, `--drop-after` mati, `wait-expiry`, penanda `kind` JSON, judul ringkasan.

## M5-T29 — Harness e2e + E1, E2, E6, E11 + SLO server-side
- **`test/e2e/`** (tag `e2e`): `main_test.go` (`TestMain`; shim `mainTB` agar helper `testutil` jalan tanpa `*testing.T`; log subprocess dicetak bila run gagal), `env_test.go` (PG/Redis testcontainers, migrate + `seed/*.sql`, fixture 16 kHz lewat `genaudio -to16k`, `go build -race` `cmd/api` dan `cmd/realtime` ke temp, subprocess dengan `cmd.Env` eksplisit sesuai plan, port acak, `/readyz` ≤ 30 s, `startRealtime(pod)` siap dipakai Task 30), `helpers_test.go` (`newUser`, `startSession[For]`, `packets`, `recorder` (Sink), `converse`, `scrape`, `histogram`, `quantile`, query turn/ledger), `e1_e2_e6_test.go` (E1, E2, E6, E11), `slo_test.go` (`TestSLOServerSide`: 20 turn berurutan `ja_question`/`ja_greeting_short`, selisih scrape `/metrics` realtime).
- **Bug ditemukan e2e dan diperbaiki:**
  1. `cmd/realtime` tidak mengisi `session.Deps.OpeningPrompts`, sehingga `session.start` apa pun panik (nil pointer di `Service.BeginStart`, `lifecycle.go:206`) dan koneksi tidak pernah mendapat `session.started`. Perbaikan: `cmd/realtime/adapters.go` (`openingPromptSource` membaca repo konten langsung, tanpa cache API, supaya retire OPENING berlaku pada start berikutnya, E12); tes `TestOpeningPromptSourceAdapter`. Regresi level proses tercakup E1/E2.
  2. `kotokiclient.RunConversation`: opening AI_FIRST adalah turn 0, tetapi penanda "tidak ada turn dilihat" bernilai 0, sehingga `WaitAIDone` pada sesi AI_FIRST selalu timeout (juga untuk `kotoki-cli converse` AI_FIRST). Perbaikan: `maxTurnSeq`, `doneTurnSeq`, `utt.floorSeq` mulai -1. Tes regresi `TestWaitAIDoneSeesTheAIFirstOpening` (merah tanpa perbaikan; `evo.zeroTurn` ditambahkan ke harness tes).
  3. Histogram `server_overhead_seconds` terlalu kasar untuk menegakkan ≤ 50 ms: bucket halus (lihat deviations M5-T29); `metrics_test` disesuaikan.
- **`Platform: "cli"` pada `session.start`** diterima server asli (E1/E2/SLO sukses); masalah terbuka T28 itu tertutup. Auto-refresh token tidak dibutuhkan (SLO terpanjang ~150 s < 15 menit).
- **Hasil SLO (fake provider, 20 turn, 3 run identik hingga pembulatan):** `turn_end_to_first_audio_seconds` (kind=reply) P50 = 1,125 s, P95 = 1,238 s (≤ 1,5 s, D-100); `server_overhead_seconds` P50 = 0,014-0,015 s, P95 = 0,019 s (≤ 0,05 s), rata-rata 11,2-11,4 ms. Kuantil dari interpolasi bucket. Klien (`turn_latency_ms`, dari frame ucapan terakhir, memuat ambang silence 500/800 ms): ~1560 ms (`ja_question`) dan ~1755 ms (`ja_greeting_short`).
- **Verifikasi:** `go test -race -count=1 -tags=e2e ./test/e2e/... -run 'TestE1|TestE2|TestE6|TestE11|TestSLO'` hijau 3 kali berturut-turut (~170 s per run; E1 9,8 s, E2 3,4 s, E6 0,4 s, E11 0,4 s, SLO 145,5 s); `go build ./...`; `go test -race ./internal/... ./cmd/...` (1975 lulus); `make arch-check`; `make lint` dan `golangci-lint run --build-tags e2e ./...` (0 isu); `go vet -tags e2e ./test/e2e`.

## M5-T30 — E2E E3, E4, E5, E7, E10, E12, E13, E14 + P95 barge-in
- **`test/e2e/`**: `e3_bargein_test.go` (E3), `e4_e5_resume_test.go` (E4 `Drop{3s}` + resume, E5 dua proses + `SIGKILL` pemilik), `e7_e12_e13_test.go` (E7 STT, E12 fallback OPENING, E13 content filter + kontrol), `e10_e14_test.go` (E10 hanya `E2E_LONG=1`; E14 handoff drain tanpa kembali + kontrol kembali < 30 s, `session.NewSweeper(...).Tick` in-process), `bargein_p95_test.go` (`TestBargeInP95`). Helper baru: `extraRealtime`, `readSession`, `sessionEvents`, `setFaults`, `newSweeper`. `rtharness.WithRealClock()` ditambahkan (jam sistem, ticker nyata, pacer real-time). `Report` klien mendapat `stale_frames_dropped` dan `close_code`.
- **Bug ditemukan e2e dan diperbaiki:**
  1. `kotokiclient.RunConversation`: `session.started` yang di-replay (start ke sesi `ACTIVE` = resume di server, replay dari seq 0) dianggap tanda attach, sehingga skrip mulai sebelum replay/snapshot selesai dan `WaitAIDone` memakai `floorSeq` salah (selesai prematur; E5 menemukannya). Perbaikan: hanya `session.started` non-replay dan `session.snapshot` yang meng-attach. Tes regresi `TestReplayedSessionStartedDoesNotAttach` (merah tanpa perbaikan).
  2. Tidak ada bug runtime/gateway/fake: E3, E4, E5, E7, E12, E13, E14, E10 hijau tanpa perubahan server. Tambahan klien (non-bug): `Report.StaleFramesDropped` dan `Report.CloseCode` (tes di `TestBargeInStepMeasured`, `TestFatalServerErrorIsReturned`).
- **Hasil `TestBargeInP95`** (200 barge-in, jam nyata, `ScriptedRouter`, 3 kalimat x 1,5 s): `bargein_stop_seconds` P50 = 0,0000 s, P95 = 0,0000 s, maks 0,0000 s (dua run identik); nol frame gen lama tertulis setelah `ai.interrupted`. Nol berarti tidak ada frame audio ditulis setelah konfirmasi (stempel tulis terakhir mendahului konfirmasi; nilai diklem ke 0). Batas D-101 0,15 s. E3 (klien, biner nyata): `barge_in_stop_ms` = 462 (dari frame pertama ucapan penyela dikirim sampai frame gen lama terakhir diterima; memuat deteksi konfirmasi, bukan jaminan server), `stale_frames_dropped` = 0.
- **Hasil lain:** E10 `charged_seconds` = 300 (300,6 s); E13 `charged_seconds` fault 26 (dinding 25,6 s) vs kontrol 37 (36,5 s); E14 `charged_seconds` 4 (start..handoff 3,0 s, start..ended 34,1 s).
- **Verifikasi:** `go test -race -count=1 -tags=e2e ./test/e2e/... -run 'TestE3|TestE4|TestE5|TestE7|TestE12|TestE13|TestE14|TestBargeInP95'` hijau 2 kali (215 s dan 214 s); `E2E_LONG=1 go test -race -tags=e2e -timeout 30m ./test/e2e/... -run TestE10` hijau (300,6 s); `make test-e2e` hijau (375 s, E10 dilewati); `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau; `make test`; `make arch-check`; `make lint` dan `golangci-lint run --build-tags e2e ./...` 0 isu; `go vet -tags e2e ./test/e2e`.


## M6-T1 — Grup konfigurasi worker (RabbitMQ, outbox, job)
- **`internal/platform/config/{rabbit,outbox,jobs}.go`**: `Rabbit{URL Secret}`, `Outbox{PollInterval, ConfirmTimeout, TxTimeout, BatchSize, MaxAttempts}`, `Jobs{SweeperInterval, SweeperExpiryGrace, SweeperHeartbeatTimeout, Retention*Days (5), AccountPurgeDelay}` di-embed ke `Worker`; default sesuai Global Constraints M6; `defaultWorker` mengisinya. `validate.go`: `Worker.Validate` memanggil `Rabbit/Outbox/Jobs.validate` (durasi ≤ 0, batch/attempts ≤ 0, hari di luar 1–3650, `amqps://` di staging/prod).
- **`.env.example`**: blok "Worker: outbox & job" (13 variabel; `RABBITMQ_URL` dan `RETENTION_TRANSCRIPT_DAYS` sudah ada). `TestEnvExampleLoads` tetap hijau.
- **Tes** (`worker_groups_test.go`): `TestWorkerGroupDefaults`, `TestWorkerGroupOverrides`, `TestValidateRabbit` (21 kasus), `TestRabbitURLRedacted`.
- **Verifikasi:** tes gagal (build) tanpa implementasi, lalu hijau; `go test -race ./internal/platform/config` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` 0 isu. Mutasi, semua merah: cek amqps strict dihapus, validasi `OUTBOX_TX_TIMEOUT` dihapus, batas bawah hari dihapus, default `BatchSize` 50, default heartbeat 40 s, `Rabbit.validate`/`Jobs.validate` tidak dipanggil.


## M6-T2 — Paket `events` (envelope, payload V1, idempotensi, error)
- **Baru:** `envelope.go` (`Envelope`, `Event.Envelope`, `DecodeEnvelope`, `DecodePayload[T]`), `delivery.go`, `errors.go` (`PermanentError`/`Permanent`, `ErrSkippedDeletedUser`), `idempotency.go` (`ErrAlreadyProcessed`, `ProcessedEvents`), `queues.go` (4 nama queue), `payloads.go` (`SessionCompletedV1` alias, `EvaluationCompletedV1`, `EvaluationFailedV1`, `MemoryExtractedV1` + builder). **Diubah:** `types.go` (konstanta `EvaluationCompleted/Failed`, `MemoryExtracted`, `AggregateEvaluation`, `CurrentVersion`, `SupportedVersions`).
- **Tes:** `envelope_test.go`, `payloads_test.go`, `errors_test.go` + 4 golden di `testdata/`; cakupan paket 100%.
- **Verifikasi:** `go test -race -cover ./internal/events` hijau (cover 100,0%); `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` 0 isu. Mutasi, semua merah (16): pembulatan alih-alih pemotongan ms, `occurred_at` RFC3339Nano, batas versi, payload `null`, cek `event_id`/`event_type`, kode DECODE_ERROR, map/array nil jadi null, aggregate ID/type salah, `omitempty` pada skor, format `Error()`, `Unwrap` putus, nama queue, `SupportedVersions`.


## M6-T3 — Postgres: verifikasi outbox writer M3 + `processed_events`
- **Baru:** `internal/postgres/processed_events.go` (`processedEventsTx.MarkProcessed` → `INSERT … ON CONFLICT (consumer,event_id) DO NOTHING`, 0 baris → `events.ErrAlreadyProcessed`, error DB → `errs.Wrap`; `ProcessedEventsReader.IsProcessed`), `db/queries/processed_events.sql` + `sqlcgen/processed_events.sql.go`, `fakes/processed.go`. **Diubah:** `store.go` (`Tx.Processed()`), `testutil/trace.go` (propagator W3C, lihat deviasi).
- **Tes (integration):** `outbox_writer_m6_integration_test.go` (7: traceparent dari span aktif, tracestate, tanpa span, 4 tipe + tipe asing ditolak, batas 64 KiB, rollback), `processed_events_integration_test.go` (6: duplikat, rollback, consumer tak dikenal, consumer berbeda, 8 goroutine paralel, `IsProcessed`), `fakes/processed_test.go`.
- **Verifikasi:** perintah Step 2 gagal (build: `Processed` belum ada), lalu `go test -tags integration ./internal/postgres/... -race -run 'TestOutboxWriter|TestMarkProcessed|TestIsProcessed'` hijau (14 tes); seluruh `-tags integration ./internal/postgres/... ./internal/session/...` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` 0 isu (juga dengan `--build-tags integration`); `make sqlc` tanpa diff. Mutasi, semua merah (8): `Inject` dihapus, tracestate tidak disimpan, `n==0` diganti, `ON CONFLICT` dihapus, `Processed()` memakai pool bukan tx, `IsProcessed` tanpa predikat consumer, `event_type` salah.
- **Temuan `TestFinalizeRuntimeVsSweeperRace` (M3, `test/integration/m3_end_test.go`): bukan bug `Finalize`/outbox/ledger.** Kegagalan ada pada asersi `CONSUME = 1` (iterasi acak, mis. ke-6), bukan outbox. Sesi dibuat dengan `clock.System{}`: `Activate` lalu `BeginEnding` terjadi dalam < 1 ms, jadi `activeMs = billableEnd − startedAt` = 0 → `ComputeCharge` = 0 → `Finalize` memang tidak menulis ledger `CONSUME` (rancangan, bukan race). Dengan `time.Sleep(3ms)` setelah `activate` tes lulus 3 × 100 iterasi. Perbaikan yang tepat adalah di tes (jam palsu atau jeda), tidak saya ubah (bukan akar outbox/ledger). Catatan untuk Task 23: tes exactly-once M6 harus memakai jam palsu atau durasi aktif ≥ 1 s agar `CONSUME` tidak kosong; jumlah `session.completed` (outbox) pada tes itu tetap 1 di semua iterasi yang gagal (asersi outbox tidak pernah menjadi yang gagal).

## M6-T4 — Topologi RabbitMQ + helper `testutil.NewRabbitMQ`
- **Baru:** `internal/amqp/topology.go` (konstanta exchange/queue, `RetryLevels`, `MaxRetries`, `Queues`, `RetryQueueName`, `DLQName`, `DeclareTopology` mengembalikan error), `internal/testutil/rabbitmq.go` (`NewRabbitMQ`: image `rabbitmq:4-management`, satu container per paket tes, vhost `t_<uuid>` lewat management API dan dihapus di cleanup; override `TEST_RABBITMQ_URL`/`TEST_RABBITMQ_MGMT_URL`; `RabbitMQAdmin` dengan `CloseAllConnections/QueueMessages/QueueArgs/Queues/Exchanges`), `docs/ops/rabbitmq.md`. `go.mod`: `amqp091-go v1.15.0`, modul testcontainers `rabbitmq v0.44.0`.
- **CI:** `ci.yml` tidak diubah. Job `integration` sudah `ubuntu-24.04` (Docker daemon tersedia) dan menjalankan `make test-integration` (`./...` dengan tag `integration`), jadi tes RabbitMQ ikut berjalan tanpa `services:`.
- **Tes:** unit `TestQueuesTable/TestRetryLevels/TestDLQName`; integration `TestDeclareTopology_Idempotent_25Queues`, `TestQueueArguments`, `TestRouting_*` (3), `TestRetryQueue_5s_ReturnsOnlyToOriginQueue`.
- **Verifikasi:** `go test -tags integration ./internal/amqp/... -race -count=3` hijau (27 tes); `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` (juga `--build-tags integration`) 0 isu; `go mod tidy` tanpa diff setelah commit. Mutasi, semua merah (5): `alternate-exchange` dihapus, binding `kotoki.requeue` diganti `kotoki.dlx`, DLX retry queue diganti `kotoki.events`, `x-delivery-limit` 20, binding learner ke `session.completed`.

## M6-T5 — Koneksi ber-reconnect dan `ConfirmPublisher`
- **Baru:** `internal/amqp/conn.go` (`Backoff`, `Conn` dengan `Dial/Channel/Healthy/Closed/Reconnect/Close`, `ErrBrokerUnavailable`), `internal/amqp/confirm.go` (`ConfirmPublisher`, `Confirmation`, `ErrNacked`, `ErrConfirmTimeout`). `Dial` satu kali percobaan (terikat ctx + timeout 10 s, heartbeat 10 s, `connection_name`) lalu `DeclareTopology`; `Reconnect` mencoba segera, lalu `Backoff` 1s,2s,4s... (maks 30 s nominal, jitter ±20%) sampai sukses/ctx selesai, dan no-op bila sudah sehat; setelah `Close` selalu `ErrBrokerUnavailable`.
- **Tes:** unit `TestBackoff_Sequence`, `TestConfirmation_Wait_TimeoutAndCallerCancel`; integration `TestConfirmPublisher_PublishConfirmed_Delivers`, `_Send_BatchThenWait`, `_Nack_ErrNacked`, `TestConn_ReconnectsAfterForcedClose`, `TestConn_ClosedForGood_ReconnectFails`, `TestConfirmPublisher_ClosedConn_ErrBrokerUnavailable`.
- **Verifikasi:** `go test -tags integration ./internal/amqp/... -race -count=3` hijau (51 tes lulus); `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint dengan `--build-tags integration` 0 isu; `go mod tidy` tanpa diff. Mutasi, semua merah (6): batas backoff 60 s, jitter ±30%, mode confirm tidak diaktifkan, nack dikembalikan sebagai sukses, timeout dipetakan ke nack, topologi tidak dideklarasi ulang saat reconnect.

## M6-T6 — Outbox publisher: pembentukan message & trace propagation
- **Baru:** `internal/amqp/carrier.go` (`amqpHeaderCarrier`), `internal/amqp/outbox_publisher.go` (`OutboxRow`, `BuildMessage`; bagian publisher batch menyusul di T7). `BuildMessage` membuang span aktif dari ctx pemanggil, merekonstruksi parent dari kolom `traceparent/tracestate` (NULL atau tidak valid: span root baru), memulai span `kotoki.events publish` (PRODUCER), menyuntikkan span itu ke header AMQP dan field envelope, lalu menyusun properti §9.3.3 (`DeliveryMode 2`, `AppId kotoki`, `x-event-version` int32, `MessageId`, `Type`, `Timestamp`).
- **Tes:** `outbox_message_test.go` (properti, versi header mengikuti row, body = envelope dengan payload byte-identik, parent dari traceparent tersimpan + tracestate, NULL = root tanpa header tracestate, traceparent tak valid = root, parent bukan span pemanggil, NULL mengabaikan span pemanggil), `carrier_internal_test.go` (`TestCarrier_GetSetKeys`).
- **Verifikasi:** perintah Step 2 gagal (build) lalu hijau dengan `-race`; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi, semua merah (10): kind selain PRODUCER, span pemanggil tidak dibuang (ditangkap setelah tes `NULL_IgnoresCallerSpan` ditambah), header versi bukan int32, `DeliveryMode` transient, field envelope bukan span publish, `occurred_at` bergeser, `MessageId` salah, `AppId` salah, `Set` carrier tidak menulis.

## M6-T7 — Outbox publisher: batch SKIP LOCKED, confirm, backoff, gauge
- **Baru/diubah:** `internal/amqp/outbox_publisher.go` (`BatchPublisher`, `PublisherConfig`, `OutboxPublisher{Sleep, WaitReconnect}`, `NewOutboxPublisher`, `PublishBatch`, `Run`), `outbox_gauges.go` (`RefreshOutboxGauges`, `RefreshQueueDepth`, `RunOutboxGauges` 15 s, `RunQueueDepth` 30 s), `metrics.go` (`Metrics`/`NewMetrics`: `kotoki_consumer_*`, `kotoki_outbox_*`, `kotoki_rabbitmq_queue_messages`). SQL §9.3.2 persis (`FOR UPDATE SKIP LOCKED`, update PUBLISHED/gagal dalam tx yang sama, `LEAST(power(2, attempts+1), 300)`).
- **Tes (integration, 168 lulus dengan `-count=3` untuk seluruh paket):** `PublishBatch` — ack (urutan `available_at` lalu `event_id`, event_id berlawanan arah dengan available_at), nack (attempts 1→2, backoff (1,3] lalu (3,5] s, `last_error`, metrik `nack`), timeout confirm (metrik `timeout`), batas 300 s, percobaan ke-10 jadi `FAILED` dan berhenti diambil, `MaxAttempts` dari config, verdict campuran, broker mati (`attempts` tetap, tak ada Send), `Send` error di tengah batch (rollback utuh lalu dipublish ulang, baris 1 terkirim dua kali), channel mati saat menunggu confirm (baris yang sudah di-ack ikut rollback, `attempts` tetap, dipublish ulang), shutdown saat menunggu confirm tetap commit, `TxTimeout` membatasi batch, dua publisher paralel `SKIP LOCKED` (200 baris, rendezvous membuktikan batch bersamaan, tiap event tepat satu kali), baris masa depan/non-PENDING diabaikan, `BatchSize`, antrean kosong tidak menyentuh broker; `Run` — backlog 2,2,1 lalu sleep tepat 1x, broker mati menunggu reconnect lalu terbit tanpa membakar attempts, default wait = Backoff 1/2/4 s, error umum sleep `PollInterval`, berhenti bersih saat cancel; gauge (`pending/failed/oldest`, kosong, queue depth). Broker nyata: `TestOutbox_EndToEnd_SessionCompleted` (tepat 1 pesan di 3 queue, trace-id sama dengan kolom outbox, span baru), `TestOutbox_BusinessRollback_NoEventReachesTheBroker` (rollback bisnis, tidak ada event), `TestOutbox_RealBrokerOutage_DoesNotBurnAttemptsAndRecovers` (`CloseAllConnections`, 3 poll gagal, `attempts 0`, `Reconnect`, terbit), `TestOutbox_Run_DrainsBacklogThroughRealBroker`.
- **Verifikasi:** Step 2 gagal (build: `PublisherConfig` belum ada), lalu hijau; `go test -tags integration ./internal/amqp/... -race -count=3` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan `golangci-lint run --build-tags integration ./...` 0 isu; `go mod tidy` tanpa diff. Mutasi (19), semua merah setelah dua perbaikan tes: `SKIP LOCKED` dihapus (awalnya selamat karena rendezvous tertipu oleh `Wait` berikutnya, kini hanya `Wait` pertama tiap publisher yang dihitung), `ORDER BY event_id` saja (awalnya selamat: id acak; kini id berlawanan urutan), `attempts+1 > max`, `power(2, attempts)`, batas 600 s, `IsOpen` dilewati, `ErrBrokerUnavailable` dihitung sebagai timeout, error `Send` diabaikan, urutan `available_at DESC`, batch tanpa `WithoutCancel`, filter `available_at` dihapus, label reason salah, `n > BatchSize`, backoff default tidak naik, `LIMIT` salah, baris ter-ack tak ditandai, timeout dilaporkan `nack`, `min(created_at)` tanpa filter PENDING, baris ter-ack juga dihitung gagal.

## M6-T8 — `Classify` dan kode error consumer
- **Baru:** `internal/amqp/errors.go` (`Class`, `Classify` 11 aturan §9.7.3 berurutan, `errorCode`, `headerInt`, `truncate`).
- **Tes:** `errors_test.go` (`TestClassify` 26 kasus tabel termasuk error dibungkus `%w`, `TestClassify_PermanentWinsOverShutdown`), `errors_internal_test.go` (`TestErrorCode`, `TestHeaderInt`, `TestTruncate_512`).
- **Verifikasi:** Step 2 gagal (build), lalu hijau dengan `-race`; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi, semua merah (11): `ErrSkippedDeletedUser` tak di-Ack, `shuttingDown` diabaikan, aturan PermanentError dimatikan, polaritas `IsRetryable` dibalik, kelas `22` dihapus, `23` dihapus (via `22`/`23` terpisah), default menjadi Dead, `int32` header tak dibaca, rune terpotong, kode `RETRIES_EXHAUSTED` salah. Dua mutasi awal pada cabang retry SQLSTATE selamat karena cabang itu sama dengan default; cabang redundan dihapus dari kode.

## M6-T9 — `Consumer.process`: ack/nack, retry ladder, DLQ, idempotensi
- **Baru:** `internal/amqp/consumer.go` (`Handler`, `Publisher`, `ProcessedChecker`, `Consumer`, `NewConsumer`, `process`, `checkSupported`, `retry`, `dead`, `safeHandle`, `cloneHeaders`, `copyMessage`), `internal/amqp/export_test.go` (`Consumer.Process`, `NewConsumerForTest`); `errors.go` + `asPermanent`. Alur §9.7.4: span CONSUMER `<queue> process` (parent dari header `traceparent`, fallback field envelope), pre-check `IsProcessed`, handler dengan `Timeout()` + `recover()`, `Classify(err, root.Err() != nil)` → ack / requeue / retry (publish salinan ke `kotoki.retry` lalu ack) / DLQ (publish ke `kotoki.dlx` lalu ack); metrik `ack|duplicate|retry|dead|requeue|skipped_deleted_user`, gauge inflight, histogram durasi; log slog dengan `trace_id, span_id, event_id, event_type, queue, retry_count, worker_version`.
- **Tes (`consumer_process_test.go`, 39 tes):** permanent → DLQ langsung satu publish tanpa header `x-retry-count` dan tanpa masuk tangga; ladder tabel (absen/1/2/3 → `Q.retry.5s/30s/2m/10m`, `x-retry-count` int32 1..4, body identik, header delivery tidak termutasi) plus pendakian penuh 5 percobaan (4 retry lalu `kotoki.dlx`) dengan salinan diumpan balik seperti broker; `x-first-failed-at` tidak ditimpa; `x-last-error` ≤ 512; retry habis → DLQ `RETRIES_EXHAUSTED` dengan `x-dead-at`; decode/versi/tipe tak dikenal → DLQ tanpa `OnFailure`; panic dan timeout → retry; gagal publish retry → `Nack(requeue=true)`, gagal publish DLQ → `Nack(requeue=false)`; shutdown (root dibatalkan di tengah handler) → `Nack(requeue=true)`, tanpa publish, tanpa `OnFailure`, metrik `requeue` saja (tidak menghitung retry); `ErrAlreadyProcessed`/pre-check duplikat/`ErrSkippedDeletedUser` → ack dengan metrik masing-masing; redelivery paksa tiga kali = satu efek; trace (parent, kind, atribut, fallback, header menang, salinan retry membawa span consumer); body/properti salinan; gauge inflight.
- **Verifikasi:** Step 2 gagal (build, `consumer.go`/`export_test.go` dipindah sementara karena implementasi ditulis lebih dulu; tes ditulis sesudahnya dan diperiksa merah dengan cara itu), lalu hijau; `go test ./internal/amqp/... -race -run 'TestProcess|...' -count=3` hijau (213 lulus); `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu; `go vet -tags integration` bersih; `go mod tidy` tanpa diff. Mutasi (31), semua merah: `RetryLevels[0]`, `x-retry-count` selalu 1, `>` bukan `>=` MaxRetries, `x-first-failed-at` selalu ditimpa, `OnFailure(final=false)` dihapus, `OnFailure(final=true)` dihapus (dead & exhausted), DLQ ke exchange retry, key DLQ kosong, retry ke `kotoki.requeue`, Nack retry-gagal tanpa requeue, Nack DLQ-gagal dengan requeue, shutdown tak terdeteksi, pre-check dibalik, versi `>=`, body salinan nil, `DeliveryMode` transient, `cloneHeaders` aliasing, batas `x-last-error` 1024, `OnFailure` pada requeue, label skipped dihapus, `recover()` dihapus, timeout handler dihapus, kind span bukan CONSUMER, fallback traceparent envelope dihapus, `Inject` dihapus, `Inc` inflight dihapus, `x-original-queue` salah, `x-dead-at` kosong, retry tanpa `Ack` asli. Satu mutasi awal selamat (atribut `messaging.message.id` fallback untuk pesan tak terdecode); cabang itu dihapus.

## M6-T10 — Runtime consumer: `Run`, Qos, reconnect, graceful shutdown
- **Baru:** `internal/amqp/consumer_run.go` (`Consumer.Run`, `Consumer.Shutdown`, `consumeOnce`, `prefetchOf`, `lifecycle`); `consumer.go` + field `lifeOnce`/`lc` (zero-value `Consumer` tetap valid untuk `NewConsumerForTest`). `Run`: satu channel, `Qos(prefetch,0,false)` dari tabel `Queues`, `prefetch` goroutine pemroses, `Consume` manual-ack; bila koneksi/channel putus: `ch.Cancel`, tunggu semua goroutine in-flight, `conn.Reconnect` (backoff 1s..30s ±20%, topologi dideklarasi ulang), `Qos`+`Consume` ulang; bila Consume gagal padahal koneksi sehat: tunggu `Backoff` agar tidak berputar. `Shutdown(ctx, grace)`: tutup `stop` (basic.cancel; delivery yang sudah diambil tapi belum mulai di-`Nack(requeue)` dan dihitung `result="requeue"`), tunggu in-flight ≤ grace, lalu batalkan root ctx (handler -> `context.Canceled` -> requeue tanpa `OnFailure`/`x-retry-count`), kembali setelah `Run` selesai.
- **Tes (`consumer_run_integration_test.go`, 5 tes, 6 dengan subtest) dengan broker nyata:** prefetch 16 (learner) dan 4 (evaluation) dengan 40 pesan (puncak concurrency == prefetch; broker menahan 40-prefetch pesan ready), shutdown dalam grace (ack in-flight, queue kosong, `Run` kembali nil), grace habis (pesan kembali ke queue utama `Redelivered=true`, tanpa `x-retry-count`, retry/DLQ kosong, `OnFailure` 0, metrik `requeue`=1), 10 pesan dengan 4 in-flight (10 tetap di queue setelah shutdown, hanya 4 yang pernah dimulai), reconnect setelah `CloseAllConnections`. Helper bersama suite integration consumer (`queueHandler`, `startConsumer`, `testEnvelope`, `publishToQueue`, `getOne`) ada di file ini dan dipakai Task 11.
- **Verifikasi:** Step 2 gagal (build, `Run` belum ada), lalu `go test -tags integration ./internal/amqp/... -race -count=3 -run TestConsumer_` hijau (21 lulus); `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2115); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah: Qos `prefetch+2` (PrefetchLimitsConcurrency), root ctx handler diganti `context.Background()` (GraceExpires dan UnstartedPrefetched), `Reconnect` dimatikan (ReconnectsAfterForcedClose). Cabang `halting` yang me-nack delivery buffer belum-mulai tidak punya mutasi merah: jumlah worker == prefetch sehingga buffer praktis kosong dan cabang itu hanya pagar balapan; sifat yang dijaga (pesan tidak hilang/tidak dihitung retry saat shutdown) tetap dibuktikan lewat broker oleh dua tes shutdown.

## M6-T11 — Suite integration consumer: retry nyata, poison, idempotensi, trace end-to-end
- **Baru:** `internal/amqp/consumer_integration_test.go` (9 tes, broker + Postgres nyata); `consumer_run_integration_test.go` + `queueHandler.deliveries`; `consumer.go` hanya komentar `retry` dikoreksi (perilaku tidak berubah). Tidak ada kode produksi baru.
- **Tes:** `TestRetry_FirstRung_RealBroker` (pesan terlihat di `….retry.5s`, handler dipanggil lagi setelah 4 s..9 s, `RetryCount=1`, metrik retry 1 lalu ack 1, `OnFailure(final=false)`; TTL nyata 5 s, tidak dipersingkat: plan tidak menyediakan cara dan hanya anak tangga pertama yang diuji nyata, tangga penuh dikunci unit `TestProcess_RetryLadder_*`); `TestPoison_PermanentError_LandsInDLQWithHeaders` (DLQ 1 pesan dengan 4 header, body utuh, tanpa retry queue, pesan berikutnya tetap diproses); `TestPoison_MalformedBody_DLQDecodeError`; `TestPoison_DeliveryLimit_DeadLettersToDLQ` (channel ditutup dengan delivery unacked: pesan dikirim 11 kali lalu di-dead-letter queue ke `Q.dlq`); `TestDeliveryLimit_NackRequeueDoesNotCount` (mengunci perilaku RabbitMQ 4, lihat deviasi); `TestIdempotency_DuplicateDelivery_SingleEffect` (handler = satu tx `MarkProcessed`+INSERT efek di Postgres nyata; kopi kedua `duplicate` lewat pre-check, handler 1x, 1 efek, 1 baris `processed_events`); `TestIdempotency_PrecheckBypassed_MarkProcessedStillGuards` (dua kopi diproses serentak, tanpa pre-check, 1 efek, kedua di-ack); `TestIdempotency_ForcedRedelivery_AfterCommitBeforeAck` (efek commit, koneksi consumer dimatikan broker sebelum ack, `Run` reconnect, redelivery dikenali pre-check `duplicate`, handler 1x, 1 efek); `TestTrace_OutboxToConsumer_SameTraceID` (tx bisnis -> outbox -> `PublishBatch` nyata -> consumer: trace id sama, span handler berbeda).
- **Verifikasi:** Step 2: delivery-limit versi plan (Nack requeue 11x) gagal terhadap broker nyata (13 delivery, tanpa dead-letter) sehingga tes diubah (deviasi); delapan tes lain hijau pada run pertama (consumer sudah benar). `go test -tags integration ./internal/amqp/... -race -count=3 -run 'TestRetry_|TestPoison_|TestIdempotency_|TestTrace_|TestDeliveryLimit'` hijau (27 lulus), seluruh paket integration `-race` hijau (143); `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2115); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu; `go mod tidy` tanpa diff. Mutasi merah: pre-check `IsProcessed` dimatikan (DuplicateDelivery, ForcedRedelivery), `ErrAlreadyProcessed` dihapus dari aturan Ack (PrecheckBypassed), `MarkProcessed` tak lagi melapor duplikat (PrecheckBypassed), `x-retry-count` +2 (FirstRung), `x-error-class` salah (kedua tes Poison), ekstraksi trace dimatikan di header dan fallback envelope (Trace). Mematikan ekstraksi header saja tidak terdeteksi, disengaja: fallback field envelope menghasilkan trace id yang sama (§9.7.5).

## M6-T12 — `dlq-replay`
- **Baru:** `internal/amqp/dlq_replay.go` (`ReplayOptions`, `ReplayDLQ`), `internal/amqp/dlq_replay_integration_test.go` (7 tes, broker nyata). `ch.Get` manual-ack satu per satu; publish ke `kotoki.requeue` key `Queue` dengan `x-retry-count=int32(0)` + `x-replayed-at` (header lain dan body utuh), ack setelah confirm; `DryRun` mencetak lalu satu `Nack(multiple, requeue)`; queue di luar `Queues` ditolak sebelum menyentuh broker.
- **Tes:** `MovesBackToMainQueueWithResetHeaders`, `Limit`, `DryRun_PrintsAndLeavesAll`, `DryRun_LimitListsOnlyLimit`, `UnknownQueue_ErrorBeforeBroker`, `PublishFailure_KeepsMessageInDLQ`, `EndToEnd_PoisonThenFixedHandler`.
- **Verifikasi:** Step 2 gagal (build, simbol belum ada); `go test -tags integration ./internal/amqp/... -race -count=3 -run TestReplayDLQ` hijau (21 lulus); `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2115); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah: `x-retry-count` tidak di-reset, dry run `Ack` alih-alih `Nack` (dua tes dry run), `Limit` diabaikan, nack-requeue saat publish gagal diganti ack.

## M6-T13 — Scheduler `jobs` + advisory lock + tabel job kanonik
- **Baru:** `internal/jobs/scheduler.go` (`Spec`, `Specs`, `SpecByName`, `SpecsForFlags`, `Job`, `SchedulerDeps`, `Scheduler.Register/Run/RunOnce`), `lock.go` (`Locker`, `PgLocker`), `metrics.go` (`Metrics`, `NewMetrics`), `scheduler_test.go` (13 tes unit), `lock_integration_test.go` (3 tes, Postgres nyata). Satu goroutine per job: jitter awal -> run -> tick; tanpa tumpang tindih; lock per run + `context.WithTimeout(Spec.Timeout)`; hasil `ok|error|skipped_locked` + durasi + `last_success` (hanya ok).
- **Verifikasi:** Step 2 gagal (build, paket belum ada); `go test ./internal/jobs/... -race -count=10` hijau, `go test -tags integration ./internal/jobs/... -race -count=3` hijau (48 lulus); `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2128); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu; `go mod tidy` tanpa diff. Mutasi merah: timeout run dihapus (RunTimeout, RunOnce), `skipped_locked` jadi `ok` (SkippedLocked, RunOnce), `SweeperInterval` berlaku untuk semua job, jitter awal dilewati (InitialJitter, CancelDuringInitialJitter), run memakai ctx yang bisa dibatalkan (StopsStartingNewRuns), unlock + fallback dihapus (ketiga tes PgLocker). Deviasi: 8 job, bukan 9 (lihat deviations).

## M6-T14 — Job `session_sweeper` + `reservation_cleanup` (membungkus M3)
- **Baru:** `internal/jobs/session_sweeper.go` (`SessionTicker`, `SessionSweeperJob`: peta `SweepReport` ke `kotoki_sweeper_actions_total`, `job_items_total`, gauge `heartbeat_checks_suspended`, counter + log ERROR mass loss; instance `Sweeper` baru bila gap run > 2x interval), `reservation_cleanup.go` (`AbandonSource`, `Abandoner`, `ReservationCleanupJob`; batas 200, actor `CLEANUP`), `internal/testutil/sessionenv/env.go` (harness jam palsu), tes unit + integration (Postgres + Redis nyata). **Diubah (M3, aditif):** `internal/session/sweeper.go` (field `SweepReport` baru), `sweeper_test.go` (2 tes), `test/integration/m3_end_test.go` (tes race memakai jam palsu).
- **Verifikasi:** `go test ./internal/jobs/... ./internal/session/... -race -count=3 -run 'TestSessionSweeperJob|TestSweepReport|TestReservationCleanup'` hijau; `go test -tags integration ./internal/jobs/... -race -count=3 -run 'TestReservationCleanup_|TestSessionSweeperJob_'` hijau; `go test -tags integration ./test/integration -race -count=3 -run 'TestFinalizeRuntimeVsSweeperRace|TestSweeper'` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` (2139 lulus); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah (semua tertangkap): flag `HeartbeatSuspended` dan `MassHeartbeatLoss` dihapus, `ActiveNonHandoff` memakai kandidat, gap 2x diganti 200x, `MassHeartbeatLoss.Inc` dihapus, gauge selalu 0, label action salah, actor CLEANUP jadi SWEEPER, `ErrInvalidTransition` bukan noop, query kandidat memakai `now−1h`, limit 200 jadi 1000.

## M6-T15 — Job `balance_reconcile`
- **Baru:** `internal/jobs/balance_reconcile.go` (`Reconciler`, `BindReconciler`, `BalanceReconcileJob`: gauge `kotoki_balance_invariant_violations{check}` R1–R6, log ERROR per check ≤ 20 id, hanya membaca), `balance_reconcile_test.go` (unit), `balance_reconcile_integration_test.go` (3 tes: bersih, tamper R1/R3 tanpa auto-fix, R6), `internal/balance/reconcile_continue_test.go`, `internal/postgres/{export_test,reconcile_repo_integration_test}.go`. **Diubah:** `internal/balance/reconcile.go` (lanjut setelah satu check gagal, `errors.Join`, hormati ctx), `internal/balance/reconcile_test.go` (tes lama yang bertentangan dihapus), `internal/postgres/reconcile_repo.go` (tx READ ONLY + `SET LOCAL statement_timeout='60s'`), `internal/testutil/sessionenv/env.go` (`SkipFinalReconcile`).
- **Verifikasi:** Step 2 gagal (build/asersi merah: `jobs.Reconciler` belum ada, `TestReconcile_ContinuesAfterCheckError`/`JoinsEveryCheckError` gagal, `export_test` tidak kompilasi); `go test ./internal/jobs/... ./internal/balance/... -race -count=3 -run 'TestBalanceReconcile|TestReconcile|TestBind'` hijau; `go test -tags integration ./internal/jobs/... ./internal/postgres/... -race -count=3 -run 'TestBalanceReconcile|TestRunCheck|TestPGReconcile'` hijau; integration penuh `./internal/jobs ./internal/postgres ./test/integration` -race hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` (2148 lulus); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah (semua tertangkap): `continue` jadi return awal, error tidak dikembalikan, cek ctx dihapus, gauge tidak di-0-kan/selalu di-0-kan saat error, batas log 20 jadi 25, gauge = 1 bukan jumlah baris, `Run` menelan error, tx tanpa READ ONLY, timeout 30s, `SET` tanpa `LOCAL`.

## M6-T16 — Job `partition_maintenance`
- **Baru:** `internal/jobs/partition_maintenance.go` (`Partition`, `PartitionStore`, `PartitionConfig`, `PartitionName`, `PlanCreate`, `ParseBound`, `ExpiredPartitions` dengan guard `upper >= awal bulan berjalan UTC`, `PartitionMaintenanceJob`), `internal/jobs/partition_store.go` (`PgPartitionStore`), `internal/postgres/partition_repo.go` (`PartitionRepo`: `List` via `pg_inherits` + `pg_get_expr` + `inhdetachpending`, `Ensure`, `Drop`). Tes: `partition_maintenance_test.go` (unit), `partition_maintenance_integration_test.go`, `internal/postgres/partition_repo_integration_test.go`.
- **Verifikasi:** Step 2 gagal (build: simbol belum ada; implementasi dipindah sementara untuk memeriksa merah); `go test ./internal/jobs/... -race -count=3 -run 'TestPartition|TestPlanCreate|TestParseBound|TestExpired|TestJob_'` hijau (13 tes x3); `go test -tags integration ./internal/postgres/... ./internal/jobs/... -race -count=3 -run 'TestPartition|TestPgPartition'` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` (2161 lulus); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah (semua tertangkap): guard bulan berjalan dibalik/dinonaktifkan, margin 2 jam jadi 1 jam, margin ikut ke events, `Before` jadi `!After` (batas sama), `partitionsAhead` 2, `monthStart` tanpa `.UTC()`, retensi events memakai hari transcript, gauge memakai `Upper` bukan `Lower`, daftar partisi ada diabaikan, bound tanpa `.UTC()`; repo: `FINALIZE` jadi `CONCURRENTLY`, `lock_timeout` dihapus/50 ms, cek nama-bulan dan cek `upper` dihapus, regex nama dinonaktifkan (awalnya selamat; ditambah tes partisi `session_turns_legacy`), urutan `List` dibalik (awalnya selamat; ditambah asersi urutan), `IF NOT EXISTS` dihapus, literal batas bukan `+00`.

## M6-T17 — Job `transcript_retention` (+ pembersihan token)
- **Baru:** `internal/jobs/transcript_retention.go` (`RetentionStore`, `RetentionConfig`, `TranscriptRetentionJob`), `internal/postgres/retention_repo.go` (`RetentionRepo`, tiap batch satu transaksi), `db/queries/retention.sql` + `sqlcgen/retention.sql.go`. Tes: `transcript_retention_test.go` (unit, store palsu), `internal/postgres/retention_repo_integration_test.go`, `internal/jobs/transcript_retention_integration_test.go`.
- **Verifikasi:** Step 2 gagal (build: `jobs.RetentionStore`/`postgres.RetentionRepo` belum ada, implementasi dipindah sementara); `go test ./internal/jobs/... -race -count=3 -run TestRetentionJob` hijau (7 tes x3); `go test -tags integration ./internal/postgres/... ./internal/jobs/... -race -count=3 -run 'TestRetention_|TestTranscriptRetentionJob'` hijau, seluruh paket integration `internal/jobs` + `internal/postgres` `-race` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` (2168 lulus); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu; `sqlc generate` tanpa diff. Mutasi merah (semua tertangkap, 29): margin dihapus/1 jam, cutoff events memakai hari transcript, `n < batch` jadi `== 0`/`<=`, hari token salah, ukuran batch evaluasi/token/ringkasan salah, error step tidak dikumpulkan, hitungan item dihapus; SQL: turns `<=` dan tanpa `LIMIT`, events `<=` (awalnya selamat; ditambah event tepat di cutoff), evaluasi tanpa `details_purged_at IS NULL`/tanpa hapus findings/tanpa NULL feedback/tanpa kosongkan strengths, ringkasan tanpa `transcript_purged_at IS NULL` dan memakai `created_at` bukan `ended_at`, token `min` bukan `max`, tanpa `LEAST/COALESCE`, reset token 48 jam, ambang token `now`, jumlah reset tak dihitung.

## M6-T19 — Job `outbox_retention`
- **Baru:** `internal/jobs/outbox_retention.go` (`OutboxRetentionStore`, `OutboxRetentionConfig`, `OutboxRetentionJob`); `internal/postgres/retention_repo.go` bertambah `DeleteOutboxPublished/DeleteOutboxFailed/DeleteProcessedEvents` (`DELETE … WHERE ctid IN (SELECT ctid … LIMIT n)`, pgx mentah). Refactor kecil `transcript_retention.go`: loop drain dipindah ke `runRetentionSteps`/`drainStep` (dipakai bersama; perilaku T17 tak berubah). Tes: `outbox_retention_test.go` (unit), `outbox_retention_integration_test.go` (jobs, DB nyata 5.001 baris), `internal/postgres/outbox_retention_integration_test.go`.
- **Verifikasi:** Step 2 merah (build: simbol belum ada); `go test ./internal/jobs/... -race -count=3 -run 'TestOutboxRetention|TestRetentionJob'` hijau; `go test -tags integration ./internal/postgres/... ./internal/jobs/... -race -count=3 -run TestOutboxRetention` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah (tertangkap): FAILED ikut PENDING, batas `LIMIT+1`, ambang processed +5 hari, published memakai `created_at`, hari FAILED 7, batch 500, published memakai hari processed. Mutan tak tertangkap (setara): membuang `status='PUBLISHED'` dari langkah published, karena CHECK `chk_event_outbox_published_at` menjamin hanya baris PUBLISHED yang punya `published_at`; filter dipertahankan agar memakai index parsial.

## M6-T20 — Job `memory_cleanup` (kerangka) + `redisstore.MemVerBumper`
- **Baru:** `internal/jobs/memory_cleanup.go` (`ExpiredMemoryStore`, `MemVerBumper`, `MemoryCleanupJob`), `internal/redisstore/memver.go` (`MemVerBumper`: `INCR` + `EXPIRE` 30 hari per user dalam satu pipeline), `RetentionRepo.DeleteExpiredMemories` + query sqlc `SysDeleteExpiredMemories` di `db/queries/retention.sql`. Tes: `memory_cleanup_test.go` (unit), `internal/redisstore/memver_integration_test.go`, `internal/postgres/memory_expired_integration_test.go`.
- **Verifikasi:** Step 2 merah (build: simbol belum ada); `go test ./internal/jobs/... -race -count=3 -run TestMemoryCleanup` hijau; `go test -tags integration ./internal/redisstore/... ./internal/postgres/... -race -count=3 -run 'TestMemVerBumper|TestDeleteExpiredMemories'` hijau; seluruh paket integration jobs/postgres/redisstore `-race` hijau (261 tes); `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu; `sqlc generate` stabil. Mutasi merah (tertangkap): `len==0` bukan `<1000`, batch 500, tanpa dedup user, Bump dilewati saat ada error, tanpa `EXPIRE`, TTL 7 hari, `SetNX` bukan `INCR`, ambang `expires_at` +2 jam, `ORDER BY DESC`, filter `status='ACTIVE'`. Mutan awal yang selamat: Bump memakai ctx job (dibatalkan) bukan `WithoutCancel`; ditambah `TestMemoryCleanup_CancelMidRunStillBumpsWithLiveContext`, kini tertangkap.

## M6-T21 — Job `account_purge`
- **Baru:** `internal/jobs/account_purge.go` (`PurgeStore`, `AccountPurgeJob`), `internal/postgres/purge_repo.go` (`PurgeRepo`), `db/queries/purge.sql` + `sqlcgen/purge.sql.go`. Tes: `account_purge_test.go` (unit), `internal/postgres/purge_repo_integration_test.go`, `internal/jobs/account_purge_integration_test.go` (DB nyata lewat `sessionenv`: sesi ditagih sungguhan sehingga ledger/reservasi/akun ada; `AssertReconciled` jalan di akhir tiap tes).
- **Verifikasi:** Step 2 merah (build: simbol belum ada); `go test ./internal/jobs/... -race -count=3 -run TestPurge` hijau; `go test -tags integration ./internal/postgres/... ./internal/jobs/... -race -count=3 -run 'TestPurgeRepo|TestAccountPurge'` hijau; seluruh paket integration jobs/postgres/redisstore `-race` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...`; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Tes kunci: `TestAccountPurge_RerunAfterCrash_Idempotent` (sub-tes per langkah, crash disuntik di memories/evaluations/learner/transcripts/summaries/anonymize: `purged_at` NULL dan email belum dianonimkan setelah run 1, run 2 menuntaskan, run 3 no-op dan `purged_at` tidak ditulis ulang), `TestAccountPurge_Max50PerRun_OldestFirst` (55 user: 50 tertua dulu, lalu sisa 5), `TestAccountPurge_OnlyDeletedPastDelay` (batas `deleted_at == now-delay` tidak ikut), `TestAccountPurge_RemovesDataKeepsFinancials` (akun, reservasi, ledger, sessions, processed_events, admin_audit_logs tak berubah; user muda/ACTIVE utuh), `TestAccountPurge_NoPIIRemainsInSummaries`. Mutasi merah (tertangkap): anonymize sebelum summaries, langkah gagal tidak menghentikan user itu, gagal satu user menghentikan run, limit 100, delay diabaikan, batch transcript 500, urutan evaluasi/learner terbalik, metrik users_purged dihapus; SQL: `<=` pada cutoff, tanpa `purged_at IS NULL`, `ORDER BY ... DESC`, password_hash/phone tak diubah, summary tak di-NULL, loop batch berhenti setelah batch pertama, event transcript tak dihapus, filter tambahan pada memories dan evaluations, cek "sudah purged" dibalik. Mutan setara (tak tertangkap, dijamin constraint/FK): membuang `status='DELETED'` dari daftar kandidat (CHECK `chk_users_deleted_at`), membuang guard status pada `Anonymize` (CHECK `chk_users_purged_at` menolak), penghapusan `evaluation_findings`/`learner_skill_states` eksplisit (juga terjadi lewat `ON DELETE CASCADE`).

## M6-T22a — Backoff terbatas pada fallback `Consumer.retry`
- **Ubah:** `internal/amqp/consumer.go` (`retryFallbackDelay` 1 s, `holdBeforeRequeue`, hasil `retry_publish_failed`), tes di `consumer_process_test.go`, `consumer_run_integration_test.go`, `export_test.go`. Rincian di deviations § M6-T22.
- **Verifikasi:** `go test ./internal/amqp -race -count=3` dan `-tags integration -run 'TestRetryFallback|TestRetry_|TestDeliveryLimit'` hijau. Mutasi tertangkap: jeda dibuang (tes unit menunggu `asked` tak pernah terisi dan integration melampaui 5 delivery), jeda tidak dibatalkan oleh `Shutdown` (`TestProcess_RetryPublishFails_DelayCutShortByShutdown` merah).

## M6-T22 — `cmd/worker`: wiring, `--handlers`, `dlq-replay`, `run-job`, readiness dinamis, shutdown
- **Baru/ubah:** `cmd/worker/{main,wiring,commands,lifecycle}.go` (server mode, `handlerSet`, `buildJobs` 8 job, seam `buildConsumers`/`consumerBuilders` kosong → `errHandlerNotImplemented`, `Lifecycle`, `stopConsumers`, `dlqReplayCmd`, `runJobCmd`, exit 2 untuk kesalahan pemakaian), `internal/platform/health/ops.go` (`OpsOptions.LiveChecks`, `/readyz` 503 `not_ready`), `internal/amqp/conn.go` (`Reconnect` diserialisasi), `Makefile` (`run-worker` memakai `WORKER_HANDLERS`). Tes: `cmd/worker/{handlers,lifecycle}_test.go`, `cmd/worker/{main,worker}_integration_test.go`, `internal/platform/health/live_test.go`, `internal/amqp/conn_integration_test.go`. Rincian dan deviasi di deviations § M6-T22.
- **Verifikasi:** Step 2 merah (health: `LiveChecks` belum ada; worker: simbol belum ada; `TestConn_ConcurrentReconnect_DialsOnce` merah: 8 dial untuk 8 pemanggil). `go test ./cmd/worker ./internal/platform/health -race -count=3` hijau; `go test -tags integration ./cmd/worker ./internal/platform/health -race -count=3` hijau (`TestWorker_RunCore_ReadyAndMetrics`, `TestWorker_RunJob_*`, `TestWorker_DLQReplay_Subcommand`, `TestWorker_StopConsumers_GraceThenCancelThenRequeue`); `go test -tags integration ./internal/amqp ./internal/platform/health ./cmd/worker -race -count=3` 573 tes hijau; `go build ./...`, `go vet ./...`, `make build`, `go test -race ./internal/... ./cmd/...`, `go test ./test/arch`, `make arch-check` exit 0, `make lint` dan `golangci-lint --build-tags integration` 0 isu. **Binary nyata** (`bin/worker`, `WORKER_HANDLERS=outbox,sweeper,cleanup`, container Postgres/Redis/RabbitMQ sendiri, migrasi dari `migrations/*.up.sql`): `/readyz` `{"status":"ready"}` (≈1 s), `/metrics` memuat `kotoki_outbox_pending 0`, `kotoki_job_runs_total{job="session_sweeper",result="ok"} 2`, `kotoki_rabbitmq_queue_messages{queue="evaluation.session_completed"} 0`; `run-job outbox_retention` → `job=outbox_retention ran=true` exit 0, `run-job nope` exit 2, `dlq-replay --queue evaluation.session_completed --dry-run` → `listed=0` exit 0; `--handlers=evaluation` exit 1 (`errHandlerNotImplemented`); SIGTERM → log `stopped`, exit 0. Container dihentikan dan dihapus.
- **Mutasi (tertangkap):** `ShutdownConsumers` menerima `grace` penuh bukan sisa; langkah scheduler dibuang; langkah setelah publisher dilewati bila ada error; slack `shutdownSlack` dihapus (setelah tes diperkuat: awalnya selamat karena selisih µs); `liveChecksPass` dilewati; `liveCheckTimeout` semenit atau 100 ms (setelah tes diperkuat: awalnya selamat karena kegagalan lain juga 503); `Publisher` selalu true; dedup consumer dihapus; exit 2 → 1; nama job tak diperiksa; `keepAlive` dibuang (readiness tak pulih); live check RabbitMQ dibuang; `Consumer.Shutdown(ctx, 0)` di `stopConsumers`; semaphore `Reconnect` dibuang. Mutan setara (tak tertangkap): menghapus `stopRun()` di `stopConsumers` (`Run` sudah berhenti lewat sinyal stop `Shutdown`; panggilan itu hanya sabuk pengaman).
- **Masalah terbuka:** Task 24 (E5-varian lewat `worker run-job session_sweeper`) tidak akan pernah menjalankan rule (b), lihat deviations. Window 503 pada `TestWorker_RunCore_ReadyAndMetrics` pendek (reconnect < 1 s); sampler loop ketat menangkapnya di 6 run beruntun (3 + 3 + run awal) tanpa flake.

## M6-T23 — Gerbang integration M6: alur penuh, race, pod-kill setara, Redis flush
- **Baru:** `test/integration/m6_async_test.go` (tag `integration`), 7 tes + 2 subtes; harness `sessionenv` (jam palsu) + RabbitMQ nyata, tanpa consumer terpasang kecuali `DLQReplay`. Tidak ada kode produksi baru dan tidak ada bug yang ditemukan.
- **Tes:** `FlowsToThreeQueues` (lifecycle M3 `USER_ENDED` → `PublishBatch` → 1 pesan di `evaluation|memory|summary`, 0 di learner; `evaluation.completed` hanya ke learner; `memory.extracted` ke `kotoki.events.unrouted`), `ExactlyOnce_RuntimeVsSweeperVsPublisher` (20 sesi ENDING > 60 s; per sesi `Finalize(fence)` dan `Sweeper.Tick` paralel, 2 publisher paralel; 1 outbox, 1 CONSUME, 1 RELEASE per sesi, tiap queue tepat 20 pesan dengan 20 `event_id` unik), `PodKillEquivalent_HeartbeatLoss_FailsOnceWithFairCharge` (via `Scheduler.RunOnce("session_sweeper")` + `PgLocker`: tick `t0+130s` belum memproses dan gauge suspended=1; tick `t0+200s` → `FAILED`/`SERVER_ERROR`, `ended_at = heartbeat terakhir`, charge 90 (120 − 30) / varian aktif 50 s → 0 tanpa CONSUME; 1 outbox `final_status=FAILED`; 1 pesan per queue; run ketiga no-op; tak ada reservasi `HELD`), `RedisFlush_DoesNotFailHealthySessions` (25 sesi, `FLUSHALL`, tick sampai +60 s: semua tetap ACTIVE, `mass_heartbeat_loss_total ≥ 1`, log ERROR `sweeper.mass_heartbeat_loss` terekam, tetap ACTIVE setelah heartbeat ditulis ulang), `HandoffSessions_ExcludedFromMassLoss` (30 handoff + 5 hilang: aturan (b) memproses tepat 5, tanpa mass loss; sesudah > 30 s aturan (e) menutup 30 dengan `RECONNECT_TIMEOUT`, `ending_at = handoff_at`, charge 50), `DLQReplay_Works` (consumer nyata gagal permanen → DLQ, setelah "perbaikan" `ReplayDLQ` → 1 efek dan 1 baris `processed_events`, queue lain tak tersentuh), `OutboxSurvivesBrokerOutage` (`CloseAllConnections`: `attempts` 0 setelah 3 poll gagal; setelah `Reconnect` terkirim tepat sekali).
- **Verifikasi:** tes lulus pada run pertama (tidak ada kode produksi yang bisa dibuat merah untuk Step 2; keandalan tes dibuktikan dengan mutasi, lihat bawah). `go test -tags integration ./test/integration -race -run TestM6_ -count=3` hijau; `make test-integration` penuh (52 paket) hijau; lint `--build-tags integration` 0 isu. Mutasi merah: `HeartbeatGuard.Enabled` selalu true (pod-kill, kedua varian), `massLossMinCandidates` 2000 (RedisFlush), filter `handoff_at IS NULL` dihapus dari `ListActiveSessionsForHeartbeat` (Handoff).

## M6-T24 — E2E pod-kill dan client disconnect
- **Baru:** `test/e2e/m6_podkill_test.go` (`TestM6_PodKill_SweeperCompletesOnce`, `TestM6_ClientDisconnect_NoCharge`); **diubah:** `test/e2e/env_test.go` (`env.startWorker`: biner `cmd/worker` dibangun `-race` saat pertama dipakai, env eksplisit lewat `baseEnv` + `WORKER_HANDLERS`, `RABBITMQ_URL` dari `testutil.NewRabbitMQ` per tes, `SWEEPER_INTERVAL=2s`). Tidak ada kode produksi baru, tidak ada bug ditemukan.
- **Pod-kill:** worker `--handlers=sweeper,outbox` hidup sebelum sesi dibuat; sesi USER_FIRST pada proses realtime tunggal, `kill -9` tepat 70 s setelah `session.started`; heartbeat terakhir dibaca dari Redis; sesi berpindah dari ACTIVE ke `FAILED`/`SERVER_ERROR` 41 s setelah kill; `ended_at` = heartbeat terakhir (±1 ms), `charged_seconds = ceil(aktif ms) − 30` persis dan lebih kecil dari waktu sampai kill; tepat 1 `event_outbox` (producer `WORKER`, `final_status=FAILED`), terbit 1 pesan per queue sesi (pesan sesi lain difilter lewat `aggregate_id`), ledger `RESERVE/CONSUME/RELEASE` dengan nilai persis, tanpa reservasi `HELD`, `AssertReconciled` bersih; 6 s kemudian tidak ada event/pesan tambahan.
- **Client disconnect:** `Drop{For: 40s}` setelah 2 s; runtime menutup `COMPLETED`/`RECONNECT_TIMEOUT`, `disconnected_seconds ≥ 29`, `charged_seconds = ceil((ending_at − started_at) − disconnected)` dan < 15, 1 outbox (`COMPLETED`), 1 pesan per queue, 1 `SESSION_COMPLETED`, `AssertReconciled` bersih; resume klien sesudah jendela ditolak `409 SESSION_NOT_CONNECTABLE` (diharapkan).
- **Verifikasi:** `go test -tags 'e2e m5' ./test/e2e -race -run TestM6_ -timeout 10m` hijau (126 s + 45 s); seluruh suite e2e `-tags 'e2e m5' -race` hijau (545 s); lint `--build-tags e2e` 0 isu.

## M6 — Gerbang milestone
Dijalankan 2026-10-16 pada `feat/m6-async-jobs` (Docker OrbStack, `TESTCONTAINERS_RYUK_DISABLED=true`). Hasil per item:
1. **Build/vet/lint/arch — LULUS.** `go build ./... && go vet ./...` bersih; `golangci-lint run` → `0 issues` (juga `--build-tags integration` dan `--build-tags e2e` pada `test/...`); `make arch-check` exit 0, `CGO_ENABLED=0 go build ./cmd/api ./cmd/worker` bersih.
2. **`make test` — LULUS** (`-race -short`; `internal/events` 100,0 %, `internal/jobs` 85,9 %, `internal/amqp`, `cmd/worker` hijau).
3. **`make test-integration` — LULUS** (dijalankan dua kali penuh, `-race -p 1`, 52 paket, 1.810 tes PASS pada run berverbose; daftar tes gerbang semuanya ada dan lulus: `TestDeclareTopology_` 1, `TestRouting_` 3, `TestRetryQueue_5s_` 1, `TestPublishBatch_` 18, `TestRetry_FirstRung_RealBroker`, `TestPoison_` 3, `TestIdempotency_` 3, `TestTrace_OutboxToConsumer_SameTraceID`, `TestReplayDLQ_` 7, `TestPgLocker_` 3, `TestReservationCleanup_` 5, `TestBalanceReconcile_` 9, `TestPartitionRepo_` 9, `TestRetention_` 10, `TestOutboxRetention_` 9, `TestAccountPurge_` 5, `TestWorker_` 5). Tes M5 yang flaky diperbaiki, lihat di bawah.
4. **`go test -tags integration ./test/integration -race -run TestM6_ -count=3` — LULUS** (27 run: 9 tes x 3 termasuk subtes).
5. **`go test -tags 'e2e m5' ./test/e2e -race -run TestM6_ -timeout 10m` — LULUS** (`TestM6_PodKill_SweeperCompletesOnce` 126 s, `TestM6_ClientDisconnect_NoCharge` 45 s); seluruh suite e2e juga hijau (545 s).
6. **Coverage — LULUS untuk butir gerbang, dengan satu perbaikan perkakas.** `internal/events` 100 %; total `internal/...` (di luar adapter) 76,7 % ≥ 75 %. `make test-cover-integration` semula GAGAL (`internal/postgres` 38,0 % karena `sqlcgen` ikut dihitung 0 % dan hanya tes di dalam paket yang dihitung; `internal/postgres` sendiri 67,1 %): akar masalah adalah target Makefile hanya menjalankan tes ketiga paket adapter dan tanpa `-coverpkg`, padahal repository adapter M6 diuji dari `internal/jobs`, `cmd/worker`, dan `test/integration`. Target kini menjalankan `./...` dengan `-coverpkg=./internal/{postgres,redisstore,amqp}/...`; hasil terukur (ekuivalen dengan target baru): `internal/amqp` 89,0 %, `internal/postgres` 80,2 % (`sqlcgen` 72,9 % terpisah), `internal/redisstore` 88,2 %, `covercheck` dengan `thresholds.integration.txt` lulus. **`make test-cover` (unit) tetap gagal pada `internal/session` 87,7 % < 90 %: pre-existing** (di `main` 87,8 %, dibandingkan lewat `git worktree`); bukan butir gerbang M6, tidak diubah.
7. **Manual — LULUS.** `docker compose up` (postgres, redis, rabbitmq) + `make migrate-up`; `WORKER_HANDLERS=outbox,sweeper,cleanup make run-worker`: `curl -fsS localhost:9092/readyz` → `{"status":"ready"}`; `/metrics` memuat `kotoki_outbox_pending 0` dan `kotoki_job_runs_total{job="session_sweeper",result="ok"} 1` (muncul setelah jitter awal job); `go run ./cmd/worker run-job outbox_retention` → `job=outbox_retention ran=true`; SIGTERM → log `stopped`. Compose di-`reset`, `.env` sementara dihapus.
8. **`docs/ops/rabbitmq.md` — LULUS:** `grep -c 'kotoki-worker\|5671\|%2Fkotoki'` = 4 (≥ 3). **Job CI `integration` hijau — BELUM BISA DIVERIFIKASI** sampai PR dibuat (cabang tidak di-push). Catatan: `gh run list --workflow ci.yml` menunjukkan `main` dan cabang M5 terakhir MERAH karena sebab pre-existing di luar M6: job `lint` gagal pada `make modelname-check` (juga gagal lokal: literal model di `internal/provider/config.go`, `internal/redisstore/realtime_state.go`, `internal/testutil/{fakes/runtime_store,rtharness/harness}.go`) dan job `integration` gagal pada `internal/wsgateway` `TestLimiterErrorFailsOpen` (hanya di CI; lulus lokal). Keduanya milik M5 dan tidak diperbaiki di sini.
9. **Entri log dan deviasi — LULUS:** entri M6-T1..T24 di `docs/implementation-log.md`; deviasi dry-run DLQ (T12), tanpa library JSON Schema (T2), readiness dinamis (T22), dan § M6-T23/T24 di `docs/implementation-deviations.md`.

**Perbaikan tes lama (M5).** `TestContentRepoUpdateCharacterLockAndMerge` flaky: `created_at` karakter diisi `now()` oleh Postgres di container, sedangkan `updated_at` diisi `time.Now()` host; bila jam VM container mendahului host, `updated_at` < `created_at` dan asersi `UpdatedAt >= CreatedAt` gagal (gagal dua kali berturut-turut pada `make test-cover-integration`, lulus pada 40 pengulangan terpisah). Kini timestamp update diambil dari jam database (`SELECT clock_timestamp()` dalam transaksi yang sama); asersinya tidak diubah.

**Bug nyata yang ditemukan di M6 gerbang:** tidak ada (Task 23/24 hijau tanpa perubahan kode produksi).

## M7-T1 — Preflight kontrak M3/M4/M6 + kode error evaluasi
- **Baru:** `internal/m7preflight_test.go` (pemeriksaan kompilasi simbol `prompt`, `provider`, `content`, `events`, `amqp` yang dipakai M7), `internal/platform/errs/m7codes_test.go`. Tidak ada kode produksi: kedua kode error sudah ada.
- **Verifikasi:** `go test -race` errs/httpapi/internal `-run TestM7` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2212); `make arch-check`; `make lint` 0 isu. Mutasi status `SESSION_NOT_ENDED` 409 → 400 merah.

## M7-T2 — Domain evaluasi: enum, spesifikasi finding, overall, insufficient, `Config`, `Metrics`
- **Baru:** `internal/evaluation/{enums,rules,config,metrics}.go` (paket baru; hanya impor `content`, `platform/metrics`, prometheus), `internal/platform/config/worker_eval.go`. **Diubah:** `platform/config/{config,validate}.go` (`Worker.Evaluation/Summary`), `.env.example`.
- **Tes:** `rules_test.go`, `metrics_test.go`, `worker_eval_test.go`.
- **Verifikasi:** Step 2 gagal (build, undefined), lalu `go test ./internal/evaluation/ ./internal/platform/config/ -race` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2224); `make arch-check`; `make lint` 0 isu. Mutasi, semua merah (11): pembulatan overall dihapus, `<` jadi `<=` pada active/user turns, `corrected` EXCESSIVE_FILLER jadi Required, format `rubric-v%d`, default confidence 0.5, default speech 25, bucket histogram nil, guard nil metrik dibalik, `Evaluation.validate`/`Summary.validate` tidak dipanggil.

## M7-T3 — Transcript, statistik, kelayakan turn
- **Baru:** `internal/evaluation/transcript.go` (`TurnRow`, `EligibleUserTurn`, `ComputeStats`, `BuildTranscript`), `transcript_test.go`.
- **Verifikasi:** Step 2 gagal (build, `TurnRow` undefined), lalu `go test ./internal/evaluation/ -race -count=3` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau (2238); `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah (14): status `DISCARDED` eligible, ambang rune 1, speech dibulatkan alih-alih floor (awalnya lolos; data tes diubah ke jumlah 4,6 s lalu merah), `active_seconds` tidak di-clamp, durasi negatif tidak di-clamp (awalnya lolos; ditambah tes durasi terbalik), spoken text diabaikan, penanda interrupted dilepas, batas AI 199, keep 119, cabang `<= keepLastTurns` dihapus, DISCARDED dirender, `chars/s` memakai semua rune, batas `maxChars` dilonggarkan.

## M7-T4 — EvaluationPromptData, formalitas sesi (S-17), fixture
- **Baru:** `internal/evaluation/prompt.go` (`EvaluationPromptData`, `PromptInput`, `BuildPromptData`, `ExpectedFormality`, `FindingTypesText`, `PatternKeyExamplesText`, `PromptFixtures`, `ExampleOutput`), `prompt_test.go`.
- **Verifikasi:** Step 2 gagal (build, `undefined`), lalu `go test ./internal/evaluation/ -race -count=3` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau; `make arch-check`; `make lint` 0 isu. Mutasi merah: KEIGO_LIGHT dipetakan ke frasa KEIGO, kelompok `optional` dibuang, baris grammar selalu ditambahkan, fallback akhir diubah (setelah ditambah kasus tes), `json.Unmarshal` error diabaikan, contoh pattern key dibuang, statistik speech tertukar. Mutan setara: helper `repeatRunes` tanpa `+1` (panjang unit transcript membagi 60.000 persis).

## M7-T5 — Seed prompt EVALUATION (+ `ApplySeed`) dan tes render/schema
- **Baru:** `internal/testutil/seed.go` (`ApplySeed`), `internal/evaluation/testdata/output_schema.json`, `test/integration/m7_seed_test.go` (`TestEvaluationSeedPrompt`, `TestSchemaCopyMatchesSeed`). **Diubah:** `seed/0070_prompts.sql` (version EVALUATION v1, id `...000d-...0004`), `test/integration/m3_seed_test.go` (`expectedPromptVersionCount` 3, `init()` `RegisterFixtures("EVALUATION", ...)`), `internal/evaluation/prompt.go` (fixture maksimal `rubric-v1`).
- **Verifikasi:** Step 2 merah (`content: not found`); `go test -tags integration ./test/integration -race -count=3 -run 'TestEvaluationSeedPrompt|TestSchemaCopyMatchesSeed|TestSeed'` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi seed merah: `seed` 42→41, `{{.RubricVersion}}` dihapus, `{{.Transcript}}` salah nama (merusak validasi katalog M3), `maximum` skor 100→99 (salinan skema), `Expected politeness` dibuat konstan; dua mutasi (teks `difficulty level`, daftar `EvaluationFocus`) awalnya lolos, tes diperkuat dengan cek tiap field di user prompt, kini merah.

## M7-T6 — Fake evaluation: fixture rubric + finding turunan transcript
- **Baru:** `internal/provider/fake/fixtures/kotoki_evaluation_rubric_v1.json`, `internal/provider/fake/rubric_findings.go` (`RubricFindings`), `rubric_findings_test.go`. **Diubah:** `internal/provider/fake/eval.go` (hook setelah substitusi `{{score}}` bila `SchemaName == "kotoki_evaluation_rubric_v1"`), `test/integration/m7_seed_test.go` (`TestFakeRubricFixtureMatchesSeedSchema`).
- **Verifikasi:** `go test ./internal/provider/fake/ -race -count=3` hijau; `go test -tags integration ./test/integration -race -count=3 -run TestFakeRubricFixture` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah: hook dimatikan, modulo 3 digeser, 6 rune jadi 7, confidence 0.9 jadi 0.8, `corrected` tanpa `です`, `turn_seq` memakai indeks, pembuangan whitespace dan himpunan tanda baca dihapus (mutasi `unicode.IsPunct` bertahan sehingga kode itu dibuang, lihat deviasi).

## M7-T7 — `EvalOutput`, validasi schema + bahasa
- **Baru:** `internal/evaluation/{rubric,validator}.go` (`EvalOutput`, `RawFinding`, `DimensionScore`, `Signal`, `Recommendation`, `OutputError`, `ErrInvalidModelOutput`, `SchemaCache`, `ParseOutput`, `CheckLanguage`), `validator_test.go`. `santhosh-tekuri/jsonschema/v6` dan `x/text` sudah ada di `go.mod` (tidak ada perubahan).
- **Verifikasi:** Step 2 gagal (build, `undefined`), lalu `go test ./internal/evaluation/ -race -count=3` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Mutasi merah: `Round` jadi `Floor`, normalisasi angka dilepas, syarat huruf Latin dilepas, ambang 50% (diperlonggar 25%, `>` alih-alih `>=`, dihapus), segmen kutipan 「」, cek summary, cek komentar dimensi kelima, cache tak dipakai, guard integral `turn_seq`, `Unwrap`, `ー`, digit (empat mutan awalnya lolos: ambang 25%, `ー`, digit, ditutup dengan kasus batas tambahan). Mutan setara: `DisallowUnknownFields` dilepas (skema `additionalProperties:false` sudah menolak lebih dulu; dipertahankan sebagai pertahanan berlapis sesuai plan).

## M7-T8 — Filter finding per aturan (§10.7.2)
- **Baru:** `internal/evaluation/filter.go` (`TurnRef`, `Finding`, `FilterResult`, `FilterFindings`, `NormalizeForMatch`, `TooManyRejected`, konstanta `Drop*`), `filter_test.go`.
- **Verifikasi:** Step 2 gagal (build, `undefined`), lalu `go test ./internal/evaluation/ -race -count=3` hijau; `go build ./...`; `go test -race ./internal/... ./cmd/...` hijau; `make arch-check`; `make lint` dan lint `--build-tags integration` 0 isu. Sekitar 38 mutasi pada filter semuanya merah (ambang confidence `<`/`<=`, batas 15/3, paksa INFO/corrected null STRENGTH, `TooManyRejected` `>`/`>=`, kecocokan dimensi, normalisasi kedua sisi, kunci duplikat tiap komponen, urutan MAJOR/confidence, penghitungan `Rejected` per alasan, `TurnID`, himpunan tanda baca `・`, NFKC, whitespace, validasi dimensi STRENGTH); dua mutan awalnya lolos: guard turn ganda (ditambah `TestFilterDuplicateTurnRefFirstWins`) dan `f.TurnSeq == nil` pada cek original (setara: `TurnRef` nol berteks kosong tetap gagal cocok).
