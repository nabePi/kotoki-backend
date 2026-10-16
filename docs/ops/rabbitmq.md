# RabbitMQ: serah-terima ops

Sumber kebenaran topologi: `internal/amqp/topology.go` (spec Bab 9 §9.6). Dokumen ini hanya
memuat hal yang harus disiapkan operator di luar kode. Diacu oleh M10.

## Virtual host dan pengguna

| Item | Nilai |
|---|---|
| vhost | `/kotoki` (lokal: `/`). Di URI ditulis `%2Fkotoki`, mis. `amqps://kotoki-worker:***@mq.internal:5671/%2Fkotoki` |
| `kotoki-worker` | configure/write/read pada regex `^(kotoki\.\|evaluation\.\|memory\.\|learner\.\|summary\.).*`. Dipakai hanya binary `worker`; `api` dan `realtime` tidak memakai RabbitMQ |
| `keda` | monitoring read-only (tag `monitoring`; tanpa permission data) untuk autoscaling berdasar panjang queue |
| `admin` | operator (tag `administrator`) |

`kotoki-worker` mendeklarasikan topologi saat start dan setelah reconnect, sehingga butuh hak
configure pada seluruh nama di atas (queue, retry queue, `.dlq`, exchange `kotoki.*`).

## Transport dan klaster

- TLS `amqps://...:5671` wajib di staging dan prod; port 5672 non-TLS dimatikan. Validasi
  konfigurasi worker menolak `amqp://` di kedua lingkungan itu (M6 Task 1).
- 3 node; semua queue bertipe quorum (D-092). RabbitMQ 4.x.
- `rabbitmq.conf`:
  - `vm_memory_high_watermark.relative = 0.6`
  - `disk_free_limit.absolute = 2GB`
- Plugin: `rabbitmq_prometheus` pada :15692 (metrik queue untuk dashboard dan KEDA);
  `rabbitmq_management` hanya untuk operator.

## Topologi

5 exchange dan 25 queue (1 `kotoki.events.unrouted` + 4 queue utama x (utama, 4 retry, 1 DLQ)).
Argumen queue tidak dapat diubah setelah dideklarasi: RabbitMQ menjawab `PRECONDITION_FAILED`.
Aturan perubahan: buat queue baru bernama `<nama>.v2`, bind, kosongkan queue lama (drain),
lalu hapus queue lama. Jangan mengubah argumen queue yang sudah ada di kode tanpa langkah ini,
karena worker akan gagal start.

`kotoki.events.unrouted` menampung event tanpa consumer (`evaluation.failed`, `memory.extracted`)
selama 3 hari (maks 100.000 pesan, `drop-head`).

## Runbook singkat: DLQ

1. Pesan masuk `<queue>.dlq` bila error permanen, percobaan habis (5 total, retry 5s/30s/2m/10m),
   atau `x-delivery-limit=10` terlampaui. Header `x-error-class`, `x-last-error`,
   `x-first-failed-at`, `x-dead-at` menjelaskan sebabnya.
2. Perbaiki akar masalah (deploy, data), lalu jalankan ulang dengan `worker dlq-replay`
   (M6 Task 12). Pesan dikembalikan ke queue asal dengan `x-replayed-at`; idempotensi
   (`processed_events`) mencegah efek ganda.
3. Alarm: `kotoki_rabbitmq_queue_messages{queue="*.dlq"} > 0`.
