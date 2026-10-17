# R4 Redis

Severity: `RedisDown` = SEV1 bila berlangsung lebih dari failover normal (sesi realtime memakai Redis untuk status runtime, ticket, heartbeat dan rate limit); `RedisMemoryHigh` = SEV3/SEV2.

Alert: RedisDown
Alert: RedisMemoryHigh

## Gejala

- `RedisDown`: `redis_up == 0` selama 1 menit. Dampak: handshake WebSocket gagal (`rejected_unavailable`), ticket baru tidak bisa diterbitkan, rate limit memakai limiter lokal per pod (`kotoki_ratelimit_backend_errors_total` naik), sesi aktif dapat terputus.
- `RedisMemoryHigh`: `kotoki:redis_memory_utilization:ratio > 0.8` selama 10 menit. Recording rule ini didefinisikan per lingkungan di `deploy/k8s/overlays/<env>/infra-recording-rules.yaml` (nilai ekspresi metrik cloud harus diisi pada MANUAL GATE; `make pins-check ARGS=--prod` menolak placeholder `REPLACE_CLOUD_METRIC`).

## Diagnosis cepat

1. Dashboard `Data stores`: "Redis up", "Redis memory", "Redis memory used (bytes) and max", "Redis commands per second", "Redis client errors per second".
2. Status managed Redis di konsol penyedia: failover sedang berjalan? Failover normal selesai dalam 1-2 menit.
3. Dampak pada aplikasi:

   ```promql
   sum(rate(kotoki_ratelimit_backend_errors_total[5m]))
   sum by (result) (rate(kotoki_realtime_connections_total[5m]))
   max(kotoki_sweeper_heartbeat_checks_suspended)
   sum by (result, replay) (increase(kotoki_realtime_reconnect_total[15m]))
   ```

4. Memori tinggi: ukuran kunci menurut prefiks `kotoki:` (`kotoki:session:*`, `kotoki:realtime:*`, `kotoki:ratelimit:*`) lewat `redis-cli --scan --pattern 'kotoki:session:*' | head` dari host bastion yang berhak; pertumbuhan sejalan dengan `kotoki_realtime_sessions_active`?

## Mitigasi

- Redis mati: tunggu failover (maksimum 1-2 menit). Sweeper menahan aturan heartbeat selama Redis tak terjangkau dan sampai terjangkau terus-menerus >= 45 detik (D-NEW-13-19), sehingga sesi sehat tidak diakhiri palsu; konfirmasi `kotoki_sweeper_heartbeat_checks_suspended` = 1 selama pemulihan lalu kembali 0. Jangan restart `kotoki-realtime` massal selama Redis mati (pod yang sehat tetap melayani sesi yang sudah terhubung).
- Redis pulih namun sesi hilang: pengguna reconnect otomatis (klien resume); sesi yang tidak bisa dipulihkan diakhiri sweeper dan ditagih sampai heartbeat terakhirnya (sekali), sesuai aturan sweeper.
- Memori tinggi: naikkan ukuran instance Redis (scale vertikal) lebih dulu. Jangan `FLUSHALL`/`FLUSHDB` di produksi (menghapus heartbeat, ticket dan fence seluruh sesi).
- Redis tidak terjangkau dari pod tertentu saja: periksa NetworkPolicy (`deploy/k8s/base/networkpolicy/`) dan CIDR data store (`REPLACE_DATA_STORE_CIDR` di overlay) lalu `kubectl -n kotoki describe pod`.

## Verifikasi pulih

- `redis_up` = 1, `kotoki_ratelimit_backend_errors_total` berhenti naik, `kotoki_sweeper_heartbeat_checks_suspended` = 0.
- `sum by (result) (rate(kotoki_realtime_connections_total[5m]))` didominasi `accepted`.
- Periksa sesi `FAILED` selama jendela insiden dan pastikan integritas: `kotoki-verify --db "$READONLY_DB_URL" --since <waktu-insiden-RFC3339>` -> exit 0.
- Satu percakapan `kotoki-cli converse` berhasil.
