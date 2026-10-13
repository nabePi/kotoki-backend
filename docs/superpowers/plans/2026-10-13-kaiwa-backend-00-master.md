# Kaiwa Backend — Master Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement each milestone plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Membangun backend Kaiwa (latihan percakapan bahasa Jepang realtime dengan AI: STT → LLM → TTS streaming, balance server-authoritative, evaluasi/memory/learner pasca-sesi) sampai siap produksi untuk 1.000 sesi bersamaan.

**Architecture:** Monolit modular Go dengan layout per-fitur (spec §2.2): paket fitur berisi domain + service + port; adapter (`postgres`, `redisstore`, `amqp`, `objectstore`, `mailer`, `provider/<vendor>`) mengimplementasikan port; handler (`httpapi`, `wsgateway`) tipis; wiring manual di `cmd/*/main.go`. Tiga binary (`api`, `realtime`, `worker`) + `kaiwa-cli`. PostgreSQL = sumber kebenaran, Redis = state runtime, RabbitMQ = pengiriman async via transactional outbox.

**Tech Stack:** Go 1.25, chi, pgx + sqlc, golang-migrate, coder/websocket, go-redis v9, amqp091, aws-sdk-go-v2 (S3/MinIO), golang-jwt, argon2id, hraban/opus (cgo), go-webrtcvad (cgo), OpenTelemetry (trace) + Prometheus client (metrik), caarlos0/env, testify, testcontainers. Daftar lengkap & versi: spec D-003 (allowlist; library di luar daftar = ubah register dulu).

**Spec (sumber kebenaran, wajib dibaca bersama rencana ini):** `docs/superpowers/specs/2026-10-12-kaiwa-backend-design.md`. Rencana mengacu ke bab/§ spec (nomor § stabil; nomor baris tidak). Jika rencana dan spec berbeda, **spec menang**; catat di `docs/implementation-deviations.md`.

## Global Constraints

Berlaku untuk setiap task di semua milestone (nilai disalin dari spec):

- Go **1.25**, module path `github.com/nabePi/kotoki-backend`, module di root repo, satu `go.mod` (D-001, D-002).
- Layout & aturan paket spec §2.2 + D-005: paket fitur **tidak** mengimpor adapter/library infrastruktur; dependensi antar fitur asiklik sesuai graf §2.2 butir 3; nama paket huruf kecil tanpa underscore; wiring hanya di `cmd/*/main.go` (D-004); tanpa ORM, tanpa DI framework.
- `cmd/api` dan `cmd/worker` MUST dapat di-build `CGO_ENABLED=0`; hanya `internal/conversation/audio` dan `cmd/realtime`/`kaiwa-cli` memakai cgo (D-NEW-14-6, `make arch-check`).
- Semua PK `UUID` UUIDv7 dari port `idgen.Generator`; waktu dari `clock.Clock`; paket fitur dilarang `time.Now()`/`uuid.New*()` langsung (D-010, §2.2 butir 7).
- Timestamp `TIMESTAMPTZ` UTC; durasi **detik integer**; enum = `TEXT` + `CHECK`, nilai UPPER_SNAKE_CASE (D-010, D-011).
- Format error tetap `{"error":{"code","message","request_id","details"}}`, `message` bahasa Indonesia tetap per kode, ditulis hanya lewat `httpapi.WriteError` (D-013, §5.8.2); kode hanya dari katalog §5.10 (REST) / §7.4.8 (WS).
- Handler REST bertipe `Handle[B,R]`, tanpa `interface{}`; empat grup route; route diregistrasi per fitur (`httpapi/{fitur}_routes.go`) (§5.8.6).
- Logging `log/slog` JSON dengan redaksi (§13.7.1); tracing: span di setiap layer + `errs.New/Wrap` merekam lokasi (§13.7.2.1–2); **tidak** ada teks user/AI/prompt di log/span/metrik.
- Config hanya dari environment variable via `caarlos0/env` di `internal/platform/config`; tidak ada `os.Getenv` di tempat lain (§13.2).
- Semua test dijalankan dengan `-race`; unit test tanpa jaringan/Docker/jam dinding; integration pakai testcontainers (§14.1).
- Migration: `migrations/NNNNNN_name.{up,down}.sql` (golang-migrate, 6 digit), nomor & isi mengikuti spec §15.1; reversible; edit boleh sampai tag `v0.1.0`.
- Provider: adapter tanpa SDK vendor (`net/http` + `coder/websocket`); mode `PROVIDER_MODE=fake` first-class (D-007); nama vendor/kapabilitas UPPERCASE (R-6).
- Definition of Done per task: spec §14.9.2 (build, vet, lint 0 issue, `make test` race hijau, integration/e2e bila relevan, coverage §14.8, entri `docs/implementation-log.md`, penyimpangan di `docs/implementation-deviations.md`).
- Setiap task = TDD (test gagal dulu → implementasi minimal → hijau → commit). Satu commit per task, pesan `feat|test|chore|docs(<area>): <ringkas>`. Jangan commit secret atau file generated yang tidak terkait.
- Tag image/versi yang di spec ditulis `<PIN>` (mis. `grafana/otel-lgtm`, MinIO): pin ke rilis terbaru saat bootstrap dan catat di `docs/implementation-log.md`.

## Review Focus

Kondisi yang tersirat spec tetapi paling mungkin menggigit pengguna, dan milestone yang wajib menguji masing-masing (test-nya ditambahkan di plan pemilik):

1. **Klik ganda / retry `POST /sessions`** (jaringan putus, user menekan tombol dua kali): harus tepat satu sesi + satu reservasi, replay `Idempotency-Key` mengembalikan sesi yang sama, kunci sama payload beda → `409 IDEMPOTENCY_KEY_REUSED`, tanpa header → `400 IDEMPOTENCY_KEY_REQUIRED`, key berbeda saat sudah ada sesi terbuka → `409 SESSION_ALREADY_OPEN` (M3; spec §5.8.4).
2. **Mikrofon senyap atau hanya noise, dan ucapan sangat panjang** (> 45 s): tidak boleh memicu turn kosong; noise-only → turn `DISCARDED`; ucapan dipaksa selesai di `MAX_UTTERANCE`; filler `えっと…` tidak memotong user (M5).
3. **Barge-in dengan echo speaker** dan frame audio lama sesudah interupsi: audio generasi lama tidak boleh diputar/diproses; frame dengan `generation` lama dibuang client dan runtime tidak menghitung echo sebagai barge-in (M5).
4. **Client terputus di tengah turn / pod mati saat sesi berjalan:** saldo tetap benar (tagihan berhenti di `ENDING`, detik disconnect tidak ditagih), sesi diselesaikan sweeper tepat sekali, tidak ada reservasi yatim, `session.completed` terbit sekali (M3, M5, M6).
5. **STT final kosong, teks non-Jepang, atau LLM diblokir content filter:** STT final kosong → turn `DISCARDED` (spec §7.6.2); teks non-Jepang atau generation gagal/diblokir content filter → turn `FAILED` + APOLOGY; sesi tetap berjalan, tidak dihitung ke batas 3 turn gagal berturut-turut (khusus content filter), tagihan tidak berubah (M5, M4).

## Cara membaca rencana ini

| File | Isi |
|---|---|
| `…-00-master.md` (ini) | Urutan milestone, batas, kontrak antar-milestone, aturan eksekusi |
| `…-M1-foundation-schema.md` … `…-M10-hardening.md` | Satu rencana per milestone, task berurut, siap dieksekusi agent |

Eksekusi **berurutan** M1 → M10 kecuali tabel dependensi di bawah mengizinkan paralel. Satu milestone selesai bila semua task-nya DONE **dan** gerbang milestone (di akhir plan-nya) lulus. Jangan mulai milestone berikutnya sebelum gerbang lulus.

## Daftar milestone

| # | Plan | Deliverable (bisa diuji sendiri) | Bab spec | Bergantung pada |
|---|---|---|---|---|
| **M1** | Foundation & Schema | Repo bootstrap, `platform/*` (config, log, otel, errs, clock, idgen, health, metrics), lint `depguard`, CI, docker-compose, **seluruh 21 migration** + sqlc + `postgres.Store` + test migration | §2, §13.2, §14, §15 | — |
| **M2** | Identity, Auth & Admin | Register/login/refresh/logout/reset password, JWT, ticket WS, rate limit, consent, hapus akun, `balance_accounts`, endpoint admin (grant/adjust, audit) | Bab 3, §4 (grant/adjust), §5.8 | M1 |
| **M3** | Content, Balance & Session lifecycle | Konten + seed + resolver + katalog REST; reservasi/ledger/settlement; `POST /sessions`, cancel, state machine, `RuntimeService`, transcript/query, sweeper rules (tanpa runtime) | Bab 4, 5, 6 | M2, **M4** (paket `prompt` + `provider` ports) |
| **M4** | Provider layer, Prompt & Fakes | Port provider, router + breaker + fallback, `prompt` (Render, tokens, parser kalimat), fake STT/LLM/TTS/Evaluation/Embedding/Pronunciation, cost metering | Bab 8, §6.8 | M1 |
| **M5** | Realtime core (**demo pertama**) | `conversation` (turn engine, VAD, barge-in, context builder), protokol WS, `wsgateway`, Redis state, reconnect/takeover, drain, `kaiwaclient` + `kaiwa-cli`, e2e dengan fake provider | Bab 7 | M3, M4 |
| **M6** | Async backbone & jobs | Outbox + publisher, topologi RabbitMQ, consumer framework (retry/DLQ/idempotency), `jobs` scheduler + semua job terjadwal (sweeper, cleanup, partition, reconcile, purge) | Bab 9 | M3 (M5 untuk e2e penutup) |
| **M7** | Evaluation & Summary | Worker evaluasi (rubric, JSON schema, pronunciation), `GET /sessions/{id}/evaluation`, summary worker | Bab 10, §9.9.6 | M6, M4 |
| **M8** | Memory & Learner | Ekstraksi/dedup/retrieval memory, learner worker (proficiency), adaptive difficulty terhubung ke `POST /sessions` & prompt, endpoint profile/memory | Bab 11, 12 | M7, M5 |
| **M9** | Real providers & benchmark | Adapter ElevenLabs, Deepgram, OpenAI, Gemini, Azure; contract test `-tags live`; `kaiwa-cli provider-bench`; penetapan model & voice ID | Bab 8 §8.4, §8.10 | M5, M7 |
| **M10** | Hardening & operasi | Manifest k8s, dashboard/alert, load test gate G1–G4, failure injection, checklist produksi | Bab 13 | M8, M9 |

Paralel yang diizinkan: **M4 paralel dengan M2** (hanya butuh M1; **M4 harus selesai sebelum M3** karena M3 memakai `prompt.Render/Template`); **M6 paralel dengan M5** setelah M3; **M9 paralel dengan M7–M8** setelah M5. Selain itu berurutan.

## Kontrak antar-milestone (siapa memproduksi apa)

Nama persis ada di spec; tabel ini hanya menunjuk pemilik agar plan tidak mendefinisikan ulang.

| Kontrak | Pemilik (milestone) | Dipakai oleh |
|---|---|---|
| `errs.Code`, `errs.New/Wrap`, `errs.Messages`, `clock.Clock`, `idgen.Generator`, `config.Config`, `otel.Start/End` | M1 | semua |
| Semua tabel & migration `000001`–`000021`, `sqlcgen`, `postgres.Store`/`Tx` + `repotest` | M1 (DDL), repository per fitur dilengkapi di milestone fiturnya | semua |
| `auth.Principal`, `auth.Service`, ticket WS (`auth` + `redisstore`) | M2 | M3, M5 |
| `balance.Service` (reserve/settle/release, `ComputeCharge`), `session.Service`, `session.RuntimeService` (§7.13.1 signature), `content.Resolver` | M3 | M5, M6 |
| `provider.Router`, port STT/LLM/TTS/…, `prompt.Render/EstimateTokens/SentenceParser`, `provider/fake` | M4 | M5, M7, M8, M9 |
| `conversation.Manager`, protokol WS v1, `kaiwaclient` | M5 | M8 (adaptive), M10 (load) |
| `events.OutboxWriter`, `amqp` consumer framework, `jobs` scheduler | M6 | M7, M8 |
| `evaluation.Service` + event `evaluation.completed` | M7 | M8 (learner) |
| `memory.RetrievalService`, `learner.Service.PromptContext`, `ComputeAdaptiveDifficulty` | M8 | M5 (ports sudah ada di M5 dengan fake, diganti implementasi nyata di M8) |

Aturan integrasi: jika milestone memakai kontrak milestone yang belum selesai (mis. M5 butuh `memory.RetrievalService` sebelum M8), plan memakai **port di sisi pemakai + fake** dan milestone pemilik menggantinya dengan implementasi nyata, tanpa mengubah port.

## Aturan eksekusi untuk agent

1. Satu branch per milestone (`feat/m<N>-<slug>`), dari `main` setelah milestone sebelumnya di-merge; satu PR per milestone, gerbang milestone dicantumkan di deskripsi PR.
2. Per task: baca bagian spec yang dirujuk task → tulis test gagal → implementasi → `make lint test` (+ `test-integration`/`test-e2e` bila disebut) → commit. Template laporan task: spec §14.9.4.
3. Task hanya menyentuh file yang tercantum di bagian **Files**; temuan di luar itu dicatat di `docs/implementation-log.md`, bukan diperbaiki diam-diam.
4. DDL, rumus, angka, dan kontrak JSON **tidak disalin ke plan**; plan merujuk § spec dan test memuat nilai persisnya. Implementer membaca § itu.
5. Jika spec ambigu atau bertentangan: pilih interpretasi paling konservatif, catat di `docs/implementation-deviations.md`, lanjutkan; jangan berhenti menunggu kecuali task tidak mungkin diselesaikan.
6. Gerbang milestone wajib dijalankan di akhir plan; hasilnya (perintah + output ringkas) dicatat di `docs/implementation-log.md`.
