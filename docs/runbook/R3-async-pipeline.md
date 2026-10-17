# R3 Pipeline async: outbox, RabbitMQ, antrean dan DLQ

Severity: `OutboxLagCritical`, `DLQGrowing`, `RabbitMQAlarm` = SEV2 (evaluasi/memory/learner tertunda, tanpa dampak langsung ke percakapan; SEV1 bila berlangsung berjam-jam dan saldo/sesi terdampak); sisanya SEV3.

Alert: OutboxLagHigh
Alert: OutboxLagCritical
Alert: DLQNotEmpty
Alert: DLQGrowing
Alert: QueueBacklog
Alert: OutboxFailed
Alert: RabbitMQNodeDown
Alert: RabbitMQAlarm

## Gejala

- `OutboxLagHigh` (> 60 detik) dan `OutboxLagCritical` (> 300 detik): `kotoki_outbox_oldest_pending_age_seconds`, event `session.completed`/`evaluation.completed` tidak sampai ke broker.
- `OutboxFailed`: `kotoki_outbox_failed > 0`, baris `event_outbox` berstatus `FAILED` (publish gagal permanen).
- `DLQNotEmpty` (> 0 selama 10 menit) dan `DLQGrowing` (> 50): pesan di `<antrean>.dlq` pada `kotoki_rabbitmq_queue_messages`.
- `QueueBacklog`: antrean utama > 200 pesan dan terus naik selama 15 menit (konsumer tertinggal).
- `RabbitMQNodeDown` (kurang dari 3 node) dan `RabbitMQAlarm` (alarm watermark memori atau disk).

Antrean utama: `evaluation.session_completed`, `memory.session_completed`, `summary.session_completed`, `learner.evaluation_completed`; masing-masing punya antrean `.retry.*` dan `.dlq`. Penerbit outbox berjalan di `kotoki-worker-core` (`--handlers=outbox,sweeper,cleanup`); konsumer: `kotoki-worker-evaluation` (evaluation), `kotoki-worker-memory` (memory), `kotoki-worker-learner` (learner dan summary).

## Diagnosis cepat

1. Dashboard `Async`: "Outbox pending and oldest age", "Queue depth per queue (main)", "Queue depth per queue (retry and DLQ)", "Consumer results per queue", "Consumer in-flight deliveries", "Job runs per hour by result". Dashboard `Data stores`: "RabbitMQ nodes", "RabbitMQ alarms (memory, disk)".
2. Pod penerbit hidup dan tidak restart:

   ```bash
   kubectl -n kotoki get pods | grep kotoki-worker
   kubectl -n kotoki logs deploy/kotoki-worker-core --since=15m | grep -iE 'outbox|amqp|error'
   ```

3. Hasil konsumer dan penyebab DLQ:

   ```promql
   sum by (queue, result) (rate(kotoki_consumer_messages_total[5m]))
   max by (queue) (kotoki_rabbitmq_queue_messages{queue=~".*\\.dlq"})
   increase(kotoki_outbox_publish_failures_total[15m])
   ```

   Pratinjau isi DLQ tanpa memindahkan: header `x-error-class`, `x-last-error`, `x-first-failed-at`, `x-dead-at`:

   ```bash
   kubectl -n kotoki exec deploy/kotoki-worker-core -- /usr/local/bin/worker dlq-replay --queue evaluation.session_completed --dry-run --limit 20
   ```

   Nilai `--queue`: salah satu antrean utama di atas (bukan nama `.dlq`).
4. Alarm RabbitMQ: `kubectl -n kotoki-data get pods` (namespace RabbitmqCluster), `rabbitmqctl` via pod `kotoki-rabbitmq-server-0`. Memori/disk: dashboard `Data stores` ("RabbitMQ memory (resident) per node", "RabbitMQ free disk per node"). Konfigurasi dan aturan perubahan argumen antrean: `docs/ops/rabbitmq.md`.
5. Penyebab non-retryable di log konsumer (`kubectl -n kotoki logs deploy/kotoki-worker-evaluation --since=30m`): vendor LLM down ([R2](R2-provider-degraded.md)), data tidak valid, bug.

## Mitigasi

- Penerbit macet: `kubectl -n kotoki rollout restart deployment/kotoki-worker-core`. Event tidak hilang (transactional outbox); duplikasi ditangani idempotensi (`processed_events`).
- Konsumer tertinggal: KEDA menskalakan evaluation (2-8), memory (1-4), learner (1-3) menurut panjang antrean; bila KEDA tidak menjangkau RabbitMQ (`kotoki-keda-secrets`), naikkan manual lewat `minReplicaCount` pada ScaledObject yang bersangkutan: `kubectl -n kotoki patch scaledobject kotoki-worker-evaluation --type merge -p '{"spec":{"minReplicaCount":6}}'`.
- DLQ: perbaiki akar masalah dulu (deploy perbaikan, pulihkan vendor, perbaiki data), baru replay. Idempotensi mencegah efek ganda:

  ```bash
  kubectl -n kotoki exec deploy/kotoki-worker-core -- /usr/local/bin/worker dlq-replay --queue evaluation.session_completed --limit 100
  ```

  Ulangi per antrean; perintah mencetak jumlah pesan yang dipindahkan. Pesan yang masih gagal kembali ke DLQ.
- `OutboxFailed`: periksa `last_error` baris `FAILED` di Postgres (`SELECT event_id, event_type, attempts, last_error FROM event_outbox WHERE status = 'FAILED' ORDER BY created_at LIMIT 20;`). Setelah akar masalah diperbaiki, kembalikan baris satu per satu: `UPDATE event_outbox SET status = 'PENDING', attempts = 0, available_at = now() WHERE event_id = '<uuid>';` (tidak ada subperintah CLI untuk ini; operasi DBA dengan peran yang berhak, catat di kanal insiden).
- Node RabbitMQ turun atau alarm: pulihkan node (Cluster Operator), bebaskan disk/memori; quorum queue bertahan dengan 2 dari 3 node. Jangan menghapus antrean atau mengubah argumennya (worker gagal start dengan `PRECONDITION_FAILED`; lihat `docs/ops/rabbitmq.md`).

## Verifikasi pulih

- `kotoki_outbox_oldest_pending_age_seconds` kembali di bawah 60 dan `kotoki_outbox_pending` menuju 0; `kotoki_outbox_failed` = 0.
- `kotoki_rabbitmq_queue_messages` untuk semua `.dlq` = 0 dan antrean utama turun; alert padam.
- Cek integritas hasil pipeline: `kotoki-verify --db "$READONLY_DB_URL" --since <waktu-insiden-RFC3339> --await-evaluations 5m` -> exit 0 (V7 evaluasi tanpa duplikasi, V8 semua sesi `COMPLETED` punya evaluasi `COMPLETED`).
- Node RabbitMQ kembali 3 (`count(rabbitmq_identity_info)`) dan `rabbitmq_alarms_*` = 0.
