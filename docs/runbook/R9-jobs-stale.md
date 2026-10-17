# R9 Job terjadwal macet (sweeper, cleanup, partisi)

Severity: `SweeperStale` dan `PartitionsRunningOut` = SEV1/SEV2 (sesi menggantung dan saldo tertahan; tanpa partisi baru insert gagal pada awal bulan); `SweeperHeartbeatSuspended` dan `CleanupStale` = SEV3.

Alert: SweeperStale
Alert: SweeperHeartbeatSuspended
Alert: CleanupStale
Alert: PartitionsRunningOut

## Gejala

- `SweeperStale`: `session_sweeper` tidak sukses > 60 detik (`kotoki_job_last_success_timestamp_seconds`).
- `SweeperHeartbeatSuspended`: `kotoki_sweeper_heartbeat_checks_suspended == 1` selama 10 menit; aturan heartbeat sweeper sengaja ditahan (Redis tak terjangkau atau baru pulih < 45 detik, atau pelindung kehilangan heartbeat massal: `kotoki_sweeper_mass_heartbeat_loss_total`).
- `CleanupStale`: salah satu job melewati batas: `transcript_retention`, `outbox_retention`, `balance_reconcile`, `memory_cleanup` > 3 jam; `reservation_cleanup` > 5 menit; `account_purge` > 1 jam; `partition_maintenance` > 26 jam.
- `PartitionsRunningOut`: `kotoki_partition_months_ahead` < 2 selama 30 menit untuk satu tabel (`session_turns`/`session_events`); buffer normal adalah bulan berjalan + 3 bulan.

Semua job berjalan di `kotoki-worker-core` (`--handlers=outbox,sweeper,cleanup`) di bawah advisory lock Postgres per job; tabel job dan interval ada di `internal/jobs/scheduler.go` (`session_sweeper` 10 dtk, `reservation_cleanup` 30 dtk, `transcript_retention`/`outbox_retention`/`balance_reconcile`/`memory_cleanup` 1 jam, `account_purge` 15 menit, `partition_maintenance` 24 jam).

## Diagnosis cepat

1. Dashboard `Business integrity` ("Session sweeper actions per hour", "Seconds since last balance reconcile and reservation cleanup", "Partition months ahead per table") dan `Async` ("Job runs per hour by result", "Job duration P95", "Seconds since last successful job run").
2. Pod worker-core hidup, log job:

   ```bash
   kubectl -n kotoki get pods | grep kotoki-worker-core
   kubectl -n kotoki logs deploy/kotoki-worker-core --since=30m | grep -iE 'job|sweeper|lock|error'
   ```

   ```promql
   sum by (job, result) (increase(kotoki_job_runs_total[1h]))
   time() - max by (job) (kotoki_job_last_success_timestamp_seconds)
   ```

3. Advisory lock tertahan (lock sesi `pg_try_advisory_lock`; kunci `7100001`-`7100009`, satu per job):

   ```sql
   SELECT l.objid AS lock_key, l.pid, a.state, now() - a.state_change AS age, left(a.query, 80)
     FROM pg_locks l JOIN pg_stat_activity a USING (pid)
    WHERE l.locktype = 'advisory' AND l.classid = 0 AND l.objsubid = 1 AND l.objid BETWEEN 7100001 AND 7100009;
   ```

4. Heartbeat tertahan: kaitkan dengan [R4](R4-redis.md) (Redis tidak terjangkau/baru pulih).

## Mitigasi

- Restart penjadwal: `kubectl -n kotoki rollout restart deployment/kotoki-worker-core` (lock sesi dilepas ketika koneksi pemegangnya mati; bila `pid` pemegang menggantung di DB, `SELECT pg_terminate_backend(<pid>)`).
- Jalankan satu kali job yang tertinggal (satu proses, di bawah lock yang sama; cetakan `job=<nama> ran=<true|false>`, `ran=false` berarti instance lain memegang lock):

  ```bash
  kubectl -n kotoki exec deploy/kotoki-worker-core -- /usr/local/bin/worker run-job session_sweeper
  kubectl -n kotoki exec deploy/kotoki-worker-core -- /usr/local/bin/worker run-job partition_maintenance
  ```

  Nama job yang sah: `session_sweeper`, `reservation_cleanup`, `transcript_retention`, `outbox_retention`, `balance_reconcile`, `account_purge`, `memory_cleanup`, `partition_maintenance`.
- Partisi hampir habis: jalankan `partition_maintenance` segera (membuat bulan berjalan + 3 bulan ke depan dengan `IF NOT EXISTS`, aman diulang). Bila gagal, baca galat pada keluaran dan log (mis. izin DDL peran worker) dan perbaiki sebelum awal bulan.
- Heartbeat ditangguhkan karena kehilangan massal: jangan memaksa; pastikan Redis sehat dan tunggu penangguhan berakhir sendiri.
- Job gagal karena timeout (lihat `Timeout` di tabel job): periksa beban Postgres ([R5](R5-postgres.md)).

## Verifikasi pulih

- `time() - max by (job) (kotoki_job_last_success_timestamp_seconds{job="session_sweeper"})` < 60 dan alert padam; `kotoki_job_runs_total{result="success"}` bertambah per job.
- `min by (table) (kotoki_partition_months_ahead)` >= 3 setelah `partition_maintenance` sukses.
- `kotoki_sweeper_heartbeat_checks_suspended` = 0.
- Tidak ada sesi `ACTIVE` yang menggantung: `kotoki-verify --db "$READONLY_DB_URL"` -> exit 0 dan panel "Session sweeper actions per hour" menunjukkan aksi normal.
