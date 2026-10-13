# Implementation Deviations

Penyimpangan dari spec dicatat per task dengan heading `## <TASK-ID>` agar
dapat ditautkan dari `docs/implementation-log.md` (`#<TASK-ID>`).

## M1-T5 — internal/platform/logging
- `go get` untuk otel menaikkan direktif `go` menjadi `1.26.0` (otel 1.47.0, x/sys 0.48.0). Constraint plan: modul tetap `go 1.25`. Dipin: `go.opentelemetry.io/otel{,/sdk,/trace,/metric}` `v1.38.0` dan `golang.org/x/sys v0.35.0` (keduanya tidak mensyaratkan go > 1.25). Jangan jalankan `go get -u` tanpa memeriksa `head -5 go.mod`.
