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

## M5-T8 — detector turn
- Nama konstanta Go `EventKind` memakai CamelCase (`EventSpeechConfirmed`, …) karena `revive` (var-naming) menolak ALL_CAPS; nilai string-nya tetap nama spec (`"SPEECH_CONFIRMED"`, …). `TurnState` bernilai `"LISTENING"` dst., sama dengan `rtstate.RT.TurnState`.
- `TextView.TurnOnsetMs()` adalah method (spec menulis `tx.TurnOnsetMs` seperti field); `InjectSilence` menerima `st, tx` karena memanggil `Push`.
- Ambang start (10/15) dan barge-in (15/20) diturunkan dari `VAD_SPEECH_START_MS/_WINDOW_MS` dan `BARGEIN_VAD_MS/_WINDOW_MS` (spec menulis literal dengan komentar nama env; default menghasilkan nilai yang sama). Cluster 3/5, mundur 60 ms, dan pembatalan kandidat barge-in pada silence ≥ 300 ms tetap literal seperti spec. `SPEECH_PAUSE` memakai `VAD_SPEECH_END_MS` (`cfg.SpeechEndMs`).
- `IsFillerOnly` mengikuti spec harfiah (perbaikan review): token filler dihapus dulu dalam satu lintasan kiri-ke-kanan (`strings.NewReplacer`, penghapusan tidak membentuk token baru: `"そあのの"` → false), baru whitespace/tanda baca/simbol (`prompt.NormalizeSpeakable`). Teks tanpa rune terucap (`"。"`, `"？"`, `"「」"`) = filler-only → ambang 2000 (bukan 500/800); `""` juga true tetapi `Threshold` menanganinya lebih dulu (800). `ー` (Lm) bukan tanda baca dan tetap ada, jadi `"あのー、えっと"` → false → 1400; `"え、っと"`/`"あ、の"` → false. Tidak ada deviasi dari spec di fungsi ini.
- `InjectSilence` mengembalikan semua event dari ms/20 frame (spec), termasuk `SILENCE_REACHED` berulang bila injeksi melewati ambang lebih dari satu frame; runtime (Task 19–20) memproses event pertama dan mengabaikan sisanya sesuai tabel §7.6.1.

## M5-T9 — sttState, ScriptedSTT, component test detector
- Kasus brief "speech < 200 ms (potongan 150 ms `ja_greeting_short`) → nol event" tidak berlaku dengan VAD asli: WebRTC VAD mode 2 punya overhang setelah burst (terukur konsisten di 3 fixture/9 posisi: +4 frame setelah ≤ 100 ms ujaran, +6 frame setelah ≥ 120 ms), sehingga 150 ms ujaran = 13 frame voiced (VAD aktif 260 ms) → `SPEECH_CONFIRMED` sah menurut D-076 ("VAD aktif ≥ 200 ms"). Detector (T8) dan fixture (T7) benar; angka brief yang keliru. Tes kini memeriksa kriteria spec: potongan 100 ms di dalam ujaran (9 frame, VAD aktif 180 ms) → nol event, dan potongan 150 ms dipatok sebagai karakterisasi (`SPEECH_CONFIRMED`). Akibat nyata: burst akustik ≥ ~120 ms sudah memulai turn; noise pendek tetap disaring `isDiscard` di baris 8. Bila produk ingin "200 ms akustik", perlu keputusan (mis. kompensasi overhang atau menaikkan `VAD_SPEECH_START_MS`); tidak diubah diam-diam.
- `MarkTurn(onsetMs int64)` menerima onset (brief/plan: `MarkTurn()`), karena `sttState` mengimplementasikan `TextView.TurnOnsetMs()`.
- `OnFinal` mengembalikan `FinalResult{Outcome, Commit, Purpose}` dan ada `ExpireTurnEnd(id)`/`ResumeTurn()`/`PendingCommits()` tambahan agar `Runtime.loop` dapat membedakan STT_COMMITTED_FINAL, TURN_END terlambat (setelah STT_FINAL_TIMEOUT), TURN_END yang turn-nya berlanjut (baris 9) dan DISCARD tanpa membaca state internal. Identitas per entri (§7.6.2 `turnID`) dimodelkan sebagai `CommitID` (urut, mulai 1, dikembalikan `Commit`) + nomor turn internal (`MarkTurn` ke-n); `at` tidak disimpan. FINAL{FromFinalize} tanpa commit pending diperlakukan sebagai segmen final biasa.
- FINAL commit (TURN_END maupun DISCARD) membuang partial berjalan (audio sebelum commit; untuk TURN_END teks FINAL menggantikannya). Segmen tidak pernah dipadatkan selama sesi (tumbuh linear dengan jumlah segmen).
- `ScriptedSTT`: buffer `Events` default 64 (kontrak port); buffer penuh membuang PARTIAL tertua (`DroppedPartials`), FINAL/CLOSED/BARRIER tidak pernah dibuang: bila tidak ada PARTIAL untuk dibuang, event menunggu di daftar overflow yang dipindahkan ke channel oleh panggilan berikutnya, dan `Close` menyerahkan overflow + CLOSED ke goroutine yang mengirimnya lalu menutup channel (tidak pernah panic/blok; goroutine hanya ada bila > 64 event non-PARTIAL tidak dikuras). Membuat ruang = kuras-lalu-isi-ulang channel, tidak atomik terhadap penerima konkuren (konsumen yang mengikuti tidak memicunya). `Barrier()` setelah `Close` mengembalikan 0 tanpa sentinel. `LoadTimeline` menolak key tak dikenal, data setelah array, dan teks kosong pada step non-final (FINAL kosong boleh).
- `ScriptedSTT`: step `final` hanya mengirim FINAL (bukan PARTIAL lalu FINAL); `SetFinalizeText` berlaku untuk satu `Finalize` berikutnya; `Barrier()` mengembalikan id `uint64` dan sentinel bertipe `EventBarrier` (bukan tipe `provider` resmi) — runtime/harness (T18) harus mengenali atau melewatinya. `ReceivedAt` dibiarkan nol (waktu = audio). Pola run brief tidak memuat `FinalCoversLatest`; nama tes disesuaikan (`TestFinalCoversLatestAcrossCommits`) agar cocok dengan `Commit`. Tidak ada tes `Switched` (spec §7.6.2 menyatakan tidak ada event PROVIDER_SWITCHED).

### Catatan serah-terima mengikat untuk Task 19/21 (perbaikan review M5-T9)
- Setiap `stt.Finalize()` disertai tepat satu `sttState.Commit(purpose)`; simpan `CommitID` dari commit `TURN_END` bersama timer `STT_FINAL_TIMEOUT`; saat timer jatuh panggil `ExpireTurnEnd(id)` (false = commit sudah dijawab/di-supersede, abaikan).
- Baris 9 (`PROCESSING` → `SPEECH_CONFIRMED`, turn sama berlanjut): panggil `ResumeTurn()` (menandai semua `TURN_END` pending milik turn saat ini, kedaluwarsa atau belum, sebagai superseded) dan hentikan timer STT. Turn yang berakhir lagi membuat commit `TURN_END` baru.
- `OnFinal` → aksi: `FinalTurnEnd` = STT_COMMITTED_FINAL (baris 7/8), **hanya** jika `res.Commit` = commit `TURN_END` terakhir turn itu (selalu benar karena commit lama di-supersede); `FinalTurnEndSuperseded` = tidak ada aksi (teks FINAL disimpan sebagai segmen final turn yang berlanjut, `sttMark` tetap; partial yang diterima setelah `ResumeTurn` tetap di belakang FINAL, partial sebelum resume diganti FINAL); `FinalTurnEndLate` = tidak ada aksi (teks + segmen sebelum commit dibuang, sesuai spec); `FinalDiscarded` = metrik `stt_discarded_commits_total`; `FinalSegment` = tidak ada aksi.
- Skenario (a) dua kali akhir turn sebelum FINAL#1: FINAL#1 → `Superseded{id1}` (bukan jawaban `TURN_END#2`, partial setelah resume tidak hilang), FINAL#2 → `TurnEnd{id2}`. Skenario (b) timeout lalu resume: resume mengalahkan timeout, FINAL terlambat → `Superseded`, teks turn sebelumnya tetap ada. Commit `TURN_END` turn lama yang kedaluwarsa tidak terpengaruh `ResumeTurn` turn baru (tetap `Late`).

## M5-T10 — Conn, Outbound, reader, writer, heartbeat
- `Metrics` (`metrics.go`) dibuat sekarang sebagai subset minimal: `NewMetrics(reg prometheus.Registerer)` mendaftarkan `kotoki_realtime_audio_frames_in_total{result}`, `audio_frames_out_total{result}`, `protocol_violations_total{kind}`, `slow_consumer_total`, dan `outbound_transient_dropped_total{reason=coalesced|dropped}`; semua method aman pada `*Metrics` nil. Task 18 **memperluas** tipe yang sama (bukan mengganti) dengan sisa metrik §7.14. `outbound_transient_dropped_total` tidak ada di tabel kanonik §7.14 tetapi §7.12.3 mewajibkan "dibuang (metric)" tanpa nama; nama ini dipilih builder.
- `version ≠ 1` → `ProtocolError{Code: UNSUPPORTED_PROTOCOL_VERSION, Kind: "version"}` (close 1008), bukan `PROTOCOL_VIOLATION` seperti tabel tes brief: §7.4.2 dan katalog §7.4.8 menetapkan kode `UNSUPPORTED_PROTOCOL_VERSION`. Close code dan label metrik `version` sama dengan brief.
- Reader tidak pernah menulis ke `Conn`: pelanggaran fatal dikembalikan sebagai `*ProtocolError` (sudah dihitung di metrik) dan `INVALID_MESSAGE` non-fatal dikirim sebagai `ReaderNotice` lewat channel `notices` (parameter ke-5 `Reader.Run`); `Runtime.loop` (Task 18+) yang mengalokasikan `server_seq`, meng-emit event `error`, menulis `PROTOCOL_VIOLATION` ke `session_events`, lalu menutup koneksi. `Writer.Run` mengembalikan `ErrWriteTimeout`/error `Conn`, dan pemanggil menjalankan alur disconnect.
- Dedupe `event_id` memakai `EventIDCache` (FIFO 64 id terakhir) yang dapat disuntikkan lewat `ReaderConfig.Seen` agar bertahan lintas reconnect ("per sesi", §7.4.2); nil = cache baru per reader. Pesan tanpa `event_id` = `INVALID_MESSAGE`.
- Jendela `AUDIO_FRAME_INVALID` (> 50 dalam 10 s) bergantung pada decode Opus di `audioPipeline`, bukan di reader; Task 10 menyediakan `burstWindow` (aman konkuren) + konstanta `audioFrameInvalidLimit/Window`, dipakai audioPipeline dengan kind `invalid_burst`. Reader memakai `burstWindow` yang sama untuk `INVALID_MESSAGE` (> 10 dalam 60 s → `PROTOCOL_VIOLATION`, kind `invalid_burst`). Frame < 6 byte → kind `frame_size`, byte 0 ≠ 0x01 → `header`.
- Tambahan API di luar brief: `Outbound.EnqueueAudioEnd(gen)` (penanda akhir generation di antrean audio; writer meng-ack `AUDIO_WRITTEN_ALL` saat mencapainya dan gen masih aktif — `audio.OutFrame` tidak punya flag "frame terakhir generation"; pacer Task 11 memanggilnya setelah frame terakhir), `Outbound.EnqueueEvent(ev)` + `EventDurable(ev)` (durable = `protocol.IsDurable(type)` atau `error` dengan `Fatal=false`, catatan review Task 3), `Outbound.ControlLen()`, `Writer.SentencesWritten(gen)` (pengganti `gen.sentencesWritten` atomic: writer menyimpan hitungan untuk generation yang sedang diputar karena `Generation` belum ada), `WriterAck{Kind, Gen, SentenceIdx, Sentences}` dengan `AckSentenceWritten` dan `AckAudioWrittenAll`.
- Audio tidak pernah ditolak `Outbound` (pacer membatasi < 25 frame); kapasitas 256 menghitung control + frame audio, dan hanya control durable yang mendapat `ErrSlowConsumer`. Transient pada antrean penuh dibuang tanpa error. Coalescing memindahkan transient terbaru ke ekor antrean (bukan menimpa di posisi lama) agar urutan `server_seq` tetap naik. `slow_consumer_total` naik sekali per `Outbound` (Outbound = per koneksi). `PurgeAudio` membuang semua item gen ≠ aktif (spec: gen < aktif; setara karena gen naik monoton) dan menghitungnya sebagai `dropped_stale`.
- Seam waktu: fake clock (`clock.Fake`) tidak punya timer, sehingga `Writer.WithTimeout`/`Heartbeat.WithTimeout` (`TimeoutFunc`, default `context.WithTimeout`) dan `Heartbeat.NewTicker` (`Ticker{C(); Stop()}`, default `NewRealTicker`) dapat disuntikkan; tes memakai `fakeTimeouts`/`manualTicker`. `Ticker` didefinisikan di `conn.go` sekarang (plan: Task 18 memproduksinya) — Task 18 memakai ulang.
- Heartbeat menutup koneksi sendiri (`Close(4102, "HEARTBEAT_TIMEOUT")`, best effort, kontrol frame — bukan `Write`) lalu mengembalikan `ErrHeartbeatTimeout`; ping gagal tidak fatal, hanya aturan idle `now − lastActivity > 45 s` (dicek tiap tick 15 s, jadi deteksi 45–60 s).
- `Conn` in-memory (`pipeConn`) ada di `internal/conversation/conn_test.go` (helper tes, bersama `fakeTimeouts`, `manualTicker`); `testutil/rtharness/pipeconn.go` (Task 18) dibuat terpisah dengan pola yang sama karena rtharness mengimpor `conversation`.
- Komentar paket di `ports.go` diperbarui: `conversation` kini mengimpor `conversation/audio` (cgo) dan hanya berada di graf realtime.
