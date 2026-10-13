# Kaiwa Backend — M9 Real Providers & Benchmark Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Mengganti provider fake dengan adapter vendor nyata (ElevenLabs, Deepgram, OpenAI, Gemini, Azure) yang lulus recorded-fixture test + `providertest`, men-wire `PROVIDER_MODE=real` dengan validasi startup, dan menyediakan `kaiwa-cli provider-bench`/`provider-report` untuk menetapkan model & voice ID awal berdasarkan benchmark terukur.
**Architecture:** Satu paket per vendor `internal/provider/<vendor>` (`net/http` + `coder/websocket`, tanpa SDK) yang hanya mengimpor `provider`, `provider/httpx`, `platform/otel`, stdlib; tidak mengimpor `conversation/audio`, `fake`, `pricing`, vendor lain (resample/WAV lewat `provider/pcm.go`). Adapter dirakit hanya di `cmd/internal/providerwire` (dipakai `cmd/realtime`, `cmd/worker`, `cmd/kaiwa-cli`); tool benchmark hidup di `cmd/kaiwa-cli/internal/{bench,report}` dan bergantung pada port (bukan vendor) sehingga teruji dengan `fake`. Fixture recorded dimainkan oleh simulator vendor di `internal/testutil/replay`; semua yang butuh kunci/akun/penilaian manusia ditandai **MANUAL GATE**.
**Tech Stack:** Go 1.25, `net/http`, `github.com/coder/websocket`, `encoding/json`, `otelhttp`/`platform/otel` (trace), `prometheus/client_golang` (hanya gauge rate-limit adapter), `gopkg.in/yaml.v3` (hanya `provider/pricing`, sudah ada), `pgx/v5` (hanya `provider-report`), testify, testcontainers-postgres (integration). Tanpa SDK vendor, tanpa library baru (D-003).
**Spec:** docs/superpowers/specs/2026-10-12-kaiwa-backend-design.md — Bab 8 §8.4.0–8.4.11 (adapter), §8.5.1 (env & validasi startup), §8.8.3 (`provider-report`), §8.10 (benchmark), §8.11 + §14.6 (test adapter), §6.3.5/§6.13.3 (voice ID), D-040, D-041, D-NEW-8-1…8-31.
**Master plan:** docs/superpowers/plans/2026-10-13-kaiwa-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M4 (port & tipe `provider.*`, `provider.Error/KindOf`, `httpx.NewClient/ClassifyHTTP/ClassifyError/ClassifyWSClose/SSEReader`, `providertest.{LLM,TTS,STT,Evaluation,Embedding,Pronunciation}Suite`, `provider.RouterConfig`, `ResolveTTSParams`, `ExpressionCatalog`, `PriceTable`, `pricing.Load`, `fake.*`, `provider.NormalizeJA/WAVDurationMs/EncodeWAV/DecodeWAV/ResamplePCM16`); M5 (`cmd/realtime` wiring fake, `kaiwaclient`, `kaiwa-cli converse`, `testdata/audio/*.wav` §14.5, harness e2e) untuk Task 17 dan Task 20; M7 (worker evaluasi memakai `AsyncRouter`) untuk gate real-mode worker; M3 (`voice_provider_configs`, seed §6.13).

## Global Constraints

- Master Global Constraints berlaku.
- Tanpa SDK vendor; dependensi adapter = stdlib + `coder/websocket` + `provider`/`provider/httpx`/`platform/otel`. Adapter MUST NOT mengimpor `conversation/audio`, `fake`, `pricing`, vendor lain, paket fitur selain `provider` (§8.1 butir 2; depguard §14.9.5). `time.Now()` boleh di adapter (pengecualian forbidigo), tapi timer yang diuji dibuat dapat dikonfigurasi lewat `Config`.
- Setiap adapter: satu `Config` + `New<Cap>(cfg Config, hc *http.Client, log *slog.Logger) (*<Cap>, error)` (§8.1 butir 3); nama vendor/kapabilitas UPPERCASE; span OTel `provider.<capability>` dengan atribut `provider.name`, `provider.model`, `provider.outcome`, `provider.error_kind` dibuat adapter sendiri (router M4 tidak punya span); log berisi `provider, capability, model, request_id, kind, http_status, vendor_code`, MUST NOT berisi API key, teks user/AI, audio (§8.4.0).
- `make modelname-check`: kode non-test adapter MUST NOT memuat literal `gpt-|o[0-9]-|gemini-|claude-|eleven_|scribe|nova-|aura-|text-embedding-` (bandingkan model ElevenLabs multilingual lewat `strings.Contains(model, "multilingual_v2")`); nama model hanya dari config/`req.Model`.
- Audio: STT in PCM16LE mono 16 kHz; TTS out PCM16LE mono 24 kHz, panjang chunk genap (byte ganjil di batas chunk ditahan); Cancel ctx → tutup koneksi ≤ 50 ms, tidak ada event/chunk sesudahnya (§8.2.3/8.2.4).
- STT: tepat satu `FINAL{FromFinalize:true}` per `Finalize()`; `Events()` buffered 64, PARTIAL tertua dibuang saat penuh, FINAL/ERROR/CLOSED tidak pernah dibuang; `SendAudio` buffer internal ≤ 2 s audio (penuh → `*Error{KindUnavailable}`); `Close` menunggu ≤ 500 ms, idempotent.
- Konstanta vendor (§8.4): ElevenLabs STT batching 2×20 ms=1.280 byte, silence keep-alive 100 ms setelah 5 s sepi, FINAL sintetis 400 ms; Deepgram `KeepAlive` setelah 4 s sepi, FINAL sintetis 400 ms; ElevenLabs `speed` 0,7–1,2 (sudah di-clamp `ResolveTTSParams`); Azure < 500 ms `TOO_SHORT`, > 30.000 ms `TOO_LONG` tanpa request; embedding 1536 dim; Gemini OPENING turn user konstan `「会話を始めてください。」`; Gemini `thinkingBudget` default `0` (`-1` = tidak dikirim).
- Gate benchmark (§8.10.4): STT final ≤ 300/450 ms; CER native/learner ≤ 8%/15% (primary) dan 12%/20% (fallback); LLM time-to-first-sentence ≤ 800/1.200 ms; format ≥ 97%/95%; kualitas ≥ 4,0/3,5; TTS first audio ≤ 400/600 ms; MOS/kesalahan kanji ≥ 4,0/≤ 3% dan ≥ 3,5/≤ 5%; error rate pada konkurensi 100 ≤ 1%/2%. Seleksi: STANDARD = termurah lolos gate; PREMIUM = kualitas tertinggi lolos gate dengan biaya ≤ 3× STANDARD (D-NEW-8-20). Gate tidak boleh diturunkan tanpa update D-100.
- Fixture recorded: `internal/provider/<vendor>/testdata/recordings/<cap>_<case>.jsonl` (§14.6 menang atas §8.11), tidak boleh berisi secret/PII; format JSONL milik Task 1. Fixture awal disalin dari contoh payload dokumentasi vendor (URL di spec §8.4) dengan `meta.source=<URL>`, `meta.verified=false`; diganti rekaman nyata (`-record`, MANUAL GATE Task 16) → `verified=true`. Jangan mengarang field di luar dokumentasi/rekaman.
- Setiap butir spec bertanda **"verifikasi di task adapter"** diperiksa di Step 0 task adapternya (buka URL dokumentasi di spec §8.4.x), hasilnya (ya/tidak + kutipan field) dicatat di `docs/implementation-log.md`; bila berbeda dari spec, perbarui spec bab 8 dan catat di `docs/implementation-deviations.md` sebelum menulis fixture.
- Harga di `prices.yaml`, voice ID, dan model final HANYA dari hasil benchmark/riset resmi (tidak dikarang).

## Review Focus

(Master Review Focus #5 sisi adapter + kondisi tersirat; tiap butir punya test di task pemilik.)

1. **STT final kosong tidak menggantung / tidak ganda (master #5):** tiap `Finalize()` → tepat satu `FINAL{FromFinalize:true}` (sintetis 400 ms bila vendor diam, teks boleh kosong); FINAL kosong tanpa `from_finalize` tidak diteruskan — `TestELSTTFinalizeSyntheticEmptyFinal` (Task 7), `TestDGSTTFinalizeSynthetic`, `TestDGSTTEmptyFinalDropped` (Task 9), `STTSuite` (Task 13).
2. **LLM diblokir content filter tidak memicu fallback (master #5):** OpenAI `finish_reason=content_filter` → `DONE{CONTENT_FILTERED}`, HTTP 400 `content_policy_violation` → `KindContentFiltered`, `refusal` → `KindContentFiltered`; Gemini `blockReason`/`SAFETY` setara — `TestOpenAIContentFilter*` (Task 2–3), `TestGeminiContentFilter*` (Task 4–5); `FallbackEligible==false` ditegaskan.
3. **Cancel/barge-in menutup stream ≤ 50 ms tanpa goroutine bocor atau chunk lama (master #3 sisi adapter):** `Test<Vendor><Cap>CancelWithin50ms` (Task 2, 4, 6, 7, 11) + `TestNoGoroutineLeakAfterCancel` semua adapter (Task 13).
4. **Rahasia tidak bocor:** API key tidak ada di log, `Error.Message`, span, dan rekaman — `TestRecorderRedactsSecrets` (Task 1), `TestNoSecretInLogsOrErrors` semua adapter (Task 13).
5. **Real mode tidak boleh start dengan konfigurasi yang pasti gagal di tengah sesi:** kunci vendor yang direferensikan wajib, voice ID `PLACEHOLDER_*` untuk vendor TTS primary/fallback ditolak (`CONTENT_MISCONFIGURED` di runtime = sesi pertama gagal) — `TestValidateRealRejectsMissingKeys` (Task 14), `TestPlaceholderVoiceRejectedAtStartup` (Task 15).

## File Structure

```text
internal/testutil/replay/{script.go,httpsim.go,wssim.go,recorder.go}   # simulator vendor dari fixture JSONL (T1)
internal/testutil/live.go                                              # RequireEnv (skip tanpa kunci) (T1)
internal/provider/httpx/span.go                                        # StartSpan helper adapter (T1)
internal/provider/openai/{config,client,llm,evaluation,embedding,tts}.go + testdata/recordings/   # T2, T3, T23
internal/provider/gemini/{config,client,llm,evaluation,schema}.go + testdata/recordings/          # T4, T5
internal/provider/elevenlabs/{config,tts,stt,stt_events}.go + testdata/recordings/                # T6, T7, T8
internal/provider/deepgram/{config,stt,tts}.go + testdata/recordings/                             # T9, T10
internal/provider/azure/{config,pronunciation}.go + testdata/recordings/                          # T11
internal/provider/*/conformance_test.go, internal/provider/adapters_test.go                       # providertest + aturan impor (T12)
internal/provider/*/live_test.go (tag live)                                                       # T16
cmd/internal/providerwire/{build.go,validate.go,voicecheck.go}                                    # T13–T14 (build T13; validate/voicecheck T14)
internal/postgres/voicecheck_repo.go + db/queries/voicecheck.sql                                  # T14
cmd/realtime, cmd/worker (ubah wiring); cmd/api (validasi voice)                                  # T14
test/e2e/real/{run.sh,real_test.go}; Makefile (e2e-real, bench)                                   # T16
cmd/kaiwa-cli/{provider_bench.go,provider_report.go}; cmd/kaiwa-cli/internal/{bench,report}/       # T17–T19
testdata/benchmark/manifest.jsonl, bench/models.yaml, bench/README (format); docs/provider-benchmark.md; deploy/config/provider-profile.env; seed/env/<env>/0900_voice_ids.sql   # T20–T22 (manual)
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
  - `replay.NewRecorder(t, vendor, capability, path string) *Recorder` dengan `(*Recorder).Transport(base http.RoundTripper) http.RoundTripper`, `(*Recorder).Tap(dir string, text bool, data []byte)` (dipasang ke `Config.Tap` adapter WS) dan `Flush()` menulis JSONL; header `Authorization, xi-api-key, x-goog-api-key, Ocp-Apim-Subscription-Key, Token` dan query `key=` dihapus (§14.6).
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
- Produces: `openai.Config{APIKey, BaseURL string; OmitSampling bool; ReasoningEffort string; Tap func(dir string, text bool, data []byte)}` (secret tidak pernah di-`String()`); `openai.NewLLM(cfg Config, hc *http.Client, log *slog.Logger) (*LLM, error)` (error bila `APIKey` atau `BaseURL` kosong); `(*LLM).Name() provider.Name == provider.OpenAI`; `Stream(ctx, provider.LLMRequest) (provider.LLMStream, error)`; helper paket `classify(resp, body) *provider.Error` + `(*client).publishRateLimit` (gauge `kaiwa_provider_ratelimit_remaining{provider,capability,kind}` dari header `x-ratelimit-remaining-requests/-tokens`).

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
Tambah: `TestOpenAILLMFinishLength` → `FinishMaxTokens`; `TestOpenAILLMContentFilterDone` (`finish_reason=content_filter` → `DONE{FinishContentFiltered}`, bukan ERROR); `TestOpenAILLMUsageMissingIsEstimated` (tanpa chunk usage → `Usage.Estimated==true`, `InputTokens` = total rune pesan); `TestOpenAILLMStopOmittedWhenEmpty`; `TestOpenAILLMOmitSampling` (`OmitSampling=true` → body tanpa `temperature` & `stop`; `ReasoningEffort="minimal"` → `"reasoning_effort":"minimal"`); `TestOpenAILLMErrors` tabel: 401→`KindAuth`, 429 `insufficient_quota`→`KindRateLimited` `Retryable=false`, 429 biasa `Retry-After:3`→`RetryAfter=3s` retryable, 400 `content_policy_violation`→`KindContentFiltered`, 400 lain→`KindInvalidRequest`, 503→`KindUnavailable`, error dikembalikan sinkron dari `Stream` (bukan event); `Message` tidak memuat `sk-test`; `TestOpenAILLMCancelWithin50ms` (fixture `llm_hang`: kirim 1 delta lalu menggantung; cancel → `DONE{FinishCancelled}` dengan `Usage.Estimated` ≤ 50 ms, channel tertutup, `Close` idempotent); `TestOpenAILLMStreamBadJSONIsInvalidResponse` (event `ERROR` kind `INVALID_RESPONSE`); `TestOpenAILLMRateLimitGauge`; `TestOpenAILLMSpan`.
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
- Consumes: Task 2 (`Config`, `client`, `classify`).
- Produces: `openai.NewEvaluation(cfg Config, hc *http.Client, log *slog.Logger) (*Evaluation, error)` (`provider.EvaluationProvider`, `GenerateJSON`); `openai.NewEmbedding(...) (*Embedding, error)` (`provider.EmbeddingProvider`, `Embed`).

- [ ] **Step 0 (verifikasi di task adapter):** https://platform.openai.com/docs/guides/structured-outputs — keyword JSON Schema yang didukung strict mode & field `refusal`; https://developers.openai.com/api/docs/guides/embeddings — urutan `data[i].index`, `usage`.
- [ ] **Step 1: Write the failing test**
```go
func TestOpenAIEvalRequestAndResult(t *testing.T) { // fixture eval_basic
	res, err := ev.GenerateJSON(ctx, provider.StructuredRequest{Task: "EVALUATION", Model: "m", SchemaName: "kaiwa_evaluation_rubric_v1",
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}`),
		Messages: msgs, MaxOutputTokens: 800, Temperature: 0})
	require.NoError(t, err); require.JSONEq(t, `{"s":77}`, string(res.JSON))
	require.JSONEq(t, `{"type":"json_schema","json_schema":{"name":"kaiwa_evaluation_rubric_v1","strict":true,"schema":{"type":"object","properties":{"s":{"type":"integer"}},"required":["s"],"additionalProperties":false}}}`, requestField(srv, "response_format"))
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

### Task 4: Gemini — `Config`, klien, LLM streaming (§8.4.4)

**Files:**
- Create: `internal/provider/gemini/{config.go,client.go,llm.go}`, `internal/provider/gemini/testdata/recordings/llm_{basic,opening_only,max_tokens,safety,block_reason,other_finish,thought_part,429,hang}.jsonl`
- Test: `internal/provider/gemini/llm_test.go`

**Interfaces:**
- Consumes: Task 1; M4 `httpx.*`.
- Produces: `gemini.Config{APIKey, BaseURL string; ThinkingBudget int; Tap …}` (`ThinkingBudget` zero-value 0 = kirim `{"thinkingBudget":0}`; `-1` = tidak dikirim); `gemini.NewLLM(cfg, hc, log) (*LLM, error)`; `(*LLM).Name()==provider.Gemini`; `Stream`. Konstanta tak-ekspor `openingTurn = "「会話を始めてください。」"`.

- [ ] **Step 0 (verifikasi di task adapter):** https://ai.google.dev/api/generate-content & /gemini-api/docs/thinking: header `x-goog-api-key` (bila tidak diterima pakai query `key=`; catat di log), bentuk `thinkingConfig`, apakah role harus bergantian, field `thoughtsTokenCount`.
- [ ] **Step 1: Write the failing test**
```go
func TestGeminiRequestMapping(t *testing.T) { // fixture llm_basic
	st, _ := l.Stream(ctx, provider.LLMRequest{Model: "g1", MaxTokens: 250, Temperature: 0.8, Stop: []string{"X"}, Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: "A"}, {Role: provider.RoleSystem, Content: "B"},
		{Role: provider.RoleUser, Content: "u1"}, {Role: provider.RoleUser, Content: "u2"}, {Role: provider.RoleAssistant, Content: "a1"}}})
	_ = drain(st.Events())
	r := srv.Requests()[0]
	require.Equal(t, "/models/g1:streamGenerateContent", r.Path); require.Equal(t, "sse", r.Query.Get("alt")); require.Equal(t, "k", r.Header.Get("x-goog-api-key"))
	require.JSONEq(t, `{"systemInstruction":{"parts":[{"text":"A\n\nB"}]},"contents":[{"role":"user","parts":[{"text":"u1\n\nu2"}]},{"role":"model","parts":[{"text":"a1"}]}],
	  "generationConfig":{"maxOutputTokens":250,"temperature":0.8,"stopSequences":["X"],"thinkingConfig":{"thinkingBudget":0}}}`, string(r.Body))
}
func TestGeminiOpeningInjectsConstantUserTurn(t *testing.T) { /* hanya pesan system → contents=[{"role":"user","parts":[{"text":"「会話を始めてください。」"}]}] */ }
```
Tambah: `TestGeminiThinkingBudgetMinusOneOmitted`; `TestGeminiDeltaAndUsage` (`OutputTokens = candidatesTokenCount + thoughtsTokenCount`, `InputTokens = promptTokenCount`, part `thought:true` diabaikan, beberapa part teks digabung dalam satu DELTA); `TestGeminiFinishReasonMapping` (`STOP`→`FinishStop`; `MAX_TOKENS`→`FinishMaxTokens`; `SAFETY|RECITATION|BLOCKLIST|PROHIBITED_CONTENT|SPII`→`FinishContentFiltered`; `OTHER|LANGUAGE|MALFORMED_RESPONSE`→`ERROR{KindInvalidResponse}`); `TestGeminiContentFilterBlockReason` (`promptFeedback.blockReason` → `ERROR{KindContentFiltered}`, `FallbackEligible==false`); `TestGeminiHTTPErrors` (429/`RESOURCE_EXHAUSTED`→`KindRateLimited`; 400→`KindInvalidRequest`; 503→`KindUnavailable`; sinkron); `TestGeminiCancelWithin50ms`; `TestGeminiAPIKeyNotInLogsOrErrors`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/gemini/ -race -run TestGemini`. Expected: FAIL.
- [ ] **Step 3: Implement** pemetaan pesan §8.4.4 (gabung system dengan `\n\n`, `assistant→model`, gabung role berurutan sama, OPENING). Stream = SSE `alt=sse`; `DONE` dikirim di akhir stream dengan finish reason terakhir; `usageMetadata` dari chunk terakhir.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/gemini/ -race -count=3` → PASS; `make modelname-check`.
- [ ] **Step 5: Commit** — `git add internal/provider/gemini && git commit -m "feat(provider/gemini): LLM streaming"`

---

### Task 5: Gemini — Evaluation + konversi skema (§8.4.9)

**Files:**
- Create: `internal/provider/gemini/{evaluation.go,schema.go}`, `testdata/recordings/eval_{basic,max_tokens,safety}.jsonl`
- Test: `internal/provider/gemini/evaluation_test.go`, `schema_test.go`

**Interfaces:**
- Consumes: Task 4.
- Produces: `gemini.NewEvaluation(cfg, hc, log) (*Evaluation, error)` (`provider.EvaluationProvider`); `Config.SchemaMode string` (`"json_schema"` default → `responseJsonSchema`; `"openapi"` → `responseSchema` hasil konversi; nilai default ditetapkan oleh Step 0); `func ToOpenAPISchema(in json.RawMessage) (json.RawMessage, error)` (hapus `additionalProperties`, `["T","null"]` → `{"type":"T","nullable":true}`, rekursif ke `properties`/`items`/`anyOf`).

- [ ] **Step 0 (verifikasi di task adapter):** https://ai.google.dev/gemini-api/docs/structured-output — apakah `responseJsonSchema` tersedia untuk model evaluasi; tetapkan default `SchemaMode`; catat keyword yang ditolak.
- [ ] **Step 1: Write the failing test**
```go
func TestToOpenAPISchema(t *testing.T) {
	out, err := gemini.ToOpenAPISchema(json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"n":{"type":["string","null"]},"a":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"x":{"type":"integer"}},"required":["x"]}}},"required":["n","a"]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"object","properties":{"n":{"type":"string","nullable":true},"a":{"type":"array","items":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}}},"required":["n","a"]}`, string(out))
}
func TestGeminiEvalRequest(t *testing.T) { /* path /models/gm:generateContent (tanpa alt=sse); generationConfig memuat responseMimeType "application/json", responseJsonSchema = skema asli, maxOutputTokens, temperature 0 dikirim, seed bila ada */ }
```
Tambah: `TestGeminiEvalOpenAPIMode` (`SchemaMode:"openapi"` → `responseSchema` terkonversi, tanpa `responseJsonSchema`); `TestGeminiEvalJoinsParts` (teks beberapa part digabung → `json.Valid` → `StructuredResult.JSON`); `TestGeminiEvalMaxTokensIsInvalidResponse` (`Retryable=true`); `TestGeminiEvalSafetyIsContentFiltered`; `TestGeminiEvalInvalidJSON` → `KindInvalidResponse`; `TestGeminiEvalNegativeTemperatureDefaults02`; `TestGeminiEvalUsage` (`thoughtsTokenCount` dijumlahkan ke output).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/gemini/ -race -run 'Eval|OpenAPISchema'`. Expected: FAIL.
- [ ] **Step 3: Implement** sesuai §8.4.9; mapping finishReason memakai fungsi yang sama dengan Task 4 (`mapFinish`); pesan = `systemInstruction` + `contents` (aturan §8.4.4), tanpa OPENING turn tambahan bila ada pesan non-system.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/gemini/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/gemini): evaluation structured output dan konversi skema"`

---

### Task 6: ElevenLabs — `Config` dan TTS HTTP streaming per kalimat (§8.4.5)

**Files:**
- Create: `internal/provider/elevenlabs/{config.go,tts.go}`, `testdata/recordings/tts_{basic,odd_bytes,voice_not_found_404,quota_exceeded_429,mid_error,hang}.jsonl`
- Test: `internal/provider/elevenlabs/tts_test.go`

**Interfaces:**
- Consumes: Task 1; M4 `provider.TTSRequest/ResolvedTTSParams`, `httpx.*`.
- Produces: `elevenlabs.Config{APIKey, BaseURL, WSBaseURL string; Tap …; SyntheticFinalDelay, KeepAliveAfter time.Duration}` (dua timer terakhir dipakai Task 7–8; zero-value → 400 ms / 5 s); `elevenlabs.NewTTS(cfg, hc, log) (*TTS, error)`; `(*TTS).Name()==provider.ElevenLabs`; `Synthesize(ctx, provider.TTSRequest) (provider.TTSStream, error)`.

- [ ] **Step 0 (verifikasi di task adapter):** https://elevenlabs.io/docs/api-reference/text-to-speech/stream: body PCM `pcm_24000` = PCM16 LE mono tanpa header (durasi = bytes/48000 s), `language_code` & `previous_text`, bentuk body error `detail.status`; catat apakah `eleven_v3` dkk menolak `speed`/`previous_text`.
- [ ] **Step 1: Write the failing test**
```go
func TestELTTSRequest(t *testing.T) { // fixture tts_basic
	st, err := tts.Synthesize(ctx, provider.TTSRequest{Text: "こんにちは。", Language: "ja", PreviousText: "はい。", Model: "m_flash",
		Resolved: provider.ResolvedTTSParams{ProviderVoiceID: "VOICE1", Params: map[string]any{"stability": 0.5, "similarity_boost": 0.75, "style": 0.2, "use_speaker_boost": true, "speed": 1.0}}})
	require.NoError(t, err); chunks := drainTTS(st.Chunks())
	r := srv.Requests()[0]
	require.Equal(t, "/v1/text-to-speech/VOICE1/stream", r.Path); require.Equal(t, "pcm_24000", r.Query.Get("output_format")); require.Empty(t, r.Query.Get("optimize_streaming_latency"))
	require.Equal(t, "k", r.Header.Get("xi-api-key"))
	require.JSONEq(t, `{"text":"こんにちは。","model_id":"m_flash","language_code":"ja","voice_settings":{"stability":0.5,"similarity_boost":0.75,"style":0.2,"use_speaker_boost":true,"speed":1.0},"previous_text":"はい。","apply_text_normalization":"auto"}`, string(r.Body))
	last := chunks[len(chunks)-1]
	require.True(t, last.Final); require.Equal(t, 6, last.Usage.Characters); require.Equal(t, totalBytes(chunks)/48, last.Usage.AudioMs)
}
```
Tambah: `TestELTTSMultilingualV2OmitsLanguageCode` (model berisi `multilingual_v2` → body tanpa `language_code`); `TestELTTSOmitsAbsentVoiceSettings` (Params kosong → tanpa `voice_settings`; hanya key hadir yang dikirim); `TestELTTSAudioTagPrefix` (`Params["audio_tag"]="laughs"` → `text:"[laughs] こんにちは。"`, `Usage.Characters` tetap dari `Text` asli); `TestELTTSOddByteBoundary` (fixture `tts_odd_bytes`: chunk vendor 1001+999 byte → setiap `PCM` genap, total byte terjaga); `TestELTTSErrors` (404 `detail.status=voice_not_found` → `KindInvalidRequest`, `HTTPStatus=404`, `VendorCode="voice_not_found"` (dihitung breaker); 429 `quota_exceeded` → `KindRateLimited` `Retryable=false`; 401 → `KindAuth`; 500 → `KindUnavailable`; sinkron); `TestELTTSMidStreamError` (stream putus → chunk terakhir `Err.Kind==UNAVAILABLE`, tanpa chunk setelahnya); `TestELTTSCancelWithin50ms` (fixture `hang`; tidak ada chunk setelah cancel); `TestELTTSSpan`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/elevenlabs/ -race -run TestELTTS`. Expected: FAIL.
- [ ] **Step 3: Implement** POST `{BaseURL}/v1/text-to-speech/{ProviderVoiceID}/stream?output_format=pcm_24000`; baca body per potongan, tahan byte ganjil; chunk `Final` + `Usage{Characters: utf8.RuneCountInString(req.Text), AudioMs: bytes/48}` dikirim saat EOF; hanya membaca `req.Model`, `req.Resolved.*`, `req.Text`, `req.PreviousText`, `req.Language` (§8.4.12); key `Params` tak dikenal → log DEBUG.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/elevenlabs/ -race -count=3` → PASS; `make modelname-check`.
- [ ] **Step 5: Commit** — `git add internal/provider/elevenlabs && git commit -m "feat(provider/elevenlabs): TTS HTTP streaming per kalimat"`

---

### Task 7: ElevenLabs — STT realtime: open, kirim audio, event, Finalize (§8.4.1)

**Files:**
- Create: `internal/provider/elevenlabs/{stt.go,stt_events.go}`, `testdata/recordings/stt_{basic,commit_empty,auto_commit,partial_cumulative}.jsonl`
- Test: `internal/provider/elevenlabs/stt_test.go`

**Interfaces:**
- Consumes: Task 6 (`Config`); M4 `provider.STTProvider/STTSession/STTEvent`, `provider.NormalizeJA`; Task 1 `replay.NewWSServer`.
- Produces: `elevenlabs.NewSTT(cfg Config, hc *http.Client, log *slog.Logger) (*STT, error)` dengan `Config.STTModel string` (`PROVIDER_STT_ELEVENLABS_MODEL`; wajib non-kosong); `(*STT).Name()`, `Model()`, `Open(ctx, provider.STTSessionConfig) (provider.STTSession, error)`.

- [ ] **Step 0 (verifikasi di task adapter):** https://elevenlabs.io/docs/api-reference/speech-to-text/v-1-speech-to-text-realtime dan cookbook commit strategies. Putuskan & catat: (a) `language_code=ja` vs `jpn`; (b) `partial_transcript` = teks penuh segmen atau delta; (c) apakah commit 20 ms silence menghasilkan `committed_transcript` kosong (jika tidak → FINAL sintetis tetap berlaku); (d) ukuran chunk maksimum & backlog lebih cepat dari realtime; (e) peran `keepalive_interval_ms`. Perbarui fixture, spec §8.4.1, dan §8.4.13 sesuai hasil.
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

### Task 8: ElevenLabs — STT: error, rotasi, keep-alive, buffer, Close (§8.4.1)

**Files:**
- Modify: `internal/provider/elevenlabs/{stt.go,stt_events.go}`
- Create: `testdata/recordings/stt_{auth_error,quota,rate_limited,rotate_time_limit,drop}.jsonl`
- Test: `internal/provider/elevenlabs/stt_errors_test.go`

**Interfaces:**
- Consumes: Task 7; M4 router resilient STT (`provider/resilient_stt.go`) mengenali rotasi lewat `Err.VendorCode ∈ {"session_time_limit_exceeded","insufficient_audio_activity"}` (**ambiguitas spec** — keputusan: adapter mengirim `STTEvent{Type: STTError, Err: &provider.Error{Kind: KindUnavailable, VendorCode: <kode vendor persis>}}` lalu `CLOSED`; Task 12 memverifikasinya terhadap test rotasi M4).
- Produces: tabel pemetaan error §8.4.1 lengkap; keep-alive; `Close` sesuai kontrak.

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
Tambah: `TestELSTTRotationSignals` (`session_time_limit_exceeded` dan `insufficient_audio_activity` → `ERROR` dengan `VendorCode` tersebut lalu `CLOSED`, bukan kind AUTH/RATE_LIMITED); `TestELSTTWarningLogsOnly` (`warning` → tanpa event, ada log WARN); `TestELSTTSocketDropIsUnavailable` (fixture `drop`: close 1006 tanpa `CloseConnection` kita → `ERROR{UNAVAILABLE}` lalu `CLOSED`); `TestELSTTSendAudioBufferFull` (server tidak membaca; > 2 s audio (> 64.000 byte) ter-antre → `SendAudio` mengembalikan `*Error{KindUnavailable}` tanpa blok); `TestELSTTSendAfterClose` (`errors.Is(err, provider.ErrSessionClosed)`); `TestELSTTCloseIdempotentWithin500ms` (`Close` dua kali, selesai ≤ 500 ms walau server tidak menutup; channel `Events()` tertutup setelah `CLOSED`); `TestELSTTEventsDropOldestPartialNeverFinal` (konsumen berhenti membaca, banjir 200 PARTIAL + 1 FINAL + ERROR → FINAL & ERROR tetap ada); `TestELSTTCancelWithin50ms` (cancel ctx saat `Open` menggantung).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/elevenlabs/ -race -run 'ErrorMapping|KeepAlive|Rotation|Warning|Drop|BufferFull|AfterClose|CloseIdempotent|DropOldest|CancelWithin'`. Expected: FAIL.
- [ ] **Step 3: Implement** pemetaan §8.4.1; antrean tulis berkapasitas 2 s audio; keep-alive timer `Config.KeepAliveAfter` direset tiap pengiriman; `Close`: kirim pesan penutupan, tunggu ≤ 500 ms, `Conn.CloseNow()`, emit `CLOSED`, tutup channel; semua goroutine berhenti (diverifikasi Task 12).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/elevenlabs/ -race -count=3` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/elevenlabs): STT error mapping, rotasi, keep-alive, close"`

---

### Task 9: Deepgram — STT Nova live (§8.4.2)

**Files:**
- Create: `internal/provider/deepgram/{config.go,stt.go}`, `testdata/recordings/stt_{basic,from_finalize_absent,empty_final_from_finalize,empty_final_other,error_401,drop}.jsonl`
- Test: `internal/provider/deepgram/stt_test.go`

**Interfaces:**
- Consumes: Task 1; M4 `provider.STT*`, `provider.NormalizeJA`.
- Produces: `deepgram.Config{APIKey, BaseURL, WSBaseURL, STTModel string; Tap …; SyntheticFinalDelay, KeepAliveAfter time.Duration}` (zero → 400 ms / 4 s); `deepgram.NewSTT(cfg, hc, log) (*STT, error)`; `Name()==provider.Deepgram`.

- [ ] **Step 0 (verifikasi di task adapter):** https://developers.deepgram.com/reference/speech-to-text/listen-streaming, /docs/finalize, /docs/keep-alive, /docs/models-languages-overview. Putuskan & catat: nama parameter opt-out (`mip_opt_out`), penempatan `from_finalize` di payload `Results`, ada/tidaknya `start`/`duration` top-level (jika tidak: hitung dari `words`), apakah `nova-3` + `language=ja` berjalan di `/v1/listen` (jika tidak, rekomendasikan `nova-2`; keputusan akhir di Task 21).
- [ ] **Step 1: Write the failing test**
```go
func TestDGSTTOpenQuery(t *testing.T) {
	sess, err := stt.Open(ctx, provider.STTSessionConfig{Language: "ja", SampleRate: 16000}); require.NoError(t, err); defer sess.Close()
	u := srv.UpgradeRequest()
	require.Equal(t, "/v1/listen", u.Path)
	require.Equal(t, url.Values{"model": {"nova-3"}, "language": {"ja"}, "encoding": {"linear16"}, "sample_rate": {"16000"}, "channels": {"1"}, "interim_results": {"true"}, "punctuate": {"true"}, "endpointing": {"300"}, "vad_events": {"false"}, "smart_format": {"false"}, "mip_opt_out": {"true"}}, u.Query)
	require.Equal(t, "Token k", u.Header.Get("Authorization"))
}
func TestDGSTTMapsPartialAndFinalWithWords(t *testing.T) { /* fixture stt_basic: is_final=false → PARTIAL{Confidence:&c, StartMs: start*1000}; is_final=true from_finalize=true → FINAL{FromFinalize:true, Words ms} */ }
```
Tambah: `TestDGSTTSendsRawBinaryFramesAndControl` (`SendAudio` → frame biner 640 byte tanpa batching; `Finalize` → teks `{"type":"Finalize"}`; `Close` → `{"type":"CloseStream"}`); **`TestDGSTTFinalizeSynthetic`** (fixture `from_finalize_absent`: setelah `Finalize` tidak ada `from_finalize=true` dalam `SyntheticFinalDelay` (30 ms test) sejak pesan `Results` terakhir/`Finalize` → tepat satu `FINAL{"",FromFinalize:true}`; `is_final` yang datang sesudahnya diteruskan `FromFinalize=false`); **`TestDGSTTEmptyFinalDropped`** (`is_final=true` teks kosong dengan `from_finalize` salah/absen → tidak ada event; dengan `from_finalize=true` → `FINAL` kosong diteruskan); `TestDGSTTEmptyPartialDropped`; `TestDGSTTKeepAliveAfterIdle` (`KeepAliveAfter` 40 ms → `{"type":"KeepAlive"}`); `TestDGSTTErrorMapping` (upgrade 401 → `KindAuth`; 429 → `KindRateLimited`; 400 → `KindInvalidRequest`; close 1008/4001 → `KindAuth`; close 1006 → `KindUnavailable`; semua `ERROR` lalu `CLOSED`); `TestDGSTTIgnoresMetadataSpeechStartedUtteranceEnd`; `TestDGSTTNonFatalErrorLogsWarn`; `TestDGSTTBufferFullAndSendAfterClose`; `TestDGSTTCancelWithin50ms`; `TestDGSTTKeyNotLeaked`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/deepgram/ -race -run TestDGSTT`. Expected: FAIL.
- [ ] **Step 3: Implement** seperti Task 7–8 (kontrak Events/Close/buffer identik; paket `deepgram` tidak berbagi kode dengan `elevenlabs`, tidak boleh mengimpornya) dengan pemetaan §8.4.2; `Open` sukses = handshake 101.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/deepgram/ -race -count=3` → PASS; `make modelname-check`.
- [ ] **Step 5: Commit** — `git add internal/provider/deepgram && git commit -m "feat(provider/deepgram): STT Nova, Finalize sintetis, keep-alive"`

---

### Task 10: Deepgram — TTS Aura-2 per kalimat (§8.4.6)

**Files:**
- Create: `internal/provider/deepgram/tts.go`, `testdata/recordings/tts_{basic_raw,wav_header,401,429,hang}.jsonl`
- Test: `internal/provider/deepgram/tts_test.go`

**Interfaces:**
- Consumes: Task 9 `Config`; M4 `provider.TTSRequest`.
- Produces: `deepgram.NewTTS(cfg, hc, log) (*TTS, error)`; `Synthesize`.

- [ ] **Step 0 (verifikasi di task adapter):** https://developers.deepgram.com/reference/text-to-speech/speak-streaming dan /docs/tts-models: (a) `encoding/sample_rate/container/speed` berlaku untuk REST `POST /v1/speak`, (b) `container=none` = PCM mentah (jika tidak: parse+buang header RIFF 44 byte, fixture `wav_header`), (c) `speed` untuk Aura-2 Jepang — hasil (c) menentukan apakah kapabilitas `SPEED` boleh diset di seed (Task 22).
- [ ] **Step 1: Write the failing test**
```go
func TestDGTTSRequest(t *testing.T) {
	st, err := tts.Synthesize(ctx, provider.TTSRequest{Text: "はい。", Resolved: provider.ResolvedTTSParams{ProviderVoiceID: "aura-2-izanami-ja"}}); require.NoError(t, err)
	chunks := drainTTS(st.Chunks()); r := srv.Requests()[0]
	require.Equal(t, "/v1/speak", r.Path); require.Equal(t, url.Values{"model": {"aura-2-izanami-ja"}, "encoding": {"linear16"}, "sample_rate": {"24000"}, "container": {"none"}}, r.Query)
	require.Equal(t, "Token k", r.Header.Get("Authorization")); require.JSONEq(t, `{"text":"はい。"}`, string(r.Body))
	require.True(t, chunks[len(chunks)-1].Final); require.Equal(t, 3, chunks[len(chunks)-1].Usage.Characters)
}
```
Tambah: `TestDGTTSSpeedOnlyWhenResolved` (`Params["speed"]=0.9` → query `speed=0.9`; tanpa key → tanpa query `speed`); `TestDGTTSStripsRIFFHeaderWhenPresent` (fixture `wav_header`: 44 byte pertama dibuang, PCM genap); `TestDGTTSOddByteBoundary`; `TestDGTTSModelIsVoiceNotReqModel` (`req.Model` diabaikan, query `model` = `ProviderVoiceID`); `TestDGTTSErrors` (401→`KindAuth`; 429→`KindRateLimited` retryable; 400→`KindInvalidRequest`; 404→`KindInvalidRequest` `HTTPStatus=404`; 5xx→`KindUnavailable`); `TestDGTTSCancelWithin50ms`; `TestDGTTSUsageAudioMs` (`bytes/48`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/deepgram/ -race -run TestDGTTS`. Expected: FAIL.
- [ ] **Step 3: Implement** POST `{BaseURL}/v1/speak?model=<voice>&encoding=linear16&sample_rate=24000&container=none[&speed=<x>]`; sisanya seperti Task 6.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/deepgram/ -race -count=3` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/deepgram): TTS Aura-2 per kalimat"`

---

### Task 11: Azure — Pronunciation Assessment ja-JP (§8.4.11)

**Files:**
- Create: `internal/provider/azure/{config.go,pronunciation.go}`, `testdata/recordings/pron_{success,no_match,initial_silence,error_status,401,429}.jsonl`
- Test: `internal/provider/azure/pronunciation_test.go`

**Interfaces:**
- Consumes: Task 1; M4 `provider.PronunciationRequest/Result`, `provider.WAVDurationMs/DecodeWAV`.
- Produces: `azure.Config{Key, Endpoint string; Tap …}`; `azure.NewPronunciation(cfg, hc, log) (*Pronunciation, error)` (error bila `Key`/`Endpoint` kosong); `Name()==provider.Azure`; `Assess`.

- [ ] **Step 0 (verifikasi di task adapter):** https://learn.microsoft.com/en-us/azure/ai-services/speech-service/rest-speech-to-text-short & how-to-pronunciation-assessment: dukungan `ProsodyScore`/`EnableProsodyAssessment` untuk ja-JP (jika tidak ada → `ProsodyScore=nil`), kode `RecognitionStatus` lengkap, satuan `Offset/Duration` (100 ns), batas ukuran/durasi.
- [ ] **Step 1: Write the failing test**
```go
func TestAzureAssessRequestAndMapping(t *testing.T) { // fixture pron_success; wav 2 s dibangun dengan provider.EncodeWAV
	res, err := p.Assess(ctx, provider.PronunciationRequest{AudioWAV: wav(2000), ReferenceText: "はじめまして", Locale: "ja-JP"})
	require.NoError(t, err)
	r := srv.Requests()[0]
	require.Equal(t, "/stt/speech/recognition/conversation/cognitiveservices/v1", r.Path); require.Equal(t, "ja-JP", r.Query.Get("language")); require.Equal(t, "detailed", r.Query.Get("format"))
	require.Equal(t, "k", r.Header.Get("Ocp-Apim-Subscription-Key")); require.Equal(t, "audio/wav; codecs=audio/pcm; samplerate=16000", r.Header.Get("Content-Type"))
	hdr, _ := base64.StdEncoding.DecodeString(r.Header.Get("Pronunciation-Assessment"))
	require.JSONEq(t, `{"ReferenceText":"はじめまして","GradingSystem":"HundredMark","Granularity":"Word","Dimension":"Comprehensive","EnableMiscue":"True","EnableProsodyAssessment":"True"}`, string(hdr))
	require.Equal(t, provider.PronunciationOK, res.Status); require.Equal(t, int64(2000), res.AudioMs); require.Equal(t, provider.Azure, res.Provider)
	require.Equal(t, provider.PronErrMispronunciation, res.Words[1].ErrorType); require.Equal(t, int64(1234), res.Words[0].StartMs) // Offset/10000
}
```
Tambah: `TestAzureTooShortNoRequest` (400 ms → `TOO_SHORT`, `srv.Requests()` kosong, error nil); `TestAzureTooLongNoRequest` (30.001 ms → `TOO_LONG`); `TestAzureExactly30sIsSent` (30.000 ms dikirim) dan `Exactly500msIsSent`; `TestAzureNoMatchStatuses` (`NoMatch`, `InitialSilenceTimeout`, `BabbleTimeout` → `NO_MATCH`, error nil); `TestAzureErrorStatusIsUnavailable` (`RecognitionStatus=Error` → `KindUnavailable`); `TestAzureErrorTypeMapping` (`None|Omission|Insertion|Mispronunciation` → 4 enum); `TestAzureProsodyNullable` (`ProsodyScore` absen → `nil`); `TestAzureHTTPErrors` (401→`KindAuth`, 429→`KindRateLimited`, 5xx→`KindUnavailable`); `TestAzureRejectsNon16kWAV` (→ `KindInvalidRequest`); `TestAzureCancelWithin50ms`; `TestAzureKeyNotLeaked`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/azure/ -race`. Expected: FAIL.
- [ ] **Step 3: Implement** durasi dari header WAV (`provider.WAVDurationMs`) sebelum request; body = bytes WAV apa adanya (chunked); header `Pronunciation-Assessment` = base64 JSON persis di atas dengan `ReferenceText` dari request.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/azure/ -race -count=3` → PASS.
- [ ] **Step 5: Commit** — `git add internal/provider/azure && git commit -m "feat(provider/azure): pronunciation assessment ja-JP"`

---

### Task 12: Conformance `providertest` untuk semua adapter + aturan impor + hygiene

**Files:**
- Create: `internal/provider/{openai,gemini,elevenlabs,deepgram,azure}/conformance_test.go`, `internal/provider/adapters_test.go` (paket `provider_test`)
- Test: file yang sama

**Interfaces:**
- Consumes: M4 `providertest.LLMSuite(t, newProvider func(t *testing.T) provider.LLMProvider)`, `TTSSuite(t, newProvider, newReq func() provider.TTSRequest)`, `STTSuite(t, newProvider, speech func() []byte)`, `EvaluationSuite(t, newProvider, req provider.StructuredRequest)`, `EmbeddingSuite(t, newProvider)`, `PronunciationSuite(t, newProvider)`; fixture *reaktif* (aturan `on`, `times:0`) per vendor: `<cap>_suite.jsonl` (dibuat di task ini, berisi balasan generik yang memenuhi suite: LLM delta+usage; STT membalas tiap commit/Finalize dengan satu final; dll.).
- Produces: pemetaan suite→adapter: `openai` (LLM, Evaluation, Embedding), `gemini` (LLM, Evaluation), `elevenlabs` (STT, TTS), `deepgram` (STT, TTS), `azure` (Pronunciation).

- [ ] **Step 1: Write the failing test**
```go
func TestOpenAILLMConformance(t *testing.T) {
	providertest.LLMSuite(t, func(t *testing.T) provider.LLMProvider {
		srv := replay.NewHTTPServer(t, replay.LoadScript(t, replay.RecordingsDir(t, "openai"), "llm_suite.jsonl"))
		l, err := openai.NewLLM(openai.Config{APIKey: "sk-test", BaseURL: srv.URL}, httpx.NewClient(), slog.Default()); require.NoError(t, err); return l })
}
// … satu fungsi per (vendor, kapabilitas): STT memakai speech() = 1 s sinus 440 Hz PCM16 16 kHz; Config timer sintetis dipendekkan (30 ms)
func TestAdaptersImportRules(t *testing.T) { /* go/parser ImportsOnly: tiap internal/provider/<vendor> non-test hanya boleh stdlib, coder/websocket, prometheus/client_golang, platform/otel, provider, provider/httpx; terlarang: conversation/audio, fake, pricing, vendor lain, paket fitur lain, "os" Getenv tidak dicek di sini */ }
func TestNoGoroutineLeakAfterCancel(t *testing.T) { /* untuk tiap adapter: catat runtime.NumGoroutine(), 20× buka stream/sesi lalu cancel/Close, tunggu ≤ 500 ms, selisih ≤ 2 */ }
func TestNoSecretInLogsOrErrors(t *testing.T) { /* tiap adapter dengan key "SECRETKEY-123": jalankan kasus sukses + setiap kasus error 401/429/5xx dengan slog ke buffer; buffer, err.Error(), Error.Message, dan span attributes tidak memuat "SECRETKEY-123" */ }
func TestAdapterRotationSignalMatchesRouter(t *testing.T) { /* ELEVENLABS: session_time_limit_exceeded → ERROR VendorCode yang dikenali resilientSTT M4: gunakan provider.NewRouter dengan RouterDeps.STT{ELEVENLABS: adapter(replay), DEEPGRAM: adapter(replay)} dan pastikan Stats().Reconnects==1 dan tidak ada PROVIDER_SWITCHED */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/... -race -run 'Conformance|ImportRules|NoGoroutineLeak|NoSecret|RotationSignal'`. Expected: FAIL (fixture `*_suite.jsonl` belum ada; suite gagal).
- [ ] **Step 3: Implement** fixture `*_suite.jsonl` reaktif per adapter dan wiring test. Bila `providertest` menemukan pelanggaran kontrak adapter, perbaiki ADAPTER (bukan suite). Bila `TestAdapterRotationSignalMatchesRouter` gagal karena router M4 mengenali sinyal lain, sesuaikan adapter (Task 8) ke sinyal router dan catat di `docs/implementation-deviations.md`; jangan ubah router.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race -count=2` → PASS; `go test ./internal/provider/... -race -cover` → `internal/provider/...` ≥ 70% (§14.8); `make lint arch-check modelname-check` → exit 0.
- [ ] **Step 5: Commit** — `git add internal/provider && git commit -m "test(provider): conformance providertest, aturan impor, kebocoran goroutine & secret untuk semua adapter"`

---

### Task 13: `providerwire.Build` — merakit adapter nyata per kapabilitas

**Files:**
- Create: `cmd/internal/providerwire/build.go`
- Modify: `internal/provider/config.go` (hanya field yang BELUM ada: `ElevenLabsBaseURL`, `ElevenLabsWSBaseURL`, `DeepgramBaseURL`, `DeepgramWSBaseURL`, `OpenAIBaseURL`, `GeminiBaseURL`, `OpenAIOmitSampling`, `OpenAIReasoningEffort`, `GeminiThinkingBudget` dengan tag/default §8.5.1/§13.2.8; periksa dulu `grep`), `.env.example`, `deploy/k8s/base` ConfigMap bila field baru ditambah
- Test: `cmd/internal/providerwire/build_test.go`

**Interfaces:**
- Consumes: semua `New*` Task 2–11, `httpx.NewClient`, `provider.RouterConfig` (+`Selection(cap)`, `Mode(cap)` bila ada; jika M4 tidak mengekspos mode efektif per kapabilitas, tambahkan `func (c RouterConfig) ModeFor(cap Capability) string` (nama dari plan) di `config.go` + test).
- Produces: `type RealDeps struct{ STT map[provider.Name]provider.STTProvider; LLM map[provider.Name]provider.LLMProvider; TTS map[provider.Name]provider.TTSProvider; Evaluation map[provider.Name]provider.EvaluationProvider; Embedding provider.EmbeddingProvider; Pron provider.PronunciationProvider }`; `func Build(cfg provider.RouterConfig, caps []provider.Capability, log *slog.Logger) (RealDeps, error)` — hanya membangun kapabilitas di `caps` yang `ModeFor(cap)=="real"`, hanya vendor yang dirujuk `Selection(cap)` (primary + fallback bila `FallbackEnabled`); `cmd/*/main.go` menggabungkan: kapabilitas mode `real` ← `RealDeps`, sisanya ← wiring fake M4/M5 yang sudah ada. Satu `*http.Client` (`httpx.NewClient`) per vendor.

- [ ] **Step 1: Write the failing test**
```go
func TestBuildRealRealtimeCaps(t *testing.T) {
	cfg := realConfig(t) // RouterConfig mode real, semua key terisi, BaseURL → httptest
	d, err := providerwire.Build(cfg, []provider.Capability{provider.CapSTT, provider.CapLLM, provider.CapTTS}, slog.Default())
	require.NoError(t, err)
	require.Equal(t, []provider.Name{provider.Deepgram, provider.ElevenLabs}, sortedKeys(d.STT))
	require.Equal(t, []provider.Name{provider.Gemini, provider.OpenAI}, sortedKeys(d.LLM)); require.Empty(t, d.Evaluation); require.Nil(t, d.Pron)
}
func TestBuildSkipsFakeCapabilities(t *testing.T) { /* ModeTTS="fake" → d.TTS kosong; STT tetap real */ }
func TestBuildFallbackDisabledBuildsPrimaryOnly(t *testing.T) { /* FallbackEnabled=false → STT hanya ELEVENLABS */ }
func TestBuildSwappedFallbackTTSOpenAI(t *testing.T) { /* TTSFallback=OPENAI → d.TTS memuat OPENAI (adapter Task 23) ; sebelum Task 23: error "openai TTS belum diimplementasikan" */ }
func TestBuildPronunciationNone(t *testing.T) { /* PronunciationPrimary=NONE → d.Pron nil tanpa error walau AZURE_* kosong */ }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/internal/providerwire/ -race -run TestBuild`. Expected: FAIL.
- [ ] **Step 3: Implement** `Build` dengan `switch name` per kapabilitas; konfigurasi adapter dari `RouterConfig` (model STT ke `STTModel`); error tak dikenal → `fmt.Errorf` menyebut env var. `cmd/api` TIDAK mengimpor paket ini (hanya `cmd/realtime`, `cmd/worker`, `cmd/kaiwa-cli`).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/internal/providerwire/ ./internal/provider/ -race` → PASS; `make arch-check` → exit 0 (api/worker `CGO_ENABLED=0`; worker memuat adapter tanpa `conversation/audio`).
- [ ] **Step 5: Commit** — `git add cmd/internal internal/provider/config.go .env.example deploy && git commit -m "feat(cmd): providerwire.Build merakit adapter vendor nyata per kapabilitas"`

---

### Task 14: Validasi startup mode `real` + penolakan voice ID placeholder + wiring `cmd/*`

**Files:**
- Create: `cmd/internal/providerwire/{validate.go,voicecheck.go}`, `db/queries/voicecheck.sql`, `internal/postgres/voicecheck_repo.go`
- Modify: `cmd/realtime/main.go` & `wiring.go` (blok pembuat `RouterDeps`), `cmd/worker/wiring.go`, `cmd/api/main.go` (hanya panggil `ValidateVoices`), `internal/postgres/sqlcgen` (hasil `make sqlc`)
- Test: `cmd/internal/providerwire/validate_test.go`, `internal/postgres/voicecheck_repo_integration_test.go` (tag `integration`), `cmd/realtime/main_test.go`

**Interfaces:**
- Consumes: Task 13; `postgres` pool; M3 tabel `voice_provider_configs`, seed §6.13.
- Produces: `func ValidateReal(cfg provider.RouterConfig, caps []provider.Capability) error` (kunci wajib per vendor yang dirujuk `Selection(cap)` di kapabilitas mode `real`: `ELEVENLABS_API_KEY`, `DEEPGRAM_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `AZURE_SPEECH_KEY`+`AZURE_SPEECH_ENDPOINT` (kecuali `PRONUNCIATION_PRIMARY=NONE`); model profile PREMIUM/STANDARD terisi; pesan menyebut nama env, tidak pernah nilainya); `type VoiceChecker interface{ PlaceholderVoices(ctx context.Context, vendors []provider.Name) ([]PlaceholderVoice, error) }` + `PlaceholderVoice{VoiceProfileCode string; Provider provider.Name; VoiceID string}`; `func ValidateVoices(ctx context.Context, cfg provider.RouterConfig, vc VoiceChecker) error` (hanya bila `ModeFor(CapTTS)=="real"`: baris `ACTIVE` untuk `TTSPrimary` dan, bila `FallbackEnabled`, `TTSFallback` dengan `provider_voice_id LIKE 'PLACEHOLDER\_%'` → error menyebut `voice_profile.code`, vendor, dan petunjuk `seed/env/<env>/0900_voice_ids.sql` §6.13.3); `postgres.NewVoiceCheckRepo(pool) *VoiceCheckRepo` (`PlaceholderVoices`).

- [ ] **Step 1: Write the failing test**
```go
func TestValidateRealRejectsMissingKeys(t *testing.T) {
	cfg := realConfig(t); cfg.OpenAIAPIKey = ""
	err := providerwire.ValidateReal(cfg, allCaps); require.ErrorContains(t, err, "OPENAI_API_KEY"); require.NotContains(t, err.Error(), "sk-")
	cfg = realConfig(t); cfg.PronunciationPrimary = provider.None; cfg.AzureSpeechKey = ""; require.NoError(t, providerwire.ValidateReal(cfg, allCaps))
	cfg = realConfig(t); cfg.ModeLLM = "fake"; cfg.OpenAIAPIKey = ""; require.NoError(t, providerwire.ValidateReal(cfg, allCaps)) // LLM fake → key tak wajib
}
func TestPlaceholderVoiceRejectedAtStartup(t *testing.T) { // VoiceChecker fake mengembalikan {Code:"JA_F_KANTO_PREMIUM_01", Provider: ELEVENLABS, VoiceID: "PLACEHOLDER_ELEVENLABS_JA_F_KANTO_PREMIUM_01"}
	err := providerwire.ValidateVoices(ctx, realConfig(t), fakeChecker{...}); require.ErrorContains(t, err, "JA_F_KANTO_PREMIUM_01"); require.ErrorContains(t, err, "0900_voice_ids.sql")
	cfg := realConfig(t); cfg.ModeTTS = "fake"; require.NoError(t, providerwire.ValidateVoices(ctx, cfg, fakeChecker{...})) // mode TTS fake → tidak diperiksa
}
```
Integration (`-tags integration`): `TestVoiceCheckRepoFindsPlaceholders` — `testutil.NewPostgres(t)` + apply `seed/*.sql` (helper seed M3 `testutil.ApplySeed(t, pool)`; jika tidak ada, jalankan file seed lewat `pool.Exec`): `PlaceholderVoices(ctx, [ELEVENLABS, DEEPGRAM])` → 8 baris (4 voice × 2 vendor); setelah `UPDATE … SET provider_voice_id='real-1'` untuk semua ELEVENLABS & status DISABLED untuk satu DEEPGRAM → hanya 3 baris DEEPGRAM tersisa; vendor `OPENAI` → 0 baris (voice bawaan valid). `cmd/realtime/main_test.go`: `PROVIDER_MODE=real` tanpa `ELEVENLABS_API_KEY` → `run` mengembalikan error sebelum membuka koneksi DB/Redis (exit code 1 di `main`); `PROVIDER_MODE=fake` → tidak memanggil `ValidateReal`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/internal/providerwire/ ./cmd/realtime/ -race -run 'ValidateReal|PlaceholderVoice|Realtime'`; `go test -tags integration ./internal/postgres/ -race -run VoiceCheck`. Expected: FAIL.
- [ ] **Step 3: Implement** query sqlc `-- name: PlaceholderVoices :many` (JOIN `voice_profiles` untuk `code`, `provider = ANY($1)`, `status='ACTIVE'`, `provider_voice_id LIKE 'PLACEHOLDER\_%'`); `make sqlc`. Wiring: `cmd/realtime` & `cmd/worker` memanggil `ValidateReal` setelah config parse (sebelum koneksi apa pun), lalu `Build`, lalu mengisi `RouterDeps` per kapabilitas; `cmd/realtime` dan `cmd/api` memanggil `ValidateVoices` setelah pool siap (api memakai pool-nya sendiri; worker tidak memakai TTS). Kegagalan → log ERROR tanpa secret + exit code 1.
- [ ] **Step 4: Run tests to verify they pass** — perintah Step 2 → PASS; `make sqlc-check lint arch-check` → exit 0; `PROVIDER_MODE=fake make run-realtime` masih start (smoke manual 5 detik).
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(cmd): validasi startup mode real, tolak voice ID placeholder, wiring adapter"`

---

### Task 15: Test `-tags live` (kunci asli) + mode `-record` — **MANUAL GATE (kunci API vendor)**

**Files:**
- Create: `internal/provider/{openai,gemini,elevenlabs,deepgram,azure}/live_test.go` (`//go:build live`), `internal/provider/live_helpers_test.go` (`//go:build live`, paket `provider_test`)
- Test: file yang sama; **butuh**: `OPENAI_API_KEY`, `GEMINI_API_KEY`, `ELEVENLABS_API_KEY`, `DEEPGRAM_API_KEY`, `AZURE_SPEECH_KEY`, `AZURE_SPEECH_ENDPOINT`, `LIVE_ELEVENLABS_VOICE_ID`, `LIVE_DEEPGRAM_VOICE` (mis. `aura-2-izanami-ja`), `LIVE_LLM_MODEL_OPENAI`, `LIVE_LLM_MODEL_GEMINI`, `LIVE_TTS_MODEL_ELEVENLABS`, `LIVE_STT_MODEL_DEEPGRAM`, `LIVE_STT_MODEL_ELEVENLABS` — dibaca hanya lewat `testutil.RequireEnv`.

**Interfaces:**
- Consumes: Task 2–11, `testutil.RequireEnv`, `testdata/audio/{ja_greeting_short,ja_self_intro,ja_question,ja_filler_pause,ja_long_utterance}.wav+.txt` (M5/§14.5; resample 48→16 kHz dengan `provider.ResamplePCM16`; **bila fixture belum direkam penutur asli → MANUAL GATE: rekam ≥ 4 fixture asli**), flag test `-record` (`flag.Bool("record", false, …)` di `live_helpers_test.go`; bila true, `replay.NewRecorder` dipasang ke `http.Client.Transport`/`Config.Tap` dan menulis ke `testdata/recordings/<cap>_<case>.jsonl` dengan `meta.verified=true`).
- Produces: kriteria §14.6 sebagai test: STT (ElevenLabs & Deepgram) CER ≤ 0,15 untuk 5 fixture (CER = Levenshtein rune setelah membuang spasi & `。、？！?!,.・「」`; helper `cer(a,b string) float64` di test, diuji `TestCER`); LLM (OpenAI & Gemini) 10 prompt golden → teks Jepang, ≥ 90% kalimat berformat tag `<e:NAME>` D-081, first token < 2 s; TTS (ElevenLabs & Deepgram) kalimat 30 karakter → ≥ 0,5 s PCM16 24 kHz per voice; embedding dimensi 1536; Azure skor terisi untuk `ja_self_intro`; evaluation OpenAI & Gemini: skema kecil → JSON valid; satu run < 5 menit.

- [ ] **Step 1: Write the failing test**
```go
//go:build live
func TestLiveElevenLabsSTT(t *testing.T) {
	env := testutil.RequireEnv(t, "ELEVENLABS_API_KEY", "LIVE_ELEVENLABS_STT_MODEL")   // t.Skip bila kosong
	for _, name := range []string{"ja_greeting_short", "ja_self_intro", "ja_question", "ja_filler_pause", "ja_long_utterance"} {
		got := transcribe(t, elevenSTT(t, env), loadPCM16k(t, name)); require.LessOrEqual(t, cer(got, loadRef(t, name)), 0.15, name)
	}
}
func TestCER(t *testing.T) { /* tanpa tag live (helper di file tanpa tag): cer("こんにちは","こんにちわ") == 0.2 ; spasi & 。 diabaikan */ }
```
Tambah: satu test live per baris kriteria di atas (`TestLiveDeepgramSTT`, `TestLiveOpenAILLM`, `TestLiveGeminiLLM`, `TestLiveElevenLabsTTS`, `TestLiveDeepgramTTS`, `TestLiveOpenAIEmbedding`, `TestLiveAzurePronunciation`, `TestLiveOpenAIEvaluation`, `TestLiveGeminiEvaluation`) dan `TestLiveRecordMode` (hanya jalan dengan `-record`).
- [ ] **Step 2: Run it to verify it fails/skips** — tanpa kunci: `make test-live` → semua `SKIP`, exit 0 (bukan FAIL); `go test ./internal/provider/... -tags live -run TestCER -race` → FAIL sebelum helper ada.
- [ ] **Step 3: Implement** helper (`transcribe`, `loadPCM16k`, `cer`, `loadRef`) dan test live. **MANUAL GATE:** operator mengisi `.env` dengan kunci nyata, menjalankan `go test -race -count=1 -tags=live -timeout 10m ./internal/provider/... -record` satu kali, me-review rekaman yang ditulis (tidak ada secret/PII; `grep -ri 'sk-\|xi-api\|Bearer' internal/provider/*/testdata`), mengganti fixture berbasis dokumentasi (`verified=false`) dengan rekaman nyata, dan menjalankan ulang seluruh test recorded (`make test`) — bila ada perbedaan format nyata vs fixture awal, perbaiki adapter + spec bab 8 + `docs/implementation-deviations.md`.
- [ ] **Step 4: Run tests to verify they pass** — `make test-live` (dengan kunci) → semua PASS < 5 menit; `make test` → PASS dengan fixture rekaman nyata; `grep -rc '"verified":false' internal/provider/*/testdata` → 0 untuk fixture yang direkam ulang.
- [ ] **Step 5: Commit** — `git add internal/provider && git commit -m "test(provider): live contract test per adapter dan mode -record; ganti fixture dengan rekaman nyata"`

---

### Task 16: Sesi e2e `PROVIDER_MODE=real` skala kecil — **MANUAL GATE (kunci API + voice ID nyata)**

**Files:**
- Create: `test/e2e/real/run.sh`, `test/e2e/real/real_test.go` (`//go:build e2e && live`)
- Modify: `Makefile` (target `e2e-real`)
- Test: `test/e2e/real/real_test.go`

**Interfaces:**
- Consumes: M5 `bin/realtime`, `bin/api`, `bin/kaiwa-cli converse` (flag §14.11.2), `testdata/audio/ja_self_intro.wav`; Task 13–14; voice ID nyata dari Task 22 (sebelum itu gunakan voice ID sementara di `seed/env/local/0900_voice_ids.sql` dari akun operator, tidak di-commit).
- Produces: `make e2e-real` = `test/e2e/real/run.sh`: start Postgres/Redis/RabbitMQ (`docker compose up -d --wait`), `make migrate-up seed`, `seed/env/local/0900_voice_ids.sql`, jalankan `bin/api` & `bin/realtime` dengan `PROVIDER_MODE=real`, `kaiwa-cli register/login`, `kaiwa-cli converse --wav testdata/audio/ja_self_intro.wav --duration 300 --end after-last --out-dir out/e2e-real`, lalu cek keluaran; exit 2 bila kunci hilang (pesan menyebut variabel, bukan nilainya).

- [ ] **Step 1: Write the failing test**
```go
//go:build e2e && live
func TestRealProviderSingleSession(t *testing.T) {
	testutil.RequireEnv(t, "ELEVENLABS_API_KEY", "DEEPGRAM_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY")
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
- [ ] **Step 3: Implement** `run.sh` (`set -euo pipefail`, trap untuk mematikan proses & `docker compose down -v`) dan target `e2e-real`. **MANUAL GATE:** operator menjalankan `make e2e-real` dengan kunci nyata dan voice ID nyata (Task 22), mendengarkan `out/e2e-real/turn-001-gen-1.wav` dan mencatat verdict (jelas/Jepang/sesuai karakter) serta `turn_latency_ms` di `docs/implementation-log.md`.
- [ ] **Step 4: Run tests to verify they pass** — `make e2e-real` → exit 0, `ls out/e2e-real/*.wav` ≥ 1 file, `jq '.turns|length' out/e2e-real/summary.json` ≥ 1; `make test-e2e` (fake) tetap PASS.
- [ ] **Step 5: Commit** — `git add test/e2e/real Makefile && git commit -m "test(e2e): sesi real-provider skala kecil (make e2e-real)"`

---

### Task 17: `provider-bench` — inti metrik, gate, dan seleksi model (pure unit)

**Files:**
- Create: `cmd/kaiwa-cli/internal/bench/{manifest.go,metrics.go,gates.go,select.go,models.go}`
- Test: `cmd/kaiwa-cli/internal/bench/{metrics_test.go,gates_test.go,select_test.go,manifest_test.go}`

**Interfaces:**
- Consumes: `provider.TokenUsage`, `provider.PriceTable`.
- Produces (nama dari plan): `type Item struct{ ID, Set, AudioURI, ReferenceText, Speaker string; DurationMs int64; Tags []string; Messages []provider.Message \`json:"messages,omitempty"\`; Expression string \`json:"expression,omitempty"\` }`; `LoadManifest(path string) ([]Item, error)` (menolak `set` di luar `STT-native|STT-learner|STT-noise|LLM-context|TTS-sentences`, `speaker` di luar `native|learner`, id ganda); `NormalizeForCER(s string) string` (NFKC-setara tanpa `x/text`: konversi lebar-penuh→setengah-lebar untuk angka/latin, hapus tanda baca & spasi, katakana→hiragana); `CER(ref, hyp string) float64` (Levenshtein rune / `len(ref)`); `Percentile(xs []float64, p float64) float64` (nearest-rank); `type Candidate struct{ Capability provider.Capability; Provider provider.Name; Model, Voice string; Role string /*PRIMARY|FALLBACK*/ }`; `type Metrics struct{ P50,P95 map[string]float64; CERNative, CERLearner, ErrorRate, FormatOK, QualityMean, MOS, KanjiErrRate float64; CostPerMinUSD float64 }`; `type Gate struct{ Name string; Pass bool; Actual, Limit float64 }`; `EvalGates(cap provider.Capability, role string, m Metrics) []Gate` (nilai di Global Constraints); `CostPerMinute(p provider.PriceTable, c Candidate, u Usage) (float64, bool)` dengan asumsi §8.10.3 (6 turn, 60 s audio STT, 400 token input + 80 output per turn, 60 karakter TTS per turn); `SelectProfiles(passed []ScoredCandidate) (standard, premium *ScoredCandidate, notes []string)` (aturan 1–2 §8.10.4; PREMIUM biaya ≤ 3× STANDARD; kosong → catatan "tidak ada yang lolos, laporkan angka aktual; gate tidak diturunkan"); `SelectFallbackModel(passed []ScoredCandidate) *ScoredCandidate` (termurah vendor fallback lolos gate fallback); `FormatOK(text string) bool` (≥ 1 tag `<e:[a-z_]+>` bentuk valid atau tanpa tag, tanpa `{`/`[`/`**`/pagar kode, rasio rune Jepang ≥ 0,9 dari rune non-tag).

- [ ] **Step 1: Write the failing test**
```go
func TestCER(t *testing.T) {
	require.InDelta(t, 0.0, bench.CER("こんにちは。", "コンニチハ"), 1e-9)    // katakana→hiragana, tanda baca dibuang
	require.InDelta(t, 0.25, bench.CER("今日は雨", "今日は晴れ"), 0.2)         // lihat nilai persis di bawah
	require.InDelta(t, 1.0/6, bench.CER("日本語の勉強", "日本語の勉"), 1e-9)
}
func TestGatesPrimaryVsFallback(t *testing.T) {
	m := bench.Metrics{P95: map[string]float64{"stt_final_ms": 320}, CERNative: 0.07, CERLearner: 0.14, ErrorRate: 0.005}
	g := bench.EvalGates(provider.CapSTT, "PRIMARY", m); require.False(t, passed(g, "stt_final_ms"))  // 320 > 300
	g = bench.EvalGates(provider.CapSTT, "FALLBACK", m); require.True(t, passed(g, "stt_final_ms"))   // 320 ≤ 450
}
func TestSelectProfiles(t *testing.T) { /* A cost 1 quality 4.1, B cost 2.5 quality 4.6, C cost 4 quality 4.9 (>3x), D gagal gate → STANDARD=A, PREMIUM=B */ }
```
Ganti baris kedua `TestCER` dengan nilai tetap: `CER("今日は雨", "今日は雪") == 0.25`. Tambah: `TestPercentileNearestRank` (`[1..100]`, p95 → 95); `TestLoadManifestRejectsBadSet`; `TestFormatOK` (tag valid lolos; teks berisi `**` / JSON / Inggris murni gagal); `TestCostPerMinuteUsesAssumptions` (harga uji LLM 1 USD/1M input & 2 USD/1M output, TTS 10 USD/1M char, STT 0,6 USD/jam → `6×(400/1e6×1+80/1e6×2)+6×60/1e6×10+60/3600×0.6`); `TestSelectNoneQualifies` (notes menyebut "gate tidak diturunkan"); `TestSelectFallbackModelCheapestPassing`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kaiwa-cli/internal/bench/ -race`. Expected: FAIL.
- [ ] **Step 3: Implement** murni (tanpa jaringan/waktu). `models.go`: parser `bench/models.yaml` memakai `gopkg.in/yaml.v3` (`candidates: [{capability, provider, model, voice, role}]`, `KnownFields(true)`, `provider`/`capability` UPPERCASE).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/kaiwa-cli/internal/bench/ -race -cover` → PASS, cover ≥ 85%.
- [ ] **Step 5: Commit** — `git add cmd/kaiwa-cli/internal/bench && git commit -m "feat(kaiwa-cli): inti benchmark provider (CER, gate, seleksi STANDARD/PREMIUM)"`

---

### Task 18: `provider-bench` — runner suite, pencatat hasil, output file config

**Files:**
- Create: `cmd/kaiwa-cli/internal/bench/{runner_stt.go,runner_llm.go,runner_tts.go,runner_e2e.go,runner_conc.go,output.go}`, `cmd/kaiwa-cli/provider_bench.go`
- Modify: `cmd/kaiwa-cli/main.go` (daftarkan subcommand `provider-bench`, `provider-report`)
- Test: `cmd/kaiwa-cli/internal/bench/runner_test.go`, `output_test.go`, `cmd/kaiwa-cli/provider_bench_test.go`

**Interfaces:**
- Consumes: Task 17, M4 `fake.*` (di test), `providerwire.Build` (di `provider_bench.go`), `objectstore`-free: audio dibaca dari `file://`/path lokal atau `s3://` lewat `objectstore` bila `--audio-root` bukan lokal (gunakan `--audio-root <dir>` hasil `aws s3 sync`; `objectstore` tidak diimpor).
- Produces: CLI `kaiwa-cli provider-bench --suite stt|llm|tts|e2e|concurrency|all --dataset testdata/benchmark --models-file bench/models.yaml --out bench/results/<tanggal>/ --repeat 3 [--audio-root DIR] [--turn-end-ms 0] [--concurrency-steps 10,50,100,200]`; fungsi `RunSTT(ctx, p provider.STTProvider, c Candidate, items []Item, audio AudioSource, repeat int, clk clock.Clock) []Result` (connect/first-partial/final-after-Finalize/CER/jumlah revisi partial; audio dikirim 20 ms-paced bila `Pacing=true`), `RunLLM(ctx, p provider.LLMProvider, …)` (TTFT, time-to-first-sentence sampai `。！？`, total, `FormatOK`, panjang rune), `RunTTS(ctx, p provider.TTSProvider, …)` (TTFA, RTF = waktu total/durasi audio, bytes), `RunE2E(...)` (komposisi STT→LLM→TTS in-process; `turn_end_to_first_audio_ms = stt_final + llm_first_sentence + tts_first_audio + turn_end_ms` — **penyimpangan dari §8.10.3** (tanpa VAD/turn engine nyata): dicatat di `docs/implementation-deviations.md`), `RunConcurrency(ctx, steps []int, call func(ctx) error, clk)` (ramp bertahap; P95, % `KindRateLimited`, % error); `WriteResults(dir string, rs []Result) error` (`results.jsonl` satu baris per percobaan, `summary.csv` per kandidat+suite berisi metrik & hasil gate), `WriteProfileEnv(path string, std, prem, fb *ScoredCandidate, sttModels map[provider.Name]string, evalModels map[provider.Name]string, concurrencyComments map[string]string) error` (format §8.10.5: hanya variabel `PROVIDER_PROFILE_*`, `PROVIDER_STT_*_MODEL`, `PROVIDER_EVALUATION_*_MODEL`, `PROVIDER_CONCURRENCY_*` + komentar limit plan akun), `UpdatePricesYAML` TIDAK ditulis otomatis (manual, Task 21).

- [ ] **Step 1: Write the failing test** (runner diuji dengan `fake` + `clock.NewFake`, tanpa jaringan)
```go
func TestRunLLMMeasuresFirstSentence(t *testing.T) {
	l := fake.NewLLM(provider.FakePrimary, fastCap(), fake.Config{}, fakeDeps(t))   // opening template berisi "。"
	rs := bench.RunLLM(ctx, l, bench.Candidate{Provider: provider.FakePrimary, Model: "fake"}, []bench.Item{{ID: "c1", Set: "LLM-context", Messages: msgs}}, 2)
	require.Len(t, rs, 2); require.Greater(t, rs[0].Values["first_sentence_ms"], 0.0); require.True(t, rs[0].Flags["format_ok"])
}
func TestRunSTTComputesCERAndFinalLatency(t *testing.T) { /* fake STT mode fixture dengan testdata/audio WAV+txt → CER 0 untuk item yang cocok; final_after_finalize_ms terisi */ }
func TestWriteProfileEnvFormat(t *testing.T) { /* golden: baris "PROVIDER_PROFILE_STANDARD_LLM_MODEL=<m>", "PROVIDER_STT_ELEVENLABS_MODEL=…", komentar "# limit plan akun: …"; tanpa secret; tidak memuat kunci ELEVENLABS_API_KEY */ }
func TestConcurrencyRamp(t *testing.T) { /* call mengembalikan *provider.Error{KindRateLimited} untuk 30% → rate_limited_pct≈30 (±1) pada step 100 */ }
```
Tambah: `TestWriteResultsJSONLAndCSV` (jumlah baris = percobaan; kolom CSV tetap: `suite,capability,provider,model,voice,metric,p50,p95,gate,pass`); `TestProviderBenchFlagValidation` (`--suite x` → exit 2; `--out` kosong → exit 2; `--repeat 0` → exit 2; kandidat dengan vendor tak punya key → pesan menyebut env var); `TestProviderBenchDryRunWithFake` (`PROVIDER_MODE=fake` + `--models-file` berisi `FAKE_PRIMARY` → `results.jsonl` terisi; tanpa jaringan); `TestBenchDoesNotLogTextOrKeys`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kaiwa-cli/... -race -run 'RunLLM|RunSTT|WriteProfileEnv|Concurrency|WriteResults|ProviderBench|BenchDoesNot'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Setiap runner menerima port (bukan vendor); percobaan ke-`repeat` berurutan; error vendor dicatat sebagai baris `error_kind` (bukan abort); `--suite all` urut stt→llm→tts→e2e→concurrency; gate dievaluasi per kandidat sesuai `role` (PRIMARY/FALLBACK) di `bench/models.yaml`; ringkasan cetak ke stdout (tanpa teks dataset, hanya id & angka); `cmd/kaiwa-cli` membangun adapter lewat `providerwire.Build` dengan `RouterConfig` override per kandidat (model/voice dari YAML).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/kaiwa-cli/... -race` → PASS; `CGO_ENABLED=1 go build ./cmd/kaiwa-cli`; `./bin/kaiwa-cli provider-bench --help` mencantumkan semua flag.
- [ ] **Step 5: Commit** — `git add cmd/kaiwa-cli && git commit -m "feat(kaiwa-cli): provider-bench (runner stt/llm/tts/e2e/concurrency, hasil, provider-profile.env)"`

---

### Task 19: `kaiwa-cli provider-report` (§8.8.3)

**Files:**
- Create: `cmd/kaiwa-cli/internal/report/{aggregate.go,query.go,csv.go}`, `cmd/kaiwa-cli/provider_report.go`
- Test: `cmd/kaiwa-cli/internal/report/aggregate_test.go`, `query_integration_test.go` (tag `integration`), `provider_report_test.go`

**Interfaces:**
- Consumes: `pricing.Load`, `provider.PriceTable`, `testutil.NewPostgres`, DDL `session_turns` (§5), `evaluations` bila ada (kolom `overall_score` Bab 10; **bila M7 belum ada, kolom `avg_eval_*` kosong, bukan error**).
- Produces: `kaiwa-cli provider-report --from 2026-10-01 --to 2026-10-08 --format csv --prices prices.yaml` (membaca `DATABASE_URL`; koneksi `default_transaction_read_only=on`; `--format csv|json`); `type TurnAgg struct{ SttProvider, LLMProvider, LLMModel, TTSProvider, TTSModel string; Turns int; P50,P95,P99TurnEndToFirstAudioMs, P95SttFinalMs, P95LLMFirstTokenMs, P95TTSFirstAudioMs float64; FailedRate, InterruptedRate, FallbackRate float64; InputTokens, OutputTokens, TTSCharacters, STTAudioMs int64 }`; `Query(ctx, db Querier, from, to time.Time) ([]TurnAgg, error)` (SQL sketsa §8.8.3, `status IN ('COMPLETED','INTERRUPTED','FAILED')`, `percentile_cont`); `Build(aggs []TurnAgg, p provider.PriceTable) []Row` menambah `avg_cost_usd_per_turn` dan `cost_usd_per_active_minute` (menit aktif = `Σ stt_audio_ms`/60000 sebagai pendekatan; dicatat di header CSV komentar) ; `WriteCSV(w io.Writer, rows []Row) error` dengan kolom persis daftar §8.8.3.

- [ ] **Step 1: Write the failing test**
```go
func TestBuildCost(t *testing.T) {
	a := report.TurnAgg{LLMProvider: "OPENAI", LLMModel: "m", TTSProvider: "ELEVENLABS", TTSModel: "t", SttProvider: "ELEVENLABS", Turns: 10, InputTokens: 4000, OutputTokens: 800, TTSCharacters: 600, STTAudioMs: 600000}
	rows := report.Build([]report.TurnAgg{a}, priceStub{llmIn: 1, llmOut: 2, ttsPer1M: 10, sttPerHour: 0.6})
	require.InDelta(t, (4000.0/1e6*1+800.0/1e6*2+600.0/1e6*10+600000.0/3.6e6*0.6)/10, rows[0].AvgCostUSDPerTurn, 1e-9)
}
func TestCSVColumns(t *testing.T) { /* header persis: stt_provider,llm_provider,llm_model,tts_provider,tts_model,turns,p50_turn_end_to_first_audio_ms,p95_…,p99_…,p95_stt_final_ms,p95_llm_first_token_ms,p95_tts_first_audio_ms,failed_rate,interrupted_rate,fallback_rate,avg_cost_usd_per_turn,cost_usd_per_active_minute,avg_eval_overall_score */ }
```
Integration `TestQueryGroupsByStack`: `testutil.NewPostgres` + helper insert sesi & `session_turns` (3 kombinasi stack, status campur COMPLETED/INTERRUPTED/FAILED/DISCARDED, `created_at` dalam/di luar rentang) → baris = 3 kombinasi; DISCARDED & di luar rentang tidak dihitung; `failed_rate` = FAILED/total; P95 `turn_end_to_first_audio_ms` sesuai `percentile_cont`. `TestReportRefusesWrites` (koneksi read-only: `INSERT` gagal). `TestProviderReportFlags` (`--from` > `--to` → exit 2; `DATABASE_URL` kosong → exit 2 menyebut nama variabel).
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kaiwa-cli/... -race -run 'BuildCost|CSVColumns|ProviderReportFlags'`; `go test -tags integration ./cmd/kaiwa-cli/... -race -run 'QueryGroups|RefusesWrites'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Query memakai `pgx` langsung (bukan sqlc: tool, bukan repository fitur; lisan di `docs/implementation-log.md`); `avg_eval_*` di-join hanya bila tabel `evaluations` ada (`to_regclass`).
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS; `./bin/kaiwa-cli provider-report --help`.
- [ ] **Step 5: Commit** — `git add cmd/kaiwa-cli && git commit -m "feat(kaiwa-cli): provider-report (stack × latency/failed/fallback/biaya)"`

---

### Task 20: Dataset golden Jepang + `bench/models.yaml` — **MANUAL GATE (manusia: penutur asli, persetujuan tertulis, reviewer Jepang)**

**Files:**
- Create: `testdata/benchmark/manifest.jsonl`, `testdata/benchmark/README` (format & aturan rekaman, bukan dokumentasi tambahan lain), `bench/models.yaml`
- Test: `cmd/kaiwa-cli/internal/bench/manifest_test.go` (menambah `TestRepoManifestValid`)

**Interfaces:**
- Consumes: `bench.LoadManifest`, `bench.LoadModels` (Task 17); bucket privat `s3://kaiwa-benchmark/v1/` (§8.10.2); CONVERSATION prompt nyata dari seed (`prompt.Render` pada 30 konteks).
- Produces: dataset sesuai §8.10.2: STT-native ≥ 40 (jawaban pendek `はい`/`そうですね`, kalimat 8–15 s, angka/tanggal/harga, katakana serapan, nama tempat), STT-learner ≥ 40 (pelajar Indonesia N5–N3; filler, jeda, salah ucap), STT-noise 10 (noise kafe/jalan SNR 10 dB), LLM-context 30 (3–8 turn riwayat; field `messages` berisi system prompt CONVERSATION nyata hasil render + riwayat), TTS-sentences 50 (pendek, tanya, emosi, kanji sulit, angka; `expression` terisi); subset 10 file berlisensi bebas di `testdata/audio/` untuk fake-STT CI; `bench/models.yaml` kandidat: LLM 2–3 model OpenAI + 2 model Gemini (`role` PRIMARY/FALLBACK), TTS 3 model ElevenLabs × voice kandidat + 2 suara Aura-2 ja (`aura-2-fujin-ja`, `aura-2-izanami-ja`, `aura-2-uzume-ja`, `aura-2-ebisu-ja`, `aura-2-ama-ja` dipilih 2), STT `scribe_v2_realtime`, `nova-3`, `nova-2`.

- [ ] **Step 1: Write the failing test**
```go
func TestRepoManifestValid(t *testing.T) { // memuat testdata/benchmark/manifest.jsonl
	items, err := bench.LoadManifest(testutil.TestdataPath(t, "benchmark/manifest.jsonl")); require.NoError(t, err)
	count := map[string]int{}; for _, it := range items { count[it.Set]++; require.NotEmpty(t, it.ReferenceText) }
	require.GreaterOrEqual(t, count["STT-native"], 40); require.GreaterOrEqual(t, count["STT-learner"], 40); require.Equal(t, 10, count["STT-noise"])
	require.GreaterOrEqual(t, count["LLM-context"], 30); require.GreaterOrEqual(t, count["TTS-sentences"], 50)
	ms, err := bench.LoadModels(testutil.TestdataPath(t, "../bench/models.yaml")); require.NoError(t, err); require.NotEmpty(t, ms)
}
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./cmd/kaiwa-cli/internal/bench/ -race -run TestRepoManifestValid`. Expected: FAIL (manifest belum ada).
- [ ] **Step 3: Checklist MANUAL GATE** (selesai bila Step 4 lulus): (1) rekrut penutur asli + pelajar Indonesia, persetujuan tertulis tersimpan di luar repo; (2) rekam WAV 16 kHz mono (STT-noise: campur noise SNR 10 dB), upload ke `s3://kaiwa-benchmark/v1/`, isi `audio_uri`, `reference_text` diverifikasi dua orang; (3) tulis 30 konteks LLM (render CONVERSATION prompt nyata); (4) tulis 50 kalimat TTS + label ekspresi; (5) pilih 10 file berlisensi bebas ke `testdata/audio/` (`.txt` sidecar); (6) tulis `bench/models.yaml` (kandidat, role, voice) dari daftar model yang tersedia di akun saat ini (verifikasi di dokumentasi vendor, jangan menebak nama model); (7) tetapkan 2 reviewer berbahasa Jepang untuk penilaian 1–5 (naturalness, kesesuaian level, konsistensi karakter) dan MOS TTS; format lembar penilaian: CSV `item_id,reviewer,candidate,score_naturalness,score_level,score_character,mos,kanji_misread(0|1)` disimpan di `bench/results/<tanggal>/human.csv`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./cmd/kaiwa-cli/internal/bench/ -race -run TestRepoManifestValid` → PASS; `aws s3 ls s3://kaiwa-benchmark/v1/ | wc -l` ≥ 90 objek audio.
- [ ] **Step 5: Commit** — `git add testdata/benchmark bench/models.yaml testdata/audio && git commit -m "test(benchmark): dataset golden Jepang (manifest) dan kandidat model"`

---

### Task 21: Jalankan benchmark, verifikasi dukungan Jepang streaming Scribe/Nova, tetapkan model & harga — **MANUAL GATE (kunci API, akun, biaya nyata, penilai manusia)**

**Files:**
- Create: `docs/provider-benchmark.md`, `deploy/config/provider-profile.env`, `bench/results/<tanggal>/{results.jsonl,summary.csv,human.csv}`
- Modify: `internal/provider/pricing/prices.yaml` (harga resmi + tanggal + URL sumber per entri, komentar YAML), `internal/platform/config` default model & `.env.example` hanya bila hasil berbeda dari placeholder §8.5.1, spec Bab 8 §8.4.x/§8.5.1/§8.10 bila ada perbedaan
- Test: `internal/provider/pricing/pricing_test.go` (menambah `TestShippedPricesNonZeroForSelectedModels`), `internal/platform/config` test default

**Interfaces:**
- Consumes: Task 15, 17–19; dataset Task 20; akun vendor dengan kunci; region deployment produksi (benchmark dijalankan dari sana).
- Produces: `docs/provider-benchmark.md` (format keluaran di bawah), `deploy/config/provider-profile.env` (format §8.10.5), `prices.yaml` terisi (`version` = tanggal benchmark).

- [ ] **Step 1: Write the failing test**
```go
func TestShippedPricesNonZeroForSelectedModels(t *testing.T) { // membaca deploy/config/provider-profile.env (parse KEY=VAL) + pricing.Load("")
	env := parseEnvFile(t, testutil.TestdataPath(t, "../deploy/config/provider-profile.env"))
	tb, _ := pricing.Load("")
	for _, m := range selectedModels(env) { usd, ok := priceProbe(tb, m); require.True(t, ok, m.String()); require.Greater(t, usd, 0.0, m.String()) }
}
```
(`selectedModels` = model LLM PREMIUM/STANDARD + fallback, TTS, STT, evaluation, embedding yang tercantum di env file; `Missing` M4 dipakai sebagai pembanding.)
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/pricing/ -race -run TestShippedPrices`. Expected: FAIL (env file belum ada / harga 0).
- [ ] **Step 3: Checklist MANUAL GATE:**
  1. **Verifikasi dukungan Jepang streaming (Bab 8 "Masalah", D-NEW-8-23):** jalankan `kaiwa-cli provider-bench --suite stt --models-file bench/models.yaml --out bench/results/<tanggal>/ --repeat 3`. Keluaran per kandidat: `{model, "ja" diterima? (ya/tidak + pesan error vendor), CER native, CER learner, P95 final-after-Finalize}`. Scribe `scribe_v2_realtime` gagal menerima `ja`/CER > gate → ikuti §8.10.4 butir 5 (lapor pemilik produk; boleh tukar `PROVIDER_STT_PRIMARY/FALLBACK` di config; mengubah D-040 butuh update register). Nova-3 `ja` gagal di `/v1/listen` → `PROVIDER_STT_DEEPGRAM_MODEL=nova-2`.
  2. Jalankan `--suite llm`, `--suite tts`, `--suite e2e`, `--suite concurrency` (ramp 10→50→100→200; catat limit plan akun tiap vendor), lalu penilaian manusia (`human.csv`: kualitas LLM rata-rata, MOS TTS, kesalahan baca kanji); hitung ulang gate dengan `kaiwa-cli provider-bench --suite all --human bench/results/<tanggal>/human.csv --out …`.
  3. TTS fallback: bila Aura-2 gagal `TTS_FALLBACK_JA_QUALITY` (MOS ≥ 3,5 & kanji ≤ 5%) → `PROVIDER_TTS_FALLBACK=OPENAI`, kerjakan Task 23, ulangi suite TTS untuk OpenAI.
  4. Seleksi otomatis (`SelectProfiles`): STANDARD termurah lolos gate primary; PREMIUM kualitas tertinggi lolos gate primary dengan biaya ≤ 3× STANDARD; model fallback per profile = termurah vendor fallback lolos gate fallback. Tidak ada yang lolos → laporkan angka aktual, JANGAN turunkan gate.
  5. Isi `prices.yaml` dari halaman harga resmi (komentar: tanggal + URL), `version` = tanggal; jalankan `kaiwa-cli provider-bench` biaya per menit untuk keputusan.
  6. Tulis `deploy/config/provider-profile.env` (`WriteProfileEnv`) dan `docs/provider-benchmark.md` dengan format: (a) ringkasan & tanggal & region; (b) tabel gate per kandidat (`kandidat | metrik | nilai | batas | lulus`); (c) tabel keputusan (`profil/peran | model | vendor | biaya per menit`); (d) hasil verifikasi dukungan Jepang Scribe/Nova (ya/tidak + bukti); (e) daftar semua item "verifikasi di task adapter" beserta hasil (Task 2–11 Step 0); (f) limit plan akun per vendor → nilai `PROVIDER_CONCURRENCY_*` = `floor(limit × 0,9 / replika)`; (g) penyimpangan & rekomendasi ke pemilik produk. Perbarui spec Bab 8 bila ada perbedaan; `docs/implementation-deviations.md` untuk lokasi dokumen (`docs/provider-benchmark.md` menggantikan `docs/benchmarks/provider-benchmark-<tanggal>.md` pada §8.10.5 butir 4).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/pricing/ ./internal/platform/config/ -race` → PASS; `make modelname-check` → exit 0; `test -s docs/provider-benchmark.md && grep -c 'lulus' docs/provider-benchmark.md` > 0; `make test` → PASS.
- [ ] **Step 5: Commit** — `git add docs deploy bench internal/provider/pricing internal/platform/config .env.example && git commit -m "docs(provider): hasil benchmark, model awal, harga resmi, provider-profile.env"`

---

### Task 22: Voice ID nyata + verifikasi Kansai — **MANUAL GATE (akun ElevenLabs/Deepgram + telinga manusia)**

**Files:**
- Create: `seed/env/<env>/0900_voice_ids.sql` (tiap env: `staging`, `prod`; `local` tidak di-commit)
- Modify: `seed/*` capabilities (`UPDATE voice_provider_configs SET capabilities=…`) lewat file `seed/env/<env>/0910_voice_caps.sql`
- Test: `internal/postgres/voicecheck_repo_integration_test.go` (menambah `TestEnvVoiceSeedClearsPlaceholders`, tag `integration`)

**Interfaces:**
- Consumes: Task 14 `PlaceholderVoices`, hasil Task 21 (model & vendor terpilih), seed §6.13.4 (4 voice profile: `JA_F_KANTO_PREMIUM_01`, `JA_M_KANTO_PREMIUM_01`, `JA_F_KANSAI_STANDARD_01`, `JA_M_KANTO_STANDARD_01`; config id `01920000-0009-7000-8000-00000000000N`).
- Produces: `0900_voice_ids.sql` berformat §6.13.3 (`BEGIN; UPDATE voice_provider_configs SET provider_voice_id='<id nyata>', updated_at=now() WHERE id='<id>' AND provider_voice_id LIKE 'PLACEHOLDER\_%'; … COMMIT;`) satu baris per config ELEVENLABS dan DEEPGRAM (8 baris); `0910_voice_caps.sql` yang menambah `AUDIO_TAGS` (hanya bila model efektif ElevenLabs mendukung audio tag) dan `KANSAI_DIALECT` (hanya untuk voice Kansai setelah verifikasi manusia) — `SPEED` Deepgram hanya bila Task 10 Step 0(c) membuktikan `speed` berlaku untuk Aura-2 Jepang.

- [ ] **Step 1: Write the failing test**
```go
func TestEnvVoiceSeedClearsPlaceholders(t *testing.T) { // integration; env dari KAIWA_SEED_ENV via testutil (staging)
	pool := testutil.NewPostgres(t); applySeed(t, pool); applyEnvSeed(t, pool, "staging")
	got, err := postgres.NewVoiceCheckRepo(pool).PlaceholderVoices(ctx, []provider.Name{provider.ElevenLabs, provider.Deepgram})
	require.NoError(t, err); require.Empty(t, got)
}
```
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres/ -race -run TestEnvVoiceSeed`. Expected: FAIL (8 placeholder tersisa).
- [ ] **Step 3: Checklist MANUAL GATE:** (1) pilih voice ElevenLabs untuk 4 voice profile (kriteria: dialek sesuai `accent` — KANTO standar Tokyo, KANSAI Osaka; gender; tier PREMIUM vs STANDARD; lulus gate TTS Task 21); (2) **Kansai (`JA_F_KANSAI_STANDARD_01`)**: dua penutur Kansai asli mendengarkan ≥ 10 kalimat sampel dan menyatakan "terdengar Kansai: ya/tidak" (format: CSV `voice,vendor,sample_id,reviewer,kansai_ok`); `KANSAI_DIALECT` hanya diset bila kedua reviewer ya, kalau tidak: voice Kansai tetap tanpa kapabilitas itu dan (opsi) `instructions` OpenAI dicatat sebagai satu-satunya dukungan dialek; (3) pilih 4 suara Aura-2 (`aura-2-*-ja`) untuk DEEPGRAM, satu per voice profile, sesuai gender; (4) tulis dua file SQL per env; (5) jalankan `make seed KAIWA_ENV=staging`, lalu `kaiwa-cli provider-bench --suite tts` ulang dengan voice final untuk memastikan gate lulus; (6) catat voice ID & verdict di `docs/provider-benchmark.md` (bagian "Voice").
- [ ] **Step 4: Run tests to verify they pass** — `go test -tags integration ./internal/postgres/ -race -run 'VoiceCheck|TestEnvVoiceSeed'` → PASS; `make seed KAIWA_ENV=staging && psql "$DATABASE_URL" -tAc "select count(*) from voice_provider_configs where provider_voice_id like 'PLACEHOLDER\_%' and provider in ('ELEVENLABS','DEEPGRAM')"` → `0`; `TestSeedContentIsValid` (M3) tetap PASS dengan `PROVIDER_MODE=real` di env uji.
- [ ] **Step 5: Commit** — `git add seed && git commit -m "feat(seed): voice ID nyata dan kapabilitas hasil verifikasi per env"`

---

### Task 23: (Kondisional) OpenAI TTS — hanya bila Aura-2 gagal gate `TTS_FALLBACK_JA_QUALITY` (§8.4.7)

**Files:**
- Create: `internal/provider/openai/tts.go`, `testdata/recordings/tts_{basic,401}.jsonl`
- Modify: `cmd/internal/providerwire/build.go` (cabang `OPENAI` untuk `CapTTS`), `internal/provider/openai/conformance_test.go`
- Test: `internal/provider/openai/tts_test.go`

**Interfaces:**
- Consumes: Task 2 (`Config`, `classify`), Task 13 (`Build`), M4 `TTSSuite`.
- Produces: `openai.NewTTS(cfg Config, hc *http.Client, log *slog.Logger) (*TTS, error)`; `Synthesize` (`POST {BaseURL}/audio/speech`). **Lewati task ini (centang "N/A — Aura-2 lulus gate" dengan bukti dari `docs/provider-benchmark.md`) bila Task 21 menyatakan Aura-2 lulus.**

- [ ] **Step 0 (verifikasi di task adapter):** https://platform.openai.com/docs/api-reference/audio/createSpeech — `response_format:"pcm"` = 24 kHz 16-bit LE mono tanpa header, `speed` dan `instructions` didukung model terpilih.
- [ ] **Step 1: Write the failing test**
```go
func TestOpenAITTSRequest(t *testing.T) {
	st, err := tts.Synthesize(ctx, provider.TTSRequest{Text: "こんにちは。", Model: "m_tts", Resolved: provider.ResolvedTTSParams{ProviderVoiceID: "nova", Params: map[string]any{"instructions": "Speak Japanese with a natural Kansai (Osaka) accent.", "speed": 1.0}}})
	require.NoError(t, err); chunks := drainTTS(st.Chunks()); r := srv.Requests()[0]
	require.Equal(t, "/audio/speech", r.Path); require.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
	require.JSONEq(t, `{"model":"m_tts","input":"こんにちは。","voice":"nova","response_format":"pcm","instructions":"Speak Japanese with a natural Kansai (Osaka) accent.","speed":1.0}`, string(r.Body))
	require.True(t, chunks[len(chunks)-1].Final)
}
```
Tambah: `TestOpenAITTSOmitsEmptyInstructionsAndSpeed`; `TestOpenAITTSOddByteBoundary`; `TestOpenAITTSErrors` (401/429/400 `content_policy_violation`→`KindContentFiltered`/5xx); `TestOpenAITTSCancelWithin50ms`; `OpenAITTSConformance` (`TTSSuite`); `TestBuildSwappedFallbackTTSOpenAI` (Task 13) kini PASS.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/openai/ ./cmd/internal/providerwire/ -race -run 'OpenAITTS|SwappedFallback'`. Expected: FAIL.
- [ ] **Step 3: Implement** seperti `elevenlabs.TTS` dengan format §8.4.7 (`Usage.AudioMs = bytes/48`).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/openai/ ./cmd/internal/providerwire/ -race` → PASS; `go test -tags live ./internal/provider/openai -run TestLiveOpenAITTS` (dengan kunci) → ≥ 0,5 s PCM.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/openai): TTS contingency (fallback bila Aura-2 gagal gate)"`

---

## Gerbang milestone

- [ ] `make test` (race, tanpa jaringan) → PASS; semua adapter lulus recorded-fixture + `providertest` (`go test ./internal/provider/... -race -count=2`), `internal/provider/...` coverage ≥ 70% (§14.8).
- [ ] `make lint arch-check modelname-check sqlc-check` → exit 0 (adapter tidak mengimpor `conversation/audio`/`fake`/`pricing`; api/worker `CGO_ENABLED=0`).
- [ ] `make test-integration` → PASS (`VoiceCheck`, `provider-report` query).
- [ ] `grep -rc '"verified":false' internal/provider/*/testdata` → 0 untuk semua fixture yang tercakup rekaman live (Task 15).
- [ ] **MANUAL GATE:** `make test-live` dengan semua kunci → PASS < 5 menit (Task 15).
- [ ] **MANUAL GATE:** `make e2e-real` (1 sesi, `PROVIDER_MODE=real`) → ≥ 1 file `turn-*-gen-*.wav` > 0,5 s dan `summary.json` ≥ 1 turn; verdict pendengaran dicatat (Task 16).
- [ ] **MANUAL GATE:** `bench/results/<tanggal>/{results.jsonl,summary.csv,human.csv}`, `docs/provider-benchmark.md` terisi (tabel gate, keputusan STANDARD/PREMIUM/fallback, hasil verifikasi Jepang Scribe/Nova, limit plan akun), `deploy/config/provider-profile.env`, `prices.yaml` non-nol untuk model terpilih (Task 21); voice ID nyata + verdict Kansai (Task 22).
- [ ] `PROVIDER_MODE=real` tanpa kunci / dengan voice placeholder → `realtime` exit 1 dengan pesan menyebut env/voice (Task 14).
- [ ] Hasil perintah gerbang dicatat di `docs/implementation-log.md` (master aturan 6).

## Self-Review

- **Cakupan spec:** §8.4.0 (aturan umum: Task 1–11, 12), §8.4.1 (T7–8), §8.4.2 (T9), §8.4.3 (T2), §8.4.4 (T4), §8.4.5 (T6), §8.4.6 (T10), §8.4.7 (T23 kondisional), §8.4.8 (T3), §8.4.9 (T5), §8.4.10 (T3), §8.4.11 (T11), §8.4.12 (dikonsumsi adapter via `req.Resolved`; tes di T6/T10/T23), §8.4.13 (tabel verifikasi diperbarui T21), §8.5.1 validasi startup (T13–14), §8.8.3 (T19), §8.10 (T17–22), §8.11 (T1, T12, T15), §14.6 (T1, T15), §6.13.3 (T14, T22), "Masalah" dukungan Jepang Scribe/Nova (T21). Tidak dicakup (milik milestone lain): router/fallback/breaker (M4), `ResolveTTSParams` (M4), metrik Prometheus router & biaya (M4), worker evaluasi (M7), manifest k8s untuk `provider-profile.env` (M10).
- **Konsistensi tipe:** semua `New<Cap>(cfg, hc, log)` sesuai §8.1 butir 3; `Config` per vendor menyimpan `Tap`, timer sintetis; `RealDeps` field = `RouterDeps` §8.5.2; `providertest` dipanggil dengan signature M4 apa adanya.
- **Ambiguitas spec & keputusan:** (1) lokasi fixture: §8.11 (`testdata/<cap>_*.jsonl`) vs §14.6 (`testdata/recordings/<case>.jsonl`) → pakai `testdata/recordings/<cap>_<case>.jsonl`; (2) `internal/testutil/replay` di §2.2 = replay timeline STT untuk turn engine, di §14.6 = server vendor → file vendor-sim ditambahkan di paket yang sama (`httpsim.go`, `wssim.go`); (3) sinyal rotasi STT antara adapter dan `resilientSTT` tidak dinamai → `Err.VendorCode` kode vendor persis, diverifikasi terhadap router M4 di T12; (4) `docs/provider-benchmark.md` (brief) vs `docs/benchmarks/provider-benchmark-<tanggal>.md` (§8.10.5) → `docs/provider-benchmark.md`, dicatat di deviations; (5) benchmark e2e tanpa VAD/turn engine nyata (§8.10.3) → komposisi in-process + `--turn-end-ms`, dicatat di deviations; (6) env base URL/knob vendor ditambahkan ke `RouterConfig` hanya bila M4 belum punya.
