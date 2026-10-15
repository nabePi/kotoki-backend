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
