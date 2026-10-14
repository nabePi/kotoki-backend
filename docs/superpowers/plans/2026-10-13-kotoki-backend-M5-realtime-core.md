# Kotoki Backend — M5 Realtime Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Percakapan suara realtime end-to-end (WebSocket v1 → VAD/turn engine → STT→LLM→TTS pipelining → Opus downstream) dengan fake provider, reconnect/takeover/drain, `kotoki-cli`, dan e2e E1–E14 yang relevan.
**Architecture:** `conversation` (actor `Runtime.loop` per sesi, `Manager`, detector, generation, context builder) mengimpor `conversation/audio` (cgo) dan `conversation/protocol` (tanpa cgo); `wsgateway` satu-satunya pemakai `coder/websocket` di sisi server; state Redis lewat port `rtstate.Store` (paket tanpa cgo, agar `redisstore` — yang di-link `cmd/api` — tidak menarik `conversation/audio`, §2.2 aturan 5). Semua tulis Postgres ber-fence lewat `session.RuntimeService`/`TurnRepository`/`EventRepository` (M3); provider hanya lewat `provider.RealtimeRouter` (M4). Memory & learner = port + fake (nyata di M8).
**Tech Stack:** `gopkg.in/hraban/opus.v2`, `github.com/maxhawkins/go-webrtcvad` (cgo), `coder/websocket`, `go-redis/v9`, `aws-sdk-go-v2/service/s3`, `prometheus/client_golang`, OpenTelemetry (trace), `golang.org/x/sync/errgroup`, `testify`, `testcontainers` (redis/minio/postgres). Goleak **tidak** ada di D-003 → helper sendiri (Task 2).
**Spec:** docs/superpowers/specs/2026-10-12-kotoki-backend-design.md — Bab 7 (§7.1–§7.16), §14.4 (golden WS), §14.5 (E2E, fixture audio), §14.10.5, §14.11, §8.9 (fake, fault injection), §13.4.3 (limiter WS).
**Master plan:** docs/superpowers/plans/2026-10-13-kotoki-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M3 (`session.RuntimeService`, `session.TurnStart/TurnUser/TurnFinal/TurnRecord`, `session.TurnRepository/EventRepository/Event`, `session.Sweeper`, `session.HeartbeatReader`, `content.SessionConfigLoader/SessionConfig/SessionRefs/SessionSnapshot/CharacterSnapshot/Voice/ScenarioFormality/SpeakingRate/DefaultStyle/PromptVersion` (D-014/D-015: karakter dan TTS per karakter (`provider_name`, `model_name`, `voice_id`, `voice{gender,accent,speaking_rate,fallback?}`) dari `sessions.config_snapshot`, bukan baris `characters` hidup; tidak ada `sessions.provider_profile`; D-016: tanpa tier), seed konten, `cmd/api`, REST sesi/ticket) dan M4 (`provider.RealtimeRouter/Generation/RoutedSTTSession/NewRouter/RouterDeps/RouterConfig` (D-017: satu vendor aktif STT/LLM dari flag `*_ENABLED`; tanpa fallback vendor STT/LLM), `fake.New{STT,LLM,TTS}`/`LoadCapConfig`/`Deps`, `redisstore.NewFakeFaults`, `prompt.Render/Template/EstimateTokens/Budget/DefaultBudget/LearnerContext/PromptData/FormalityInstruction/NewSentenceParser(maxSentences)/Sentence{Index,Text}/NormalizeSpeakable/OutputContract(closing)`). Juga M2: `auth.TicketStore.Consume`, `redisstore.NewTicketStore`, `ratelimit.Limiter/Catalog` (`ws_connect_ip`, `ws_attach_user`), `redisstore.NewRateLimiter`. **M4 memiliki `internal/prompt`: M5 tidak membuat file di sana.**

## Global Constraints

- Master Global Constraints berlaku.
- Protokol: `version` selalu `1`; subprotocol `kotoki.v1`; JSON ≤ 16384 B, biner ≤ 4096 B; upstream `[0x01][u32 seq][opus]` (seq mulai 0 per koneksi, opus 1..4091 B); downstream `[0x02][u32 generation][u32 seq][opus]` (seq mulai 0 per generation, opus 1..4087 B); big-endian (D-073, §7.4.7).
- Limit input: biner token bucket 100/s burst 200; JSON 20/s burst 40; outbound queue 256; ping 15 s, idle 45 s; write timeout 5 s; `INVALID_MESSAGE` > 10/60 s dan `AUDIO_FRAME_INVALID` > 50/10 s → `PROTOCOL_VIOLATION` 1008; duplikat `event_id` (LRU 64) diabaikan (D-082, §7.4.10).
- Close code: 1000, 1008, 1011, 4001, 4003, 4004, 4008, 4009, 4010, 4011, 4029, 4100, 4101, 4102, 4500, 4503 (§7.4.8–7.4.9).
- VAD mode 2, frame 20 ms; speech start ≥ 10 voiced dari 15 frame; barge-in ≥ 15 dari 20 frame **dan** partial ≥ 2 rune sejak onset; cluster ≥ 3 dari 5 frame; `speech.ended` pada silence ≥ 300 ms; ambang silence 800 / 1400 (filler) / 500 (selesai + final diterima) / 2000 (filler-only) ms; max utterance 45000 ms (D-076, D-077, §7.6.2).
- Audio: decode upstream langsung 16 kHz mono (320 sampel/frame); encoder downstream 24 kHz mono `AppVoIP`, 32000 bps, complexity 5, DTX & FEC off, blok 480 sampel; gap seq ≤ 5 → PLC, ≤ 50 → silence, > 50 → 50 silence; watchdog 100 ms / starvation 200 ms (§7.5).
- Pacer: `AUDIO_MAX_LEAD_MS=400`, backpressure `Outbound.audio < 25` frame; `frames` cap 250, `sentences` cap 8, `cmdCh` 16, `audioIn` 50, `frameEvents` 64, `sttEvents` 64, `genEvents` 128, `writerAcks` 64, persister 64 (§7.7.2, §7.12.3).
- `generation` naik saat generation baru **dan** saat dibatalkan; frame gen lama dibuang writer; barge-in → frame audio terakhir ≤ 150 ms P95 (D-101).
- Redis: owner TTL 15 s refresh 5 s; hb tulis 5 s TTL 60 s; replay `XADD MAXLEN ~ 500` + `EXPIRE 120`; timeout operasi 200 ms; `TTL_S = max(300, expires_at−now+300)`; semua key ber-TTL (§7.9).
- Kapasitas: `/readyz` 503 ≥ 95; `session.start` baru ditolak 4503 ≥ 100; resume s.d. 120; upgrade HTTP 503 ≥ 120 atau draining, **sebelum** ticket dikonsumsi; drain safepoint 20 s, flush 5 s, close 4100 (§7.12).
- Expiry: peringatan T-60 s dan T-10 s, `balance.tick` tiap 10 s, grace 10 s, `RECONNECT_WINDOW=30s`, `MAX_CONSECUTIVE_FAILED_TURNS=3`, `PROVIDER_SESSION_GRACE=15s` (§7.2, §7.11).
- Metrik `kotoki_realtime_*` §7.14 (bucket `TURN_BUCKETS`/`BARGE_BUCKETS` Bab 13); tanpa `session_id`/`user_id` pada label; tanpa teks user/AI di log/span/metrik.
- Hanya `conversation/audio` yang cgo; `cmd/api`/`cmd/worker` tetap `CGO_ENABLED=0` (`make arch-check`); `conversation/protocol` dan `conversation/rtstate` tanpa cgo.

## Review Focus

1. **Mic senyap/noise & ucapan panjang** (master #2): noise-only tidak membuat turn dari detector (Task 9) dan turn `DISCARDED` bila STT kosong (Task 19); `ja_max_utterance` dipaksa selesai di 45 s dan tidak digabung ulang (Task 9, 19); filler `えっと…` tidak memotong (Task 9).
2. **Barge-in, echo, frame lama** (master #3): partial dari audio sebelum commit tidak mengonfirmasi barge-in (Task 9, 20); writer membuang frame `gen < active` (Task 10); client membuang frame `generation` lama (Task 27); P95 `bargein_stop_seconds` ≤ 0,15 s (Task 29).
3. **Putus di tengah turn / pod mati** (master #4): turn berjalan `FAILED`, detik terputus ke `AddDisconnected`, takeover menghentikan pemilik lama, sesi `ENDING` berhenti menagih (Task 21, 22; e2e E4/E5/E14 Task 30).
4. **STT final kosong, non-Jepang, content filter** (master #5): kosong → `DISCARDED` (§7.6.2 menang atas kata "FAILED" di master; dicatat di deviations), non-Jepang → `FAILED`+APOLOGY, `CONTENT_FILTERED` ×3 → tetap `ACTIVE`; ketiganya tidak menaikkan `consecutiveFailedTurns` dan tidak mengubah charge (Task 20).
5. **Redis mati saat sesi berjalan** (§7.9.4/§7.16 #8): sesi selesai & ter-settle, attach baru → 4503 (Task 15, 23).

## File Structure

| Path | Tanggung jawab |
|---|---|
| `internal/conversation/protocol/` | envelope, payload, registry event, codec, frame biner, katalog error/close code |
| `internal/conversation/audio/` | opus decode/encode, framer, WAV, VAD, capture, aset APOLOGY (cgo) |
| `internal/conversation/rtstate/` | tipe & port `Store` state Redis (tanpa cgo) |
| `internal/conversation/{detector,japanese,stt_state,conn,outbound,reader,writer,heartbeat,pacer,generation,context_builder,rolling_summary,adaptive,summarizer,persister,redissync,config_loader,event_writer,metrics,runtime*,manager,ports}.go` | runtime & turn engine (§7.1) |
| `internal/learner/difficulty.go`, `internal/memory/port.go` | tipe/port minimal (nyata di M8) |
| `internal/redisstore/realtime_state.go` | `rtstate.Store` (Lua owner, stream, hb, rt, turn, summary, pubsub, podinfo) |
| `internal/objectstore/audio.go` | `conversation.AudioStore` (put-only WAV) |
| `internal/wsgateway/` | handshake §7.3.1 langkah 1–6, adapter `conversation.Conn` |
| `internal/testutil/{replay,audiofx,rtharness,fakes/…}` | `ScriptedSTT`, harness runtime, fake port |
| `internal/kotokiclient/`, `internal/kotokiclient/audio/`, `cmd/kotoki-cli/` | klien uji REST+WS, pacer, skrip percakapan, CLI |
| `cmd/realtime/` | wiring manual, `/readyz`, podinfo |
| `test/tools/genaudio`, `test/tools/genapology`, `testdata/audio/synth/`, `internal/testutil/audiofx/`, `testdata/api/ws/` | fixture & golden |
| `test/e2e/` | E1–E7, E10–E14 + SLO |

---

### Task 1: Preflight kontrak, verifikasi kode error WS, config `RealtimeTuning`

**Files:**
- Create: `internal/m5preflight_test.go`, `internal/platform/config/realtime_tuning.go`
- Modify: `internal/platform/config/config.go` (tanam `RealtimeTuning` di `Realtime` milik M1 T4), `.env.example`, `internal/testutil/minimalenv.go` (perluas `testutil.MinimalEnv` bila ada env wajib baru), `internal/postgres/session_turn_repo.go`, `internal/postgres/session_event_repo.go`
- Test: `internal/platform/errs/m5codes_test.go`, `internal/platform/config/realtime_tuning_test.go`

**Interfaces:**
- Consumes: semua nama M2/M3/M4 di header; M1: `config.Realtime` (per-binary: `HTTPAddr`, `OpsAddr`, `PodID`, `WSAllowedOrigins`, `WSAllowEmptyOrigin`, `ReadyMaxSessions`, `MaxSessions`, `HardMaxSessions`, … — cek nama persis di M1 T4), `errs.Code<Pascal>` (mis. `errs.CodeInternalError`); seluruh 17 kode WS-only dan `ORIGIN_NOT_ALLOWED` (pesan `Permintaan ditolak.`) sudah dibuat M1 — **M5 tidak menambah/mengubah kode atau pesan `errs`**.
- Produces: `config.RealtimeTuning` (satu field per baris tabel §7.2 **yang belum ada di `config.Realtime` M1**, nama = PascalCase env, mis. `TurnSilenceFillerMS`, `RollingSummaryTriggerTurns`, tag `env`/`envDefault` persis tabel; tanpa `ProviderSessionGrace`/`ProviderTimeoutSTTFinal` — milik `provider.RouterConfig`), tertanam anonim di `config.Realtime`; `RealtimeTuning.Validate()`; `postgres.NewTurnRepo(pool) session.TurnRepository` dan `postgres.NewEventRepo(pool) session.EventRepository` (non-tx, bungkus query M3; nama dari plan).

- [ ] **Step 1: Write the failing test** — `TestM5Preflight` (kompilasi-saja: asersi `var _` untuk `*session.RuntimeService` memenuhi sepuluh method §7.13.1, `session.Event` punya `TurnID *uuid.UUID`, `provider.RealtimeRouter`, `prompt.Render`, `content.SessionConfigLoader`, `auth.TicketStore`, `ratelimit.Limiter`); `TestWSCodesExist` (semua kode yang dipakai M5 ada di `errs.Messages` dan `httpapi.StatusOf` memetakannya: `TICKET_INVALID, FORBIDDEN, SESSION_NOT_FOUND, HANDSHAKE_TIMEOUT, SESSION_STATE_CONFLICT, SESSION_NOT_RESUMABLE, SESSION_START_DEADLINE_PASSED, SESSION_TAKEN_OVER, RATE_LIMITED, SERVER_DRAINING, SLOW_CONSUMER, HEARTBEAT_TIMEOUT, PROVIDER_FAILURE, SERVER_AT_CAPACITY, SERVICE_UNAVAILABLE, PROTOCOL_VIOLATION, UNSUPPORTED_PROTOCOL_VERSION, UNSUPPORTED_AUDIO_FORMAT, INTERNAL_ERROR, INVALID_MESSAGE, COMMAND_REJECTED, AUDIO_FRAME_INVALID, AI_GENERATION_FAILED, ORIGIN_NOT_ALLOWED`; pesan `TICKET_INVALID` verbatim §7.4.8; `SERVER_DRAINING`/`SERVER_AT_CAPACITY` → 503, `RATE_LIMITED` → 429, `ORIGIN_NOT_ALLOWED` → 403); `TestRealtimeTuningDefaults` (parse env minimal via `testutil.MinimalEnv`: `WSBinaryRate==100`, `WSBinaryBurst==200`, `TurnFillerSuffixes` 14 item, `TurnCompletionSuffixes` 12 item, `PronunciationCaptureEnabled==true`, `MaxConsecutiveFailedTurns==3`, `AudioMaxLeadMS==400`, `RollingSummaryTriggerTurns==4` (`ROLLING_SUMMARY_TRIGGER_TURNS`), `RollingSummaryTimeout==10s`, `ReconnectWindow==30s`; refleksi memastikan tidak ada nama field ganda dengan `Realtime` M1); `TestTurnRepoConstructors` (integration, `-tags integration`: `NewTurnRepo(pool).Insert` fenced gagal `session.ErrFenceLost` untuk fence salah).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/... ./internal/ -race -run 'M5|WSCodes|RealtimeTuning'`. Expected: FAIL (undefined `RealtimeTuning`).
- [ ] **Step 3: Implement** — hanya `RealtimeTuning` dan konstruktor repo; `Validate()` menolak `TurnSilenceCompleteMS >= TurnSilenceDefaultMS` dan nilai ≤ 0; env baru ke `.env.example`. Jika `TestWSCodesExist` menemukan kode hilang → itu bug M1: hentikan, catat di `docs/implementation-log.md`, jangan tambah diam-diam. Jika `session.TurnStart/TurnUser/TurnFinal` belum punya field yang dipakai §7.6.6 (mis. `TurnFinal.SpokenText`, `TTSVoiceID`, `ErrorCode`; tanpa `Expressions`, D-014), tambahkan di `session` (catat di log).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go build ./...`; `go test -tags integration ./internal/postgres/... -race -run TurnRepoConstructors` PASS.
- [ ] **Step 5: Commit** — `git add internal .env.example && git commit -m "chore(realtime): preflight contracts, RealtimeTuning config"`

### Task 2: Port minimal learner/memory, `rtstate`, fake port, helper leak

**Files:**
- Create: `internal/learner/difficulty.go`, `internal/memory/port.go`, `internal/conversation/rtstate/rtstate.go`, `internal/conversation/ports.go`, `internal/testutil/fakes/{session_runtime,turn_repo,event_writer,runtime_store,memory_retrieval,learner_context,audio_store}.go`, `internal/testutil/repotest/runtime_store.go`, `internal/testutil/leak.go`
- Test: `internal/learner/difficulty_test.go`, `internal/testutil/fakes/runtime_store_test.go`, `internal/testutil/fakes/session_runtime_test.go`

**Interfaces:**
- Produces `learner`: `AdaptiveDifficulty` (§12.8.2, tag json persis), `Formality`, `DialectExposure`, `Skill` (string), `TTSSpeakingRate(speed int) float64` (1→0.80, 2→0.88, 3→0.95, 4→1.00, 5→1.05), `MaxAISentences(speed int) int` (1→2, 2→2, 3→3, 4→3, 5→4).
- Produces `memory` (§11.7.1 persis): `Category`+4 konstanta, `ContextItem`, `MemoryContext`, `SessionContextInput`, `TurnText`, `RetrievalService`.
- Produces `rtstate`: `RT`, `TurnSnap`, `EventEntry{Seq, PrevSeq int64; Type string; Env []byte}`, `Summary`, `Takeover{SessionID uuid.UUID; Fence int64}`, `PodInfo`, dan `type Store interface{ AcquireOwner(ctx, sid uuid.UUID, pod string, fence int64, ttl time.Duration) (bool, error); ReleaseOwner(ctx, sid, pod string, fence int64) error; WriteHeartbeat(ctx, sid, at time.Time, ttl time.Duration) error; ReadHeartbeat(ctx, sid) (time.Time, bool, error); WriteRT(ctx, sid, RT, ttl) error; ReadRT(ctx, sid) (RT, bool, error); WriteTurn(ctx, sid, TurnSnap, ttl) error; ReadTurn(ctx, sid) (TurnSnap, bool, error); DeleteTurn(ctx, sid) error; AppendEvent(ctx, sid, EventEntry) error; ReadEvents(ctx, sid) ([]EventEntry, error); WriteSummary(ctx, sid, Summary, ttl) error; ReadSummary(ctx, sid) (Summary, bool, error); ReadMemCtx(ctx, sid) ([]byte, bool, error); PublishTakeover(ctx, pod string, Takeover) error; SubscribePod(ctx, pod string) (<-chan Takeover, error); WritePodInfo(ctx, pod string, PodInfo, ttl) error }` (field `RT` = persis kolom hash §7.9.1).
- Produces `conversation` (ports.go): `SessionRuntime` (10 method §7.13.1), `TurnRepository`, `SessionEventWriter{Append(ctx, fence int64, sid uuid.UUID, eventType string, turnID *uuid.UUID, payload any) error}` (param `turnID` tambahan — nama dari plan, tabel §7.13.1 butuh `turn_id`), `RuntimeStore = rtstate.Store`, `ConfigLoader`, `ConfigSnapshot{content.SessionConfig; Opening *content.PromptVersion}`, `LearnerContextProvider{PromptContext(ctx, userID uuid.UUID, ad learner.AdaptiveDifficulty) (prompt.LearnerContext, error); Rerender(prev prompt.LearnerContext, ad learner.AdaptiveDifficulty) prompt.LearnerContext}` (`Rerender` = tambahan dari plan agar runtime tak impor implementasi M8; M8 mengisinya dengan `learner.NewPromptContext`), `AudioStore{PutTempWAV(ctx, key string, pcm16k []int16) error}`, alias `TurnStart = session.TurnStart` dst.
- Produces `testutil.NoGoroutineLeak(t, pkgPrefix string)` (snapshot `runtime.Stack(all)` sebelum/sesudah; gagal bila goroutine ber-prefix paket tersisa setelah `require.Eventually` 2 s).

- [ ] **Step 1: Write the failing test** — `TestSpeedTables` (nilai tabel di atas); `repotest.RuntimeStoreSuite(t, mk func(t) rtstate.Store)` dijalankan ke `fakes.NewRuntimeStore` (Task 16 ke Redis): owner Lua semantik §7.9.2 (`AcquireOwner(fence=3)` ok; `fence=2` setelahnya → `false`; `ReleaseOwner` hanya jika pemilik cocok), `AppendEvent` 600× → `len(ReadEvents) >= 500`, `ReadEvents` urut `Seq`, `PublishTakeover` diterima `SubscribePod`; `TestFakeSessionRuntime` (state machine memakai `session.CanTransition`; `ClaimRuntime` menaikkan fence; panggilan ber-fence lama → `session.ErrFenceLost`; `Complete` idempotent; `AddDisconnected` terakumulasi; `Settlement.ChargedSeconds = min(duration, ceil((endingAt-startedAt)/1s) - disconnected)` memakai fake clock).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/learner/... ./internal/testutil/... -race`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** — fake aman konkuren (`sync.Mutex`), hook `FailNext(method, err)` (§14.2); `fakes.SessionRuntime` meng-embed state sesi in-memory + `Seed(rec session.RuntimeRecord)`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): ports, rtstate, learner/memory ports, fakes"`

### Task 3: Protokol WS — envelope, payload, registry, golden

**Files:**
- Create: `internal/conversation/protocol/{envelope.go,client.go,server.go,codec.go,registry.go}`, `testdata/api/ws/{client,server}/<event_type>.json` (28 file)
- Test: `internal/conversation/protocol/golden_test.go`, `codec_test.go`

**Interfaces:**
- Produces: `ServerEvent{Type string; Version int; EventID uuid.UUID; ServerSeq int64; SessionID uuid.UUID; TurnID *uuid.UUID; TurnSeq *int; Generation *uint32; TS time.Time; Replay bool; Payload any}`, `ClientEvent{Type string; Version int; EventID uuid.UUID; SessionID uuid.UUID; TS time.Time; Payload any}`; payload struct per tipe §7.4.4/§7.4.5 (`SessionStart{Client *ClientInfo; Audio AudioParams}`, `SessionResume{LastServerSeq int64; Audio AudioParams}`, `SessionEnd{Reason}`, `ClientBargeIn{Generation uint32; VadMS *int}`, `ClientPlaybackFinished{Generation uint32; PlayedMS int}`, `Ping{ClientTS int64}`; server: `SessionReady, SessionStarted, SessionSnapshot, SessionEnding, SessionCompleted, SessionFailed, SpeechStarted, SpeechEnded, TurnStarted, STTPartial, STTFinal, AIGenerationStarted, AITextDelta{Text string}, AISentence{SentenceIndex int; Text string; FirstFrameSeq uint32; StartMS int}` (tanpa `expression`, D-014), `AIAudioStarted, AIAudioCompleted, AIInterrupted, TurnCompleted, BalanceTick, Pong, ErrorPayload{Error ErrorBody; Retryable, Fatal bool; RefEventID *string}`); `const Version = 1`; `func IsDurable(typ string) bool` (daftar §7.4.3); `func MarshalServer(ServerEvent) ([]byte, error)` (field `turn_id`/`turn_seq`/`generation` dihilangkan bila nil, `replay` hanya bila true); `func DecodeServerEvent([]byte) (ServerEvent, error)`; `func DecodeClientEvent([]byte) (ClientEvent, error)` (`ErrUnknownType`, `ErrUnsupportedVersion`, `ErrInvalidPayload`); `Registered() (client, server []string)`.

- [ ] **Step 1: Write the failing test** — `TestGoldenServer`/`TestGoldenClient` (§14.4: marshal nilai tetap `JSONEq` golden; unmarshal→re-marshal sama; **kelengkapan**: setiap tipe terdaftar punya golden dan sebaliknya; flag `-update`); `TestDurableSet` (durable: `session.started, session.ending, session.completed, session.failed, turn.started, stt.final, ai.generation_started, ai.sentence, ai.audio_started, ai.audio_completed, ai.interrupted, turn.completed`; transient: `session.ready, session.snapshot, speech.started, speech.ended, stt.partial, ai.text_delta, balance.tick, pong`); `TestEnvelopeOmitsEmpty` (`turn_id` absen bila nil; `replay` absen bila false); `TestDecodeClient` (type tak dikenal → `ErrUnknownType`; `version:2` → `ErrUnsupportedVersion`; **field payload tak dikenal diabaikan** (§7.4.2 menang atas §14.4, dicatat di deviations); `session.start` dengan `audio.sample_rate:16000` → decode sukses tetapi `AudioParams.Valid()==false`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/protocol/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — payload persis contoh §7.4.5 (field wajib selalu ada; `latency` pointer opsional); registry = `map[string]reflect.Type`; golden ditulis dari contoh spec (UUID/waktu tetap, indent 2).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git add internal/conversation/protocol testdata/api/ws && git commit -m "feat(protocol): WS v1 envelope, payloads, golden"`

### Task 4: Protokol — frame biner, katalog error, close code

**Files:**
- Create: `internal/conversation/protocol/{binary.go,errors.go}`, `testdata/api/ws/frames/{upstream_audio,downstream_audio}.hex`
- Test: `internal/conversation/protocol/binary_test.go`, `errors_test.go`

**Interfaces:**
- Produces: `EncodeUpstream(seq uint32, opus []byte) []byte`, `DecodeUpstream(b []byte) (seq uint32, opus []byte, err error)`, `EncodeDownstream(gen, seq uint32, opus []byte) []byte`, `DecodeDownstream(b []byte) (gen, seq uint32, opus []byte, err error)`, `MaxBinary=4096`, `ErrFrameTooShort/ErrFrameTooLarge/ErrBadType`; `type Spec struct{Fatal bool; Close int; Retryable bool}`, `var Catalog map[errs.Code]Spec` (tabel §7.4.8), `func NewError(code errs.Code, requestID uuid.UUID, ref *uuid.UUID, details any) ErrorPayload` (pesan dari `errs.Messages`, `request_id` = ref bila ada), `func CloseCode(code errs.Code) int`.

- [ ] **Step 1: Write the failing test** — golden hex §7.4.7: seq 258 + paket `78 9A BC` → `01 00 00 01 02 78 9A BC`; gen 7 seq 0 → `02 00 00 00 07 00 00 00 00 …`; `TestUpstreamValidation` (5 byte → `ErrFrameTooShort`, byte0 `0x02` → `ErrBadType`, 4097 B → `ErrFrameTooLarge`, 4096 B payload 4091 sah); `TestCatalog` (tabel `{code, fatal, close, retryable}` semua 24 baris §7.4.8, mis. `SESSION_STATE_CONFLICT` → `{true,4009,*}`, `SERVER_AT_CAPACITY` → `{true,4503,true}`, `INVALID_MESSAGE` → `{false,0,false}`; `UNSUPPORTED_AUDIO_FORMAT` → 1008); `TestNewErrorFormat` (empat field D-013 selalu ada, `details:null`, `retryable/fatal` dari katalog).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/protocol/... -race -run 'Binary|Frame|Catalog|NewError|Upstream'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `encoding/binary.BigEndian`; `SESSION_STATE_CONFLICT.Retryable` ditentukan pemanggil (true hanya status `STARTING`; `NewError` menerima override lewat `WithRetryable(bool)` — nama dari plan).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/conversation/protocol/... -race` PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(protocol): binary frames and error catalog"`

### Task 5: Audio — Opus, framer, PCM, WAV (cgo)

**Files:**
- Create: `internal/conversation/audio/{opus.go,framer.go,pcm.go,wav.go}`
- Test: `internal/conversation/audio/opus_test.go`, `framer_test.go`, `wav_test.go`

**Interfaces:**
- Produces: `NewDecoder() (*Decoder, error)` (16 kHz mono), `(*Decoder).Decode(packet []byte) ([]int16, error)` (valid hanya bila 320 sampel, selain itu `ErrBadFrame`), `DecodePLC() ([]int16, error)`; `NewEncoder(sampleRate, bitrate, complexity int) (*Encoder, error)` (`AppVoIP`, DTX off, FEC off), `(*Encoder).Encode(block []int16) ([]byte, error)` (panjang blok = `sampleRate/50`); `NewFramer(sampleRate int) *Framer`, `Write(pcm []byte) [][]int16` (menahan byte ganjil), `Flush() []int16` (pad nol, nil bila kosong); `Int16ToBytes(s []int16, dst []byte) []byte`, `BytesToInt16`, `Decimate48to16(s []int16) []int16` (rata-rata 3 sampel), `Resample(s []int16, from, to int) []int16` (linear); `type PCM struct{Rate int; Samples []int16}`, `ReadWAV(path string) (PCM, error)` (PCM16 mono), `WriteWAV(w io.Writer, rate int, s []int16) error`; `type OutFrame struct{Gen, Seq uint32; SentenceIdx int; LastOfSentence bool; Opus []byte}`.

- [ ] **Step 1: Write the failing test** — `TestEncodeDecodeRoundTrip` (sinus 440 Hz 24 kHz, 50 blok 480 → paket 1..4087 B → decoder 16 kHz memberi 320 sampel, RMS hasil ≥ 0.3× RMS masukan setelah 5 frame pemanasan); `TestDecodeInvalid` (paket sampah 3 byte → `ErrBadFrame`; paket 40 ms → `ErrBadFrame`); `TestPLC` (`DecodePLC` 320 sampel); `TestFramerCarriesOddByte` (chunk 959 + 961 byte → 2 blok 480 + sisa 0; chunk ganjil tidak membelah sampel); `TestFramerFlushPads` (100 sampel → 480 dengan 380 nol); `TestWAVRoundTrip` (header 44 B, rate/ch benar, WAV stereo → error); `TestDecimate` (rata-rata); `TestEncoderBlockSizeError`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/audio/... -race`. Expected: FAIL (build: undefined). Prasyarat: `pkg-config --modversion opus opusfile`.
- [ ] **Step 3: Implement** — `gopkg.in/hraban/opus.v2`; WAV parser minimal (chunk `fmt `/`data`, tolak non-PCM16).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make arch-check` tetap hijau.
- [ ] **Step 5: Commit** — `git add internal/conversation/audio go.mod go.sum && git commit -m "feat(audio): opus codec, framer, wav"`

### Task 6: Audio — VAD, capture, aset APOLOGY

**Files:**
- Create: `internal/conversation/audio/{vad.go,capture.go,apology.go}`, `internal/conversation/audio/assets/apology_ja.opus`, `test/tools/genapology/main.go`
- Modify: `Makefile` (target `assets-apology`)
- Test: `internal/conversation/audio/vad_test.go`, `capture_test.go`, `apology_test.go`

**Interfaces:**
- Produces: `NewVAD(mode int) (*VAD, error)`, `(*VAD).Process(pcm []int16) bool` (320 sampel → 640 byte LE, `err != nil` → false; tidak thread-safe); `NewCapture(maxMS int) *Capture` dengan `Start(onsetMS int64)`, `Append(pcm []int16)`, `Take() []int16` (reset), `Active() bool`, batas 45 s (720000 sampel); `const ApologyText = "すみません、うまく聞き取れませんでした。もう一度お願いします。"`, `ApologyFrames() ([][]byte, error)` (parse embed `[uint16 BE len][packet]`).

- [ ] **Step 1: Write the failing test** — `TestVADSilenceAndTone` (320 nol → false ×50; derau acak amplitudo 200 seed tetap → false ≥ 95%); `TestVADRejectsBadMode`; `TestCaptureCap` (append 50 s → `len(Take()) == 720000`); `TestApologyFrames` (N ≥ 40 paket, tiap paket 1..4087 B, durasi ≥ 1 s, dekode berhasil 320 sampel).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/audio/... -race -run 'VAD|Capture|Apology'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `genapology`: sintesis nada naik-turun 2,4 s (sinus 220–330 Hz per suku kata, 24 kHz) → `Framer` → `Encoder(24000,32000,5)` → tulis format len-prefixed; `make assets-apology` menjalankan `go run ./test/tools/genapology -o internal/conversation/audio/assets/apology_ja.opus` (M9 menggantinya dengan TTS asli lewat flag `-tts`; di M5 hanya mode fake); jalankan dan commit file hasil.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git add internal/conversation/audio test/tools/genapology Makefile && git commit -m "feat(audio): VAD wrapper, capture, apology asset"`

### Task 7: Fixture audio Jepang (sintetis) + validator

**Files:**
- Create: `test/tools/genaudio/main.go`, `testdata/audio/synth/*.wav|*.txt|*.partials.json` (10 fixture §14.5; rekaman asli dari M9 tetap di `testdata/audio/`), `testdata/audio/synth/manifest.json`, `internal/testutil/audiofx/audiofx.go`
- Modify: `Makefile` (`fixtures`, `fixtures16k`), `.gitignore` (`out/`), `.env.example` (`FAKE_STT_FIXTURE_DIR=out/fixtures16k`), `.golangci.yml` (aturan 5 §14.9.5: tambah pengecualian `!**/internal/testutil/audiofx/**` agar paket ini boleh mengimpor `conversation/audio`; `internal/testutil` root **tidak** boleh mengimpor cgo)
- Test: `internal/testutil/audiofx/audiofx_test.go` (`TestAudioFixtures`), `test/tools/genaudio/main_test.go`

**Interfaces:**
- Produces: `genaudio` deterministik (seed tetap): sumber harmonik F0 120 Hz + 3 formant, modulasi amplitudo 4 Hz per suku kata (voiced), jeda = nol digital, `noise_only` = derau merah −45 dBFS; flag `-to16k <dir>` menulis salinan 16 kHz (decimate) + `.txt` ke `out/fixtures16k` (dipakai `FAKE_STT_FIXTURE_DIR`, M4 mewajibkan 16 kHz); `audiofx.LoadFixture(t, name) (audio.PCM, transcript string)`, `audiofx.FixtureManifest(t)` (membaca `testdata/audio/synth/` lewat `testutil.TestdataPath`; setelah M9, `LoadFixture` mencari `testdata/audio/` lebih dulu, lalu `synth/`); format `.partials.json`: `[{"at_ms":400,"text":"えっと"},{"at_ms":2400,"text":"…","final":true}]` (`final` opsional — nama dari plan).

- [ ] **Step 1: Write the failing test** — `TestAudioFixtures` (§14.5): tiap `manifest.json` entry punya WAV PCM16 mono 48 kHz, durasi = `duration_ms ±20`, `.txt` satu baris; durasi: `ja_greeting_short` 1,5 s, `ja_self_intro` 4 s, `ja_filler_pause` memuat jeda 1,0 s, `ja_conjunction_pause` jeda 1,1 s, `ja_question` 2 s, `ja_long_utterance` 20 s, `ja_max_utterance` 50 s, `ja_barge_in` 3 s, `noise_only` 5 s, `silence_5s` 5 s; **`TestFixturesVADSanity`**: dengan `audio.VAD` mode 2 pada hasil `Decimate48to16`, fixture ucapan memiliki ≥ 60% frame voiced pada segmen bicara, `noise_only` dan `silence_5s` ≤ 5% voiced; `ja_filler_pause` punya tepat 2 run voiced dipisah ≥ 40 frame hening; total ukuran `testdata/audio/synth` ≤ 10 MB (spec < 8 MB tidak mungkin: 98,5 s × 96 KB/s ≈ 9,5 MB → catat di deviations; 4 fixture rekaman penutur asli = tugas pemilik produk, sintetis dipakai sementara).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/testutil/audiofx/ -race -run AudioFixtures`. Expected: FAIL (fixture belum ada).
- [ ] **Step 3: Implement** — jalankan `make fixtures`; bila `TestFixturesVADSanity` gagal, ubah parameter formant/energi `genaudio` (bukan ambang test) sampai lulus; commit hasil.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/testutil/audiofx/ ./test/tools/genaudio/ -race` PASS; `make fixtures16k && ls out/fixtures16k | wc -l` ≥ 20 (wav+txt).
- [ ] **Step 5: Commit** — `git add test/tools/genaudio testdata/audio internal/testutil .golangci.yml Makefile .gitignore .env.example && git commit -m "test(audio): synthetic Japanese fixtures and validator"`

### Task 8: Detector turn + aturan teks Jepang (murni)

**Files:**
- Create: `internal/conversation/{detector.go,japanese.go}`
- Test: `internal/conversation/detector_test.go`, `japanese_test.go`

**Interfaces:**
- Produces (§7.6.2 persis): `TurnState` (`StateListening`, `StateUserSpeaking`, `StateProcessing`, `StateAISpeaking`), `EventKind` (`SPEECH_CONFIRMED, SPEECH_PAUSE, SPEECH_RESUME, SILENCE_REACHED, MAX_UTTERANCE, BARGE_IN_CONFIRMED`), `Event{Kind; OnsetMs, ThresholdMs int64}`, `TextView interface{TurnText() string; TextSince(onsetMs int64) string; FinalCoversLatest() bool; TurnOnsetMs() int64}`, `DetectorConfig` + `DetectorConfigFrom(config.RealtimeTuning)`, `NewDetector(cfg) *Detector`, `Push(voiced bool, st TurnState, tx TextView) []Event`, `Threshold(tx) int64`, `InjectSilence(ms int64, st, tx) []Event`, `ResetForNewTurn()`, `NowMs()`; `NormalizeTail`, `HasSuffixAny`, `IsFillerOnly`, `RuneLen`, `isDiscard(text string, voicedMs int64) bool` (memakai `prompt.NormalizeSpeakable`), `HasJapanese(s string) bool` (ada rune Hiragana/Katakana/Han).

- [ ] **Step 1: Write the failing test** — `TestSpeechStart` (9 frame voiced berturut → tidak ada event; 10 dari 15 → `SPEECH_CONFIRMED`, `OnsetMs` = waktu frame voiced pertama jendela); `TestSilenceThresholds` (tabel: teks `昨日会社に行きました` + `FinalCoversLatest=false` → 800; sama + true → 500; `えっと` → 2000 (filler-only); `昨日は雨だったけど、` → 1400; `ですが` → 1400; `ですか` + true → 500; `` → 800; setelah run voiced berakhir di frame k, event `SILENCE_REACHED` muncul **tepat** di frame `k+2+N` dengan `N = ambang/20` (cluster 3/5 bertahan 2 frame setelah voiced terakhir) dan `ThresholdMs` = ambang); `TestMaxUtterance` (voiced terus-menerus: event `MAX_UTTERANCE` pada `nowMs - onset >= 45000`); `TestPauseResume` (`SPEECH_PAUSE` tepat sekali di silence ≥ 300 ms; voiced lagi → `SPEECH_RESUME`); `TestBargeInRules` (state `AI_SPEAKING`: 15/20 voiced + `TextSince="あの"` → `BARGE_IN_CONFIRMED`; `TextSince="あ"` → tidak; 14/20 → tidak; `TextSince=""` selama 40 frame voiced (gema sebelum commit) → tidak; silence ≥ 300 ms membatalkan kandidat); `TestInjectSilence`; tabel `japanese_test.go`: `NormalizeTail("えっとー")=="えっと"`, `("あのー…")=="あの"`, `IsFillerOnly("えっと、あの")`, `isDiscard("",1000)`, `isDiscard("ん",300)==true`, `isDiscard("ん",400)==false`, `isDiscard("あ。",300)==true`, `HasJapanese("OK")==false`, `HasJapanese("OKです")==true`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Detector|Japanese|Speech|Silence|MaxUtterance|BargeInRules|Discard'`. Expected: FAIL.
- [ ] **Step 3: Implement** — salin algoritma §7.6.2 apa adanya (`BoolRing` ukuran 15/20/5 dengan `Count`, `SpanFromFirstTrue`); detector tidak punya goroutine/clock (waktu = frame × 20).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS (`-race`).
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): turn detector and Japanese text rules"`

### Task 9: `sttState`, `ScriptedSTT`, component test detector dengan VAD asli

**Files:**
- Create: `internal/conversation/stt_state.go`, `internal/testutil/replay/{timeline.go,scripted_stt.go}`
- Test: `internal/conversation/stt_state_test.go`, `internal/conversation/detector_component_test.go`, `internal/testutil/replay/scripted_stt_test.go`

**Interfaces:**
- Produces `sttState` (implements `TextView`): `Segment{Text string; Final bool; RecvAudioMs int64}`; `OnPartial/OnFinal(ev provider.STTEvent)`, `Commit(purpose CommitPurpose)` (`CommitTurnEnd`/`CommitDiscard`, FIFO `pendingCommits`), `MarkTurn()` (set `sttMark`), `TurnText()`, `TextSince(onset)`, `FinalCoversLatest()`, `CommitIdx()`.
- Produces `replay.LoadTimeline(path)`, `replay.ScriptedSTT` (memenuhi `provider.RoutedSTTSession`): `NewScriptedSTT(tl Timeline, opts...)`; event dipicu **waktu audio** kumulatif dari `SendAudio` (tiap 640 B = 20 ms), `PARTIAL` saat `at_ms` tercapai, `FINAL` bila `final`; `Finalize()` selalu menghasilkan tepat satu `FINAL{FromFinalize:true}` (teks = partial terakhir atau `SetFinalizeText`); `Barrier()` menaruh sentinel yang diteruskan lewat `Events()` (untuk `rtharness.Sync`), `FinalizeCalls() int`, `HoldFinalize(bool)`.

- [ ] **Step 1: Write the failing test** — `TestTurnTextJoinsFinalPlusPartial`; `TestCommitFIFO` (urutan `TURN_END, DISCARD`: FINAL pertama masuk turn, FINAL kedua dibuang + `commitIdx` maju); `TestTextSinceEmptyWhilePendingDiscard` (S-8: `TextSince` = "" selama ada entri `DISCARD`; setelah FINAL commit datang, hanya partial baru yang dihitung); `TestFinalCoversLatest`; `TestScriptedSTTDeterministic` (20 ms per `SendAudio`, partial `えっと` tepat saat total audio ≥ 400 ms, `Finalize` → 1 FINAL). **Component** `TestDetectorFixtures` (fixture via `audiofx.LoadFixture`; VAD asli pada `Decimate48to16(fixture)`, STT dari `.partials.json`, state machine mini: LISTENING→USER_SPEAKING→…): `ja_filler_pause` → tepat satu `SPEECH_CONFIRMED` dan satu `SILENCE_REACHED` (tidak ada di jeda 1,0 s karena teks `えっと` → ambang 2000); `ja_conjunction_pause` → satu turn (ambang 1400 > jeda 1,1 s); `ja_question` + final diterima → `ThresholdMs==500`; `ja_greeting_short` → 800; `ja_max_utterance` → `MAX_UTTERANCE` di 45000 ms sejak onset (±40 ms) dan setelahnya tidak ada `SILENCE_REACHED` ganda; **`noise_only` dan `silence_5s` → nol event** (Review Focus 1); speech < 200 ms (potongan 150 ms `ja_greeting_short`) → nol event.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ ./internal/testutil/replay/ -race -run 'sttState|TurnText|Commit|TextSince|Switched|Scripted|DetectorFixtures'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `stt_state.go` mengikuti blok pseudo §7.6.2 (`pendingCommits`, `commitIdx`, `sttMark`); waktu fake clock tidak dipakai (waktu = audio).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): sttState, scripted STT, detector component tests"`

### Task 10: Conn, Outbound, reader, writer, heartbeat

**Files:**
- Create: `internal/conversation/{conn.go,outbound.go,reader.go,writer.go,heartbeat.go,ratebucket.go}`
- Test: `internal/conversation/outbound_test.go`, `reader_test.go`, `writer_test.go`

**Interfaces:**
- Produces `Conn`/`MessageKind` (§7.13.1); `Outbound` (thread-safe): `NewOutbound(cap int, m *Metrics)`, `EnqueueControl(env []byte, durable bool) error` (penuh dan durable → `ErrSlowConsumer`; `len(control) > 128` → transient menggantikan transient bertipe sama), `EnqueueAudio(f audio.OutFrame)`, `PurgeAudio(activeGen uint32)`, `AudioLen() int`, `Next(ctx) (item, ok)` (semua control dulu, lalu satu frame audio), `Drained() bool`; `Writer.Run(ctx, conn Conn, out *Outbound, activeGen *atomic.Uint32, acks chan<- WriterAck)` (cek `f.Gen == activeGen` sebelum tulis; setelah frame `LastOfSentence` naikkan `sentencesWritten`; ack `AUDIO_WRITTEN_ALL`); `Reader.Run(ctx, conn, cmdCh chan<- protocol.ClientEvent, audioIn chan<- AudioIn, ...)` (limit §7.4.10, validasi frame, dedupe `event_id` LRU 64, `PROTOCOL_VIOLATION` karena rate/size/`session_id`/`version`, `INVALID_MESSAGE` window); `bucket` token bucket berbasis `clock.Clock`.

- [ ] **Step 1: Write the failing test** — `TestOutboundPriority` (control selalu keluar sebelum audio); `TestPurgeAudioDropsStale` (enqueue gen 1 ×10, `PurgeAudio(2)` → `AudioLen()==0`); `TestWriterDropsStaleGen` (activeGen diubah ke 2 sebelum frame gen 1 ditulis → tidak ada tulis, metrik `audio_frames_out_total{dropped_stale}`; Review Focus 2); `TestSlowConsumer` (256 durable penuh → `ErrSlowConsumer`); `TestTransientCoalescing` (balance.tick berulang saat control > 128 menggantikan yang lama); `TestReaderLimits` (tabel: frame biner 4097 B, header `0x02`, `version:2`, `session_id` salah, burst 201 frame biner dalam 0 s dengan fake clock → `PROTOCOL_VIOLATION` + kind metrik `frame_size|header|version|session_id|rate`; 40 JSON burst ok, ke-41 → violation); `TestInvalidMessageWindow` (11 `INVALID_MESSAGE` dalam 60 s → violation; 10 → tidak); `TestDuplicateEventIDIgnored`; `TestWriterTimeout` (Conn.Write blok 5 s fake → error → disconnect).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Outbound|Writer|Reader|SlowConsumer|Transient|Invalid|Duplicate'`. Expected: FAIL.
- [ ] **Step 3: Implement** — writer satu-satunya penulis; `Conn` in-memory pipa untuk test (`testutil/rtharness` memakai ulang, Task 18).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): outbound queue, reader, writer, limits"`

### Task 11: Pacer downstream

**Files:**
- Create: `internal/conversation/pacer.go`
- Test: `internal/conversation/pacer_test.go`

**Interfaces:**
- Produces: `type Pacer struct`; `NewPacer(clk clock.Clock, sleep func(ctx, d time.Duration) error, maxLeadMS int64) *Pacer`; `(*Pacer) Run(ctx, frames <-chan audio.OutFrame, out *Outbound, hooks PacerHooks) error` dengan `PacerHooks{BeforeFirst func(); OnFirst func(); OnSentenceStart func(f audio.OutFrame); OnDone func(frameCount int, totalMS int64)}`; `EstimatedPlaybackEnd() time.Time`.

- [ ] **Step 1: Write the failing test** — fake clock + `sleep` yang memajukan clock: `TestPacerBurstThenRealtime` (50 frame sekaligus: 20 frame pertama (400 ms) di-enqueue tanpa menunggu; frame ke-21 menunggu sampai lead < 400 → selisih waktu enqueue frame n ≥ 20 adalah 20 ms); `TestPacerUnderrunResetsClock` (jeda TTS 1 s → tidak burst setelahnya); `TestPacerBackpressure` (`out.AudioLen()==25` → menunggu); `TestPacerHookOrder` (`BeforeFirst` (→ `MarkAudioSent`) dipanggil **sebelum** `EnqueueAudio` frame pertama, `OnFirst` sesudahnya, `OnSentenceStart` pada frame pertama tiap kalimat); `TestPacerCancel` (ctx dibatalkan → keluar tanpa enqueue lagi); `TestPlaybackEnd` (`start + sentMs`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run Pacer`. Expected: FAIL.
- [ ] **Step 3: Implement** — algoritma §7.5.4 persis (blok kode spec); tanpa `time.Now()` (pakai `clk`).
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): lead-bounded audio pacer"`

### Task 12: Generation pipeline (LLM → parser → TTS → Opus → pacer)

**Files:**
- Create: `internal/conversation/generation.go`, `internal/testutil/fakes/scripted_router.go`
- Test: `internal/conversation/generation_test.go`

**Interfaces:**
- Consumes `provider.Generation`, `prompt.NewSentenceParser(max)`, `audio.Encoder/Framer`, `Pacer`.
- Produces `GenKind` (`GenOpening, GenReply, GenApology`); `Generation` (`Number uint32; Kind; TurnID uuid.UUID; ctx/cancel; llmCancel`); `GenEvent` (varian: `EvTextDelta{Clean string}`, `EvSentence{Sentence prompt.Sentence}`, `EvFirstAudio`, `EvSentenceAudio{Index int}`, `EvAudioDone{LLMFinished bool}`, `EvGenFailed{Cause GenCause}`, semua membawa `Gen uint32`; `GenCause` ∈ `CauseProvider, CauseContentFiltered`); `func (r *Runtime) startGeneration(...)` dirinci di Task 19 — di sini `newGeneration(p genParams) *Generation` dengan `runLLM`, `runStatic(openingLine)` (S-2), `runTTS`, `playAsset`; `voiceSelection(c content.CharacterSnapshot) provider.VoiceSelection` (D-015: `Primary = VoiceRef{Provider: provider.Name(c.ProviderName), ModelName: c.ModelName, VoiceID: c.VoiceID}`; `Fallback = &VoiceRef{…}` dari `c.Voice.Fallback` bila ada, selain itu nil → tanpa fallback TTS; dihitung sekali per sesi, dipakai ulang tiap generation) dan `genParams{Voice provider.VoiceSelection; …}` → `router.NewGeneration(sid, gen, p.Voice)`; `fakes.ScriptedRouter` (`provider.RealtimeRouter`) + `Script{Deltas []string; Finish provider.FinishReason; LLMErrAfter int; PCMPerSentenceMS int; Gate <-chan struct{}; TTSEpochAfterSentence int}`.

- [ ] **Step 1: Write the failing test** — `TestPipeliningOrder` (3 kalimat dari delta terbelah; `Synthesize` dipanggil berurutan 0,1,2 dan kalimat 1 mulai sebelum LLM selesai bila `Gate` menahan delta terakhir); `TestSentenceLimit` (6 kalimat, `MaxSentences=2` → tepat 2 `EvSentence`, `llmCancel` terpanggil, `EvAudioDone{LLMFinished:true}`, metrik `ai_sentence_limit_total{reply}`=1; R-17); `TestTextDeltaSanitized` (`**はい**` terbelah antar delta → `Clean` = `はい`; tidak ada tag/markup di `Clean` maupun di teks TTS, D-014); `TestSpeakingRateFormula` (`voice 1.0 × adaptive 1.05 → 1.05`; `1.2 × 1.05 → 1.05` (clamp); `0.9 × 0.80 → 0.80` (clamp bawah); `adaptive 0 → 1.0`; dihitung sekali per generation); `TestVoiceSelectionFromSnapshot` (snapshot `provider_name:"CARTESIA"`, `model_name:"sonic3.6"`, `voice_id:"db6b0ed5-…"`, `voice.fallback={ELEVENLABS,"Eleven v3 Conversational","21m00Tcm4TlvDq8ikWAM"}` → `Primary`/`Fallback` persis (nilai teks apa adanya, tanpa normalisasi); tanpa `voice.fallback` → `Fallback==nil`; `NewGeneration` menerima `Voice` tersebut; `TTSRequest.Voice` dari snapshot, `VoiceID`/`Model` diisi router, bukan runtime); `TestNoTTSFallbackFailsToApology` (`Fallback==nil`, `ScriptedRouter` mengembalikan `TTSChunk{Err}` sebelum audio → `EvGenFailed{CauseProvider}` → generation FAILED → APOLOGY (D-042); tidak ada epoch/switch vendor); `TestLLMErrorFailsGeneration` (router `ERROR` LLM di tengah delta → `EvGenFailed{CauseProvider}`, tanpa event reset/pindah vendor, D-017); `TestTTSEpochRestart` (epoch naik setelah kalimat 1 → frame generasi lama dibuang, semua kalimat disintesis ulang dari 0, chunk `Epoch` lama diabaikan); `TestTruncated` (`DONE{TRUNCATED}` → parser tidak di-`Close()`, kalimat tersisa diputar, `EvAudioDone{LLMFinished:false}`); `TestContentFiltered` (`DONE{CONTENT_FILTERED}` dan `ERROR` kind `CONTENT_FILTERED` → `EvGenFailed{CauseContentFiltered}`); `TestStaticOpening` (tanpa panggilan `StreamLLM`, `ai.sentence` tanpa field `expression`); `TestApologyAsset` (frame dari `ApologyFrames`, `Gen` benar, `LastOfSentence` pada frame terakhir); `TestMarkAudioSentBeforeFirstFrame`; `TestCancelNoLeak` (`NoGoroutineLeak`, goroutine keluar ≤ 1 s).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Pipelining|SentenceLimit|TextDeltaSanitized|SpeakingRateFormula|VoiceSelection|LLMErrorFails|TTSEpoch|Truncated|ContentFiltered|StaticOpening|ApologyAsset|MarkAudioSent|CancelNoLeak'`. Expected: FAIL.
- [ ] **Step 3: Implement** — sketsa §7.7.4: tiga goroutine per generation, `errgroup`-free (pemilik jelas: `gen.ctx`); kirim event ke `genEvents` dengan `select` pada `ctx.Done()`; hanya satu `Synthesize` aktif; `speakingRate = content.SpeakingRate(snapshot.Character.Voice.SpeakingRate, adaptive.TTSSpeakingRate)` (= `clamp(voice.speaking_rate × adaptiveRate, 0.80, 1.05)`) dihitung sekali per generation; `TTSRequest` hanya membawa teks, `Voice` (`VoiceSelection` dari snapshot karakter), `SpeakingRate`, `PreviousText`, `RequestID` (tanpa ekspresi).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): generation pipeline"`

### Task 13: Context builder (§7.8)

**Files:**
- Create: `internal/conversation/context_builder.go`, `testdata/prompt/conversation_max.golden.json`
- Test: `internal/conversation/context_builder_test.go`

**Interfaces:**
- Produces (§7.8.3 persis kecuali tipe konten memakai tipe M3, bukan `*Snapshot`): `Mode`, `TurnContext`, `ConversationInput{Mode; Prompt content.PromptVersion; Config content.SessionConfig` (karakter = `Config.Snapshot.Character`, `content.CharacterSnapshot`)`; Learner prompt.LearnerContext; Adaptive learner.AdaptiveDifficulty; Memory memory.MemoryContext; RollingSummary string; RecentTurns []TurnContext; CurrentUserText string; RemainingSeconds int; Budget prompt.Budget}`, `Message`, `BuiltPrompt`, `SummaryInput`, `PromptBuilder`, `NewPromptBuilder() PromptBuilder`.

- [ ] **Step 1: Write the failing test** — `TestBuildMaxFixtureWithinBudget` (fixture maksimum: `EstimatedInputTokens ≤ 6450`, komponen learner ≤ 700 tanpa memotong baris descriptor dimensi, memory core ≤ 600, episodik ≤ 500; golden byte-identik, `-update`); `TestTrimOrder` (input membengkak: urutan pemangkasan 8 → 7 → 10 → 6 (hanya `学習のねらい`) → 11 sampai 4 turn, dicatat `Truncations`); `TestMessageShape` (`CONVERSATION`: `system` + pasangan user/assistant + `user` terakhir; `INTERRUPTED` → `assistant_spoken_text + "（ユーザーが割り込みました）"`; `FAILED` → hanya `user`; `DISCARDED` dilewati; opening → hanya `assistant`); `TestOpeningOnlySystem` (`OPENING` hanya satu pesan `system`); `TestSystemIsRender` (`Messages[0].Content == prompt.Render(...).System` dengan `PromptData` terisi: `Session.RemainingMinutes` dibulatkan ke atas, `OutputContract(closing=remaining<=60)`); `TestFullNameNeverInPrompt` (`DisplayName` hanya nickname; string nama lengkap fixture tidak muncul di `Messages`; S-4); `TestCharacterFromSnapshot` (`PromptData.Character` = `Name`/`Persona`/`GreetingHint` dari `Config.Snapshot.Character`; voice ID vendor tidak pernah muncul di `Messages`; persona > 500 token dipotong di akhir paragraf); `TestSpeechFromAdaptive` (`Speech.Formality` = `adaptive.Formality`, fallback `Snapshot.Formality`; `FormalityInstruction` = `prompt.FormalityInstruction`; `MaxSentencesPerTurn` = `adaptive.MaxAISentencesPerTurn`, fallback/`MaxConsecutiveQuestions` dari `content.DefaultStyle`);  `TestDeterministic` (100× identik); `TestMaxOutputFromParams` (`llm_params` DB menang; default 300/200/0,8); `TestRollingSummaryPrompt` (template §7.8.5 persis, `MaxRunes 300`); `TestRecentWindow` (window 8, diperluas ≤ 12 bila `coveredUntilSeq` tertinggal).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Build|Trim|MessageShape|Opening|SystemIsRender|FullName|CharacterFromSnapshot|SpeechFromAdaptive|Deterministic|MaxOutput|RollingSummaryPrompt|RecentWindow'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `buildPromptData` memetakan `content.SessionConfig` (snapshot karakter, `ScenarioVersion`, `Snapshot.Formality`) ke `prompt.PromptData` (§6.8.2; tanpa `Style`/`Expression*`) tanpa I/O; pemangkasan dengan `prompt.EstimateTokens` + `ItemOverheadTokens`; tidak ada akses `users.full_name`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): prompt context builder"`

### Task 14: Adaptive in-session, rolling summary, summarizer

**Files:**
- Create: `internal/conversation/{adaptive.go,rolling_summary.go,summarizer.go}`
- Test: `internal/conversation/adaptive_test.go`, `summarizer_test.go`

**Interfaces:**
- Produces: `AdaptiveState{Current learner.AdaptiveDifficulty; TurnsSinceAdjust int}`, `ClarificationRegex` (`もう一度|もう一回|ゆっくり|わかりません|分かりません|わからない|分からない|聞き取れ|何て言いました|なんて言いました|どういう意味`), `MaybeAdjust(s AdaptiveState, recentUserTexts []string) (AdaptiveState, bool)`; `Summarizer` (`NewSummarizer(router provider.RealtimeRouter, b PromptBuilder, timeout time.Duration)`, `ShouldRun(turns []TurnContext, covered, oldestSeqInWindow int, running bool) bool`, `Run(ctx, in SummaryInput) (SummaryResult{Text string; CoveredUntilSeq int}, error)`; 3 gagal berturut → berhenti).

- [ ] **Step 1: Write the failing test** — `TestMaybeAdjust` (2 dari 6 turn berisi `もう一度`, `TurnsSinceAdjust` naik ke 4, `InSessionAdjustments<2` → `true`: `SpeechSpeed` turun 1 (min 1), `TTSSpeakingRate=learner.TTSSpeakingRate(baru)`, `MaxAISentencesPerTurn=learner.MaxAISentences(baru)`, `QuestionComplexity−1` (min 1), `Scaffolding+1` (maks 3), `InSessionAdjustments++`, counter 0; 1 sinyal → false; `TurnsSinceAdjust=3` → false; sudah 2 penyesuaian → false; snapshot masukan tidak dimutasi); `TestSummarizerTrigger` (butuh ≥ 4 turn non-DISCARDED dengan `seq ≤ oldest−1` dan `> covered`; tidak bila sedang berjalan); `TestSummarizerRequest` (`TextRequest{Task:"ROLLING_SUMMARY", Model: cfg.RollingSummaryLLMModel (env `ROLLING_SUMMARY_LLM_MODEL`, default model LLM vendor aktif: `OPENAI_LLM_MODEL` atau `DEEPSEEK_LLM_MODEL`), MaxTokens:400, Temperature:0.3, RequestID:"ROLLING_SUMMARY:{sid}"}`); `TestSummarizerTimeoutAndStop` (timeout 10 s dengan router lambat → error; 3 gagal berturut-turut → `ShouldRun` false).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'MaybeAdjust|Summarizer'`. Expected: FAIL.
- [ ] **Step 3: Implement** — murni sesuai §7.8.5–7.8.6.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): in-session adaptive adjust and rolling summary"`

### Task 15: Persister & redisSync

**Files:**
- Create: `internal/conversation/{persister.go,redissync.go}`
- Test: `internal/conversation/persister_test.go`, `redissync_test.go`

**Interfaces:**
- Produces `Persister`: `NewPersister(turns TurnRepository, events SessionEventWriter, audio AudioStore, fence int64, m *Metrics, log)`, `Submit(job)` (cap 64, tidak memblok loop: job `TurnUpdate` untuk turn sama digabung), jobs `InsertTurn`, `UpdateUser`, `FinalizeTurn`, `UploadWAV`, `Event`, `AddDisconnected`, `Close(ctx)`; ack `FenceLost` ke loop (`errors.Is(err, session.ErrFenceLost)` → berhenti total, tak menulis lagi); upload WAV 3 percobaan lalu `audio_capture`-gagal tanpa retry; antrean > 64 selama > 30 s → `ErrPersisterStuck`. `RedisSync`: `NewRedisSync(store rtstate.Store, clk, pod string, m)`, `Update(rt rtstate.RT)` (coalesce ≤ 1 tulis/100 ms; turn 250 ms), `Event(entry)` (durable → `AppendEvent`), `Flush(ctx)` sinkron, `Heartbeat`/owner refresh loop (5 s), snapshot tersimpan di memori saat Redis error lalu ditulis ulang **seluruhnya** (owner via `AcquireOwner`, hb, rt, turn, summary) begitu pulih.

- [ ] **Step 1: Write the failing test** — `TestPersisterOrdered` (job diproses berurutan: Insert < UpdateUser < Finalize); `TestPersisterCoalesce` (50 `UpdateUser` untuk turn sama saat worker diblok → ≤ 2 tulis, nilai terakhir menang); `TestPersisterFenceLost` (fake `TurnRepo` mengembalikan `session.ErrFenceLost` → callback `OnFenceLost` sekali, job berikutnya tidak dikirim); `TestUploadRetries3` (`AudioStore` gagal ×3 → `user_audio_object_key` tidak di-set, metrik naik, tidak ada panggilan ke-4); `TestRedisSyncCoalesce` (fake clock: 10 `Update` dalam 100 ms → 1 `WriteRT`); `TestRedisDownKeepsRunning` (`Store` gagal → tidak panic, tidak memblok; setelah pulih `WriteRT/WriteHeartbeat/AcquireOwner/WriteTurn/WriteSummary` ditulis ulang dengan snapshot terbaru; event replay yang terlewat tidak ditulis ulang; Review Focus 5); `TestEventsOnlyDurable`; `TestOwnerRefreshLost` (`AcquireOwner` → false → callback `OnFenceLost`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Persister|Upload|RedisSync|RedisDown|EventsOnlyDurable|OwnerRefresh'`. Expected: FAIL.
- [ ] **Step 3: Implement** — timeout 200 ms per operasi Redis; semua goroutine berhenti dengan `ctx`.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): persister and redis sync"`

### Task 16: `redisstore` state realtime (integration)

**Files:**
- Create: `internal/redisstore/realtime_state.go`
- Test: `internal/redisstore/realtime_state_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces: `NewRealtimeState(rdb redis.UniversalClient, log *slog.Logger) *RealtimeState` (`var _ rtstate.Store`); key persis §7.9.1; skrip Lua owner (acquire/refresh/takeover) dan release §7.9.2 verbatim lewat `EVALSHA` (fallback `EVAL` bila `NOSCRIPT`); `XADD … MAXLEN ~ 500` + `EXPIRE 120`; setiap operasi berbatas 200 ms.

- [ ] **Step 1: Write the failing test** — `repotest.RuntimeStoreSuite` (Task 2) ke Redis nyata; `TestOwnerLua` (fence 3 lalu 2 → 0; format nilai `pod|3`; `PX` ≈ ttl); `TestEveryKeyHasTTL` (setelah menulis semua jenis key, `TTL > 0` untuk semua kecuali stream yang `EXPIRE 120`); `TestStreamMaxLen` (1000 `AppendEvent` → `XLEN ≥ 500` dan ≤ ~600); `TestRTHashFields` (semua field §7.9.1 round-trip termasuk `adaptive` JSON; tanpa `stt_vendor`, D-017); `TestTurnDeleted`; `TestPubSubTakeover` (publish ke `kotoki:realtime:pod:{id}` diterima; subscribe ulang setelah koneksi diputus ≤ 1 s backoff); `TestKeysUseCanonicalUUID`; `TestRedisTimeout` (server di-`DEBUG SLEEP` → error ≤ 250 ms).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/redisstore/... -race -run 'RealtimeState|RuntimeStore|OwnerLua|EveryKey|StreamMaxLen|RTHash|PubSub'`. Expected: FAIL.
- [ ] **Step 3: Implement** — `TTL_S` dihitung pemanggil; adapter membungkus error dengan `errs.Wrap(err, errs.CodeInternalError, …)`; tidak mengimpor `conversation` (hanya `rtstate`).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make arch-check` hijau (cmd/api tidak menarik cgo).
- [ ] **Step 5: Commit** — `git commit -am "feat(redisstore): realtime state store"`

### Task 17: `objectstore` AudioStore & event writer adapter

**Files:**
- Create: `internal/objectstore/audio.go`, `internal/conversation/{event_writer.go,config_loader.go}`
- Test: `internal/objectstore/audio_test.go`, `internal/objectstore/audio_integration_test.go` (`integration`, MinIO), `internal/conversation/event_writer_test.go`, `config_loader_test.go`

**Interfaces:**
- Produces: `objectstore.NewAudioStore(s3 *s3.Client, bucket string, sse string, storageErrors *prometheus.CounterVec) *AudioStore` (`kotoki_storage_errors_total{op}` §13.7.3; gagal put → `op="put"` naik, error dibungkus `errs.Wrap`) (`PutTempWAV(ctx, key, pcm16k []int16)` menulis WAV PCM16 16 kHz mono; key harus berawalan `audio/tmp/`, selain itu error — put-only, tanpa get/delete); `conversation.NewSessionEventWriter(repo interface{AppendFenced(ctx, fence int64, e session.Event) error}, ids idgen.Generator, clk clock.Clock) SessionEventWriter` (`actor=REALTIME`, `from/to_status` NULL, payload JSON tanpa teks); `conversation.NewConfigLoader(l content.SessionConfigLoader, prompts interface{GetPromptVersion(ctx, id uuid.UUID) (content.PromptVersion, error)}, size int) ConfigLoader` (LRU per version ID; **tidak** query `PUBLISHED`; OPENING dimuat by ID dari `rec.OpeningPromptVersionID` bila non-nil).

- [ ] **Step 1: Write the failing test** — `TestPutTempWAV` (MinIO: objek ada, header WAV 44 B, `rate=16000`, panjang = 2× sampel + 44); `TestRejectsBadKey` (`audio/other/x.wav` → error); `TestPutFailureCountsStorageError` (S3 fake mengembalikan error → `kotoki_storage_errors_total{op="put"}==1`, error non-nil, tidak panic; registry diinjeksi); `TestEventWriterFields` (`event_type`, `actor=REALTIME`, `turn_id` diteruskan, `fence` diteruskan; payload `BARGE_IN` `{"generation","spoken_sentence_count","stop_ms"}` tanpa teks); `TestConfigLoaderCachesByID` (panggilan kedua tidak menyentuh `SessionConfigLoader`; `Opening` nil bila ID nil dan tidak ada query prompt); `TestConfigLoaderNeverQueriesPublished` (fake repo yang `panic` bila `GetPublishedPromptVersion` dipanggil); `TestConfigLoaderUsesSnapshot` (karakter/voice/formalitas dari `rec.ConfigSnapshot` (JSON) — tidak ada query `characters`; karakter yang di-`DISABLE` setelah sesi dibuat tetap termuat; snapshot rusak → `CONTENT_MISCONFIGURED`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'EventWriter|ConfigLoader'`; `go test -tags integration ./internal/objectstore/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `ConfigLoader.LoadSnapshot(ctx, rec session.RuntimeRecord)` memetakan `rec` → `content.SessionRefs{TopicID, ScenarioVersionID, PromptVersionID, ConfigSnapshot: rec.ConfigSnapshot}` (tanpa `ProviderProfile`: kolom `sessions.provider_profile` dihapus, D-015; TTS dari `ConfigSnapshot.Character`) → `SessionConfigLoader.Load` (scenario/prompt version di-cache LRU per ID; snapshot karakter di-decode per sesi, tidak di-cache).
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): audio store, event writer, config loader"`

### Task 18: Metrik §7.14, `Runtime` kerangka, harness, alur start

**Files:**
- Create: `internal/conversation/{metrics.go,runtime.go,runtime_start.go,manager.go}` (kerangka), `internal/testutil/rtharness/{harness.go,pipeconn.go,ticker.go}`
- Modify: `.golangci.yml` (aturan 5: tambah pengecualian `!**/internal/testutil/rtharness/**`; harness mengimpor `conversation` → cgo)
- Test: `internal/conversation/metrics_test.go`, `internal/conversation/runtime_start_test.go`

**Interfaces:**
- Produces `NewMetrics(reg prometheus.Registerer) *Metrics` (semua metrik §7.14, prefix `kotoki_realtime_`, bucket `TURN_BUCKETS`/`BARGE_BUCKETS`; label tanpa `session_id`/`user_id`); `Deps{Cfg config.RealtimeTuning; Limits Limits{ReadyMax, Max, HardMax int} (dari `config.Realtime` M1); Clock clock.Clock; IDs idgen.Generator; Log *slog.Logger; Metrics *Metrics; Session SessionRuntime; Turns TurnRepository; Events SessionEventWriter; Store RuntimeStore; Configs ConfigLoader; Learner LearnerContextProvider; Memory memory.RetrievalService; Router provider.RealtimeRouter; Audio AudioStore; Builder PromptBuilder; NewTicker func(time.Duration) Ticker; Apology func() ([][]byte, error)}`, `Ticker{C() <-chan time.Time; Stop()}`; `Runtime` (actor: `loop` mengonsumsi `cmdCh/frameEvents/sttEvents/genEvents/writerAcks/persistAcks/ctl` + ticker 100 ms; semua deadline = `time.Time` dicek terhadap `clock.Now()` per tick: warning, expiry, balance.tick, reconnect, STT final, playback, safepoint); `Manager` kerangka `NewManager(Deps)`, `Attach(ctx, conn Conn, t AttachTicket) error` (`AttachTicket{UserID, SessionID uuid.UUID}`); harness `rtharness.New(t, opts...) *H` (Clock fake, ManualTicker, PipeConn, `ScriptedSTT`, `ScriptedRouter`, fake port, `Mgr`) dengan `Connect(first protocol.ClientEvent)`, `Speak(fixture string)` (frame Opus asli per 20 ms dari fixture via `audio.Encoder(48000)`), `Silence(ms int)`, `Sync()` (barrier lewat audio→loop, STT→loop, gen→loop, `Outbound.Drained()`), `Events(types ...string)`, `Frames()`, `AdvanceClock(d)`, `Tick()`, `Close()`.

- [ ] **Step 1: Write the failing test** — `TestMetricsNamesAndBuckets` (semua nama §7.14 terdaftar tepat sekali, tanpa label terlarang); `TestStartUserFirst` (RESERVED → `session.ready{expected_command:"session.start"}` → `session.start` valid → panggilan fake `ClaimRuntime([RESERVED])` → `MarkStarting` → paralel load → `Activate` → `session.started{status:ACTIVE, turn_state:LISTENING, remaining_seconds:600}`, urutan panggilan tercatat, `AcquireOwner` dipanggil, event `OWNER_CHANGED{reason:"START"}`); `TestStartLoadsCharacterFromSnapshot` (karakter di `config_snapshot` berstatus `DISABLED` di tabel `characters` → sesi tetap start; `Config.Snapshot.Character.{ProviderName,ModelName,VoiceID}` + `Voice.Fallback` dipakai untuk `TTSRequest.Voice`, `NewGeneration` tanpa tier; D-014/D-015); `TestStartAIFirst` (turn 0 `INSERT` status NULL, `turn.started{initiator:"AI"}`, `ai.generation_started{kind:"OPENING"}`, `generation==1`, `nextTurnSeq==1`); `TestOpeningFallbackStatic` (`OpeningPromptVersionID=nil` → `Router.StreamLLM` **tidak** dipanggil, TTS memutar `opening_line`, `kotoki_content_opening_fallback_total`+1, `assistant_text==opening_line`, `llm_provider NULL`; S-2); `TestStartDeadlinePassed` (`MarkStarting` → `session.ErrStartDeadlinePassed` → error `SESSION_START_DEADLINE_PASSED`, close 4010); `TestStartFailsWhenSTTUnavailable` (`OpenSTT` error → `FailStart(PROVIDER_FAILURE)`, `session.failed`, close 4500, charge 0); `TestStartDegrades` (learner gagal 1 s / memory `Degraded` → lanjut tanpa); `TestStartTimeout10s` (load menggantung → `FailStart` setelah 10 s fake clock); `TestSessionEndBeforeActive` (`session.end` saat `STARTING` ditunda lalu dieksekusi setelah `ACTIVE`); `TestHandshakeFirstCommand` (command pertama selain start/resume → close 1008; tanpa command 10 s → 4008); `TestStartWhileActive` (status ACTIVE + `session.start` → diperlakukan `resume{0}`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Metrics|Start|Opening|Handshake|SessionEndBefore'`. Expected: FAIL.
- [ ] **Step 3: Implement** — alur §7.6.3 langkah 1–9 (errgroup deadline 10 s untuk langkah 4); loop satu-satunya pemilik state (tanpa mutex untuk state turn); `recover` per goroutine → `FATAL{SERVER_ERROR}`; span `session.start` (`session.id`,`fence`,`result`), log berisi `session_id, turn_id, generation, conn_id`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS (`-race`).
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): runtime skeleton, metrics, start flow, harness"`

### Task 19: Turn engine — user turn, discard, generation, penutupan turn

**Files:**
- Create: `internal/conversation/{runtime_turn.go,runtime_audio.go}`
- Modify: `internal/conversation/runtime.go`
- Test: `internal/conversation/runtime_turn_test.go`

**Interfaces:**
- Consumes Task 8–15, 18. Produces `audioPipeline` (decode 16 kHz, seq `dup/gap/PLC`, VAD, capture, `stt.SendAudio`, `frameEvents`), transisi tabel §7.6.1 baris 1–10, 14, 15, 18, 19; `commitBoundary()`; persist turn (D-078/D-079, tabel §7.6.6); pasca-turn (`recentTurns`, adaptive, summary, memory refresh `RefreshEpisodic` & `ReloadCoreIfChanged` di goroutine, maks 1 in-flight).

- [ ] **Step 1: Write the failing test** — `TestSeqGapPLC` (gap 3 → 3 PLC; gap 30 → 30 silence; gap 80 → 50 silence + WARN; duplikat dibuang; metrik `audio_frames_in_total{gap|dup}`); `TestInvalidFrameSilenceInserted` (paket sampah → `AUDIO_FRAME_INVALID` ≤ 1× / 5 s dan frame silence diteruskan; 51 dalam 10 s → violation); `TestUserTurnHappyPath` (`ja_question` → baris 1,3,7: event berurutan `speech.started`, `turn.started{USER}`, `stt.partial`…, `speech.ended`, `stt.final{end_reason:"SILENCE"}`, `ai.generation_started{REPLY}`, `ai.text_delta`, `ai.audio_started`, `ai.sentence`, binary gen 2 seq 0.., `ai.audio_completed`; `client.playback_finished{generation:2}` → `turn.completed{COMPLETED}`, state LISTENING; baris DB: INSERT status NULL lalu Finalize `COMPLETED` dengan `llm_provider`, `tts_voice_id` (= `VoiceRef.VoiceID` aktif dari `Snapshot.Character.VoiceID` atau `voice.fallback.voice_id` setelah switch; `FAKE` di mode fake), `turn_end_to_first_audio_ms ≥ 0`; tanpa kolom `expressions`, D-014); `TestFinalizeCountIsThree` (S-8: `FinalizeCalls()==3` per turn REPLY normal); `TestDiscardEchoCommit` (FINAL commit `DISCARD` berisi "gema" **tidak** muncul di `user_text` turn berikutnya, metrik `stt_discarded_commits_total`); `TestNoiseOnlyDiscarded` (STT final kosong → baris 8: `turn.completed{DISCARDED}`, tanpa generation, state LISTENING, `consecutiveFailedTurns` tetap, **tidak** ada APOLOGY; Review Focus 1/4); `TestShortNoiseDiscarded` (`"ん"` voiced 300 ms → DISCARDED); `TestUserResumesInProcessing` (baris 9: ucapan lanjut sebelum audio pertama → `ai.interrupted{USER_RESUMED}`, `generation` naik, turn sama, tidak ada INSERT baru); `TestNoMergeAfterMaxUtterance` (`ja_max_utterance` → `stt.final{end_reason:"MAX_UTTERANCE"}`; ucapan baru sesudahnya → turn baru, bukan merge); `TestSTTFinalTimeout` (tak ada FINAL dalam 1,5 s fake clock → pakai `TurnText()`, `stt_final_ms` payload −1 dan kolom NULL, `ReportFinalTimeout` dipanggil); `TestPlaybackDeadline` (tanpa `playback_finished`: turn ditutup di `estimatedEnd + 1500 ms`, `playback_finished_missing_total`+1); `TestOpeningUserResumes9b` (AI_FIRST, user bicara sebelum audio opening → turn 0 `INTERRUPTED`, assistant_spoken_text kosong); `TestPostTurnTasks` (`RefreshEpisodic` dipanggil dengan ≤ 3 turn, dilewati bila `user_text` < 6 rune, tidak > 1 in-flight; `ReloadCoreIfChanged` dipanggil); `TestSpokenTextAfterAudioOnly`; **`TestCaptureSkippedNoConsent`** (`audio_consent=false`: tidak ada `PutTempWAV`, `user_audio_object_key` NULL untuk semua turn, `kotoki_realtime_audio_capture_skipped_total{reason="no_consent"}==1` tepat sekali per sesi walau 3 turn; `PronunciationCaptureEnabled=false` + consent true → `{reason="disabled"}==1`; consent true + enabled → satu WAV `audio/tmp/{sid}/{turn_id}.wav` per turn final non-`DISCARDED`, metrik skipped 0; §7.5.5, §7.16 #7c).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'SeqGap|InvalidFrame|UserTurn|FinalizeCount|DiscardEcho|NoiseOnly|ShortNoise|UserResumes|NoMerge|STTFinalTimeout|PlaybackDeadline|OpeningUserResumes|PostTurn|SpokenText'`. Expected: FAIL.
- [ ] **Step 3: Implement** — tabel transisi sebagai `switch` di loop; jangan sentuh state dari goroutine lain; `endUserTurn` menghitung ambang untuk metrik `turn_end_reason_total`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): turn engine, audio pipeline, turn persistence"`

### Task 20: Barge-in, kegagalan generation, APOLOGY, penghitung gagal

**Files:**
- Create: `internal/conversation/runtime_bargein.go`, `internal/conversation/runtime_failure.go`
- Test: `internal/conversation/runtime_bargein_test.go`, `internal/conversation/runtime_failure_test.go`

**Interfaces:**
- Consumes Task 19. Produces `(*Runtime).bargeIn(onsetMs int64)` (§7.6.5: cancel, `generation++`, `activeGen.Store`, `PurgeAudio`, `ai.interrupted{BARGE_IN}`, finalize `INTERRUPTED`, `stt.MarkTurn`, `beginUserTurn`), baris 11–13, 16; `consecutiveFailedTurns`, `contentFilteredTurns`; peristiwa `session_events` `BARGE_IN`/`TURN_FAILED`/`PROVIDER_FALLBACK`.

- [ ] **Step 1: Write the failing test** — `TestBargeInConfirmed` (AI berbicara 2 kalimat; user `ja_barge_in` dengan partial ≥ 2 rune: `ai.interrupted{reason:"BARGE_IN", active_generation: N+1, spoken_sentence_count: k}`, `speech.started{during_ai_speech:true}`, `turn.started{USER}`; baris turn lama `INTERRUPTED` dengan `assistant_spoken_text` = tepat k kalimat yang frame terakhirnya tertulis dan `assistant_text` penuh; D-078); `TestNoStaleFramesAfterInterrupt` (setelah `ai.interrupted` tak ada frame biner gen < N+1 tertulis; goroutine gen lama keluar ≤ 1 s `NoGoroutineLeak`; event gen lama terlambat dibuang loop); `TestEchoDoesNotBargeIn` (suara berenergi saat `AI_SPEAKING` tanpa partial sejak commit terakhir → tidak ada barge-in; `ScriptedSTT` mengirim partial **lama** sebelum commit → tetap tidak; Review Focus 2); `TestClientBargeInHintOnly` (`client.barge_in` → metrik `bargein_total{source="hint_unconfirmed"}`, state tak berubah); `TestBargeInDuringApology` (turn tetap `FAILED`); `TestBargeInDisabledAfterExpiry` (`acceptingTurns=false`); `TestReplyFailureApology` (router `ERROR` LLM atau TTS tanpa `voice.fallback` → baris 11: `error{AI_GENERATION_FAILED}` non-fatal durable, `ai.generation_started{kind:"APOLOGY"}`, audio apology, turn `FAILED` dengan `error_code`, teks APOLOGY tidak di `assistant_text`); `TestFailureAfterFirstAudio16` (baris 16: `ai.interrupted{GENERATION_FAILED}` lalu APOLOGY); `TestOpeningFailure12` (turn 0 `FAILED`, kembali LISTENING, opening tidak diulang); `TestThreeFailedTurnsEndSession` (3 turn `FAILED` berturut → `BeginEnding(PROVIDER_FAILURE)` → `Fail`, `session.failed`, close 4500; turn `COMPLETED` di antara mereset); `TestContentFilteredNotCounted` (3 turn `CONTENT_FILTERED` berturut → 3 APOLOGY, sesi tetap `ACTIVE`, `consecutiveFailedTurns==0`, `kotoki_llm_content_filtered_total==3`, tak ada panggilan `Fail`; Review Focus 4); `TestNonJapaneseSTTFailsTurn` (final `"OK thanks"` tanpa aksara Jepang → turn `FAILED` + APOLOGY, tidak dihitung ke batas; nama dari plan `error_code="STT_NON_JAPANESE"`); `TestTruncatedIsCompleted` (S-7: turn `COMPLETED`, `ai.audio_completed{llm_finished:false}`, counter tak naik, `llm_truncated_total`+1); `TestFailedTurnDoesNotChangeCharge` (`Settlement` fake identik dengan sesi tanpa kegagalan berdurasi sama); `TestSTTErrorEndsSessionProviderFailure` (`STTError` dari router → `ACTIVE→ENDING` dengan `PROVIDER_FAILURE`, `session.failed`; tanpa pindah vendor, tanpa event `PROVIDER_FALLBACK`; rotasi koneksi vendor sama transparan bagi runtime).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'BargeIn|NoStale|EchoDoes|ClientBargeIn|Apology|ReplyFailure|FailureAfterFirst|OpeningFailure|ThreeFailed|ContentFiltered|NonJapanese|TruncatedIs|FailedTurnDoesNot|STTFallbackSticky'`. Expected: FAIL.
- [ ] **Step 3: Implement** — kode `bargeIn` §7.6.5; `isDiscard` → tambah cabang `!HasJapanese(text) && RuneLen ≥ 2` → turn `FAILED` (bukan DISCARDED).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): barge-in, failure handling, apology"`

### Task 21: Disconnect, resume pod sama, takeover, replay

**Files:**
- Create: `internal/conversation/{runtime_conn.go,replay.go}`
- Modify: `internal/conversation/manager.go`
- Test: `internal/conversation/runtime_conn_test.go`, `internal/conversation/replay_test.go`

**Interfaces:**
- Consumes Task 15, 18–20. Produces `(*Runtime).onDisconnect(from time.Time)` (§7.10.1), `attachResume(conn, lastSeq, audio)` (§7.10.2), `Manager.takeover(...)` (§7.10.3), `func replay(entries []rtstate.EventEntry, lastSeq, knownLast int64) ([]rtstate.EventEntry, string)` (§7.10.4), `session.snapshot` builder, subscriber `TAKEOVER`.

- [ ] **Step 1: Write the failing test** — `TestReplayAlgorithm` (tabel: stream kosong & `lastSeq==known` → `COMPLETE`; kosong & beda → `UNAVAILABLE`; `lastSeq > known` → `UNAVAILABLE`; tak ada yang terlewat → `COMPLETE` tanpa event; `missed[0].prev_seq <= lastSeq` → `COMPLETE` dengan event `replay:true`; celah (`prev_seq > lastSeq`) → `UNAVAILABLE`); `TestDisconnectMidTurn` (putus saat `AI_SPEAKING`: gen dibatalkan, `ai.interrupted{DISCONNECTED}` hanya ke replay (tak terkirim), turn berjalan `FAILED` dengan `TURN_FAILED{stage:"DISCONNECT"}`, STT ditutup, `rt.connected=0`, `SESSION_DISCONNECTED`, timer expiry tetap jalan, `balance.tick` berhenti; Review Focus 3); `TestResumeSamePod` (reconnect 7 s → `AddDisconnected(7)` (floor), STT dibuka ulang (vendor aktif env), event terlewat dikirim dengan `replay:true` tanpa duplikat lalu `session.snapshot{replay:"COMPLETE", server_seq_baseline}`; `session.started` tak dikirim ulang; `SESSION_RECONNECTED{same_pod:true}`; audio tidak di-replay); `TestReconnectTimeout30s` (tanpa reconnect 30 s fake clock → `AddDisconnected(30)` → `BeginEnding(RECONNECT_TIMEOUT)` → `Complete`; tanpa `session.ended` ke client yang tak ada); `TestOldConnClosed4011` (koneksi baru saat lama masih terpasang → lama ditutup 4011, `disconnectedFrom = lastActivity` lama); `TestTakeoverFencesOldOwner` (dua `Manager` berbagi fake `SessionRuntime`/`Store`: B `ClaimRuntime` → fence naik; A menerima `TAKEOVER`/`ErrFenceLost` dan berhenti ≤ 1 s tanpa tulis apa pun lagi (`TurnRepo` fake mencatat nol tulis dari A setelah itu; metrik `fence_lost_total`); `FailInProgress` dipanggil; `generation = max(rt.generation, maxGen)+1`; `nextTurnSeq = maxSeq+1`; `server_seq = max(rt.last_server_seq, ujung stream)+1`; `OWNER_CHANGED{reason:"TAKEOVER"}`); `TestTakeoverBeyondWindow` (`disconnected_at` > 30 s → `BeginEnding(RECONNECT_TIMEOUT)`+`Complete`, `session.completed`, close 1000); `TestTakeoverRedisDown` (§7.9.4: pakai Postgres saja, replay `UNAVAILABLE`, summary kosong); `TestResumeRejectedWhenEnding` (→ `SESSION_NOT_RESUMABLE` 4010); `TestResumeAdaptiveFromRT` (`rt.adaptive` menimpa snapshot DB).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Replay|Disconnect|ResumeSame|ReconnectTimeout|OldConn|Takeover|ResumeRejected|ResumeAdaptive'`. Expected: FAIL.
- [ ] **Step 3: Implement** — blok algoritma `replay` §7.10.4; urutan langkah takeover §7.10.3 (budget 3 s paralel); `AddDisconnected` dipotong `expires_at` oleh service M3 (runtime hanya mengirim detik penuh).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): disconnect, resume, takeover, replay"`

### Task 22: Akhir sesi — expiry, user end, kegagalan fatal

**Files:**
- Create: `internal/conversation/runtime_end.go`
- Test: `internal/conversation/runtime_end_test.go`

**Interfaces:**
- Consumes Task 18–21. Produces `(*Runtime).beginExpiry()`, `userEnd()`, `fatal(reason session.EndReason, code errs.Code)`, `finish()` (§7.11.2–7.11.4); warning timers.

- [ ] **Step 1: Write the failing test** — fake clock: `TestWarnings` (`session.ending{stage:"WARNING", remaining_seconds:60}` tepat di `expires_at−60 s`, lalu `10`; arahan penutup `残り時間が1分ほどです。…` masuk prompt generation berikutnya (`OutputContract(closing=true)`)); `TestBalanceTick10s` (tiap 10 s elapsed; berhenti saat terputus); `TestExpiryListening` (di `expires_at`: `BeginEnding(DURATION_ELAPSED)`, `session.ending{stage:"ENDING", reason:"DURATION_ELAPSED", grace_seconds:0}`, `Complete`, `session.completed{charged_seconds ≤ duration}`, close 1000, owner dilepas, runtime hilang dari registry); `TestExpiryAISpeaking` (LLM dipotong, kalimat yang sudah diparse tetap diputar, tak ada turn baru, selesai ≤ `expires_at+10 s`; turn `COMPLETED` bila LLM selesai normal selain itu `INTERRUPTED`; `grace_seconds = ceil(...)`); `TestExpiryUserSpeaking` (turn user disimpan `COMPLETED` tanpa jawaban AI, `stt.final{end_reason:"SESSION_ENDING"}`); `TestGraceDeadlineHard` (`expires_at+10 s` memotong sisanya); `TestUserEnd` (`session.end` → `BeginEnding(USER_ENDED)`, `grace_seconds:2`, gen dibatalkan `ai.interrupted{SESSION_ENDING}`, selesai; `session.end` kedua diabaikan, `Complete` tepat sekali); `TestBillingStopsAtEnding` (setelah `BeginEnding`, `Settlement.ChargedSeconds` fake tidak bertambah walau flush/grace memakan 9 s; Review Focus 3); `TestFatalSTTFailure` (urutan: `acceptingTurns=false` → batal → turn `FAILED` → `BeginEnding(PROVIDER_FAILURE)` → tunggu persister ≤ 5 s → `Fail` → `session.failed{charged_seconds}` + `error{PROVIDER_FAILURE}` → close 4500); `TestPanicInGoroutine` (panic di `gen.llm` → recover → `FATAL{SERVER_ERROR}` → `error{INTERNAL_ERROR}`, close 1011); `TestPersisterStuckFatal` (> 30 s → `SERVER_ERROR`); `TestNoGoroutineLeakAfterSession` (`NoGoroutineLeak(t,"internal/conversation")` setelah sesi selesai; §7.16 #10).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'Warnings|BalanceTick|Expiry|GraceDeadline|UserEnd|BillingStops|FatalSTT|PanicIn|PersisterStuck|NoGoroutineLeakAfter'`. Expected: FAIL.
- [ ] **Step 3: Implement** — urutan §7.11.4; `finish()` menunggu persister kosong ≤ 5 s sebelum `Complete`; tidak ada `time.Sleep`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): expiry, user end, fatal failure"`

### Task 23: Manager — admission, kapasitas, drain

**Files:**
- Modify: `internal/conversation/manager.go`
- Create: `internal/conversation/{manager_attach.go,drain.go}`
- Test: `internal/conversation/manager_test.go`, `internal/conversation/drain_test.go`

**Interfaces:**
- Produces `(*Manager).Admission() error` (`SERVER_DRAINING` bila draining; `SERVER_AT_CAPACITY` bila `active ≥ HardMax`), `ActiveSessions() int`, `Ready() bool` (`active < ReadyMax && !draining`), `Drain(ctx) error`, `Attach` langkah 7–11 §7.3.1 + tabel §7.3.2, `StartSubscriber(ctx)` (pub/sub pod), `PodInfoLoop(ctx)`.

- [ ] **Step 1: Write the failing test** — `TestAttachStatusTable` (`RESERVED`+`session.resume` → 4009 `retryable=false`; `STARTING` → 4009 `retryable=true`; `ACTIVE`+resume → resume; `ACTIVE`+start → resume `{0}`; `ENDING/COMPLETED/CANCELLED/ABANDONED/FAILED` → 4010; sesi tak ada → 4004; user beda → 4003); `TestCapacity` (100 runtime: `session.start` baru → `error{SERVER_AT_CAPACITY}` close 4503, sesi tetap `RESERVED`, `connections_total{rejected_capacity}`; resume tetap diterima sampai 120; `Admission()` error di 120; `Ready()` false di 95); `TestAdmissionBeforeTicket` (hanya `Admission`, tidak menyentuh store); `TestOneConnectionPerSession` (attach kedua menang, lama 4011); `TestDrainLISTENING` (§7.12.1: sesi LISTENING → flush persister ≤ 5 s, `rt` flush (connected=0, disconnected_at), `MarkHandoff`, `OWNER_CHANGED{DRAIN_RELEASE}`, `ReleaseOwner`, `error{SERVER_DRAINING}`, close 4100, sesi tetap `ACTIVE`, **tanpa turn FAILED**; podinfo `draining=1`); `TestDrainWaitsSafepoint` (turn berjalan menunda handoff; lewat 20 s → `gen.cancel()` + turn `FAILED` lalu handoff); `TestDrainDetachedImmediate`; `TestDrainStartingFinishesFirst`; `TestDrainEndingFinishesNormally`; `TestResumeAfterDrainOnOtherManager` (B resume → `handoff_at` NULL (via `ClaimRuntime` fake), turn tidak `FAILED` kecuali yang berjalan); `TestRedisDownAttach` (`Store` error saat handshake → `SERVICE_UNAVAILABLE` 4503; sesi berjalan tak terpengaruh; Review Focus 5).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/conversation/ -race -run 'AttachStatus|Capacity|AdmissionBefore|OneConnection|Drain|ResumeAfterDrain|RedisDownAttach'`. Expected: FAIL.
- [ ] **Step 3: Implement** — registry `map[uuid.UUID]*Runtime` dengan `sync.RWMutex` (hanya registry, bukan state turn); `activeSessions` sama dengan metrik `sessions_active` dijumlah.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(conversation): manager admission, capacity, drain"`

### Task 24: `wsgateway` — handshake, adapter Conn, redaksi ticket

**Files:**
- Create: `internal/wsgateway/{handler.go,conn.go,redact.go}`
- Test: `internal/wsgateway/handler_test.go`, `conn_test.go`

**Interfaces:**
- Consumes: `ratelimit.Limiter`/`ratelimit.Catalog`, `auth.TicketStore.Consume` (diperkecil menjadi `wsgateway.Tickets`), `conversation.Manager.Admission/Attach`. Produces `wsgateway.Handler{Tuning config.RealtimeTuning; Origins []string; AllowEmptyOrigin bool (dari `config.Realtime` M1); Limiter ratelimit.Limiter; Tickets Tickets; Mgr *conversation.Manager; Log; Metrics}`, `(*Handler).ServeHTTP` (langkah 1–6 §7.3.1), `NewConn(*websocket.Conn) conversation.Conn`, `RedactQuery(rawQuery string) string` (`ticket=REDACTED`).

- [ ] **Step 1: Write the failing test** — `httptest` + client `coder/websocket`: `TestOriginRejected` (Origin tak diizinkan → HTTP 403 tanpa upgrade, body D-013 `ORIGIN_NOT_ALLOWED`, ticket **tidak** dikonsumsi); `TestEmptyOriginPolicy` (kosong ditolak bila `WS_ALLOW_EMPTY_ORIGIN=false`, diterima bila true); `TestRateLimitIP` (limiter deny → 429, `Retry-After`, `RATE_LIMITED`, ticket utuh); `TestDrainingBeforeTicket` (`Admission` error → HTTP 503 `SERVER_DRAINING`/`SERVER_AT_CAPACITY`, `Retry-After: 1`, `Set-Cookie: kotoki_rt=; Max-Age=0; Path=/api/v1/realtime; Secure; HttpOnly`; `Tickets.Consume` **tidak dipanggil**; ticket yang sama lalu dipakai di handler lain → upgrade sukses; Review Focus 5); `TestTicketInvalidCloses4001` (upgrade sukses, event `error{TICKET_INVALID}` lalu close 4001); `TestTicketRedisError4503`; `TestAttachRateLimit4029` (`ws_attach_user`); `TestSubprotocolEcho` (`kotoki.v1`, kompresi mati, read limit 16384); `TestRedactTicket` (`/api/v1/realtime?ticket=abc&x=1` → `ticket=REDACTED&x=1` di log/trace); `TestConnAdapter` (Read/Write/Ping/Close memetakan kode 4xxx dan reason ≤ 123 byte).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/wsgateway/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — `websocket.Accept(w, r, &AcceptOptions{OriginPatterns, CompressionMode: CompressionDisabled, Subprotocols: ["kotoki.v1"]})`; urutan langkah 1→2→3→4→5→6 persis; gateway tidak mengimpor `go-redis`. Handler ini `http.Handler` mentah (bukan `httpapi.Handle`): header (`Retry-After`, `Set-Cookie`) ditulis langsung ke `w`, lalu body error D-013 lewat `httpapi.WriteError(w, r, err)` (status dari `httpapi.StatusOf`).
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(wsgateway): handshake, origin, capacity before ticket"`

### Task 25: Wiring `cmd/realtime`, `/readyz`, tracing

**Files:**
- Create: `cmd/realtime/{main.go,providers.go,ops.go}`, `cmd/internal/providerwire/{fake.go,fake_test.go}`
- Modify: `Makefile` (`run-realtime` bergantung `fixtures16k`, `build` cgo), `.env.example`
- Test: `cmd/realtime/ops_test.go`, `test/integration/m5_realtime_boot_test.go` (`integration`)

**Interfaces:**
- Consumes semua. Produces wiring manual (D-004): `config` → `pgxpool` (6 koneksi) → `postgres.NewStore`/`NewTurnRepo`/`NewEventRepo`/session repo → `session.NewService` + `session.NewRuntimeService` (ikuti wiring `cmd/api` M3) → `redisstore.NewRealtimeState`/`NewRateLimiter`/`NewTicketStore`/`NewFakeFaults` → `providerwire.BuildFake(cfg provider.RouterConfig, fakeCfg fake.Config, caps []provider.Capability, deps providerwire.Deps) (provider.RouterDeps, error)` (paket `cmd/internal/providerwire`, dipakai `cmd/realtime` dengan `Role: provider.RoleRealtime` dan wiring worker M7 dengan `RoleWorker`): memparse env `RouterConfig` & `fake.Config`, membangun fake `FAKE_PRIMARY` per kapabilitas di `caps` dan `FAKE_FALLBACK` hanya untuk TTS (slot `voice.fallback`; instance fake mengabaikan flag `*_ENABLED`, §8.5.1/§8.9) (`fake.New{STT,LLM,TTS,Evaluation,Embedding,Pronunciation}`, `fake.LoadCapConfig`) dan mengisi `RouterDeps` **penuh**: `Prices` via `pricing.Load`, `Metrics`, `Role`, `Clock`, `Log`, `Faults` (`redisstore.NewFakeFaults`) → `provider.NewRouter(cfg, rd)`; `PROVIDER_MODE=real` → error `adapter vendor tersedia di M9` (termasuk `CARTESIA`/TTS; di M5 hanya fake, yang mengabaikan `provider_name`/`model_name`/`voice_id` karakter, D-015; `RouterConfig` tanpa env profil/TTS; validasi flag `*_ENABLED` hanya mode real). `cmd/realtime/providers.go` memanggilnya dengan `caps = {STT, LLM, TTS, EMBEDDING}` (EMBEDDING = kapabilitas async pada router realtime, `fake.NewEmbedding`, agar M8 hanya menukar port memory/learner dengan `memory.Retriever` nyata tanpa mengubah wiring provider) → `objectstore.NewAudioStore` → `conversation.NewManager` → `wsgateway.Handler` pada `HTTP_ADDR` `:8081`; ops (`OPS_ADDR` `:9091`): `/healthz`, `/readyz` (503 bila `!Mgr.Ready()`), `/metrics`; SIGTERM → `Mgr.Drain` ≤ 30 s lalu exit 0; learner/memory: M5 memakai implementasi `noop` kecil di `cmd/realtime/providers.go` (`noopMemory`, `staticLearner` mengembalikan `LearnerContext{}`), diganti M8 (port saja).

- [ ] **Step 1: Write the failing test** — `TestReadyzThresholds` (`Ready()` false → 503, true → 200); `TestRealtimeRejectsRealModeInM5` (M9 membalik test ini: mode real diterima setelah adapter vendor ada); `TestBuildFakeFillsFullRouterDeps` (semua field `RouterDeps` terisi untuk `caps={STT,LLM,TTS,EMBEDDING}` dengan `RoleRealtime`: `router.Embed` mengembalikan vektor 1536 dim; `RoleWorker` + `caps` lengkap juga valid; kapabilitas tak diminta tidak dibuat); `TestBootIntegration` (Postgres+Redis testcontainers, `PROVIDER_MODE=fake`: proses naik, `/readyz` 200, `GET /api/v1/realtime` dengan Origin salah → 403, SIGTERM → exit 0 ≤ 30 s, podinfo `draining=1` sempat tertulis); `make arch-check` (api/worker bebas cgo).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/realtime/... -race`; `go test -tags integration ./test/integration -race -run M5Boot`. Expected: FAIL.
- [ ] **Step 3: Implement** — tracing: `otelhttp` untuk ops, span `ws.connection` (`session.id`, `conn.id`, `resume`, `close.code`) di handler; log redaksi query `ticket`; env validasi startup (`WS_ALLOWED_ORIGINS` wajib kecuali `APP_ENV=local`).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make build` sukses (api/worker `CGO_ENABLED=0`, realtime/cli cgo).
- [ ] **Step 5: Commit** — `git commit -am "feat(realtime): cmd/realtime wiring, ops, graceful drain"`

### Task 26: `kotokiclient` — REST, WS, audio, pacer

**Files:**
- Create: `internal/kotokiclient/{client.go,types.go,realtime.go,pacer.go,report.go}`, `internal/kotokiclient/audio/{wav.go,encode.go,decode.go}`
- Test: `internal/kotokiclient/client_test.go`, `realtime_test.go`, `pacer_test.go`, `audio/audio_test.go`

**Interfaces:**
- Produces (§14.11.1, signature persis): `New(baseURL, opts...)`, `Register/Login/Refresh/Catalog/CreateSession/CancelSession/RealtimeTicket/GetSession/GetEvaluation/Balance`, tambahan (nama dari plan) `Turns(ctx, id uuid.UUID, afterSeq *int, limit int) ([]Turn, string, error)` (`GET /api/v1/sessions/{id}/turns`) dan `EndSession` tidak ada di REST (akhir via WS); `Dial(ctx, wsURL, ticket, origin)`, `Send`, `SendAudio` (header D-073, seq otomatis), `Events()`, `Audio()` (`AudioFrame{Generation, Seq, Payload, At}`), `Close`; `audio.ReadWAV` (downmix + resample linear → 48 kHz), `WriteWAV`, `PreEncode(pcm) ([][]byte, error)` (Opus 48 kHz, 20 ms/960 sampel, VOIP, 32 kbps, complexity 5), `SilencePacket()`, `Decoder`; `Pacer` jadwal absolut `start + i×20 ms` (`NewPacer(start time.Time, now func() time.Time, sleep func(ctx, d) error)` — injeksi agar test deterministik). Klien membuang frame `Generation <` generasi aktif terakhir dari `ai.generation_started`/`ai.interrupted.active_generation`/`session.snapshot.active_generation` (Review Focus 2). DTO JSON REST disalin dari DTO `httpapi` M2/M3 (jangan impor `httpapi`).

- [ ] **Step 1: Write the failing test** — `TestPacerNoDrift` (1000 `Wait` dengan sleep yang telat 3 ms acak → waktu ke-1000 = `start + 20 s` ±0, bukan drift); `TestPreEncodeShape` (WAV 1 s 48 kHz → 50 paket 1..4091 B; `SilencePacket` dekode ke ≈ hening); `TestReadWAVResamples` (16 kHz stereo → 48 kHz mono); `TestSendAudioHeader` (seq 0,1,2 big-endian `01 00 00 00 00 …`); `TestDropsStaleGeneration` (server httptest menyiarkan `ai.generation_started{gen 3}` lalu frame gen 2 → tidak muncul di `Audio()`; setelah `ai.interrupted{active_generation:5}` frame gen 4 dibuang); `TestRESTRoundTrip` (httptest: `Register` → `Login` → cookie refresh tersimpan → `CreateSession` mengirim header `Idempotency-Key` (UUID) → `RealtimeTicket`); `TestErrorDecoding` (body D-013 → `*APIError{Code, Message, RequestID}`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/kotokiclient/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — hanya mengimpor `conversation/protocol` dan `conversation/audio` (bukan `conversation`); tidak ada `time.Now()` langsung di Pacer.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(kotokiclient): REST+WS client, audio, pacer"`

### Task 27: `kotokiclient` — skrip percakapan & Report

**Files:**
- Create: `internal/kotokiclient/{steps.go,converse.go,sink.go}`
- Test: `internal/kotokiclient/converse_test.go`

**Interfaces:**
- Produces (§14.11.1): `Step` + `Speak{Packets, Label}`, `WaitAIDone{Timeout}`, `Think{D}`, `BargeIn{After, Packets}`, `Drop{For}`, `End{}`; `SessionSpec`, `Sink{OnEvent(Received[protocol.ServerEvent]); OnAudio(AudioFrame)}`, `Report{ConnectMS, StartMS int64; TurnLatencyMS, BargeInStopMS, ResumeMS []int64; ServerLatencies []ServerLatency; ChargedSeconds int; Status string}` dengan definisi pengukuran tabel §14.11.1; `RunConversation(ctx, c *Client, s SessionSpec, steps []Step, sink Sink) (Report, error)` selalu mengirim frame tiap 20 ms (ucapan atau `SilencePacket`), `Drop` = tutup TCP → ticket baru → `session.resume{last_server_seq}` → lanjut.

- [ ] **Step 1: Write the failing test** — server palsu in-process (httptest + `coder/websocket`) yang memainkan skrip protokol: `TestRunConversationHappy` (Speak→WaitAIDone→End; `Report.TurnLatencyMS` = jarak frame ucapan terakhir → frame audio pertama generation baru, dihitung dengan jam fake; `StartMS` dari `session.start` → `session.started`); `TestBargeInStepMeasured` (`BargeInStopMS` = frame ucapan penyela pertama → frame terakhir generation lama); `TestDropAndResume` (setelah `Drop` klien meminta ticket baru, mengirim `session.resume` dengan `last_server_seq` terbesar yang terlihat; event `replay:true` tidak menggandakan `Report`; `ResumeMS` terisi); `TestAlwaysSendsFramesEvery20ms` (selama `Think{1s}` server menerima 50 ± 1 frame, seq berurutan); `TestSnapshotResetsBaseline` (setelah `session.snapshot`, `last_server_seq = server_seq_baseline`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/kotokiclient/ -race -run 'RunConversation|BargeInStep|DropAndResume|AlwaysSends|SnapshotResets'`. Expected: FAIL.
- [ ] **Step 3: Implement** — satu goroutine pengirim frame (Pacer), satu pembaca event; `Report` ditulis `summary.json` oleh CLI.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(kotokiclient): conversation scripts and report"`

### Task 28: `cmd/kotoki-cli`

**Files:**
- Create: `cmd/kotoki-cli/{main.go,creds.go,cmd_auth.go,cmd_session.go,cmd_converse.go}`
- Test: `cmd/kotoki-cli/cli_test.go`

**Interfaces:**
- Produces subcommand `flag` standar: `register`, `login`, `catalog`, `balance`, `session {create|get|cancel|transcript}`, `ticket`, `converse` (alias `chat`), `evaluation get` (implementasi trivial; diuji di M7); flag global `--api-url` (`KOTOKI_CLI_API_URL`, `http://localhost:8080`), `--ws-url` (`KOTOKI_CLI_WS_URL`, `ws://localhost:8081/api/v1/realtime`), `--origin` (`KOTOKI_CLI_ORIGIN`, `http://localhost:3000`), `--json`, `--timeout 30s`; kredensial `$XDG_CONFIG_HOME/kotoki-cli/credentials.json` mode 0600 per `api-url`; flag `converse` persis tabel §14.11.2 (`--wav` wajib, `--think 1s`, `--ai-timeout 20s`, `--barge-in-after`, `--drop-after/--drop-for`, `--end after-last|wait-expiry|<dur>`, `--out-dir out/<session_id>`, `--no-audio-save`, `--quiet`); output: `turn-<seq 3 digit>-gen-<generation>.wav`, `events.jsonl`, `summary.json`; `session end` **tidak ada** (akhir sesi hanya lewat WS; spec menyebut `session end` — dilayani dengan membuka WS singkat `session.resume`+`session.end`, nama dari plan).

- [ ] **Step 1: Write the failing test** — `TestFlagDefaults` (default env/flag persis di atas); `TestCredentialsFileMode0600` & dikunci per `api-url`; `TestConverseRequiresWav`; `TestConverseOutputs` (server palsu dari Task 27: tiga file keluaran ada, WAV downstream valid 48 kHz, `events.jsonl` satu JSON per baris dengan timestamp terima, `summary.json` memuat `turn_latency_ms`); `TestJSONOutputMode`; `TestRegisterUsesTermsVersion` (mengambil `GET /api/v1/auth/terms` bila `--terms-version` kosong).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kotoki-cli/... -race`. Expected: FAIL.
- [ ] **Step 3: Implement** — teks percakapan hanya dicetak lokal (tidak dilog server); `make cli` membangun `bin/kotoki-cli` (cgo).
- [ ] **Step 4: Run tests to verify they pass** — PASS; `make cli && ./bin/kotoki-cli --help` sukses.
- [ ] **Step 5: Commit** — `git commit -am "feat(cli): kotoki-cli basic subcommands"`

### Task 29: Harness e2e + E1, E2, E6, E11 + SLO server-side

**Files:**
- Create: `test/e2e/{main_test.go,env_test.go,helpers_test.go,e1_e2_e6_test.go,slo_test.go}`
- Test: (file itu sendiri; tag `e2e`)

**Interfaces:**
- Produces `TestMain` (§14.5): testcontainers PG/Redis (+MinIO) via `testutil`, `migrate` + `seed/*.sql`, `go build -race` `cmd/api` & `cmd/realtime` ke temp, subprocess dengan `cmd.Env` eksplisit (`APP_ENV=local`, `PROVIDER_MODE=fake`, port acak, `INITIAL_BALANCE_SECONDS=3600`, `WS_ALLOW_EMPTY_ORIGIN=true`, `FAKE_STT_FIXTURE_DIR=<temp 16 kHz dari testdata/audio/synth via genaudio -to16k>`, `FAKE_PRIMARY_STT_LATENCY_MS=250`, `FAKE_PRIMARY_LLM_LATENCY_MS=500`, `FAKE_LLM_TOKEN_INTERVAL_MS=30`, `FAKE_PRIMARY_TTS_LATENCY_MS=300`), tunggu `/readyz` 200 ≤ 30 s, log subprocess ke file (dicetak saat gagal); helper `newUser(t)`, `startSession(t, mode)`, `scrape(t, opsAddr) map[string]float64`, `quantile(hist, 0.95)`. Worker belum ada (M6): E8/E9 ditunda ke M6–M8.

- [ ] **Step 1: Write the failing test** — **E1** (`ja_self_intro.wav` USER_FIRST: urutan event Bab 7, frame gen ≥ 1 diterima, `session_turns` 1 baris `COMPLETED` dengan `turn_end_to_first_audio_ms`/`llm_first_token_ms`/`tts_first_audio_ms` terisi, `end` → sesi `COMPLETED`, ledger `RESERVE`+`CONSUME`+`RELEASE` konsisten dengan `charged_seconds`); **E2** (AI_FIRST: turn `sequence_number=0`, `user_text IS NULL`, audio opening diterima); **E6** (cancel `RESERVED` → `CANCELLED`, reservation `RELEASED`, saldo kembali 3600); **E11** (ticket dipakai dua kali → upgrade lalu close 4001; ticket > 30 s (Redis `EXPIRE` dipaksa) → 4001); **`TestSLOServerSide`**: 20 turn berurutan (`ja_question` + `ja_greeting_short` bergantian) → scrape `/metrics` realtime: P95 `kotoki_realtime_turn_end_to_first_audio_seconds` ≤ 1,5 s (D-100) dan `kotoki_realtime_server_overhead_seconds` P95 ≤ 0,05 s; hasil (P50/P95) dicatat ke `docs/implementation-log.md` oleh implementer.
- [ ] **Step 2: Run it to verify it fails** — `go test -race -count=1 -tags=e2e ./test/e2e/... -run 'TestE1|TestE2|TestE6|TestE11|TestSLO'`. Expected: FAIL (harness/biner belum lengkap).
- [ ] **Step 3: Implement** — perbaiki bug yang ditemukan di runtime (bukan melonggarkan assertion); `require.Eventually` ≤ 10 s tick 50 ms, tanpa sleep tetap.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS.
- [ ] **Step 5: Commit** — `git commit -am "test(e2e): realtime harness, E1 E2 E6 E11, SLO server-side"`

### Task 30: E2E E3, E4, E5, E7, E10, E12, E13, E14 + P95 barge-in

**Files:**
- Create: `test/e2e/{e3_bargein_test.go,e4_e5_resume_test.go,e7_e12_e13_test.go,e10_e14_test.go,bargein_p95_test.go}`
- Test: (file itu; tag `e2e`)

**Interfaces:**
- Consumes `kotokiclient` (`Drop`, `BargeIn`), Redis `kotoki:fake:faults`, `session.NewSweeper(...).Tick` in-process terhadap DB e2e (substitusi `worker run-job session_sweeper`; worker = M6), admin retire OPENING lewat `POST /api/v1/admin/content/prompts/OPENING/versions/{id}/retire` (M3), proses realtime kedua (`REALTIME_POD_ID` berbeda).

- [ ] **Step 1: Write the failing test** — **E3** (`ja_barge_in.wav` 1 s setelah frame audio AI pertama → `ai.interrupted`; tak ada frame gen lama > 200 ms setelah event; turn `INTERRUPTED`, `assistant_spoken_text` prefix `assistant_text`); **E4** (`Drop{For:3s}` → resume, event terlewat tanpa duplikat, sesi lanjut, `disconnected_seconds ≥ 2`, turn berjalan saat putus `FAILED`); **E5** (dua proses realtime; `kill -9` pemilik; reconnect ke proses lain → takeover, turn berjalan `FAILED`, sesi lanjut; pemilik lama tak menulis); **E7** (STT gagal, `kotoki:fake:faults={"STT.FAKE_PRIMARY":{"mode":"timeout","rate":1.0}}` → `STTError` → sesi `FAILED` `PROVIDER_FAILURE` (close 4500; tanpa pindah vendor, D-017); hapus fault, sesi baru normal); **E10** (long, `E2E_LONG=1`): sesi 300 s → `session.ending` (60 s dan 10 s WARNING lalu `ENDING`) → `COMPLETED`, `charged_seconds ≤ 300`; **E12** (admin retire OPENING → AI_FIRST: `opening_prompt_version_id IS NULL`, turn 0 `assistant_text == opening_line`, audio diterima, `kotoki_content_opening_fallback_total` naik; cleanup publish ulang; tidak paralel dengan E2); **E13** (fault `LLM.FAKE_PRIMARY` `{"mode":"error","kind":"CONTENT_FILTERED","rate":1.0}` 4 turn → semua `FAILED` + APOLOGY diterima, sesi `ACTIVE`, `kotoki_llm_content_filtered_total==4`; hapus fault → turn berikut `COMPLETED`; `charged_seconds` sama sesi kontrol); **E14** (proses realtime tambahan; `SIGTERM` → close 4100, `handoff_at` terisi; klien tak reconnect; tunggu 31 s → `Sweeper.Tick` → `COMPLETED` `RECONNECT_TIMEOUT`, `charged_seconds` dari `billable_end = handoff_at`; kontrol: reconnect < 30 s → `handoff_at` NULL, sesi lanjut); **`TestBargeInP95`** (200 percobaan barge-in in-process dengan `rtharness` jam nyata + `ScriptedRouter`: P95 `bargein_stop_seconds` ≤ 0,15 s, nol frame gen lama tertulis setelah `ai.interrupted`; D-101/§7.16 #3).
- [ ] **Step 2: Run it to verify it fails** — `go test -race -count=1 -tags=e2e ./test/e2e/... -run 'TestE3|TestE4|TestE5|TestE7|TestE12|TestE13|TestE14|TestBargeInP95'`. Expected: FAIL.
- [ ] **Step 3: Implement** — perbaiki runtime/gateway bila perlu; E10 hanya bila `E2E_LONG=1` (`t.Skip` selain itu, sesuai §14.5).
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `E2E_LONG=1 go test -race -tags=e2e ./test/e2e/... -run TestE10` PASS.
- [ ] **Step 5: Commit** — `git commit -am "test(e2e): barge-in, resume, takeover, faults, drain E3-E14"`

---

## Gerbang milestone

Jalankan berurutan; catat perintah + ringkasan output di `docs/implementation-log.md` (termasuk P50/P95 SLO fake dan hasil `TestBargeInP95`).

- [ ] `make arch-check` → `CGO_ENABLED=0 go build ./cmd/api ./cmd/worker` sukses dan tidak ada baris `internal/conversation/audio|hraban/opus|go-webrtcvad` pada `go list -deps`.
- [ ] `make lint` → 0 issue (depguard: `conversation` tidak mengimpor `coder/websocket`/`go-redis`/adapter; `wsgateway` tak mengimpor `go-redis`; `redisstore` tak mengimpor `conversation`).
- [ ] `make test` → hijau (`-race`), < 90 s; coverage `internal/conversation/...` ≥ 80%, `internal/wsgateway` ≥ 70% (`make test-cover`).
- [ ] `make test-integration` → hijau (redis realtime state, objectstore, turn repo fence, boot).
- [ ] `make test-e2e` → hijau (E1–E7, E11–E14, SLO, `TestBargeInP95`); `E2E_LONG=1 make test-e2e` → E10 hijau.
- [ ] SLO fake tercatat: P95 `turn_end_to_first_audio_seconds` ≤ 1,5 s (D-100); `server_overhead_seconds` P95 ≤ 50 ms; `bargein_stop_seconds` P95 ≤ 0,15 s (D-101).
- [ ] §14.10.5 (clone → percakapan pertama, ≤ 15 menit): `make env up migrate-up seed fixtures16k` → `make run-api`/`run-realtime` (terminal terpisah) → `make cli` → `./bin/kotoki-cli register …` → `login` → `converse --wav testdata/audio/synth/ja_self_intro.wav,testdata/audio/synth/ja_question.wav --out-dir out/first-run` → keluaran: event berurutan, ringkasan latensi, `out/first-run/turn-001-gen-*.wav`, `events.jsonl`, `summary.json`, `session COMPLETED`. Catat waktu total. (`evaluation get` menunggu M6–M8.)
- [ ] `docs/implementation-deviations.md` memuat: pemisahan `rtstate`, `SessionEventWriter.turnID`, `LearnerContextProvider.Rerender`, helper leak pengganti goleak, field payload tak dikenal diabaikan (§7.4.2 vs §14.4), STT kosong → `DISCARDED` (§7.6.2 vs Review Focus), `STT_NON_JAPANESE`, ukuran fixture > 8 MB & fixture sintetis, subcommand `session end` via WS, E8/E9 ditunda.

## Self-Review

- **Cakupan spec:** §7.1–7.14 dan §7.16 (butir 1–10, 7b–7h) tercakup: protokol §7.4 (T3–4), audio §7.5 (T5–6, 19), detector/turn engine §7.6 (T8–9, 19–20), orkestrasi & parser §7.7 (T12; parser di M4), context builder §7.8 (T13–14), Redis §7.9 (T15–16), reconnect §7.10 (T21), akhir sesi §7.11 (T22), drain/kapasitas §7.12 (T23–24), port §7.13 (T2, 17), metrik/trace §7.14 (T18, 25). §7.15 = diagram (tidak ada kode). §14.4 golden WS, §14.5 E1–E7/E10–E14, §14.10.5, §14.11 tercakup. **Tidak dicakup:** E8 (pipeline async), E9 (hapus akun) → M6–M8; implementasi nyata memory/learner → M8; vendor nyata & aset APOLOGY dari TTS asli → M9; `provider-bench` → M9; chaos Redis-down di staging → M10 (di M5 hanya unit/integration: T15, 21, 23).
- **Konsistensi tipe:** `TurnState`, `GenKind`, `Generation`, `GenEvent` (T12) dipakai T18–T22; `Outbound/Writer` (T10) dipakai T12, 18; `Pacer` (T11) dipakai T12; `rtstate.Store` (T2) diimplementasi T16 dan di-fake T2; `Deps` (T18) menjadi satu-satunya titik wiring (T25); `Persister`/`RedisSync` (T15) dipakai T18–23; `Detector`/`sttState` (T8–9) dipakai T19–20; nama M3/M4 dipakai persis seperti produksi mereka (lihat header Depends on).
- **Risiko yang perlu keputusan implementer:** (1) fixture sintetis harus lolos `TestFixturesVADSanity` (T7) — ubah generator, bukan ambang; (2) bila `session.TurnFinal`/`session.Event` M3 kekurangan field, tambahkan di `session` (T1) dan catat; (3) `ScriptedSTT` dan harness hanya deterministik jika `Sync()` dipakai sebelum setiap assertion urutan event.
