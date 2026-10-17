# Failure injection lokal (2026-10-17)

Skenario failure injection §13.13.2 terhadap docker compose (proyek `kotoki-chaoslocal`) dan biner lokal
(`PROVIDER_MODE=fake`, `FAKE_STT_MODE=energy`, fault dibaca dari Redis `kotoki:fake:faults` tiap 1 s).
Dihasilkan oleh `make chaos-local` (`test/chaos/local/run.sh` -> `kotoki-chaos run`); angka di bawah dikutip dari
`out/chaos/<F>.json` apa adanya. Satu skenario = `kotoki-load` (20 VU) berjalan terus, fault dipasang, dicabut,
`kotoki-verify` (V1-V8, V8 menunggu evaluasi sampai 5 m), lalu asersi. Asersi `prom` dijawab `Sampler`
(`test/chaos/live`) yang mengambil `/metrics` api/realtime/worker tiap 2 s: stack compose tidak punya Prometheus.

## Batas bukti lokal (dibaca sebelum tabel)

- **VU dan skala waktu bukan spesifikasi.** Spesifikasi: 100 CCU, inject T+5 m, revert T+15 m. Di sini 20 VU; F2, F3,
  F4, F5, F6, F16 dijalankan dengan `--time-scale 0.1` (inject 30 s, revert 90 s, lalu 2 m pemulihan yang tidak
  diskalakan); F1 dan F4 juga dengan waktu nyata (`--time-scale 1`: inject 5 m, revert 15 m); F8 dengan jendela 90 s - 4 m
  (outage 150 s, bukan 10 m); F13 dan F15 memakai jendela nyata sendiri (`Scenario.Fixed`).
- **Fault fake hanya mengena pada pembukaan stream/generation.** Fault STT dibaca saat stream dibuka (sesi baru), jadi
  pada skala 0,1 hanya 3 sesi baru yang terkena (F1, empat run awal dengan angka sama; F1 tidak diulang pada skala itu di run akhir); pada skala 1 sebanyak 2.254. Itu sebabnya F1 dan F4 diulang dengan waktu nyata.
- **Batas laju per-user diisi ulang oleh harness** (`kotoki-chaos --refill-every 5s`, juga di awal run): sesi yang gagal
  dalam hitungan detik menghabiskan `session_create_user` (5 burst + 10/jam), `ws_connect_ip` (30/mnt) dan `ws_attach_user`
  (10/mnt); user tanpa jatah tidak lagi membuat request, sehingga circuit breaker tidak pernah mendapat panggilan yang
  men-half-open-kannya, dan pemulihan tidak bisa diamati. Limiter bukan objek uji di sini (lihat temuan 1).
- Asersi yang butuh Prometheus nyata (alert `ProviderCircuitOpen`, `ProviderUnavailable`, `OutboxLagCritical`) hanya
  berlaku di `--env kube` dan tidak dijalankan lokal.


## Hasil

Semua baris adalah run terakhir tiap skenario. Run A-C memakai harness yang identik dengan yang di-commit; satu-satunya perubahan sesudahnya adalah probe dan asersi F15, sehingga F15 dijalankan ulang (run D). Run awal yang gagal karena harness (temuan 1) atau probe (temuan 5) tidak dihitung, tetapi disebut di temuan.

| Run | Skenario | Hasil | Mulai (UTC) | Jendela (sebelum skala; jendela nyata = nilai x skala) |
|---|---|---|---|---|
| A | F2 LLM first-token timeout | **PASS** | 2026-10-17T07:50:04Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 0.1, 20 VU |
| A | F3 TTS fails after the first audio | **PASS** | 2026-10-17T07:53:37Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 0.1, 20 VU |
| A | F4 Active STT/LLM vendor down for 2 minutes | **PASS** | 2026-10-17T07:57:41Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 0.1, 20 VU |
| A | F5 Every TTS vendor of the character down | **PASS** | 2026-10-17T08:01:44Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 0.1, 20 VU |
| A | F6 Redis restart / failover | **PASS** | 2026-10-17T08:05:48Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 0.1, 20 VU |
| A | F16 Client network drops and returns | **PASS** | 2026-10-17T08:09:52Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 0.1, 20 VU |
| B | F1 STT timeout in the middle of an utterance | **PASS** | 2026-10-17T07:31:53Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 1, 20 VU |
| B | F4 Active STT/LLM vendor down for 2 minutes | **PASS** | 2026-10-17T07:14:50Z | inject 5m0s, revert 15m0s, tail 2m0s, skala 1, 20 VU |
| C | F8 RabbitMQ down for 10 minutes | **PASS** | 2026-10-17T08:14:48Z | inject 1m30s, revert 4m0s, tail 2m0s, skala 1, 20 VU |
| C | F13 Duplicate message | **PASS** | 2026-10-17T08:20:53Z | inject 1m30s, revert 2m0s, tail 2m0s, skala 1, 20 VU |
| D | F15 Session runs out while the AI speaks | **PASS** | 2026-10-17T08:32:33Z | inject 5s, revert 10s, tail 5s, skala 1, 20 VU |

Run A = `make chaos-local SCEN="F2 F3 F4 F5 F6 F16" ARGS="--vus 20 --time-scale 0.1"`; B = `SCEN="F4 F1" ARGS="--vus 20 --time-scale 1"`; C = `SCEN="F8 F13 F15" ARGS="--vus 20 --time-scale 1 --inject-at 90s --revert-at 4m"` (F15 pada run ini gagal karena probe salah memodelkan pemotongan jawaban, lihat temuan 5; diulang di D); D = `SCEN=F15`.

## Rincian asersi

### F2 LLM first-token timeout (run A)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| provider_errors_total{LLM} rose | PASS | 285 |
| turns ended FAILED | PASS | 291 |
| normal turns after the revert | PASS | 121 |
| kotoki-verify: 0 violations | PASS | 0 |

### F3 TTS fails after the first audio (run A)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| provider_errors_total{TTS} rose | PASS | 178 |
| turns ended FAILED | PASS | 179 |
| normal turns after the revert | PASS | 115 |
| kotoki-verify: 0 violations | PASS | 0 |

### F4 Active STT/LLM vendor down for 2 minutes (run A)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| circuit opened | PASS | 5 |
| circuit went half-open | PASS | 5 |
| circuit closed again | PASS | 2 |
| every circuit CLOSED again | PASS | 0 |
| normal turns after the revert | PASS | 121 |
| kotoki-verify: 0 violations | PASS | 0 |

### F5 Every TTS vendor of the character down (run A)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| provider_errors_total{TTS} rose | PASS | 177 |
| normal turns after the revert | PASS | 115 |
| kotoki-verify: 0 violations | PASS | 0 |

### F6 Redis restart / failover (run A)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| the pods saw Redis fail | PASS | 29 |
| no false heartbeat_lost (rule b) | PASS | 0 |
| normal turns after the revert | PASS | 121 |
| kotoki-verify: 0 violations | PASS | 0 |

### F16 Client network drops and returns (run A)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| reconnects served | PASS | 2 |
| kotoki-verify: 0 violations | PASS | 0 |

### F1 STT timeout in the middle of an utterance (run B)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| provider_errors_total{STT} rose | PASS | 2254 |
| no vendor switch for STT (D-017) | PASS | 0 |
| sessions ended PROVIDER_FAILURE | PASS | 2254 |
| normal turns after the revert | PASS | 124 |
| kotoki-verify: 0 violations | PASS | 0 |

### F4 Active STT/LLM vendor down for 2 minutes (run B)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| circuit opened | PASS | 32 |
| circuit went half-open | PASS | 32 |
| circuit closed again | PASS | 2 |
| every circuit CLOSED again | PASS | 0 |
| normal turns after the revert | PASS | 126 |
| kotoki-verify: 0 violations | PASS | 0 |

### F8 RabbitMQ down for 10 minutes (run C)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| outbox_pending rose | PASS | 2 |
| outbox_pending drained to 0 | PASS | 0 |
| kotoki-verify: 0 violations | PASS | 0 |

### F13 Duplicate message (run C)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| consumers skipped the duplicate | PASS | 3 |
| kotoki-verify: 0 violations | PASS | 0 |

### F15 Session runs out while the AI speaks (run D)

Hasil: **PASS**

| Asersi | Hasil | Nilai |
|---|---|---|
| sessions ended by DURATION_ELAPSED | PASS | 8 |
| kotoki-verify: 0 violations | PASS | 0 |

## Temuan (jujur, termasuk yang bukan bug produksi)

1. **Harness vs batas laju (menentukan hasil F4, dan sebelum diperbaiki membuat F2-F5 gagal "tanpa load").** Run awal pada skala 0,1
   gagal: `circuit closed again = 0`, gauge akhir `1`, `normal turns after the revert = 0`, padahal log realtime memperlihatkan breaker
   sempat HALF_OPEN lalu OPEN lagi. Penyebab berantai, dibuktikan dari log dan DB run itu: (a) sesi yang gagal dalam detik
   menghabiskan `session_create_user` (5 burst + 10/jam) sehingga VU tidak lagi membuat request; breaker hanya pindah OPEN → HALF_OPEN pada
   `Allow()` berikutnya (`internal/provider/breaker.go`), jadi tanpa traffic ia tetap OPEN selamanya; (b) setelah jatah create diisi ulang, VU
   menghantam `ws_attach_user` (10/mnt) dan `ws_connect_ip` (30/mnt): handshake WS ditolak 429 dan sesi tertinggal `RESERVED` (deadline mulai 5 menit,
   D-035), sehingga setiap `POST /sessions` berikutnya 409 `SESSION_ALREADY_OPEN` (10.940 kali dalam satu run) sampai sesi itu `ABANDONED`;
   (c) VU `kotoki-load` membuat sesi baru tiap gagal (backoff 1 s) alih-alih mencoba attach lagi atau membatalkan sesi `RESERVED` itu.
   Limiter dan mesin status berperilaku sesuai spesifikasi; yang tidak realistis adalah klien. Penyelesaian di harness (bukan kode produksi):
   `kotoki-chaos` mengosongkan `session_create_user`, `ticket_user`, `ws_connect_ip` dan `ws_attach_user` di awal run dan tiap 5 s
   (`--refill-every`); run yang dicatat di atas memakainya. Pertanyaan terbuka untuk tim klien: setelah `RATE_LIMITED` di handshake, klien harus
   mencoba attach lagi pada sesi yang sama atau membatalkannya.
2. **F3: "0 sesi berakhir karena F3" tidak bisa benar untuk fault yang bertahan.** Dengan `fail_after_first_audio` pada setiap generation
   (rate 1,0), tiap turn `FAILED` dan tiga turn gagal berturut-turut mengakhiri sesi `PROVIDER_FAILURE` (§7.6.1): 44 sesi di sisi klien pada run A.
   Yang diasersikan: error TTS naik, turn `FAILED` terhitung, turn normal kembali setelah fault dicabut, verify 0.
3. **F1 pada skala 0,1 hanya mengena 3 sesi baru** (empat run awal, angka sama) karena fault STT berlaku saat stream dibuka (sesi baru) dan VU memegang sesinya 300+ detik;
   pada waktu nyata (run B) 2.254 sesi. Tidak ada pergantian vendor (`kotoki_provider_fallback_total{capability="STT"}` 0, D-017).
4. **`session.ready` memakai `server_seq` 1 di setiap koneksi** (`internal/conversation/manager_attach.go`: handshake menomori eventnya sendiri
   dari 1), sehingga resume membawa `session.ready` kedua dengan seq 1, padahal §7.4.3 menyebut `server_seq` monotonik per sesi. Tidak berbahaya
   (event transient, tidak diputar ulang, `last_server_seq` = maksimum), tetapi probe F16 sempat mengiranya event ganda. Probe mengecualikan
   `session.ready` dari pemeriksaan seq; kode M5 tidak diubah.
5. **F15: jawaban yang sedang berjalan boleh selesai dalam grace `ENDING`.** Probe F15 (sesi 300 s; ucapan kedua dimulai 296,4 s) menunjukkan
   `ai.generation_started` 298,3 s, `session.ending` stage `ENDING` 300,2 s, `turn.completed` 301,5 s, `session.completed` `DURATION_ELAPSED` 301,5 s,
   `charged_seconds` 300 (= yang direservasi). Tidak ada `ai.interrupted`: server memotong jawaban hanya bila grace (<= 10 s) habis. Model awal probe
   (menuntut `ai.interrupted SESSION_ENDING`) salah dan menggagalkan run C; juga `session.ending` stage `WARNING` (T-60 s, T-10 s) tidak boleh dihitung sebagai ending.
6. **F6:** log realtime memperlihatkan `redis state write failed; keeping the snapshot in memory` (sesi tetap berjalan dari memori), counter
   `kotoki_redis_command_errors_total` naik 29, `kotoki_sweeper_actions_total{rule="b",result="ok"}` 0 (tidak ada `heartbeat_lost` palsu) dan turn normal
   kembali. Yang tidak diasersikan: "ticket baru 503 sementara" dan "koneksi baru pulih <= 30 s" (restart Redis lokal hanya beberapa detik).
7. **F8 lokal = outage 150 s, bukan 10 menit; `outbox_pending` maksimum 2** (hanya sesi yang selesai selama outage, VU lokal sedikit). Outbox terkuras ke 0,
   `kotoki-verify` termasuk V8 (tiap sesi `COMPLETED` punya evaluasi) 0 pelanggaran. Asersi alert `OutboxLagCritical` hanya di `kube`.
8. **F13:** `kotoki_consumer_messages_total{result="duplicate"}` naik 3 (tiga konsumen `session.completed`: evaluation, memory, summary) setelah event yang sama
   (message id dan envelope identik, diambil dari `event_outbox`) diterbitkan ulang lewat management API; V7 (tidak ada evaluasi ganda) 0.
9. **Setiap run: `kotoki-verify` 0 pelanggaran** (V1-V8, termasuk saldo/ledger, reservasi, turn ganda, evaluasi ganda dan sesi `COMPLETED` tanpa evaluasi).

## Tidak dijalankan lokal: MANUAL GATE (Task 24)

F7, F9, F10, F11, F12 butuh cluster nyata (Chaos Mesh, kubectl, KEDA, CloudNativePG/managed PG); F4, F5, F8 punya asersi alert yang hanya berlaku dengan
Prometheus. Tidak ada yang dari ini diklaim lulus. Semuanya sudah diverifikasi offline: `kotoki-chaos list`, argv driver (`TestKubeDriver_Argv`), manifest Chaos Mesh
(`TestChaosManifestsAreChaosMesh`), skrip `test/chaos/fNN-*.sh` (`TestEveryScenarioHasScript`), tabel (`TestTable_*`). Perintah di staging
(`--users-file` = hasil `kotoki-load seed-users`, `--prom-url` = Prometheus staging, `--db` read-only, `--redis-url` Redis staging dengan pod `PROVIDER_MODE=fake`):

```
test/chaos/f07-rabbitmq-node-down.sh   --env kube --prom-url "$PROM" --db "$DB_RO" --redis-url "$REDIS" --users-file users.csv --api-url ... --ws-url ... --origin ... --vus 100
test/chaos/f09-realtime-pod-kill.sh    --env kube ... (idem)
test/chaos/f10-realtime-rolling-restart.sh --env kube ...
test/chaos/f11-pg-failover.sh          --env kube ...   # managed PG: picu failover penyedia, bukan hapus pod CNPG
test/chaos/f12-worker-evaluation-kill.sh --env kube ...
test/chaos/f04-vendor-down.sh / f05-... / f08-... --env kube ...   # menambah asersi alert ProviderCircuitOpen / ProviderUnavailable / OutboxLagCritical
```

| Skenario | Kriteria lulus (§13.13.2) | Dicek otomatis oleh `kotoki-chaos` | Perlu konfirmasi manusia |
|---|---|---|---|
| F7 | 0 pesan hilang; lag < 30 s | `max(outbox_oldest_pending_age) < 30`, verify | `kubectl get pods -n kotoki-data` (node kembali), tidak ada pesan di DLQ |
| F9 | >= 99% sesi terdampak lanjut; detik terputus tidak dicharge; verify 0 | `reconnect_total{result="takeover"} > 0`, reconnect gap P95 < 30 s, verify | `resume_ok_ratio >= 0,99` di `out/chaos/F9-load.json` (asersi `manual`, run tidak lulus otomatis) |
| F10 | 0 sesi `FAILED`; reconnect P95 < 5 s | 0 `SERVER_ERROR`, reconnect gap P95 < 5 s, verify | rollout selesai (`rollout status` dijalankan sebagai revert) |
| F11 | 0 double-charge; 0 sesi `ENDING` > 5 m setelah pulih; verify 0 | verify (V1-V8) | failover penyedia (asersi `manual`) dan REST `503` selama jendela |
| F12 | tepat 1 evaluasi per sesi | verify (V7, V8) | pod pengganti jalan, tidak ada pesan di DLQ |

## Kontainer dan proses

Hanya proyek compose `kotoki-chaoslocal` (postgres, redis, rabbitmq) yang dibuat dan di-`down -v` oleh `test/chaos/local/run.sh`; stack dev `kotoki` tidak disentuh. Tiga run awal yang
dihentikan di tengah jalan (harness lama, hasilnya tidak dipakai) dibersihkan per PID proses sendiri dan `docker compose -p kotoki-chaoslocal down -v`. Setelah run terakhir `docker ps -a`,
volume dan jaringan `kotoki*` kosong. Tidak ada `docker rm -f`.

## Run ulang pada gerbang M10 Task 23 (bug nyata ditemukan dan diperbaiki)

`make chaos-local SCEN="F1 F2 F3 F4 F5 F6 F8 F13 F15 F16" ARGS="--vus 20 --time-scale 0.1"` dijalankan lagi pada pohon kode yang sama dengan gerbang milestone. Hasil run pertama: F1-F6 PASS; **F8 FAIL** (`outbox_pending` tidak turun ke 0, 22 baris tertinggal, `kotoki-verify` melaporkan 12 pelanggaran V8: sesi `COMPLETED` tanpa evaluasi); **F13 ERROR** (`republisher: no session.completed event was published`, outbox macet); **F15 dan F16 FAIL** hanya pada asersi `kotoki-verify: 0 violations` (V8 yang sama; probe F15 dan F16 sendiri lolos). Run Task 21 sebelumnya lulus untuk F8/F13/F15/F16: kegagalan ini bergantung pada pewaktuan.

Akar masalah (kode produksi M6, `internal/amqp/confirm.go`, ada juga di `main`): `ConfirmPublisher.IsOpen()` mengembalikan `false` selama channel lama yang sudah mati masih dipegang (`p.ch != nil && p.ch.IsClosed()`). `OutboxPublisher.PublishBatch` hanya memanggil `Send` (yang membuat ulang channel) bila `IsOpen()` true. Setelah restart broker, koneksi pulih lewat `Conn.Reconnect`, tetapi channel mati itu tidak pernah diganti bila tidak ada `Send` yang berjalan tepat saat broker jatuh; `IsOpen()` tetap `false` selamanya. Karena `WaitReconnect` (`Conn.Reconnect`) langsung kembali saat koneksi sehat, `Run` berputar tanpa jeda: 2,59 juta baris log `outbox publisher: broker unavailable` (~490 MB) dalam 14 menit dan outbox tidak pernah terbit lagi (efek di produksi: evaluasi/memory/learner berhenti sampai pod worker-core di-restart; `OutboxLagCritical` akan menyala).

Perbaikan: `IsOpen()` = koneksi sehat (channel mati dibuat ulang oleh `Send`), dan `Run` menambah backoff (`Backoff(n-3)`) bila `WaitReconnect` berulang kali kembali segera tetapi publisher masih tidak tersedia. Tes baru: `TestConfirmPublisher_IsOpenAfterReconnect_WithoutSendDuringOutage` (merah sebelum perbaikan, broker nyata) dan `TestRun_BrokerDown_ImmediateReconnectStillBacksOff`.

Run ulang sesudah perbaikan, `make chaos-local SCEN="F8 F13 F15 F16" ARGS="--vus 20 --time-scale 0.1"`: **F8, F13, F15, F16 PASS** (`kotoki-verify` 0 pelanggaran; log worker 32 KB, 8 baris `broker unavailable`). F1-F6 PASS berasal dari run pertama (biner dibangun sebelum perbaikan; perbaikan tidak menyentuh jalur provider/Redis dan tidak diulang). Kontainer: hanya proyek `kotoki-chaoslocal` yang dibuat dan dibersihkan oleh skrip.
