# Kotoki Backend — M9 Real Providers & Benchmark Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mengganti provider fake dengan adapter vendor nyata (D-017: OpenAI untuk STT/LLM/Evaluation/Embedding, DeepSeek untuk LLM/Evaluation, ElevenLabs untuk STT/TTS, Cartesia untuk STT/TTS) yang lulus recorded-fixture test + `providertest`, men-wire `PROVIDER_MODE=real` dengan sakelar `*_ENABLED` dan validasi startup ("tepat satu" STT, "tepat satu" LLM, minimal satu TTS), dan menyediakan `kotoki-cli provider-bench`/`provider-report` yang membandingkan 3 STT, 2 LLM, dan vendor/model/voice TTS per karakter (`characters.provider_name/model_name/voice_id` + `voice.fallback`, D-015). Hasil benchmark hanya **rekomendasi**; pemilihan vendor aktif di `.env` adalah keputusan manual pemilik produk (tidak ada pemilihan otomatis, tidak ada fallback vendor STT/LLM).
**Architecture:** Satu paket per vendor `internal/provider/<vendor>` (`openai`, `deepseek`, `elevenlabs`, `cartesia`; `net/http` + `coder/websocket`, tanpa SDK) yang hanya mengimpor `provider`, `provider/httpx`, `platform/otel`, stdlib; tidak mengimpor `conversation/audio`, `fake`, `pricing`, vendor lain (`deepseek` tidak mengimpor `openai`; kemiripan Chat Completions diduplikasi kecil atau lewat helper `httpx`; resample/WAV lewat `provider/pcm.go`). Adapter dirakit hanya di `cmd/internal/providerwire` (dipakai `cmd/realtime`, `cmd/worker`, `cmd/kotoki-cli`): `providerwire.Build` membangun adapter hanya untuk kapabilitas yang **enabled** (flag `*_ENABLED`; satu STT, satu LLM + Evaluation vendor yang sama, Embedding OpenAI, satu atau dua TTS) dan `ValidateReal` menolak kombinasi flag yang salah; untuk TTS (D-015) SEMUA vendor TTS enabled dibangun karena vendor dipilih per karakter saat runtime. Tool benchmark hidup di `cmd/kotoki-cli/internal/{bench,report}` dan bergantung pada port (bukan vendor) sehingga teruji dengan `fake`. Fixture recorded dimainkan oleh simulator vendor di `internal/testutil/replay`; semua yang butuh kunci/akun/penilaian manusia ditandai **MANUAL GATE**.
**Tech Stack:** Go 1.27, `net/http`, `github.com/coder/websocket`, `encoding/json`, `otelhttp`/`platform/otel` (trace), `prometheus/client_golang` (hanya gauge rate-limit adapter), `gopkg.in/yaml.v3` (hanya `provider/pricing`, sudah ada), `pgx/v5` (hanya `provider-report`), testify, testcontainers-postgres (integration). Tanpa SDK vendor, tanpa library baru (D-003).
**Spec:** docs/superpowers/specs/2026-10-12-kotoki-backend-design.md — Bab 8 §8.4.0–8.4.14 (adapter: §8.4.1 ElevenLabs STT, §8.4.2 OpenAI STT, §8.4.3 OpenAI LLM, §8.4.4 DeepSeek LLM, §8.4.5 ElevenLabs TTS, §8.4.6 Cartesia STT, §8.4.7 matriks, §8.4.8 OpenAI Evaluation, §8.4.9 DeepSeek Evaluation, §8.4.10 Embedding, §8.4.11 (dihapus dari v1), §8.4.12 voice per karakter, §8.4.14 Cartesia TTS), §8.5.1 (sakelar `*_ENABLED` & validasi startup), §8.5.8 (rotasi koneksi STT vendor yang sama), §8.8.3 (`provider-report`), §8.10 (benchmark), §8.11 + §14.6 (test adapter), Bab 13 §13.2.8 (env), §6.3.5/§6.13.3 (voice ID), D-017, D-040, D-041, D-015, D-NEW-8-1…8-31.
**Master plan:** docs/superpowers/plans/2026-10-13-kotoki-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M4 (port & tipe `provider.*`, `provider.Error/KindOf`, `httpx.NewClient/ClassifyHTTP/ClassifyError/ClassifyWSClose/SSEReader`, `providertest.{LLM,TTS,STT,Evaluation,Embedding}Suite`, `provider.RouterConfig` (+`Validate()` dan flag `*_ENABLED`), `provider.NewRouter`/`RouterDeps` (satu adapter per kapabilitas STT/LLM/Evaluation/Embedding; `TTS map[Name]TTSProvider`), `rotatingSTT` (§8.5.8), `provider.TTSRequest` (`VoiceID`/`Model`/`SpeakingRate`), `PriceTable`, `pricing.Load`, `fake.*`, `provider.NormalizeJA/WAVDurationMs/EncodeWAV/DecodeWAV/ResamplePCM16`); M5 (`cmd/realtime` wiring fake, `kotokiclient`, `kotoki-cli converse`, `testdata/audio/*.wav` §14.5, harness e2e) untuk Task 17 (e2e real), Task 19 (`RunE2E`), dan Task 21; M7 (worker evaluasi memakai `AsyncRouter.Evaluation()`) dan M8 (ekstraksi memory memakai `AsyncRouter.Evaluation()`) untuk gate real-mode worker; M3 (tabel `characters` dengan kolom `provider_name` (CHECK `IN ('ELEVENLABS','CARTESIA')`)/`model_name`/`voice_id` dan `voice` JSONB (`gender`/`accent`/`speaking_rate`/`fallback?`), seed §6.13, D-014/D-015).

## Global Constraints

- Master Global Constraints berlaku.
- Tanpa SDK vendor; dependensi adapter = stdlib + `coder/websocket` + `provider`/`provider/httpx`/`platform/otel`. Adapter MUST NOT mengimpor `conversation/audio`, `fake`, `pricing`, vendor lain, paket fitur selain `provider` (§8.1 butir 2; depguard §14.9.5). `time.Now()` boleh di adapter (pengecualian forbidigo), tapi timer yang diuji dibuat dapat dikonfigurasi lewat `Config`.
- Setiap adapter: satu `Config` per vendor + `New<Cap>(cfg Config, hc *http.Client, log *slog.Logger) (*<Cap>, error)` (§8.1 butir 3): `elevenlabs.NewSTT`/`NewTTS`, `cartesia.NewSTT`/`NewTTS`, `openai.NewSTT`/`NewLLM`/`NewEvaluation`/`NewEmbedding`, `deepseek.NewLLM`/`NewEvaluation`; nama vendor/kapabilitas UPPERCASE; span OTel `provider.<capability>` dengan atribut `provider.name`, `provider.model`, `provider.outcome`, `provider.error_kind` dibuat adapter sendiri (router M4 tidak punya span); log berisi `provider, capability, model, request_id, kind, http_status, vendor_code`, MUST NOT berisi API key, teks user/AI, audio (§8.4.0).
- `make modelname-check`: kode non-test adapter MUST NOT memuat literal `gpt-|o[0-9]-|deepseek-|claude-|eleven_|scribe|ink-|sonic|text-embedding-` (`ink-`, `deepseek-`, `sonic` ditambahkan ke pola Makefile di Task 4, 5, dan 11; bandingkan model ElevenLabs multilingual lewat `strings.Contains(model, "multilingual_v2")`); nama model hanya dari config (`OPENAI_STT_MODEL`, `ELEVENLABS_STT_MODEL`, `CARTESIA_STT_MODEL`, `OPENAI_LLM_MODEL`, `DEEPSEEK_LLM_MODEL`, `OPENAI_EVALUATION_MODEL`, `DEEPSEEK_EVALUATION_MODEL`, `OPENAI_EMBEDDING_MODEL`)/`req.Model` (TTS: dari `characters.model_name`).
- Audio: STT in PCM16LE mono 16 kHz (OpenAI STT me-resample ke 24 kHz di dalam adapter); TTS out PCM16LE mono 24 kHz, panjang chunk genap (byte ganjil di batas chunk ditahan); Cancel ctx → tutup koneksi ≤ 50 ms, tidak ada event/chunk sesudahnya (§8.2.3/8.2.4).
- Vendor dan sakelar (D-017): STT = `OPENAI` (`OPENAI_STT_ENABLED`, default), `ELEVENLABS` (`ELEVENLABS_STT_ENABLED`), `CARTESIA` (`CARTESIA_STT_ENABLED`), **tepat satu `true`**; LLM + Evaluation = `OPENAI` (`OPENAI_LLM_ENABLED`, default) atau `DEEPSEEK` (`DEEPSEEK_LLM_ENABLED`), **tepat satu `true`**; Embedding selalu `OPENAI`; TTS = `ELEVENLABS_TTS_ENABLED` dan `CARTESIA_TTS_ENABLED` (default keduanya `true`, minimal satu). **Tidak ada fallback vendor otomatis untuk STT/LLM/Evaluation/Embedding**: "backup" = vendor lain dipilih manual (ubah env + restart, runbook R10 M10). Dihapus dari seluruh plan: adapter/task/test/fixture Deepgram, Gemini, OpenAI TTS; `PROVIDER_STT_*`/`PROVIDER_LLM_*`/`PROVIDER_PROFILE_*`/`PROVIDER_TTS_*`; STT replay buffer lintas vendor, `PROVIDER_SWITCHED`, `StartWith`, `ResilientSTT`, evaluasi "fallback provider". Yang dipertahankan: breaker per (vendor, kapabilitas), timeout/retry, rotasi koneksi vendor yang sama (`rotatingSTT`, §8.5.8), retry async, repair JSON (maks 2 panggilan LLM per delivery), dan fallback TTS opsional per karakter (`voice.fallback`).
- TTS (D-014, D-015): vendor/model/voice dipilih **per karakter**, bukan oleh profile/env global; tidak ada `PROVIDER_PROFILE_*`, `PROVIDER_TTS_PRIMARY/_FALLBACK`, atau fallback TTS global. Adapter hanya membaca `req.VoiceID`, `req.Model` (apa adanya dari `characters.model_name`; kosong/tidak dikenal → vendor mengembalikan `INVALID_REQUEST`, adapter tidak menebak dan tidak menormalisasi), `req.SpeakingRate`, `req.Text`, `req.PreviousText`, `req.Language` — diisi router (M4, §8.4.12) dari `sessions.config_snapshot.character` (`provider_name`, `model_name`, `voice_id`, `voice.speaking_rate`, `voice.fallback?`); tanpa ekspresi, gaya, katalog, atau parameter vendor di data konten (ElevenLabs hanya `voice_settings.speed`; Cartesia hanya `generation_config.speed`, tanpa `volume`/`emotion`). Vendor TTS valid: `ELEVENLABS`, `CARTESIA`; "terkonfigurasi" = `*_TTS_ENABLED=true` + API key di mode `real`; karakter yang `provider_name`-nya tidak enabled ditolak resolver/validator (`CONTENT_MISCONFIGURED` / `CHARACTER_NOT_AVAILABLE`).
- STT: tepat satu `FINAL{FromFinalize:true}` per `Finalize()`; `Events()` buffered 64, PARTIAL tertua dibuang saat penuh, FINAL/ERROR/CLOSED tidak pernah dibuang; `SendAudio` buffer internal ≤ 2 s audio (penuh → `*Error{KindUnavailable}`); `Close` menunggu ≤ 500 ms, idempotent. Sinyal rotasi (vendor menutup sesi karena batas durasi/idle) = `STTEvent{Type: STTError, Err: &provider.Error{Kind: KindUnavailable, VendorCode: <kode persis>}}` lalu `CLOSED`, dikenali `rotatingSTT` (§8.5.8) yang membuka koneksi baru ke vendor yang sama dan me-replay dari rotation buffer 15 s; selain sinyal itu `ERROR` diteruskan apa adanya (runtime: `PROVIDER_FAILURE`), tanpa pindah vendor.
- Konstanta vendor (§8.4): ElevenLabs STT batching 2×20 ms=1.280 byte, silence keep-alive 100 ms setelah 5 s sepi, FINAL sintetis 400 ms; OpenAI STT batching 2×20 ms (1.280 byte di 16 kHz → 1.920 byte di 24 kHz), FINAL sintetis 400 ms, tidak mengirim commit bila tidak ada audio sejak commit terakhir; Cartesia STT batching 5×20 ms=3.200 byte per pesan; ElevenLabs `speed` 0,7–1,2, Cartesia 0,6–1,5 (verifikasi di Task 11; adapter meng-clamp `req.SpeakingRate`; `0` → 1,0); embedding 1536 dim; DeepSeek `thinking:{"type":"disabled"}` default (`DEEPSEEK_LLM_THINKING`/`DEEPSEEK_EVALUATION_THINKING`), OPENING turn user konstan `「会話を始めてください。」` hanya bila hasil Step 0 Task 5 menyatakan request tanpa pesan non-system ditolak.
- Gate benchmark (§8.10.4; satu set gate karena tidak ada fallback vendor STT/LLM): STT final ≤ 300 ms, CER native/learner ≤ 8%/15%; LLM time-to-first-sentence ≤ 800 ms, format ≥ 97%, kualitas ≥ 4,0, JSON lolos skema setelah ≤ 1 repair ≥ 98%; TTS first audio ≤ 400 ms, MOS ≥ 4,0 dan kesalahan kanji ≤ 3% (voice utama karakter), `voice.fallback` MOS ≥ 3,5 / kesalahan ≤ 5% / first audio ≤ 600 ms; error rate pada konkurensi 100 ≤ 1%. Rekomendasi (bukan keputusan): STT = lolos semua gate dengan CER terendah; LLM = kualitas manusia tertinggi yang lolos gate dengan biaya per menit ≤ batas pemilik produk (default = ≤ 3× kandidat lolos termurah, D-016); TTS per karakter = MOS tertinggi yang lolos. Gate tidak boleh diturunkan tanpa update D-100.
- Fixture recorded: `internal/provider/<vendor>/testdata/recordings/<cap>_<case>.jsonl` (§14.6 menang atas §8.11), tidak boleh berisi secret/PII; format JSONL milik Task 1. Fixture awal disalin dari contoh payload dokumentasi vendor (URL di spec §8.4) dengan `meta.source=<URL>`, `meta.verified=false`; diganti rekaman nyata (`-record`, MANUAL GATE Task 16) → `verified=true`. Jangan mengarang field di luar dokumentasi/rekaman.
- Setiap butir spec bertanda **"verifikasi di task adapter"** diperiksa di Step 0 task adapternya (buka URL dokumentasi di spec §8.4.x), hasilnya (ya/tidak + kutipan field) dicatat di `docs/implementation-log.md`; bila berbeda dari spec, perbarui spec bab 8 dan catat di `docs/implementation-deviations.md` sebelum menulis fixture.
- Harga di `prices.yaml`, voice ID, dan model final HANYA dari hasil benchmark/riset resmi (tidak dikarang).

## Review Focus

(Master Review Focus #5 sisi adapter + kondisi tersirat; tiap butir punya test di task pemilik.)

1. **STT final kosong tidak menggantung / tidak ganda (master #5):** tiap `Finalize()` → tepat satu `FINAL{FromFinalize:true}` (sintetis 400 ms bila vendor diam, teks boleh kosong); FINAL kosong tanpa `from_finalize` tidak diteruskan — `TestOpenAISTTFinalizeSyntheticEmptyFinal` (Task 4), `TestELSTTFinalizeSyntheticEmptyFinal` (Task 8), `TestCartesiaSTTFinalizeFlushDone` (Task 10), `STTSuite` (Task 13).
2. **LLM diblokir content filter tidak berpindah vendor (master #5, D-017):** OpenAI/DeepSeek `finish_reason=content_filter` → `DONE{CONTENT_FILTERED}`, HTTP 400 `content_policy_violation` → `KindContentFiltered`, `refusal` → `KindContentFiltered`; generation/turn `FAILED` + APOLOGY tanpa fallback — `TestOpenAIContentFilter*` (Task 2–3), `TestDeepSeekContentFilter*` (Task 5–6).
3. **Cancel/barge-in menutup stream ≤ 50 ms tanpa goroutine bocor atau chunk lama (master #3 sisi adapter):** `Test<Vendor><Cap>CancelWithin50ms` (Task 2, 4, 5, 7, 8, 10, 11) + `TestNoGoroutineLeakAfterCancel` semua adapter (Task 13).
4. **Rahasia tidak bocor:** API key tidak ada di log, `Error.Message`, span, dan rekaman — `TestRecorderRedactsSecrets` (Task 1), `TestNoSecretInLogsOrErrors` semua adapter (Task 13).
5. **Real mode tidak boleh start dengan konfigurasi yang pasti gagal di tengah sesi:** nol/lebih dari satu STT atau LLM `true`, kedua TTS `false`, kunci vendor enabled kosong, model vendor enabled kosong, dan `characters.voice_id`/`model_name` berawalan `PLACEHOLDER_` (primary) atau `voice.fallback` placeholder untuk karakter ACTIVE yang vendor TTS-nya enabled ditolak — `TestValidateRealExactlyOneSTT`, `TestValidateRealExactlyOneLLM`, `TestValidateRealRejectsMissingKeys`, `TestPlaceholderVoiceRejectedAtStartup` (Task 15).
6. **TTS per karakter, tanpa profile TTS global (D-015):** `providerwire.Build` membangun semua adapter TTS enabled sehingga dua karakter berbeda vendor berjalan bersamaan dalam satu proses; benchmark & checklist voice per karakter — `TestBuildAllEnabledTTSAdapters` (Task 14), `RecommendCharacterVoices` (Task 18), checklist Task 23, adapter Cartesia (Task 11).
7. **DeepSeek tanpa `json_schema` strict (D-017):** skema ditegakkan di sisi kode (pemanggil M7/M8) + repair; adapter menyuntikkan instruksi JSON + skema + kerangka contoh, menolak konten kosong/JSON tidak valid sebagai `INVALID_RESPONSE` retryable, dan benchmark mengukur `% JSON lolos pada panggilan pertama` — `TestDeepSeekEvalInjectsSchemaInstruction`, `TestDeepSeekEvalEmptyContentIsInvalidResponse` (Task 6), `RunLLMJSON` (Task 19).

## File Structure

```text
internal/testutil/replay/{script.go,httpsim.go,wssim.go,recorder.go}   # simulator vendor dari fixture JSONL (T1)
internal/testutil/live.go                                              # RequireEnv (skip tanpa kunci) (T1)
internal/provider/httpx/span.go                                        # StartSpan helper adapter (T1)
internal/provider/openai/{config,client,llm,evaluation,embedding,stt,stt_events}.go + testdata/recordings/   # T2, T3, T4
internal/provider/deepseek/{config,client,llm,evaluation,schema}.go + testdata/recordings/        # T5, T6
internal/provider/elevenlabs/{config,tts,stt,stt_events}.go + testdata/recordings/                # T7, T8, T9
internal/provider/cartesia/{config,stt,stt_events,tts,pool,errors}.go + testdata/recordings/ + live_test.go (live)   # T10, T11
internal/provider/*/conformance_test.go, internal/provider/adapters_test.go     # providertest + aturan impor (T13; T10/T11 menambah cartesia)
internal/provider/*/live_test.go (tag live)                                                       # T16
cmd/internal/providerwire/{build.go,validate.go}  (fake.go milik M5)                                 # T14 (build), T15 (validate)
internal/content/voicecheck.go                                                                    # T15 (VoiceChecker, ValidateVoices)
internal/postgres/voicecheck_repo.go + db/queries/voicecheck.sql                                  # T15
cmd/realtime, cmd/worker (ubah wiring); cmd/api (validasi voice)                                  # T15
test/e2e/real/{run.sh,real_test.go}; Makefile (e2e-real, bench)                                   # T17
cmd/kotoki-cli/{provider_bench.go,provider_report.go}; cmd/kotoki-cli/internal/{bench,report}/       # T18–T20
testdata/benchmark/manifest.jsonl, bench/models.yaml, bench/README (format); docs/provider-benchmark.md; deploy/config/provider.env (rekomendasi sakelar/model STT/LLM/evaluation + concurrency; keputusan manual); bench/results/<tanggal>/character-voices.json; seed/env/<env>/0900_voice_ids.sql (UPDATE kolom characters)   # T21–T23 (manual)
```

---

### Task 1: Simulator vendor `replay`, recorder, `RequireEnv`, `httpx.StartSpan`

**Files:**
- Create: `internal/testutil/replay/{script.go,httpsim.go,wssim.go,recorder.go}`, `internal/testutil/live.go`, `internal/provider/httpx/span.go`
- Test: `internal/testutil/replay/replay_test.go`, `internal/testutil/live_test.go`, `internal/provider/httpx/span_test.go`

**Interfaces:**
- Consumes: M1 `testutil.TestdataPath`, `testutil.NewTraceRecorder`; M1 `otel.Start/End`; M4 `provider.Name/Capability`.
- Produces (nama dari plan; format fixture milik task ini):
  - Fixture JSONL satu objek per baris. Baris 1 wajib `{"meta":{"vendor","capability","source","verified":bool}}`. HTTP: `{"http":{"expect":{"method","path","query":{k:v},"headers":{k:v|"*"},"json":<subset>},"respond":{"status","headers":{},"chunks":[{"at_ms":N,"text":"…"}|{"at_ms":N,"b64":"…"}]},"repeat":bool}}` (urut; `repeat` = layani tanpa batas). WS: `{"ws_upgrade":{"path","query","headers"}}` lalu `{"ws":{"dir":"send","at_ms":N,"text":"…"|"b64":"…"}}` (server→client), `{"ws":{"dir":"recv","json":<subset>}}` (server mengharapkan pesan klien yang memuat subset; nilai `"*"` = apa saja; `"binary":true` untuk frame biner dengan `"len":N`), `{"ws":{"dir":"on","match":<subset>|{"binary":true},"send":[…],"times":0}}` (aturan reaktif; `times 0` = tanpa batas), `{"ws":{"dir":"close","code":N}}`.
  - `replay.RecordingsDir(t testing.TB, vendor string) string` (= `internal/provider/<vendor>/testdata/recordings`); `replay.LoadScript(t testing.TB, dir, name string) *Script`; `replay.LoadScriptString(t testing.TB, jsonl string) *Script` (hanya test); `replay.NewHTTPServer(t, s *Script) *Server`; `replay.NewWSServer(t, s *Script) *Server`; `(*Server).URL string` (`http://…` / `ws://…`); `(*Server).Requests() []Request{Method,Path,Query,Header,Body}`; `(*Server).ClientMessages() []Message{Text bool; Data []byte; At time.Time}`; `(*Server).UpgradeRequest() Request` (WS: path/query/header handshake); `(*Server).AssertDone()` (semua baris non-`repeat` terpakai; mismatch `expect` → `t.Errorf` menyebut baris & path JSON).
  - `replay.NewRecorder(t, vendor, capability, path string) *Recorder` dengan `(*Recorder).Transport(base http.RoundTripper) http.RoundTripper`, `(*Recorder).Tap(dir string, text bool, data []byte)` (dipasang ke `Config.Tap` adapter WS) dan `Flush()` menulis JSONL; header `Authorization, xi-api-key, X-API-Key, Ocp-Apim-Subscription-Key` dan query `key=` dihapus (§14.6).
  - `testutil.RequireEnv(t testing.TB, keys ...string) map[string]string` (`t.Skip` bila ada yang kosong; satu-satunya tempat test live membaca env).
  - `httpx.StartSpan(ctx, c provider.Capability, n provider.Name, model string) (context.Context, *Span)`; `(*Span).End(outcome string, err error)` (outcome `success|error|cancelled|truncated`; `provider.error_kind` = `provider.KindOf(err)`).

- [ ] **Step 1: Write the failing test**
```go
func TestHTTPSimMatchesSubsetAndStreams(t *testing.T) {
	s := replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"LLM","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/v1/chat/completions","json":{"stream":true,"model":"*"}},"respond":{"status":200,"chunks":[{"at_ms":0,"text":"data: {\"a\":1}\n\n"},{"at_ms":5,"text":"data: [DONE]\n\n"}]}}}`)
	srv := replay.NewHTTPServer(t, s)
	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true,"model":"m","extra":1}`))
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n", string(b)); srv.AssertDone()
}
func TestRecorderRedactsSecrets(t *testing.T) { /* RoundTrip dengan header xi-api-key & query key=SECRET → file hasil tidak memuat "SECRET" */ }
func TestRequireEnvSkips(t *testing.T) { /* subtest tanpa env → t.Skipped() true */ }
```
Tambah: `TestWSSimReactiveRule` (aturan `on` dengan `match:{"commit":true}` membalas `send` setiap kali; `ClientMessages` mencatat urutan), `TestWSSimMismatchFailsTest` (pakai `testing.TB` palsu perekam `Errorf`), `TestSpanAttributes` (`NewTraceRecorder`: span `provider.LLM` memuat `provider.name=OPENAI`, `provider.outcome=error`, `provider.error_kind=RATE_LIMITED`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/testutil/... ./internal/provider/httpx/ -race -run 'HTTPSim|Recorder|RequireEnv|WSSim|Span'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** HTTP sim: `httptest.Server`, handler mencocokkan permintaan ke baris `http` berikutnya, subset-JSON rekursif (`"*"` wildcard), menulis `chunks` dengan `Flush()` menurut `at_ms` relatif awal respons. WS sim: `httptest` + `websocket.Accept` (`coder/websocket`), memeriksa `ws_upgrade` (path/query/header), lalu memproses baris berurutan; `on` aktif sebagai tambahan saat menunggu `recv`. `LoadScriptString` hanya untuk test. Tidak ada `time.Sleep` selain penjadwalan `at_ms`.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `go vet ./internal/testutil/... ./internal/provider/httpx/`.
- [ ] **Step 5: Commit** — `git add internal/testutil internal/provider/httpx && git commit -m "test(provider): simulator vendor replay, recorder, RequireEnv, span helper"`

---

### Task 2: OpenAI — `Config`, klien, LLM streaming (§8.4.3)

**Files:**
- Create: `internal/provider/openai/{config.go,client.go,llm.go}`, `internal/provider/openai/testdata/recordings/llm_{basic,length,content_filter,usage_missing,429_quota,400_content_policy,hang}.jsonl`
- Test: `internal/provider/openai/llm_test.go`

**Interfaces:**
- Consumes: Task 1 (`replay.*`, `httpx.StartSpan`); M4 `httpx.NewSSEReader/ClassifyHTTP/ClassifyError`, `provider.LLMProvider`, `provider.EstimateUsage` TIDAK ada → adapter menghitung estimasi sendiri (`ceil(rune semua Messages × 1,0)` §8.7.3; fungsi tak-ekspor `estimateTokens`).
- Produces: `openai.Config{APIKey, BaseURL string; OmitSampling bool; ReasoningEffort string; Tap func(dir string, text bool, data []byte)}` (Task 4 menambah `WSBaseURL`, `STTModel`, timer STT) (secret tidak pernah di-`String()`); `openai.NewLLM(cfg Config, hc *http.Client, log *slog.Logger) (*LLM, error)` (error bila `APIKey` atau `BaseURL` kosong); `(*LLM).Name() provider.Name == provider.OpenAI`; `Stream(ctx, provider.LLMRequest) (provider.LLMStream, error)`; helper paket `classify(resp, body) *provider.Error` + `(*client).publishRateLimit` (gauge `kotoki_provider_ratelimit_remaining{provider,capability,kind}` dari header `x-ratelimit-remaining-requests/-tokens`).

- [ ] **Step 0 (verifikasi di task adapter):** buka https://platform.openai.com/docs/api-reference/chat/create, /chat-streaming, /guides/rate-limits; catat: nama header rate-limit, parameter yang ditolak model reasoning (`temperature`/`stop`/`max_completion_tokens`), bentuk `error.code` `insufficient_quota` & `content_policy_violation`. Salin payload contoh ke fixture dengan `meta.source`.
- [ ] **Step 1: Write the failing test**
```go
func TestOpenAILLMStreamBasic(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScript(t, replay.RecordingsDir(t, "openai"), "llm_basic.jsonl"))
	l, _ := openai.NewLLM(openai.Config{APIKey: "sk-test", BaseURL: srv.URL}, httpx.NewClient(), slog.Default())
	st, err := l.Stream(ctx, provider.LLMRequest{Model: "m1", MaxTokens: 300, Temperature: 0.8, Stop: []string{"。。"},
		Messages: []provider.Message{{Role: provider.RoleSystem, Content: "S"}, {Role: provider.RoleUser, Content: "U"}}})
	require.NoError(t, err)
	evs := drain(st.Events())
	require.Equal(t, []string{"こんに", "ちは。"}, deltas(evs))
	done := evs[len(evs)-1]
	require.Equal(t, provider.LLMDone, done.Type); require.Equal(t, provider.FinishStop, done.FinishReason)
	require.Equal(t, provider.TokenUsage{InputTokens: 120, OutputTokens: 8}, *done.Usage)
	req := srv.Requests()[0]
	require.Equal(t, "Bearer sk-test", req.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"m1","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"stream":true,
	  "stream_options":{"include_usage":true},"max_completion_tokens":300,"temperature":0.8,"stop":["。。"]}`, string(req.Body))
}
```
Tambah: `TestOpenAILLMFinishLength` → `FinishMaxTokens`; `TestOpenAILLMContentFilterDone` (`finish_reason=content_filter` → `DONE{FinishContentFiltered}`, bukan ERROR; tanpa fallback vendor, D-017: generation `FAILED` → APOLOGY); `TestOpenAILLMUsageMissingIsEstimated` (tanpa chunk usage → `Usage.Estimated==true`, `InputTokens` = total rune pesan); `TestOpenAILLMStopOmittedWhenEmpty`; `TestOpenAILLMOmitSampling` (`OmitSampling=true` → body tanpa `temperature` & `stop`; `ReasoningEffort="minimal"` → `"reasoning_effort":"minimal"`); `TestOpenAILLMErrors` tabel: 401→`KindAuth`, 429 `insufficient_quota`→`KindRateLimited` `Retryable=false`, 429 biasa `Retry-After:3`→`RetryAfter=3s` retryable, 400 `content_policy_violation`→`KindContentFiltered`, 400 lain→`KindInvalidRequest`, 503→`KindUnavailable`, error dikembalikan sinkron dari `Stream` (bukan event); `Message` tidak memuat `sk-test`; `TestOpenAILLMCancelWithin50ms` (fixture `llm_hang`: kirim 1 delta lalu menggantung; cancel → `DONE{FinishCancelled}` dengan `Usage.Estimated` ≤ 50 ms, channel tertutup, `Close` idempotent); `TestOpenAILLMStreamBadJSONIsInvalidResponse` (event `ERROR` kind `INVALID_RESPONSE`); `TestOpenAILLMRateLimitGauge`; `TestOpenAILLMSpan`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/openai/ -race -run 'TestOpenAILLM'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `Stream`: POST `{BaseURL}/chat/completions`; status non-2xx → `httpx.ClassifyHTTP` lalu override §8.4.3 (`insufficient_quota`, `content_filter`/`content_policy_violation`); body diparse `httpx.NewSSEReader` pada goroutine yang membaca `ctx.Done()` dan menutup body (`resp.Body.Close()`); `delta.content` non-kosong → DELTA; simpan `finish_reason` hingga `[DONE]`; chunk dengan `choices:[]` + `usage` → usage. Channel `Events()` kapasitas 128; adapter MUST NOT mengubah teks DELTA.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/openai/ -race -count=3` → PASS; `make modelname-check` → exit 0.
- [ ] **Step 5: Commit** — `git add internal/provider/openai && git commit -m "feat(provider/openai): LLM streaming Chat Completions"`

---

### Task 3: OpenAI — Evaluation (structured output) dan Embedding (§8.4.8, §8.4.10)

**Files:**
- Create: `internal/provider/openai/{evaluation.go,embedding.go}`, `internal/provider/openai/testdata/recordings/{eval_basic,eval_refusal,eval_length,eval_invalid_json,embed_basic,embed_wrong_dim}.jsonl`
- Test: `internal/provider/openai/evaluation_test.go`, `embedding_test.go`

**Interfaces:**
- Consumes: Task 2 (`Config`, `client`, `classify`). Dipakai lewat `AsyncRouter.Evaluation()` (D-017: 1 attempt per panggilan, tanpa fallback vendor; urutan utama + repair (maks 2 panggilan per delivery) milik M7/M8); `OPENAI_EVALUATION_MODEL` diisi router.
- Produces: `openai.NewEvaluation(cfg Config, hc *http.Client, log *slog.Logger) (*Evaluation, error)` (`provider.EvaluationProvider`, `GenerateJSON`); `openai.NewEmbedding(...) (*Embedding, error)` (`provider.EmbeddingProvider`, `Embed`).

- [ ] **Step 0 (verifikasi di task adapter):** https://platform.openai.com/docs/guides/structured-outputs — keyword JSON Schema yang didukung strict mode & field `refusal`; https://developers.openai.com/api/docs/guides/embeddings — urutan `data[i].index`, `usage`.
- [ ] **Step 1: Write the failing test**
```go
func TestOpenAIEvalRequestAndResult(t *testing.T) { // fixture eval_basic
	res, err := ev.GenerateJSON(ctx, provider.StructuredRequest{Task: "EVALUATION", Model: "m", SchemaName: "kotoki_evaluation_rubric_v1",
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`),
		Messages: msgs, MaxOutputTokens: 800, Temperature: 0})
	require.NoError(t, err); require.JSONEq(t, `{"s":77}`, string(res.JSON))
	require.JSONEq(t, `{"type":"json_schema","json_schema":{"name":"kotoki_evaluation_rubric_v1","strict":true,"schema":{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}}}`, requestField(srv, "response_format"))
	require.Equal(t, provider.FinishStop, res.FinishReason); require.Equal(t, provider.OpenAI, res.Provider)
}
func TestOpenAIEmbedBodyAndOrder(t *testing.T) { // fixture embed_basic: respons index [1,0] → hasil diurutkan; body tanpa "dimensions"
	require.JSONEq(t, `{"model":"m","input":["a","b"],"encoding_format":"float"}`, string(srv.Requests()[0].Body)) }
```
Tambah: `TestOpenAIEvalTemperatureZeroSentNegativeDefaults02` (0 dikirim apa adanya; −1 → 0.2); `TestOpenAIEvalSeedSent` (`Seed` → `"seed"`); `TestOpenAIEvalRefusalIsContentFiltered` (`message.refusal` non-null → `KindContentFiltered`, `Retryable=false`); `TestOpenAIEvalLengthIsInvalidResponse` (`finish_reason=length` → `KindInvalidResponse`, `Retryable=true`); `TestOpenAIEvalInvalidJSON` (`json.Valid` gagal → `KindInvalidResponse`); `TestOpenAIEmbedWrongDimension` (vektor 1024 → `KindInvalidResponse`); `TestOpenAIEmbedUsage` (`InputTokens` dari `usage.prompt_tokens`, `OutputTokens==0`, `Provider==OPENAI`); `TestOpenAIEmbedRejectsOver64Texts` (65 teks → `KindInvalidRequest` tanpa request); `TestOpenAIEvalCancel`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/openai/ -race -run 'Eval|Embed'`. Expected: FAIL.
- [ ] **Step 3: Implement** sesuai tabel §8.4.8/§8.4.10; `Latency` = durasi request; error HTTP lewat `classify` yang sama dengan Task 2.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/openai/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/openai): evaluation JSON schema strict dan embedding"`

---

### Task 4: OpenAI — STT Realtime transcription (§8.4.2)

**Files:**
- Modify: `internal/provider/openai/config.go` (tambah `WSBaseURL`, `STTModel`, `SyntheticFinalDelay`, `KeepAliveAfter`), `Makefile` (pola `modelname-check` tidak berubah di task ini)
- Create: `internal/provider/openai/{stt.go,stt_events.go}`, `internal/provider/openai/testdata/recordings/stt_{basic,commit_empty,partial_cumulative,error_auth,error_quota,session_limit,drop}.jsonl`
- Test: `internal/provider/openai/stt_test.go`, `stt_errors_test.go`

**Interfaces:**
- Consumes: Task 1 (`replay.NewWSServer`), Task 2 (`Config`, `classify`); M4 `provider.STTProvider/STTSession/STTEvent`, `provider.NormalizeJA`, `provider.ResamplePCM16`.
- Produces: `openai.Config` ditambah `WSBaseURL string` (`OPENAI_WS_BASE_URL`, default `wss://api.openai.com/v1/realtime`), `STTModel string` (`OPENAI_STT_MODEL`; wajib non-kosong untuk `NewSTT`), `SyntheticFinalDelay, KeepAliveAfter time.Duration` (zero → 400 ms / sesuai Step 0); `openai.NewSTT(cfg Config, hc *http.Client, log *slog.Logger) (*STT, error)`; `(*STT).Name()==provider.OpenAI`, `Model()`, `Open(ctx, provider.STTSessionConfig) (provider.STTSession, error)`.

- [ ] **Step 0 (verifikasi di task adapter; hasil ya/tidak + kutipan field dicatat di `docs/implementation-log.md`, perbedaan dari spec → perbarui spec §8.4.2/§8.4.13 dan `docs/implementation-deviations.md` SEBELUM menulis fixture):** baca https://developers.openai.com/api/docs/guides/realtime-transcription, https://developers.openai.com/api/docs/guides/realtime-websocket dan Realtime API reference (https://developers.openai.com/api/reference). Putuskan & catat: (a) URL/query handshake sesi transkripsi (`?model=` vs `?intent=transcription`) dan perlunya header `OpenAI-Beta` (default: tidak dikirim); (b) nama model default (`gpt-live-transcribe`?) dan bentuk `transcription.languages` (array, model baru) vs `transcription.language` (model lama), kode `ja` diterima; (c) apakah `delta` datang sebelum `commit` pada `turn_detection=null` (menentukan ada/tidaknya PARTIAL saat user bicara; dicatat untuk benchmark `first_partial`); (d) urutan `completed` terhadap commit (`previous_item_id`), payload `session.created/updated`, `usage` pada `completed`; (e) batas durasi sesi + kode/pesan penutupan (→ sinyal rotasi `VendorCode`), keep-alive, ukuran maks `input_audio_buffer.append`; (f) apakah commit tanpa audio ditolak (→ adapter tidak mengirim commit dan langsung FINAL kosong); (g) kode `error` untuk auth, rate limit, `insufficient_quota`, parameter salah; (h) apakah replay backlog lebih cepat dari realtime diterima (rotasi §8.5.8; bila tidak, catat kebutuhan `PROVIDER_ROTATION_REPLAY_SPEED`). Tambah `ink-`/`gpt-` tidak perlu ke pola `modelname-check` (sudah ada `gpt-`).
- [ ] **Step 1: Write the failing test**
```go
func TestOpenAISTTOpenHandshakeAndSessionUpdate(t *testing.T) { // fixture stt_basic; Open berhasil setelah konfirmasi sesi
	stt, _ := openai.NewSTT(openai.Config{APIKey: "sk-test", WSBaseURL: srv.URL, STTModel: "m_stt"}, httpx.NewClient(), slog.Default())
	sess, err := stt.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: 16000}); require.NoError(t, err); defer sess.Close()
	u := srv.UpgradeRequest(); require.Equal(t, "Bearer sk-test", u.Header.Get("Authorization")); require.Empty(t, u.Header.Get("OpenAI-Beta"))   // path/query mengikuti Step 0(a)
	require.JSONEq(t, `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"m_stt","languages":["ja"]},"turn_detection":null}}}}`, string(srv.ClientMessages()[0].Data)) // bentuk languages/language mengikuti Step 0(b)
}
func TestOpenAISTTResamplesBatchesAndCommits(t *testing.T) {
	sess := openSession(t)
	require.NoError(t, sess.SendAudio(make([]byte, 640))); require.NoError(t, sess.SendAudio(make([]byte, 640))); require.NoError(t, sess.SendAudio(make([]byte, 640)))
	require.NoError(t, sess.Finalize())
	msgs := appendMsgs(srv)                       // 2 frame 20 ms @16 kHz → 1 pesan input_audio_buffer.append berisi 1.920 byte PCM16 @24 kHz; sisa 640 byte → 960 byte @24 kHz
	require.Len(t, msgs, 2); require.Equal(t, 1920, b64len(msgs[0])); require.Equal(t, 960, b64len(msgs[1]))
	require.Equal(t, "input_audio_buffer.commit", lastClientType(srv))
	ev := waitFinal(t, sess); require.True(t, ev.FromFinalize); require.Equal(t, "日本語の勉強", ev.Text)
}
```
Tambah: `TestOpenAISTTPartialIsCumulativePerItem` (beberapa `...transcription.delta{item_id,delta}` pada item sama → `PARTIAL{Text: NormalizeJA(buffer lengkap), Confidence: nil}`; item baru mereset buffer; `StartMs/EndMs` dari posisi audio terkirim byte/32 pada sampel 16 kHz asli, bukan 24 kHz); **`TestOpenAISTTFinalizeSyntheticEmptyFinal`** (fixture `commit_empty`: commit terkirim, vendor diam; setelah `SyntheticFinalDelay`=30 ms di test → tepat satu `FINAL{"",FromFinalize:true}`; 3× Finalize beruntun → 3 FINAL); `TestOpenAISTTFinalizeWithoutAudioSkipsCommit` (tidak ada audio sejak commit terakhir → tidak ada pesan commit, langsung `FINAL{"",FromFinalize:true}`); `TestOpenAISTTCompletedWithoutPendingIsPlainFinal` (`completed` tanpa Finalize pending → `FINAL{FromFinalize:false}`; `pendingCommits` tidak negatif); `TestOpenAISTTCompletedOrderFollowsCommit` (dua commit, `completed` datang terbalik → FINAL keluar berurutan commit, Step 0(d)); `TestOpenAISTTKeytermsMapped` (`Keyterms` → `keywords`/`prompt` bila didukung, diabaikan bila tidak, Step 0); `TestOpenAISTTOpenFailsWithoutSessionConfirmation` (tidak ada konfirmasi dalam `ctx` 100 ms → `KindTimeout`); `TestOpenAISTTOpenAuthFailure` (401 pada upgrade → `*Error{KindAuth}`); `TestOpenAISTTSendAudioOddLengthRejected` (→ `KindInvalidRequest`); `TestOpenAISTTUsageAudioMsSent` (`Stats().AudioMsSent` = byte terkirim/32 sampel 16 kHz asli).
`stt_errors_test.go`: `TestOpenAISTTErrorMapping` (tabel `error` → auth `KindAuth`; rate limit `KindRateLimited` retryable; `insufficient_quota` → `KindRateLimited` `Retryable=false`; parameter/event salah `KindInvalidRequest`; lainnya `KindUnavailable`; `VendorCode` = kode vendor persis); `TestOpenAISTTSessionLimitIsRotationSignal` (fixture `session_limit`: penutupan karena batas durasi → `ERROR{KindUnavailable, VendorCode:<kode Step 0(e)>}` lalu `CLOSED`, bukan kind AUTH/RATE_LIMITED); `TestOpenAISTTSocketDropIsUnavailable` (close 1006 tanpa `Close()` kita → `ERROR{UNAVAILABLE}` lalu `CLOSED`); `TestOpenAISTTKeepAliveWhenIdle` (sesuai Step 0(e); bila vendor tidak butuh, test dihapus dan dicatat); `TestOpenAISTTSendAudioBufferFull` (> 2 s audio antre → `*Error{KindUnavailable}` tanpa blok); `TestOpenAISTTSendAfterClose` (`errors.Is(err, provider.ErrSessionClosed)`); `TestOpenAISTTCloseIdempotentWithin500ms`; `TestOpenAISTTEventsDropOldestPartialNeverFinal`; `TestOpenAISTTCancelWithin50ms` (cancel ctx saat `Open` menggantung); `TestOpenAISTTKeyNotLeaked`; `TestOpenAISTTSpan`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/openai/ -race -run TestOpenAISTT`. Expected: FAIL.
- [ ] **Step 3: Implement** `Open`: `websocket.Dial` ke `{WSBaseURL}` (query per Step 0(a)) dengan `Authorization: Bearer`, kirim `session.update`, tunggu konfirmasi (`session.updated`, atau handshake 101 + `session.created` bila tidak ada konfirmasi); goroutine baca → `stt_events.go` memetakan event (tabel §8.4.2); goroutine tulis: batching 2 frame 20 ms, `provider.ResamplePCM16` 16→24 kHz per batch (state resampler dijaga lintas batch agar tanpa klik di batas), `input_audio_buffer.append` base64; state `pendingCommits`, `segStartMs`, `audioSentMs` (dari sampel 16 kHz), buffer delta per `item_id`, antrean urutan commit; timer FINAL sintetis per `Finalize`; `Events()` kapasitas 64 dengan kebijakan drop-PARTIAL-tertua; `Config.Tap` dipanggil untuk setiap frame (untuk `-record`). Tidak ada pindah vendor: error/penutupan non-rotasi diteruskan sebagai `ERROR`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/openai/ -race -count=3` → PASS; `make modelname-check` → exit 0.
- [ ] **Step 5: Commit** — `git add internal/provider/openai && git commit -m "feat(provider/openai): STT realtime transcription, resample 24 kHz, FINAL sintetis"`

---

### Task 5: DeepSeek — `Config`, klien, LLM streaming (§8.4.4)

**Files:**
- Create: `internal/provider/deepseek/{config.go,client.go,llm.go}`, `internal/provider/deepseek/testdata/recordings/llm_{basic,length,content_filter,insufficient_resource,usage_last_chunk,keepalive_comment,reasoning_content,system_only,401,402_balance,429,400,503,hang}.jsonl`
- Modify: `Makefile` (pola `modelname-check` + `deepseek-`)
- Test: `internal/provider/deepseek/llm_test.go`

**Interfaces:**
- Consumes: Task 1 (`replay.*`, `httpx.StartSpan`); M4 `httpx.NewSSEReader/ClassifyHTTP/ClassifyError`, `provider.LLMProvider`. Paket ini TIDAK mengimpor `internal/provider/openai` (kemiripan Chat Completions diduplikasi kecil, `estimateTokens` tak-ekspor sendiri).
- Produces: `deepseek.Config{APIKey, BaseURL string; Thinking string /* "disabled"|"enabled"; default "disabled" */; Tap func(dir string, text bool, data []byte)}` (secret tidak pernah di-`String()`); `deepseek.NewLLM(cfg Config, hc *http.Client, log *slog.Logger) (*LLM, error)` (error bila `APIKey`/`BaseURL` kosong); `(*LLM).Name() == provider.DeepSeek`; `Stream(ctx, provider.LLMRequest) (provider.LLMStream, error)`; helper paket `classify(resp, body) *provider.Error`.

- [ ] **Step 0 (verifikasi di task adapter; hasil dicatat di `docs/implementation-log.md`, perbedaan → perbarui spec §8.4.4/§8.4.13 + `docs/implementation-deviations.md` sebelum menulis fixture):** buka https://api-docs.deepseek.com/ , /api/create-chat-completion , /guides/thinking_mode , /guides/json_mode , /quick_start/pricing , /quick_start/error_codes , /quick_start/rate_limit. Periksa: (a) base URL `https://api.deepseek.com` + path `/chat/completions` dan header `Authorization: Bearer`; (b) nama model sah per hari ini (`deepseek-flash`, `deepseek-v4-pro`; status `deepseek-v4-flash` retired) — nilai awal `DEEPSEEK_LLM_MODEL`; (c) `max_tokens` (bukan `max_completion_tokens`), `stop` ≤ 16 item, `thinking:{"type":"disabled"}` diterima dan membuat `temperature` berlaku; (d) bentuk `usage` pada chunk konten terakhir (tidak ada chunk usage terpisah) dan `prompt_cache_hit_tokens`; (e) komentar SSE `: keep-alive`, `finish_reason` `insufficient_system_resource`/`aborted`; (f) kode HTTP 402 (saldo habis), 429 (batas konkurensi akun), 400/422; (g) apakah request tanpa pesan non-system (generation `OPENING`) diterima — bila tidak, adapter menambahkan satu giliran `user` konstanta `「会話を始めてください。」` (konstanta tak-ekspor `openingTurn`); (h) apakah `reasoning_content` muncul walau `thinking` disabled (diabaikan).
- [ ] **Step 1: Write the failing test**
```go
func TestDeepSeekLLMStreamBasic(t *testing.T) { // fixture llm_basic
	srv := replay.NewHTTPServer(t, replay.LoadScript(t, replay.RecordingsDir(t, "deepseek"), "llm_basic.jsonl"))
	l, _ := deepseek.NewLLM(deepseek.Config{APIKey: "dk-test", BaseURL: srv.URL}, httpx.NewClient(), slog.Default())
	st, err := l.Stream(ctx, provider.LLMRequest{Model: "m1", MaxTokens: 300, Temperature: 0.8, Stop: []string{"。。"},
		Messages: []provider.Message{{Role: provider.RoleSystem, Content: "S"}, {Role: provider.RoleUser, Content: "U"}}})
	require.NoError(t, err)
	evs := drain(st.Events()); require.Equal(t, []string{"こんに", "ちは。"}, deltas(evs))
	done := evs[len(evs)-1]; require.Equal(t, provider.LLMDone, done.Type); require.Equal(t, provider.FinishStop, done.FinishReason)
	require.Equal(t, provider.TokenUsage{InputTokens: 120, OutputTokens: 8, CachedInputTokens: 100}, *done.Usage)
	req := srv.Requests()[0]; require.Equal(t, "/chat/completions", req.Path); require.Equal(t, "Bearer dk-test", req.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"m1","messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}],"stream":true,
	  "stream_options":{"include_usage":true},"max_tokens":300,"temperature":0.8,"stop":["。。"],"thinking":{"type":"disabled"}}`, string(req.Body))
}
```
Tambah: `TestDeepSeekLLMUsesMaxTokensNotMaxCompletionTokens`; `TestDeepSeekLLMStopOmittedWhenEmpty`; `TestDeepSeekLLMThinkingEnabledConfig` (`Thinking:"enabled"` → body tanpa `thinking` atau `{"type":"enabled"}` sesuai Step 0(c)); `TestDeepSeekLLMUsageOnLastContentChunk` (fixture `usage_last_chunk`: setiap chunk punya `usage:null` kecuali yang terakhir → `DONE.Usage` terisi, `CachedInputTokens=prompt_cache_hit_tokens`, `Estimated==false`); `TestDeepSeekLLMUsageMissingIsEstimated`; `TestDeepSeekLLMIgnoresKeepAliveComment` (baris `: keep-alive` tidak menghasilkan event); `TestDeepSeekLLMIgnoresReasoningContent` (`delta.reasoning_content` tidak pernah menjadi `DELTA`); `TestDeepSeekLLMFinishReasons` (`stop`→`FinishStop`; `length`→`FinishMaxTokens`; `content_filter`→`DONE{FinishContentFiltered}` bukan ERROR; `insufficient_system_resource`/`aborted` → `ERROR{KindUnavailable}`); `TestDeepSeekLLMSystemOnlyRequest` (fixture `system_only`; perilaku sesuai Step 0(g): kirim apa adanya, atau tambah `openingTurn` user); `TestDeepSeekLLMErrors` tabel: 401/403→`KindAuth`; 402→`KindAuth` `Retryable=false` dengan `VendorCode` mentah; 429→`KindRateLimited` retryable (`Retry-After` dihormati bila ada); 400/422→`KindInvalidRequest`; 500/503→`KindUnavailable`; dikembalikan sinkron dari `Stream`; `Message` tidak memuat `dk-test`; `TestDeepSeekLLMCancelWithin50ms` (fixture `hang`: satu delta lalu menggantung; cancel → `DONE{FinishCancelled}` dengan `Usage.Estimated`, channel tertutup ≤ 50 ms, `Close` idempotent); `TestDeepSeekLLMStreamBadJSONIsInvalidResponse`; `TestDeepSeekLLMSpan`; `TestDeepSeekLLMAPIKeyNotInLogsOrErrors`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/deepseek/ -race -run TestDeepSeekLLM`. Expected: FAIL (paket belum ada).
- [ ] **Step 3: Implement** `Stream`: POST `{BaseURL}/chat/completions`; status non-2xx → `httpx.ClassifyHTTP` lalu override §8.4.4 (402); body diparse `httpx.NewSSEReader` pada goroutine yang membaca `ctx.Done()` dan menutup body; `delta.content` non-kosong → DELTA; `finish_reason` disimpan hingga `[DONE]`; `usage` dari chunk terakhir yang memuatnya; channel `Events()` kapasitas 128; adapter MUST NOT mengubah teks DELTA. Tidak ada retry/fallback di adapter (router mengatur timeout/breaker; kegagalan = generation `FAILED` → APOLOGY). Tambah `deepseek-` ke pola `make modelname-check`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/deepseek/ -race -count=3` → PASS; `make modelname-check` → exit 0.
- [ ] **Step 5: Commit** — `git add internal/provider/deepseek Makefile && git commit -m "feat(provider/deepseek): LLM streaming Chat Completions kompatibel"`

---

### Task 6: DeepSeek — Evaluation JSON mode tanpa skema strict (§8.4.9)

**Files:**
- Create: `internal/provider/deepseek/{evaluation.go,schema.go}`, `internal/provider/deepseek/testdata/recordings/eval_{basic,empty_content,length,invalid_json,content_filter,401}.jsonl`
- Modify: `internal/provider/deepseek/config.go` (tambah `EvaluationThinking`)
- Test: `internal/provider/deepseek/evaluation_test.go`, `schema_test.go`

**Interfaces:**
- Consumes: Task 5 (`Config`, `client`, `classify`).
- Produces: `deepseek.Config.EvaluationThinking string` (`DEEPSEEK_EVALUATION_THINKING`, default `"disabled"`); `deepseek.NewEvaluation(cfg Config, hc *http.Client, log *slog.Logger) (*Evaluation, error)` (`provider.EvaluationProvider`, `GenerateJSON`; 1 attempt per panggilan, dipakai lewat `AsyncRouter.Evaluation()`); fungsi tak-ekspor `schemaInstruction(schema json.RawMessage, schemaName string) (string, error)` (instruksi "Keluarkan **JSON** saja" + teks skema + kerangka kosong yang dibangkitkan dari skema) dan `skeleton(schema json.RawMessage) (json.RawMessage, error)`.

- [ ] **Step 0 (verifikasi di task adapter):** https://api-docs.deepseek.com/guides/json_mode — `response_format:{"type":"json_object"}` satu-satunya mode JSON (tidak ada `json_schema`/`strict`), syarat kata "json" di prompt + contoh format, perilaku konten kosong sesekali, `max_tokens` yang cukup; apakah `seed` didukung (bila tidak, `StructuredRequest.Seed` tidak dikirim); `temperature` berlaku saat `thinking` disabled; nama model evaluasi (`deepseek-v4-pro`, §8.5.1).
- [ ] **Step 1: Write the failing test**
```go
func TestDeepSeekEvalRequestAndResult(t *testing.T) { // fixture eval_basic
	res, err := ev.GenerateJSON(ctx, provider.StructuredRequest{Task: "EVALUATION", Model: "m", SchemaName: "kotoki_evaluation_rubric_v1",
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`),
		Messages: msgs, MaxOutputTokens: 800, Temperature: 0})
	require.NoError(t, err); require.JSONEq(t, `{"s":77}`, string(res.JSON)); require.Equal(t, provider.DeepSeek, res.Provider)
	r := srv.Requests()[0]; require.Equal(t, "/chat/completions", r.Path)
	require.JSONEq(t, `{"type":"json_object"}`, requestField(srv, "response_format"))
	require.JSONEq(t, `{"type":"disabled"}`, requestField(srv, "thinking")); require.Contains(t, string(r.Body), `"max_tokens":800`)
	require.NotContains(t, string(r.Body), "json_schema"); require.NotContains(t, string(r.Body), "strict")
}
func TestDeepSeekEvalInjectsSchemaInstruction(t *testing.T) { /* pesan system terakhir di blok system berisi kata "JSON", teks JSONSchema persis, dan kerangka kosong {"s":0}; pesan lain tidak berubah urutannya */ }
```
`schema_test.go`: `TestSkeletonFromSchema` (object → properti `required` terisi nilai nol per tipe: integer `0`, string `""`, boolean `false`, array `[]`, `["string","null"]` → `null`, nested object rekursif; `enum` → elemen pertama); `TestSchemaInstructionContainsJSONWord`.
Tambah: `TestDeepSeekEvalEmptyContentIsInvalidResponse` (fixture `empty_content`: `content:""` → `KindInvalidResponse`, `Retryable=true`); `TestDeepSeekEvalInvalidJSON` (`json.Valid` gagal → `KindInvalidResponse` retryable); `TestDeepSeekEvalLengthIsInvalidResponse` (`finish_reason=length` → `KindInvalidResponse` retryable); `TestDeepSeekEvalContentFilteredNotRetryable` (→ `KindContentFiltered`, `Retryable=false`); `TestDeepSeekEvalDoesNotValidateSchema` (JSON valid tetapi melanggar skema → adapter MENGEMBALIKANNYA apa adanya; validasi skema ketat adalah kewajiban pemanggil M7/M8 + repair, dicatat di dokumentasi paket); `TestDeepSeekEvalThinkingConfig` (`EvaluationThinking:"enabled"` mengubah body sesuai Step 0); `TestDeepSeekEvalTemperatureZeroSentNegativeDefaults02`; `TestDeepSeekEvalSeedOnlyIfSupported` (sesuai Step 0); `TestDeepSeekEvalUsage` (`InputTokens=prompt_tokens`, `OutputTokens=completion_tokens`, `CachedInputTokens`); `TestDeepSeekEvalHTTPErrors` (kelas yang sama dengan Task 5); `TestDeepSeekEvalCancel`; `TestDeepSeekEvalSingleAttempt` (503 → satu request saja, tanpa retry internal).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/deepseek/ -race -run 'Eval|Skeleton|SchemaInstruction'`. Expected: FAIL.
- [ ] **Step 3: Implement** sesuai tabel §8.4.9: body `{"model","messages","max_tokens","temperature","response_format":{"type":"json_object"},"thinking":{"type":"disabled"}}`; pesan `system` tambahan di akhir blok system (instruksi JSON + skema + kerangka); `choices[0].message.content` → `json.Valid` → `StructuredResult.JSON`; `Latency` = durasi request.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/deepseek/ -race` → PASS; `make modelname-check`.
- [ ] **Step 5: Commit** — `git add internal/provider/deepseek && git commit -m "feat(provider/deepseek): evaluation JSON mode dengan instruksi skema (tanpa strict)"`

---

### Task 7: ElevenLabs — `Config` dan TTS HTTP streaming per kalimat (§8.4.5)

**Files:**
- Create: `internal/provider/elevenlabs/{config.go,tts.go}`, `testdata/recordings/tts_{basic,odd_bytes,voice_not_found_404,quota_exceeded_429,mid_error,hang}.jsonl`
- Test: `internal/provider/elevenlabs/tts_test.go`

**Interfaces:**
- Consumes: Task 1; M4 `provider.TTSRequest` (`VoiceID`, `Model`, `SpeakingRate`), `httpx.*`.
- Produces: `elevenlabs.Config{APIKey, BaseURL, WSBaseURL string; Tap …; SyntheticFinalDelay, KeepAliveAfter time.Duration}` (dua timer terakhir dipakai Task 8–9; zero-value → 400 ms / 5 s); `elevenlabs.NewTTS(cfg, hc, log) (*TTS, error)`; `(*TTS).Name()==provider.ElevenLabs`; `Synthesize(ctx, provider.TTSRequest) (provider.TTSStream, error)`.

- [ ] **Step 0 (verifikasi di task adapter):** https://elevenlabs.io/docs/api-reference/text-to-speech/stream: body PCM `pcm_24000` = PCM16 LE mono tanpa header (durasi = bytes/48000 s), `language_code` & `previous_text`, bentuk body error `detail.status`; catat apakah `eleven_v3` dkk menolak `speed`/`previous_text`.
- [ ] **Step 1: Write the failing test**
```go
func TestELTTSRequest(t *testing.T) { // fixture tts_basic
	st, err := tts.Synthesize(ctx, provider.TTSRequest{Text: "こんにちは。", Language: "ja", PreviousText: "はい。", Model: "m_flash",
		VoiceID: "VOICE1", SpeakingRate: 1.0})
	require.NoError(t, err); chunks := drainTTS(st.Chunks())
	r := srv.Requests()[0]
	require.Equal(t, "/v1/text-to-speech/VOICE1/stream", r.Path); require.Equal(t, "pcm_24000", r.Query.Get("output_format")); require.Empty(t, r.Query.Get("optimize_streaming_latency"))
	require.Equal(t, "k", r.Header.Get("xi-api-key"))
	require.JSONEq(t, `{"text":"こんにちは。","model_id":"m_flash","language_code":"ja","voice_settings":{"speed":1.0},"previous_text":"はい。","apply_text_normalization":"auto"}`, string(r.Body))
	last := chunks[len(chunks)-1]
	require.True(t, last.Final); require.Equal(t, 6, last.Usage.Characters); require.Equal(t, totalBytes(chunks)/48, last.Usage.AudioMs)
}
```
Tambah: `TestELTTSMultilingualV2OmitsLanguageCode` (model berisi `multilingual_v2` → body tanpa `language_code`); `TestELTTSOnlySpeedInVoiceSettings` (body hanya memuat `voice_settings.speed`; tanpa `stability`/`similarity_boost`/`style`/`use_speaker_boost`; `SpeakingRate` 0 → `speed:1.0`, 0.80 → 0.8, 1.05 → 1.05, di luar 0,7–1,2 di-clamp ke batas vendor); `TestELTTSEmptyModelNotGuessed` (`req.Model` kosong → adapter tetap mengirim `model_id:""` apa adanya, tidak mengisi model default; fixture 400 vendor → `KindInvalidRequest`; model berasal dari `characters.model_name`, D-015); `TestELTTSOddByteBoundary` (fixture `tts_odd_bytes`: chunk vendor 1001+999 byte → setiap `PCM` genap, total byte terjaga); `TestELTTSErrors` (404 `detail.status=voice_not_found` → `KindInvalidRequest`, `HTTPStatus=404`, `VendorCode="voice_not_found"` (dihitung breaker); 429 `quota_exceeded` → `KindRateLimited` `Retryable=false`; 401 → `KindAuth`; 500 → `KindUnavailable`; sinkron); `TestELTTSMidStreamError` (stream putus → chunk terakhir `Err.Kind==UNAVAILABLE`, tanpa chunk setelahnya); `TestELTTSCancelWithin50ms` (fixture `hang`; tidak ada chunk setelah cancel); `TestELTTSSpan`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/elevenlabs/ -race -run TestELTTS`. Expected: FAIL.
- [ ] **Step 3: Implement** POST `{BaseURL}/v1/text-to-speech/{req.VoiceID}/stream?output_format=pcm_24000`; baca body per potongan, tahan byte ganjil; chunk `Final` + `Usage{Characters: utf8.RuneCountInString(req.Text), AudioMs: bytes/48}` dikirim saat EOF; hanya membaca `req.VoiceID`, `req.Model`, `req.SpeakingRate`, `req.Text`, `req.PreviousText`, `req.Language` (§8.4.12; nilai dari `config_snapshot.character`; tanpa ekspresi, D-014); body `voice_settings` hanya `{"speed":S}`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/elevenlabs/ -race -count=3` → PASS; `make modelname-check`.
- [ ] **Step 5: Commit** — `git add internal/provider/elevenlabs && git commit -m "feat(provider/elevenlabs): TTS HTTP streaming per kalimat"`

---

### Task 8: ElevenLabs — STT realtime: open, kirim audio, event, Finalize (§8.4.1)

**Files:**
- Create: `internal/provider/elevenlabs/{stt.go,stt_events.go}`, `testdata/recordings/stt_{basic,commit_empty,auto_commit,partial_cumulative}.jsonl`
- Test: `internal/provider/elevenlabs/stt_test.go`

**Interfaces:**
- Consumes: Task 7 (`Config`); M4 `provider.STTProvider/STTSession/STTEvent`, `provider.NormalizeJA`; Task 1 `replay.NewWSServer`.
- Produces: `elevenlabs.NewSTT(cfg Config, hc *http.Client, log *slog.Logger) (*STT, error)` dengan `Config.STTModel string` (`ELEVENLABS_STT_MODEL`; wajib non-kosong); `(*STT).Name()`, `Model()`, `Open(ctx, provider.STTSessionConfig) (provider.STTSession, error)`.

- [ ] **Step 0 (verifikasi di task adapter):** https://elevenlabs.io/docs/api-reference/speech-to-text/v-1-speech-to-text-realtime dan cookbook commit strategies. Putuskan & catat: (a) `language_code=ja` vs `jpn`; (b) `partial_transcript` = teks penuh segmen atau delta; (c) apakah commit 20 ms silence menghasilkan `committed_transcript` kosong (jika tidak → FINAL sintetis tetap berlaku); (d) ukuran chunk maksimum & backlog lebih cepat dari realtime; (e) peran `keepalive_interval_ms`. Perbarui fixture, spec §8.4.1, dan §8.4.13 sesuai hasil; catat juga apakah backlog replay (§8.5.8) diterima tanpa pacing.
- [ ] **Step 1: Write the failing test**
```go
func TestELSTTOpenQueryAndAuth(t *testing.T) { // fixture stt_basic; Open berhasil setelah session_started
	sess, err := stt.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: 16000})
	require.NoError(t, err); defer sess.Close()
	u := srv.UpgradeRequest()
	require.Equal(t, "/v1/speech-to-text/realtime", u.Path)
	require.Equal(t, url.Values{"model_id": {"scribe_v2_realtime"}, "language_code": {"ja"}, "audio_format": {"pcm_16000"}, "commit_strategy": {"manual"}, "include_timestamps": {"false"}, "enable_logging": {"false"}}, u.Query) // language_code mengikuti Step 0
	require.Equal(t, "k", u.Header.Get("xi-api-key"))
}
func TestELSTTBatchesTwoFramesAndFinalizeCommits(t *testing.T) {
	sess := openSession(t)
	require.NoError(t, sess.SendAudio(make([]byte, 640))); require.NoError(t, sess.SendAudio(make([]byte, 640))); require.NoError(t, sess.SendAudio(make([]byte, 640)))
	require.NoError(t, sess.Finalize())
	msgs := srv.ClientMessages()   // 2 frame → 1 pesan 1280 byte (commit:false); frame sisa + commit:true
	require.Len(t, msgs, 2); require.Equal(t, 1280, b64len(msgs[0])); require.False(t, commitFlag(msgs[0])); require.True(t, commitFlag(msgs[1])); require.Equal(t, 640, b64len(msgs[1]))
	ev := waitFinal(t, sess); require.True(t, ev.FromFinalize); require.Equal(t, "日本語の勉強", ev.Text)
}
```
Tambah: `TestELSTTPartialMapping` (`partial_transcript{text}` → `PARTIAL{Text: NormalizeJA, Confidence: nil, StartMs: segStartMs, EndMs: audioSentMs}`; `audioSentMs = byte/32`); `TestELSTTFinalizeWithNoBufferedFrameSendsSilence20ms` (commit tanpa audio sisa → payload 640 byte nol); **`TestELSTTFinalizeSyntheticEmptyFinal`** (fixture `commit_empty`: vendor tidak membalas; setelah `SyntheticFinalDelay`=30 ms di test → tepat satu `FINAL{Text:"",FromFinalize:true}`; 3× Finalize beruntun → 3 FINAL); `TestELSTTAutoCommitIsPlainFinal` (`committed_transcript` tanpa Finalize pending → `FINAL{FromFinalize:false}`; `pendingCommits` tidak negatif; `segStartMs` maju ke `commitMs`); `TestELSTTIgnoresTimestampedCommit` (`committed_transcript_with_timestamps` diabaikan); `TestELSTTOpenFailsWithoutSessionStarted` (tidak ada `session_started` dalam `ctx` 100 ms → error `KindTimeout`); `TestELSTTOpenAuthFailure` (HTTP 401 saat upgrade → `*Error{KindAuth}`); `TestELSTTSendAudioOddLengthRejected` (panjang ganjil → `KindInvalidRequest`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/elevenlabs/ -race -run TestELSTT`. Expected: FAIL.
- [ ] **Step 3: Implement** `Open`: `websocket.Dial` ke `{WSBaseURL}/v1/speech-to-text/realtime?…` dengan header `xi-api-key`, tunggu `session_started`; goroutine baca → `stt_events.go` memetakan pesan (tabel §8.4.1); goroutine tulis dengan batching 2 frame; state `pendingCommits`, `segStartMs`, `audioSentMs`; timer FINAL sintetis per `Finalize` (dibatalkan oleh `committed_transcript` yang menjawabnya). `Events()` kapasitas 64 dengan kebijakan drop-PARTIAL-tertua. `Config.Tap` dipanggil untuk setiap frame (untuk `-record`).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/elevenlabs/ -race -count=3` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/elevenlabs): STT realtime Scribe, batching, commit manual, FINAL sintetis"`

---

### Task 9: ElevenLabs — STT: error, rotasi, keep-alive, buffer, Close (§8.4.1)

**Files:**
- Modify: `internal/provider/elevenlabs/{stt.go,stt_events.go}`
- Create: `testdata/recordings/stt_{auth_error,quota,rate_limited,rotate_time_limit,drop}.jsonl`
- Test: `internal/provider/elevenlabs/stt_errors_test.go`

**Interfaces:**
- Consumes: Task 8; M4 `rotatingSTT` (`provider/stt_session.go`, §8.5.8: rotasi koneksi ke vendor yang SAMA, tanpa pindah vendor) mengenali rotasi lewat `Err.VendorCode ∈ {"session_time_limit_exceeded","insufficient_audio_activity"}` (**ambiguitas spec** — keputusan: adapter mengirim `STTEvent{Type: STTError, Err: &provider.Error{Kind: KindUnavailable, VendorCode: <kode vendor persis>}}` lalu `CLOSED`; Task 13 memverifikasinya terhadap test rotasi M4).
- Produces: tabel pemetaan error §8.4.1 lengkap; keep-alive; `Close` sesuai kontrak; rotasi vendor-sama oleh `rotatingSTT` (§8.5.8 butir 5; tanpa `PROVIDER_SWITCHED`) untuk KEDUA kode `session_time_limit_exceeded` dan `insufficient_audio_activity`.

- [ ] **Step 1: Write the failing test**
```go
func TestELSTTErrorMapping(t *testing.T) {
	cases := []struct{ code string; kind provider.ErrorKind; retry bool }{
		{"error", provider.KindUnavailable, true}, {"transcriber_error", provider.KindUnavailable, true},
		{"auth_error", provider.KindAuth, false}, {"unaccepted_terms", provider.KindAuth, false},
		{"quota_exceeded", provider.KindRateLimited, false}, {"rate_limited", provider.KindRateLimited, true}, {"queue_overflow", provider.KindRateLimited, true},
		{"resource_exhausted", provider.KindRateLimited, true}, {"commit_throttled", provider.KindRateLimited, true},
		{"input_error", provider.KindInvalidRequest, false}, {"invalid_request", provider.KindInvalidRequest, false}, {"chunk_size_exceeded", provider.KindInvalidRequest, false}}
	for _, c := range cases { ev := injectVendorError(t, c.code); require.Equal(t, provider.STTError, ev.Type); require.Equal(t, c.kind, ev.Err.Kind); require.Equal(t, c.retry, ev.Err.Retryable); require.Equal(t, c.code, ev.Err.VendorCode) }
}
func TestELSTTKeepAliveSilenceWhenIdle(t *testing.T) { /* KeepAliveAfter=40ms, tanpa audio → pesan 100 ms silence (3200 byte nol, commit:false); tidak ada commit */ }
```
Tambah: `TestELSTTRotationSignals` (`session_time_limit_exceeded` dan `insufficient_audio_activity` → `ERROR` dengan `VendorCode` tersebut lalu `CLOSED`, bukan kind AUTH/RATE_LIMITED; test tabel dua kasus, `Err.VendorCode` persis kode vendor); `TestELSTTWarningLogsOnly` (`warning` → tanpa event, ada log WARN); `TestELSTTSocketDropIsUnavailable` (fixture `drop`: close 1006 tanpa `CloseConnection` kita → `ERROR{UNAVAILABLE}` lalu `CLOSED`); `TestELSTTSendAudioBufferFull` (server tidak membaca; > 2 s audio (> 64.000 byte) ter-antre → `SendAudio` mengembalikan `*Error{KindUnavailable}` tanpa blok); `TestELSTTSendAfterClose` (`errors.Is(err, provider.ErrSessionClosed)`); `TestELSTTCloseIdempotentWithin500ms` (`Close` dua kali, selesai ≤ 500 ms walau server tidak menutup; channel `Events()` tertutup setelah `CLOSED`); `TestELSTTEventsDropOldestPartialNeverFinal` (konsumen berhenti membaca, banjir 200 PARTIAL + 1 FINAL + ERROR → FINAL & ERROR tetap ada); `TestELSTTCancelWithin50ms` (cancel ctx saat `Open` menggantung).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/elevenlabs/ -race -run 'ErrorMapping|KeepAlive|Rotation|Warning|Drop|BufferFull|AfterClose|CloseIdempotent|DropOldest|CancelWithin'`. Expected: FAIL.
- [ ] **Step 3: Implement** pemetaan §8.4.1; antrean tulis berkapasitas 2 s audio; keep-alive timer `Config.KeepAliveAfter` direset tiap pengiriman; `Close`: kirim pesan penutupan, tunggu ≤ 500 ms, `Conn.CloseNow()`, emit `CLOSED`, tutup channel; semua goroutine berhenti (diverifikasi Task 12).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/elevenlabs/ -race -count=3` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/elevenlabs): STT error mapping, rotasi, keep-alive, close"`

---

### Task 10: Cartesia — `Config` dan STT Ink realtime (§8.4.6)

**Files:**
- Create: `internal/provider/cartesia/{config.go,stt.go,stt_events.go}`, `internal/provider/cartesia/testdata/recordings/stt_{basic,flush_empty,finalize_chunks,error_401,error_429,session_limit,drop}.jsonl`
- Modify: `Makefile` (pola `modelname-check` + `ink-`)
- Test: `internal/provider/cartesia/stt_test.go`, `stt_errors_test.go`

**Interfaces:**
- Consumes: Task 1 (`replay.NewWSServer`); M4 `provider.STT*`, `provider.NormalizeJA`, `httpx.ClassifyWSClose`.
- Produces: `cartesia.Config{APIKey, BaseURL, WSBaseURL, Version, STTModel string; PoolSize int; SyntheticFinalDelay, KeepAliveAfter time.Duration; Tap …}` (zero-value → `https://api.cartesia.ai`, `wss://api.cartesia.ai`, `Version` dari `CARTESIA_VERSION` (default `2026-08-14`), `STTModel` dari `CARTESIA_STT_MODEL` (default `ink-2`), `PoolSize` dipakai Task 11); `cartesia.NewSTT(cfg, hc, log) (*STT, error)` (error bila `APIKey`/`STTModel`/`Version` kosong); `(*STT).Name()==provider.Cartesia`; `Open(ctx, provider.STTSessionConfig) (provider.STTSession, error)`. Task 11 menambah `NewTTS` pada paket yang sama dan memperluas `Config.Validate`.

- [ ] **Step 0 (verifikasi di task adapter; hasil dicatat di `docs/implementation-log.md`, perbedaan → perbarui spec §8.4.6/§8.4.13 dan `docs/implementation-deviations.md` sebelum menulis fixture):** buka https://docs.cartesia.ai/api-reference/stt/stt dan https://docs.cartesia.ai/build-with-cartesia/stt-models. Periksa: (a) header auth WS `X-API-Key` dan pemasangan versi API (query `cartesia_version` per docs vs header `Cartesia-Version`; kirim keduanya hanya bila terbukti diterima); (b) query wajib `model`, `encoding=pcm_s16le`, `sample_rate=16000`; `keyterm` (diulang; hanya `ink-2`/`ink-preview`; maks 100 istilah/1.200 karakter); cara memilih bahasa Jepang pada `ink-2` (tanpa parameter `language`: otomatis/multibahasa?) dan apakah `ink-whisper` mendukung `ja` (parameter VAD `min_volume`/`max_silence_duration_secs` TIDAK dipakai, D-076); (c) ukuran potongan audio yang disarankan (±100 ms = 3.200 byte); (d) semantik `transcript.is_final=false` (teks segmen kumulatif vs delta) dan `transcript.is_final=true` sebelum `finalize`; (e) `finalize` → `transcript(is_final=true)` lalu `flush_done`; `close` → `done`; (f) bentuk `error{status_code,title,message,error_code,request_id}` dan kode kuota/kredit habis; (g) batas durasi/idle sesi (→ sinyal rotasi `VendorCode`) dan keep-alive; (h) apakah backlog lebih cepat dari realtime diterima (rotasi §8.5.8); (i) satuan harga dan limit konkurensi plan (docs.cartesia.ai/pricing; dicatat untuk Task 22).
- [ ] **Step 1: Write the failing test**
```go
func TestCartesiaSTTOpenQueryAndAuth(t *testing.T) { // fixture stt_basic; Open sukses = handshake 101
	stt, _ := cartesia.NewSTT(cartesia.Config{APIKey: "k", WSBaseURL: srv.URL, STTModel: "m_ink", Version: "v1"}, httpx.NewClient(), slog.Default())
	sess, err := stt.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: 16000, Keyterms: []string{"歌舞伎"}}); require.NoError(t, err); defer sess.Close()
	u := srv.UpgradeRequest(); require.Equal(t, "/stt/websocket", u.Path); require.Equal(t, "k", u.Header.Get("X-API-Key"))
	require.Equal(t, url.Values{"model": {"m_ink"}, "encoding": {"pcm_s16le"}, "sample_rate": {"16000"}, "cartesia_version": {"v1"}, "keyterm": {"歌舞伎"}}, u.Query) // bahasa mengikuti Step 0(b); versi mengikuti Step 0(a)
}
func TestCartesiaSTTBatchesFiveFramesAndFinalizeFlush(t *testing.T) {
	sess := openSession(t)
	for i := 0; i < 7; i++ { require.NoError(t, sess.SendAudio(make([]byte, 640))) }
	require.NoError(t, sess.Finalize())
	msgs := srv.ClientMessages()    // 5 frame → 1 pesan biner 3.200 byte; sisa 2 frame (1.280 byte) dikirim saat Finalize; lalu teks "finalize"
	require.Equal(t, 3200, len(msgs[0].Data)); require.Equal(t, 1280, len(msgs[1].Data)); require.Equal(t, "finalize", string(msgs[2].Data))
	ev := waitFinal(t, sess); require.True(t, ev.FromFinalize); require.Equal(t, "日本語の勉強", ev.Text)
}
```
Tambah: `TestCartesiaSTTKeytermsTruncated` (> 100 istilah atau > 1.200 karakter dipotong ke batas; model tanpa dukungan keyterm → tidak dikirim); `TestCartesiaSTTPartialMapping` (`transcript{is_final:false}` → `PARTIAL{Text: NormalizeJA, Confidence: nil}`; kumulatif/delta sesuai Step 0(d)); `TestCartesiaSTTFinalBeforeFinalizeIsPlainFinal` (`is_final=true` sebelum `finalize` → `FINAL{FromFinalize:false, Words: detik→ms}`); **`TestCartesiaSTTFinalizeFlushDone`** (fixture `finalize_chunks`: dua `transcript(is_final=true)` antara `finalize` dan `flush_done` ditampung tanpa trim/spasi tambahan, saat `flush_done` → tepat satu `FINAL{FromFinalize:true, Text: gabungan ternormalisasi}`); `TestCartesiaSTTFlushDoneWithoutTranscriptIsEmptyFinal` (fixture `flush_empty` → `FINAL{"",FromFinalize:true}`; 3× Finalize → 3 FINAL); `TestCartesiaSTTCloseSendsClose` (`close` → `done`; `Close` idempotent ≤ 500 ms); `TestCartesiaSTTSendAudioOddLengthRejected`; `TestCartesiaSTTOpenAuthFailure` (401 upgrade → `KindAuth`); `TestCartesiaSTTUsageAudioMsSent` (byte/32).
`stt_errors_test.go`: `TestCartesiaSTTErrorMapping` (pesan `error`: `status_code` 401/403 → `KindAuth`; 429 → `KindRateLimited`; 400/404/422 → `KindInvalidRequest`; 5xx → `KindUnavailable`; `VendorCode`=`error_code`; kode kuota habis sesuai Step 0(f) → `Retryable=false`); `TestCartesiaSTTSessionLimitIsRotationSignal` (fixture `session_limit` sesuai Step 0(g): `ERROR{KindUnavailable, VendorCode:<kode persis>}` lalu `CLOSED`); `TestCartesiaSTTSocketDropIsUnavailable` (fixture `drop`); `TestCartesiaSTTKeepAliveWhenIdle` (sesuai Step 0(g)); `TestCartesiaSTTSendAudioBufferFull`; `TestCartesiaSTTSendAfterClose`; `TestCartesiaSTTEventsDropOldestPartialNeverFinal`; `TestCartesiaSTTCancelWithin50ms`; `TestCartesiaSTTKeyNotLeaked`; `TestCartesiaSTTSpan`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/cartesia/ -race -run TestCartesiaSTT`. Expected: FAIL (paket belum ada).
- [ ] **Step 3: Implement** `config.go` (default + `Validate`), `Open`: `websocket.Dial` ke `{WSBaseURL}/stt/websocket?…` dengan `X-API-Key`; goroutine baca → `stt_events.go` (tabel §8.4.6: `transcript`, `flush_done`, `done`, `error`); goroutine tulis dengan batching 5 frame (3.200 byte), `finalize`/`close` sebagai text frame; state `pendingFinalize`, buffer teks final antara `finalize` dan `flush_done`, `audioSentMs`; FINAL sintetis hanya bila `flush_done` tidak datang dalam `SyntheticFinalDelay` setelah `finalize` (default 400 ms, pengaman); `Events()` kapasitas 64 dengan kebijakan drop-PARTIAL-tertua. Tambah `ink-` ke pola `make modelname-check`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/cartesia/ -race -count=3` → PASS; `make modelname-check` → exit 0.
- [ ] **Step 5: Commit** — `git add internal/provider/cartesia Makefile && git commit -m "feat(provider/cartesia): STT Ink realtime, finalize/flush_done, FINAL tepat satu"`

---

### Task 11: Cartesia — TTS WebSocket (Sonic) per karakter (§8.4.14, D-015)

**Files:**
- Create: `internal/provider/cartesia/{tts.go,pool.go,errors.go}`, `testdata/recordings/tts_{basic,odd_bytes,error_model_not_found,error_429,error_401,mid_error,hang,cancel_ack}.jsonl`, `testdata/recordings/tts_suite.jsonl` (reaktif untuk `TTSSuite`), `internal/provider/cartesia/live_test.go` (`//go:build live`)
- Modify: `internal/provider/cartesia/config.go` (dibuat Task 10; tambah `PoolSize` default dan validasi TTS), `cmd/internal/providerwire/build.go` (cabang `CARTESIA` untuk `CapTTS`; Task 14 sudah menyediakan field `Cartesia*` di `RouterConfig`), `.env.example` (blok `CARTESIA_*`, sudah ada di spec), `Makefile` (pola `modelname-check` ditambah `sonic`; `ink-` sudah ditambah Task 10), `internal/provider/pricing/prices.yaml` (entri `CARTESIA` TTS; nilai nol sampai Task 22)
- Test: `internal/provider/cartesia/tts_test.go`, `conformance_test.go`, `live_test.go`

**Interfaces:**
- Consumes: Task 1 (`replay.NewWSServer`/`httpsim`, `RequireEnv`, `httpx.StartSpan`), M4 `provider.TTSRequest` (`VoiceID`, `Model`, `SpeakingRate`, `RequestID`), `httpx.ClassifyHTTP/ClassifyWSClose`, `providertest.TTSSuite`, Task 10 `cartesia.Config` dan Task 14 `RouterConfig.Cartesia*`.
- Produces: `cartesia.Config` (milik Task 10; di sini `PoolSize` dari `PROVIDER_CONCURRENCY_CARTESIA_TTS` dipakai; zero-value → `https://api.cartesia.ai`, `wss://api.cartesia.ai`, `Version` dari `CARTESIA_VERSION`); `cartesia.NewTTS(cfg, hc, log) (*TTS, error)`; `(*TTS).Name()==provider.Cartesia`; `Synthesize(ctx, provider.TTSRequest) (provider.TTSStream, error)`. Satu context per kalimat (`context_id = req.RequestID`, `continue:false`); pool koneksi WS persisten per pod, satu context aktif per koneksi (multiplex hanya bila terbukti di benchmark, builder call §8.4.14). Hanya membaca `req.Model`, `req.VoiceID`, `req.SpeakingRate`, `req.Text`, `req.Language`, `req.RequestID`; `PreviousText` tidak dikirim.

- [ ] **Step 0 (verifikasi di task adapter; hasil ya/tidak + kutipan field dicatat di `docs/implementation-log.md`, perbedaan dari spec → perbarui spec §8.4.14 dan `docs/implementation-deviations.md` SEBELUM menulis fixture):** buka https://docs.cartesia.ai/api-reference/tts/websocket , /api-reference/tts/bytes , /use-the-api/compare-tts-endpoints , /pricing. Periksa: (a) header auth WS (`X-API-Key` vs `Authorization: Bearer`) dan untuk `POST /tts/bytes`; (b) cara memasang versi API pada WS (header `Cartesia-Version` vs query/param `cartesia_version`; kirim keduanya hanya bila terbukti diterima) dan nilai default `CARTESIA_VERSION` terkini; (c) ID model sah (`sonic-3.6`/`sonic-3.5`/`sonic-3`/`sonic-latest` vs contoh pemilik produk `sonic3.6`) — adapter mengirim apa adanya, tidak menormalisasi; (d) `generation_config.speed` (rentang 0,6–1,5; model mana yang mendukung → konstanta `speedSupported` per prefiks model di adapter bila model lama menolak); (e) `output_format` `raw`/`pcm_s16le`/24000 = mono, durasi = bytes/48000 s, kemungkinan frame audio kosong; (f) bentuk pesan `chunk`/`done`/`error` (`status_code`, `title`, `message`, `error_code`, `request_id`) dan kode error kuota/kredit; (g) semantik `cancel` per `context_id`, idle timeout koneksi/ping; (h) limit konkurensi per plan dan satuan harga (karakter vs jam audio); (i) dukungan bahasa Jepang oleh model Sonic (hasil kualitas → benchmark Task 22, bukan diasumsikan di sini).
- [ ] **Step 1: Write the failing test** (fixture awal dari contoh docs, `meta.source=<URL>`, `meta.verified=false`)
```go
func TestCartesiaTTSRequest(t *testing.T) { // fixture tts_basic (WS)
	st, err := tts.Synthesize(ctx, provider.TTSRequest{Text: "こんにちは。", Language: "ja", Model: "M1", VoiceID: "V1", SpeakingRate: 1.0, RequestID: "sid:1:0"})
	require.NoError(t, err); chunks := drainTTS(st.Chunks())
	h := srv.Handshakes()[0]; require.Equal(t, "k", h.Header.Get("X-API-Key")) // header/versi mengikuti hasil Step 0(a)(b)
	m := srv.Messages()[0]
	require.JSONEq(t, `{"model_id":"M1","transcript":"こんにちは。","voice":"V1","language":"ja","context_id":"sid:1:0","continue":false,"output_format":{"container":"raw","encoding":"pcm_s16le","sample_rate":24000},"generation_config":{"speed":1.0}}`, string(m))
	last := chunks[len(chunks)-1]; require.True(t, last.Final)
	require.Equal(t, 6, last.Usage.Characters); require.Equal(t, totalBytes(chunks)/48, last.Usage.AudioMs)
}
```
Tambah: `TestCartesiaTTSNoLocaleWithLanguage` (body tidak memuat `locale` bersamaan dengan `language`); `TestCartesiaTTSModelPassedVerbatim` (`Model:"sonic3.6"` terkirim apa adanya, tidak dinormalisasi; `Model:""` terkirim kosong dan fixture 400 `model_not_found` → `KindInvalidRequest`, `VendorCode="model_not_found"`, `HTTPStatus=400`, tanpa fallback otomatis di adapter); `TestCartesiaTTSOnlySpeedInGenerationConfig` (tanpa `volume`/`emotion`/`previous_text`; `SpeakingRate` 0 → `speed:1.0`; 0.80 → 0.8; 1.05 → 1.05; di luar 0,6–1,5 di-clamp ke batas vendor; model tanpa dukungan speed → `generation_config` tidak dikirim sesuai Step 0(d)); `TestCartesiaTTSDecodesBase64Chunks` (pesan `chunk` base64 → `PCM` genap; fixture `tts_odd_bytes`: 1001+999 byte → setiap `PCM` genap, total byte terjaga; frame `data` kosong diabaikan); `TestCartesiaTTSDoneBecomesFinalChunk` (`done` → chunk `Final` + `Usage`; tidak ada chunk sesudahnya; pesan `flush_done`/`timestamps` diabaikan); `TestCartesiaTTSErrors` (pesan WS `error` `status_code` 400/404/422 → `KindInvalidRequest` (dihitung breaker bila `VendorCode` bukan `voice_binding_missing`), 401/403 → `KindAuth`, 429 atau `error_code` kuota → `KindRateLimited` `Retryable=false`, 5xx → `KindUnavailable`; handshake HTTP 401/403/5xx sinkron sebelum stream dibuka; `VendorCode`=`error_code`); `TestCartesiaTTSMidStreamError` (socket putus abnormal → chunk terakhir `Err.Kind==UNAVAILABLE`, tanpa chunk setelahnya); `TestCartesiaTTSCancelSendsCancelAndKeepsConn` (ctx dibatalkan → pesan `{"context_id":"…","cancel":true}` terkirim ≤ 50 ms, tidak ada chunk setelah cancel, koneksi kembali ke pool bila cancel diterima; fixture `cancel_ack`); `TestCartesiaTTSCancelWithin50ms` (fixture `hang`/server tidak merespons cancel → koneksi ditutup ≤ 50 ms dan diganti koneksi baru pada panggilan berikutnya); `TestCartesiaPoolBoundsConcurrency` (`PoolSize=2`: panggilan ke-3 menunggu slot dan membatalkan bersih saat ctx dibatalkan; satu context aktif per koneksi); `TestCartesiaPoolReconnectsAfterIdleClose` (koneksi idle ditutup server → panggilan berikut membuka koneksi baru tanpa error ke pemanggil); `TestCartesiaTTSNoSecretInLogsOrErrors` (key tidak muncul di log/`Error.Message`/span); `TestCartesiaTTSSpan` (`provider.name=CARTESIA`, `provider.model`, `provider.outcome`, `provider.error_kind`); `CartesiaTTSConformance` dan `CartesiaSTTConformance` (`providertest.TTSSuite`/`STTSuite` dengan `tts_suite.jsonl`/`stt_suite.jsonl` reaktif; `speech`/`newReq` seperti Task 13).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/cartesia/ -race -run 'Cartesia'`. Expected: FAIL (paket belum ada).
- [ ] **Step 3: Implement** adapter (`config.go` diperluas dari Task 10; `pool.go` pool WS dengan `coder/websocket`, ping/idle sesuai Step 0(g); `tts.go` `Synthesize` mengirim satu pesan generate per kalimat, membaca pesan `chunk`/`done`/`error`, tahan byte ganjil di batas chunk, `Usage{Characters: utf8.RuneCountInString(req.Text), AudioMs: bytes/48}`; `errors.go` pemetaan `status_code`/`error_code` → `*provider.Error`). Hanya impor yang diizinkan Task 13 (`provider`, `provider/httpx`, `platform/otel`, stdlib, `coder/websocket`, `prometheus/client_golang`); tanpa literal nama model (`sonic`) di kode non-test. Perluas `providerwire.Build`: `CARTESIA_TTS_ENABLED=true` → `d.TTS[provider.Cartesia]` (kunci API wajib, divalidasi Task 15). Tambah `sonic` ke pola `make modelname-check`; entri `prices.yaml` `CARTESIA` TTS dengan satuan sesuai Step 0(h) (nilai nol sampai Task 22; `TestShippedPrices…` Task 22 menegakkan non-nol).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/cartesia/ ./cmd/internal/providerwire/ -race -count=3` → PASS; `go test ./internal/provider/... -race -run 'ImportRules|NoGoroutineLeak|NoSecret'` → PASS (mencakup `cartesia` STT dan TTS); `make lint arch-check modelname-check` → exit 0. **Live test (MANUAL GATE, kunci `CARTESIA_API_KEY`, `LIVE_CARTESIA_VOICE_ID`, `LIVE_CARTESIA_TTS_MODEL`):** `go test -tags live ./internal/provider/cartesia -run TestLiveCartesiaTTS -race -count=1` → kalimat 30 karakter Jepang menghasilkan ≥ 0,5 s PCM16 24 kHz dengan `TTFA` tercatat; `-record` (bersama STT Task 10) menulis `testdata/recordings/tts_*.jsonl` dengan `meta.verified=true` (ganti fixture dokumentasi; tanpa secret: `grep -ri 'sk_car\|X-API-Key' internal/provider/cartesia/testdata` kosong) dan seluruh test recorded dijalankan ulang; tanpa kunci → SKIP, bukan FAIL.
- [ ] **Step 5: Commit** — `git add internal/provider/cartesia cmd/internal/providerwire Makefile .env.example internal/provider/pricing && git commit -m "feat(provider/cartesia): TTS WebSocket per kalimat (Sonic), pool koneksi, recorded-fixture dan live test"`

---

### Task 12: Dihapus dari v1 (pronunciation ditunda ke pasca-v1).

---

### Task 13: Conformance `providertest` untuk semua adapter + aturan impor + hygiene

**Files:**
- Create: `internal/provider/{openai,deepseek,elevenlabs,cartesia}/conformance_test.go`, `internal/provider/adapters_test.go` (paket `provider_test`); fixture reaktif `testdata/recordings/<cap>_suite.jsonl` per (vendor, kapabilitas) — `cartesia/*_suite.jsonl` dibuat Task 10 (STT) dan Task 11 (TTS) dengan pola yang sama; `TestAdaptersImportRules`/`NoGoroutineLeak`/`NoSecret` di sini mengiterasi direktori `internal/provider/*` sehingga otomatis mencakup `cartesia`
- Test: file yang sama

**Interfaces:**
- Consumes: M4 `providertest.LLMSuite(t, newProvider func(t *testing.T) provider.LLMProvider)`, `TTSSuite(t, newProvider, newReq func() provider.TTSRequest)`, `STTSuite(t, newProvider, speech func() []byte)`, `EvaluationSuite(t, newProvider, req provider.StructuredRequest)`, `EmbeddingSuite(t, newProvider)`; fixture *reaktif* (aturan `on`, `times:0`) per vendor: `<cap>_suite.jsonl` (dibuat di task ini, berisi balasan generik yang memenuhi suite: LLM delta+usage; STT membalas tiap commit/Finalize dengan satu final; dll.).
- Produces: pemetaan suite→adapter: `openai` (STT, LLM, Evaluation, Embedding), `deepseek` (LLM, Evaluation), `elevenlabs` (STT, TTS), `cartesia` (STT, TTS; Task 10–11). Total 10 kombinasi (vendor, kapabilitas) (§8.4.7).

- [ ] **Step 1: Write the failing test**
```go
func TestOpenAILLMConformance(t *testing.T) {
	providertest.LLMSuite(t, func(t *testing.T) provider.LLMProvider {
		srv := replay.NewHTTPServer(t, replay.LoadScript(t, replay.RecordingsDir(t, "openai"), "llm_suite.jsonl"))
		l, err := openai.NewLLM(openai.Config{APIKey: "sk-test", BaseURL: srv.URL}, httpx.NewClient(), slog.Default()); require.NoError(t, err); return l })
}
// … satu fungsi per (vendor, kapabilitas): OpenAI STT/Evaluation/Embedding, DeepSeek LLM/Evaluation (EvaluationSuite: JSON valid dikembalikan apa adanya), ElevenLabs STT/TTS, Cartesia STT/TTS;
// STT memakai speech() = 1 s sinus 440 Hz PCM16 16 kHz; Config timer sintetis dipendekkan (30 ms)
func TestAdaptersImportRules(t *testing.T) { /* go/parser ImportsOnly: tiap internal/provider/<vendor> non-test hanya boleh stdlib, coder/websocket, prometheus/client_golang, platform/otel, provider, provider/httpx; terlarang: conversation/audio, fake, pricing, vendor lain (deepseek tidak mengimpor openai dan sebaliknya), paket fitur lain; paket vendor selain {openai,deepseek,elevenlabs,cartesia} (mis. deepgram, gemini) tidak boleh ada */ }
func TestNoGoroutineLeakAfterCancel(t *testing.T) { /* untuk tiap adapter: catat runtime.NumGoroutine(), 20× buka stream/sesi lalu cancel/Close, tunggu ≤ 500 ms, selisih ≤ 2 */ }
func TestNoSecretInLogsOrErrors(t *testing.T) { /* tiap adapter dengan key "SECRETKEY-123": jalankan kasus sukses + setiap kasus error 401/429/5xx dengan slog ke buffer; buffer, err.Error(), Error.Message, dan span attributes tidak memuat "SECRETKEY-123" */ }
func TestAdapterRotationSignalMatchesRouter(t *testing.T) { /* tabel per STT adapter (ELEVENLABS: session_time_limit_exceeded & insufficient_audio_activity; OPENAI & CARTESIA: kode batas sesi hasil Step 0 Task 4 dan 10): provider.NewRouter dengan RouterDeps.STT = adapter(replay) tunggal; kode vendor → ERROR dengan VendorCode yang dikenali rotatingSTT M4 → Stats().Reconnects==1, sesi baru dibuka ke vendor SAMA, replay dari rotation buffer, tidak ada pindah vendor dan tidak ada event PROVIDER_SWITCHED */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/... -race -run 'Conformance|ImportRules|NoGoroutineLeak|NoSecret|RotationSignal'`. Expected: FAIL (fixture `*_suite.jsonl` belum ada; suite gagal).
- [ ] **Step 3: Implement** fixture `*_suite.jsonl` reaktif per adapter dan wiring test. Bila `providertest` menemukan pelanggaran kontrak adapter, perbaiki ADAPTER (bukan suite). Bila `TestAdapterRotationSignalMatchesRouter` gagal karena router M4 mengenali sinyal lain, sesuaikan adapter (Task 4, 9, 10) ke sinyal router dan catat di `docs/implementation-deviations.md`; jangan ubah router.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race -count=2` → PASS; `go test ./internal/provider/... -race -cover` → `internal/provider/...` ≥ 70% (§14.8); `make lint arch-check modelname-check` → exit 0.
- [ ] **Step 5: Commit** — `git add internal/provider && git commit -m "test(provider): conformance providertest, aturan impor, kebocoran goroutine, secret, dan sinyal rotasi untuk semua adapter"`

---

### Task 14: `providerwire.Build` — merakit adapter nyata dari sakelar `*_ENABLED`

**Files:**
- Create: `cmd/internal/providerwire/build.go`
- Modify: `internal/provider/config.go` (hanya field yang BELUM ada: `OpenAIBaseURL`, `OpenAIWSBaseURL`, `DeepSeekBaseURL`, `ElevenLabsBaseURL`, `ElevenLabsWSBaseURL`, `CartesiaBaseURL`, `CartesiaWSBaseURL`, `CartesiaVersion` (`CARTESIA_VERSION`), sakelar `OpenAISTTEnabled`/`ElevenLabsSTTEnabled`/`CartesiaSTTEnabled`/`OpenAILLMEnabled`/`DeepSeekLLMEnabled`/`ElevenLabsTTSEnabled`/`CartesiaTTSEnabled`/`OpenAIEmbeddingEnabled`, model `OpenAISTTModel`/`ElevenLabsSTTModel`/`CartesiaSTTModel`/`OpenAILLMModel`/`DeepSeekLLMModel`/`OpenAIEvaluationModel`/`DeepSeekEvaluationModel`/`OpenAIEmbeddingModel`, `OpenAIOmitSampling`, `OpenAIReasoningEffort`, `DeepSeekLLMThinking`, `DeepSeekEvaluationThinking`, kunci `OpenAIAPIKey`/`DeepSeekAPIKey`/`ElevenLabsAPIKey`/`CartesiaAPIKey` (secret) dengan tag/default §8.5.1/§13.2.8; periksa dulu `grep`; M4 `RouterConfig` mungkin sudah memuatnya), `.env.example` (blok vendor sudah ada di spec §14), `deploy/k8s/base` ConfigMap bila field baru ditambah; HAPUS dari `RouterConfig`/env bila masih ada: `PROVIDER_STT_*`, `PROVIDER_LLM_*`, `PROVIDER_EVALUATION_*`, `TTSPrimary`, `TTSFallback`, `PROVIDER_TTS_PRIMARY/_FALLBACK`, `PROVIDER_PROFILE_*`, `DEEPGRAM_*`, `GEMINI_*` (D-015/D-016/D-017)
- Test: `cmd/internal/providerwire/build_test.go`

**Interfaces:**
- Consumes: semua `New*` Task 2–12, `httpx.NewClient`, `provider.RouterConfig` (+`ModeFor(cap Capability) string` dari M4 T3; dipakai tanpa syarat, tidak ditambahkan ulang), `providerwire` yang sudah dibuat M5 (`cmd/internal/providerwire/fake.go`, `BuildFake`) — task ini MEMPERLUAS paket itu dengan `build.go`.
- Produces: `type RealDeps struct{ STT provider.STTProvider; LLM provider.LLMProvider; Evaluation provider.EvaluationProvider; TTS map[provider.Name]provider.TTSProvider; Embedding provider.EmbeddingProvider }` (bentuk sama dengan `provider.RouterDeps` §8.5.2: tepat satu adapter STT/LLM/Evaluation/Embedding, tanpa peta primary/fallback); `func (d RealDeps) TTSVendors() []string` (nama vendor TTS terbangun, UPPERCASE, terurut; diteruskan `cmd/*` ke validator konten sebagai "vendor TTS enabled"); `func ActiveVendor(cfg provider.RouterConfig, c provider.Capability) (provider.Name, error)` (hanya STT dan LLM/EVALUATION: membaca flag; nol atau lebih dari satu `true` → error yang menyebut SEMUA nama variabel, mis. `exactly one of OPENAI_STT_ENABLED, ELEVENLABS_STT_ENABLED, CARTESIA_STT_ENABLED must be true`; EVALUATION mengikuti LLM; EMBEDDING selalu `OPENAI`); `func Build(cfg provider.RouterConfig, caps []provider.Capability, log *slog.Logger) (RealDeps, error)` — hanya membangun kapabilitas di `caps` yang `ModeFor(cap)=="real"`; untuk STT/LLM/Evaluation hanya vendor aktif dari `ActiveVendor` (`openai.NewSTT`/`elevenlabs.NewSTT`/`cartesia.NewSTT`; `openai.NewLLM`+`openai.NewEvaluation` atau `deepseek.NewLLM`+`deepseek.NewEvaluation`; model per vendor dari `RouterConfig`); Embedding `openai.NewEmbedding` selalu; **untuk TTS (D-015) membangun SEMUA adapter TTS yang enabled** (`ELEVENLABS_TTS_ENABLED` → `elevenlabs.NewTTS`, `CARTESIA_TTS_ENABLED` → `cartesia.NewTTS` dengan `PoolSize = PROVIDER_CONCURRENCY_CARTESIA_TTS`) tanpa pemilihan primary/fallback global; vendor TTS yang tidak enabled tidak terdaftar (karakter yang memakainya ditolak resolver/router `voice_binding_missing`); `cmd/*/main.go` menggabungkan: kapabilitas mode `real` ← `RealDeps`, sisanya ← wiring fake M4/M5 yang sudah ada. Satu `*http.Client` (`httpx.NewClient`) per vendor; kapabilitas vendor yang tidak enabled tidak dibangun (tanpa dial, tanpa kunci).

- [ ] **Step 1: Write the failing test**
```go
func TestBuildDefaultsOpenAISTTAndLLM(t *testing.T) {
	cfg := realConfig(t) // RouterConfig mode real, semua kunci terisi, BaseURL → httptest, flag default D-017
	d, err := providerwire.Build(cfg, []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapEvaluation, provider.CapEmbedding, provider.CapTTS}, slog.Default())
	require.NoError(t, err)
	require.Equal(t, provider.OpenAI, d.STT.Name()); require.Equal(t, provider.OpenAI, d.LLM.Name()); require.Equal(t, provider.OpenAI, d.Evaluation.Name())
	require.Equal(t, []string{"CARTESIA", "ELEVENLABS"}, d.TTSVendors())
}
func TestBuildSelectsVendorBySwitch(t *testing.T) { /* tabel: {OPENAI_STT_ENABLED=false, ELEVENLABS_STT_ENABLED=true} → d.STT.Name()==ELEVENLABS; CARTESIA_STT_ENABLED=true saja → CARTESIA; DEEPSEEK_LLM_ENABLED=true + OPENAI_LLM_ENABLED=false → d.LLM dan d.Evaluation bernama DEEPSEEK; hanya adapter terpilih yang dibangun (adapter lain tidak membuka koneksi/tidak butuh kunci) */ }
func TestBuildRejectsNotExactlyOneSTT(t *testing.T) { /* nol true → error menyebut OPENAI_STT_ENABLED, ELEVENLABS_STT_ENABLED, CARTESIA_STT_ENABLED; dua true → error yang sama */ }
func TestBuildRejectsNotExactlyOneLLM(t *testing.T) { /* nol/dua → error menyebut OPENAI_LLM_ENABLED, DEEPSEEK_LLM_ENABLED */ }
func TestBuildSkipsFakeCapabilities(t *testing.T) { /* ModeTTS="fake" → d.TTS kosong; STT tetap real; flag kapabilitas fake tidak divalidasi */ }
func TestBuildAllEnabledTTSAdapters(t *testing.T) { /* ELEVENLABS_TTS_ENABLED=true + CARTESIA_TTS_ENABLED=true → d.TTS memuat kedua vendor sekaligus, d.TTSVendors()==[CARTESIA ELEVENLABS]; salah satu false → hanya vendor lain; keduanya false → error; tanpa referensi TTSPrimary/TTSFallback (field sudah dihapus); TestBuildCartesiaPoolSizeFromConcurrency: PoolSize = PROVIDER_CONCURRENCY_CARTESIA_TTS */ }
func TestBuildEmbeddingAlwaysOpenAI(t *testing.T) { /* DEEPSEEK_LLM_ENABLED=true → d.Embedding.Name()==OPENAI; OPENAI_EMBEDDING_ENABLED=false → error */ }
func TestNoLegacyVendorPackages(t *testing.T) { /* tidak ada import internal/provider/deepgram|gemini dan tidak ada openai.NewTTS di build.go */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/internal/providerwire/ -race -run 'TestBuild|TestNoLegacy'`. Expected: FAIL.
- [ ] **Step 3: Implement** `ActiveVendor` dan `Build` dengan `switch`; konfigurasi adapter dari `RouterConfig` (model STT/LLM/Evaluation per vendor aktif; model/voice TTS TIDAK dikonfigurasi di adapter — datang per request dari karakter); error tak dikenal → `fmt.Errorf` menyebut env var. `cmd/api` TIDAK mengimpor paket ini (hanya `cmd/realtime`, `cmd/worker`, `cmd/kotoki-cli`). `cmd/worker` memanggil `Build` dengan `caps` LLM/Evaluation/Embedding; `cmd/realtime` dengan STT/LLM/TTS/Embedding (retrieval memory).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/internal/providerwire/ ./internal/provider/ -race` → PASS; `make arch-check` → exit 0 (api/worker `CGO_ENABLED=0`; worker memuat adapter tanpa `conversation/audio`).
- [ ] **Step 5: Commit** — `git add cmd/internal internal/provider/config.go .env.example deploy && git commit -m "feat(cmd): providerwire.Build merakit adapter nyata dari sakelar *_ENABLED"`

---

### Task 15: Validasi startup mode `real` + penolakan voice ID placeholder + wiring `cmd/*`

**Files:**
- Create: `cmd/internal/providerwire/validate.go`, `internal/content/voicecheck.go`, `db/queries/voicecheck.sql`, `internal/postgres/voicecheck_repo.go`
- Modify: `cmd/realtime/main.go` & `cmd/realtime/providers.go` (blok pembuat `RouterDeps`: kapabilitas mode real ← `providerwire.Build`, sisanya ← `providerwire.BuildFake`), `cmd/worker/main.go` (+ wiring provider worker bila sudah ada file wiring-nya; ikuti nama file M6/M7), `cmd/api/main.go` (hanya `content.ValidateVoices`; TIDAK mengimpor `providerwire`), `internal/postgres/sqlcgen` (hasil `make sqlc`)
- Test: `cmd/internal/providerwire/validate_test.go`, `internal/content/voicecheck_test.go`, `internal/postgres/voicecheck_repo_integration_test.go` (tag `integration`), `cmd/realtime/main_test.go` (HAPUS/balik `TestRealtimeRejectsRealModeInM5` milik M5 T25 — mode real kini diterima — diganti `TestRealtimeAcceptsRealModeWithKeys` dan `TestRealtimeRejectsRealModeWithoutKeys`)

**Interfaces:**
- Consumes: Task 14 (`ActiveVendor`, `Build`); `postgres` pool; M3 tabel `characters` (kolom `voice` JSONB), seed §6.13.
- Produces: `func ValidateReal(cfg provider.RouterConfig, caps []provider.Capability) error` (hanya kapabilitas mode `real`; mode `fake` → flag kapabilitas itu tidak divalidasi, §8.5.1): (1) **tepat satu** STT `true` dan **tepat satu** LLM `true` (via `ActiveVendor`; pesan menyebut semua nama variabel); (2) TTS: minimal satu dari `ELEVENLABS_TTS_ENABLED`/`CARTESIA_TTS_ENABLED` `true`; (3) `OPENAI_EMBEDDING_ENABLED=true`; (4) kunci API per vendor yang punya kapabilitas enabled: `OPENAI_API_KEY` (STT/LLM OpenAI **atau Embedding — selalu wajib di mode real**), `DEEPSEEK_API_KEY` (bila `DEEPSEEK_LLM_ENABLED`), `ELEVENLABS_API_KEY` (STT atau TTS ElevenLabs enabled), `CARTESIA_API_KEY` (STT atau TTS Cartesia enabled); (5) model non-kosong untuk kapabilitas vendor enabled (`OPENAI_STT_MODEL`/`ELEVENLABS_STT_MODEL`/`CARTESIA_STT_MODEL`, `OPENAI_LLM_MODEL`/`DEEPSEEK_LLM_MODEL`, `OPENAI_EVALUATION_MODEL`/`DEEPSEEK_EVALUATION_MODEL`, `OPENAI_EMBEDDING_MODEL`); `CARTESIA_VERSION` tidak boleh kosong bila ada kapabilitas Cartesia enabled; `EMBEDDING_DIMENSIONS=1536`; nilai boolean tidak valid ditolak; pesan menyebut nama env, tidak pernah nilainya; di paket `content` (M3; tanpa impor `provider`/infra, vendor berupa `string` UPPERCASE): `type PlaceholderVoice struct{ CharacterCode, Slot, Provider string; PlaceholderVoiceID, PlaceholderModel bool }` (`Slot` = `PRIMARY` untuk `characters.provider_name/model_name/voice_id`, `FALLBACK` untuk `characters.voice.fallback`); `type VoiceChecker interface{ PlaceholderVoices(ctx context.Context, vendors []string) ([]PlaceholderVoice, error) }`; `type VoiceCheckConfig struct{ RealTTS bool; Vendors []string /* vendor TTS enabled, RealDeps.TTSVendors() */ }`; `func ValidateVoices(ctx context.Context, vc VoiceChecker, c VoiceCheckConfig) error` (hanya bila `RealTTS`: karakter `ACTIVE` dengan `provider_name` ∈ `Vendors` yang `voice_id` atau `model_name`-nya `LIKE 'PLACEHOLDER\_%'`, serta `voice.fallback` dengan vendor ∈ `Vendors` yang `voice_id`/`model_name` placeholder → error menyebut `characters.code`, slot, vendor, dan petunjuk `seed/env/<env>/0900_voice_ids.sql` §6.13.3; karakter ACTIVE dengan `provider_name` ∉ `Vendors` TIDAK menggagalkan startup — hanya log WARN berisi `characters.code` dan sakelar `*_TTS_ENABLED` yang mati; resolver §6.9 menolak sesi `CONTENT_MISCONFIGURED`/`CHARACTER_NOT_AVAILABLE`); `postgres.NewVoiceCheckRepo(pool) *VoiceCheckRepo` memenuhi `content.VoiceChecker`. Pemanggil `cmd/realtime` mengisi `VoiceCheckConfig{RealTTS: cfg.ModeFor(provider.CapTTS)=="real", Vendors: deps.TTSVendors()}`; `cmd/api` (tidak mengimpor `providerwire`) menurunkan `Vendors` dari sakelar `ELEVENLABS_TTS_ENABLED`/`CARTESIA_TTS_ENABLED` lewat helper `content.ConfiguredTTSVendors(enabled map[string]bool) []string` (murni, diuji) — sumber yang sama dengan resolver Bab 6 §6.9. `providerwire` hanya memuat `Build`, `ActiveVendor`, `ValidateReal` (dan `BuildFake` milik M5).

- [ ] **Step 1: Write the failing test**
```go
func TestValidateRealExactlyOneSTT(t *testing.T) {
	cfg := realConfig(t); cfg.OpenAISTTEnabled = false
	err := providerwire.ValidateReal(cfg, allCaps); require.ErrorContains(t, err, "OPENAI_STT_ENABLED"); require.ErrorContains(t, err, "ELEVENLABS_STT_ENABLED"); require.ErrorContains(t, err, "CARTESIA_STT_ENABLED")
	cfg = realConfig(t); cfg.ElevenLabsSTTEnabled = true; require.Error(t, providerwire.ValidateReal(cfg, allCaps)) // dua true
	cfg = realConfig(t); cfg.ModeSTT = "fake"; cfg.OpenAISTTEnabled = false; require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // STT fake → flag tak divalidasi
}
func TestValidateRealExactlyOneLLM(t *testing.T) { /* nol/dua true → error menyebut OPENAI_LLM_ENABLED dan DEEPSEEK_LLM_ENABLED */ }
func TestValidateRealTTSAtLeastOne(t *testing.T) { /* kedua *_TTS_ENABLED=false → error menyebut kedua nama; satu true cukup */ }
func TestValidateRealRejectsMissingKeys(t *testing.T) {
	cfg := realConfig(t); cfg.OpenAIAPIKey = ""
	err := providerwire.ValidateReal(cfg, allCaps); require.ErrorContains(t, err, "OPENAI_API_KEY"); require.NotContains(t, err.Error(), "sk-")
	cfg = realConfig(t); cfg.DeepSeekLLMEnabled, cfg.OpenAILLMEnabled = true, false; cfg.DeepSeekAPIKey = ""; require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "DEEPSEEK_API_KEY")
	cfg = realConfig(t); cfg.DeepSeekLLMEnabled, cfg.OpenAILLMEnabled, cfg.OpenAISTTEnabled, cfg.ElevenLabsSTTEnabled = true, false, false, true; cfg.OpenAIAPIKey = ""; require.ErrorContains(t, providerwire.ValidateReal(cfg, allCaps), "OPENAI_API_KEY") // embedding tetap butuh OpenAI
	cfg = realConfig(t); cfg.CartesiaTTSEnabled = false; cfg.CartesiaAPIKey = ""; require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // vendor tak enabled → kunci tak wajib
	cfg = realConfig(t); require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
	cfg = realConfig(t); cfg.ModeLLM = "fake"; cfg.OpenAILLMEnabled = false; require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // LLM fake → tak divalidasi
}
func TestValidateRealRejectsEmptyModel(t *testing.T) { /* OpenAISTTModel="" dengan OpenAI STT aktif → error menyebut OPENAI_STT_MODEL; model vendor tak aktif boleh kosong */ }
func TestPlaceholderVoiceRejectedAtStartup(t *testing.T) { // (di internal/content/voicecheck_test.go) VoiceChecker fake mengembalikan {CharacterCode:"HARUKA", Slot:"PRIMARY", Provider:"ELEVENLABS", PlaceholderVoiceID:true, PlaceholderModel:true}
	err := content.ValidateVoices(ctx, fakeChecker{...}, content.VoiceCheckConfig{RealTTS: true, Vendors: []string{"ELEVENLABS", "CARTESIA"}}); require.ErrorContains(t, err, "HARUKA"); require.ErrorContains(t, err, "0900_voice_ids.sql")
	require.NoError(t, content.ValidateVoices(ctx, fakeChecker{...}, content.VoiceCheckConfig{RealTTS: false, Vendors: []string{"ELEVENLABS"}})) // mode TTS fake → tidak diperiksa
}
func TestValidateVoicesIgnoresDisabledVendor(t *testing.T) { /* checker hanya dipanggil dengan Vendors enabled: karakter ber-provider_name CARTESIA dengan CARTESIA_TTS_ENABLED=false → tidak error (WARN dicatat); placeholder di voice.fallback vendor enabled → error slot FALLBACK */ }
func TestConfiguredTTSVendors(t *testing.T) { /* map sakelar {ELEVENLABS:true, CARTESIA:true} → [CARTESIA ELEVENLABS] terurut UPPERCASE; {ELEVENLABS:false, CARTESIA:true} → [CARTESIA] */
}
```
Integration (`-tags integration`): `TestVoiceCheckRepoFindsPlaceholders` — `testutil.NewPostgres(t)` + apply `seed/*.sql` (helper seed M3 `testutil.ApplySeed(t, pool)`; jika tidak ada, jalankan file seed lewat `pool.Exec`): `PlaceholderVoices(ctx, []string{"ELEVENLABS","CARTESIA"})` → 4 baris `Slot=PRIMARY` (HARUKA/KENJI ELEVENLABS, AOI/RIKU CARTESIA; `PlaceholderVoiceID` dan `PlaceholderModel` true); `[]string{"ELEVENLABS"}` → 2 baris; setelah `UPDATE characters SET voice_id='real-1', model_name='real-model'` untuk semua karakter → 0 baris; `UPDATE ... SET voice_id='real-1'` saja → baris tetap ada dengan `PlaceholderVoiceID=false, PlaceholderModel=true`; satu karakter `status='DISABLED'` tidak dihitung; `UPDATE characters SET voice = jsonb_set(voice,'{fallback}','{"provider_name":"CARTESIA","model_name":"PLACEHOLDER_M","voice_id":"PLACEHOLDER_V"}')` pada karakter ber-`provider_name=ELEVENLABS` → satu baris `Slot=FALLBACK` untuk vendor `CARTESIA`; `UPDATE characters SET provider_name='DEEPGRAM'` → ditolak CHECK (`provider_name IN ('ELEVENLABS','CARTESIA')`). `cmd/realtime/main_test.go`: `TestRealtimeRejectsRealModeWithoutKeys` — `PROVIDER_MODE=real` dengan default D-017 tanpa `OPENAI_API_KEY` → `run` mengembalikan error sebelum membuka koneksi DB/Redis (exit code 1 di `main`); `TestRealtimeRejectsTwoSTTEnabled` (`OPENAI_STT_ENABLED=true` + `CARTESIA_STT_ENABLED=true` → exit 1, pesan menyebut ketiga variabel); `PROVIDER_MODE=fake` → tidak memanggil `ValidateReal`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/internal/providerwire/ ./cmd/realtime/ ./internal/content/ -race -run 'ValidateReal|PlaceholderVoice|ConfiguredTTS|Realtime'`; `go test -tags integration ./internal/postgres/ -race -run VoiceCheck`. Expected: FAIL.
- [ ] **Step 3: Implement** query sqlc `-- name: PlaceholderVoices :many` (dua SELECT di-`UNION ALL`: `SELECT c.code, 'PRIMARY' AS slot, c.provider_name AS provider, (c.voice_id LIKE 'PLACEHOLDER\_%') AS ph_voice, (c.model_name LIKE 'PLACEHOLDER\_%') AS ph_model FROM characters c WHERE c.status='ACTIVE' AND c.provider_name = ANY($1) AND (c.voice_id LIKE 'PLACEHOLDER\_%' OR c.model_name LIKE 'PLACEHOLDER\_%')` dan `SELECT c.code, 'FALLBACK', c.voice->'fallback'->>'provider_name', (c.voice->'fallback'->>'voice_id' LIKE 'PLACEHOLDER\_%'), (c.voice->'fallback'->>'model_name' LIKE 'PLACEHOLDER\_%') FROM characters c WHERE c.status='ACTIVE' AND c.voice->'fallback'->>'provider_name' = ANY($1) AND (… placeholder …)`); `make sqlc`. Wiring (`cmd/realtime/providers.go`, `cmd/worker`): panggil `providerwire.ValidateReal` setelah config parse (sebelum koneksi apa pun), lalu `Build` untuk kapabilitas mode real dan `BuildFake` untuk sisanya, gabungkan ke `RouterDeps` per kapabilitas (`RouterDeps.TTS` = semua adapter TTS enabled; pemilihan per karakter oleh router dari `VoiceSelection`); `cmd/realtime` dan `cmd/api` memanggil `content.ValidateVoices` setelah pool siap (api memakai pool-nya sendiri dan `content.ConfiguredTTSVendors`; worker tidak memakai TTS). Kegagalan → log ERROR tanpa secret + exit code 1.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make sqlc-check lint arch-check` → exit 0; `PROVIDER_MODE=fake make run-realtime` masih start (smoke manual 5 detik).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(cmd): validasi startup mode real (tepat satu STT/LLM), tolak voice ID placeholder, wiring adapter"`

---

### Task 16: Test `-tags live` (kunci asli) + mode `-record` — **MANUAL GATE (kunci API vendor)**

**Files:**
- Create: `internal/provider/{openai,deepseek,elevenlabs,cartesia}/live_test.go` (`//go:build live`), `internal/provider/live_helpers_test.go` (`//go:build live`, paket `provider_test`)
- Test: file yang sama; **butuh** (satu test live per vendor; tiap test `t.Skip` bila kuncinya tidak ada, sehingga vendor backup boleh diuji terpisah): `OPENAI_API_KEY` (STT, LLM, Evaluation, Embedding), `DEEPSEEK_API_KEY` (LLM, Evaluation), `ELEVENLABS_API_KEY` (STT, TTS), `CARTESIA_API_KEY` (STT, TTS), `LIVE_ELEVENLABS_VOICE_ID`, `LIVE_CARTESIA_VOICE_ID`, `LIVE_TTS_MODEL_ELEVENLABS`, `LIVE_CARTESIA_TTS_MODEL` (dipakai `TestLiveCartesiaTTS`, file milik Task 11), `LIVE_STT_MODEL_OPENAI`, `LIVE_STT_MODEL_ELEVENLABS`, `LIVE_STT_MODEL_CARTESIA`, `LIVE_LLM_MODEL_OPENAI`, `LIVE_LLM_MODEL_DEEPSEEK`, `LIVE_EVAL_MODEL_OPENAI`, `LIVE_EVAL_MODEL_DEEPSEEK` — dibaca hanya lewat `testutil.RequireEnv`.

**Interfaces:**
- Consumes: Task 2–12, `testutil.RequireEnv`, `testdata/audio/{ja_greeting_short,ja_self_intro,ja_question,ja_filler_pause,ja_long_utterance}.wav+.txt` — HANYA rekaman penutur asli di `testdata/audio/` (M5 menyimpan WAV sintetis di `testdata/audio/synth/`; test live TIDAK memakai `synth/` dan task ini TIDAK menimpa/mengubah isinya) (§14.5; resample 48→16 kHz dengan `provider.ResamplePCM16`; **bila fixture belum direkam penutur asli → MANUAL GATE: rekam ≥ 4 fixture asli**), flag test `-record` (`flag.Bool("record", false, …)` di `live_helpers_test.go`; bila true, `replay.NewRecorder` dipasang ke `http.Client.Transport`/`Config.Tap` dan menulis ke `testdata/recordings/<cap>_<case>.jsonl` dengan `meta.verified=true`).
- Produces: kriteria §14.6 sebagai test: STT (OpenAI, ElevenLabs, dan Cartesia — masing-masing sebagai vendor yang dites, bukan sebagai fallback) CER ≤ 0,15 untuk 5 fixture (CER = Levenshtein rune setelah membuang spasi & `。、？！?!,.・「」`; helper `cer(a,b string) float64` di test, diuji `TestCER`); LLM (OpenAI & DeepSeek) 10 prompt golden → teks Jepang polos tanpa tag/markup yang dipotong `SentenceParser` (M4/M5) menjadi kalimat valid (D-081/D-014), first token < 2 s; TTS (ElevenLabs dan Cartesia via Task 11) kalimat 30 karakter dengan `Model`/`VoiceID`/`SpeakingRate` dari env `LIVE_*` (bukan dari profile) → ≥ 0,5 s PCM16 24 kHz per voice; embedding dimensi 1536; evaluation OpenAI (strict `json_schema`) & DeepSeek (`json_object`; skema divalidasi di sisi test dengan decoder ketat karena vendor tidak menegakkan) : skema kecil → JSON valid; satu run < 5 menit.

- [ ] **Step 1: Write the failing test**
```go
//go:build live
func TestLiveElevenLabsSTT(t *testing.T) {
	env := testutil.RequireEnv(t, "ELEVENLABS_API_KEY", "LIVE_STT_MODEL_ELEVENLABS")   // t.Skip bila kosong
	for _, name := range []string{"ja_greeting_short", "ja_self_intro", "ja_question", "ja_filler_pause", "ja_long_utterance"} {
		got := transcribe(t, elevenSTT(t, env), loadPCM16k(t, name)); require.LessOrEqual(t, cer(got, loadRef(t, name)), 0.15, name)
	}
}
func TestCER(t *testing.T) { /* tanpa tag live (helper di file tanpa tag): cer("こんにちは","こんにちわ") == 0.2 ; spasi & 。 diabaikan */ }
```
Tambah: satu test live per baris kriteria di atas (`TestLiveOpenAISTT`, `TestLiveCartesiaSTT` (di `internal/provider/cartesia/live_test.go`, ikut run `-record` Task ini), `TestLiveOpenAILLM`, `TestLiveDeepSeekLLM`, `TestLiveElevenLabsTTS`, `TestLiveCartesiaTTS` (file milik Task 11), `TestLiveOpenAIEmbedding`, `TestLiveOpenAIEvaluation`, `TestLiveDeepSeekEvaluation` (juga memverifikasi konten kosong sesekali → `INVALID_RESPONSE`, bukan panik), `TestLiveOpenAISTTSessionLimit` bila Step 0 Task 4 mendokumentasikan batas sesi yang dapat dipicu murah) dan `TestLiveRecordMode` (hanya jalan dengan `-record`).
- [ ] **Step 2: Run it to verify it fails/skips** — tanpa kunci: `make test-live` → semua `SKIP`, exit 0 (bukan FAIL); `go test ./internal/provider/... -tags live -run TestCER -race` → FAIL sebelum helper ada.
- [ ] **Step 3: Implement** helper (`transcribe`, `loadPCM16k`, `cer`, `loadRef`) dan test live. **MANUAL GATE:** operator mengisi `.env` dengan kunci nyata, menjalankan `go test -race -count=1 -tags=live -timeout 10m ./internal/provider/... -record` satu kali, me-review rekaman yang ditulis (tidak ada secret/PII; `grep -ri 'sk-\|xi-api\|Bearer' internal/provider/*/testdata`), mengganti fixture berbasis dokumentasi (`verified=false`) dengan rekaman nyata, dan menjalankan ulang seluruh test recorded (`make test`) — bila ada perbedaan format nyata vs fixture awal, perbaiki adapter + spec bab 8 + `docs/implementation-deviations.md`.
- [ ] **Step 4: Run tests to verify they pass** — `make test-live` (dengan kunci) → semua PASS < 5 menit; `make test` → PASS dengan fixture rekaman nyata; `grep -rc '"verified":false' internal/provider/*/testdata` → 0 untuk fixture yang direkam ulang.
- [ ] **Step 5: Commit** — `git add internal/provider && git commit -m "test(provider): live contract test per adapter dan mode -record; ganti fixture dengan rekaman nyata"`

---

### Task 17: Sesi e2e `PROVIDER_MODE=real` skala kecil — **MANUAL GATE (kunci API + voice ID nyata)**

**Files:**
- Create: `test/e2e/real/run.sh`, `test/e2e/real/real_test.go` (`//go:build e2e && live`)
- Modify: `Makefile` (target `e2e-real`)
- Test: `test/e2e/real/real_test.go`

**Interfaces:**
- Consumes: M5 `bin/realtime`, `bin/api`, `bin/kotoki-cli converse` (flag §14.11.2), `testdata/audio/ja_self_intro.wav` (rekaman asli, bukan `synth/`); Task 14–15; `provider_name`/`model_name`/`voice_id` nyata per karakter dari Task 23 (sebelum itu gunakan nilai sementara di `seed/env/local/0900_voice_ids.sql` yang meng-UPDATE kolom `characters` dari akun operator, tidak di-commit).
- Produces: `make e2e-real` = `test/e2e/real/run.sh`: start Postgres/Redis/RabbitMQ (`docker compose up -d --wait`), `make migrate-up seed`, `seed/env/local/0900_voice_ids.sql`, jalankan `bin/api` & `bin/realtime` dengan `PROVIDER_MODE=real` dan sakelar default D-017 (`OPENAI_STT_ENABLED=true`, `OPENAI_LLM_ENABLED=true`, `ELEVENLABS_TTS_ENABLED=true`, `CARTESIA_TTS_ENABLED=true`; varian lain via `E2E_STT`/`E2E_LLM` = `OPENAI|ELEVENLABS|CARTESIA` / `OPENAI|DEEPSEEK` yang memasang tepat satu flag `true` — untuk mengesahkan backup manual), `kotoki-cli register/login`, `kotoki-cli converse --wav testdata/audio/ja_self_intro.wav --duration 300 --end after-last --out-dir out/e2e-real`, lalu cek keluaran; exit 2 bila kunci hilang (pesan menyebut variabel, bukan nilainya).

- [ ] **Step 1: Write the failing test**
```go
//go:build e2e && live
func TestRealProviderSingleSession(t *testing.T) {
	testutil.RequireEnv(t, "OPENAI_API_KEY", "ELEVENLABS_API_KEY", "CARTESIA_API_KEY")  // + DEEPSEEK_API_KEY bila E2E_LLM=DEEPSEEK; sesi diulang untuk satu karakter ELEVENLABS dan satu CARTESIA
	out := t.TempDir(); cmd := exec.Command("test/e2e/real/run.sh"); cmd.Env = append(os.Environ(), "OUT_DIR="+out)
	b, err := cmd.CombinedOutput(); require.NoError(t, err, string(b))
	wavs, _ := filepath.Glob(filepath.Join(out, "turn-*-gen-*.wav")); require.NotEmpty(t, wavs)
	fi, _ := os.Stat(wavs[0]); require.Greater(t, fi.Size(), int64(48000)) // > 0,5 s PCM16 48 kHz
	var sum struct{ Turns []struct{ TurnLatencyMs int64 `json:"turn_latency_ms"` } }; mustJSON(t, filepath.Join(out, "summary.json"), &sum)
	require.NotEmpty(t, sum.Turns); require.Greater(t, sum.Turns[0].TurnLatencyMs, int64(0))
	require.NotContains(t, string(b), os.Getenv("OPENAI_API_KEY"))
}
```
- [ ] **Step 2: Run it to verify it fails/skips** — `go test -tags 'e2e live' ./test/e2e/real/ -race` tanpa kunci → SKIP; dengan kunci sebelum `run.sh` ada → FAIL.
- [ ] **Step 3: Implement** `run.sh` (`set -euo pipefail`, trap untuk mematikan proses & `docker compose down -v`) dan target `e2e-real`. **MANUAL GATE:** operator menjalankan `make e2e-real` dengan kunci nyata dan karakter ber-voice nyata (Task 23; satu sesi per vendor TTS yang dipakai karakter, termasuk Cartesia; ditambah satu sesi per vendor STT/LLM backup yang akan dipakai pemilik produk bila diminta, dengan hanya satu flag aktif), mendengarkan `out/e2e-real/turn-001-gen-1.wav` dan mencatat verdict (jelas/Jepang/sesuai karakter) serta `turn_latency_ms` di `docs/implementation-log.md`.
- [ ] **Step 4: Run tests to verify they pass** — `make e2e-real` → exit 0, `ls out/e2e-real/*.wav` ≥ 1 file, `jq '.turns|length' out/e2e-real/summary.json` ≥ 1; `make test-e2e` (fake) tetap PASS.
- [ ] **Step 5: Commit** — `git add test/e2e/real Makefile && git commit -m "test(e2e): sesi real-provider skala kecil (make e2e-real)"`

---

### Task 18: `provider-bench` — inti metrik, gate, dan seleksi model (pure unit)

**Files:**
- Create: `cmd/kotoki-cli/internal/bench/{manifest.go,metrics.go,gates.go,select.go,models.go}`
- Test: `cmd/kotoki-cli/internal/bench/{metrics_test.go,gates_test.go,select_test.go,manifest_test.go}`

**Interfaces:**
- Consumes: `provider.TokenUsage`, `provider.PriceTable`.
- Produces (nama dari plan): `type Item struct{ ID, Set, AudioURI, ReferenceText, Speaker string; DurationMs int64; Tags []string; Messages []provider.Message \`json:"messages,omitempty"\` }`; `LoadManifest(path string) ([]Item, error)` (menolak `set` di luar `STT-native|STT-learner|STT-noise|LLM-context|LLM-json|TTS-sentences`, `speaker` di luar `native|learner`, id ganda); `NormalizeForCER(s string) string` (NFKC-setara tanpa `x/text`: konversi lebar-penuh→setengah-lebar untuk angka/latin, hapus tanda baca & spasi, katakana→hiragana); `CER(ref, hyp string) float64` (Levenshtein rune / `len(ref)`); `Percentile(xs []float64, p float64) float64` (nearest-rank); `type Candidate struct{ Capability provider.Capability; Provider provider.Name; Model, Voice string; Role string /*hanya TTS: PRIMARY|FALLBACK (gate voice utama vs `voice.fallback`); STT/LLM tidak punya peran (satu set gate, D-017)*/; Characters []string /*khusus TTS: kode karakter yang dicalonkan, mis. HARUKA; kosong = semua*/ }`; `type Metrics struct{ P50,P95 map[string]float64; CERNative, CERLearner, ErrorRate, FormatOK, QualityMean, MOS, KanjiErrRate, JSONFirstPassOK, JSONAfterRepairOK float64; CostPerMinUSD float64 }`; `type Gate struct{ Name string; Pass bool; Actual, Limit float64 }`; `EvalGates(cap provider.Capability, role string, m Metrics) []Gate` (nilai di Global Constraints; `role` hanya dipakai TTS; LLM memuat gate `JSONAfterRepairOK ≥ 98%`); `CostPerMinute(p provider.PriceTable, c Candidate, u Usage) (float64, bool)` dengan asumsi §8.10.3 (6 turn, 60 s audio STT, 400 token input + 80 output per turn, 60 karakter TTS per turn); `RecommendSTT(passed []ScoredCandidate) (rec *ScoredCandidate, backups []ScoredCandidate, notes []string)` (hanya **rekomendasi**, bukan pemilihan otomatis: kandidat STT yang lolos semua gate dengan CER terendah; sisanya dilaporkan sebagai backup manual beserta angkanya; kosong → catatan "tidak ada yang lolos, laporkan angka aktual; gate tidak diturunkan"); `RecommendLLM(passed []ScoredCandidate, maxCostPerMin float64) (rec *ScoredCandidate, backups []ScoredCandidate, notes []string)` (kualitas manusia tertinggi yang lolos gate dengan biaya per menit ≤ batas (default ≤ 3× kandidat lolos termurah); vendor lain yang lolos = backup manual; tidak ada tier/profile TTS, D-015/D-016); `type CharacterSpec struct{ Code, Gender, Accent string }` dan `type CharacterVoice struct{ Code string; Primary VoiceChoice; Fallback *VoiceChoice; Notes []string }` / `type VoiceChoice struct{ ProviderName, ModelName, VoiceID string }`; `RecommendCharacterVoices(passed []ScoredCandidate, chars []CharacterSpec) []CharacterVoice` (rekomendasi TTS per karakter, D-015, vendor hanya `ELEVENLABS`/`CARTESIA`: per karakter pilih dari kandidat TTS yang lolos gate PRIMARY dan mencalonkan karakter itu — MOS tertinggi, tie → biaya per menit terendah → TTFA P95 terendah; `Fallback` = kandidat terbaik berikutnya yang lolos gate FALLBACK dengan **vendor berbeda** dari `Primary` (D-015), `nil` bila tidak ada → `Notes` memuat "tanpa fallback TTS: kegagalan → APOLOGY (D-042)"; tidak ada yang lolos → `Notes` "tidak ada yang lolos, laporkan angka aktual; gate tidak diturunkan" dan `Primary` kosong; nilai `ProviderName` UPPERCASE, `ModelName`/`VoiceID` apa adanya dari `bench/models.yaml`); `FormatOK(text string) bool` (teks Jepang polos: tanpa tag/markup (`<`…`>`), tanpa `{`/`[`/`**`/pagar kode, rasio rune Jepang ≥ 0,9 dari rune non-spasi; D-014).

- [ ] **Step 1: Write the failing test**
```go
func TestCER(t *testing.T) {
	require.InDelta(t, 0.0, bench.CER("こんにちは。", "コンニチハ"), 1e-9)    // katakana→hiragana, tanda baca dibuang
	require.InDelta(t, 0.25, bench.CER("今日は雨", "今日は晴れ"), 0.2)         // lihat nilai persis di bawah
	require.InDelta(t, 1.0/6, bench.CER("日本語の勉強", "日本語の勉"), 1e-9)
}
func TestGatesSingleSetForSTTAndLLM(t *testing.T) {
	m := bench.Metrics{P95: map[string]float64{"stt_final_ms": 320}, CERNative: 0.07, CERLearner: 0.14, ErrorRate: 0.005}
	g := bench.EvalGates(provider.CapSTT, "", m); require.False(t, passed(g, "stt_final_ms"))  // 320 > 300; tidak ada gate fallback 450 ms untuk STT/LLM
	g = bench.EvalGates(provider.CapTTS, "FALLBACK", bench.Metrics{P95: map[string]float64{"tts_first_audio_ms": 550}, MOS: 3.6, KanjiErrRate: 0.04}); require.True(t, allPassed(g)) // gate voice.fallback tetap ada hanya untuk TTS
}
func TestRecommendSTT(t *testing.T) { /* OPENAI CER 6%, CARTESIA CER 5% lolos, ELEVENLABS gagal gate → rec=CARTESIA, backups=[OPENAI] dengan angka; semua gagal → rec nil + catatan "gate tidak diturunkan" */ }
func TestRecommendLLM(t *testing.T) { /* A (OPENAI) cost 1 quality 4.1, B (OPENAI) cost 2.5 quality 4.6, C (DEEPSEEK) cost 4 quality 4.9 (>3x batas), D (DEEPSEEK) gagal gate JSON → rec=B; backups memuat kandidat lolos vendor lain bila ada */ }
func TestRecommendCharacterVoices(t *testing.T) { /* HARUKA: ELEVENLABS MOS 4.5, CARTESIA MOS 4.3 → Primary=ELEVENLABS, Fallback=CARTESIA (vendor berbeda); AOI: hanya CARTESIA lolos → Primary=CARTESIA, Fallback=nil + catatan D-042; kandidat yang tidak mencalonkan karakter itu diabaikan; KENJI tanpa kandidat lolos → Primary kosong + catatan "gate tidak diturunkan" */ }
```
Ganti baris kedua `TestCER` dengan nilai tetap: `CER("今日は雨", "今日は雪") == 0.25`. Tambah: `TestPercentileNearestRank` (`[1..100]`, p95 → 95); `TestLoadManifestRejectsBadSet`; `TestFormatOK` (teks Jepang polos lolos; teks berisi tag `<...>` / `**` / JSON / Inggris murni gagal); `TestCostPerMinuteUsesAssumptions` (harga uji LLM 1 USD/1M input & 2 USD/1M output, TTS 10 USD/1M char, STT 0,6 USD/jam → `6×(400/1e6×1+80/1e6×2)+6×60/1e6×10+60/3600×0.6`); `TestRecommendNoneQualifies` (notes menyebut "gate tidak diturunkan"); `TestRecommendCharacterVoicesNeverSameVendorFallback`; `TestRecommendNeverAutoSelects` (fungsi hanya mengembalikan rekomendasi + backup; tidak ada fungsi yang menulis `*_ENABLED`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kotoki-cli/internal/bench/ -race`. Expected: FAIL.
- [ ] **Step 3: Implement** murni (tanpa jaringan/waktu). `models.go`: parser `bench/models.yaml` memakai `gopkg.in/yaml.v3` (`candidates: [{capability, provider, model, voice, role, characters}]`, `KnownFields(true)`, `provider`/`capability` UPPERCASE; `provider` ∈ `OPENAI|DEEPSEEK|ELEVENLABS|CARTESIA|FAKE_PRIMARY` — vendor lain ditolak).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/kotoki-cli/internal/bench/ -race -cover` → PASS, cover ≥ 85%.
- [ ] **Step 5: Commit** — `git add cmd/kotoki-cli/internal/bench && git commit -m "feat(kotoki-cli): inti benchmark provider (CER, gate, rekomendasi STT/LLM/voice)"`

---

### Task 19: `provider-bench` — runner suite, pencatat hasil, output file config

**Files:**
- Create: `cmd/kotoki-cli/internal/bench/{runner_stt.go,runner_llm.go,runner_tts.go,runner_e2e.go,runner_conc.go,output.go}`, `cmd/kotoki-cli/provider_bench.go`
- Modify: `cmd/kotoki-cli/main.go` (daftarkan subcommand `provider-bench`, `provider-report`)
- Test: `cmd/kotoki-cli/internal/bench/runner_test.go`, `output_test.go`, `cmd/kotoki-cli/provider_bench_test.go`

**Interfaces:**
- Consumes: Task 18, M4 `fake.*` (di test), `providerwire.Build` (di `provider_bench.go`), `objectstore`-free: audio dibaca dari `file://`/path lokal atau `s3://` lewat `objectstore` bila `--audio-root` bukan lokal (gunakan `--audio-root <dir>` hasil `aws s3 sync`; `objectstore` tidak diimpor).
- Produces: CLI `kotoki-cli provider-bench --suite stt|llm|llm-json|tts|e2e|concurrency|all --dataset testdata/benchmark --models-file bench/models.yaml --out bench/results/<tanggal>/ --repeat 3 [--audio-root DIR] [--turn-end-ms 0] [--concurrency-steps 10,50,100,200]`; fungsi `RunSTT(ctx, p provider.STTProvider, c Candidate, items []Item, audio AudioSource, repeat int, clk clock.Clock) []Result` (connect/first-partial/final-after-Finalize/CER/jumlah revisi partial; audio dikirim 20 ms-paced bila `Pacing=true`), `RunLLM(ctx, p provider.LLMProvider, …)` (TTFT, time-to-first-sentence sampai `。！？`, total, `FormatOK`, panjang rune), `RunLLMJSON(ctx, p provider.EvaluationProvider, c Candidate, items []Item, validate func(schemaName string, raw []byte) error, repeat int) []Result` (suite `LLM-json`: transkrip dengan prompt EVALUATION dan MEMORY_EXTRACTION nyata → `GenerateJSON`; mencatat `% JSON valid & lolos validator kode pada panggilan pertama` dan `% lolos setelah 1 repair` (maks 2 panggilan, sama dengan M7/M8), `invalid_response` retryable, konten kosong; DeepSeek `json_object` dibandingkan dengan OpenAI strict), `RunTTS(ctx, p provider.TTSProvider, …)` (TTFA, RTF = waktu total/durasi audio, bytes; per kandidat TTS = (vendor, model, voice) dan per karakter yang dicalonkan — bukan per profile; `req.Model`/`req.VoiceID` diisi dari kandidat apa adanya seperti router mengisi dari `characters`), `RunE2E(...)` (komposisi STT→LLM→TTS in-process untuk kombinasi 3 STT × 2 LLM × TTS karakter; `turn_end_to_first_audio_ms = stt_final + llm_first_sentence + tts_first_audio + turn_end_ms` — **penyimpangan dari §8.10.3** (tanpa VAD/turn engine nyata): dicatat di `docs/implementation-deviations.md`), `RunConcurrency(ctx, steps []int, call func(ctx) error, clk)` (ramp bertahap; P95, % `KindRateLimited`, % error); `WriteResults(dir string, rs []Result) error` (`results.jsonl` satu baris per percobaan, `summary.csv` per kandidat+suite berisi metrik & hasil gate), `WriteRecommendationEnv(path string, stt, llm *ScoredCandidate, sttModels, llmModels, evalModels map[provider.Name]string, concurrencyComments map[string]string) error` (format §8.10.5 butir 1: `deploy/config/provider.env` memuat **rekomendasi** sakelar (`OPENAI_STT_ENABLED`/`ELEVENLABS_STT_ENABLED`/`CARTESIA_STT_ENABLED` dengan tepat satu `true` = kandidat rekomendasi; `OPENAI_LLM_ENABLED`/`DEEPSEEK_LLM_ENABLED` tepat satu `true`; `ELEVENLABS_TTS_ENABLED`/`CARTESIA_TTS_ENABLED` sesuai vendor yang dipakai `character-voices.json`), model per vendor (`OPENAI_STT_MODEL`, `ELEVENLABS_STT_MODEL`, `CARTESIA_STT_MODEL`, `OPENAI_LLM_MODEL`, `DEEPSEEK_LLM_MODEL`, `OPENAI_EVALUATION_MODEL`, `DEEPSEEK_EVALUATION_MODEL`), `LLM_MAX_OUTPUT_TOKENS`, `LLM_TEMPERATURE`, `ROLLING_SUMMARY_LLM_MODEL`, `PROVIDER_CONCURRENCY_<VENDOR>_<CAP>` (STT/LLM/TTS/EVALUATION; termasuk `PROVIDER_CONCURRENCY_CARTESIA_STT`/`_TTS`) + komentar limit plan akun; vendor backup ditulis sebagai baris komentar `# backup manual: <flag>=true` beserta angka benchmark; header file menyatakan "rekomendasi; keputusan akhir vendor aktif manual pemilik produk" (**tanpa** sufiks tier, `PROVIDER_PROFILE_*`, `PROVIDER_STT_*`/`PROVIDER_LLM_*` lama, atau fallback TTS global, D-015/D-016/D-017), `WriteCharacterVoices(path string, cv []CharacterVoice) error` (menulis `bench/results/<tanggal>/character-voices.json`: per karakter `provider_name`, `model_name`, `voice_id`, `fallback{…}`; masukan manual untuk `seed/env/<env>/0900_voice_ids.sql`, Task 23), `UpdatePricesYAML` TIDAK ditulis otomatis (manual, Task 22).

- [ ] **Step 1: Write the failing test** (runner diuji dengan `fake` + `clock.NewFake`, tanpa jaringan)
```go
func TestRunLLMMeasuresFirstSentence(t *testing.T) {
	l := fake.NewLLM(provider.FakePrimary, fastCap(), fake.Config{}, fakeDeps(t))   // opening template berisi "。"
	rs := bench.RunLLM(ctx, l, bench.Candidate{Provider: provider.FakePrimary, Model: "fake"}, []bench.Item{{ID: "c1", Set: "LLM-context", Messages: msgs}}, 2)
	require.Len(t, rs, 2); require.Greater(t, rs[0].Values["first_sentence_ms"], 0.0); require.True(t, rs[0].Flags["format_ok"])
}
func TestRunSTTComputesCERAndFinalLatency(t *testing.T) { /* fake STT mode fixture dengan `testdata/audio/synth/` WAV+txt (sintetis M5; hanya dibaca) → CER 0 untuk item yang cocok; final_after_finalize_ms terisi */ }
func TestWriteRecommendationEnvFormat(t *testing.T) { /* golden: tepat satu *_STT_ENABLED=true, tepat satu *_LLM_ENABLED=true, baris "OPENAI_LLM_MODEL=<m>"/"DEEPSEEK_LLM_MODEL=…", "CARTESIA_STT_MODEL=…", "PROVIDER_CONCURRENCY_CARTESIA_TTS=…", komentar "# limit plan akun: …" dan "# backup manual: …"; header "keputusan akhir manual"; tanpa secret; tidak memuat kunci *_API_KEY; tidak memuat "PROVIDER_TTS_", "PROVIDER_PROFILE_", "PROVIDER_LLM_", "PROVIDER_STT_", "DEEPGRAM", "GEMINI" */ }
func TestRunLLMJSONFirstPassAndRepair(t *testing.T) { /* fake EvaluationProvider skrip: item 1 valid; item 2 invalid lalu valid setelah repair; item 3 invalid dua kali → first_pass_ok=1/3, after_repair_ok=2/3; panggilan per item ≤ 2 */ }
func TestConcurrencyRamp(t *testing.T) { /* call mengembalikan *provider.Error{KindRateLimited} untuk 30% → rate_limited_pct≈30 (±1) pada step 100 */ }
```
Tambah: `TestWriteResultsJSONLAndCSV` (jumlah baris = percobaan; kolom CSV tetap: `suite,capability,provider,model,voice,metric,p50,p95,gate,pass`); `TestProviderBenchFlagValidation` (`--suite x` → exit 2; `--out` kosong → exit 2; `--repeat 0` → exit 2; kandidat dengan vendor tak punya key → pesan menyebut env var; `provider` DEEPGRAM/GEMINI → ditolak sebagai vendor tak dikenal); `TestProviderBenchDryRunWithFake` (`PROVIDER_MODE=fake` + `--models-file` berisi `FAKE_PRIMARY` → `results.jsonl` terisi; tanpa jaringan); `TestBenchDoesNotLogTextOrKeys`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kotoki-cli/... -race -run 'RunLLM|RunSTT|WriteProfileEnv|Concurrency|WriteResults|ProviderBench|BenchDoesNot'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Setiap runner menerima port (bukan vendor); percobaan ke-`repeat` berurutan; error vendor dicatat sebagai baris `error_kind` (bukan abort); `--suite all` urut stt→llm→llm-json→tts→e2e→concurrency (flag `--suite` menerima juga `llm-json`); gate dievaluasi per kandidat (STT/LLM satu set gate; TTS sesuai `role` PRIMARY/FALLBACK di `bench/models.yaml`); ringkasan cetak ke stdout (tanpa teks dataset, hanya id & angka); `cmd/kotoki-cli` membangun adapter lewat `providerwire.Build` dengan `RouterConfig` override per kandidat (model/voice dari YAML).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/kotoki-cli/... -race` → PASS; `CGO_ENABLED=1 go build ./cmd/kotoki-cli`; `./bin/kotoki-cli provider-bench --help` mencantumkan semua flag.
- [ ] **Step 5: Commit** — `git add cmd/kotoki-cli && git commit -m "feat(kotoki-cli): provider-bench (runner stt/llm/llm-json/tts/e2e/concurrency, hasil, rekomendasi provider.env)"`

---

### Task 20: `kotoki-cli provider-report` (§8.8.3)

**Files:**
- Create: `cmd/kotoki-cli/internal/report/{aggregate.go,query.go,csv.go}`, `cmd/kotoki-cli/provider_report.go`
- Test: `cmd/kotoki-cli/internal/report/aggregate_test.go`, `query_integration_test.go` (tag `integration`), `provider_report_test.go`

**Interfaces:**
- Consumes: `pricing.Load`, `provider.PriceTable`, `testutil.NewPostgres`, DDL `session_turns` (§5), `evaluations` bila ada (kolom `overall_score` Bab 10; **bila M7 belum ada, kolom `avg_eval_*` kosong, bukan error**).
- Produces: `kotoki-cli provider-report --from 2026-10-01 --to 2026-10-08 --format csv --prices prices.yaml` (membaca `DATABASE_URL`; koneksi `default_transaction_read_only=on`; `--format csv|json`); `type TurnAgg struct{ SttProvider, LLMProvider, LLMModel, TTSProvider, TTSModel string; Turns int; P50,P95,P99TurnEndToFirstAudioMs, P95SttFinalMs, P95LLMFirstTokenMs, P95TTSFirstAudioMs float64; FailedRate, InterruptedRate, TTSFallbackRate float64; InputTokens, OutputTokens, TTSCharacters, STTAudioMs int64 }`; `Query(ctx, db Querier, from, to time.Time) ([]TurnAgg, error)` (SQL sketsa §8.8.3, `status IN ('COMPLETED','INTERRUPTED','FAILED')`, `percentile_cont`); `Build(aggs []TurnAgg, p provider.PriceTable) []Row` menambah `avg_cost_usd_per_turn` dan `cost_usd_per_active_minute` (menit aktif = `Σ stt_audio_ms`/60000 sebagai pendekatan; dicatat di header CSV komentar) ; `WriteCSV(w io.Writer, rows []Row) error` dengan kolom persis daftar §8.8.3.

- [ ] **Step 1: Write the failing test**
```go
func TestBuildCost(t *testing.T) {
	a := report.TurnAgg{LLMProvider: "OPENAI", LLMModel: "m", TTSProvider: "ELEVENLABS", TTSModel: "t", SttProvider: "ELEVENLABS", Turns: 10, InputTokens: 4000, OutputTokens: 800, TTSCharacters: 600, STTAudioMs: 600000}
	rows := report.Build([]report.TurnAgg{a}, priceStub{llmIn: 1, llmOut: 2, ttsPer1M: 10, sttPerHour: 0.6})
	require.InDelta(t, (4000.0/1e6*1+800.0/1e6*2+600.0/1e6*10+600000.0/3.6e6*0.6)/10, rows[0].AvgCostUSDPerTurn, 1e-9)
}
func TestCSVColumns(t *testing.T) { /* header persis: stt_provider,llm_provider,llm_model,tts_provider,tts_model,turns,p50_turn_end_to_first_audio_ms,p95_…,p99_…,p95_stt_final_ms,p95_llm_first_token_ms,p95_tts_first_audio_ms,failed_rate,interrupted_rate,tts_fallback_rate,avg_cost_usd_per_turn,cost_usd_per_active_minute,avg_eval_overall_score */ }
```
Integration `TestQueryGroupsByStack`: `testutil.NewPostgres` + helper insert sesi & `session_turns` (3 kombinasi stack, status campur COMPLETED/INTERRUPTED/FAILED/DISCARDED, `created_at` dalam/di luar rentang) → baris = 3 kombinasi; DISCARDED & di luar rentang tidak dihitung; `failed_rate` = FAILED/total; P95 `turn_end_to_first_audio_ms` sesuai `percentile_cont`. `TestReportRefusesWrites` (koneksi read-only: `INSERT` gagal). `TestProviderReportFlags` (`--from` > `--to` → exit 2; `DATABASE_URL` kosong → exit 2 menyebut nama variabel).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kotoki-cli/... -race -run 'BuildCost|CSVColumns|ProviderReportFlags'`; `go test -tags integration ./cmd/kotoki-cli/... -race -run 'QueryGroups|RefusesWrites'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Query memakai `pgx` langsung (bukan sqlc: tool, bukan repository fitur; lisan di `docs/implementation-log.md`); `avg_eval_*` di-join hanya bila tabel `evaluations` ada (`to_regclass`).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `./bin/kotoki-cli provider-report --help`.
- [ ] **Step 5: Commit** — `git add cmd/kotoki-cli && git commit -m "feat(kotoki-cli): provider-report (stack × latency/failed/fallback/biaya)"`

---

### Task 21: Dataset golden Jepang + `bench/models.yaml` — **MANUAL GATE (manusia: penutur asli, persetujuan tertulis, reviewer Jepang)**

**Files:**
- Create: `testdata/benchmark/manifest.jsonl`, `testdata/benchmark/README` (format & aturan rekaman, bukan dokumentasi tambahan lain), `bench/models.yaml`
- Test: `cmd/kotoki-cli/internal/bench/manifest_test.go` (menambah `TestRepoManifestValid`)

**Interfaces:**
- Consumes: `bench.LoadManifest`, `bench.LoadModels` (Task 18); bucket privat `s3://kotoki-benchmark/v1/` (§8.10.2); CONVERSATION prompt nyata dari seed (`prompt.Render` pada 30 konteks).
- Produces: dataset sesuai §8.10.2: STT-native ≥ 40 (jawaban pendek `はい`/`そうですね`, kalimat 8–15 s, angka/tanggal/harga, katakana serapan, nama tempat), STT-learner ≥ 40 (pelajar Indonesia N5–N3; filler, jeda, salah ucap), STT-noise 10 (noise kafe/jalan SNR 10 dB), LLM-context 30 (3–8 turn riwayat; field `messages` berisi system prompt CONVERSATION nyata hasil render + riwayat), LLM-json 20 (transkrip sesi dengan prompt EVALUATION dan MEMORY_EXTRACTION nyata + skema; untuk `% JSON lolos validasi` terutama DeepSeek `json_object`), TTS-sentences 50 (pendek, tanya, emosi, kanji sulit, angka; teks polos tanpa tag); subset 10 file berlisensi bebas di `testdata/audio/bench-free/` (+`.txt`) untuk fake-STT CI (tidak menyentuh `testdata/audio/synth/` milik M5); `bench/models.yaml` kandidat (D-017; hanya vendor yang berlaku): STT 3 vendor — `openai` (`gpt-live-transcribe` + model lain yang terverifikasi), `elevenlabs` (`scribe_v2_realtime`), `cartesia` (`ink-2`, dan `ink-whisper` bila terbukti mendukung Jepang); LLM 2 vendor — `openai` (2–3 model) dan `deepseek` (`deepseek-flash`, `deepseek-v4-pro`; thinking `disabled`); TTS **per karakter** (HARUKA, KENJI, AOI, RIKU; field `characters` tiap kandidat): ElevenLabs (≥ 2 model × voice kandidat per gender, mis. model `Eleven v3 Conversational` dan voice `21m00Tcm4TlvDq8ikWAM` dari pemilik produk sebagai titik awal), Cartesia (≥ 2 model Sonic × voice kandidat per gender, mis. voice `db6b0ed5-d5d3-463d-ae85-518a07d3c2b4`; nama model dari docs Cartesia: `sonic-3.6` vs `sonic3.6` diverifikasi di Task 11/22, tidak ditebak). Tidak ada kandidat Deepgram, Gemini, atau OpenAI TTS.

- [ ] **Step 1: Write the failing test**
```go
func TestRepoManifestValid(t *testing.T) { // memuat testdata/benchmark/manifest.jsonl
	items, err := bench.LoadManifest(testutil.TestdataPath(t, "benchmark/manifest.jsonl")); require.NoError(t, err)
	count := map[string]int{}; for _, it := range items { count[it.Set]++; require.NotEmpty(t, it.ReferenceText) }
	require.GreaterOrEqual(t, count["STT-native"], 40); require.GreaterOrEqual(t, count["STT-learner"], 40); require.Equal(t, 10, count["STT-noise"])
	require.GreaterOrEqual(t, count["LLM-context"], 30); require.GreaterOrEqual(t, count["LLM-json"], 20); require.GreaterOrEqual(t, count["TTS-sentences"], 50)
	ms, err := bench.LoadModels(testutil.TestdataPath(t, "../bench/models.yaml")); require.NoError(t, err); require.NotEmpty(t, ms)
}
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kotoki-cli/internal/bench/ -race -run TestRepoManifestValid`. Expected: FAIL (manifest belum ada).
- [ ] **Step 3: Checklist MANUAL GATE** (selesai bila Step 4 lulus): (1) rekrut penutur asli + pelajar Indonesia, persetujuan tertulis tersimpan di luar repo; (2) rekam WAV 16 kHz mono (STT-noise: campur noise SNR 10 dB), upload ke `s3://kotoki-benchmark/v1/`, isi `audio_uri`, `reference_text` diverifikasi dua orang; (3) tulis 30 konteks LLM (render CONVERSATION prompt nyata); (4) tulis 50 kalimat TTS (teks polos) dan 20 sampel `LLM-json` (transkrip + prompt EVALUATION/MEMORY_EXTRACTION nyata); (5) pilih 10 file berlisensi bebas ke `testdata/audio/bench-free/` (`.txt` sidecar; jangan menimpa `testdata/audio/synth/`); (6) tulis `bench/models.yaml` (kandidat 3 STT, 2 LLM, TTS per karakter; role hanya TTS) dari daftar model yang tersedia di akun saat ini (verifikasi di dokumentasi vendor, jangan menebak nama model); (7) tetapkan 2 reviewer berbahasa Jepang untuk penilaian 1–5 (naturalness, kesesuaian level, konsistensi karakter) dan MOS TTS; format lembar penilaian: CSV `item_id,reviewer,candidate,score_naturalness,score_level,score_character,mos,kanji_misread(0|1)` disimpan di `bench/results/<tanggal>/human.csv`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/kotoki-cli/internal/bench/ -race -run TestRepoManifestValid` → PASS; `aws s3 ls s3://kotoki-benchmark/v1/ | wc -l` ≥ 90 objek audio.
- [ ] **Step 5: Commit** — `git add testdata/benchmark bench/models.yaml testdata/audio/bench-free && git commit -m "test(benchmark): dataset golden Jepang (manifest) dan kandidat model"`

---

### Task 22: Jalankan benchmark, verifikasi dukungan Jepang streaming STT, tetapkan rekomendasi model & harga — **MANUAL GATE (kunci API, akun, biaya nyata, penilai manusia)**

**Files:**
- Create: `docs/provider-benchmark.md`, `deploy/config/provider.env` (rekomendasi sakelar STT/LLM + model STT/LLM/Evaluation + concurrency; tanpa TTS), `bench/results/<tanggal>/{results.jsonl,summary.csv,human.csv,character-voices.json}`
- Modify: `internal/provider/pricing/prices.yaml` (harga resmi + tanggal + URL sumber per entri, komentar YAML), `internal/platform/config` default model & `.env.example` hanya bila hasil berbeda dari placeholder §8.5.1, spec Bab 8 §8.4.x/§8.5.1/§8.10 bila ada perbedaan
- Test: `internal/provider/pricing/pricing_test.go` (menambah `TestShippedPricesNonZeroForSelectedModels`), `internal/platform/config` test default

**Interfaces:**
- Consumes: Task 16, 18–20; dataset Task 21; akun vendor dengan kunci (OpenAI, DeepSeek, ElevenLabs, Cartesia); region deployment produksi (benchmark dijalankan dari sana).
- Produces: `docs/provider-benchmark.md` (format keluaran di bawah), `deploy/config/provider.env` (format §8.10.5; **rekomendasi**: tepat satu `*_STT_ENABLED=true`, tepat satu `*_LLM_ENABLED=true`, model, concurrency; vendor backup sebagai komentar), `bench/results/<tanggal>/character-voices.json` (vendor/model/voice TTS + fallback per karakter, `WriteCharacterVoices`), `prices.yaml` terisi (`version` = tanggal benchmark; entri `OPENAI` (STT, LLM, Evaluation, Embedding), `DEEPSEEK` (LLM/Evaluation), `ELEVENLABS` (STT, TTS), `CARTESIA` (STT, TTS) dengan satuan harga sesuai halaman resmi: per token / per jam audio / per karakter).

- [ ] **Step 1: Write the failing test**
```go
func TestShippedPricesNonZeroForSelectedModels(t *testing.T) { // membaca deploy/config/provider.env (parse KEY=VAL) + bench/results/<tanggal terbaru>/character-voices.json + pricing.Load("")
	env := parseEnvFile(t, testutil.TestdataPath(t, "../deploy/config/provider.env"))
	tb, _ := pricing.Load("")
	require.Equal(t, 1, countTrue(env, "OPENAI_STT_ENABLED", "ELEVENLABS_STT_ENABLED", "CARTESIA_STT_ENABLED")); require.Equal(t, 1, countTrue(env, "OPENAI_LLM_ENABLED", "DEEPSEEK_LLM_ENABLED"))
	for _, m := range selectedModels(env) { usd, ok := priceProbe(tb, m); require.True(t, ok, m.String()); require.Greater(t, usd, 0.0, m.String()) }
}
```
(`selectedModels` = model STT, LLM, evaluation milik vendor yang `*_ENABLED=true` di env file, embedding OpenAI, serta pasangan (vendor, model) TTS primary dan `fallback` tiap karakter dari `character-voices.json`; vendor backup tidak diwajibkan berharga non-nol tetapi harganya diisi bila benchmark membandingkannya; `Missing` M4 dipakai sebagai pembanding.)
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/pricing/ -race -run TestShippedPrices`. Expected: FAIL (env file belum ada / harga 0).
- [ ] **Step 3: Checklist MANUAL GATE:**
  1. **Verifikasi dukungan Jepang streaming (Bab 8 "Masalah", D-NEW-8-23) untuk 3 STT:** jalankan `kotoki-cli provider-bench --suite stt --models-file bench/models.yaml --out bench/results/<tanggal>/ --repeat 3`. Keluaran per kandidat: `{vendor, model, "ja" diterima? (ya/tidak + pesan error vendor), CER native, CER learner, P95 final-after-Finalize, ada/tidaknya PARTIAL sebelum commit (OpenAI), jumlah revisi partial}`. Dukungan Jepang model realtime OpenAI (`gpt-live-transcribe`), Scribe realtime, dan Ink (`ink-2`) belum dinyatakan eksplisit di spec: bila salah satu gagal menerima `ja`/CER > gate → catat sebagai tidak layak (tidak dipakai sebagai default maupun backup), lapor pemilik produk; gate tidak diturunkan (§8.10.4 butir 4).
  2. Jalankan `--suite llm`, `--suite llm-json`, `--suite tts`, `--suite e2e` (3 STT × 2 LLM × TTS per karakter), `--suite concurrency` (ramp 10→50→100→200; catat limit plan akun tiap vendor), lalu penilaian manusia (`human.csv`: kualitas LLM rata-rata dua reviewer, MOS TTS, kesalahan baca kanji); hitung ulang gate dengan `kotoki-cli provider-bench --suite all --human bench/results/<tanggal>/human.csv --out …`.
  3. TTS **per karakter** (D-015; tidak ada profile TTS/fallback TTS global): `--suite tts` membandingkan vendor ELEVENLABS vs CARTESIA untuk tiap karakter dengan 50 kalimat set TTS-sentences; verifikasi eksplisit dukungan Jepang & kualitas model Sonic (Cartesia), ID model yang sah (`sonic-3.6` vs `sonic3.6`, `Eleven v3 Conversational`), serta konkurensi plan Cartesia (docs/pricing; catat ke `PROVIDER_CONCURRENCY_CARTESIA_TTS` dan `_STT`). `RecommendCharacterVoices` menetapkan `provider_name`/`model_name`/`voice_id` per karakter dan `voice.fallback` (vendor berbeda; opsional — gate fallback MOS ≥ 3,5 & kanji ≤ 5%). Tulis hasilnya ke `character-voices.json`.
  4. **Rekomendasi STT dan LLM (bukan keputusan otomatis):** `RecommendSTT` memilih dari 3 STT yang lolos semua gate dengan CER terendah; `RecommendLLM` memilih dari OpenAI vs DeepSeek model dengan kualitas manusia tertinggi yang lolos gate (termasuk JSON lolos setelah ≤ 1 repair ≥ 98%) dengan biaya per menit ≤ batas pemilik produk. Vendor lain yang lolos dilaporkan sebagai backup manual beserta angkanya (runbook R10 M10). **Pemilik produk memutuskan** vendor aktif di `.env` produksi; tidak ada skrip yang menulis flag `*_ENABLED` otomatis. Tidak ada yang lolos → laporkan angka aktual, JANGAN turunkan gate.
  5. Isi `prices.yaml` dari halaman harga resmi (komentar: tanggal + URL; Cartesia dan OpenAI STT: pilih satuan sesuai halaman harga resmi, spec §8.7.2), `version` = tanggal; jalankan `kotoki-cli provider-bench` biaya per menit untuk keputusan.
  6. Tulis `deploy/config/provider.env` (`WriteRecommendationEnv`), `character-voices.json` (`WriteCharacterVoices`), dan `docs/provider-benchmark.md` dengan format: (a) ringkasan & tanggal & region; (b) tabel gate per kandidat (`kandidat | metrik | nilai | batas | lulus`) untuk 3 STT, 2 LLM, TTS; (c) tabel rekomendasi STT/LLM (`kapabilitas | vendor | model | biaya per menit | status (rekomendasi/backup manual)`) dan tabel TTS per karakter (`karakter | provider_name | model_name | voice_id | fallback | biaya per menit`); (d) hasil verifikasi dukungan Jepang OpenAI STT/Scribe/Ink/Sonic (ya/tidak + bukti); (e) `% JSON lolos pada panggilan pertama` per vendor LLM; (f) daftar semua item "verifikasi di task adapter" beserta hasil (Task 2–12 Step 0); (g) limit plan akun per vendor → nilai `PROVIDER_CONCURRENCY_*` = `floor(limit × 0,9 / replika)`; (h) penyimpangan & rekomendasi ke pemilik produk, termasuk catatan bahwa keputusan akhir manual. Perbarui spec Bab 8 bila ada perbedaan; `docs/implementation-deviations.md` untuk lokasi dokumen (`docs/provider-benchmark.md` menggantikan `docs/benchmarks/provider-benchmark-<tanggal>.md` pada §8.10.5 butir 4).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/pricing/ ./internal/platform/config/ -race` → PASS; `make modelname-check` → exit 0; `test -s docs/provider-benchmark.md && grep -c 'lulus' docs/provider-benchmark.md` > 0; `make test` → PASS.
- [ ] **Step 5: Commit** — `git add docs deploy bench internal/provider/pricing internal/platform/config .env.example && git commit -m "docs(provider): hasil benchmark, rekomendasi vendor/model, harga resmi, provider.env"`

---

### Task 23: Vendor/model/voice ID nyata per karakter + verifikasi Kansai — **MANUAL GATE (akun ElevenLabs/Cartesia + telinga manusia)**

**Files:**
- Create: `seed/env/<env>/0900_voice_ids.sql` (tiap env: `staging`, `prod`; `local` tidak di-commit)
- Catatan: tidak ada lagi tabel/kapabilitas suara (D-014) dan tidak ada profile TTS (D-015); vendor/model/voice TTS adalah kolom `characters.provider_name`/`model_name`/`voice_id` (+ `characters.voice.fallback` opsional) yang diisi lewat `0900_voice_ids.sql` saja (tanpa file `0910_voice_caps.sql`)
- Test: `internal/postgres/voicecheck_repo_integration_test.go` (menambah `TestEnvVoiceSeedClearsPlaceholders`, tag `integration`)

**Interfaces:**
- Consumes: Task 15 `content.VoiceChecker`/`PlaceholderVoices`, `bench/results/<tanggal>/character-voices.json` dan keputusan Task 22 (vendor, model, voice per karakter), seed §6.13.4 (4 karakter: `HARUKA` FEMALE/KANTO, `KENJI` MALE/KANTO, `AOI` FEMALE/KANSAI, `RIKU` MALE/KANTO; id `01920000-000a-7000-8000-00000000000N`).
- Produces: `0900_voice_ids.sql` berformat §6.13.3: `BEGIN; UPDATE characters SET provider_name = '<ELEVENLABS|CARTESIA>', model_name = '<model_name apa adanya>', voice_id = '<voice_id nyata>', voice = <voice dengan jsonb_set fallback bila ada: {"provider_name","model_name","voice_id"}, vendor ≠ primary>, updated_at = now() WHERE code = '<CODE>' AND (voice_id LIKE 'PLACEHOLDER\_%' OR model_name LIKE 'PLACEHOLDER\_%'); … COMMIT;` satu statement per karakter (4 statement; `voice.gender/accent/speaking_rate` tidak disentuh). Contoh nilai nyata dari pemilik produk boleh dicantumkan sebagai komentar (`21m00Tcm4TlvDq8ikWAM` ElevenLabs, `db6b0ed5-d5d3-463d-ae85-518a07d3c2b4` Cartesia). Dukungan `speed` per model Cartesia diputuskan di Task 11 Step 0(d), bukan di seed.

- [ ] **Step 1: Write the failing test**
```go
func TestEnvVoiceSeedClearsPlaceholders(t *testing.T) { // integration; env dari KOTOKI_SEED_ENV via testutil (staging)
	pool := testutil.NewPostgres(t); applySeed(t, pool); applyEnvSeed(t, pool, "staging")
	got, err := postgres.NewVoiceCheckRepo(pool).PlaceholderVoices(ctx, []string{"ELEVENLABS", "CARTESIA"})
	require.NoError(t, err); require.Empty(t, got)
	// provider_name valid (CHECK) dan voice.fallback (bila ada) memakai vendor berbeda dari primary
	var bad int; require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM characters WHERE status='ACTIVE' AND voice->'fallback'->>'provider_name' = provider_name`).Scan(&bad)); require.Zero(t, bad)
}
```
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres/ -race -run TestEnvVoiceSeed`. Expected: FAIL (4 karakter masih placeholder).
- [ ] **Step 3: Checklist MANUAL GATE (per karakter):** (1) dari `character-voices.json` (Task 22) tetapkan untuk **tiap karakter** `provider_name` + `model_name` + `voice_id` utama dan `voice.fallback` opsional (vendor TTS lain yang enabled; tanpa fallback → kegagalan TTS = APOLOGY, D-042; catat keputusan itu); checklist voice ID = kolom `characters.voice_id` + `voice.fallback.voice_id` untuk keempat karakter (8 ID maksimum), tiap ID ditandai terverifikasi didengar; (2) kriteria pemilihan voice: dialek sesuai `accent` (KANTO standar Tokyo, KANSAI Osaka), gender, lulus gate TTS Task 22; (3) **Kansai (`AOI`, `accent=KANSAI`)**: dua penutur Kansai asli mendengarkan ≥ 10 kalimat sampel per vendor kandidat (ElevenLabs, Cartesia) dan menyatakan "terdengar Kansai: ya/tidak" (format: CSV `character,vendor,sample_id,reviewer,kansai_ok`); bila salah satu reviewer tidak setuju untuk suatu vendor, catat verdict di `docs/provider-benchmark.md` dan laporkan ke pemilik produk (pilih vendor/voice lain atau ubah `accent` karakter lewat `PATCH /admin/content/characters/{id}`); dialek di sisi teks dijaga `persona`; (4) tulis `0900_voice_ids.sql` per env (nilai `model_name` apa adanya dari hasil verifikasi Task 22/11, mis. `Eleven v3 Conversational` atau ID model Cartesia yang terverifikasi); (5) jalankan `make seed KOTOKI_ENV=staging`, lalu `kotoki-cli provider-bench --suite tts` ulang dengan nilai final untuk memastikan gate lulus; (6) catat vendor/model/voice ID per karakter dan verdict di `docs/provider-benchmark.md` (bagian "Voice per karakter"); (7) pastikan sakelar `ELEVENLABS_TTS_ENABLED`/`CARTESIA_TTS_ENABLED` untuk vendor yang dipilih karakter bernilai `true` dan kuncinya (`ELEVENLABS_API_KEY`, `CARTESIA_API_KEY`) tersedia di secret manager (M10) sebelum mode real diaktifkan.
- [ ] **Step 4: Run tests to verify they pass** — `go test -tags integration ./internal/postgres/ -race -run 'VoiceCheck|TestEnvVoiceSeed'` → PASS; `make seed KOTOKI_ENV=staging && psql "$DATABASE_URL" -tAc "select count(*) from characters where status='ACTIVE' and (voice_id like 'PLACEHOLDER\_%' or model_name like 'PLACEHOLDER\_%' or voice->'fallback'->>'voice_id' like 'PLACEHOLDER\_%')"` → `0`; `TestSeedContentIsValid` (M3) tetap PASS dengan `PROVIDER_MODE=real` di env uji.
- [ ] **Step 5: Commit** — `git add seed && git commit -m "feat(seed): vendor, model, dan voice ID TTS nyata per karakter dan env"`

---

## Gerbang milestone

- [ ] `make test` (race, tanpa jaringan) → PASS; semua adapter lulus recorded-fixture + `providertest` (`go test ./internal/provider/... -race -count=2`), `internal/provider/...` coverage ≥ 70% (§14.8).
- [ ] `make lint arch-check modelname-check sqlc-check` → exit 0 (adapter tidak mengimpor `conversation/audio`/`fake`/`pricing`/vendor lain; api/worker `CGO_ENABLED=0`; tidak ada paket `internal/provider/deepgram|gemini`).
- [ ] `make test-integration` → PASS (`VoiceCheck`, `provider-report` query).
- [ ] `grep -rc '"verified":false' internal/provider/*/testdata` → 0 untuk semua fixture yang tercakup rekaman live (Task 16).
- [ ] **MANUAL GATE:** `make test-live` dengan semua kunci (OpenAI, DeepSeek, ElevenLabs, Cartesia; `TestLiveCartesiaSTT`/`TestLiveCartesiaTTS`) → PASS < 5 menit (Task 16).
- [ ] **MANUAL GATE:** `make e2e-real` (default D-017; satu sesi per vendor TTS yang dipakai karakter, plus satu sesi per vendor STT/LLM backup yang diminta pemilik produk dengan hanya satu flag aktif) → ≥ 1 file `turn-*-gen-*.wav` > 0,5 s dan `summary.json` ≥ 1 turn; verdict pendengaran dicatat (Task 17).
- [ ] **MANUAL GATE:** `bench/results/<tanggal>/{results.jsonl,summary.csv,human.csv,character-voices.json}`, `docs/provider-benchmark.md` terisi (tabel gate 3 STT/2 LLM/TTS, rekomendasi STT dan LLM + backup manual, TTS per karakter, `% JSON lolos` per vendor LLM, hasil verifikasi Jepang OpenAI STT/Scribe/Ink/Sonic, limit plan akun termasuk Cartesia), `deploy/config/provider.env` (rekomendasi; tepat satu STT dan satu LLM `true`; tanpa TTS), `prices.yaml` non-nol untuk model terpilih termasuk Cartesia (Task 22); keputusan vendor aktif di `.env` produksi = manual pemilik produk; `provider_name`/`model_name`/`voice_id` nyata + `voice.fallback` per karakter + verdict Kansai (Task 23).
- [ ] `PROVIDER_MODE=real` dengan nol/dua STT atau LLM `true`, kunci vendor enabled kosong, atau `voice_id`/`model_name` placeholder pada karakter ACTIVE yang vendornya enabled → `realtime` exit 1 dengan pesan menyebut env/karakter (Task 15).
- [ ] `grep -rniE 'deepgram|gemini|openai\.NewTTS|OPENAI TTS|PROVIDER_STT_|PROVIDER_LLM_|PROVIDER_PROFILE|PROVIDER_SWITCHED|StartWith|rt\.stt_vendor|ResilientSTT|provider_profile|PROVIDER_TTS_|voice\.providers|TTSPrimary|TTSFallback' docs/superpowers/plans/*M9* cmd internal deploy` → kosong, kecuali baris gerbang ini dan catatan D-015/D-017 yang menyebut penghapusan.
- [ ] Hasil perintah gerbang dicatat di `docs/implementation-log.md` (master aturan 6).

## Self-Review

- **Jumlah task: 23** (sebelumnya 24; dihapus: 2 task Gemini, 2 task Deepgram, 1 task OpenAI TTS kondisional; ditambah: OpenAI STT, DeepSeek LLM, DeepSeek Evaluation, Cartesia STT). Peta penomoran: 1 simulator; 2–3 OpenAI LLM/Evaluation/Embedding; 4 OpenAI STT; 5–6 DeepSeek LLM/Evaluation; 7 ElevenLabs TTS; 8–9 ElevenLabs STT; 10 Cartesia STT; 11 Cartesia TTS; 12 (dihapus dari v1, nomor dipertahankan); 13 conformance; 14 `providerwire.Build`; 15 validasi startup + voice placeholder; 16 live; 17 e2e real; 18–20 `provider-bench`/`provider-report`; 21 dataset; 22 benchmark run; 23 voice per karakter. Urutan eksekusi: Task 10 dan 11 (Cartesia) dikerjakan setelah Task 9 dan sebelum Task 13 agar `cartesia` tercakup conformance; Task 14 memperluas `Build` untuk semua adapter (Task 2–12); Task 15 memakai `ActiveVendor` dari Task 14.
- **Cakupan spec:** §8.4.0 (aturan umum: Task 1–12, 13), §8.4.1 (T8–9), §8.4.2 (T4), §8.4.3 (T2), §8.4.4 (T5), §8.4.5 (T7), §8.4.6 (T10), §8.4.14 (T11), §8.4.8 (T3), §8.4.9 (T6), §8.4.10 (T3), §8.4.11 (dihapus dari v1), §8.4.12 (dikonsumsi adapter via `req.VoiceID`/`req.Model`/`req.SpeakingRate`; sumber nilai = `config_snapshot.character`; tes di T7/T11; `providerwire.Build` membangun semua adapter TTS enabled di T14), §8.4.13 (tabel verifikasi diperbarui T22), §8.5.1 sakelar & validasi startup (T14–15), §8.5.8 (sinyal rotasi diuji T13), §8.8.3 (T20), §8.10 (T18–23: 3 STT, 2 LLM, TTS per karakter, rekomendasi manual), §8.11 (T1, T13, T16), §14.6 (T1, T16), §6.3.4/§6.13.3 (T15, T23; `characters.provider_name/model_name/voice_id` + `voice.fallback`), "Masalah" dukungan Jepang STT (T22). Tidak dicakup (milik milestone lain): router/breaker/`rotatingSTT`/validasi `RouterConfig` (M4), metrik Prometheus router & biaya (M4), worker evaluasi/memory yang memvalidasi skema DeepSeek di kode dan repair (M7/M8), manifest k8s untuk `provider.env` dan runbook R10 ganti vendor (M10).
- **Konsistensi tipe:** semua `New<Cap>(cfg, hc, log)` sesuai §8.1 butir 3 (`openai.NewSTT/NewLLM/NewEvaluation/NewEmbedding`, `deepseek.NewLLM/NewEvaluation`, `elevenlabs.NewSTT/NewTTS`, `cartesia.NewSTT/NewTTS`); `Config` per vendor menyimpan `Tap`, timer sintetis; `RealDeps` field = `RouterDeps` §8.5.2 (satu adapter per kapabilitas STT/LLM/Evaluation/Embedding, `TTS map`); `providertest` dipanggil dengan signature M4 apa adanya.
- **Ambiguitas spec & keputusan:** (1) lokasi fixture: §8.11 (`testdata/<cap>_*.jsonl`) vs §14.6 (`testdata/recordings/<cap>_<case>.jsonl`) → pakai `testdata/recordings/<cap>_<case>.jsonl`; (2) `internal/testutil/replay` di §2.2 = replay timeline STT untuk turn engine, di §14.6 = server vendor → file vendor-sim ditambahkan di paket yang sama (`httpsim.go`, `wssim.go`); (3) sinyal rotasi STT antara adapter dan `rotatingSTT` tidak dinamai → `Err.VendorCode` kode vendor persis, diverifikasi terhadap router M4 di T13; kode batas sesi OpenAI/Cartesia ditetapkan di Step 0 T4/T10; (4) `docs/provider-benchmark.md` (brief) vs `docs/benchmarks/provider-benchmark-<tanggal>.md` (§8.10.5) → `docs/provider-benchmark.md`, dicatat di deviations; (5) benchmark e2e tanpa VAD/turn engine nyata (§8.10.3) → komposisi in-process + `--turn-end-ms`, dicatat di deviations; (6) env base URL/knob vendor ditambahkan ke `RouterConfig` hanya bila M4 belum punya, sedangkan `TTSPrimary`/`TTSFallback`/`PROVIDER_STT_*`/`PROVIDER_LLM_*`/profile dihapus bila masih ada (D-015/D-016/D-017); (7) Cartesia diberi dua task berurutan (10 STT membuat `config.go`; 11 TTS memperluas) agar satu paket punya satu `Config`; (8) hasil benchmark TTS berupa `character-voices.json` (bukan env) karena TTS per karakter; (9) validasi "tepat satu" diletakkan di `providerwire.ActiveVendor` (dipakai `Build` dan `ValidateReal`) agar tidak bergantung pada urutan task M4; bila `RouterConfig.Validate()` M4 sudah menegakkannya, `ActiveVendor` memanggilnya dan tidak menduplikasi aturan; (10) benchmark hanya merekomendasikan, tidak menulis flag aktif (keputusan manual pemilik produk, D-017); (11) §13.2.8 menyebut "13 kombinasi" (vendor, kapabilitas) sedangkan §8.4.7 menghitung 10 (OPENAI 4 + DEEPSEEK 2 + ELEVENLABS 2 + CARTESIA 2) → plan memakai 10 (catat di deviations; angka §13.2.8 disesuaikan bila spec belum diperbarui).
