# Implementation Deviations

Penyimpangan dari spec dicatat per task dengan heading `## <TASK-ID>` agar
dapat ditautkan dari `docs/implementation-log.md` (`#<TASK-ID>`).

## M1-T5 — internal/platform/logging
- `go get` untuk otel menaikkan direktif `go` menjadi `1.26.0` (otel 1.47.0, x/sys 0.48.0). Constraint plan: modul tetap `go 1.25`. Dipin: `go.opentelemetry.io/otel{,/sdk,/trace,/metric}` `v1.38.0` dan `golang.org/x/sys v0.35.0` (keduanya tidak mensyaratkan go > 1.25). Jangan jalankan `go get -u` tanpa memeriksa `head -5 go.mod`.

## M1-T11 — podman sebagai pengganti docker
- Seluruh task container (testcontainers, compose, build image) dijalankan dengan podman rootless: `systemctl --user start podman.socket`, `DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock` (Makefile meng-export otomatis bila `docker` tidak ada). `podman compose` memakai provider `docker-compose`.
- Integration test wajib `-p 1` (nama reaper testcontainers bentrok antar paket paralel di podman); `make test-integration` sudah memakainya. Run berurutan sangat cepat kadang gagal `reaper_default network not found`; ulangi setelah ±15 s.
- `testutil.MustInsert(ctx, t, db, table, row)` — urutan argumen berbeda dari brief (`t, ctx`) agar lolos revive `context-as-argument`.

## M1-T20 — pgvector-go
- `github.com/pgvector/pgvector-go v0.3.0` ditambahkan (allowlist D-003 via R-30); sqlc mengimpornya untuk `vector(1536)`.

## M1-T23 — urutan Serve vs RunStartupChecks
- Listener ops mulai `Serve` sebelum `RunStartupChecks` (brief: sesudah) supaya `/readyz` bisa melapor `starting`.

## M1-T24 — compose untuk podman
- `rabbitmq`: `user: "999:999"`; `MIGRATE_IMAGE=docker.io/migrate/migrate:v4.18.3`; Makefile `CONTAINER_ENGINE`/`COMPOSE`.

## M1-T18 — ambang coverage adapter
- Ambang `internal/postgres|redisstore|amqp` dipindah ke `thresholds.integration.txt` + `make test-cover-integration`; `TOTAL internal` run unit mengecualikan adapter (`except=`).
