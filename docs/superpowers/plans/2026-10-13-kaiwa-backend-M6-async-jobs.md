# Kaiwa Backend — M6 Async backbone & jobs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Event `session.completed` (dan event lain) mengalir dari transaksi bisnis lewat outbox → RabbitMQ → consumer framework dengan retry/DLQ/idempotensi, dan semua job terjadwal non-evaluasi berjalan di `cmd/worker` dengan advisory lock.
**Architecture:** `internal/events` (kontrak, tanpa infra) **diperluas** dari minimal M3; `internal/amqp` (adapter: topologi, koneksi, outbox publisher, consumer framework, DLQ replay) dan `internal/jobs` (scheduler + satu file per job) memakai port kecil yang didefinisikan di sisi pemakai; job hanya **memanggil** `session.Sweeper.Tick`, `session.Service.Abandon`, `balance.Service.Reconcile` buatan M3 dan menambah repository retensi/purge/partisi di `internal/postgres`. Wiring manual di `cmd/worker`; consumer evaluasi/memory/learner/summary diregistrasi M7/M8 lewat satu seam (`buildConsumers`).
**Tech Stack:** amqp091-go, pgx v5/pgxpool + sqlc (DDL dinamis & `ctid` pakai pgx mentah), go-redis v9, aws-sdk-go-v2/s3 (MinIO), OpenTelemetry (propagator W3C), prometheus client, testcontainers (postgres, redis, rabbitmq, minio), testify.
**Spec:** docs/superpowers/specs/2026-10-12-kaiwa-backend-design.md — Bab 9 (§9.1–§9.11, tanpa handler evaluasi/memory/learner/summary), §5.6.4, §4.8, §13.4.4, §13.5, §13.6, §13.9.3, §14.3
**Master plan:** docs/superpowers/plans/2026-10-13-kaiwa-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M1 (`config.Worker`/`ParseWorkerHandlers`, `postgres.Store.WithinTx/Pool`, `metrics.NewRegistry`, `health.NewOps`, `otel.Start/End`, `clock.NewFake`, `idgen`, `testutil.NewPostgres/NewRedis/NewTraceRecorder/SeedUser/SeedSession/SeedContent`, tabel `event_outbox`/`processed_events`/partisi dari migration 000020/000013/000014). M3 (`events.Event/OutboxWriter/SessionCompleted/SessionCompletedPayload/NewSessionCompleted(id, now, producer, userID, p)`, writer Postgres minimal, `session.Sweeper.Tick/SweepReport`, `session.Service.Abandon`, `session.Repository.ListAbandonCandidates`, `session.HeartbeatReader`, `balance.Service.Reconcile`, `balance.ReconcileRepository.RunCheck`, `testutil.AssertReconciled`). M2 (`redisstore` basis, `admin.Service.DeleteAccount` → status `DELETED`).

## Global Constraints

- Master Global Constraints berlaku.
- Outbox: poll `500ms`, batch `100`, confirm timeout `5s`, `OUTBOX_MAX_ATTEMPTS=10`, tx timeout `15s`; backoff publish `min(2^attempts, 300)` detik; `attempts` hanya naik untuk nack/timeout (bukan broker down); payload ≤ 65536 byte; message `DeliveryMode=2`, `AppId=kaiwa`, header `traceparent`, `tracestate` (bila ada), `x-event-version` int32.
- RabbitMQ: vhost `/kaiwa` (lokal `/`); exchange `kaiwa.events` (topic, `alternate-exchange=kaiwa.events.unrouted`), `kaiwa.events.unrouted` (fanout), `kaiwa.retry`, `kaiwa.requeue`, `kaiwa.dlx` (direct); 5 exchange + 25 queue, semua quorum; queue utama `x-delivery-limit=10`, `x-dead-letter-strategy=at-least-once`, `x-overflow=reject-publish`; retry TTL `5s/30s/2m/10m` (ms 5000/30000/120000/600000) dead-letter ke `kaiwa.requeue` (key = queue asal); `kaiwa.events.unrouted`: `x-message-ttl=259200000`, `x-max-length=100000`, `x-overflow=drop-head`.
- Queue/binding/prefetch/timeout handler: `evaluation.session_completed`←`session.completed` 4/200s; `memory.session_completed`←`session.completed` 4/120s; `summary.session_completed`←`session.completed` 8/60s; `learner.evaluation_completed`←`evaluation.completed` 16/30s. `MaxRetries=4` (total 5 percobaan ≈ 12,6 menit).
- Header consumer: `x-retry-count` (int32), `x-original-queue`, `x-last-error` (≤512), `x-first-failed-at`, `x-error-class`, `x-dead-at`, `x-replayed-at`. `Classify` 11 aturan §9.7.3; `PermanentError.Code` ∈ `DECODE_ERROR, UNSUPPORTED_EVENT_VERSION, ENTITY_NOT_FOUND, INVARIANT_VIOLATION, CONFIGURATION_ERROR` (+ kode domain Bab 10).
- Reconnect backoff 1s→2s→4s… maks 30s, jitter ±20%. `WORKER_SHUTDOWN_GRACE=25s` (evaluation 200s); `WORKER_HTTP_ADDR=:8081`.
- Job (tabel §9.9.1, satu-satunya): `session_sweeper` sweeper 10s `7100001` 30s; `reservation_cleanup` sweeper 30s `7100002` 25s; `transcript_retention` cleanup 1h `7100003` 15m; `audio_tmp_cleanup` cleanup 1h `7100004` 15m; `outbox_retention` cleanup 1h `7100005` 15m; `balance_reconcile` cleanup 1h `7100006` 10m; `account_purge` cleanup 15m `7100007` 10m; `memory_cleanup` cleanup 1h `7100008` 15m; `partition_maintenance` cleanup 24h `7100009` 10m. Lock = `pg_try_advisory_lock` session-level; jitter awal acak 0–interval; run tidak tumpang tindih.
- Retensi: transcript/summary/teks feedback 180 hari setelah `ended_at` (turn: `created_at < now−180d−2h`); `session_events` 365 hari; outbox `PUBLISHED` 7 hari, outbox `FAILED` & `processed_events` 30 hari; token kedaluwarsa 7 hari (`RETENTION_EXPIRED_TOKENS_DAYS`), `password_reset_tokens` 24 jam; batch: evaluasi 500, turns/events 5.000, summary 1.000, token 1.000, outbox 5.000, memory 1.000; audio sementara: hapus `LastModified < now − (24h − 1h)`; `account_purge` ≤ 50 user/run, `deleted_at < now − ACCOUNT_PURGE_DELAY(1h)`.
- Partisi: `{tabel}_pYYYYMM`, batas bulan UTC, buat bulan berjalan..+3, tanpa `DEFAULT`, drop `session_turns` bila `upper < now−180d−2h`, `session_events` bila `upper < now−365d`, tidak pernah drop partisi dengan `upper ≥ awal bulan berjalan`.
- Redis: heartbeat `kaiwa:session:{id}:hb` (epoch ms, M3 membaca); `kaiwa:user:{user_id}:memver` (INCR oleh `memory_cleanup`). Object storage: hanya prefix `audio/tmp/`; worker = Get/Delete/List.
- Metrik (nama persis §9.7.9, §13.7.3): `kaiwa_consumer_messages_total{queue,result}`, `kaiwa_consumer_processing_seconds{queue}` (ASYNC_BUCKETS), `kaiwa_consumer_inflight{queue}`, `kaiwa_outbox_published_total{event_type}`, `kaiwa_outbox_publish_failures_total{event_type,reason}`, `kaiwa_outbox_pending`, `kaiwa_outbox_oldest_pending_age_seconds`, `kaiwa_outbox_failed`, `kaiwa_job_runs_total{job,result}`, `kaiwa_job_duration_seconds{job}`, `kaiwa_job_items_total{job,action}`, `kaiwa_job_last_success_timestamp_seconds{job}`, `kaiwa_rabbitmq_queue_messages{queue}`, `kaiwa_sweeper_actions_total{rule,result}`, `kaiwa_sweeper_heartbeat_checks_suspended`, `kaiwa_sweeper_mass_heartbeat_loss_total`, `kaiwa_balance_invariant_violations{check}`, `kaiwa_partition_months_ahead{table}`, `kaiwa_partitions_created_total{table}`, `kaiwa_partitions_dropped_total{table}`. Tanpa label `user_id/session_id`.

## Review Focus

1. **Pod mati / client putus saat sesi berjalan:** sesi ditutup sweeper tepat sekali, `session.completed` terbit tepat sekali (race runtime vs sweeper vs publisher), saldo benar (R1–R6 bersih), tidak ada reservasi `HELD` yatim — Task 23 (`TestM6_PodKillEquivalent_*`, `TestM6_SessionCompleted_ExactlyOnce_*`), Task 24 (e2e M5), Task 14 (wiring job).
2. **Redis flush/failover tidak boleh menggagalkan sesi sehat:** aturan (b) ditangguhkan + skip massal tercatat di metrik/log — Task 14 (`TestSessionSweeperJob_Metrics*`), Task 23 (`TestM6_RedisFlush_DoesNotFailHealthySessions`).
3. **Outbox atomik dan tahan crash:** rollback bisnis → tak ada event; broker mati tidak menaikkan `attempts`; batch yang gagal di tengah di-rollback utuh dan dipublish ulang (duplikat aman) — Task 3, Task 7.
4. **Pesan racun / duplikat tidak memblokir queue dan tidak menggandakan efek:** permanent → DLQ langsung, retry ladder tepat 5s/30s/2m/10m, `x-delivery-limit`, redelivery paksa tetap satu efek, shutdown tidak menghitung retry — Task 9, 10, 11.
5. **Retensi dan purge:** tidak ada transcript terhapus sebelum 180 hari sejak `ended_at` (margin 2 jam, partisi batas), purge akun idempoten setelah crash di tengah dan `purged_at` hanya setelah semua langkah — Task 16, 17, 21.

## File Structure

| Path | Tanggung jawab |
|---|---|
| `internal/platform/config/{rabbit,outbox,jobs,s3}.go` | Grup env worker (§13.2.5, §13.2.6, §13.2.11) + validasi §13.2.13 |
| `internal/events/{envelope,delivery,errors,idempotency,queues,payloads}.go` (+ ubah `outbox.go`, `types.go`) | Kontrak event, payload V1, builder, `Delivery`, `Permanent`, `ProcessedEvents` |
| `internal/postgres/{outbox_writer,processed_events}.go`, `{retention,partition,purge,sweeper_job}_repo.go` | Writer/idempotensi + repository job |
| `internal/amqp/{topology,conn,confirm,outbox_publisher,outbox_gauges,metrics,errors,carrier,consumer,consumer_run,dlq_replay}.go` | Adapter RabbitMQ |
| `internal/objectstore/{client,config}.go` | S3/MinIO dasar (put/get/list/delete) |
| `internal/redisstore/memver.go` | `MemVerBumper` |
| `internal/jobs/{scheduler,lock,metrics,session_sweeper,reservation_cleanup,balance_reconcile,partition_maintenance,transcript_retention,audio_tmp_cleanup,outbox_retention,memory_cleanup,account_purge}.go` | Scheduler + job |
| `internal/testutil/{rabbitmq,minio}.go`, `internal/testutil/sessionenv/env.go` | Container helper, harness sesi (integration) |
| `cmd/worker/{main,wiring,commands,lifecycle}.go`, `internal/platform/health/ops.go` (ubah) | Wiring, `dlq-replay`, `run-job`, shutdown, readiness dinamis |
| `test/integration/m6_async_test.go`, `test/e2e/m6_podkill_test.go` | Gerbang M6 |

---

### Task 1: Grup konfigurasi worker (RabbitMQ, outbox, job, S3)

**Files:**
- Create: `internal/platform/config/{rabbit,outbox,jobs,s3}.go`
- Modify: `internal/platform/config/config.go` (embed ke `Worker`), `validate.go`, `.env.example`
- Test: `internal/platform/config/worker_groups_test.go`

**Interfaces:**
- Consumes: M1 `config.Worker`, `config.Secret`, `loadWorker(environ map[string]string)`.
- Produces: `Worker.Rabbit{URL Secret}`; `Worker.Outbox{PollInterval, ConfirmTimeout, TxTimeout time.Duration; BatchSize, MaxAttempts int}`; `Worker.Jobs{SweeperInterval, SweeperExpiryGrace, SweeperHeartbeatTimeout time.Duration; RetentionTranscriptDays, RetentionSessionEventsDays, RetentionOutboxPublishedDays, RetentionProcessedEventsDays, RetentionExpiredTokensDays int; RetentionAudioTmp, AccountPurgeDelay time.Duration}`; `Worker.S3{Endpoint, Region, BucketAudio string; AccessKeyID, SecretAccessKey Secret; UsePathStyle bool; SSE string; RequestTimeout time.Duration}`.

- [ ] **Step 1: Write the failing test** — `TestWorkerGroupDefaults` (`RABBITMQ_URL=amqp://guest:guest@localhost:5672/`; `OUTBOX_POLL_INTERVAL=500ms`, `OUTBOX_BATCH_SIZE=100`, `OUTBOX_CONFIRM_TIMEOUT=5s`, `OUTBOX_TX_TIMEOUT=15s`, `OUTBOX_MAX_ATTEMPTS=10`; `SWEEPER_INTERVAL=10s`, `SWEEPER_EXPIRY_GRACE=15s`, `SWEEPER_HEARTBEAT_TIMEOUT=45s`; `RETENTION_TRANSCRIPT_DAYS=180`, `RETENTION_SESSION_EVENTS_DAYS=365`, `RETENTION_OUTBOX_PUBLISHED_DAYS=7`, `RETENTION_PROCESSED_EVENTS_DAYS=30`, `RETENTION_EXPIRED_TOKENS_DAYS=7`, `RETENTION_AUDIO_TMP=24h`, `ACCOUNT_PURGE_DELAY=1h`; `S3_REGION=us-east-1`, `S3_BUCKET_AUDIO=kaiwa-audio-local`, `S3_REQUEST_TIMEOUT=10s`). `TestValidateRabbitS3` table: `APP_ENV=staging` + `RABBITMQ_URL=amqp://…` → error menyebut `RABBITMQ_URL`; `APP_ENV=prod` + `S3_SSE=""` → error menyebut `S3_SSE`; `APP_ENV=local` keduanya lolos; `OUTBOX_POLL_INTERVAL=0s` → error menyebut `OUTBOX_POLL_INTERVAL`; `RABBITMQ_URL` tidak tampil di `fmt.Sprintf("%v", cfg)`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/config/... -race -run 'TestWorkerGroup|TestValidateRabbitS3'`. Expected: FAIL.
- [ ] **Step 3: Implement** — tag `env:"…" envDefault:"…"` per §13.2.5/§13.2.6/§13.2.11; validasi §13.2.13 (baris `RABBITMQ_URL`, `S3_SSE`, durasi ≤ 0). Tambah nama ke `.env.example` kecuali var yang default-nya beda per binary.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS; `go test ./internal/platform/config/... -race` PASS (`TestEnvExampleLoads` tetap hijau).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(config): worker rabbitmq, outbox, jobs and s3 groups"`

### Task 2: Perluas paket `events` (envelope, payload V1, idempotensi, error)

**Files:**
- Create: `internal/events/{envelope,delivery,errors,idempotency,queues,payloads}.go`
- Modify: `internal/events/outbox.go`, `internal/events/types.go` (milik M3; jangan ubah signature yang ada)
- Test: `internal/events/{envelope,payloads,errors}_test.go`, `internal/events/testdata/*.golden.json`

**Interfaces:**
- Consumes (M3, tidak diubah): `type Event`, `type OutboxWriter`, `const SessionCompleted = "session.completed"`, `SessionCompletedPayload`, `NewSessionCompleted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p SessionCompletedPayload) Event`.
- Produces (§9.3–§9.7; nama dari plan ditandai):
```go
const (EvaluationCompleted = "evaluation.completed"; EvaluationFailed = "evaluation.failed"; MemoryExtracted = "memory.extracted") // (nama dari plan)
const (AggregateSession = "SESSION"; AggregateEvaluation = "EVALUATION"; ProducerAPI = "API"; ProducerRealtime = "REALTIME"; ProducerWorker = "WORKER"; CurrentVersion = 1) // (nama dari plan)
var SupportedVersions = map[string]int{SessionCompleted: 1, EvaluationCompleted: 1, EvaluationFailed: 1, MemoryExtracted: 1} // (nama dari plan)
const (QueueEvaluation = "evaluation.session_completed"; QueueMemory = "memory.session_completed"; QueueSummary = "summary.session_completed"; QueueLearner = "learner.evaluation_completed")
type Envelope struct{ EventID uuid.UUID; EventType string; EventVersion int; OccurredAt time.Time; Producer, AggregateType string; AggregateID, UserID uuid.UUID; Traceparent string; Tracestate *string; Payload json.RawMessage } // tag json §9.4
func (e Event) Envelope(traceparent string, tracestate *string) (Envelope, error) // OccurredAt dipotong ke ms UTC
func DecodeEnvelope(b []byte) (Envelope, error)                                  // abaikan field tak dikenal; wajib: event_id, event_type, event_version>=1, payload
func DecodePayload[T any](env Envelope) (T, error)                               // gagal → Permanent("DECODE_ERROR", err)
type Delivery struct{ Envelope Envelope; Queue string; RetryCount int; Redelivered bool }
type PermanentError struct{ Code string; Err error }; func Permanent(code string, err error) error
var ErrAlreadyProcessed = errors.New("event already processed")
type ProcessedEvents interface{ MarkProcessed(ctx context.Context, consumer string, ev Envelope) error }
type SessionCompletedV1 = SessionCompletedPayload
type EvaluationCompletedV1, EvaluationFailedV1, MemoryExtractedV1 struct // field & tag JSON persis §9.5.2–§9.5.4 (skor nullable: *int; signal: *EvalSignal{Score int; Confidence float64})
func NewEvaluationCompleted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p EvaluationCompletedV1) Event // aggregate EVALUATION, AggregateID=p.EvaluationID
func NewEvaluationFailed(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p EvaluationFailedV1) Event     // idem
func NewMemoryExtracted(id uuid.UUID, now time.Time, producer string, userID uuid.UUID, p MemoryExtractedV1) Event      // aggregate SESSION, AggregateID=p.SessionID
```
Catatan: D-003 tidak punya library JSON Schema; kesesuaian schema diuji dengan golden `JSONEq` + daftar `required` per schema (deviasi dicatat).

- [ ] **Step 1: Write the failing test** — `TestEnvelope_Golden_SessionCompleted` (nilai spec: `event_id 0192f1a2-7c3e-7b51-9d2a-3c4e5f6a7b8c`, `occurred_at "2026-10-12T06:30:15.123Z"` dari `time.Date(…, 123456789ns)` → dipotong `.123Z`, `producer REALTIME`, `traceparent 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`, `tracestate null`; `require.JSONEq`). `TestPayloadRequiredKeys` (marshal tiap payload → set key ⊇ daftar `required` §9.5.1–§9.5.4; `EvaluationCompletedV1.Scores` memuat 6 key dengan `null` untuk nil). `TestEvaluationCompletedV1_Golden` (contoh `finding_counts {"GRAMMAR":{"MAJOR":1,"MINOR":3,"INFO":0}}`). `TestDecodeEnvelope`: field tak dikenal diabaikan; tanpa `event_id` → error; `event_version 0` → error; payload tetap `RawMessage` identik. `TestDecodePayload_Invalid_IsPermanentDecodeError` (`errors.As(*PermanentError)`, `Code=="DECODE_ERROR"`). `TestPermanent_UnwrapAndError` (`"CODE: cause"`). `TestBuilders_AggregateAndVersion` (4 builder: `EventVersion==1`, aggregate type/ID sesuai tabel). `TestQueueConstants` (4 string persis).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/events/... -race`. Expected: FAIL (tipe belum ada).
- [ ] **Step 3: Implement** — file per tanggung jawab di atas; `DecodeEnvelope` memakai `json.Unmarshal` biasa (tanpa `DisallowUnknownFields`); `Event.Envelope` meng-`json.Marshal` `Payload`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/events/... -race -cover` → PASS, cover ≥ 85%; `go build ./...` (M3 tidak rusak).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(events): envelope, v1 payloads, delivery, permanent errors, idempotency port"`

### Task 3: Postgres — verifikasi outbox writer M3 + `processed_events` (integration)

**Files:**
- Modify: `internal/postgres/outbox_writer.go` (hanya bila tes gagal), `internal/postgres/tx.go` (tambah `Processed()`)
- Create: `internal/postgres/processed_events.go`, `db/queries/processed_events.sql`, `internal/testutil/fakes/processed.go`
- Test: `internal/postgres/outbox_writer_m6_integration_test.go`, `internal/postgres/processed_events_integration_test.go`

**Interfaces:**
- Consumes: M1 `postgres.Store.WithinTx`, M3 `(*Tx).Outbox() events.OutboxWriter`, `events.ProcessedEvents` (Task 2).
- Produces: `func (t *Tx) Processed() events.ProcessedEvents`; `type ProcessedEventsReader struct`; `func NewProcessedEventsReader(pool *pgxpool.Pool) *ProcessedEventsReader`; `func (r *ProcessedEventsReader) IsProcessed(ctx context.Context, consumer string, eventID uuid.UUID) (bool, error)`; `fakes.NewProcessed() *fakes.Processed` (`MarkProcessed`, `IsProcessed`, aman konkuren).

- [ ] **Step 1: Write the failing test** — tag `integration`. `TestOutboxWriter_StoresTraceparentFromActiveSpan` (span aktif via `NewTraceRecorder`; kolom `traceparent` cocok `^00-<traceid>-<spanid>-01$`; `tracestate` NULL bila kosong); `TestOutboxWriter_NoSpan_NullTrace` (kedua kolom NULL); `TestOutboxWriter_AllFourEventTypes` (4 type diterima; type lain → error CHECK, tx rollback); `TestOutboxWriter_PayloadOver64KiB_Rejected` (payload 65.537 byte → error, tidak ada baris); `TestOutboxWriter_RollbackLeavesNoEvent` (Review Focus 3: fn mengembalikan error setelah `Enqueue` → `count(*)=0`). `TestMarkProcessed_FirstInsertThenDuplicate` (kedua kali `errors.Is(err, events.ErrAlreadyProcessed)`); `TestMarkProcessed_RollbackLeavesNoRow`; `TestMarkProcessed_UnknownConsumer_NotErrAlreadyProcessed` (CHECK violation → error lain); `TestMarkProcessed_SameEventDifferentConsumers_BothOK`; `TestIsProcessed` (false → true setelah commit).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres/... -race -run 'TestOutboxWriter|TestMarkProcessed|TestIsProcessed'`. Expected: FAIL (`Processed()` belum ada).
- [ ] **Step 3: Implement** — `INSERT … ON CONFLICT (consumer, event_id) DO NOTHING`, `RowsAffected()==0` → `ErrAlreadyProcessed`; error DB dibungkus `errs.Wrap`. Bila `TestOutboxWriter_StoresTraceparent*` gagal pada writer M3, perbaiki dengan `otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier)` ke kolom.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(postgres): processed_events port and outbox writer trace/size tests"`

### Task 4: Topologi RabbitMQ + helper `testutil.NewRabbitMQ`

**Files:**
- Create: `internal/amqp/topology.go`, `internal/testutil/rabbitmq.go`
- Test: `internal/amqp/topology_test.go`, `internal/amqp/topology_integration_test.go`

**Interfaces:**
- Consumes: `events.Queue*`.
- Produces (§9.6.3 persis): `ExchangeEvents, ExchangeUnrouted, ExchangeRetry, ExchangeRequeue, ExchangeDLX, QueueUnrouted`; `type RetryLevel struct{Suffix string; TTL time.Duration}`; `var RetryLevels`; `const MaxRetries = 4`; `type QueueSpec struct{Name string; RoutingKeys []string; Prefetch int}`; `var Queues`; `RetryQueueName(q string, lvl RetryLevel) string`; `DLQName(q string) string`; `DeclareTopology(ch *amqp091.Channel) error` (mengembalikan error, tanpa panic). Helper: `testutil.NewRabbitMQ(t testing.TB) string` (image `rabbitmq:4-management`, vhost unik `t_<uuid>` via management API, hapus di cleanup; override `TEST_RABBITMQ_URL`); `testutil.RabbitMQAdmin(t, url) *RabbitAdmin` dengan `CloseAllConnections()`, `QueueMessages(queue string) int`, `QueueArgs(queue string) map[string]any`, `Queues() []string`, `Exchanges() []string`.

- [ ] **Step 1: Write the failing test** — unit `TestQueuesTable` (4 queue: prefetch `4,4,8,16`; routing key `session.completed`×3, `evaluation.completed`×1), `TestRetryLevels` (nama `Q.retry.5s|30s|2m|10m`, TTL ms `5000,30000,120000,600000`), `TestDLQName`. Integration (tag): `TestDeclareTopology_Idempotent_25Queues` (dua kali panggil sukses; management: 25 queue, 5 exchange kaiwa.*), `TestQueueArguments` (queue utama: `x-queue-type=quorum`, `x-delivery-limit=10`, `x-dead-letter-exchange=kaiwa.dlx`, `x-dead-letter-routing-key=Q`; retry 5s: `x-message-ttl=5000`, DLX `kaiwa.requeue`; unrouted: `x-message-ttl=259200000`, `x-max-length=100000`, `x-overflow=drop-head`), `TestRouting_SessionCompleted_OneMessageInEachOfThreeQueues`, `TestRouting_EvaluationCompleted_OnlyLearnerQueue`, `TestRouting_MemoryExtracted_And_EvaluationFailed_GoToUnrouted`, `TestRetryQueue_5s_ReturnsOnlyToOriginQueue` (publish ke `kaiwa.retry` key `evaluation.session_completed.retry.5s`; `Eventually(≤9s)` pesan muncul di `evaluation.session_completed` dan tidak di `memory.session_completed`/`summary.session_completed`; tidak muncul sebelum 4 s).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/amqp/... -race -run 'TestQueuesTable|TestRetryLevels|TestDLQName'` → FAIL; `go test -tags integration ./internal/amqp/... -race -run 'TestDeclareTopology|TestQueueArguments|TestRouting|TestRetryQueue'` → FAIL.
- [ ] **Step 3: Implement** — salin deklarasi §9.6.3 sebagai kode yang mengembalikan error; `go get github.com/rabbitmq/amqp091-go github.com/testcontainers/testcontainers-go/modules/rabbitmq`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): topology declaration and rabbitmq test helper"`

### Task 5: Koneksi ber-reconnect dan `ConfirmPublisher`

**Files:**
- Create: `internal/amqp/conn.go`, `internal/amqp/confirm.go`
- Test: `internal/amqp/conn_test.go`, `internal/amqp/conn_integration_test.go`

**Interfaces:**
- Consumes: `DeclareTopology`, `testutil.NewRabbitMQ/RabbitAdmin`.
- Produces:
```go
func Backoff(attempt int, rnd func() float64) time.Duration // base=min(2^attempt s, 30s) × (1 + (rnd()*0.4-0.2))
type Conn struct{ … }
func Dial(ctx context.Context, url, name string, log *slog.Logger) (*Conn, error) // sambung + DeclareTopology
func (c *Conn) Channel() (*amqp091.Channel, error)
func (c *Conn) Healthy() bool
func (c *Conn) Closed() <-chan struct{}               // tertutup saat koneksi saat ini putus
func (c *Conn) Reconnect(ctx context.Context) error   // loop Backoff sampai sukses + DeclareTopology
func (c *Conn) Close() error
var ErrNacked, ErrConfirmTimeout, ErrBrokerUnavailable error
type Confirmation interface{ Wait(ctx context.Context) (acked bool, err error) }
type ConfirmPublisher struct{ … }
func NewConfirmPublisher(conn *Conn, confirmTimeout time.Duration) *ConfirmPublisher // channel mode confirm
func (p *ConfirmPublisher) Send(ctx context.Context, exchange, key string, msg amqp091.Publishing) (Confirmation, error)
func (p *ConfirmPublisher) PublishConfirmed(ctx context.Context, exchange, key string, msg amqp091.Publishing) error // nack→ErrNacked, timeout→ErrConfirmTimeout
func (p *ConfirmPublisher) IsOpen() bool
```

- [ ] **Step 1: Write the failing test** — unit `TestBackoff_Sequence` (`rnd=0.5` → `1s,2s,4s,8s,16s,30s,30s`; `rnd=0` → ×0,8; `rnd=1` → ×1,2; attempt negatif → 1s). Integration: `TestConfirmPublisher_PublishConfirmed_Delivers` (pesan ada di queue tujuan, `err==nil`), `TestConn_ReconnectsAfterForcedClose` (`CloseAllConnections()` → `Healthy()` false dalam ≤ 5 s, `Reconnect` sukses, `Healthy()` true, topologi masih ada, `PublishConfirmed` sukses lagi), `TestConfirmPublisher_ClosedConn_ErrBrokerUnavailable` (`IsOpen()` false dan `Send` mengembalikan `ErrBrokerUnavailable` tanpa panic).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/amqp/... -race -run TestBackoff` → FAIL; `go test -tags integration ./internal/amqp/... -race -run 'TestConfirmPublisher|TestConn'` → FAIL.
- [ ] **Step 3: Implement** — `Send` membungkus `PublishWithDeferredConfirmWithContext`; `Confirmation.Wait` memakai timeout `confirmTimeout`; publisher membuat ulang channel bila mati dan koneksi sehat; satu koneksi terpisah untuk publisher vs consumer dijamin pemanggil (dua `Dial`).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): reconnecting connection and confirm publisher"`

### Task 6: Outbox publisher — pembentukan message & trace propagation (unit)

**Files:**
- Create: `internal/amqp/outbox_publisher.go` (bagian `OutboxRow`, `BuildMessage`), `internal/amqp/carrier.go`
- Test: `internal/amqp/outbox_message_test.go`

**Interfaces:**
- Consumes: `events.Envelope`, `testutil.NewTraceRecorder`.
- Produces: `type OutboxRow struct{ EventID uuid.UUID; EventType string; EventVersion int; AggregateType string; AggregateID, UserID uuid.UUID; Producer string; Payload json.RawMessage; Traceparent, Tracestate *string; OccurredAt time.Time; Attempts int }`; `func BuildMessage(ctx context.Context, row OutboxRow) (msg amqp091.Publishing, end func(), err error)` (memulai span `kaiwa.events publish`, `SpanKindProducer`, parent dari kolom; `end()` menutup span); `type amqpHeaderCarrier amqp091.Table` (`Get/Set/Keys` — §9.7.5).

- [ ] **Step 1: Write the failing test** — `TestBuildMessage_Properties` (`ContentType application/json`, `DeliveryMode 2`, `MessageId=event_id`, `Type=event_type`, `Timestamp=occurred_at`, `AppId kaiwa`, header `x-event-version == int32(1)`). `TestBuildMessage_BodyIsEnvelope` (`events.DecodeEnvelope(msg.Body)` cocok field row; `Payload` byte-identik). `TestBuildMessage_ParentFromStoredTraceparent` (row `traceparent=00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01` → span `kaiwa.events publish` ber-trace-id `4bf92f35…`, parent span id `00f067aa0ba902b7`, kind PRODUCER; header `traceparent` dan `envelope.traceparent` memuat trace-id yang sama dan span-id = span publish). `TestBuildMessage_NullTraceparent_NewRootAndNoTracestateHeader`. `TestCarrier_GetSetKeys`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/amqp/... -race -run 'TestBuildMessage|TestCarrier'` → FAIL.
- [ ] **Step 3: Implement** — extract `propagation.TraceContext{}` dari `MapCarrier{"traceparent":…,"tracestate":…}`, `tracer.Start`, inject ulang ke header & field envelope sebelum `json.Marshal`.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): outbox message builder with trace propagation"`

### Task 7: Outbox publisher — batch `SKIP LOCKED`, confirm, backoff, gauge (integration)

**Files:**
- Modify: `internal/amqp/outbox_publisher.go`
- Create: `internal/amqp/outbox_gauges.go`, `internal/amqp/metrics.go`
- Test: `internal/amqp/outbox_publisher_integration_test.go`

**Interfaces:**
- Consumes: `BuildMessage`, `Confirmation`, `ConfirmPublisher`, `config.Worker.Outbox`.
- Produces:
```go
type BatchPublisher interface{ IsOpen() bool; Send(ctx context.Context, exchange, key string, msg amqp091.Publishing) (Confirmation, error) } // dipenuhi *ConfirmPublisher
type PublisherConfig struct{ PollInterval, ConfirmTimeout, TxTimeout time.Duration; BatchSize, MaxAttempts int }
type OutboxPublisher struct{ Sleep func(context.Context, time.Duration); WaitReconnect func(context.Context); … } // dua field seam untuk test
func NewOutboxPublisher(pool *pgxpool.Pool, pub BatchPublisher, cfg PublisherConfig, m *Metrics, log *slog.Logger) *OutboxPublisher
func (p *OutboxPublisher) PublishBatch(ctx context.Context) (int, error) // algoritma §9.3.2
func (p *OutboxPublisher) Run(ctx context.Context) error                 // selesai bersih saat ctx dibatalkan
func NewMetrics(reg prometheus.Registerer) *Metrics                      // semua metrik amqp Global Constraints
func RefreshOutboxGauges(ctx context.Context, pool *pgxpool.Pool, m *Metrics) error // pending, oldest_pending_age_seconds, failed
func RefreshQueueDepth(ctx context.Context, ch *amqp091.Channel, queues []string, m *Metrics) error // QueueDeclarePassive → kaiwa_rabbitmq_queue_messages
```

- [ ] **Step 1: Write the failing test** — tag `integration`, PG nyata + `fakePub` (mencatat urutan, skenario ack/nack/error). `TestPublishBatch_AckedRowsPublished` (3 baris → `PUBLISHED`, `published_at` terisi, urutan publish = `available_at,event_id`, exchange `kaiwa.events`, key = `event_type`, `kaiwa_outbox_published_total{event_type}` naik). `TestPublishBatch_NackBackoffAndAttempts` (nack → `attempts 1`, `status PENDING`, `available_at−now()` ∈ (1s,3s], `last_error "broker nack or confirm timeout"`; nack ke-2 → ∈ (3s,5s]; metrik `kaiwa_outbox_publish_failures_total{reason="nack"}`). `TestPublishBatch_TenthFailureMarksFailed` (attempts 9 → `FAILED`, attempts 10). `TestPublishBatch_BrokerDown_DoesNotIncrementAttempts` (`IsOpen()=false` → `ErrBrokerUnavailable`, baris tak berubah). `TestPublishBatch_SendErrorMidBatch_RollsBackWholeBatch` (Send error pada baris ke-2 → semua baris `PENDING`, `attempts 0`). `TestPublishBatch_SkipLocked_TwoPublishersNoDuplicate` (200 baris, 2 publisher paralel, batch 100: tiap `event_id` terkirim tepat sekali). `TestPublishBatch_IgnoresFutureAndNonPending`. `TestRun_BacklogContinuesThenSleeps` (5 baris, batch 2 → `Sleep` terpanggil tepat 1× setelah batch kurang penuh). `TestRun_BrokerDown_WaitsReconnect` (`WaitReconnect` terpanggil, `attempts` tetap). `TestRefreshOutboxGauges` (2 PENDING + 1 FAILED → `pending 2`, `failed 1`, `oldest_age ≥ 0`). Integration RabbitMQ: `TestOutbox_EndToEnd_SessionCompleted` (Enqueue via writer M3 dalam `WithinTx` → `PublishBatch` dengan `ConfirmPublisher` nyata → tepat 1 pesan di tiap `evaluation|memory|summary.session_completed`; pesan dari `Get` memuat header `traceparent` yang sama trace-id dengan kolom outbox).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/amqp/... -race -run 'TestPublishBatch|TestRun_|TestRefreshOutboxGauges|TestOutbox_EndToEnd'` → FAIL.
- [ ] **Step 3: Implement** — SQL §9.3.2 persis (`FOR UPDATE SKIP LOCKED`, update `ok`/`failed` dalam tx yang sama, `make_interval(secs => LEAST(power(2, attempts + 1), 300))`), pgx mentah (adapter boleh); `Run` per pseudocode §9.3.2; gauge via goroutine 15 s (`RunOutboxGauges`) dan 30 s (`RunQueueDepth`) dipanggil dari wiring.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS; `go test ./internal/amqp/... -race` PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): outbox publisher with skip locked batches, backoff and gauges"`

### Task 8: `Classify` dan kode error consumer (unit)

**Files:**
- Create: `internal/amqp/errors.go`
- Test: `internal/amqp/errors_test.go`

**Interfaces:**
- Consumes: `events.PermanentError`, `events.ErrAlreadyProcessed`, M4 `provider.IsRetryable(err) bool`, `provider.Error`.
- Produces: `type Class int` (`ClassAck, ClassRetry, ClassDead, ClassRequeue`); `func Classify(err error, shuttingDown bool) Class` (11 aturan §9.7.3 berurutan); `func errorCode(err error) string` (kode `PermanentError`, selain itu `"RETRIES_EXHAUSTED"`); `func headerInt(h amqp091.Table, key string) int` (int/int32/int64, absen → 0); `func truncate(s string, n int) string`.

- [ ] **Step 1: Write the failing test** — `TestClassify` table: `nil` → Ack; `events.ErrAlreadyProcessed` (juga dibungkus `%w`) → Ack; `context.Canceled` + `shuttingDown=true` → Requeue, `shuttingDown=false` → Retry (aturan 11); `events.Permanent("ENTITY_NOT_FOUND", e)` → Dead; `provider.Error{Retryable:false}` → Dead; `provider.Error{Retryable:true}` → Retry; `&pgconn.PgError{Code}` `40001`, `40P01`, `08006`, `53300`, `57P01`, `57P03` → Retry; `23505`, `23503`, `22001` → Dead; `context.DeadlineExceeded` → Retry; error biasa → Retry. `TestClassify_PermanentWinsOverShutdown` (urutan aturan 2 sebelum 3: `Canceled` bukan permanent; permanent + shutting down → Dead). `TestErrorCode` (`PermanentError{Code:"DECODE_ERROR"}` → `"DECODE_ERROR"`; lain → `"RETRIES_EXHAUSTED"`). `TestHeaderInt` (`int32(3)`, `int64(2)`, `int(1)`, absen, string → 0). `TestTruncate_512`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/amqp/... -race -run 'TestClassify|TestErrorCode|TestHeaderInt|TestTruncate'` → FAIL.
- [ ] **Step 3: Implement** — `errors.As` untuk `*events.PermanentError`, `*pgconn.PgError` (cek `Code[:2]`/kode penuh), `provider.IsRetryable`; urutan persis tabel §9.7.3.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): error classification for consumer retry and dlq"`

### Task 9: `Consumer.process` — ack/nack, retry ladder, DLQ, idempotensi (unit)

**Files:**
- Create: `internal/amqp/consumer.go`, `internal/amqp/export_test.go`
- Test: `internal/amqp/consumer_process_test.go`

**Interfaces:**
- Consumes: `Classify`, `events.*`, `ConfirmPublisher` (via port `Publisher`), `RetryLevels`, `MaxRetries`.
- Produces:
```go
type Handler interface{ Queue() string; SupportedEvents() map[string]int; Timeout() time.Duration; Handle(ctx context.Context, d events.Delivery) error; OnFailure(ctx context.Context, d events.Delivery, cause error, final bool) }
type Publisher interface{ PublishConfirmed(ctx context.Context, exchange, key string, msg amqp091.Publishing) error }
type ProcessedChecker interface{ IsProcessed(ctx context.Context, consumer string, eventID uuid.UUID) (bool, error) }
type Consumer struct{ … }
func NewConsumer(h Handler, conn *Conn, pub Publisher, processed ProcessedChecker, m *Metrics, log *slog.Logger, clk clock.Clock) *Consumer
func (c *Consumer) process(root context.Context, msg amqp091.Delivery) // algoritma §9.7.4
func cloneHeaders(h amqp091.Table) amqp091.Table; func copyMessage(msg amqp091.Delivery, h amqp091.Table) amqp091.Publishing
```
`export_test.go` mengekspos `process` dan `NewConsumerForTest(h, pub, processed)` (tanpa `Conn`).

- [ ] **Step 1: Write the failing test** — `amqp091.Delivery{Acknowledger: fakeAck, Body, Headers}` + `fakePub` mencatat (exchange,key,headers) + `fakeHandler`. `TestProcess_Success_Ack` (metrik `result="ack"`). `TestProcess_Duplicate_PrecheckAcksWithoutHandle` (`IsProcessed=true` → handler tidak dipanggil, `result="duplicate"`). `TestProcess_ErrAlreadyProcessed_Acks` (handler mengembalikan `ErrAlreadyProcessed` → Ack). `TestProcess_RetryLadder_HeadersAndRoutingKeys` (handler selalu `errors.New("boom")`; delivery dengan `x-retry-count` absen,1,2,3 → publish ke `kaiwa.retry` key `Q.retry.5s`,`Q.retry.30s`,`Q.retry.2m`,`Q.retry.10m`; header `x-retry-count` int32 `1,2,3,4`, `x-original-queue=Q`, `x-last-error` ≤ 512, `x-first-failed-at` ada dan tidak ditimpa pada retry berikut; body identik; lalu `Ack`; `OnFailure(final=false)`). `TestProcess_RetriesExhausted_GoesToDLQ` (`x-retry-count=4` → publish ke `kaiwa.dlx` key `Q`, `x-error-class=RETRIES_EXHAUSTED`, `x-dead-at`, `OnFailure(final=true)`, Ack). `TestProcess_Permanent_DirectToDLQ_NoRetry` (`x-error-class=ENTITY_NOT_FOUND`, tanpa publish ke `kaiwa.retry`). `TestProcess_DecodeError_DLQ_DECODE_ERROR`, `TestProcess_UnsupportedVersion_DLQ` (`session.completed` v2 saat `SupportedEvents` v1 → `UNSUPPORTED_EVENT_VERSION`), `TestProcess_UnknownEventType_DLQ`. `TestProcess_HandlerPanic_Retries`. `TestProcess_HandlerTimeout_Retries` (handler menunggu `ctx.Done`, `Timeout()=10ms`). `TestProcess_RetryPublishFails_NacksRequeue`, `TestProcess_DeadPublishFails_NacksWithoutRequeue`. `TestProcess_ShuttingDown_Requeue_NoOnFailure` (root ctx dibatalkan, handler → `context.Canceled` → `Nack(false,true)`, `OnFailure` tidak dipanggil, metrik `result="requeue"`). `TestProcess_PropagatesTraceContext` (handler menerima ctx dengan span child dari header `traceparent`; fallback ke field envelope bila header kosong; span `Q process` kind CONSUMER, atribut `messaging.destination.name`, `messaging.message.id`, `kaiwa.retry_count`). `TestProcess_BodyNeverModified`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/amqp/... -race -run TestProcess` → FAIL.
- [ ] **Step 3: Implement** — salin alur §9.7.4; `copyMessage` mempertahankan `ContentType`, `DeliveryMode=2`, `MessageId`, `Type`, `Timestamp`, `AppId`, `Body`; `safeHandle` memakai `recover()`; log slog per delivery memuat `trace_id, span_id, event_id, event_type, queue, retry_count, worker_version`; metrik `kaiwa_consumer_*`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/amqp/... -race -run TestProcess` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): consumer processing with retry ladder, dlq and idempotency precheck"`

### Task 10: Runtime consumer — `Run`, Qos, reconnect, graceful shutdown

**Files:**
- Create: `internal/amqp/consumer_run.go`
- Test: `internal/amqp/consumer_run_integration_test.go`

**Interfaces:**
- Consumes: `Consumer.process`, `Conn.Reconnect/Closed`, `Queues` (prefetch per queue).
- Produces: `func (c *Consumer) Run(ctx context.Context) error` (blok sampai `ctx` selesai; satu channel per queue, `Qos(prefetch,0,false)`, `prefetch` goroutine pemroses, reconnect loop §9.7.6); `func (c *Consumer) Shutdown(ctx context.Context, grace time.Duration) error` (§9.7.7 langkah 3–5: `ch.Cancel`, delivery ter-prefetch belum diproses di-`Nack(requeue=true)`, tunggu in-flight ≤ `grace`, lalu batalkan root ctx handler).

- [ ] **Step 1: Write the failing test** — tag `integration`, RabbitMQ + handler palsu. `TestConsumer_PrefetchLimitsConcurrency` (queue `learner.evaluation_completed`, 40 pesan, handler menahan: concurrency puncak == 16; `evaluation.session_completed` == 4). `TestConsumer_ShutdownWithinGrace_AcksInflight` (handler 300 ms, `Shutdown(grace=2s)` → pesan di-ack, queue kosong). `TestConsumer_ShutdownGraceExpires_RequeuesWithoutRetryCount` (handler menunggu `ctx.Done`, grace 200 ms → pesan kembali ke queue utama dengan `Redelivered=true`, tanpa `x-retry-count`, `OnFailure` tidak dipanggil; tidak masuk retry queue). `TestConsumer_UnstartedPrefetchedMessagesRequeued` (prefetch 4, 10 pesan, 4 in-flight lama; setelah shutdown 10 pesan tetap ada di queue). `TestConsumer_ReconnectsAfterForcedClose` (`CloseAllConnections()` → consumer lanjut mengonsumsi pesan baru ≤ 10 s).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/amqp/... -race -run TestConsumer_` → FAIL.
- [ ] **Step 3: Implement** — dua context (`consumeCtx` untuk berhenti mengambil delivery, `rootCtx` dibatalkan setelah grace); `Classify(err, rootCtx.Err() != nil)`; reconnect: tunggu goroutine in-flight, `conn.Reconnect`, `Qos`, `Consume` ulang.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): consumer runtime with prefetch, reconnect and graceful shutdown"`

### Task 11: Suite integration consumer — retry nyata, poison, idempotensi, trace end-to-end

**Files:**
- Test: `internal/amqp/consumer_integration_test.go`

**Interfaces:**
- Consumes: Task 3–10, `postgres.NewProcessedEventsReader`, `Tx.Processed()`, `events.NewSessionCompleted`.
- Produces: tidak ada kode produksi baru (hanya perbaikan bila tes menemukan bug).

- [ ] **Step 1: Write the failing test** — tag `integration` (RabbitMQ + Postgres). `TestRetry_FirstRung_RealBroker` (handler gagal sekali lalu sukses: pesan masuk `….retry.5s`, kembali ke `Q` setelah ≥ 4 s dan ≤ 9 s, header `x-retry-count=1`, handler dipanggil 2×, `kaiwa_consumer_messages_total{result="retry"}=1`, lalu `ack`). `TestPoison_PermanentError_LandsInDLQWithHeaders` (`ENTITY_NOT_FOUND` → 1 pesan di `Q.dlq`, `x-error-class`, `x-original-queue`, `x-last-error`, `x-dead-at`; queue utama kosong; consumer tetap memproses pesan berikutnya — queue tidak terblokir). `TestPoison_MalformedBody_DLQDecodeError`. `TestPoison_DeliveryLimit_DeadLettersToDLQ` (channel mentah menerima lalu `Nack(false,true)` 11 kali → pesan di `Q.dlq` lewat DLX queue-level). `TestIdempotency_DuplicateDelivery_SingleEffect` (publish envelope sama 2× ke `evaluation.session_completed`; handler = tx `MarkProcessed` + `INSERT efek`; `count(efek)=1`, `processed_events` 1 baris, pesan kedua ack-duplikat). `TestIdempotency_PrecheckBypassed_MarkProcessedStillGuards` (`ProcessedChecker` selalu false → `ErrAlreadyProcessed` → ack, efek tetap 1). `TestIdempotency_ForcedRedelivery_AfterCommitBeforeAck` (handler commit lalu consumer di-kill sebelum ack → redelivery → `duplicate`, efek 1). `TestTrace_OutboxToConsumer_SameTraceID` (Enqueue dalam span → publisher → handler merekam `trace.SpanContextFromContext`; trace-id sama dengan span pembuat event).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/amqp/... -race -run 'TestRetry_|TestPoison_|TestIdempotency_|TestTrace_'`. Expected: FAIL pada setidaknya satu tes sampai bug integrasi diperbaiki.
- [ ] **Step 3: Implement** — perbaiki hanya file `internal/amqp/*.go` yang gagal; tidak ada fitur baru.
- [ ] **Step 4: Run tests to verify they pass** — same command `-count=3` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test(amqp): consumer retry, poison, idempotency and trace integration suite"`

### Task 12: `dlq-replay`

**Files:**
- Create: `internal/amqp/dlq_replay.go`
- Test: `internal/amqp/dlq_replay_integration_test.go`

**Interfaces:**
- Consumes: `Conn`, `Publisher`, `DLQName`, `ExchangeRequeue`.
- Produces: `type ReplayOptions struct{ Queue string; Limit int; DryRun bool; Out io.Writer }`; `func ReplayDLQ(ctx context.Context, conn *Conn, pub Publisher, opt ReplayOptions) (int, error)` — publish ke `kaiwa.requeue` key `Queue`, header `x-retry-count=0` (int32) dan `x-replayed-at`, tunggu confirm, lalu ack; `DryRun` mencetak `event_id`, `event_type`, `x-error-class`, `x-last-error` per baris lalu `Nack(multiple=true, requeue=true)` sekali di akhir (deviasi dari "nack per pesan" yang akan membaca pesan sama berulang; catat di deviations).

- [ ] **Step 1: Write the failing test** — `TestReplayDLQ_MovesBackToMainQueueWithResetHeaders` (3 pesan di `evaluation.session_completed.dlq` → `Limit 100`: DLQ kosong, 3 pesan di queue utama, `x-retry-count=0`, `x-replayed-at` ada, header lain & body utuh, return 3, tidak ada pesan di `memory`/`summary` queue). `TestReplayDLQ_Limit` (5 pesan, `Limit 2` → 2 dipindah, 3 tersisa). `TestReplayDLQ_DryRun_PrintsAndLeavesAll` (output memuat 3 `event_id` + `x-error-class`; DLQ tetap 3; queue utama kosong). `TestReplayDLQ_EndToEnd_PoisonThenFixedHandler` (pesan masuk DLQ karena `CONFIGURATION_ERROR`; handler diperbaiki; replay → diproses sukses, efek 1).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/amqp/... -race -run TestReplayDLQ` → FAIL.
- [ ] **Step 3: Implement** — `ch.Get` (prefetch efektif 1 untuk mode replay), batas `Limit`; unknown queue (bukan salah satu `Queues`) → error sebelum menyentuh broker.
- [ ] **Step 4: Run tests to verify they pass** — same command → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(amqp): dlq replay with dry-run"`

### Task 13: Scheduler `jobs` + advisory lock + tabel job kanonik

**Files:**
- Create: `internal/jobs/{scheduler,lock,metrics}.go`
- Test: `internal/jobs/scheduler_test.go`, `internal/jobs/lock_integration_test.go`

**Interfaces:**
- Consumes: M1 `clock.Clock`, `metrics`/prometheus.
- Produces:
```go
type Spec struct{ Name, Flag string; Interval time.Duration; LockKey int64; Timeout time.Duration }
var Specs = []Spec{ /* 9 baris tabel §9.9.1 */ }
func SpecByName(name string) (Spec, bool)
func SpecsForFlags(flags []string) []Spec                // flag "sweeper" / "cleanup"
type Job interface{ Run(ctx context.Context) error }
type Locker interface{ RunExclusive(ctx context.Context, key int64, fn func(context.Context) error) (ran bool, err error) }
type PgLocker struct{ Pool *pgxpool.Pool }               // §9.9.1 persis (pg_try_advisory_lock, unlock dengan WithoutCancel)
func NewMetrics(reg prometheus.Registerer) *Metrics      // job_runs/duration/items/last_success + sweeper_* + balance/partition gauges
type SchedulerDeps struct{ Locker Locker; Metrics *Metrics; Clock clock.Clock; Log *slog.Logger; After func(time.Duration) <-chan time.Time; NewTicker func(time.Duration) (<-chan time.Time, func()); Jitter func(time.Duration) time.Duration; SweeperInterval time.Duration }
func NewScheduler(d SchedulerDeps) *Scheduler
func (s *Scheduler) Register(spec Spec, job Job)
func (s *Scheduler) Run(ctx context.Context)             // blok; saat ctx selesai berhenti memulai run baru, menunggu run berjalan
func (s *Scheduler) RunOnce(ctx context.Context, name string) (ran bool, err error) // untuk `worker run-job`
```

- [ ] **Step 1: Write the failing test** — unit `TestSpecs_MatchSection991` (9 baris persis: nama, flag, interval `10s,30s,1h,1h,1h,1h,15m,1h,24h`, lock `7100001…7100009`, timeout `30s,25s,15m,15m,15m,10m,10m,15m,10m`). `TestSpecsForFlags_Sweeper_Cleanup` (`sweeper` → 2 job; `cleanup` → 7; `outbox` → 0). `TestScheduler_InitialJitterThenTicks` (Jitter/After/NewTicker palsu: job tidak jalan sebelum jitter; lalu jalan per tick). `TestScheduler_NoOverlapOnSameInstance` (run lambat, tick kedua datang → tidak dimulai paralel). `TestScheduler_SkippedLocked_Metric` (Locker `ran=false` → `kaiwa_job_runs_total{job,result="skipped_locked"}`, job tidak dipanggil). `TestScheduler_OkAndErrorResults` (`ok`, `error` + `kaiwa_job_duration_seconds` + `last_success` hanya pada ok). `TestScheduler_RunTimeout` (ctx job punya deadline = `Spec.Timeout`). `TestScheduler_StopsStartingNewRunsOnCancel`. `TestScheduler_SweeperIntervalOverride` (`SweeperInterval=2s` hanya mengubah `session_sweeper`). `TestRunOnce_UnknownJob_Error`. Integration `TestPgLocker_ExclusiveAcrossConnections` (dua goroutine kunci sama: tepat satu `ran=true`, satunya `false`; setelah selesai kunci bisa diambil lagi; `fn` error tetap melepas kunci; kunci berbeda tidak saling blok).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run 'TestSpecs|TestScheduler|TestRunOnce'` → FAIL; `go test -tags integration ./internal/jobs/... -race -run TestPgLocker` → FAIL.
- [ ] **Step 3: Implement** — satu goroutine per job; nilai tabel sebagai konstanta kode (bukan konfigurasi; lock key tak pernah dari env); `RunOnce` memakai `Locker` yang sama + timeout `Spec.Timeout`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): scheduler, advisory lock and canonical job table"`

### Task 14: Job `session_sweeper` + `reservation_cleanup` (membungkus M3)

**Files:**
- Create: `internal/jobs/session_sweeper.go`, `internal/jobs/reservation_cleanup.go`, `internal/testutil/sessionenv/env.go`
- Modify: `internal/session/sweeper.go` (aditif: field `SweepReport`), `internal/session/sweeper_test.go`
- Test: `internal/jobs/session_sweeper_test.go`, `internal/jobs/reservation_cleanup_test.go`, `internal/jobs/reservation_cleanup_integration_test.go`

**Interfaces:**
- Consumes: M3 `session.Sweeper.Tick(ctx) (SweepReport, error)`, `SweepReport{Rule map[string]RuleResult}`, `RuleResult{OK, Noop, Error int; Skipped bool}`, `session.Service.Abandon(ctx, id, actor) (Session, error)`, `session.Repository.ListAbandonCandidates` (diasumsikan `(ctx, now time.Time, limit int) ([]uuid.UUID, error)`; bila beda, tulis adapter tipis di `jobs`), `session.ErrInvalidTransition/ErrFenceLost`.
- Produces: `SweepReport` +`HeartbeatSuspended bool; MassHeartbeatLoss bool; HeartbeatCandidates, ActiveNonHandoff int` (aditif; M3 mengisinya dari guard & hitungan yang sudah ada). `type SessionTicker interface{ Tick(ctx context.Context) (session.SweepReport, error) }`; `func NewSessionSweeperJob(newSweeper func() SessionTicker, m *Metrics, log *slog.Logger, clk clock.Clock, interval time.Duration) *SessionSweeperJob` (`Run`); `type AbandonSource interface{ ListAbandonCandidates(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) }`; `type Abandoner interface{ Abandon(ctx context.Context, id uuid.UUID, actor session.Actor) (session.Session, error) }`; `func NewReservationCleanupJob(src AbandonSource, ab Abandoner, m *Metrics, clk clock.Clock, log *slog.Logger) *ReservationCleanupJob`. Harness: `sessionenv.New(t testing.TB) *Env` (`Pool, Redis, Clock *clock.Fake, Store *postgres.Store, Sessions *session.Service, Sweeper *session.Sweeper, Balance *balance.Service`, `SeedActive(t, o ActiveOpts) uuid.UUID` memakai transisi nyata `Create→ClaimRuntime→BeginStart→Activate`) — pindahkan dari harness M3 `test/integration` bila konstruktornya tidak terekspor.

- [ ] **Step 1: Write the failing test** — unit `TestSessionSweeperJob_MapsReportToMetrics` (report fake rule b `OK=2,Noop=1,Error=1` → `kaiwa_sweeper_actions_total{rule="b",result="ok"}=2` dst., `kaiwa_job_items_total{job="session_sweeper",action="rule_b_ok"}`). `TestSessionSweeperJob_HeartbeatSuspendedGauge` (`HeartbeatSuspended` true → gauge 1; false → 0). `TestSessionSweeperJob_MassLoss_LogsErrorAndCounts` (`MassHeartbeatLoss` → log ERROR `sweeper.mass_heartbeat_loss` dengan `candidates`,`active`; `kaiwa_sweeper_mass_heartbeat_loss_total` +1). `TestSessionSweeperJob_TickErrorReturned`. `TestSessionSweeperJob_NewSweeperAfterLockGap` (gap `> 2×interval` sejak run terakhir → `newSweeper` dipanggil lagi sehingga guard Redis mulai dari nol; gap kecil → instance sama). Di `session`: `TestSweepReport_HeartbeatFlags` (30 kandidat dari 40 → `MassHeartbeatLoss`, `HeartbeatCandidates=30`, `ActiveNonHandoff=40`; Redis error → `HeartbeatSuspended`). `TestReservationCleanup_AbandonsEachCandidate_ActorCleanup_Limit200`; `ErrInvalidTransition` → `kaiwa_job_items_total{action="noop"}`, error lain → `error` dan loop lanjut. Integration `TestReservationCleanup_ReleasesOnlyStaleReserved` (sesi `RESERVED` lewat deadline → `ABANDONED`/`NOT_STARTED`, reservasi `RELEASED`, ledger `RELEASE` `release:{reservation_id}`; sesi `ACTIVE` dan `RESERVED` belum lewat deadline tak tersentuh; run kedua no-op; `AssertReconciled`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... ./internal/session/... -race -run 'TestSessionSweeperJob|TestSweepReport|TestReservationCleanup'` → FAIL; `go test -tags integration ./internal/jobs/... -race -run TestReservationCleanup_` → FAIL.
- [ ] **Step 3: Implement** — job tipis (tanpa aturan bisnis); `Abandon` dengan `session.Actor("CLEANUP")`; `Run` mengembalikan error hanya untuk kegagalan `Tick`/listing.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): session sweeper and reservation cleanup jobs with sweeper report metrics"`

### Task 15: Job `balance_reconcile`

**Files:**
- Create: `internal/jobs/balance_reconcile.go`
- Modify: `internal/balance/reconcile.go` (lanjut ke check berikut bila satu `RunCheck` gagal; kembalikan `errors.Join`), `internal/postgres/balance_repo.go` (`RunCheck` di tx `READ ONLY` + `SET LOCAL statement_timeout='60s'`, hanya bila belum)
- Test: `internal/jobs/balance_reconcile_test.go`, `internal/balance/reconcile_continue_test.go`, `internal/jobs/balance_reconcile_integration_test.go`

**Interfaces:**
- Consumes: M3 `balance.Service.Reconcile(ctx) ([]Violation, error)`, `Violation{Check string; Rows []string}`.
- Produces: `type Reconciler interface{ Reconcile(ctx context.Context) ([]balance.Violation, error) }`; `func NewBalanceReconcileJob(r Reconciler, m *Metrics, log *slog.Logger) *BalanceReconcileJob` (`Run`).

- [ ] **Step 1: Write the failing test** — unit `TestBalanceReconcile_SetsGaugePerCheck` (violation R3 dengan 2 baris → `kaiwa_balance_invariant_violations{check="R3"}=2`, R1,R2,R4,R5,R6 = 0; ERROR log `balance.reconcile.mismatch` memuat `check` dan ≤ 20 id; tidak ada tulisan ke DB). `TestBalanceReconcile_ErrorReturnedAfterGaugesSet` (Reconciler mengembalikan violations + error → gauge tetap diset, `Run` mengembalikan error). `TestReconcile_ContinuesAfterCheckError` (di `balance`: fake `RunCheck("R2")` error → R1,R3–R6 tetap dijalankan, hasil R3 dikembalikan, error `errors.Join`). Integration: `TestBalanceReconcile_CleanDB_AllZero` (setelah satu lifecycle M3 normal → 6 gauge 0); `TestBalanceReconcile_DetectsTamper` (`UPDATE balance_accounts SET available_seconds = available_seconds + 1` → R1 dan R3 > 0; tidak ada auto-fix: nilai tetap); `TestBalanceReconcile_R6_StuckReservation` (`start_deadline_at = now() − 11 menit`, sesi `RESERVED` → R6 = 1); `TestRunCheck_ReadOnlyTxAndTimeout` (dalam eksekusi, `SHOW transaction_read_only`=`on`, `statement_timeout`=`1min`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... ./internal/balance/... -race -run 'TestBalanceReconcile|TestReconcile'` → FAIL; `go test -tags integration ./internal/jobs/... ./internal/postgres/... -race -run 'TestBalanceReconcile|TestRunCheck'` → FAIL.
- [ ] **Step 3: Implement** — gauge diset untuk semua R1–R6 pada setiap run (0 bila tidak ada baris).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): balance reconcile job with invariant gauges"`

### Task 16: Job `partition_maintenance`

**Files:**
- Create: `internal/jobs/partition_maintenance.go`, `internal/postgres/partition_repo.go`
- Test: `internal/jobs/partition_maintenance_test.go`, `internal/postgres/partition_repo_integration_test.go`

**Interfaces:**
- Consumes: `clock.Clock`.
- Produces: `type Partition struct{ Name string; Lower, Upper time.Time; DetachPending bool }`; `type PartitionStore interface{ List(ctx context.Context, table string) ([]Partition, error); Ensure(ctx context.Context, table string, month time.Time) error; Drop(ctx context.Context, table string, p Partition) error }` (impl `postgres.NewPartitionRepo(pool)`; `Ensure` = `SET lock_timeout='5s'` + `CREATE TABLE IF NOT EXISTS … PARTITION OF`; `Drop` = `DETACH … CONCURRENTLY` di luar tx lalu `DROP`, atau `FINALIZE` bila `DetachPending`); pure: `func PartitionName(table string, month time.Time) string` (`session_turns_p202701`); `func PlanCreate(now time.Time) []time.Time` (4 bulan UTC: berjalan..+3); `func ExpiredPartitions(table string, parts []Partition, now time.Time, retentionDays int) []Partition`; `func ParseBound(expr string) (lower, upper time.Time, err error)` (format `pg_get_expr`); `func NewPartitionMaintenanceJob(s PartitionStore, cfg PartitionConfig, m *Metrics, clk clock.Clock, log *slog.Logger) *PartitionMaintenanceJob` dengan `PartitionConfig{RetentionTranscriptDays, RetentionSessionEventsDays int}`.

- [ ] **Step 1: Write the failing test** — unit `TestPartitionName`, `TestPlanCreate_FourMonthsUTC` (now `2026-12-15` → `202612,202701,202702,202703`; `2026-12-31T23:30Z` tetap mulai Desember), `TestParseBound` (`FOR VALUES FROM ('2026-10-01 00:00:00+00') TO ('2026-11-01 00:00:00+00')`), `TestExpiredPartitions_Turns` (now `2027-06-15`, retensi 180: batas `now−180d−2h` = `2026-12-17 22:00Z`; partisi `202611` (upper `2026-12-01`) → drop; `202612` (upper `2027-01-01`) → **tidak**), `TestExpiredPartitions_Events365`, `TestExpiredPartitions_NeverDropsCurrentOrFutureMonth` (retensi 0 hari tetap tidak drop bulan berjalan), `TestJob_CreatesMissing_IgnoresExisting`, `TestJob_CreateErrorLogged_ContinuesToDrop` (Review Focus 5 guard), `TestJob_GaugesAndCounters` (`kaiwa_partition_months_ahead{table}` = jumlah partisi dengan `Lower > now`; `partitions_created/dropped_total`). Integration `TestPartitionRepo_*`: `Ensure` idempoten; `Drop` partisi lama (`CONCURRENTLY`); `DetachPending` → `FINALIZE` lalu drop; `INSERT` lintas dua partisi benar; tanpa partisi `DEFAULT`; `List` mengembalikan batas dari `pg_inherits`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run 'TestPartition|TestPlanCreate|TestParseBound|TestExpired|TestJob_'` → FAIL; `go test -tags integration ./internal/postgres/... -race -run TestPartitionRepo` → FAIL.
- [ ] **Step 3: Implement** — DDL pakai pgx mentah dengan identifier hasil `PartitionName` (tervalidasi regex `^session_(turns|events)_p\d{6}$`, tanpa input user); gagal satu partisi → log ERROR dan lanjut.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): partition maintenance for session turns and events"`

### Task 17: Job `transcript_retention` (+ pembersihan token)

**Files:**
- Create: `internal/jobs/transcript_retention.go`, `internal/postgres/retention_repo.go`
- Test: `internal/jobs/transcript_retention_test.go`, `internal/postgres/retention_repo_integration_test.go`

**Interfaces:**
- Consumes: `clock.Clock`, `PartitionStore.Ensure` (untuk menyiapkan partisi lama di test), `testutil.SeedSession`.
- Produces: `type RetentionStore interface{ PurgeEvaluationDetails(ctx context.Context, before time.Time, batch int) (int, error); DeleteTurnsBefore(ctx context.Context, cutoff time.Time, batch int) (int, error); DeleteEventsBefore(ctx context.Context, cutoff time.Time, batch int) (int, error); PurgeSessionSummaries(ctx context.Context, before time.Time, batch int) (int, error); DeleteExpiredTokens(ctx context.Context, now time.Time, tokenRetention time.Duration, batch int) (int, error) }` (impl `postgres.NewRetentionRepo(pool)`); `type RetentionConfig struct{ TranscriptDays, SessionEventsDays, ExpiredTokensDays int }`; `func NewTranscriptRetentionJob(s RetentionStore, cfg RetentionConfig, m *Metrics, clk clock.Clock, log *slog.Logger) *TranscriptRetentionJob`.

- [ ] **Step 1: Write the failing test** — unit (store palsu): `TestRetentionJob_LoopsEachStepUntilShortBatch` (batch `500/5000/5000/1000/1000`; store mengembalikan penuh, penuh, sisa → 3 panggilan), `TestRetentionJob_Cutoffs` (now `T`: evaluasi `before = T−180d`; turns `cutoff = T−180d−2h`; events `cutoff = T−365d`; summary `before = T−180d`; token retention 7 hari), `TestRetentionJob_StepErrorContinuesAndReturnsError`, `TestRetentionJob_ItemsMetric` (action `evaluation_details|turns|events|summaries|tokens`). Integration (partisi lama dibuat via `PartitionRepo.Ensure`): `TestRetention_Turns_NotDeletedBefore180DaysAfterEnded` (turn `created_at = now−180d−1h` (< margin 2 jam) tetap ada; `now−180d−3h` terhapus — Review Focus 5), `TestRetention_PartitionBoundaryRowsDeletedPrecisely` (dua turn di partisi batas: satu lewat batas dihapus, satu tidak), `TestRetention_Events_365d`, `TestRetention_SessionSummary_Nulled_AndPurgedAtSet` (`ended_at < now−180d` → `summary_text NULL`, `transcript_purged_at` terisi; `ended_at` 179 hari tidak berubah), `TestRetention_EvaluationDetails_FindingsDeletedScoresKept` (`details_purged_at` terisi, `*_score`/`overall_score` utuh, kolom teks dikosongkan, run kedua 0 baris), `TestRetention_Tokens` (family kedaluwarsa > 7 hari terhapus, family aktif tidak; `password_reset_tokens` > 24 jam terhapus).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run TestRetentionJob` → FAIL; `go test -tags integration ./internal/postgres/... -race -run TestRetention_` → FAIL.
- [ ] **Step 3: Implement** — SQL §9.9.4 langkah 1–5 persis (`FOR UPDATE SKIP LOCKED`, `DELETE … WHERE (id, created_at) IN (…)` agar partition pruning bekerja), tiap batch satu transaksi.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): transcript retention with token cleanup"`

### Task 18: `objectstore` dasar + job `audio_tmp_cleanup`

**Files:**
- Create: `internal/objectstore/{config,client}.go`, `internal/testutil/minio.go`, `internal/jobs/audio_tmp_cleanup.go`
- Test: `internal/objectstore/client_integration_test.go`, `internal/jobs/audio_tmp_cleanup_test.go`, `internal/jobs/audio_tmp_cleanup_integration_test.go`

**Interfaces:**
- Consumes: `config.Worker.S3`.
- Produces: `type Config struct{ Endpoint, Region, Bucket, AccessKeyID, SecretAccessKey string; UsePathStyle bool; SSE string; RequestTimeout time.Duration; ListPageSize int }`; `type ObjectInfo struct{ Key string; LastModified time.Time; Size int64 }`; `func New(ctx context.Context, cfg Config) (*Client, error)`; `(*Client).PutObject(ctx, key string, body []byte, contentType string, meta map[string]string) error` (SSE header bila `cfg.SSE`); `GetObject(ctx, key string) ([]byte, error)` (`objectstore.ErrNotFound`); `DeleteObjects(ctx, keys []string) error` (batch 1000, objek hilang bukan error, key di luar `audio/tmp/` → `ErrKeyOutsidePrefix` (nama dari plan)); `ListObjects(ctx, prefix string, fn func(page []ObjectInfo) error) error` (default 1000/halaman). `testutil.NewMinIO(t) (*s3.Client, bucket string)` (bucket `t-<uuid>`; override `TEST_S3_ENDPOINT`). Jobs: `type AudioObjects interface{ ListObjects(ctx context.Context, prefix string, fn func([]objectstore.ObjectInfo) error) error; DeleteObjects(ctx context.Context, keys []string) error }`; `type AudioKeyStore interface{ ClearAudioKeys(ctx context.Context, keys []string, now time.Time) (int, error) }` (impl di `postgres.RetentionRepo`: `UPDATE session_turns SET user_audio_object_key = NULL WHERE user_audio_object_key = ANY($1) AND created_at > $2 − interval '3 days'`); `func NewAudioTmpCleanupJob(o AudioObjects, k AudioKeyStore, retention time.Duration, m *Metrics, clk clock.Clock, log *slog.Logger) *AudioTmpCleanupJob`.

- [ ] **Step 1: Write the failing test** — integration objectstore: `TestClient_PutGetDelete_RoundTrip` (`audio/tmp/{sid}/{tid}.wav`, `Content-Type audio/wav`, meta `session-id`,`turn-id`), `TestClient_GetMissing_ErrNotFound`, `TestClient_ListPrefix_Paginates` (`ListPageSize=2`, 5 objek → 3 halaman; prefix lain tidak ikut; `LastModified` ≈ now), `TestClient_DeleteObjects_MissingKeyOK_And_BatchOver1000`, `TestClient_DeleteOutsidePrefix_Rejected`. Unit job: `TestAudioCleanup_ThresholdIs23h` (retention 24h; now `T`: objek `LastModified = T−23h−1m` dihapus, `T−22h59m` tidak), `TestAudioCleanup_DeletesPer1000AndClearsKeys`, `TestAudioCleanup_ListErrorReturned`, `TestAudioCleanup_NothingToDelete_NoDeleteCall`. Integration `TestClearAudioKeys_OnlyRecentPartitionsPredicate` (turn `created_at = now−2d` dikosongkan; `now−4d` tidak disentuh).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run TestAudioCleanup` → FAIL; `go test -tags integration ./internal/objectstore/... ./internal/postgres/... -race -run 'TestClient_|TestClearAudioKeys'` → FAIL.
- [ ] **Step 3: Implement** — `aws-sdk-go-v2` (`go get github.com/aws/aws-sdk-go-v2/{config,credentials,service/s3}`), kredensial kosong → default chain; path-style bila `UsePathStyle`; timeout per call `RequestTimeout`; image MinIO sama dengan compose.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(objectstore,jobs): s3 client basics and audio tmp cleanup"`

### Task 19: Job `outbox_retention`

**Files:**
- Create: `internal/jobs/outbox_retention.go`
- Modify: `internal/postgres/retention_repo.go`
- Test: `internal/jobs/outbox_retention_test.go`, `internal/postgres/outbox_retention_integration_test.go`

**Interfaces:**
- Produces: `type OutboxRetentionStore interface{ DeleteOutboxPublished(ctx context.Context, before time.Time, batch int) (int, error); DeleteOutboxFailed(ctx context.Context, before time.Time, batch int) (int, error); DeleteProcessedEvents(ctx context.Context, before time.Time, batch int) (int, error) }` (impl `RetentionRepo`, `DELETE … WHERE ctid IN (SELECT ctid … LIMIT $n)`); `type OutboxRetentionConfig struct{ PublishedDays, ProcessedEventsDays int }` (FAILED = 30 hari tetap); `func NewOutboxRetentionJob(s OutboxRetentionStore, cfg OutboxRetentionConfig, m *Metrics, clk clock.Clock, log *slog.Logger) *OutboxRetentionJob`.

- [ ] **Step 1: Write the failing test** — unit `TestOutboxRetention_Cutoffs` (now `T`: published `T−7d`, failed `T−30d`, processed `T−30d`), `TestOutboxRetention_LoopsUntilBatchUnder5000` (5000, 5000, 120 → 3 panggilan per tipe), `TestOutboxRetention_ErrorContinuesToNextStep`. Integration: `TestOutboxRetention_DeletesOnlyEligible` (PUBLISHED `published_at` 8 hari → hapus, 6 hari → tetap; PENDING lama tak tersentuh; FAILED `created_at` 31 hari → hapus, 29 → tetap; `processed_events` 31/29 hari), `TestOutboxRetention_Batches` (5.001 baris → seluruhnya terhapus dalam 2 batch).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run TestOutboxRetention` → FAIL; `go test -tags integration ./internal/postgres/... -race -run TestOutboxRetention` → FAIL.
- [ ] **Step 3: Implement** — tiga langkah berurutan, tiap langkah diulang sampai `< 5000`; metrik `kaiwa_job_items_total{action="outbox_published|outbox_failed|processed_events"}`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): outbox and processed events retention"`

### Task 20: Job `memory_cleanup` (kerangka; isi kebijakan retensi = M8)

**Files:**
- Create: `internal/jobs/memory_cleanup.go`, `internal/redisstore/memver.go`
- Modify: `internal/postgres/retention_repo.go` (query `SysDeleteExpiredMemories`)
- Test: `internal/jobs/memory_cleanup_test.go`, `internal/redisstore/memver_integration_test.go`, `internal/postgres/memory_expired_integration_test.go`

**Interfaces:**
- Produces: `type ExpiredMemoryStore interface{ DeleteExpiredMemories(ctx context.Context, limit int) ([]uuid.UUID, error) }` (impl `RetentionRepo`, SQL §11.8.3 verbatim); `type MemVerBumper interface{ Bump(ctx context.Context, userIDs []uuid.UUID) error }`; `redisstore.NewMemVerBumper(rdb redis.UniversalClient) *MemVerBumper` (`INCR kaiwa:user:{id}:memver`, lalu `EXPIRE` 30 hari agar tiap key punya TTL); `func NewMemoryCleanupJob(s ExpiredMemoryStore, b MemVerBumper, m *Metrics, log *slog.Logger) *MemoryCleanupJob`. M8 boleh mengganti implementasi `ExpiredMemoryStore` tanpa mengubah port.

- [ ] **Step 1: Write the failing test** — unit `TestMemoryCleanup_LoopsUntilUnder1000_BumpsUniqueUsersOnce` (batch 1: 1000 baris dari 3 user; batch 2: 40 baris → `Bump` dipanggil dengan 3 user unik per run), `TestMemoryCleanup_NoRows_NoBump`, `TestMemoryCleanup_BumpErrorReturned`. Integration: `TestMemVerBumper_IncrementsAndHasTTL` (`INCR` dua kali = 2, `TTL > 0`), `TestDeleteExpiredMemories_HardDeletesExpiredOnly` (`expires_at < now()` dihapus termasuk `SUPERSEDED`; `expires_at` masa depan tetap; return `user_id`; memory user `DELETED` bukan urusan job).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run TestMemoryCleanup` → FAIL; `go test -tags integration ./internal/redisstore/... ./internal/postgres/... -race -run 'TestMemVerBumper|TestDeleteExpiredMemories'` → FAIL.
- [ ] **Step 3: Implement** — batch 1.000 per iterasi; `kaiwa_job_items_total{action="memories_deleted"}`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): memory cleanup skeleton with memver bump"`

### Task 21: Job `account_purge`

**Files:**
- Create: `internal/jobs/account_purge.go`, `internal/postgres/purge_repo.go`
- Test: `internal/jobs/account_purge_test.go`, `internal/postgres/purge_repo_integration_test.go`, `internal/jobs/account_purge_integration_test.go`

**Interfaces:**
- Consumes: `AudioObjects` (Task 18), `testutil.SeedUser/SeedSession`, M2 `users.status='DELETED'`.
- Produces: `type PurgeStore interface{ ListCandidates(ctx context.Context, deletedBefore time.Time, limit int) ([]uuid.UUID, error); SessionIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error); DeleteMemories(ctx, userID uuid.UUID) error; DeleteLearner(ctx, userID uuid.UUID) error; DeleteEvaluations(ctx, userID uuid.UUID) error; DeleteTranscripts(ctx, userID uuid.UUID, batch int) error; ClearSummaries(ctx, userID uuid.UUID) error; Anonymize(ctx context.Context, userID uuid.UUID, now time.Time) error }` (impl `postgres.NewPurgeRepo(pool)`; `ListCandidates`: `status='DELETED' AND purged_at IS NULL AND deleted_at < $1 ORDER BY deleted_at LIMIT $2`); `func NewAccountPurgeJob(s PurgeStore, o AudioObjects, delay time.Duration, m *Metrics, clk clock.Clock, log *slog.Logger) *AccountPurgeJob`. Urutan per user: (1) objek audio per sesi, (2) memory, (3) learner, (4) evaluasi, (5) transcript, (6) summary, (7) anonimisasi + `purged_at`, lalu log audit `account_purged` (hanya `user_id`). Learner dihapus **sebelum** evaluasi karena FK `learner_* → evaluations` (migration 000019): deviasi urutan §13.9.3 langkah 3/4, catat di deviations. Saldo/ledger/reservasi/`sessions` tidak dihapus.

- [ ] **Step 1: Write the failing test** — unit (store/objek palsu, rekam panggilan): `TestPurge_StepOrderPerUser` (`[objects, memories, learner, evaluations, transcripts, summaries, anonymize]`), `TestPurge_Max50UsersPerRun` (limit 50, urut `deleted_at`), `TestPurge_CandidateCutoff` (`deleted_at < now − delay`), `TestPurge_FailureMidwayNotAnonymized_NextUserContinues` (objek gagal untuk user A → `Anonymize(A)` tidak dipanggil, user B diproses), `TestPurge_ObjectsListedPerSessionPrefix` (`audio/tmp/{session_id}/` untuk tiap sesi). Integration: `TestAccountPurge_RemovesDataKeepsFinancials` (user `DELETED` dengan sesi, turns, events, evaluasi+findings, learner, memories, ledger, 2 objek MinIO → semuanya hilang kecuali ledger/reservasi/`sessions`; `users.email = 'deleted+<id>@deleted.invalid'`, `phone_number NULL`, `full_name 'deleted'`, `password_hash '!'`, `purged_at` terisi; user `ACTIVE` dan `DELETED` belum lewat delay tak tersentuh; `admin_audit_logs`/`processed_events` tak berubah; `AssertReconciled`), `TestAccountPurge_RerunAfterCrash_Idempotent` (injeksi error di langkah evaluasi → `purged_at` NULL; run kedua penuh sukses; run ketiga no-op), `TestAccountPurge_NoPIIRemainsInSummaries`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/jobs/... -race -run TestPurge` → FAIL; `go test -tags integration ./internal/postgres/... ./internal/jobs/... -race -run TestAccountPurge` → FAIL.
- [ ] **Step 3: Implement** — `DeleteTranscripts` memakai `DELETE … WHERE session_id = ANY($1)` batch 1.000 untuk `session_turns` lalu `session_events`; semua langkah idempoten.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(jobs): account purge job with anonymization"`

### Task 22: `cmd/worker` — wiring, `--handlers`, `dlq-replay`, `run-job`, readiness, shutdown

**Files:**
- Modify: `cmd/worker/main.go`, `internal/platform/health/ops.go` (aditif `OpsOptions.LiveChecks []Check`)
- Create: `cmd/worker/{wiring,commands,lifecycle}.go`
- Test: `cmd/worker/handlers_test.go`, `cmd/worker/lifecycle_test.go`, `cmd/worker/main_integration_test.go`, `internal/platform/health/live_test.go`

**Interfaces:**
- Consumes: semua task sebelumnya, M1 `config.ParseWorkerHandlers`, `health.NewOps`.
- Produces: `func run(ctx context.Context, args []string) error` (args[0]=`dlq-replay` | `run-job` | selain itu mode server); `func buildJobs(d jobDeps) map[string]jobs.Job` (9 job); `func buildConsumers(flags []string, d consumerDeps) ([]*amqp.Consumer, error)` — `evaluation|memory|learner|summary` mengembalikan `errHandlerNotImplemented` (nama dari plan; M7/M8 mengisi case ini); `type Lifecycle struct{ SetNotReady func(); StopScheduler func(context.Context); ShutdownConsumers func(ctx context.Context, grace time.Duration) error; StopPublisher func(context.Context) error; CloseAMQP func() error; ClosePool func(); FlushOTel func(context.Context) error }` dengan `(l Lifecycle) Shutdown(ctx context.Context, grace time.Duration) error` (urutan §9.7.7: ready off → scheduler → consumer → publisher → AMQP → pool → OTel). `health.OpsOptions.LiveChecks`: dievaluasi tiap `/readyz` (timeout 1 s), gagal → 503 `{"status":"not_ready"}` (berbeda dari `Checks` startup M1). CLI: `worker dlq-replay --queue <Q> [--limit 100] [--dry-run]`; `worker run-job <name>` mencetak `job=<name> ran=<true|false>` (exit 0 juga bila `ran=false`; exit 1 bila job error; nama tak dikenal → exit 2).

- [ ] **Step 1: Write the failing test** — unit `TestHandlerSets_Section98` (empat Deployment: `outbox,sweeper,cleanup` → publisher + 2 + 7 job, 0 consumer; `evaluation` → hanya consumer evaluasi; `memory`; `learner,summary` → 2 consumer; semuanya tanpa job/publisher kecuali core), `TestBuildConsumers_UnimplementedHandlerFailsStartup` (`evaluation` → `errors.Is(err, errHandlerNotImplemented)` dengan nama handler), `TestRunRejectsEmptyOrUnknownHandlers` (M1 tetap), `TestRunJob_UnknownJobExit2`, `TestLifecycle_ShutdownOrder` (callback merekam: `ready_off, scheduler, consumers, publisher, amqp, pool, otel`; `grace` diteruskan; error satu langkah tidak membatalkan langkah berikutnya, error pertama dikembalikan), `TestLiveChecks_ReadyzTurns503WhenCheckFails` (Ops: ready → 200; `LiveChecks` gagal → 503; pulih → 200; body tanpa teks error). Integration `TestWorker_RunCore_ReadyAndMetrics` (`--handlers=outbox,sweeper,cleanup` + PG/Redis/RabbitMQ container → `/readyz` 200 ≤ 10 s, `/metrics` memuat `kaiwa_outbox_pending` dan `kaiwa_job_runs_total`; `CloseAllConnections()` → `/readyz` 503 lalu 200 setelah reconnect; cancel ctx → `run` kembali nil ≤ `WORKER_SHUTDOWN_GRACE`), `TestWorker_RunJob_ExecutesWithAdvisoryLock` (`run-job outbox_retention` → `ran=true`; saat lock `7100005` dipegang koneksi lain → `ran=false`), `TestWorker_DLQReplay_Subcommand` (pesan di DLQ → pindah, output jumlah).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/worker/... ./internal/platform/health/... -race -run 'TestHandlerSets|TestBuildConsumers|TestRunRejects|TestRunJob|TestLifecycle|TestLiveChecks'` → FAIL; `go test -tags integration ./cmd/worker/... -race -run TestWorker_` → FAIL.
- [ ] **Step 3: Implement** — wiring manual (D-004): dua `amqp.Dial` terpisah (publisher & consumer), `NewScheduler` hanya dengan `SpecsForFlags`, `session.NewSweeper(...)`, `RunOutboxGauges` 15 s dan `RunQueueDepth` 30 s hanya bila flag `outbox`; `LiveChecks`: `pool.Ping` + `conn.Healthy()` bila ada flag AMQP; `Lifecycle` dipanggil saat ctx selesai dengan `cfg.ShutdownGrace`. Tidak ada `os.Getenv`.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `make arch-check` exit 0 (`CGO_ENABLED=0 go build ./cmd/worker` bersih).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(worker): handlers wiring, run-job, dlq-replay, dynamic readiness and ordered shutdown"`

### Task 23: Gerbang integration M6 — alur penuh, race, pod-kill setara, Redis flush

**Files:**
- Create: `test/integration/m6_async_test.go`
- Test: sama (tag `integration`)

**Interfaces:**
- Consumes: `sessionenv.New`, `amqp.OutboxPublisher`, `jobs.Scheduler.RunOnce`, `jobs.NewSessionSweeperJob`, `testutil.AssertReconciled`, `testutil.RabbitAdmin`.
- Produces: tidak ada kode produksi (hanya perbaikan bug yang ditemukan).

- [ ] **Step 1: Write the failing test** — PG+Redis+RabbitMQ, fake clock, tanpa consumer terpasang (queue menampung). `TestM6_SessionCompleted_FlowsToThreeQueues` (lifecycle M3 normal `USER_ENDED` → `PublishBatch` → `QueueMessages` = 1 pada `evaluation|memory|summary.session_completed`, 0 pada `learner.evaluation_completed`; `evaluation.completed` dari `NewEvaluationCompleted` → hanya `learner`; `memory.extracted` → `kaiwa.events.unrouted`). `TestM6_SessionCompleted_ExactlyOnce_RuntimeVsSweeperVsPublisher` (Review Focus 1; 20 sesi `ENDING` > 60 s; per sesi goroutine runtime `Finalize(fence)` dan `Sweeper.Tick` paralel serta 2 publisher paralel; `count(event_outbox WHERE aggregate_id=sesi)=1`, tiap queue tepat 20 pesan dengan 20 `event_id` unik, satu `CONSUME` per sesi, `AssertReconciled`, `-count=3`). `TestM6_PodKillEquivalent_HeartbeatLoss_FailsOnceWithFairCharge` (fake runtime: sesi `ACTIVE` mulai `t0`, heartbeat terakhir `t0+120s` ditulis langsung ke `kaiwa:session:{id}:hb`, lalu berhenti; `RunOnce("session_sweeper")` pada `t0+130s` (guard belum aktif → belum diproses), tick lagi pada `t0+200s` setelah Redis sehat ≥ 45 s → `FAILED`/`SERVER_ERROR`, `ended_at = t0+120s`, `charged_seconds = 90` (D-036: 120 − 30), tepat 1 outbox `final_status=FAILED`, publisher → 1 pesan per queue, run ketiga no-op, tidak ada reservasi `HELD` untuk sesi terminal (R4), `AssertReconciled`; varian `active_ms < 60s` → charge 0). `TestM6_RedisFlush_DoesNotFailHealthySessions` (Review Focus 2; 25 sesi `ACTIVE`, `FLUSHALL`, jalankan tick sampai `+60 s`: tidak ada sesi `FAILED`, `kaiwa_sweeper_mass_heartbeat_loss_total ≥ 1` atau `heartbeat_checks_suspended = 1`, ERROR log `sweeper.mass_heartbeat_loss` terekam; setelah heartbeat ditulis ulang sesi tetap `ACTIVE`). `TestM6_HandoffSessions_ExcludedFromMassLoss` (30 sesi handoff + 5 sesi hilang heartbeat → hanya 5 diproses sesuai aturan; (e) menutup yang handoff > 30 s dengan `RECONNECT_TIMEOUT`, `billable_end=handoff_at`). `TestM6_DLQReplay_Works` (handler gagal permanen → DLQ; setelah "perbaikan" `ReplayDLQ` → diproses sekali). `TestM6_OutboxSurvivesBrokerOutage` (broker diputus via `CloseAllConnections` sebelum publish: `attempts` tetap 0, setelah `Reconnect` event terkirim tepat sekali).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./test/integration -race -run TestM6_`. Expected: FAIL sampai semua task sebelumnya terintegrasi.
- [ ] **Step 3: Implement** — perbaiki bug di paket sumber yang ditunjuk kegagalan (bukan di test); tidak ada kode baru selain itu.
- [ ] **Step 4: Run tests to verify they pass** — `go test -tags integration ./test/integration -race -run TestM6_ -count=3` → PASS; `make test-integration` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test(integration): m6 async backbone gate scenarios"`

### Task 24: E2E pod-kill (hanya bila M5 selesai; tag `e2e m5`)

**Files:**
- Create: `test/e2e/m6_podkill_test.go`

**Interfaces:**
- Consumes: M5 `test/e2e` harness (subprocess realtime/api/worker, `kaiwaclient`), `worker run-job session_sweeper`, `testutil.RabbitAdmin`.
- Produces: tidak ada.

- [ ] **Step 1: Write the failing test** — `//go:build e2e && m5`. `TestM6_PodKill_SweeperCompletesOnce` (E5-varian: sesi `ACTIVE` pada proses realtime tunggal; `kill -9`; heartbeat berhenti; ulangi `worker run-job session_sweeper` tiap 10 s sampai status terminal (≤ 120 s): `FAILED`/`SERVER_ERROR`; tepat 1 `event_outbox` `session.completed`; `worker` dengan `--handlers=outbox` mengirimnya → 1 pesan di tiap queue sesi; ledger `RESERVE`+`CONSUME`/`RELEASE` konsisten, R1–R6 kosong; `charged_seconds` tidak memuat waktu setelah heartbeat terakhir). `TestM6_ClientDisconnect_NoCharge` (client memutus TCP > 30 s: runtime → `ENDING`/`RECONNECT_TIMEOUT` → `COMPLETED`; `disconnected_seconds` dikurangkan; satu `session.completed`).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags 'e2e m5' ./test/e2e -race -run TestM6_ -timeout 10m`. Expected: FAIL.
- [ ] **Step 3: Implement** — tambahkan proses `worker` ke `TestMain` M5 bila belum ada (env eksplisit, `WORKER_HANDLERS=outbox,sweeper,cleanup`, `RABBITMQ_URL` dari container).
- [ ] **Step 4: Run tests to verify they pass** — perintah yang sama → PASS. Bila M5 belum selesai: lewati task ini, skenario setara sudah ada di Task 23 (catat di `docs/implementation-log.md`).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "test(e2e): m6 pod kill and disconnect scenarios"`

---

## Gerbang milestone M6

Jalankan berurutan; catat perintah + ringkasan output di `docs/implementation-log.md`.

- [ ] `go build ./... && go vet ./... && golangci-lint run` → 0 issue; `make arch-check` → exit 0 (`CGO_ENABLED=0 go build ./cmd/api ./cmd/worker`).
- [ ] `make test` → PASS (`-race`), termasuk `internal/events`, `internal/amqp`, `internal/jobs`, `cmd/worker` unit.
- [ ] `make test-integration` → PASS, memuat: `TestDeclareTopology_*`, `TestRouting_*`, `TestRetryQueue_5s_*`, `TestPublishBatch_*`, `TestRetry_FirstRung_RealBroker`, `TestPoison_*`, `TestIdempotency_*`, `TestTrace_OutboxToConsumer_SameTraceID`, `TestReplayDLQ_*`, `TestPgLocker_*`, `TestReservationCleanup_*`, `TestBalanceReconcile_*`, `TestPartitionRepo_*`, `TestRetention_*`, `TestOutboxRetention_*`, `TestAccountPurge_*`, `TestClient_*` (objectstore), `TestWorker_*`.
- [ ] `go test -tags integration ./test/integration -race -run TestM6_ -count=3` → PASS (event `session.completed` dari lifecycle M3 sampai ketiga queue tepat sekali; DLQ replay berfungsi; pod-kill setara; Redis flush tidak menggagalkan sesi; AssertReconciled R1–R5 + R6 bersih).
- [ ] Bila M5 selesai: `go test -tags 'e2e m5' ./test/e2e -race -run TestM6_ -timeout 10m` → PASS.
- [ ] Coverage: `go run ./test/tools/covercheck -profile coverage.out -thresholds test/tools/covercheck/thresholds.txt` → `internal/events` ≥ 85%, `internal/amqp`/`internal/postgres`/`internal/objectstore` ≥ 70% (dari run integration), total `internal/...` ≥ 75%.
- [ ] Manual: `docker compose up -d && WORKER_HANDLERS=outbox,sweeper,cleanup make run-worker` lalu `curl -fsS localhost:9092/readyz` → `{"status":"ready"}`; `curl -s localhost:9092/metrics | grep -E 'kaiwa_outbox_pending|kaiwa_job_runs_total'` menampilkan kedua metrik; `go run ./cmd/worker run-job outbox_retention` → `job=outbox_retention ran=true`.
- [ ] Entri `docs/implementation-log.md`; deviasi (urutan purge, dry-run DLQ, JSON schema tanpa library, readiness dinamis, urutan sweeper M3) di `docs/implementation-deviations.md`.

## Self-Review

**Cakupan spec.** Dicakup: §9.1–§9.8, §9.9.1–§9.9.5, §9.9.7–§9.9.10, §9.10–§9.11 (kecuali env `SUMMARY_MAX_CHARS` → M7), §5.6.4 (mekanik job; semantik di M3), §4.8 (job; query R1–R6 di M3), §13.4.4 (metrik suspended), §13.5 (gauge queue depth), §13.6 (`objectstore` put/get/list/delete), §13.9.3 (purge), §14.3 butir RabbitMQ/outbox/jobs, §13.2.5/§13.2.6/§13.2.11/§13.2.13 (grup env worker). **Tidak dicakup (milik lain):** handler evaluation/memory/learner/summary + `summary_text` worker §9.9.6 dan `PromptFixtures` (M7/M8; seam `buildConsumers`), isi kebijakan retensi memory per kategori (M8), vhost `/kaiwa`, user RabbitMQ, manifest k8s/KEDA/alert (M10), upload audio realtime (M5).
**Konsistensi tipe.** `events.Envelope/Delivery/Permanent/ProcessedEvents` (Task 2) dipakai Task 3, 6, 8, 9; `Conn`/`ConfirmPublisher`/`Confirmation` (Task 5) → `BatchPublisher` (Task 7) dan `Publisher` (Task 9, 12); `Metrics` amqp (Task 7) dipakai Task 9; `jobs.Metrics`/`Scheduler`/`Job` (Task 13) dipakai Task 14–21; `AudioObjects` (Task 18) dipakai Task 21; `PartitionStore.Ensure` (Task 16) dipakai test Task 17; `RetentionRepo` tumbuh di Task 17, 18, 19, 20 (satu struct, interface kecil per job); `sessionenv.Env` (Task 14) dipakai Task 15, 21, 23. Nama berlabel "(nama dari plan)" tidak ada di spec.
**Asumsi lintas milestone.** Signature `ListAbandonCandidates`, field `SweepReport` tambahan, dan lokasi harness M3 diasumsikan seperti ditulis di Task 14; builder `New*` mengikuti gaya M3 `(id uuid.UUID, now, producer, userID, payload)` (bukan `idgen.Generator` seperti sketsa spec); `SessionCompletedV1` adalah alias `SessionCompletedPayload` milik M3.
