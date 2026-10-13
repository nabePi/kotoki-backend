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
