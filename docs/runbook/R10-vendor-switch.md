# R10 Ganti vendor STT/LLM manual (D-017)

Severity: mengikuti pemicu. Outage vendor aktif (`ProviderUnavailable` pada STT/LLM/EVALUATION) = SEV1; kuota/biaya atau penurunan kualitas = SEV2/SEV3. Tidak ada alert yang menunjuk langsung ke runbook ini (alert vendor menunjuk [R2](R2-provider-degraded.md)); R2 mengarahkan ke sini.

## Gejala

- `ProviderUnavailable` (circuit OPEN) pada vendor STT atau LLM yang aktif; tidak ada pergantian otomatis (D-017).
- Kuota atau biaya vendor aktif tidak dapat dipenuhi, atau kualitas turun dan pemilik produk memutuskan pindah.
- TTS tidak diganti lewat runbook ini: ubah `characters.provider_name` (admin) atau sakelar `ELEVENLABS_TTS_ENABLED`/`CARTESIA_TTS_ENABLED` (lihat [R2](R2-provider-degraded.md)).

## Diagnosis cepat

Vendor aktif = sakelar `*_ENABLED` di ConfigMap `kotoki-provider-config` (satu ConfigMap untuk api, realtime dan semua worker; sumber: `deploy/k8s/base/provider-config/provider.env` dan patch overlay). Aturan startup (`config.Validate`): STT tepat satu dari `OPENAI_STT_ENABLED`/`ELEVENLABS_STT_ENABLED`/`CARTESIA_STT_ENABLED`; LLM tepat satu dari `OPENAI_LLM_ENABLED`/`DEEPSEEK_LLM_ENABLED` (berlaku untuk percakapan dan evaluasi/summary/memory); minimal satu TTS.

```bash
kubectl -n kotoki get configmap kotoki-provider-config -o yaml | grep -E '_(STT|LLM|TTS)_ENABLED|_MODEL'
```

Sebelum memindahkan, konfirmasi vendor tujuan:

1. API key vendor tujuan ada di Secret (nama kunci saja, jangan cetak nilai): `kubectl -n kotoki get secret kotoki-realtime-secrets -o json | jq -r '.data | keys[]'` (realtime memuat `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, `ELEVENLABS_API_KEY`, `CARTESIA_API_KEY`; worker memuat `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`). Bila hilang, tambahkan di secret manager dan sinkronkan ESO ([R8](R8-secret-leak.md) langkah 2).
2. Model vendor tujuan terisi (`OPENAI_STT_MODEL`, `ELEVENLABS_STT_MODEL`, `CARTESIA_STT_MODEL`, `OPENAI_LLM_MODEL`, `DEEPSEEK_LLM_MODEL`, `OPENAI_EVALUATION_MODEL`, `DEEPSEEK_EVALUATION_MODEL`), dan nilainya sudah diverifikasi terhadap dokumentasi vendor (jangan menebak nama model).
3. Kuota akun vendor tujuan cukup (STT >= 1.100 stream, LLM >= 5.000 RPM dan 12 juta TPM untuk beban penuh) dan retensi/privasi vendor sudah ditinjau (khususnya DeepSeek).

## Mitigasi

1. Ubah ConfigMap `kotoki-provider-config` lewat sumbernya (patch overlay atau `provider.env`, lalu deploy; untuk keadaan darurat `kubectl -n kotoki edit configmap kotoki-provider-config` lalu samakan sumbernya setelah itu):
   - STT: tepat satu dari `OPENAI_STT_ENABLED`, `ELEVENLABS_STT_ENABLED`, `CARTESIA_STT_ENABLED` bernilai `true`, dua lainnya `false`.
   - LLM: tepat satu dari `OPENAI_LLM_ENABLED`, `DEEPSEEK_LLM_ENABLED` bernilai `true`.
2. Restart pemakai ConfigMap (ConfigMap tidak memicu rollout sendiri):

   ```bash
   kubectl -n kotoki rollout restart deployment/kotoki-realtime       # STT dan LLM
   kubectl -n kotoki rollout restart deployment/kotoki-worker-evaluation deployment/kotoki-worker-memory deployment/kotoki-worker-learner   # LLM
   kubectl -n kotoki rollout restart deployment/kotoki-worker-core deployment/kotoki-api   # semua workload memuat ConfigMap yang sama dan memvalidasi sakelar yang sama
   kubectl -n kotoki rollout status deployment/kotoki-realtime
   ```

   Realtime memakai drain (preStop 5 detik, safepoint 20 detik, flush 5 detik; grace 40 detik): sesi aktif terputus sesaat lalu klien reconnect ke pod lain. Startup gagal bila bukan tepat satu `true` (pesan menyebut nama variabel); gagal start berarti `rollout status` macet dan pod lama tetap melayani: koreksi sakelar.
3. Verifikasi (lihat bagian di bawah). Jangan menandai selesai sebelum pemeriksaan lolos.
4. Pesan evaluasi/summary yang `FAILED` selama outage ada di DLQ; replay setelah worker memakai vendor baru ([R3](R3-async-pipeline.md): `worker dlq-replay --queue evaluation.session_completed`, lalu antrean lain).
5. Rollback: balik flag yang sama ke nilai semula dan ulangi langkah 2. Catat pergantian, alasan, waktu, dan pemilik keputusan (keputusan vendor aktif milik pemilik produk).

## Verifikasi pulih

- Pod `kotoki-realtime` dan `kotoki-worker-*` `Ready`, tanpa `CrashLoopBackOff` (tanda sakelar tidak lolos validasi "tepat satu").
- Lalu lintas pindah ke vendor baru dan circuit sehat:

  ```promql
  sum by (provider, capability) (rate(kotoki_provider_requests_total{provider="<VENDOR_BARU>"}[5m]))
  max by (provider, capability) (kotoki_provider_circuit_state)
  ```

  `provider` memakai huruf besar (mis. `DEEPSEEK`); laju untuk vendor baru naik, vendor lama berhenti, circuit 0.
- Smoke: `kotoki-cli converse --wav testdata/audio/<fixture>.wav` menghasilkan giliran AI normal tanpa kalimat maaf.
- `ProviderUnavailable` padam; `kotoki-verify --db "$READONLY_DB_URL" --since <waktu-pergantian-RFC3339> --await-evaluations 5m` -> exit 0 setelah evaluasi tertunda diproses.
