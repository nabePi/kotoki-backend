# R2 Provider (STT, LLM, TTS) bermasalah

Severity: `ProviderUnavailable` = SEV1 untuk STT/LLM/EVALUATION (tidak ada fallback otomatis, D-017) dan untuk TTS bila semua vendor TTS yang aktif OPEN; `ProviderCircuitOpen` pada vendor TTS yang punya `voice.fallback` = SEV2; `ProviderErrorRateHigh` dan `ProviderFallbackRateHigh` = SEV2/SEV3.

Alert: ProviderErrorRateHigh
Alert: ProviderCircuitOpen
Alert: ProviderUnavailable
Alert: ProviderFallbackRateHigh

## Gejala

- `ProviderErrorRateHigh`: rasio `kotoki_provider_errors_total` terhadap `kotoki_provider_requests_total` per (`provider`, `capability`) > 5% selama 5 menit.
- `ProviderCircuitOpen`: `kotoki_provider_circuit_state == 1` (OPEN; 0 CLOSED, 2 HALF_OPEN) selama 2 menit.
- `ProviderUnavailable`: circuit OPEN pada kapabilitas STT, LLM atau EVALUATION, atau pada semua vendor TTS yang aktif, selama 1 menit. Percakapan baru gagal (turn `FAILED`, AI mengucap kalimat maaf) sampai vendor pulih atau diganti.
- `ProviderFallbackRateHigh`: lebih dari 10% permintaan TTS dilayani oleh `voice.fallback` selama 15 menit (vendor TTS utama karakter bermasalah, kualitas suara berubah).

Label `provider` dan `capability` memakai huruf besar (mis. `OPENAI`, `DEEPSEEK`, `ELEVENLABS`, `CARTESIA`; `STT`, `LLM`, `TTS`, `EVALUATION`, `EMBEDDING`).

## Diagnosis cepat

1. Dashboard `Providers`: panel "Error ratio per provider and capability", "Circuit state per provider and capability", "Latency P95 per provider, capability and phase", "TTS fallback rate per from/to voice provider", "Local concurrency wait P95 and rejections".
2. Jenis error (label `kind` dan `vendor_code`, bukan `result`):

   ```promql
   sum by (provider, capability, kind, vendor_code) (rate(kotoki_provider_errors_total[5m]))
   sum by (provider, capability, outcome) (rate(kotoki_provider_requests_total[5m]))
   ```

3. Status page vendor. Bila `kind` menunjukkan rate limit (429/kuota): kuota akun vendor habis atau konkurensi terlalu tinggi. Cek penolakan lokal dan sisa kuota:

   ```promql
   sum by (provider, capability) (rate(kotoki_provider_concurrency_rejected_total[5m]))
   kotoki_provider_ratelimit_remaining
   ```

4. Sakelar vendor yang sedang aktif (hanya satu STT dan satu LLM yang boleh `true`):

   ```bash
   kubectl -n kotoki get configmap kotoki-provider-config -o yaml | grep -E '_(STT|LLM|TTS)_ENABLED'
   ```

5. Untuk TTS, karakter terdampak dan `voice.fallback`-nya ada di tabel `characters` (admin/DB); `kotoki_content_voice_fallback_missing_total` naik bila ada karakter tanpa fallback.
6. Pengecekan transisi circuit: `increase(kotoki_provider_circuit_transitions_total[15m])`. Bolak-balik OPEN/HALF_OPEN berarti vendor flap.

## Mitigasi

- Rate limit/kuota vendor (429): hubungi vendor atau naikkan kuota; sementara itu turunkan `PROVIDER_CONCURRENCY_<VENDOR>_<KAPABILITAS>` (nilai per pod) di ConfigMap `kotoki-provider-config`, lalu `kubectl -n kotoki rollout restart deployment/kotoki-realtime` (dan `kotoki-worker-*` bila kapabilitasnya LLM/EVALUATION). Tidak ada sakelar aplikasi untuk menahan pembuatan sesi baru; pembatasan CCU hanya lewat gateway atau kapasitas (lihat [R6](R6-realtime-capacity.md)).
- Outage vendor STT atau LLM aktif (SEV1, tanpa fallback otomatis): jalankan [R10](R10-vendor-switch.md) (ganti vendor manual) atau umumkan maintenance.
- Outage TTS: bila karakter punya `voice.fallback` dengan vendor berbeda, sistem sudah beralih sendiri (periksa `kotoki_provider_fallback_total{capability="TTS"}`); bila karakter tidak punya fallback atau vendor fallback ikut mati, ubah `characters.provider_name`/`voice_id` lewat admin atau nonaktifkan vendor TTS yang rusak (`ELEVENLABS_TTS_ENABLED`/`CARTESIA_TTS_ENABLED`, minimal satu tetap `true`) lalu rollout restart realtime.
- Evaluasi/summary yang gagal selama outage masuk retry lalu DLQ; setelah vendor pulih replay lewat [R3](R3-async-pipeline.md).
- Biaya tidak wajar: `sum by (provider, capability) (increase(kotoki_provider_cost_usd_total[1h]))` dan panel "Estimated cost per hour (USD)".

## Verifikasi pulih

- `kotoki_provider_circuit_state` kembali 0 untuk semua (provider, capability) aktif dan alert padam.
- `rate(kotoki_provider_requests_total{provider="<vendor>",outcome="success"}[5m])` naik dan rasio error < 5%.
- Smoke `kotoki-cli converse --wav testdata/audio/<fixture>.wav` menghasilkan giliran AI normal (tanpa kalimat maaf).
- `ProviderFallbackRateHigh` padam: panel "TTS fallback ratio (alert at 10%)" di bawah 10%.
