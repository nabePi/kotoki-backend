# R8 Secret bocor

Severity: SEV1. Tidak ada alert otomatis; dipicu laporan (gitleaks di CI, `make sec-check`, vendor, pihak luar) atau temuan manual.

## Gejala

Credential muncul di tempat yang salah: commit atau log CI, image, log aplikasi, chat, laporan vendor, atau akses mencurigakan di akun vendor/DB. Secret aplikasi hanya hidup di cloud secret manager -> External Secrets Operator -> Secret `kotoki-api-secrets`, `kotoki-realtime-secrets`, `kotoki-worker-secrets`, `kotoki-migrate-secrets`, `kotoki-keda-secrets` (kunci per Secret: `deploy/k8s/overlays/<env>/externalsecrets.yaml`). Tidak ada secret di ConfigMap (dites oleh `TestNoSecretsInConfigMaps`).

## Diagnosis cepat

1. Tentukan sumber dan cakupan: secret mana (JWT, URL DB, URL Redis, URL RabbitMQ, kunci vendor `OPENAI_API_KEY`/`DEEPSEEK_API_KEY`/`ELEVENLABS_API_KEY`/`CARTESIA_API_KEY`, `SMTP_PASSWORD`), sejak kapan, siapa yang bisa melihat.
2. Bila bocor lewat git: `make sec-check` (gitleaks pada repo dan riwayat dengan `.gitleaks.toml`) untuk lingkup temuan. Nilai tidak boleh disalin ke kanal insiden.
3. Periksa penggunaan tak wajar pada sisi penyedia (log akses vendor/DB) dan audit admin (`admin_audit_logs`).
4. Log aplikasi tidak boleh memuat secret (`TestPrivacy_LogsContainNoPII`); bila ada kebocoran lewat log, tentukan stream Loki dan hapus akses.

## Mitigasi

1. Cabut/rotasi di sumber: buat nilai baru di cloud secret manager (kunci vendor: terbitkan kunci baru lalu cabut yang lama di dasbor vendor; DB/Redis/RabbitMQ: ganti kata sandi pada layanan terkelola).
2. Sinkronkan ESO dan rollout (rotasi Secret tidak me-restart pod sendiri):

   ```bash
   kubectl -n kotoki annotate externalsecret <nama> force-sync=$(date +%s) --overwrite
   kubectl -n kotoki get externalsecret            # kolom STATUS harus SecretSynced
   deploy/k8s/hack/secrets-checksum.sh prod        # anotasi kotoki.io/secrets-checksum memicu rollout semua Deployment
   kubectl -n kotoki rollout status deployment/kotoki-api deployment/kotoki-realtime
   ```

   `secrets-checksum.sh` tidak mencetak nilai Secret, hanya sha256 gabungan.
3. JWT (`AUTH_JWT_SECRET`): isi nilai baru dan kosongkan `AUTH_JWT_PREVIOUS_SECRET` di remote `kotoki/prod/api` (kunci kosong dianggap tidak diset), sinkronkan, rollout `kotoki-api`; semua access token lama menjadi tidak sah (pengguna login ulang). Untuk rotasi tanpa pemutusan, isi `AUTH_JWT_PREVIOUS_SECRET` dengan nilai lama selama masa berlaku access token, lalu kosongkan.
4. Refresh token (bila token pengguna diduga bocor): cabut semuanya tanpa menghapus jejak (alasan yang diizinkan `chk_refresh_revoke_reason`):

   ```sql
   UPDATE refresh_tokens SET revoked_at = now(), revoke_reason = 'LOGOUT' WHERE revoked_at IS NULL;
   ```

   Pengguna harus login ulang. (Spec menyebut `DELETE FROM refresh_tokens`; `UPDATE` dipilih agar riwayat tetap ada untuk investigasi.)
5. Kredensial `kotoki-migrate-secrets` dan peran admin DB: ganti, lalu pastikan Job migrate berikutnya memakai nilai baru.
6. Bila secret masuk ke riwayat git: anggap sudah bocor (rotasi wajib); membersihkan riwayat tidak menggantikan rotasi. Setelah rotasi, tambahkan aturan pencegahan di `.gitleaks.toml` bila perlu.
7. Catat sebagai SEV1: waktu, cakupan, tindakan, dan postmortem dalam 5 hari kerja.

## Verifikasi pulih

- `kubectl -n kotoki get externalsecret` semua `SecretSynced`; semua pod `Ready` setelah rollout, tanpa restart baru.
- Kunci lama ditolak (uji token/kunci lama pada layanan yang bersangkutan) dan kunci baru dipakai: `kotoki_provider_requests_total{outcome="success"}` per vendor naik, login berhasil.
- `make sec-check` bersih; `kubectl -n kotoki get configmap -o yaml` tidak memuat nilai rahasia.
- Satu percakapan `kotoki-cli converse` berhasil lewat gateway.
