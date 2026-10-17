# R5 PostgreSQL

Severity: `PostgresDown` = SEV1; `DBPoolSaturated` = SEV2; `PostgresCPUHigh` = SEV3/SEV2.

Alert: DBPoolSaturated
Alert: PostgresDown
Alert: PostgresCPUHigh

## Gejala

- `PostgresDown`: `pg_up == 0` selama 1 menit. Pembuatan sesi, login dan penagihan gagal; sesi realtime yang berjalan berlanjut sampai butuh DB.
- `DBPoolSaturated`: `kotoki_db_pool_acquired_conns / kotoki_db_pool_max_conns` > 90% selama 5 menit per komponen (`app_kubernetes_io_component`: api, realtime, worker). Latensi API naik ([R1](R1-turn-latency.md)).
- `PostgresCPUHigh`: `kotoki:postgres_cpu_utilization:ratio > 0.8` selama 15 menit. Recording rule per lingkungan di `deploy/k8s/overlays/<env>/infra-recording-rules.yaml`; sumber metrik CPU database terkelola adalah keputusan MANUAL GATE (`REPLACE_CLOUD_METRIC`), jadi alert ini baru bermakna setelah diisi.

## Diagnosis cepat

1. Dashboard `Data stores`: "Postgres up", "Postgres CPU utilization", "Postgres connections by state", "App pool utilization per component (acquired / max)", "App pool acquire wait P95/P99", "Top 10 queries by P95 duration".
2. Failover sedang berlangsung? Cek peristiwa managed PG di konsol penyedia; setelah failover, pool aplikasi (pgx) tersambung ulang sendiri.
3. Saturasi pool: siapa yang menahan koneksi dan query apa (jalankan sebagai peran admin DB):

   ```sql
   SELECT state, wait_event_type, count(*), max(now() - xact_start) AS oldest_xact
     FROM pg_stat_activity WHERE datname = current_database() GROUP BY 1, 2 ORDER BY 3 DESC;
   SELECT pid, now() - xact_start AS age, left(query, 120) AS query
     FROM pg_stat_activity WHERE state = 'idle in transaction' ORDER BY age DESC LIMIT 10;
   SELECT pid, locktype, mode, granted FROM pg_locks WHERE NOT granted;
   SELECT calls, round(mean_exec_time::numeric, 1) AS mean_ms, left(query, 100)
     FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 10;
   ```

   ```promql
   histogram_quantile(0.99, sum by (le, app_kubernetes_io_component) (rate(kotoki_db_pool_acquire_wait_seconds_bucket[5m])))
   ```

4. Statement timeout: 5 detik untuk api/realtime dan 30 detik untuk worker (`DB_STATEMENT_TIMEOUT`); error `canceling statement` di log menunjuk query lambat. Job yang sedang berjalan: `kotoki_job_duration_seconds` (dashboard `Async`).

## Mitigasi

- Failover: tunggu, lalu verifikasi pool tersambung ulang (`kotoki_db_pool_acquired_conns` turun, error 5xx API berhenti).
- Saturasi: hentikan query berat (`SELECT pg_cancel_backend(<pid>)`; bila sesi `idle in transaction` menggantung, `SELECT pg_terminate_backend(<pid>)`), periksa deploy terbaru (`kubectl -n kotoki rollout history deployment/kotoki-api`; rollback dengan `rollout undo` bila bermula dari rilis), lalu scale vertikal DB bila beban memang naik. Menaikkan `DB_MAX_CONNS` hanya bila kuota koneksi total (`max_connections=400` dan `CONNECTION LIMIT` per peran, spec §13.3.2) masih longgar; koneksi total = replika x `DB_MAX_CONNS` (hitungan: `go run ./test/load/cmd/kotoki-load capacity --ccu 1000`).
- CPU tinggi: pakai daftar `pg_stat_statements` di atas, tambahkan index melalui migrasi expand-only (jangan membuat index manual di produksi tanpa migrasi).
- Kerusakan data atau salah hapus: restore point-in-time ke instance sementara (jangan menimpa primary), lalu jalankan pemeriksaan integritas:

  ```bash
  kotoki-verify --db "$RESTORED_READONLY_URL" --since 2020-01-01T00:00:00Z
  ```

  Waktu restore dicatat (target <= 1 jam). Prosedur yang sama dipakai latihan restore bulanan (MG-5 a).

## Verifikasi pulih

- `pg_up` = 1; `DBPoolSaturated` dan `PostgresCPUHigh` padam; "App pool acquire wait P99" < 50 ms.
- `kotoki_http_requests_total{status=~"5.."}` kembali ke dasar.
- `kotoki-verify --db "$READONLY_DB_URL"` -> exit 0 (V1/V2 saldo, V3-V8 integritas sesi).
- `worker run-job balance_reconcile` (lihat [R7](R7-balance-invariant.md)) mencetak `ran=true` dan `kotoki_balance_invariant_violations` tetap 0.
