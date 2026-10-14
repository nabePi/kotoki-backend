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
