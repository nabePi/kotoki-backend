# R1 Latensi turn, overhead server, koneksi WebSocket dan API

Severity: `TurnLatencyP95Critical` dan `ApiErrorRateHigh` = SEV1/SEV2 (percakapan atau API tidak layak dipakai); sisanya SEV2/SEV3. Langkah umum insiden (akui alert, tentukan severity, buka kanal, mitigasi dulu, postmortem SEV1/SEV2 dalam 5 hari kerja): spec §13.14.2.

Alert: TurnLatencyP95High
Alert: TurnLatencyP95Critical
Alert: ServerOverheadHigh
Alert: BargeInStopP95High
Alert: WSConnectFailureHigh
Alert: ApiErrorRateHigh
Alert: ApiLatencyP95High

## Gejala

- `TurnLatencyP95High` (> 1,5 s selama 10 menit, warning) dan `TurnLatencyP95Critical` (> 2,5 s selama 5 menit, critical): P95 `kotoki_realtime_turn_end_to_first_audio_seconds{kind="reply"}` naik. Pengguna merasakan jeda panjang setelah selesai bicara.
- `ServerOverheadHigh` (P95 `kotoki_realtime_server_overhead_seconds` > 150 ms) dan `BargeInStopP95High` (P95 `kotoki_realtime_bargein_stop_seconds` > 150 ms): bagian yang menjadi tanggung jawab server, di luar vendor.
- `WSConnectFailureHigh`: lebih dari 0,1% handshake WebSocket berakhir dengan `result` selain `accepted`, `rejected_ratelimit`, `rejected_ticket`, `rejected_draining` (jadi `rejected_origin`, `rejected_capacity`, `rejected_unavailable`, `rejected_state`).
- `ApiErrorRateHigh` (5xx > 1% selama 5 menit, critical) dan `ApiLatencyP95High` (P95 REST > 200 ms, atau `POST /api/v1/sessions` > 300 ms, selama 10 menit).

## Diagnosis cepat

1. Buka dashboard `Kotoki Overview` (panel "D-100 turn end to first audio P95", "API 5xx ratio", "WebSocket connect failure ratio"), lalu `Realtime Latency`: tahap mana yang naik. Hitung manual bila perlu:

   ```promql
   histogram_quantile(0.95, sum by (le) (rate(kotoki_realtime_stt_final_seconds_bucket[5m])))
   histogram_quantile(0.95, sum by (le) (rate(kotoki_realtime_llm_first_sentence_seconds_bucket[5m])))
   histogram_quantile(0.95, sum by (le) (rate(kotoki_realtime_tts_first_audio_seconds_bucket[5m])))
   histogram_quantile(0.95, sum by (le) (rate(kotoki_realtime_server_overhead_seconds_bucket[5m])))
   ```

2. Tahap vendor (STT/LLM/TTS) yang naik berarti masalahnya di provider: lanjut ke [R2](R2-provider-degraded.md) (dashboard `Providers`). Bila `server_overhead` yang naik, lanjut langkah 3.
3. Beban dan CPU pod realtime (panel "Realtime CPU per pod", "Realtime memory per pod", "Sessions per pod by state"):

   ```bash
   kubectl -n kotoki get pods -l app.kubernetes.io/component=realtime -o wide
   kubectl -n kotoki top pods -l app.kubernetes.io/component=realtime
   kubectl -n kotoki get scaledobject kotoki-realtime
   ```

   ```promql
   histogram_quantile(0.95, sum by (le, stage) (rate(kotoki_realtime_frame_processing_seconds_bucket[5m])))
   sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="kotoki",pod=~"kotoki-realtime-.*"}[5m]))
   sum(kotoki_realtime_sessions_active) / sum(kotoki_realtime_session_capacity)
   ```

   CPU mendekati limit (3 core) atau sesi per pod mendekati kapasitas: lanjut [R6](R6-realtime-capacity.md).
4. `WSConnectFailureHigh`: lihat sebaran `result`:

   ```promql
   sum by (result) (rate(kotoki_realtime_connections_total[5m]))
   ```

   `rejected_capacity` -> R6. `rejected_origin` -> periksa `WS_ALLOWED_ORIGINS` dan `CORS_ALLOWED_ORIGINS` (ConfigMap dari `deploy/k8s/overlays/<env>/patch-config.yaml`) terhadap origin klien. `rejected_unavailable` atau `rejected_state` -> Redis/Postgres ([R4](R4-redis.md), [R5](R5-postgres.md)). Lalu cek gateway ([R11](R11-gateway-certificate.md)).
5. API (`ApiErrorRateHigh`, `ApiLatencyP95High`): rute dan status yang bermasalah, lalu log dan pool DB:

   ```promql
   sum by (route, status) (rate(kotoki_http_requests_total{service="kotoki-api",status=~"5.."}[5m]))
   histogram_quantile(0.95, sum by (le, route) (rate(kotoki_http_request_duration_seconds_bucket{service="kotoki-api"}[5m])))
   sum by (app_kubernetes_io_component) (kotoki_db_pool_acquired_conns) / sum by (app_kubernetes_io_component) (kotoki_db_pool_max_conns)
   ```

   ```bash
   kubectl -n kotoki logs deploy/kotoki-api --since=15m | grep '"level":"ERROR"'
   kubectl -n kotoki rollout history deployment/kotoki-api
   ```

   Pool jenuh atau query lambat -> [R5](R5-postgres.md). Mulai setelah rollout baru -> pertimbangkan `kubectl -n kotoki rollout undo deployment/kotoki-api` (migration bersifat expand-only, aman untuk rollback satu versi).

## Mitigasi

- Vendor lambat atau error: ikuti [R2](R2-provider-degraded.md); vendor STT/LLM aktif yang tumbang tanpa fallback otomatis: [R10](R10-vendor-switch.md).
- Overhead server karena beban: tambah kapasitas realtime. KEDA memiliki HPA realtime, sehingga `kubectl scale` akan ditimpa; naikkan lantai replika lewat ScaledObject (maksimum 20):

  ```bash
  kubectl -n kotoki patch scaledobject kotoki-realtime --type merge -p '{"spec":{"minReplicaCount":8}}'
  ```

  Kembalikan ke 3 setelah pulih (`minReplicaCount: 3`; scale-down KEDA lambat: 1 pod per 300 detik setelah stabilisasi 600 detik).
- Overhead karena satu pod (CPU/GC tidak wajar): keluarkan pod itu setelah memastikan sesinya dapat reconnect: `kubectl -n kotoki delete pod <pod>` (drain 40 detik, reconnect mendarat di pod lain).
- API 5xx setelah deploy: `kubectl -n kotoki rollout undo deployment/kotoki-api`, lalu tunggu `rollout status`.
- API lambat karena DB: [R5](R5-postgres.md).
- Tidak ada sakelar aplikasi untuk membatasi pembuatan sesi (kebijakan rate limit `session_create_user` adalah konstanta kode, bukan env). Bila perlu membatasi beban, lakukan di gateway (di luar repo ini, belum diuji).

## Verifikasi pulih

- Alert padam dan tetap padam selama jendela `for` (10 menit untuk latensi).
- Panel "D-100 turn end to first audio P95" di `Kotoki Overview` kembali di bawah 1,5 s; "API 5xx ratio" di bawah 1%.
- Smoke: satu percakapan lewat gateway dengan `kotoki-cli --api-url https://api.<domain> --ws-url wss://realtime.<domain>/api/v1/realtime converse --wav testdata/audio/<fixture>.wav` (login lebih dulu dengan `kotoki-cli login`), keluaran ringkasan menunjukkan `turn_latency_ms` wajar.
- Bila `minReplicaCount` dinaikkan manual: catat dan kembalikan ke 3.
