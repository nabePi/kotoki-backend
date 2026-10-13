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
