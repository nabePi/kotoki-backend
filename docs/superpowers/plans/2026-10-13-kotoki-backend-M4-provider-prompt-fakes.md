# Kotoki Backend — M4 Provider layer, Prompt & Fakes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Membangun lapisan provider (port, error taxonomy, router dengan breaker/fallback/replay STT/Generation, metering, fake provider lengkap + fault injection, `providertest`) dan paket `internal/prompt` (Render, token estimator, budget, parser kalimat + tag ekspresi, template v1) sehingga M5 dapat menjalankan percakapan end-to-end dengan `PROVIDER_MODE=fake`.
**Architecture:** `internal/provider` (paket fitur) hanya mengimpor stdlib, `uuid`, `x/sync/semaphore`, `prometheus`, `platform/clock`; sub-paket `pricing`, `httpx`, `fake`, `providertest` mengimpor `provider` (tidak pernah sebaliknya); data luar masuk lewat port `ExpressionCatalog` dan `PriceTable` yang di-inject `cmd/*` lewat `RouterDeps`. `internal/prompt` tidak mengimpor paket fitur lain. Adapter vendor nyata BUKAN bagian milestone ini (M9); `providertest` ditulis agar M9 memakainya apa adanya.
**Tech Stack:** Go 1.25, `golang.org/x/sync/semaphore`, `prometheus/client_golang`, `google/uuid`, `text/template`, `gopkg.in/yaml.v3` (D-003, hanya `provider/pricing`), `testify`, `go-redis/v9` (hanya `internal/redisstore/fakefaults.go`), testcontainers-redis (hanya integration), `caarlos0/env/v11` (tag struct; test parse).
**Spec:** docs/superpowers/specs/2026-10-12-kotoki-backend-design.md — Bab 8 kecuali §8.4.1–8.4.11 & §8.10 (M9), §6.6.2–6.6.3 (`ResolveTTSParams`), §6.8 (prompt), §7.7.3 (parser), §7.8.1/7.8.3 (`EstimateTokens`, `Budget`), §14.6/14.8 (coverage), D-040–D-043, D-080, D-081, S-7, S-9, S-12, S-13.
**Master plan:** docs/superpowers/plans/2026-10-13-kotoki-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M1 saja — `clock.Clock` (`Now() time.Time`) dan `clock.NewFake(t0)` dengan method `Advance(d time.Duration)` (kontrak M1, dijamin tersedia), paket provider TIDAK memakai `errs` (error provider = `*provider.Error`; konstanta M1 bernama `errs.Code<Pascal>` tidak relevan di milestone ini). Tidak bergantung pada M2/M3 (paralel diizinkan).

## Global Constraints

- Master Global Constraints berlaku.
- `provider.Name`: `ELEVENLABS DEEPGRAM OPENAI GEMINI AZURE FAKE_PRIMARY FAKE_FALLBACK NONE`; `Capability`: `STT LLM TTS EVALUATION EMBEDDING PRONUNCIATION`; `ProfileName`: `PREMIUM STANDARD` — semua UPPERCASE di Go, JSON, label metrik, kunci fault (D-040, §8.2.1).
- Paket `internal/provider` DILARANG mengimpor `net/http`, `coder/websocket`, `go-redis`, paket fitur lain (termasuk `content`, `prompt`), maupun sub-paketnya; `fake` tidak mengimpor go-redis maupun `httpx` (§8.1 butir 1, 5). `time.Now()` dilarang di `internal/provider/**` selain adapter vendor (forbidigo): pakai `clock.Clock`.
- Audio: STT input PCM16LE mono **16000 Hz** (`STTSampleRate`), TTS output PCM16LE mono **24000 Hz** (`TTSSampleRate`), pronunciation WAV 16 kHz; panjang PCM selalu genap (§8.2).
- Timeout default: STT connect 3s, STT final 1500ms, LLM first token 2s, LLM total 15s, TTS first audio 1500ms, TTS sentence total 10s, Evaluation 60s, Embedding 5s, Pronunciation 15s, `PROVIDER_SESSION_GRACE` 15s (§8.6.1).
- Breaker per (vendor, kapabilitas) per proses: threshold 5, window 30s, open 20s, half-open probes 1 (D-043, §8.5.4).
- Konkurensi default per pod: STT 125 (tunggu 0 ms), LLM 60, TTS 80 (tunggu 100 ms realtime), Evaluation 8, Embedding 16, Pronunciation 8, LLM worker 8 (tunggu 30 s async) (§8.5.6).
- Retry async: GenerateJSON 2+2 attempt (backoff 1s, 4s ±20% jitter), CompleteText worker 2+2 / realtime 1+1, Embedding 3 (500ms, 2s), Pronunciation 2 (2s), `Evaluation().Primary()/Fallback()` 1 attempt/panggilan; `Retry-After` dihormati ≤ 30s (§8.6.2).
- Ring buffer PCM STT 15 s = 480.000 byte; replay dalam potongan 100 ms = 3.200 byte (§8.5.8).
- `EstimateTokens(s) = ceil(1.1 × rune ≥ U+3000 + rune lain / 3.5)`; `ItemOverheadTokens = 4`; `DefaultBudget` total input 6.950 (§7.8.1). Parser: `MaxRunes 60`, `SoftMin 20`, terminator `。！？!?\n`, closer `」』）)"'”’】` (§7.7.3).
- Template prompt: `text/template`, `missingkey=error`, hanya `{{.FieldName}}`; hasil render CONVERSATION fixture maksimum ≤ 12.000 karakter; `users.full_name`/email MUST NOT ada di struct data (S-4).
- Tidak ada teks user/AI/prompt/audio di log, label metrik, maupun pesan `Error.Message` yang dibuat router (§8.4.0, §13.7.1).
- Sub-paket adapter yang dibangun M9 memakai `providertest` ini tanpa perubahan signature (kontrak M9).

## Review Focus

(Dari master Review Focus #5 bagian provider + kondisi tersirat; tiap butir punya test di task pemilik.)

1. **LLM diblokir content filter / provider mengembalikan hasil tak layak (master #5, sisi provider):** `CONTENT_FILTERED` (`DONE` maupun `ERROR`) tidak memicu fallback, tidak dihitung breaker, tidak membuat `RESET` — `TestGenerationContentFilteredNoFallback` (Task 16); taksonomi `FallbackEligible`/`CountsForBreaker` (Task 1, 4).
2. **Fallback hanya sebelum audio terkirim (D-042):** setelah `MarkAudioSent` tidak ada `RESET`, tidak ada pergantian `TTSEpoch`, error → chunk/`ERROR` apa adanya; vendor TTS dipin per generation — `TestGenerationNoFallbackAfterAudioSent` (Task 16), `TestTTSNoEpochSwitchAfterAudioSent`, `TestTTSVendorPinnedPerGeneration` (Task 17).
3. **`DONE{TRUNCATED}` bukan error (S-7):** deadline LLM total/sesi **sesudah** `MarkAudioSent` → `DONE{TRUNCATED}` + usage, deadline sesi tidak dihitung breaker; **sebelum** audio → fallback/`ERROR` — `TestGenerationTruncatedAfterAudioSent`, `TestGenerationDeadlineBeforeAudioFallsBack` (Task 16).
4. **Breaker tidak "menempel" salah:** semua 9 transisi tabel §8.5.4, kegagalan `CANCELLED`/`CONTENT_FILTERED`/`local_concurrency` diabaikan, probe half-open dilepas bila cancel — `TestBreakerStateMachine` (Task 4).
5. **STT tidak kehilangan/menggandakan audio saat switch & sticky lintas resume:** offset logis, `ReplayFromMs`/`ReplayTruncated`, FINAL lama tidak diteruskan setelah switch, `StartWith=FALLBACK` — `TestResilientSTTSwitchReplay*`, `TestOpenSTTStartWithFallback` (Task 18); fault injection tidak "menempel" saat Redis hilang (> 10 s) — `TestPollerClearsAfterStaleWindow` (Task 9).

## File Structure

```text
internal/provider/
  provider.go errors.go stt.go llm.go tts.go evaluation.go embedding.go pronunciation.go   # tipe & port (T1)
  text.go pcm.go            # NormalizeJA; WAV/resample/RMS/pcmRing (T2)
  config.go                 # RouterConfig + Validate (T3)
  breaker.go                # Breaker/Ticket (T4)
  semaphore.go health.go    # semaphoreSlot; HealthTracker (T5)
  metrics.go                # Metrics + ObserveLLMFirstSentence (T6)
  tts_params.go             # ExpressionCatalog + ResolveTTSParams (T7)
  pricer.go                 # PriceTable port (T8)
  router.go                 # RealtimeRouter/AsyncRouter interface + Router/RouterDeps/NewRouter (T15)
  generation.go             # Generation + fallback D-042 (T16–17)
  resilient_stt.go          # ResilientSTTSession (T18)
  async.go                  # doAsync, GenerateJSON, CompleteText, Embed, AssessPronunciation, EvaluationRoute (T19)
  importrules_test.go       # guard §8.1 (T1)
  pricing/                  # Table + prices.yaml (T8)
  fake/                     # config.go faults.go llm.go tts.go stt.go eval.go embedding.go pronunciation.go fixtures/ (T9–13)
  providertest/             # suite per port (T10–13)
  httpx/                    # NewClient, ClassifyHTTP, SSEReader (T20)
internal/redisstore/fakefaults.go                  # FakeFaults (T14)
internal/prompt/
  tokens.go budget.go learner_context.go data.go text.go   # T21
  render.go funcs.go fixtures.go                            # T22
  core.go phrases.go templates_v1.go                        # T23
  sentence_parser.go                                        # T24–25
```

Konvensi test: test yang butuh `fake` memakai paket eksternal `provider_test` (menghindari siklus impor); test unit internal (breaker, semaphore, health, pcm) memakai `package provider`. Timer ctx (`context.WithTimeout`) dengan durasi ≤ 50 ms boleh dipakai di test timeout; backoff & jitter diinjeksi (`RouterDeps.Sleep`, `RouterDeps.Jitter`) sehingga tidak ada `time.Sleep` di test.

---

### Task 1: Tipe umum, port, error taxonomy, guard aturan impor

**Files:**
- Create: `internal/provider/provider.go`, `errors.go`, `stt.go`, `llm.go`, `tts.go`, `evaluation.go`, `embedding.go`, `pronunciation.go`
- Test: `internal/provider/errors_test.go`, `internal/provider/importrules_test.go`

**Interfaces:**
- Produces: seluruh tipe & interface §8.2.1–8.2.7 persis (`Name`, `Capability`, `ProfileName`, `Message`, `TokenUsage`, `STTSessionConfig`+`STTRole`, `STTProvider`, `STTSession`, `STTEvent`+tipe event, `STTWord`, `STTStats`, `ErrSessionClosed`, `LLMRequest/Provider/Stream/Event`, `FinishReason` (+`FinishTruncated`), `TTSRequest`, `VoiceSelection`, `VoiceBinding`, `ResolvedTTSParams`, `TTSProvider/Stream/Chunk/Usage`, `StructuredRequest/Result`, `EvaluationProvider`, `EmbeddingRequest/Result/Provider`, `Pronunciation*`, `ErrPronunciationDisabled`), konstanta `STTSampleRate=16000`, `TTSSampleRate=24000`, `PronunciationSampleRate=16000`; `ErrorKind` (8 nilai §8.3), `*Error`, `KindOf`, `IsRetryable`, `FallbackEligible`, `CountsForBreaker`, `ClassifyHTTPStatus(status int) ErrorKind`, `var ErrAllProvidersUnavailable` (nama dari plan: `*Error{Kind: UNAVAILABLE, VendorCode: "all_circuits_open"}` dibuat lewat `newAllCircuitsOpen(c Capability) *Error`).

- [ ] **Step 1: Write the failing test**
```go
func TestErrorTaxonomyTable(t *testing.T) {
	cases := []struct{ kind provider.ErrorKind; retry, fb, br bool }{
		{provider.KindTimeout, true, true, true}, {provider.KindRateLimited, true, true, true},
		{provider.KindAuth, false, true, true}, {provider.KindInvalidRequest, false, true, false},
		{provider.KindUnavailable, true, true, true}, {provider.KindContentFiltered, false, false, false},
		{provider.KindInvalidResponse, true, true, true}, {provider.KindCancelled, false, false, false}}
	for _, c := range cases { e := &provider.Error{Kind: c.kind, Retryable: c.retry}
		require.Equal(t, c.fb, provider.FallbackEligible(e)); require.Equal(t, c.br, provider.CountsForBreaker(e)) }
}
```
Tambah: `TestInvalidRequest404CountsForBreaker` (`HTTPStatus:404` → true); `TestRateLimitedLocalConcurrencyNotCounted` (`VendorCode:"local_concurrency"` → false); `TestRateLimitedQuotaNotRetryable` (`Retryable=false` → `IsRetryable` false); `TestKindOf` (`context.Canceled`→CANCELLED, `context.DeadlineExceeded`→TIMEOUT, `errors.New`→UNAVAILABLE, wrapped `*Error` via `fmt.Errorf("%w")`); `TestClassifyHTTPStatus` (408,504→TIMEOUT; 429→RATE_LIMITED; 401,403→AUTH; 400,404,409,413,422→INVALID_REQUEST; 500,502,503,529→UNAVAILABLE); `TestErrorString` (`"provider OPENAI/LLM: RATE_LIMITED (429 rate_limit_exceeded): msg"`); `TestErrorMessageTruncated512` (Message dipotong 512 byte oleh konstruktor `NewError`, nama dari plan).
`TestImportRules` (`go/parser` ImportsOnly atas file non-`_test.go`): paket `internal/provider` hanya boleh stdlib (kecuali `net/http*`), `github.com/google/uuid`, `golang.org/x/sync/semaphore`, `github.com/prometheus/client_golang/...`, `internal/platform/clock`; sub-paket `fake` dilarang mengimpor `go-redis`/`httpx`; `internal/prompt` dilarang mengimpor `internal/{auth,balance,session,content,conversation,provider,evaluation,memory,learner,summary,admin,events}`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'TestErrorTaxonomyTable|TestImportRules'`. Expected: FAIL (build error, tipe belum ada).
- [ ] **Step 3: Implement** tipe/port persis seperti spec §8.2 (salin signature, bukan penjelasan) dan §8.3. `KindOf`: `errors.As(*Error)` → Kind; `context.Canceled` → CANCELLED; `context.DeadlineExceeded` → TIMEOUT; lainnya UNAVAILABLE. `FallbackEligible` = tabel kolom 3; `CountsForBreaker` = kolom 4 dengan dua pengecualian di atas; `IsRetryable` = `Error.Retryable` (nil/ non-`*Error` → false kecuali TIMEOUT/UNAVAILABLE).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race` → PASS; `go vet ./internal/provider/`.
- [ ] **Step 5: Commit** — `git add internal/provider && git commit -m "feat(provider): port, tipe ter-normalisasi, error taxonomy"`

---

### Task 2: `NormalizeJA` dan util PCM murni Go

**Files:**
- Create: `internal/provider/text.go`, `internal/provider/pcm.go`
- Test: `internal/provider/text_test.go`, `internal/provider/pcm_test.go`

**Interfaces:**
- Produces: `NormalizeJA(s string) string` (NFC hanya untuk dakuten/handakuten, tanpa library `x/text` karena tidak ada di D-003); `EncodeWAV(pcm []byte, sampleRate int) []byte`; `DecodeWAV(b []byte) (pcm []byte, sampleRate int, err error)`; `WAVDurationMs(b []byte) (int64, error)`; `ResamplePCM16(pcm []byte, from, to int) []byte`; `RMSdBFS(pcm []byte) float64` (PCM16 mono; senyap = `-120`); `pcmRing` tak-ekspor: `newPCMRing(capBytes, sampleRate int)`, `Write(pcm []byte)`, `OldestMs() int64`, `EndMs() int64`, `ReadFrom(ms int64) []byte` (nama dari plan; semua `ms` = posisi logis absolut sejak sampel pertama stream).

- [ ] **Step 1: Write the failing test**
```go
func TestNormalizeJA(t *testing.T) {
	require.Equal(t, "日本語の勉強", provider.NormalizeJA("日本語 の　勉強"))
	require.Equal(t, "Hello 日本", provider.NormalizeJA(" Hello 日本 "))   // ASCII dibiarkan, trim
	require.Equal(t, "が", provider.NormalizeJA("が")) // NFC: か+゛ → が
}
func TestPCMRingEvictsOldest(t *testing.T) { // cap 480000 byte @16k = 15 s
	r := newPCMRing(480000, 16000); r.Write(make([]byte, 640000))   // 20 s
	require.Equal(t, int64(5000), r.OldestMs()); require.Equal(t, int64(20000), r.EndMs())
	require.Len(t, r.ReadFrom(19900), 3200) }
```
Tambah: `TestWAVRoundTrip` (header RIFF 44 byte, `WAVDurationMs` untuk 16000 sampel @16k = 1000), `TestDecodeWAVRejectsNonPCM16`, `TestResample24kTo16k` (panjang 24000 byte→16000 byte ±2, tinggi sinus 440 Hz dipertahankan RMS ±1 dB), `TestRMSdBFS` (amplitudo penuh sinus ≈ −3,01 dBFS; nol → −120).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'NormalizeJA|PCMRing|WAV|Resample|RMS'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** `NormalizeJA`: komposisi NFC hanya untuk pasangan dakuten/handakuten Hiragana & Katakana (U+3099/U+309A) tanpa library tambahan; hapus U+0020/U+3000 yang diapit dua rune non-ASCII; `strings.TrimSpace`. Ring: slice circular, `OldestMs = (totalBytes − len(buf))/2/rate*1000`. Resample: interpolasi linear. WAV: RIFF PCM16 mono, tolak format ≠ 1 atau bits ≠ 16.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): NormalizeJA dan util PCM (WAV, resample, RMS, ring)"`

---

### Task 3: `RouterConfig` dan `Validate()`

**Files:**
- Create: `internal/provider/config.go`
- Test: `internal/provider/config_test.go`, `internal/provider/config_env_test.go`

**Interfaces:**
- Produces: `type RouterConfig struct` dengan tag `env:"…" envDefault:"…"` persis tabel §8.5.1/§13.2.8 (field Go = nama env tanpa awalan `PROVIDER_`, PascalCase, mis. `Mode`, `ModeSTT`, `STTPrimary`, `STTFallback`, `LLMPrimary`, …, `FallbackEnabled`, `PremiumLLMModel`, `PremiumLLMFallbackModel`, `PremiumLLMMaxTokens`, `PremiumLLMTemperature`, `PremiumTTSModel`, `PremiumTTSFallbackModel`, `Standard*` kembarannya, `STTElevenLabsModel`, `STTDeepgramModel`, `EvaluationOpenAIModel`, `EvaluationGeminiModel`, `EmbeddingOpenAIModel`, `EmbeddingDimensions`, `Timeout*` (`time.Duration`), `SessionGrace`, `STTReplaySpeed`, `CBFailureThreshold`, `CBWindow`, `CBOpenDuration`, `CBHalfOpenProbes`, `PricesFile`, kunci `ElevenLabsAPIKey…AzureSpeechKey`, `AzureSpeechEndpoint`) plus `Concurrency ConcurrencyConfig` berisi field eksplisit `PROVIDER_CONCURRENCY_<VENDOR>_<CAP>` untuk 11 kombinasi nyata (ELEVENLABS_STT, DEEPGRAM_STT, OPENAI_LLM, GEMINI_LLM, ELEVENLABS_TTS, DEEPGRAM_TTS, OPENAI_TTS, OPENAI_EVALUATION, GEMINI_EVALUATION, OPENAI_EMBEDDING, AZURE_PRONUNCIATION) dengan default per kapabilitas (125/60/80/8/16/8); `func (c ConcurrencyConfig) Limit(n Name, cap Capability) int` (instance FAKE_* memakai default kapabilitas); `func (c RouterConfig) Validate() error` (pesan menyebut nama env); `func (c RouterConfig) ModeFor(cap Capability) string` → `"real"|"fake"` (override `PROVIDER_MODE_<CAP>` menang atas `PROVIDER_MODE`); `func (c RouterConfig) Selection(cap Capability) Selection` (mode fake → `FakePrimary`/`FakeFallback`; `FallbackEnabled=false` → Fallback `""`); `type Selection struct{ Primary, Fallback Name }`.

- [ ] **Step 1: Write the failing test** — table-driven `TestRouterConfigValidate`, satu kasus per aturan §8.5.1: primary==fallback → error menyebut `PROVIDER_LLM_PRIMARY`; nilai lowercase `openai` → error; `FAKE_PRIMARY` diisi manual di env → error; mode real tanpa `OPENAI_API_KEY` → error `OPENAI_API_KEY`; mode `fake` tanpa key → OK; `PRONUNCIATION_PRIMARY=NONE` tanpa `AZURE_SPEECH_*` → OK; `EmbeddingDimensions=1024` → error; model profile kosong (real) → error; timeout ≤ 0 → error; limit konkurensi 0 → error.
```go
func TestSelectionFakeModeIgnoresVendorEnv(t *testing.T) {
	c := validRealConfig(); c.ModeLLM = "fake"
	require.Equal(t, provider.Selection{Primary: provider.FakePrimary, Fallback: provider.FakeFallback}, c.Selection(provider.CapLLM))
	require.Equal(t, provider.Selection{Primary: provider.ElevenLabs, Fallback: provider.Deepgram}, c.Selection(provider.CapSTT))
}
```
`config_env_test.go`: `env.Parse(&cfg)` tanpa env set → default persis (`CBFailureThreshold==5`, `CBWindow==30*time.Second`, `TimeoutSTTFinal==1500*time.Millisecond`, `PremiumLLMModel=="gpt-4.1"`, `Concurrency.Limit(ElevenLabs,CapSTT)==125`, `…(OpenAI,CapLLM)==60`), env `PROVIDER_CB_WINDOW=10s` terbaca.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'RouterConfig|Selection'`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `config.go` (tanpa mengimpor `caarlos0/env`; hanya tag). Vendor valid per kapabilitas: STT {ELEVENLABS,DEEPGRAM}, LLM/EVALUATION {OPENAI,GEMINI}, TTS {ELEVENLABS,DEEPGRAM,OPENAI}, EMBEDDING {OPENAI}, PRONUNCIATION {AZURE,NONE}.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): RouterConfig, validasi startup, seleksi mode"`

---

### Task 4: Circuit breaker

**Files:**
- Create: `internal/provider/breaker.go`
- Test: `internal/provider/breaker_test.go` (`package provider`)

**Interfaces:**
- Consumes: `clock.Clock`, `clock.NewFake`, `CountsForBreaker` (T1), `RouterConfig.CB*` (T3).
- Produces: `type BreakerState int` (`BreakerClosed=0, BreakerOpen=1, BreakerHalfOpen=2`), `type Breaker`, `func newBreaker(cfg BreakerConfig, clk clock.Clock, onTransition func(from, to BreakerState)) *Breaker`, `type BreakerConfig struct{ Threshold int; Window, OpenDuration time.Duration; HalfOpenProbes int }`, `func (b *Breaker) Allow() (Ticket, bool)`, `func (t Ticket) Done(err error)`, `func (b *Breaker) State() BreakerState`.

- [ ] **Step 1: Write the failing test** — `TestBreakerStateMachine`, satu subtest per baris tabel §8.5.4 dengan `clk := clock.NewFake(t0)`:
```go
b := newBreaker(BreakerConfig{5, 30 * time.Second, 20 * time.Second, 1}, clk, nil)
for i := 0; i < 4; i++ { tk, _ := b.Allow(); tk.Done(&Error{Kind: KindUnavailable}) }
require.Equal(t, BreakerClosed, b.State())
tk, _ := b.Allow(); tk.Done(&Error{Kind: KindUnavailable}); require.Equal(t, BreakerOpen, b.State())
_, ok := b.Allow(); require.False(t, ok)
clk.Advance(20 * time.Second); tk, ok = b.Allow(); require.True(t, ok); require.Equal(t, BreakerHalfOpen, b.State())
_, ok = b.Allow(); require.False(t, ok)          // probe inflight
tk.Done(nil); require.Equal(t, BreakerClosed, b.State())
```
Subtest lain: 4 kegagalan lalu `Advance(31s)` lalu 1 kegagalan → tetap CLOSED (window bergeser); kegagalan `CANCELLED`/`CONTENT_FILTERED`/`RATE_LIMITED{local_concurrency}` diabaikan (setelah 10 kali tetap CLOSED); probe gagal terhitung → OPEN dengan `openedAt` baru (`Allow` tolak 19 s kemudian, izinkan 20 s); probe selesai `context.Canceled` → state tetap HALF_OPEN dan slot probe bebas (Allow berikutnya diizinkan); sukses saat CLOSED tidak menghapus kegagalan lama dalam window; `onTransition` dipanggil tepat sekali per transisi; uji `-race` dengan 50 goroutine memanggil `Allow/Done`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run Breaker`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `breaker.go` (mutex; `failures []time.Time` dibuang bila `< now−Window`; `Ticket` menyimpan pointer breaker + apakah probe). `Done(err)`: `err==nil` sukses; `!CountsForBreaker(err)` → lepas slot probe tanpa mengubah state (kecuali bukan probe: abaikan).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race -run Breaker -count=20` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): circuit breaker per (vendor, kapabilitas)"`

---

### Task 5: Semaphore konkurensi dan `HealthTracker`

**Files:**
- Create: `internal/provider/semaphore.go`, `internal/provider/health.go`
- Test: `internal/provider/semaphore_test.go`, `internal/provider/health_test.go`

**Interfaces:**
- Produces: `type semaphoreSlot struct`; `newSemaphoreSlot(limit int, wait time.Duration) *semaphoreSlot`; `func (s *semaphoreSlot) Acquire(ctx context.Context, p Name, c Capability) (release func(), err *Error)` (penuh setelah `wait` → `*Error{Kind: RATE_LIMITED, VendorCode: "local_concurrency", Retryable: true}`; `wait=0` = try); `Limit() int`, `InUse() int`. `type HealthTracker`; `NewHealthTracker(clk clock.Clock) *HealthTracker`; `Record(key, outcome)` (nama dari plan: `Record(p Name, c Capability, err error, latency time.Duration)`), `IncActive/DecActive`, `Fallback(from Name, c Capability)`, `Snapshot(limits func(Name, Capability) int, circuits func(Name, Capability) BreakerState) []ProviderHealth` (JSON tag persis contoh §8.5.5: `provider,capability,circuit,active,concurrency_limit,window_60s{requests,successes,failures{kind},p95_latency_ms},last_error{kind,at}`).

- [ ] **Step 1: Write the failing test**
```go
func TestSemaphoreFullReturnsLocalConcurrency(t *testing.T) {
	s := newSemaphoreSlot(1, 0)
	rel, err := s.Acquire(context.Background(), OpenAI, CapLLM); require.Nil(t, err)
	_, err = s.Acquire(context.Background(), OpenAI, CapLLM)
	require.Equal(t, "local_concurrency", err.VendorCode); require.Equal(t, KindRateLimited, err.Kind)
	rel(); _, err = s.Acquire(context.Background(), OpenAI, CapLLM); require.Nil(t, err)
}
```
Tambah: wait 100 ms dengan ctx dibatalkan → `CANCELLED`; `TestHealthWindow60s` (fake clock: request di t0 hilang dari window setelah `Advance(61s)`; bucket 1 s; `failures[UNAVAILABLE]==1`; `last_error.at` terisi); `TestHealthSnapshotJSONShape` (marshal cocok dengan contoh §8.5.5).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'Semaphore|Health'`. Expected: FAIL.
- [ ] **Step 3: Implement** memakai `golang.org/x/sync/semaphore.Weighted` (`TryAcquire` untuk wait 0, selain itu `Acquire` dengan `context.WithTimeout(wait)`); health = ring 60 bucket per key dengan mutex.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): semaphore konkurensi dan health tracker"`

---

### Task 6: Metrik Prometheus provider

**Files:**
- Create: `internal/provider/metrics.go`
- Test: `internal/provider/metrics_test.go`

**Interfaces:**
- Produces: `type Metrics struct`; `NewMetrics(reg prometheus.Registerer) *Metrics` (daftarkan SEMUA metrik §8.8.1 + §8.7.4 dengan nama & label persis; bucket latency `0.05,0.1,0.2,0.3,0.4,0.5,0.75,1,1.5,2,3,5,10,20,60`; `kotoki_session_cost_usd` bucket `0.005…2` (nama dari plan: `0.005,0.01,0.025,0.05,0.1,0.25,0.5,1,2`)); helper `ObserveCall(p Name, c Capability, model, task, outcome string, kind ErrorKind, vendorCode string)`, `ObserveLatency(p, c, model, phase string, d time.Duration)`, `ObserveFallback(c Capability, from, to Name, reason string)`, `SetCircuit(p, c, BreakerState)`, `Usage(p, c, model, unit string, n float64)`, `CostUSD(p, c, model string, usd float64)`, `ConcurrencyWait/Rejected`, `StreamDisconnect`, `STTReplay(from, to Name, seconds float64)`, `STTRotation(p)`, `TTSExpressionDegraded(p Name, from, to string)`, `TTSEpochRestart(from, to Name)`; fungsi paket `ObserveLLMFirstSentence(m *Metrics, p Name, model string, d time.Duration)` (nama dari plan: method `(*Metrics).ObserveLLMFirstSentence`, spesifikasi §8.8.1 menyebut helper `provider.ObserveLLMFirstSentence(provider, model, d)` → sediakan juga fungsi paket yang memakai `Metrics` default via `SetDefaultMetrics`). `vendor_code` di luar daftar yang dikenal → `other` (`knownVendorCodes` = `insufficient_quota, rate_limit_exceeded, local_concurrency, circuit_open, all_circuits_open, voice_binding_missing, tts_provider_switched, session_time_limit_exceeded, insufficient_audio_activity`).

- [ ] **Step 1: Write the failing test** — `TestMetricsRegistered`: `reg := prometheus.NewRegistry(); m := NewMetrics(reg)`, panggil satu helper per metrik, `testutil.GatherAndCompare`/`reg.Gather()` memuat nama: `kotoki_provider_requests_total, kotoki_provider_errors_total, kotoki_provider_latency_seconds, kotoki_provider_active, kotoki_provider_concurrency_wait_seconds, kotoki_provider_concurrency_rejected_total, kotoki_provider_fallback_total, kotoki_provider_circuit_state, kotoki_provider_circuit_transitions_total, kotoki_provider_stream_disconnects_total, kotoki_provider_ratelimit_remaining, kotoki_stt_replay_seconds, kotoki_stt_rotations_total, kotoki_tts_expression_degraded_total, kotoki_tts_epoch_restarts_total, kotoki_provider_usage_total, kotoki_provider_cost_usd_total, kotoki_session_cost_usd, kotoki_session_cost_per_minute_usd`. Tambah `TestMetricsNoHighCardinalityLabels` (tidak ada label `session_id`/`user_id` di semua descriptor), `TestUnknownVendorCodeBecomesOther`, `TestFallbackReasonLabelValues` (`CIRCUIT_OPEN`, `CALLER_DIRECTED`, ErrorKind).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run Metrics`. Expected: FAIL.
- [ ] **Step 3: Implement** `metrics.go` (registry diinjeksi; tidak memakai `prometheus.DefaultRegisterer`). `NewMetrics(nil)` → registry privat (untuk test lain).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): metrik Prometheus provider dan biaya"`

---

### Task 7: `ExpressionCatalog` dan `ResolveTTSParams`

**Files:**
- Create: `internal/provider/tts_params.go`
- Test: `internal/provider/tts_params_test.go`

**Interfaces:**
- Produces: `type ExpressionCatalog interface{ Lookup(expr string) (degradeTo string, nonVerbal bool, ok bool) }`; `func ResolveTTSParams(cat ExpressionCatalog, expr string, vendor Name, b VoiceBinding, speakingRate float64) ResolvedTTSParams` (Bab 6 §6.6.2 langkah 1–4; `ProviderVoiceID = b.ProviderVoiceID`); `func vendorSpeedRange(v Name) (min, max float64)` (ELEVENLABS 0.70–1.20, OPENAI 0.70–1.30, DEEPGRAM 0.70–1.50).

- [ ] **Step 1: Write the failing test** — katalog tes `stubCatalog` sesuai §6.3.7 (laugh→amused→happy→neutral, `laugh`/`chuckle`/`sigh` nonVerbal). Kasus table-driven:
```go
b := VoiceBinding{ProviderVoiceID: "v1", Capabilities: []string{"SPEED", "STYLE_PARAMS"},
	Settings: map[string]any{"stability": 0.5}, ExpressionMappings: map[string]map[string]any{
		"amused": {"stability": 0.35, "style": 0.55}, "laugh": {"audio_tag": "laughs"},
		"thinking": {"speed_multiplier": 0.92}}}
r := ResolveTTSParams(cat, "laugh", ElevenLabs, b, 0.90)
require.Equal(t, "amused", r.EffectiveExpression)         // tanpa AUDIO_TAGS: laugh→amused
require.Equal(t, 0.35, r.Params["stability"]); require.InDelta(t, 0.90, r.Params["speed"], 1e-9)
r = ResolveTTSParams(cat, "thinking", ElevenLabs, b, 0.90)
require.InDelta(t, 0.828, r.Params["speed"], 1e-9)         // 0.90×0.92 (contoh §6.6.3 ≈ 0.83)
```
Kasus lain: dengan `AUDIO_TAGS` → `audio_tag:"laughs"` dan `EffectiveExpression:"laugh"`; ekspresi tanpa mapping → `neutral` dan params = salinan `Settings`; `neutral` → mapping `{}`; clamp: rate 1.05 × multiplier 1.20 di ELEVENLABS → 1.20, di DEEPGRAM 1.26 (≤1.50) tak terpotong, rate 0.80 × 0.80 → 0.70 (min); tanpa kapabilitas `SPEED` → key `speed` tidak ada; `INSTRUCTIONS`: `instructions` = `join(settings.instructions, m.instructions, " ")` (kedua ada / salah satu / kosong); hasil tidak memutasi `b.Settings` (salinan); `expr==""` diperlakukan `neutral`; `nil` catalog tidak boleh panic (semua ekspresi dianggap tak dikenal → neutral).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run ResolveTTSParams`. Expected: FAIL.
- [ ] **Step 3: Implement** mengikuti pseudocode §6.6.2 (loop degradasi: `ok && (!nonVerbal || AUDIO_TAGS∈caps)` berhenti; selain itu `e = degradeTo`; berhenti di `neutral`). Fungsi murni tanpa I/O.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race -run ResolveTTSParams` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): ExpressionCatalog dan ResolveTTSParams"`

---

### Task 8: `PriceTable` port dan `pricing.Table`

**Files:**
- Create: `internal/provider/pricer.go`, `internal/provider/pricing/pricing.go`, `internal/provider/pricing/prices.yaml`
- Test: `internal/provider/pricing/pricing_test.go`

**Interfaces:**
- Produces: `provider.PriceTable` (3 method persis §8.7.2); `pricing.Table`, `pricing.Load(path string) (*Table, error)` (path `""` = tabel ter-embed), `(*Table).LLM(p, cap, model, u) (float64, bool)`, `TTS(p, model, characters)`, `Audio(p, cap, model, audioMs)`, `(*Table).Missing(configured []provider.ModelRef) []provider.ModelRef` (nama dari plan; `provider.ModelRef{Provider Name; Capability Capability; Model string}` ditambahkan di `pricer.go`) untuk WARN `provider price missing`.

- [ ] **Step 1: Write the failing test**
```go
func TestWildcardAndZeroPrice(t *testing.T) {
	tb, err := pricing.Load("")                          // embed; semua harga 0
	require.NoError(t, err)
	usd, ok := tb.TTS(provider.Deepgram, "aura-2-minerva-ja", 100)   // entri model:"*"
	require.True(t, ok); require.Zero(t, usd)
	_, ok = tb.LLM(provider.OpenAI, provider.CapLLM, "model-tak-ada", provider.TokenUsage{})
	require.False(t, ok)
}
```
Tambah (file YAML sementara via `t.TempDir()`): hitungan `LLM` = `input/1e6×in + output/1e6×out`; `TTS` = `chars/1e6×per_1m_characters`; `Audio` = `audioMs/3.6e6×per_audio_hour`; loader menolak `provider: openai` (lowercase) dan `capability: llm`; entri sama dua kali → error; `Missing` mengembalikan kombinasi tanpa entri atau berharga 0. `var _ provider.PriceTable = (*pricing.Table)(nil)`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/pricing/ -race`. Expected: FAIL.
- [ ] **Step 3: Implement.** `prices.yaml` = isi §8.7.2 (10 entri, harga 0, `version: "2026-10-12"`). Loader memakai `gopkg.in/yaml.v3` (`yaml.Unmarshal` ke struct dengan `KnownFields` ketat via `yaml.NewDecoder(...).KnownFields(true)`; field tak dikenal → error). Validasi UPPERCASE `provider`/`capability`, duplikat (provider, capability, model) → error.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race -run 'Pricing|Wildcard|Price'` → PASS; `go build ./internal/provider/...`.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): PriceTable port dan pricing.Table"`

---

### Task 9: Fake — konfigurasi umum, keputusan gagal, fault injection, poller

**Files:**
- Create: `internal/provider/fake/config.go`, `internal/provider/fake/faults.go`
- Test: `internal/provider/fake/faults_test.go`

**Interfaces:**
- Consumes: `provider.Name/Capability/ErrorKind/Error/ClassifyHTTPStatus`, `clock.Clock`.
- Produces:
  - `type Config struct` (tag env `FAKE_SEED` 42, `FAKE_STT_MODE`, `FAKE_STT_FIXTURE_DIR`, `FAKE_STT_MATCH_THRESHOLD` 0.8, `FAKE_STT_ENERGY_DBFS` -40, `FAKE_STT_PHRASES` (dipisah `|`), `FAKE_LLM_TOKEN_INTERVAL_MS` 30, `FAKE_LLM_TEMPLATES_FILE`, `FAKE_TTS_MS_PER_CHAR` 120, `FAKE_TTS_CHUNK_INTERVAL_MS` 5, `FAKE_TTS_WAVEFORM` tone, `FAKE_EVAL_FIXTURE_DIR`, `FAKE_FAULTS_REDIS_KEY` `kotoki:fake:faults`, `FAKE_FAULTS_POLL_INTERVAL` 1s) dengan default §13.2.9;
  - `type CapConfig struct{ LatencyMS, JitterMS int; FailureRate float64; FailEveryN int; FailureKind provider.ErrorKind; FailurePhase string; FailAfter int; ConnectMS int }`; `func LoadCapConfig(lookup func(string) (string, bool), inst provider.Name, cap provider.Capability) (CapConfig, error)` (baca `FAKE_<PRIMARY|FALLBACK>_<CAP>_*`; default latency per kapabilitas: STT 150, LLM 300, TTS 150, EVALUATION 500, EMBEDDING 20, PRONUNCIATION 300; STT connect 50; `FailureKind` default `UNAVAILABLE`, phase `start`, `FailAfter` 3);
  - `type Deps struct{ Clock clock.Clock; Faults FaultSource; Log *slog.Logger; Seed int64 }`;
  - `type decider` (tak-ekspor) `newDecider(seed int64, inst provider.Name, cap provider.Capability, c CapConfig) *decider` dengan `next(f *Fault) (fail bool, plan failPlan)` — `FailEveryN>0` → `counter%N==0`; selain itu `hash64 / 2^64 < rate` dengan `hash64 = FNV-64a("<seed>|<inst>|<cap>|<counter>")` (nama dari plan; spec tidak menetapkan fungsi hash), counter atomic;
  - `type Fault struct{ Mode string; Rate float64; AfterMs int64; Status int; Kind provider.ErrorKind; DelayMs int }`, `type FaultSet struct{ ... }` dengan `Lookup(cap provider.Capability, inst provider.Name) (*Fault, bool)`, `type FaultSource interface{ Current() *FaultSet }`, `var NoFaults FaultSource`, `ParseFaultSet(b []byte, now time.Time, warn func(string)) *FaultSet` (mode valid: `error timeout slow fail_after_first_audio disconnect_mid_stream`; kombinasi mode/kapabilitas di luar tabel §8.9.6 → entri diabaikan + warn; `until` lewat → set kosong; `status` → Kind via `ClassifyHTTPStatus`, default 503; `kind` eksplisit menimpa), `type Poller` (`NewPoller(fetch func(ctx context.Context) ([]byte, bool, error), interval time.Duration, clk clock.Clock, log *slog.Logger) *Poller`, `Run(ctx)`, `Current()`, `PollOnce(ctx)`).

- [ ] **Step 1: Write the failing test**
```go
func TestDeciderEveryNDeterministic(t *testing.T) {
	d := newDecider(42, provider.FakePrimary, provider.CapLLM, CapConfig{FailEveryN: 3})
	var got []bool; for i := 0; i < 6; i++ { f, _ := d.next(nil); got = append(got, f) }
	require.Equal(t, []bool{false, false, true, false, false, true}, got)
}
func TestPollerClearsAfterStaleWindow(t *testing.T) { // review focus 5
	clk := clock.NewFake(t0); fail := false
	p := NewPoller(func(context.Context) ([]byte, bool, error) {
		if fail { return nil, false, errors.New("redis down") }
		return []byte(`{"LLM.FAKE_PRIMARY":{"mode":"error","rate":1}}`), true, nil }, time.Second, clk, nil)
	p.PollOnce(ctx); _, ok := p.Current().Lookup(provider.CapLLM, provider.FakePrimary); require.True(t, ok)
	fail = true; clk.Advance(9 * time.Second); p.PollOnce(ctx); _, ok = p.Current().Lookup(provider.CapLLM, provider.FakePrimary); require.True(t, ok)
	clk.Advance(2 * time.Second); p.PollOnce(ctx); _, ok = p.Current().Lookup(provider.CapLLM, provider.FakePrimary); require.False(t, ok)
}
```
Tambah: rate deterministik (`FailureRate:0.5` seed sama → urutan sama dua kali); `ParseFaultSet` — `until` lampau → kosong; JSON rusak → kosong + 1 warn; `"TTS.FAKE_PRIMARY":{"mode":"disconnect_mid_stream"}` diabaikan + warn; `status:429` → `RATE_LIMITED`; `kind:"AUTH"` menimpa; key tidak ada (`found=false`) → set kosong; `LoadCapConfig` dengan lookup map (`FAKE_PRIMARY_LLM_FAILURE_RATE=0.05` → 0.05; nilai invalid → error menyebut nama env); `Config` terparse `env.Parse` (default §13.2.9).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/fake/ -race -run 'Decider|Poller|FaultSet|LoadCapConfig'`. Expected: FAIL.
- [ ] **Step 3: Implement** `config.go`, `faults.go` (snapshot `atomic.Pointer[FaultSet]`; poller: error → pertahankan snapshot terakhir sampai `10s` sejak sukses terakhir (diukur `clk`), lalu kosongkan; WARN maks 1×/menit per key lewat `clk`).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/fake/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/fake): config, keputusan gagal, fault set, poller"`

---

### Task 10: Fake LLM + `providertest.LLMSuite`

**Files:**
- Create: `internal/provider/fake/llm.go`, `internal/provider/providertest/llm.go`
- Test: `internal/provider/fake/llm_test.go`

**Interfaces:**
- Produces: `fake.NewLLM(name provider.Name, c CapConfig, cfg Config, d Deps) *LLM` (implements `provider.LLMProvider`); `providertest.LLMSuite(t *testing.T, newProvider func(t *testing.T) provider.LLMProvider)`.

- [ ] **Step 1: Write the failing test** — `fake/llm_test.go` memanggil `providertest.LLMSuite` lalu kasus khusus fake:
```go
func TestFakeLLMDeterministicTemplate(t *testing.T) {
	l := newTestLLM(t, CapConfig{}) // latency 0, interval 0
	req := provider.LLMRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: "こんにちは"}}}
	a := collectText(t, l, req); b := collectText(t, l, req)
	require.Equal(t, a, b); require.Contains(t, templates, a)    // idx = fnv32(lastUserMessage) % 5
}
```
`LLMSuite` (generik, juga untuk M9) berisi subtest: `StreamReturnsBeforeFirstToken` (Stream kembali ≤ 50 ms walau latency 300 ms); `EndsWithDoneWithUsage` (event terakhir `DONE`, `Usage != nil`, channel tertutup); `MaxTokensTruncatesToFinishMaxTokens`; `StopSequenceCutsBeforeMatch`; `CancelEmitsCancelledDoneWithin50ms` (cancel ctx → `DONE{CANCELLED}` ≤ 50 ms lalu channel tertutup; Close idempotent); `SyncErrorIsProviderError` (bila vendor dikonfigurasi gagal `start` → `errors.As(*provider.Error)`). Kasus fake: opening (tanpa pesan user) memakai template opening `<e:happy>こんにちは！今日はどんな一日でしたか？`; pesan user berisi `#long` → jawaban diulang 5×; DELTA 3 rune; `InputTokens` = total rune semua pesan, `OutputTokens` = rune output, `Estimated=false`; phase `before_first` → `ERROR` sebelum DELTA; `mid` + `FailAfter=2` → 2 DELTA lalu `ERROR`; `hang` → tidak ada event sampai ctx habis; fault runtime `{"mode":"slow","delay_ms":50}` menambah latency; `FAKE_LLM_TEMPLATES_FILE` menimpa template (via `Config` + `t.TempDir()`); `ReceivedAt` dari `clock`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/fake/ -race -run FakeLLM`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** Lima template §8.9.3 persis (opening + 4 reguler + `<e:concerned>…` = 5 reguler total: ikuti urutan daftar spec; template opening terpisah). Goroutine producer; `select` pada `ctx.Done()` di tiap DELTA; first token setelah `LatencyMS ± jitter` (jitter dari PRNG ber-seed `Deps.Seed`); `Close()` = cancel internal.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/fake/ ./internal/provider/providertest/ -race -run 'FakeLLM|LLMSuite'` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/fake): fake LLM dan providertest.LLMSuite"`

---

### Task 11: Fake TTS + `providertest.TTSSuite`

**Files:**
- Create: `internal/provider/fake/tts.go`, `internal/provider/providertest/tts.go`
- Test: `internal/provider/fake/tts_test.go`

**Interfaces:**
- Produces: `fake.NewTTS(name provider.Name, c CapConfig, cfg Config, d Deps) *TTS` (`provider.TTSProvider`); `providertest.TTSSuite(t, newProvider func(t *testing.T) provider.TTSProvider, newReq func() provider.TTSRequest)`.

- [ ] **Step 1: Write the failing test**
```go
func TestFakeTTSDurationAndExpressionMultiplier(t *testing.T) {
	req := provider.TTSRequest{Text: "こんにちは。", Resolved: provider.ResolvedTTSParams{Params: map[string]any{"speed": 1.0}, EffectiveExpression: "happy"}}
	pcm := drain(t, mustSynth(t, newTestTTS(t), req))
	// rune=6 → max(300, 6×120)=720 ms; speed=1.0×1.05 → 685 ms ±20 ms @24 kHz PCM16
	require.InDelta(t, 685, float64(len(pcm))/48.0, 20)
}
```
`TTSSuite`: `PCMLengthAlwaysEven`; `LastChunkIsFinalWithUsage` (`Final=true`, `Usage.Characters == utf8.RuneCountInString(Text)`, `Usage.AudioMs>0`); `CancelClosesChannelWithin50ms` (tidak ada chunk setelah cancel); `FirstChunkAfterLatency`; `SyncErrorIsProviderError`. Kasus fake: chunk berukuran 960 byte (20 ms) kecuali terakhir; `FAKE_TTS_WAVEFORM=silence` → semua byte nol, `tone` → RMS ≈ −20 dBFS (`provider.RMSdBFS` ±1,5); pengali: `happy 1.05, excited 1.08, sad 0.92, thinking 0.93`, lain 1.0; ekspresi ≠ neutral → frekuensi nada +20 Hz (uji via zero-crossing); `fail_after_first_audio` (fault runtime) → chunk pertama normal lalu `TTSChunk{Err.Kind: UNAVAILABLE}` sebagai chunk terakhir; phase `hang` → tidak ada chunk sampai ctx habis; interval antar-chunk = `FAKE_TTS_CHUNK_INTERVAL_MS`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/fake/ -race -run FakeTTS`. Expected: FAIL.
- [ ] **Step 3: Implement.** Durasi = `max(300 ms, rune×MsPerChar)/speed`, `speed = Params["speed"] (default 1.0) × m(EffectiveExpression)`; sinus 220 Hz (+20 Hz bila ekspresi ≠ neutral/""), amplitudo −20 dBFS; fake tidak membaca `Voice`/`Bindings`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/fake/ ./internal/provider/providertest/ -race -run 'FakeTTS|TTSSuite'` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/fake): fake TTS dan providertest.TTSSuite"`

---

### Task 12: Fake STT (korelasi envelope) + `providertest.STTSuite`

**Files:**
- Create: `internal/provider/fake/stt.go`, `internal/provider/providertest/stt.go`
- Test: `internal/provider/fake/stt_test.go`

**Interfaces:**
- Produces: `fake.NewSTT(name provider.Name, c CapConfig, cfg Config, d Deps) (*STT, error)` (`error` bila `FAKE_STT_FIXTURE_DIR` tak terbaca / WAV bukan PCM16 16 kHz mono); `providertest.STTSuite(t, newProvider func(t *testing.T) provider.STTProvider, speech func() []byte)` (`speech()` = PCM16 16 kHz berenergi ≥ 1 s).

- [ ] **Step 1: Write the failing test** — fixture dibuat sintetis di `t.TempDir()` (tanpa `testdata/audio`): A = 3 burst sinus 400 ms dipisah jeda 200 ms; B = ramp naik 1,2 s; masing-masing + `.txt`.
```go
func TestFakeSTTMatchesFixtureByEnvelope(t *testing.T) {
	dir := writeFixtures(t) // a.wav/a.txt "日本語の勉強は楽しいです。", b.wav/b.txt "昨日は雨でしたね。"
	s := openFake(t, Config{STTFixtureDir: dir, STTMatchThreshold: 0.8})
	feed(t, s, noisy(burstsA(), 0.02))      // amplitudo ±2 % (meniru round-trip Opus)
	require.NoError(t, s.Finalize())
	ev := nextFinal(t, s); require.True(t, ev.FromFinalize)
	require.Equal(t, "日本語の勉強は楽しいです。", ev.Text); require.InDelta(t, 0.95, *ev.Confidence, 1e-9)
}
```
`STTSuite`: `FinalizeYieldsExactlyOneFinalFromFinalize` (3× Finalize → 3 FINAL `FromFinalize`; teks boleh kosong); `PartialsPrecedeFinal`; `CloseEmitsClosedThenClosesChannel` + `Close` idempotent; `SendAfterCloseReturnsErrSessionClosed`; `EventsBufferCap64` (cap channel = 64, PARTIAL tertua dibuang saat penuh, FINAL tidak pernah dibuang); `StatsAudioMsSentCountsAudio` (`AudioMsSent` = ms nyata yang dikirim). Kasus fake: tanpa fixture cocok (skor < 0,8) → mode `energy` memakai `FAKE_STT_PHRASES[segmentIndex % len]` (default 5 frasa §8.9.2); segmen baru dimulai setelah ≥ 200 ms speech (10 frame RMS ≥ −40 dBFS), ditutup setelah 600 ms non-speech atau `Finalize`; PARTIAL tiap 200 ms **waktu audio** (deterministik, bukan wall-clock) = prefix `ceil(len_rune × min(1, elapsed/expected))` (`expected` = 120 ms/rune di mode energy); `Finalize()` tanpa segmen → `FINAL{Text:"", FromFinalize:true}`; FINAL tiba setelah `LatencyMS`; tie korelasi → nama file leksikografis terkecil; `Open` menunggu `ConnectMS`; fault `timeout{after_ms:500}` → berhenti mengirim PARTIAL/FINAL setelah 500 ms audio dan `Finalize` tidak dijawab; `disconnect_mid_stream` → `ERROR{UNAVAILABLE}` lalu `CLOSED`; `StartMs/EndMs` dari posisi sampel; `Text` lewat `provider.NormalizeJA`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/fake/ -race -run FakeSTT`. Expected: FAIL.
- [ ] **Step 3: Implement.** Envelope = RMS per 20 ms dinormalisasi terhadap maksimum; skor = cosine similarity vektor envelope (rata-rata dikurangkan) pada irisan setelah geser −10…+10 frame, ambil maksimum. `SendAudio` non-blocking dengan buffer internal 2 s (`KindUnavailable` bila penuh); `SendAudio`/`Finalize` satu goroutine pemanggil, event dari goroutine internal.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/fake/ ./internal/provider/providertest/ -race -run 'FakeSTT|STTSuite' -count=3` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/fake): fake STT (korelasi envelope) dan providertest.STTSuite"`

---

### Task 13: Fake Evaluation, Embedding, Pronunciation + suite

**Files:**
- Create: `internal/provider/fake/eval.go`, `fake/embedding.go`, `fake/pronunciation.go`, `fake/fixtures/kotoki_fake_example.json`, `providertest/eval.go`, `providertest/embedding.go`, `providertest/pronunciation.go`
- Test: `internal/provider/fake/eval_test.go`, `embedding_test.go`, `pronunciation_test.go`

**Interfaces:**
- Produces: `fake.NewEvaluation(name, c CapConfig, cfg Config, d Deps) *Evaluation` (`//go:embed fixtures/*.json`; `FAKE_EVAL_FIXTURE_DIR` menimpa); `fake.NewEmbedding(...) *Embedding`; `fake.NewPronunciation(...) *Pronunciation`; `providertest.EvaluationSuite(t, newProvider, req provider.StructuredRequest)`, `providertest.EmbeddingSuite(t, newProvider)`, `providertest.PronunciationSuite(t, newProvider)`.

- [ ] **Step 1: Write the failing test**
```go
func TestEmbeddingSimilarTextsHaveHigherCosine(t *testing.T) {
	r := embedAll(t, "日本語の勉強が好きです", "日本語の勉強が楽しいです", "今日の天気は雨です")
	require.Len(t, r.Vectors[0], 1536)
	require.Greater(t, cosine(r.Vectors[0], r.Vectors[1]), cosine(r.Vectors[0], r.Vectors[2]))
	require.InDelta(t, 1.0, norm(r.Vectors[0]), 1e-6)           // L2 = 1
}
func TestPronunciationRanges(t *testing.T) {
	require.Equal(t, provider.PronunciationTooShort, assess(t, wavMs(400)).Status)
	require.Equal(t, provider.PronunciationTooLong, assess(t, wavMs(30001)).Status)
	r := assess(t, wavMs(2000)); require.Equal(t, provider.PronunciationOK, r.Status); require.Nil(t, r.ProsodyScore)
	require.InDelta(t, r.AccuracyScore, r.PronScore, 5.0001)
}
```
Kasus lain — Evaluation: fixture `kotoki_fake_example.json` memuat `"{{score}}"` → diganti `50 + fnv32(RequestID) % 41` (50–90, integer; sama untuk RequestID sama; hasil lolos `json.Valid`); `SchemaName` tanpa fixture → `*Error{Kind: INVALID_REQUEST}`; `Usage`/`FinishReason=STOP`; berlaku juga lewat `GenerateJSON` pada instance FALLBACK. Embedding: teks kosong → `e_0` (`v[0]==1`); entri 1..64 teks, 65 → `INVALID_REQUEST`; `Usage.InputTokens` = total rune, `OutputTokens=0`; fitur: trigram rune (dipad `^`/`$`) `v[fnv32(g)%1536] += sign(fnv32("s"+g))`. Pronunciation: accuracy = `60 + fnv32(ReferenceText + strconv.Itoa(len(audio))) % 36`; fluency/completeness/pron = accuracy ± 5 deterministik (offset −5, +5, 0 — nama dari plan: fluency −5, completeness +5, pron 0); `Words` per 2 rune referensi, `ErrorType=NONE`, kata ke-3 `MISPRONUNCIATION` bila accuracy < 75; WAV bukan PCM16 16 kHz → `INVALID_REQUEST`. Suite: `JSONValidAndUsagePositive`, `UnknownSchemaIsInvalidRequest`, `VectorsDims1536InOrder`, `DurationBoundsAreNotErrors`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/fake/ -race -run 'Embedding|Pronunciation|FakeEval'`. Expected: FAIL.
- [ ] **Step 3: Implement** ketiganya; latency default 500/20/300 ms dengan fault `error|timeout|slow` didukung seperti T9 (helper `applyPlan(ctx, plan)` dibuat di T9/T10 — pakai ulang).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/fake/ ./internal/provider/providertest/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/fake): fake evaluation, embedding, pronunciation dan suite"`

---

### Task 14: `redisstore.FakeFaults` (integration)

**Files:**
- Create: `internal/redisstore/fakefaults.go`
- Test: `internal/redisstore/fakefaults_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: `fake.NewPoller`, `fake.FaultSource`, testcontainers Redis helper M1 (`testutil.NewRedis(t)`; nama dari M1 — bila belum ada, `testcontainers-go/modules/redis` langsung).
- Produces: `func NewFakeFaults(rdb redis.Cmdable, key string, interval time.Duration, clk clock.Clock, log *slog.Logger) *FakeFaults` (`var _ fake.FaultSource = (*FakeFaults)(nil)`), `(*FakeFaults).Run(ctx)`, `Current() *fake.FaultSet`.

- [ ] **Step 1: Write the failing test**
```go
func TestFakeFaultsPollsRedisKey(t *testing.T) {
	rdb := testutil.NewRedis(t)
	ff := redisstore.NewFakeFaults(rdb, "kotoki:fake:faults", 20*time.Millisecond, clock.System{}, nil)
	go ff.Run(ctx)
	require.NoError(t, rdb.Set(ctx, "kotoki:fake:faults", `{"STT.FAKE_PRIMARY":{"mode":"timeout","rate":1}}`, 0).Err())
	require.Eventually(t, func() bool { _, ok := ff.Current().Lookup(provider.CapSTT, provider.FakePrimary); return ok }, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, rdb.Del(ctx, "kotoki:fake:faults").Err())
	require.Eventually(t, func() bool { _, ok := ff.Current().Lookup(provider.CapSTT, provider.FakePrimary); return !ok }, 5*time.Second, 50*time.Millisecond)
}
```
(`clock.System{}` = implementasi nyata clock M1; nama dari M1.) Tambah: `redis.Nil` → set kosong (bukan error); mematikan container → snapshot bertahan ≤ 10 s lalu kosong (`require.Eventually` timeout 15 s).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/redisstore/ -race -run FakeFaults`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `fetch` = `rdb.Get(ctx, key)`; `redis.Nil` → `(nil,false,nil)`; error lain → error; delegasi ke `fake.Poller`.
- [ ] **Step 4: Run tests to verify they pass** — `go test -tags integration ./internal/redisstore/ -race -run FakeFaults` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(redisstore): fault injection fake provider via Redis"`

---

### Task 15: Router inti — `RouterDeps`, `NewRouter`, seleksi, health snapshot

**Files:**
- Create: `internal/provider/router.go`
- Test: `internal/provider/router_test.go` (`package provider_test`), `internal/provider/router_helpers_test.go`

**Interfaces:**
- Consumes: T1–T8, `fake.*` (hanya di test).
- Produces: interface `RealtimeRouter`, `AsyncRouter`, `EvaluationRoute`, `ResilientSTTSession`, `Generation`, `GenerationSummary`, `TextRequest/Result` persis §8.2.8; `type ProcessRole string` (`RoleRealtime`, `RoleWorker`; nama dari plan — dibutuhkan karena `RealtimeRouter.CompleteText` dan `AsyncRouter.CompleteText` bertanda tangan sama tetapi kebijakan retry/tunggu semaphore berbeda, §8.5.6/§8.6.2); `RouterDeps` §8.5.2 + `Role ProcessRole`, `Metrics *Metrics`, `Sleep func(ctx context.Context, d time.Duration) error` (default timer nyata), `Jitter func(d time.Duration) time.Duration` (default ±20 % acak; test: identitas) — ketiganya nama dari plan; `func NewRouter(cfg RouterConfig, d RouterDeps) (*Router, error)` (memanggil `cfg.Validate()`; membuat breaker & semaphore untuk setiap (vendor, kap) di `Deps`); `(*Router).candidates(c Capability) ([]candidate, error)` (§8.5.3; breaker OPEN dilewati; kosong → `*Error{UNAVAILABLE,"all_circuits_open"}`; lewati primary karena breaker → `reason=CIRCUIT_OPEN`); `(*Router).HealthSnapshot() []ProviderHealth`; `(*Router).llmModel/ttsModel`; `*Router` memenuhi `RealtimeRouter` & `AsyncRouter` (asersi `var _`).
- Wiring: M4 TIDAK merakit router untuk binary. M5 T25 (`cmd/internal/providerwire/fake.go`, `BuildFake(cfg, caps, deps)`) WAJIB mengisi semua field `RouterDeps`: `STT`, `LLM`, `TTS`, `Evaluation`, `Embedding`, `Pron` (nil bila `PRONUNCIATION_PRIMARY=NONE`), `Prices` (`PriceTable`), `Expressions` (`ExpressionCatalog`), `Clock`, `Log`, `Role` (`RoleRealtime`/`RoleWorker`), `Metrics`; `Sleep`/`Jitter` dibiarkan nil (default nyata). Instance fake: `FAKE_PRIMARY` dan `FAKE_FALLBACK` per kapabilitas dengan `fake.Deps{Clock, Faults, Log, Seed}`.
- Test helper (`router_helpers_test.go`): `newTestRouter(t, opts ...testOpt) (*provider.Router, *testEnv)` merakit `fake` PRIMARY/FALLBACK untuk semua kapabilitas, `clock.NewFake`, `Sleep` perekam, `Jitter` identitas, registry Prometheus privat, `slog` JSON ke buffer.

- [ ] **Step 1: Write the failing test**
```go
func TestCandidatesSkipsOpenBreakerAndMarksFallback(t *testing.T) {
	r, env := newTestRouter(t, withMode("fake"))
	env.openBreaker(provider.FakePrimary, provider.CapLLM)         // 5 kegagalan via fault error
	c, err := r.CandidatesForTest(provider.CapLLM)                 // export_test.go
	require.NoError(t, err); require.Equal(t, []provider.Name{provider.FakeFallback}, names(c)); require.True(t, c[0].ViaCircuitOpen)
}
func TestAllCircuitsOpenIsUnavailable(t *testing.T) { /* kedua breaker open → Kind UNAVAILABLE, VendorCode "all_circuits_open" */ }
func TestFallbackDisabledMeansPrimaryOnly(t *testing.T) { /* FallbackEnabled=false → 1 kandidat */ }
func TestNewRouterRejectsInvalidConfig(t *testing.T) { /* primary==fallback → error menyebut env */ }
```
`TestHealthSnapshotListsAllKeys` (setiap (vendor,kap) muncul dengan `concurrency_limit` dan `circuit`), `TestModelSelection` (PREMIUM LLM primary → `gpt-4.1`, fallback → `gemini-2.5-flash`; STANDARD TTS model `eleven_flash_v2_5`). `export_test.go` mengekspos `CandidatesForTest`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'Candidates|AllCircuits|FallbackDisabled|NewRouter|HealthSnapshot|ModelSelection'`. Expected: FAIL.
- [ ] **Step 3: Implement** `router.go` sesuai §8.5.2–8.5.3 (struct `Router` dengan field §8.5.2). Transisi breaker memanggil `Metrics.SetCircuit`/`circuit_transitions` dan `slog.Warn("provider circuit state changed")`. Semaphore wait per kapabilitas: STT 0; LLM/TTS 100 ms (Role realtime) atau 30 s (Role worker); EVALUATION/EMBEDDING/PRONUNCIATION 30 s.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): router inti, seleksi kandidat, health snapshot"`

---

### Task 16: `Generation` — LLM, fallback D-042, RESET, TRUNCATED, CONTENT_FILTERED

**Files:**
- Create: `internal/provider/generation.go`
- Test: `internal/provider/generation_llm_test.go` (`package provider_test`)

**Interfaces:**
- Consumes: `Router` (T15), fake LLM (T10).
- Produces: `(*Router).NewGeneration(sessionID uuid.UUID, generation int64, profile ProfileName, voice VoiceSelection) Generation`; `Generation.StreamLLM`, `MarkAudioSent`, `Summary` (LLM bagian: `LLMProvider, LLMModel, UsedFallback, LLMUsage` (jumlah SEMUA attempt, estimasi `Estimated=true` bila attempt gagal tanpa usage — 1,0 token/rune §8.7.3), `LLMFirstTokenMs`).

- [ ] **Step 1: Write the failing test**
```go
func TestGenerationFallsBackBeforeAudioWithReset(t *testing.T) {
	r, env := newTestRouter(t); env.llmFault(provider.FakePrimary, fake.CapConfig{FailurePhase: "mid", FailAfter: 2, FailureRate: 1}) // 2 DELTA lalu ERROR
	g := r.NewGeneration(sid, 1, provider.ProfilePremium, voice)
	s, err := g.StreamLLM(ctx, userReq("こんにちは")); require.NoError(t, err)
	evs := drainLLM(t, s)
	require.Contains(t, types(evs), provider.LLMReset)                     // RESET sebelum DELTA fallback
	require.Equal(t, provider.LLMDone, evs[len(evs)-1].Type)
	require.True(t, g.Summary().UsedFallback); require.Equal(t, provider.FakeFallback, g.Summary().LLMProvider)
	require.Equal(t, "gemini-2.5-flash", g.Summary().LLMModel)             // model fallback PREMIUM
}
```
(`env.fault` memakai `fake.CapConfig{FailurePhase:"mid", FailAfter:2, FailureRate:1}` pada instance primary.) Test lain (nama persis):
- `TestGenerationNoFallbackAfterAudioSent` (review 2): `g.MarkAudioSent()` setelah DELTA pertama, lalu error primary → event terakhir `ERROR` (kind UNAVAILABLE), tidak ada `RESET`, `UsedFallback=false`, fallback tidak dipanggil (counter fake fallback = 0).
- `TestGenerationTruncatedAfterAudioSent` (review 3): fake LLM `hang` setelah delta pertama (`mid` lalu `hang`), `MarkAudioSent()`, ctx induk ber-deadline → event terakhir `DONE{FinishTruncated}` dengan `Usage != nil`; `Metrics requests_total{outcome="truncated"}` naik; breaker primary TIDAK tercatat kegagalan bila penyebab deadline sesi, TERCATAT TIMEOUT bila penyebab `LLM_TOTAL` (set `TimeoutLLMTotal=40ms`).
- `TestGenerationDeadlineBeforeAudioFallsBack`: deadline total sebelum `MarkAudioSent` → `TIMEOUT` → fallback (RESET bila sudah ada DELTA); bila ctx induk sudah lewat deadline → langsung `ERROR`, tanpa fallback.
- `TestGenerationContentFilteredNoFallback` (review 1): primary mengembalikan `ERROR{CONTENT_FILTERED}` (fault `kind`) → satu `ERROR`, tanpa `RESET`, fallback tidak dipanggil, breaker tetap CLOSED setelah 10 kali; varian `DONE{CONTENT_FILTERED}` diteruskan apa adanya.
- `TestGenerationFirstTokenTimeoutFallsBack` (`TimeoutLLMFirstToken=40ms`, primary `hang` → fallback sukses, `LLMFirstTokenMs` = latensi fallback).
- `TestGenerationCancelYieldsCancelledDone` (cancel ctx pemanggil → `DONE{CANCELLED}`, `ticket.Done(Canceled)` tidak dihitung breaker, `MaxSentences`-style cancel tidak memicu fallback).
- `TestGenerationSemaphoreFullFallsBack` (limit LLM primary 1 ditahan, request kedua → `local_concurrency` → fallback, breaker primary tidak terhitung).
- `TestGenerationUsageSumsAllAttempts` (attempt gagal + attempt fallback → `InputTokens` dijumlahkan, `Estimated` true untuk yang gagal).
- `TestGenerationAllFailYieldsSingleError` (kedua vendor gagal → satu `ERROR` kind terakhir, channel ditutup).
- `TestRouterLogsNeverContainText`: teks sentinel `SENTINEL-秘密-123` di Messages dan di output tidak muncul di buffer log DEBUG maupun label metrik.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'TestGeneration|TestRouterLogs'`. Expected: FAIL (`NewGeneration` belum ada).
- [ ] **Step 3: Implement** `generation.go` mengikuti pseudocode §8.5.7 (satu goroutine per `StreamLLM`; `audioSent atomic.Bool` dibaca sebelum fallback; `RESET` hanya bila `emitted`; `LLMReq.MaxTokens`/`Temperature` kosong → default profile dari `RouterConfig`; `req.Model` diisi router; ctx per attempt `WithTimeout(LLM_TOTAL)`; deadline sesi dibedakan dari `LLM_TOTAL` dengan memeriksa `ctx` induk `Err()` vs `attemptCtx.Err()`). Setiap attempt memanggil `Metrics.ObserveCall/ObserveLatency/Usage/CostUSD` (harga lewat `PriceTable`; `ok=false` → biaya 0) dan health `Record`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race -run 'TestGeneration|TestRouterLogs' -count=5` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): Generation LLM dengan fallback D-042, RESET, TRUNCATED"`

---

### Task 17: `Generation` — TTS: epoch, vendor dipin, hookup `ResolveTTSParams`

**Files:**
- Modify: `internal/provider/generation.go`
- Test: `internal/provider/generation_tts_test.go` (`package provider_test`)

**Interfaces:**
- Consumes: T7 (`ResolveTTSParams`), T11 (fake TTS), T16.
- Produces: `Generation.Synthesize`, `TTSEpochChanged() <-chan int`, `Summary()` bagian TTS (`TTSProvider, TTSModel, TTSVoiceID, TTSCharacters` (jumlah rune SEMUA request termasuk restart), `TTSFirstAudioMs`); chunk meneruskan `Epoch`; `*Error{INVALID_REQUEST,"voice_binding_missing"}` untuk vendor nyata tanpa binding; validasi `SpeakingRate` (0 → 1.0; di luar 0.80–1.05 → `INVALID_REQUEST`).

- [ ] **Step 1: Write the failing test**
```go
func TestTTSSwitchesToFallbackBeforeAudioAndBumpsEpoch(t *testing.T) {
	r, env := newTestRouter(t); env.failTTS(provider.FakePrimary, "before_first")
	g := r.NewGeneration(sid, 1, provider.ProfileStandard, provider.VoiceSelection{})
	s, err := g.Synthesize(ctx, ttsReq("こんにちは。")); require.NoError(t, err)
	last := drainTTS(t, s)[len(drainTTS)-1]
	require.Equal(t, "tts_provider_switched", last.Err.VendorCode)
	require.Equal(t, 1, <-g.TTSEpochChanged())
	s2, _ := g.Synthesize(ctx, ttsReq("こんにちは。")) // runtime mengulang
	for _, c := range drainTTS(t, s2) { require.Equal(t, 1, c.Epoch); require.Equal(t, provider.FakeFallback, c.Provider) }
	require.True(t, g.Summary().UsedFallback)
}
```
Test lain: `TestTTSNoEpochSwitchAfterAudioSent` (review 2: `MarkAudioSent()` lalu primary gagal → `TTSChunk{Err}` apa adanya, epoch tetap 0, `TTSEpochChanged` tidak mengirim); `TestTTSVendorPinnedPerGeneration` (setelah switch, kalimat berikutnya langsung ke fallback tanpa mencoba primary; dua kalimat paralel tidak mencampur vendor); `TestTTSSwitchOnlyOncePerGeneration` (fallback gagal → chunk `Err`, epoch tidak naik lagi); `TestTTSStartsOnFallbackWhenPrimaryBreakerOpen` (`ttsVendor` awal = fallback, `UsedFallback` true, epoch 0); `TestTTSFakeVendorSkipsBindings` (fake: `req.Model=="fake-tts"`, `Resolved.ProviderVoiceID=="FAKE"`, `Params["speed"]==SpeakingRate`); `TestTTSVoiceBindingMissingIsInvalidRequestNotBreaker` (stub vendor bernama `ELEVENLABS` tanpa `Bindings[ELEVENLABS]` → error `voice_binding_missing`, breaker tetap CLOSED, primary → pindah fallback); `TestTTSExpressionDegradedMetric` (`laugh` tanpa AUDIO_TAGS → `kotoki_tts_expression_degraded_total{from="laugh",to="amused"}` = 1); `TestTTSSpeakingRateValidation` (0.79 dan 1.06 → `INVALID_REQUEST`; 0 diterima sebagai 1.0); `TestTTSFirstAudioTimeoutSwitches` (`TimeoutTTSFirstAudio=40ms`, primary `hang` → switch); `TestTTSCharactersSummed` (restart epoch menambah rune); `TestTTSSemaphoreFullSwitches`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run TestTTS`. Expected: FAIL.
- [ ] **Step 3: Implement** mengikuti §8.4.12 (alur per kalimat) dan §8.5.7 (TTS): `attempt(vendor)` mengisi `req.Model = b.ModelOverride ?? ttsModel(profile, vendor)` dan `req.Resolved`; `switchTTS` idempotent (`sync.Once`), `epochCh` buffered 1 (non-blocking send), batalkan stream epoch lama lewat ctx per-epoch; kegagalan setelah chunk pertama sebuah kalimat: bila `audioSent` → `TTSChunk{Err}`, jika belum → perlakukan seperti switch. `Metrics.TTSEpochRestart(from,to)` pada switch.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race -run 'TestTTS|TestGeneration' -count=5` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): Generation TTS dengan epoch, vendor dipin, ResolveTTSParams"`

---

### Task 18: `ResilientSTTSession` — switch, replay buffer, rotasi, sticky

**Files:**
- Create: `internal/provider/resilient_stt.go`
- Test: `internal/provider/resilient_stt_test.go` (`package provider_test`)

**Interfaces:**
- Consumes: `pcmRing` (T2), fake STT (T12) dengan `STTSession` pembungkus perekam audio (`env.recordingSTT`).
- Produces: `(*Router).OpenSTT(ctx, cfg STTSessionConfig) (ResilientSTTSession, error)`; `ResilientSTTSession.Provider()`, `UsedFallback()`, `ReportFinalTimeout()`; event `STTProviderSwitched` dengan `SwitchedFrom`, `ReplayFromMs`, `ReplayTruncated`, `Provider`; `STTStats.Reconnects`.

- [ ] **Step 1: Write the failing test**
```go
func TestResilientSTTSwitchReplayFromLastFinal(t *testing.T) {
	r, env := newTestRouter(t, withSTTRecorder())
	s, _ := r.OpenSTT(ctx, provider.STTSessionConfig{SessionID: sid, Language: "ja", SampleRate: 16000})
	sendMs(t, s, 4000); env.pushFinal(provider.FakePrimary, endMs=1500)    // FINAL berakhir di 1500 ms
	env.killSTT(provider.FakePrimary)                                      // ERROR{UNAVAILABLE} dari vendor
	sw := nextEventOfType(t, s, provider.STTProviderSwitched)
	require.Equal(t, provider.FakePrimary, sw.SwitchedFrom); require.Equal(t, provider.FakeFallback, sw.Provider)
	require.Equal(t, int64(1500), sw.ReplayFromMs); require.False(t, sw.ReplayTruncated)
	require.Equal(t, 2500*32, env.audioReceived(provider.FakeFallback))    // 4000−1500 ms × 32 byte/ms
	require.True(t, s.UsedFallback())
}
```
Test lain: `TestResilientSTTSwitchReplayTruncated` (audio 20 s tanpa FINAL → `ReplayFromMs=5000` (OldestMs), `ReplayTruncated=true`); `TestResilientSTTEventOffsets` (FINAL dari fallback `StartMs/EndMs += vendorOffsetMs`, `Provider=FAKE_FALLBACK`); `TestResilientSTTOldFinalFromFinalizeDroppedAfterSwitch` (FINAL FromFinalize vendor lama yang tiba setelah switch tidak diteruskan); `TestResilientSTTFinalizeDuringReplayDeferred` (Finalize saat replay → dikirim setelah backlog habis; audio live terjaga urutannya); `TestResilientSTTReplayChunks3200` (backlog dikirim potongan 3.200 byte); `TestResilientSTTSwitchOnlyOnce` (fallback gagal → event `ERROR` ke runtime, tidak kembali ke primary); `TestResilientSTTThreeFinalTimeoutsSwitch` (`ReportFinalTimeout()` 3× → switch; FINAL FromFinalize tepat waktu mereset hitungan); `TestResilientSTTRotationSameVendor` (table-driven, DUA kasus: vendor mengirim `ERROR{VendorCode:"session_time_limit_exceeded"}` dan `ERROR{VendorCode:"insufficient_audio_activity"}` (§8.5.8 aturan 5) → buka sesi baru vendor sama, `Reconnects==1`, `UsedFallback()==false`, breaker tidak terhitung, `kotoki_stt_rotations_total` +1); `TestOpenSTTPrimaryOpenFailsUsesFallback`; `TestOpenSTTBothFailReturnsError`; `TestOpenSTTStartWithFallback` (review 5: `StartWith=FALLBACK` → buka fallback lebih dulu, `UsedFallback()==true`, tidak ada `PROVIDER_SWITCHED`, primary tidak dibuka); `TestOpenSTTStartWithFallbackFailsTriesPrimaryOnce`; `TestResilientSTTRingMemory480000` (ring 15 s = 480.000 byte); `TestSTTAudioMsSentIncludesReplay` (`Stats().AudioMsSent` memuat replay); `TestSTTConnectTimeout3s` (`TimeoutSTTConnect=40ms`, `Open` hang → coba fallback); `TestSTTSemaphoreTryNoWait` (limit penuh → vendor berikutnya).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'ResilientSTT|OpenSTT|STTAudio|STTConnect|STTSemaphore'`. Expected: FAIL.
- [ ] **Step 3: Implement** `resilientSTT` dan algoritma `switchTo` persis §8.5.8 (state: `ring, active, activeVendor, vendorOffsetMs, lastFinalEndMs, switched, finalTimeouts, out`). `SendAudio` menulis ring lalu sesi aktif (saat switching hanya ring). `PROVIDER_STT_REPLAY_SPEED` > 0 → pacing `speed×` realtime. Metrik `kotoki_stt_replay_seconds`, `kotoki_provider_fallback_total{capability="STT"}`. Rotasi (vendor sama + replay, bukan switch) dipicu oleh KEDUA `VendorCode` `session_time_limit_exceeded` dan `insufficient_audio_activity`. Satu goroutine pump membaca `active.Events()`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/ -race -run 'ResilientSTT|OpenSTT' -count=5` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): ResilientSTTSession dengan replay buffer 15 s dan fallback sticky"`

---

### Task 19: Router async — retry, `GenerateJSON`, `CompleteText`, `Embed`, pronunciation, `EvaluationRoute`

**Files:**
- Create: `internal/provider/async.go`
- Test: `internal/provider/async_test.go` (`package provider_test`)

**Interfaces:**
- Produces: `(*Router).GenerateJSON`, `CompleteText` (kedua interface), `Embed`, `AssessPronunciation`, `Evaluation() EvaluationRoute`; `(*Router).doAsync` tak-ekspor; `(*Router).RealtimeRouter` memakai `Role` untuk 1+1 vs 2+2.

- [ ] **Step 1: Write the failing test**
```go
func TestGenerateJSONRetriesThenFallsBack(t *testing.T) {
	r, env := newTestRouter(t, withRole(provider.RoleWorker)); env.failEval(provider.FakePrimary, provider.KindUnavailable) // selalu gagal
	res, err := r.GenerateJSON(ctx, evalReq("kotoki_fake_example"))
	require.NoError(t, err); require.Equal(t, provider.FakeFallback, res.Provider)
	require.Equal(t, 2, env.calls(provider.FakePrimary, provider.CapEvaluation)); require.Equal(t, 1, env.calls(provider.FakeFallback, provider.CapEvaluation))
	require.Equal(t, []time.Duration{time.Second}, env.sleeps())     // backoff 1 s sebelum attempt kedua primary
}
```
Test lain: `TestBackoffSequenceForEvaluation` (primary gagal 2×, fallback gagal 2× → total 4 attempt, sleeps `[1s, 4s]` — backoff attempt ke-a = 1 s, 4 s; tanpa jitter; error akhir `*provider.Error` dengan `Retryable` apa adanya); `TestRetryAfterHonoredUpTo30s` (`RetryAfter:7s` → sleep 7 s; `RetryAfter:90s` → dipotong 30 s); `TestNonRetryableGoesStraightToFallback` (`AUTH` → tidak retry primary, fallback eligible); `TestContentFilteredFailsNonRetryableNoFallback`; `TestCancelledReturnsImmediately`; `TestEmbedThreeAttemptsBackoff500ms2s` (tanpa fallback; sleeps `[500ms, 2s]`); `TestAssessPronunciationTwoAttempts` (sleep `[2s]`) dan `TestAssessPronunciationDisabled` (`PronunciationPrimary=NONE` → `ErrPronunciationDisabled`); `TestCompleteTextWorkerTwoPlusTwo` dan `TestCompleteTextRealtimeOnePlusOne` (Role menentukan; teks `<e:...>` tidak dibuang; `UsedFallback`, `Latency` terisi; model STANDARD `gpt-4.1-mini`→fallback `gemini-2.5-flash-lite`); `TestEvaluationRouteSingleAttempt` (`Primary().GenerateJSON` gagal → tepat 1 panggilan, tidak ada sleep; model terisi `gpt-4.1`/`gemini-2.5-pro`; breaker primary OPEN → seketika `*Error{UNAVAILABLE,"circuit_open",Retryable:true}`; `Fallback()` nil bila `FallbackEnabled=false`; panggilan via `Fallback()` menaikkan `kotoki_provider_fallback_total{reason="CALLER_DIRECTED"}`); `TestAsyncSemaphoreWait30sThenRateLimited` (worker: limit 1 ditahan, `Sleep`/ctx timeout → `RATE_LIMITED` retryable); `TestAsyncMetricsAndCost` (`requests_total{task="EVALUATION"}`, `usage_total`, `cost_usd_total` memakai `PriceTable` stub berharga non-nol); `TestGenerateJSONInvalidResponseRetriedOnce` (`INVALID_RESPONSE` retry 1× sesuai tabel §8.3 lalu fallback).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/ -race -run 'GenerateJSON|Backoff|RetryAfter|Embed|AssessPronunciation|CompleteText|EvaluationRoute|NonRetryable|AsyncSemaphore|AsyncMetrics'`. Expected: FAIL.
- [ ] **Step 3: Implement** pseudocode `doAsync` §8.6.2: `attempts` per vendor/kapabilitas dari tabel; `Retry-After` menggantikan backoff (maks 30 s); semua `sleep` lewat `RouterDeps.Sleep` + `Jitter`; `CompleteText` memanggil `LLMProvider.Stream` dan mengumpulkan DELTA (`ERROR` event → error; `DONE{CONTENT_FILTERED}` → `*Error{CONTENT_FILTERED}`); timeout per attempt `TimeoutLLMTotal`; `StructuredRequest.Temperature<0` → 0.2, `Evaluation` timeout `TimeoutEvaluation`. `EvaluationRoute` membungkus provider dengan breaker+semaphore+timeout+metrik tanpa retry.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race -count=3` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider): router async dengan retry, fallback, EvaluationRoute"`

---

### Task 20: `provider/httpx`

**Files:**
- Create: `internal/provider/httpx/client.go`, `classify.go`, `sse.go`
- Test: `internal/provider/httpx/httpx_test.go`

**Interfaces:**
- Consumes: `provider.ClassifyHTTPStatus`, `provider.Error`, `provider.NewError`.
- Produces: `httpx.NewClient() *http.Client` (`ForceAttemptHTTP2=true`, `MaxIdleConnsPerHost=256`, `IdleConnTimeout=90s`, `TLSHandshakeTimeout=3s`, tanpa `ResponseHeaderTimeout`); `httpx.ClassifyHTTP(p provider.Name, c provider.Capability, resp *http.Response, body []byte) *provider.Error` (baca `Retry-After` detik atau HTTP-date; `Message` ≤ 512 byte dan tanpa API key); `httpx.ClassifyError(p, c, err error) *provider.Error` (`context.Canceled`→CANCELLED, `DeadlineExceeded`/`net.Error.Timeout()`→TIMEOUT, DNS/TLS/reset→UNAVAILABLE; close WebSocket 1006/1011/1012/1013 → UNAVAILABLE via `ClassifyWSClose(p, c, code int)` (nama dari plan)); `httpx.NewSSEReader(r io.Reader) *SSEReader` dengan `Next() (event, data string, err error)` (multi-line `data:`, komentar `:`, `[DONE]` dikembalikan apa adanya, `io.EOF` di akhir).

- [ ] **Step 1: Write the failing test**
```go
func TestClassifyHTTPRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"7"}}}
	e := httpx.ClassifyHTTP(provider.OpenAI, provider.CapLLM, resp, []byte(`{"error":{"code":"rate_limit_exceeded"}}`))
	require.Equal(t, provider.KindRateLimited, e.Kind); require.Equal(t, 7*time.Second, e.RetryAfter); require.True(t, e.Retryable)
}
func TestSSEReader(t *testing.T) {
	r := httpx.NewSSEReader(strings.NewReader(": ping\n\ndata: {\"a\":1}\n\nevent: x\ndata: l1\ndata: l2\n\ndata: [DONE]\n\n"))
	_, d, _ := r.Next(); require.Equal(t, `{"a":1}`, d)
	ev, d, _ := r.Next(); require.Equal(t, "x", ev); require.Equal(t, "l1\nl2", d)
}
```
Tambah: tabel seluruh baris pemetaan §8.3 (408/504 TIMEOUT retryable; 401/403 AUTH non-retryable; 400/404/409/413/422 INVALID_REQUEST; 500/502/503/529 UNAVAILABLE retryable); `Retry-After` HTTP-date (`Mon, 02 Jan 2006 15:04:05 GMT` relatif ke `Date` header respons — nama dari plan: pakai `time.Until`? DILARANG `time.Now` hanya di vendor adapter, `httpx` bukan adapter → hitung dari header `Date`); body JSON tak terbaca saat 200 → `ClassifyBody` `INVALID_RESPONSE`; kunci rahasia: `Message` dengan substring `sk-…`/`Bearer …` disamarkan (`[redacted]`); `ClassifyError` untuk `context.Canceled`, `*net.OpError` timeout, `*net.DNSError`; `NewClient` field transport sesuai nilai di atas.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/provider/httpx/ -race`. Expected: FAIL.
- [ ] **Step 3: Implement.** `httpx` boleh mengimpor `net/http`; DILARANG mengimpor adapter vendor/`fake`/`pricing`. `time.Now` tidak dipakai (HTTP-date dihitung terhadap header `Date`; bila tak ada → 0).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/provider/... -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(provider/httpx): client, klasifikasi HTTP, SSE reader"`

---

### Task 21: `prompt` — estimator token, budget, `LearnerContext`, tipe data, `NormalizeSpeakable`

**Files:**
- Create: `internal/prompt/tokens.go`, `budget.go`, `learner_context.go`, `data.go`, `text.go`
- Test: `internal/prompt/tokens_test.go`, `budget_test.go`, `data_test.go`

**Interfaces:**
- Produces: `EstimateTokens(s string) int`; `const ItemOverheadTokens = 4`; `type Budget struct{ System, Character, Scenario, Style, Expression, Learner, MemoryCore, MemoryEpisodic, Summary, SessionState, RecentTurns, CurrentInput int; MinRecentTurns int }`; `var DefaultBudget`; `func (b Budget) Total() int` (nama dari plan; jumlah 12 komponen, tanpa `MinRecentTurns`); `type LearnerContext struct{ DisplayName, Adaptation, GoalLine string; FocusSkills []string }`; `type VocabHint struct{ TermJA string \`json:"term_ja"\`; Reading string \`json:"reading"\`; Meaning string \`json:"meaning"\` }`; `type PromptData struct{...}` dan `type OpeningPromptData = PromptData` persis §6.8.2 (semua sub-struct anonim tersebut dinamai: `CharacterData, TopicData, ScenarioData, StyleData, ExpressionData, MemoryData, SessionData` — nama dari plan; field persis spec); `NormalizeSpeakable(s string) string` (hapus whitespace dan semua rune kategori P* dan S*; **diekspor di `prompt`** karena `SentenceParser` memerlukannya sedangkan spec menaruhnya di `conversation/japanese.go` — M5 memakai `prompt.NormalizeSpeakable`, tidak mendefinisikan ulang).

- [ ] **Step 1: Write the failing test**
```go
func TestEstimateTokens(t *testing.T) {
	require.Equal(t, 0, prompt.EstimateTokens(""))
	require.Equal(t, 11, prompt.EstimateTokens(strings.Repeat("あ", 10)))       // ceil(1.1×10)
	require.Equal(t, 3, prompt.EstimateTokens("hello wor"))                      // ceil(9/3.5)=3
	require.Equal(t, 9, prompt.EstimateTokens("あいうえお"+"あ"+"abcdefg"))      // ceil(1.1×6 + 7/3.5) = ceil(8.6)
}
func TestDefaultBudgetTotal(t *testing.T) { require.Equal(t, 6950, prompt.DefaultBudget.Total()); require.Equal(t, 4, prompt.DefaultBudget.MinRecentTurns)
	require.Equal(t, 700, prompt.DefaultBudget.Learner); require.Equal(t, 600, prompt.DefaultBudget.MemoryCore); require.Equal(t, 500, prompt.DefaultBudget.MemoryEpisodic) }
```
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/ -race`. Expected: FAIL (build error).
- [ ] **Step 3: Implement.** Nilai `DefaultBudget` persis §7.8.3; `EstimateTokens` memakai `math.Ceil(1.1*cjk + other/3.5)` dengan cjk = rune ≥ U+3000.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/prompt/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(prompt): estimator token, budget, LearnerContext, PromptData"`

---

### Task 22: `prompt.Render`, `Template`, `Params`, FuncMap, registry fixture

**Files:**
- Create: `internal/prompt/render.go`, `funcs.go`, `fixtures.go`
- Test: `internal/prompt/render_test.go`, `funcs_test.go`

**Interfaces:**
- Produces: `type Params struct{ Temperature *float64 \`json:"temperature,omitempty"\`; MaxOutputTokens *int \`json:"max_output_tokens,omitempty"\`; TopP *float64 \`json:"top_p,omitempty"\`; Seed *int64 \`json:"seed,omitempty"\` }` + `(p Params) TemperatureOr(def float64) float64`, `MaxOutputTokensOr(def int) int` (nilai DB menang, S-1; nama dari plan); `type Template struct{ Key string; Version int; System, User string; Params Params; OutputSchema json.RawMessage }`; `type Rendered struct{ System, User string }`; `func Render(t Template, data any) (Rendered, error)`; `func RegisterFixtures(key string, fn func() []any)`, `func Fixtures(key string) []any` (nil bila belum terdaftar), `func ResetFixturesForTest()` (nama dari plan; hanya untuk test); FuncMap: `join`, `bullets`, `quote`, `vocab`.

- [ ] **Step 1: Write the failing test**
```go
func TestRenderMissingKeyAndSnakeCaseFail(t *testing.T) {
	_, err := prompt.Render(prompt.Template{System: "{{.Nope}}"}, prompt.PromptData{}); require.Error(t, err)
	_, err = prompt.Render(prompt.Template{System: "{{.character_name}}"}, prompt.PromptData{}); require.Error(t, err)
}
func TestFuncMap(t *testing.T) {
	d := struct{ L []string; V []prompt.VocabHint }{[]string{"a", "b"}, []prompt.VocabHint{{"経験", "けいけん", "experience"}}}
	r, _ := prompt.Render(prompt.Template{System: `{{join .L ", "}}|{{bullets .L}}|{{quote "私"}}|{{vocab .V}}`}, d)
	require.Equal(t, "a, b|- a\n- b|「私」|- 経験（けいけん）: experience", r.System)
}
```
Tambah: `TestRenderUserTemplateOptional` (`User:""` → `Rendered.User==""`; terisi → dirender dengan data sama); `TestRenderIsDeterministic` (100× byte-identik); `TestParamsDBWins` (`Temperature=ptr(0.3)` → 0.3; nil → default 0.8; `MaxOutputTokensOr(320)` idem); `TestParamsJSONOmitsNil`; `TestFixturesRegistry` (`RegisterFixtures("EVALUATION", fn)` → `Fixtures("EVALUATION")` memanggil `fn`; key tak terdaftar → nil; pendaftaran ganda key sama → panic dengan pesan menyebut key); error render tidak membocorkan isi data (`err.Error()` tidak memuat nilai field sentinel).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/ -race -run 'Render|FuncMap|Params|Fixtures'`. Expected: FAIL.
- [ ] **Step 3: Implement.** `template.New(key).Option("missingkey=error").Funcs(funcMap).Parse`; eksekusi pada struct bertipe (bukan map); `vocab` menghasilkan satu baris `- {term}（{reading}）: {meaning}` per item dipisah `\n`; `bullets` pada slice kosong → `""`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/prompt/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(prompt): Render, Template, Params, FuncMap, registry fixture"`

---

### Task 23: `CoreRules`, `OutputContract`, tabel frasa, template v1, fixture min/maks

**Files:**
- Create: `internal/prompt/core.go`, `phrases.go`, `templates_v1.go`, `fixtures_conversation.go`
- Test: `internal/prompt/templates_test.go`, `core_test.go`, `phrases_test.go`

**Interfaces:**
- Produces: `const CoreRules string`; `func OutputContract(allowed []string, closing bool) string` (nama dari plan; `closing=true` menambah arahan menutup percakapan ≤ 60 s, §7.11.1); `phrases.go`: `DialectInstruction(d string) string`, `FormalityInstruction`, `VerbosityInstruction`, `EnergyInstruction`, `QuestionInstruction`, `BehaviorInstructions(flags map[string]bool) []string` (urutan tetap 6 key §6.8.4), `ExpressionGuidance(level string, allowed []string, maxTagged int) string` (nama dari plan; teks persis tabel §6.8.4); `var ConversationV1, OpeningV1 prompt.Template` (`Key`, `Version:1`, `System` = teks persis §6.8.3 disalin ke raw string Go, `Params` `{temperature 0.8, max_output_tokens 320}` / `{0.7, 120}`); `func FixtureMinimal() PromptData`, `FixtureMaximal() PromptData` (maksimum = batas §6.3.2/6.3.3/6.3.6: persona 3000 rune, situation 2000, tasks 6×200, constraints 8×200, grammar_targets 8×100, vocab_hints 20, traits 8×40, quirks 6×150, memory items 20, `Learner.Adaptation` blok lengkap); `init()` memanggil `RegisterFixtures("CONVERSATION", …)` dan `("OPENING", …)` masing-masing mengembalikan `[]any{FixtureMinimal(), FixtureMaximal()}`. Frasa "(Bab 11 §11.7)" di baris Memory rules pada spec adalah rujukan silang dokumen, BUKAN teks prompt → dibuang dari konstanta (catat di `docs/implementation-deviations.md`; M3 seed MUST memakai teks yang sama dengan konstanta ini).

- [ ] **Step 1: Write the failing test**
```go
func TestV1TemplatesRenderWithinLimit(t *testing.T) {
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		for _, d := range prompt.Fixtures(tpl.Key) {
			r, err := prompt.Render(tpl, d); require.NoError(t, err); require.Empty(t, r.User)
			require.LessOrEqual(t, utf8.RuneCountInString(r.System), 12000)
		}
	}
}
func TestNicknameNullHasNoNameSentence(t *testing.T) {
	d := prompt.FixtureMinimal(); d.Learner.DisplayName = ""
	r, _ := prompt.Render(prompt.ConversationV1, d)
	require.NotContains(t, r.System, "The learner's name is"); 
	o, _ := prompt.Render(prompt.OpeningV1, d); require.NotContains(t, o.System, "greet")
}
```
Tambah: `TestCoreRulesAndOutputContractPlaceholdersExactlyOnce` (`strings.Count(tpl.System,"{{.CoreRules}}")==1` dan `{{.OutputContract}}` ==1 untuk kedua template); `TestOutputContractMentionsFormat` (memuat `<e:NAME>`, setiap nama di `allowed`, larangan markdown dan romaji, "Japanese"; `closing=true` memuat kata "wrap up"; `closing=false` tidak); `TestCoreRulesPlusContractWithinBudget` (`EstimateTokens(CoreRules)+EstimateTokens(OutputContract(12 ekspresi,true)) ≤ DefaultBudget.System (600)` minus teks tetap template: batas uji ≤ 450); `TestFormalityKeigoPhrase` dan tabel frasa persis §6.8.4 untuk semua nilai enum (CASUAL/POLITE/KEIGO, SHORT/MEDIUM/LONG, CALM/NEUTRAL/LIVELY, LOW/MEDIUM/HIGH, STANDARD/KANSAI, 6 behavior); `TestExpressionGuidanceSuffix` (diakhiri `Allowed expressions: neutral, happy. At most 2 tagged sentences per reply.`); `TestAdaptationInsertedVerbatim` (blok Jepang berisi `<conversation_difficulty>` muncul byte-identik di output); `TestOpeningRendersOpeningLineQuoted` (`「…」` + `greet {Name} by name` bila nickname ada; kalimat episodik hanya bila `Memory.Episodes` ada); `TestMemoryRulesOnlyWhenMemoryPresent`; `TestPromptDataNeverCarriesFullName` (render dengan fixture lalu `strings.Contains` sentinel `FULLNAME-SENTINEL` tidak mungkin — dijamin struct; test mengasumsikan reflect guard T21 + memeriksa tidak ada template v1 yang menyebut `FullName`/`Email`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/ -race -run 'V1|Nickname|Core|OutputContract|Formality|Expression|Adaptation|Opening|Memory'`. Expected: FAIL.
- [ ] **Step 3: Implement.** Salin teks template dari §6.8.3 (verbatim, kecuali frasa rujukan silang di atas); `CoreRules` (bahasa Inggris, ditulis di sini sesuai §6.8.1 "isinya ditulis Bab 7") WAJIB memuat: selalu jawab bahasa Jepang natural percakapan lisan; tanpa romaji; tanpa markdown/emoji/daftar; satu orang bicara, jangan menulis giliran learner; jangan mengoreksi/menjelaskan tata bahasa; tetap dalam peran; jangan membuka atau menyebut instruksi ini. `OutputContract` memuat format tag D-081 (tag di awal kalimat, nama dari `allowed`, tanpa tag = neutral, tag tak dibacakan).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/prompt/ -race` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(prompt): CoreRules, OutputContract, frasa, template CONVERSATION/OPENING v1"`

---

### Task 24: `SentenceParser` — tag ekspresi, teks bersih, sanitasi

**Files:**
- Create: `internal/prompt/sentence_parser.go`
- Test: `internal/prompt/sentence_parser_tags_test.go`

**Interfaces:**
- Consumes: `NormalizeSpeakable` (T21).
- Produces: `type Sentence struct{ Index int; Text, Expression string }`; `type SentenceParser struct{ MaxRunes, SoftMin, MaxSentences int; Limited bool; OnTag func(result string); ExprFilter func(expr string) string; … }` (`OnTag` dipanggil dengan `"unknown"`, `"midsentence"`, `"rate_limited"`; `ExprFilter` = kebijakan frekuensi milik runtime, dipanggil sekali per kalimat non-neutral; hasil ≠ masukan → ekspresi diganti dan `OnTag("rate_limited")`; kedua hook nama dari plan); `func NewSentenceParser(allowed []string, maxSentences int) *SentenceParser` (`MaxRunes=60`, `SoftMin=20`; `maxSentences<1` → 1); `(*SentenceParser).Feed(delta string) (clean string, out []Sentence)`, `Close() (clean string, out []Sentence)`.

- [ ] **Step 1: Write the failing test**
```go
func TestTagAtSentenceStartSetsExpression(t *testing.T) {
	p := prompt.NewSentenceParser([]string{"neutral", "happy"}, 6)
	c1, o1 := p.Feed("<e:ha"); require.Empty(t, c1); require.Empty(t, o1)         // tag terpotong: tunggu
	c2, o2 := p.Feed("ppy>いい天気ですね。") 
	require.Equal(t, "いい天気ですね。", c2)
	_, o3 := p.Close(); out := append(o2, o3...)
	require.Equal(t, []prompt.Sentence{{0, "いい天気ですね。", "happy"}}, out)
}
func TestTagAfterTerminatorInSameDeltaStartsNextSentence(t *testing.T) {
	p := prompt.NewSentenceParser([]string{"neutral", "happy"}, 6)
	_, o := p.Feed("はい。<e:happy>いいね。")
	require.Equal(t, []prompt.Sentence{{0, "はい。", "neutral"}}, o)               // kalimat 1 keluar karena `<` menyelesaikan lookahead
	_, o2 := p.Close(); require.Equal(t, []prompt.Sentence{{1, "いいね。", "happy"}}, o2)
}
```
Test lain: `TestUnknownTagDroppedAndCounted` (`<e:foo>` → dibuang dari clean & buf, `OnTag("unknown")`); `TestMidSentenceTagDropped` (`今日は<e:happy>晴れ。` → teks `今日は晴れ。`, expression neutral, `OnTag("midsentence")`); `TestTagNameCaseInsensitiveAndRegex` (`<e:HAPPY>` → happy; `<e:a1>` (angka) bukan tag → diperlakukan literal; nama 25 huruf → bukan tag); `TestLiteralLessThanWhenNoCloseWithin32` (`<` diikuti 32 rune tanpa `>` → literal); `TestCloseFlushesIncompleteTagAsLiteral` (`Feed("<e:ha")` lalu `Close()` → clean memuat `<e:ha`); `TestMarkdownAndEmojiStripped` (`**はい**`→`はい`, `` ` `` `#` `_` dibuang, rune kategori So (😀) dibuang, spasi ganda diringkas); `TestExprFilterDowngrade` (filter mengembalikan `"neutral"` → Expression neutral + `OnTag("rate_limited")`); `TestFeedCleanExcludesTags`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/ -race -run 'Tag|Markdown|ExprFilter|Literal'`. Expected: FAIL.
- [ ] **Step 3: Implement** `drainRawIntoBuf` §7.7.3 aturan 1–3 (regex `^e:([a-zA-Z_]{1,24})$`, jendela 32 rune). Keputusan tak ditentukan spec: `drainRawIntoBuf` memproses `raw` rune demi rune dan, SEBELUM memproses sebuah tag, memanggil `cut` dengan lookahead terselesaikan (kalimat yang berakhir terminator+closer di akhir `buf` dianggap lengkap karena rune berikutnya `<` diketahui bukan terminator/closer) dan menyimpan hasilnya di antrean `ready`; `Feed` mengembalikan `ready` + hasil `cut` biasa. Tanpa ini tag awal kalimat berikutnya salah dihitung `midsentence`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/prompt/ -race -run 'Tag|Markdown|ExprFilter|Literal'` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(prompt): SentenceParser tag ekspresi dan sanitasi (D-081)"`

---

### Task 25: `SentenceParser` — pemotongan kalimat Jepang, `MaxSentences`, `Limited`

**Files:**
- Modify: `internal/prompt/sentence_parser.go`
- Test: `internal/prompt/sentence_parser_cut_test.go`

**Interfaces:**
- Produces: perilaku `cut(final bool)` §7.7.3 aturan 1–6 dan batas keras R-17: `Limited` true setelah kalimat ke-`MaxSentences` dikeluarkan; `Feed`/`Close` berikutnya mengembalikan `("", nil)`.

- [ ] **Step 1: Write the failing test**
```go
func TestCutOnTerminatorWithTrailingClosers(t *testing.T) {
	p := prompt.NewSentenceParser([]string{"neutral"}, 6)
	_, o := p.Feed("「はい。」そうですか？！"); 
	require.Equal(t, "「はい。」", o[0].Text)                      // closer ikut
	_, o2 := p.Feed("次"); require.Equal(t, "そうですか？！", o2[0].Text)  // ?! berurutan satu kalimat; keluar setelah rune berikutnya
}
func TestMaxSentencesLimitsAndDiscardsRest(t *testing.T) {
	p := prompt.NewSentenceParser([]string{"neutral"}, 2)
	_, o := p.Feed("一つ目です。二つ目です。三つ目です。")
	require.Len(t, o, 2); require.True(t, p.Limited)
	c, o2 := p.Feed("四つ目です。"); require.Empty(t, c); require.Empty(t, o2)
	c, o3 := p.Close(); require.Empty(t, c); require.Empty(t, o3)
}
```
Test lain: `TestWaitsWhenTerminatorIsLastRune` (`Feed("はい。")` → belum keluar; `Close()` → keluar); `TestHardCutAt60Runes` (62 rune tanpa terminator → kalimat pertama 60 rune bila tak ada soft boundary); `TestSoftBoundaryCutAtOrAfter20` (`、` pada posisi 30 dalam 60+ rune → potong setelah `、`; `、` pada posisi 10 (< SoftMin) tidak dipakai); `TestPunctuationOnlySentenceAppendedToPrevious` (`はい。\n。` → satu kalimat; teks ditempel ke sebelumnya); `TestSingleRuneSentenceMergedIntoNext` (`あ。いいですね。` → `あ。いいですね。` satu kalimat; pada `final` kalimat 1 rune tidak digabung); `TestNewlineIsTerminator`; `TestIndexIncrementsPerEmittedSentence`; `TestLimitedCountsEmittedNotRawTerminators` (kalimat 1-rune yang digabung dihitung satu); `TestFeedByRuneEqualsFeedWhole` (property: memecah input per rune menghasilkan kalimat & ekspresi identik dengan sekali Feed, untuk 5 kalimat uji beserta tag); `TestCloseEmitsRemainderAsLastSentence`; `TestExpressionResetsAfterEachSentence` (tag hanya berlaku untuk satu kalimat).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/prompt/ -race -run 'Cut|MaxSentences|Waits|Hard|Soft|Punctuation|SingleRune|Newline|Index|Limited|FeedByRune|Close|ExpressionResets'`. Expected: FAIL.
- [ ] **Step 3: Implement** `cut(final)`: (1) buang whitespace awal; (2) terminator pertama `i`, perluas `j` selama rune berikutnya terminator/closer, tunggu bila `j` rune terakhir dan `!final` (kecuali lookahead sudah diketahui, T24); (3) tanpa terminator dan `len(buf) ≥ MaxRunes` → soft boundary terakhir (`、 , ， spasi`) di posisi ≥ `SoftMin`, selain itu potong keras `MaxRunes`; (4) `final` → sisa jadi kalimat; (5) `len(NormalizeSpeakable)==0` → tempel ke kalimat sebelumnya; 1 rune ucapan → gabung ke berikutnya (kecuali `final`); (6) `Expression = pending ?? "neutral"`, `Index = idx++`; saat jumlah kalimat keluar == `MaxSentences` → `Limited=true`, kosongkan `buf/raw`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/prompt/ -race -count=3` → PASS; `go test ./internal/prompt/ -race -cover` ≥ 85 %.
- [ ] **Step 5: Commit** — `git commit -am "feat(prompt): SentenceParser pemotongan kalimat Jepang dan MaxSentences"`

---

## Gerbang milestone

Jalankan berurutan; catat perintah + ringkasan output di `docs/implementation-log.md`.

- [ ] `go build ./... && go vet ./internal/provider/... ./internal/prompt/...` → tanpa output.
- [ ] `go test ./internal/provider/... ./internal/prompt/... -race` → semua paket `ok`, termasuk `providertest` suite yang dijalankan terhadap `fake` (LLM, TTS, STT, Evaluation, Embedding, Pronunciation).
- [ ] `go test ./internal/provider/... ./internal/prompt/... -race -count=5` → tidak flaky (khususnya breaker, generation, resilient STT, fake STT).
- [ ] `go test ./internal/provider/... ./internal/prompt/... -race -coverprofile=cover.out && go run ./test/tools/covercheck -profile cover.out -thresholds test/tools/covercheck/thresholds.txt` → `internal/provider` (tanpa sub-paket vendor) ≥ 85 %, `internal/provider/...` ≥ 70 %, `internal/prompt` ≥ 85 %.
- [ ] `make arch-check` → hijau; tambahkan baris di target `arch-check` pada `Makefile` (Modify, satu baris): `go test ./internal/provider ./internal/prompt -run 'TestImportRules' -count=1` sehingga pelanggaran impor (`net/http`, `go-redis`, vendor, paket fitur lain di `provider`/`prompt`) menggagalkan target.
- [ ] `make lint` → 0 issue (depguard `feature-no-infra`, forbidigo `time.Now`/`uuid.New` tidak dilanggar di `internal/provider/**` dan `internal/prompt`).
- [ ] `go test -tags integration ./internal/redisstore/ -race -run FakeFaults` → PASS.
- [ ] Review Focus 1–5: `go test ./internal/provider/ -race -run 'ContentFiltered|NoFallbackAfterAudioSent|NoEpochSwitchAfterAudioSent|Truncated|DeadlineBeforeAudio|BreakerStateMachine|ResilientSTTSwitchReplay|OpenSTTStartWithFallback|PollerClears' -v` → semua PASS (Poller test ada di `./internal/provider/fake/`).
- [ ] Verifikasi akhir bahwa M5 dapat memakai: `go doc ./internal/provider RealtimeRouter` dan `go doc ./internal/prompt SentenceParser` menampilkan signature kontrak di bawah.

## Self-Review

**Cakupan spec.** Tercakup: §8.1–8.3, §8.4.0 (sebagian: `NormalizeJA`, aturan klasifikasi), §8.4.12, §8.5.1–8.5.8, §8.6, §8.7, §8.8.1/8.8.2, §8.9 (semua), §8.11 butir 2–3, §6.6.2–6.6.3 (`ResolveTTSParams`; `content.SpeakingRate` milik M3), §6.8, §6.14 butir 5 & 8 (bagian `Params`) & 10 (sisi prompt), §7.7.3, §7.8.1/7.8.3 (`EstimateTokens`, `Budget`), D-040/041/042/043/080/081, S-7/S-9/S-12/S-13. Tidak tercakup (milik milestone lain): adapter vendor §8.4.1–8.4.11 dan `provider-bench`/`provider-report` §8.8.3/§8.10 (M9), `GET /internal/providers` handler HTTP (M5/`cmd/*`; M4 hanya `HealthSnapshot`), perakitan `PromptData`/`PromptBuilder`/`ConversationInput` (M5), seed SQL prompt & validator konten (M3), `kotoki_session_cost_usd` observasi saat sesi terminal (M5), wiring `RouterConfig`/`fake.Config` ke `config.Config` dan `cmd/*` (M5/M6/M7).

**Konsistensi tipe.** `Selection`, `Breaker/Ticket`, `semaphoreSlot`, `HealthTracker`, `Metrics`, `PriceTable`, `ExpressionCatalog`, `RouterDeps` (+`Role`, `Sleep`, `Jitter`, `Metrics`) didefinisikan sebelum dipakai (T1→T8→T15). `fake` bergantung pada `provider` saja; test router memakai paket `provider_test` agar tidak siklus. `NormalizeSpeakable` diekspor dari `prompt` (T21) dan dipakai `SentenceParser` (T24–25).

**Ambiguitas spec & keputusan (catat di `docs/implementation-deviations.md`).** (1) `RealtimeRouter.CompleteText` dan `AsyncRouter.CompleteText` bertanda tangan sama tetapi kebijakan beda (1+1 vs 2+2, tunggu 100 ms vs 30 s) → `RouterDeps.Role`. (2) `prices.yaml` dibaca dengan `gopkg.in/yaml.v3` (kini di allowlist D-003). (3) `NormalizeSpeakable` ada di `conversation/japanese.go` menurut §7.6.2 tetapi dibutuhkan `prompt` → diekspor di `prompt`. (4) §8.1 butir 1 tidak mengizinkan `provider` mengimpor `platform/otel` sedangkan D-NEW-13-23 meminta span di layer provider → span dibuat di adapter vendor (M9), router/fake M4 tanpa OTel. (5) Aturan 7 §7.7.3 (kebijakan frekuensi ekspresi) butuh riwayat turn milik runtime → hook `ExprFilter`. (6) Tag di awal kalimat berikutnya dalam delta yang sama berisiko salah-hitung `midsentence` → lookahead di `drainRawIntoBuf` (T24). (7) Teks template v1 memuat rujukan silang "(Bab 11 §11.7)" → dibuang. (8) `hash64` fake tidak ditetapkan spec → FNV-64a. (9) Env `PROVIDER_CONCURRENCY_<VENDOR>_<CAP>` dinamai dinamis → field eksplisit 11 kombinasi; instance fake memakai default kapabilitas. (10) Fake memakai `clock.Clock` (forbidigo melarang `time.Now` di `provider/fake`).

**Kontrak yang diproduksi untuk milestone lain.** `provider.RealtimeRouter/AsyncRouter/Generation/ResilientSTTSession/EvaluationRoute/TextRequest`, `provider.NewRouter/RouterDeps/RouterConfig`, `provider.ExpressionCatalog/PriceTable/ResolveTTSParams`, `provider.Error/KindOf/IsRetryable/FallbackEligible`, `fake.New{LLM,TTS,STT,Evaluation,Embedding,Pronunciation}`, `fake.FaultSource`, `redisstore.NewFakeFaults`, `providertest.{STT,LLM,TTS,Evaluation,Embedding,Pronunciation}Suite`, `httpx.*` (M9), `prompt.{Render,Template,Rendered,Params,RegisterFixtures,Fixtures,EstimateTokens,ItemOverheadTokens,Budget,DefaultBudget,LearnerContext,PromptData,VocabHint,SentenceParser,NewSentenceParser,Sentence,NormalizeSpeakable,CoreRules,OutputContract,ConversationV1,OpeningV1}`.
