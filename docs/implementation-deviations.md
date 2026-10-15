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

## M4-T22 — template v1 dan fixture maksimum
- Baris `Memory rules` di template seed v1 mengakhiri kalimat dengan "(Bab 11 §11.7)" pada spec; itu rujukan silang dokumen, bukan teks prompt, sehingga dibuang dari `prompt.ConversationV1`/`OpeningV1`. **Seed M3 MUST memakai teks konstanta ini**, bukan menyalin ulang dari spec.
- `FixtureMaximal` mengisi `persona` (3000 rune), `situation` (2000), `greeting_hint` (200) pada batas penuh dan jumlah item pada batas (6 task, 8 constraint, 8 grammar target, 20 vocab hint, 20 memory item, 3 episode), tetapi panjang tiap item berulang dibuat realistis (40–60 rune). Semua field pada batas literal sekaligus sudah melebihi 12.000 karakter tanpa teks template (3000 + 2000 + 6×200 + 8×200 + 8×100 > 8.600), jadi syarat "fixture maksimum ≤ 12.000" hanya bisa dipenuhi dengan cara ini (render maksimum saat ini ≈ 10,9 rb karakter CONVERSATION dan 11,2 rb OPENING).
- `prompt.ResetFixturesForTest` mengembalikan fixture bawaan paket (CONVERSATION/OPENING) alih-alih mengosongkan registry sepenuhnya.

## M4 — keputusan atas ambiguitas spec
- `RealtimeRouter.CompleteText` dan `AsyncRouter.CompleteText` bertanda tangan sama tetapi kebijakan beda (1 attempt + tunggu slot 100 ms vs 3 attempt + 30 s) → ditentukan `RouterDeps.Role`; `RouterDeps.AsyncWait` (default 30 s) menjadikan tunggu slot async bisa diuji.
- `prices.yaml` dibaca dengan `gopkg.in/yaml.v3` (allowlist D-003).
- `NormalizeSpeakable` diekspor dari `prompt` (spec §7.6.2 menaruhnya di `conversation/japanese.go`, tetapi `SentenceParser` di `prompt` memerlukannya); M5 memakai `prompt.NormalizeSpeakable`.
- §8.1 butir 1 melarang `provider` mengimpor `platform/otel` sedangkan D-NEW-13-23 meminta span di layer provider → span dibuat di adapter vendor (M9); router/fake M4 tanpa OTel.
- Hash keputusan gagal fake = FNV-64a + finalizer splitmix64 (spec tidak menetapkan fungsi hash).
- `PROVIDER_CONCURRENCY_<VENDOR>_<CAP>` = 10 field eksplisit; instance fake memakai default kapabilitas.
- Galat parse bool `RouterConfig` menyebut nama field Go; pemetaan ke nama env dilakukan saat `platform/config` menanam `RouterConfig`.
- Rotasi koneksi STT membuka koneksi baru tanpa melewati breaker/semaphore lagi (sesi tetap memegang satu slot semaphore dan satu keputusan breaker dari `Open` awal); pseudocode §8.5.8 menulis "breaker+semaphore" untuk `Open` rotasi. Kegagalan `Open` rotasi tetap menjadi `STTError` dan masuk hitungan breaker lewat `ReportFinal`/`Record` hanya bila berupa ERROR vendor.
- `fake`: `CapConfig.HangAfter` / `FAKE_<INST>_<CAP>_HANG_AFTER` / fault `after_events` ditambahkan agar skenario S-7 (macet setelah N delta) bisa diuji.
- Wiring `RouterConfig`/`fake.Config` ke `platform/config` dan `cmd/*`, handler `GET /internal/providers` (hanya `HealthSnapshot` yang ada), serta observasi `kotoki_session_cost_usd` saat sesi terminal menjadi tugas M5/M6/M7.

## M3-T7 — validator konten
- Key prompt tanpa versi `PUBLISHED` di `ValidateCatalog`: ERROR hanya untuk `CONVERSATION`; WARNING `missing_published_prompt` untuk `OPENING`, `SUMMARY`, `EVALUATION`, `MEMORY_EXTRACTION` (versi SUMMARY/EVALUATION/MEMORY_EXTRACTION dan fixture-nya ditambahkan M7/M8; OPENING boleh tidak ada karena fallback `opening_line`, S-2).
- `provider_not_configured` (karakter `ACTIVE` dengan vendor TTS tidak enabled) berlaku di mode fake maupun real, mengikuti brief plan (spec §6.10.1 menyebut hanya mode real).
- Normalisasi huruf besar nama vendor diekspos sebagai `content.NormalizeCharacter` (validator mengembalikan daftar isu, bukan nilai); `ParseVoice` dan `ParseLLMParams` memeriksa key tak dikenal dari JSON mentah (`voice.providers`, `model` di `llm_params`, dst.) karena tipe Go tidak bisa memuat key asing.

## M3-T10 / T12 — admin konten
- §6.13.1 menulis action audit `CONTENT_CHARACTER_UPDATED` dan kolom `metadata`; implementasi memakai action `CHARACTER_UPDATED` dan kolom `details` sesuai CHECK `admin_audit_logs` (Bab 3).
- Retire version prompt `PUBLISHED` selalu 409 `CONTENT_VERSION_REQUIRED` (§6.13.1/§6.x: tidak boleh tanpa pengganti, dan hanya satu `PUBLISHED` per induk, jadi satu-satunya jalan adalah publish pengganti). Skenario e2e E12 yang me-retire `OPENING` lewat endpoint retire harus memakai jalur lain.

## M3-T13 / T15 — session
- `TurnRepository` tidak memuat `SetUserAudioKey` (§7.13.1) karena §5.3.2/D-099: audio user tidak pernah disimpan dan `session_turns` tidak punya kolom referensi audio.
- `MaxSeqAndGeneration` mengembalikan `(-1, 0)` bila belum ada turn; `Stats.hasAudio` = ada `stt_audio_ms` atau `tts_first_audio_ms`.

## M3-T21 — sweeper
- Kandidat aturan (b) memakai bentuk §9.9.2 (semua sesi `ACTIVE` non-handoff, LIMIT 5000, heartbeat dicek in-process via MGET per 500 key; kandidat bila heartbeat hilang/usang > 45 s **dan** `started_at < now − 45 s`), bukan teks §5.6.4 (LIMIT 2000 dan filter `started_at` di SQL). `ListActiveForHeartbeat(ctx, limit)` tidak menerima cutoff.
- Probe konektivitas Redis untuk `HeartbeatGuard` = `HeartbeatReader.Read(ctx, nil)` (adapter Redis menjalankan `PING`), bukan method terpisah, agar port `HeartbeatReader` tetap satu method.
- Aturan (e) memakai `Service.EndStaleHandoff`, yang memeriksa ulang `handoff_at IS NOT NULL AND handoff_at < cutoff` di bawah row lock (spec §9.9.2); sesi yang sudah di-claim runtime baru → no-op.

## M3-T24 — admin sesi
- Audit `SESSION_TRANSCRIPT_VIEW` ditulis **setelah** query turns berhasil (sesuai brief plan), bukan "sebelum data dikembalikan" secara harfiah di §3.12: 404/410 tidak meninggalkan baris audit; bila penulisan audit gagal, handler membalas 500 dan transcript tidak dikirim. `admin.Service.RecordTranscriptView` memakai transaksi singkatnya sendiri.
- Respons admin menyensor `provider_name`/`model_name`/`voice_id`/`voice.fallback` menjadi `"[REDACTED]"` (D-015, §5.9.8) dengan men-decode lalu meng-encode ulang `config_snapshot`, sehingga urutan key di respons alfabetis, bukan urutan spec; `voice.fallback` yang `null`/tidak ada dibiarkan.
- `status` turn yang sedang berjalan adalah `null` di respons admin (respons user tidak pernah memuat turn itu).
- Cursor events = `[created_at RFC 3339 nano, id]` (bukan milidetik) agar mikrodetik `timestamptz` tidak hilang di keyset; limit events default dan maks 100, limit turns 50/100 seperti endpoint user, list sesi 20/100.

## M5-T2 — port & fake
- `conversation.TurnRepository` hanya enam method yang dipakai runtime (tanpa `SetUserAudioKey`, D-099 seperti M3-T13); `*session.TurnRepository` memenuhinya.
- `SessionEventWriter.Append` mendapat parameter `turnID *uuid.UUID` dan `LearnerContextProvider` mendapat `Rerender` (keduanya tambahan dari plan, bukan §7.13.1).
- `ReleaseOwner(sid, pod, fence)` mencocokkan pod dan fence (Lua §7.9.2 membandingkan nilai `pod|fence` utuh).
- `fakes.AudioStore` adalah blob store generik (Put/Get/Delete): belum ada port yang memakainya dan audio user tidak pernah disimpan (D-099); Task berikutnya boleh mempersempitnya.
- Fake `RuntimeStore` mengabaikan TTL (tidak ada kedaluwarsa) dan memangkas stream tepat 500 (Redis `MAXLEN ~`).
- `memory.MemoryContext` memakai `//nolint:revive` karena nama ditetapkan §11.7.1.

## M5-T3 — decoder protokol WS
- Decoder `DecodeClientEvent`/`DecodeServerEvent` **mengabaikan** field payload (dan envelope) yang tidak dikenal, mengikuti §7.4.2 ("Field yang tidak dikenal diabaikan, forward compatibility"). §14.4 butir (c) menulis "decoder menolak field tak dikenal"; kedua teks bertentangan dan §7.4.2 dipilih karena menyangkut kompatibilitas client. `type` tak dikenal tetap ditolak (`ErrUnknownType`), `version != 1` → `ErrUnsupportedVersion`, payload bukan object/salah tipe → `ErrInvalidPayload`.
- `IsDurable("error")` selalu `false`: durable-nya `error` bergantung `ErrorPayload.Fatal` (hanya non-fatal), jadi diputuskan pemanggil.
- Jumlah golden WS = 27 (6 client + 21 server), bukan 28 seperti di brief task; setiap tipe di §7.4.4/§7.4.5 punya tepat satu golden.

## M5-T4 — katalog error WS
- Tabel §7.4.8 memuat 23 baris, bukan 24 seperti di brief task; `TestCatalog` mencakup semua 23.

## M5-T6 — aset APOLOGY
- Paket APOLOGY dienkode pada 24 kHz tetapi didekode oleh `Decoder` 16 kHz Task 5: Opus mendekode paket 20 ms ke laju keluaran mana pun (320 sampel @ 16 kHz), jadi tidak ada API baru.
- Bootstrap: `go:embed` gagal bila file belum ada, sehingga generator pertama kali dijalankan dengan file kosong sebagai placeholder.

## M5-T7 — fixture audio sintetis
- Total ukuran `testdata/audio/synth` 9 476 367 byte (98,5 s × 96 KB/s PCM16 48 kHz), melebihi "< 8 MB" §14.5; batas validator dinaikkan ke 10 000 000 byte (keputusan plan). Mencapai 8 MB berarti memangkas durasi wajib atau mengganti format (§14.5 mewajibkan WAV PCM16 48 kHz).
- Semua 10 fixture sintetis (bukan TTS dan bukan rekaman). "Minimal 4 fixture direkam penutur asli" §14.5 = tugas pemilik produk (M9, disimpan di `testdata/audio/` dan didahulukan oleh `audiofx.LoadFixture`); fixture sintetis tidak dapat dipakai untuk live STT (tidak bisa dipahami).
- Validator hidup di `internal/testutil/audiofx` (bukan `internal/testutil` seperti teks §14.5) karena memakai `conversation/audio` (cgo) dan root `internal/testutil` harus tetap cgo-free (§14.9.5).
- `ja_filler_pause` dan `ja_conjunction_pause` masing-masing 4 000 ms (durasi tidak disebut §14.5; dipilih agar total 98,5 s sesuai plan). `manifest.json` menambah field `segments` (rentang bicara) di luar `name/duration_ms/tags/expect`, dipakai `TestFixturesVADSanity` dan pemeriksaan jeda; `noise_only`/`silence_5s` memakai transcript placeholder `（雑音のみ）`/`（無音）` karena `.txt` wajib ada dan tidak boleh kosong.
