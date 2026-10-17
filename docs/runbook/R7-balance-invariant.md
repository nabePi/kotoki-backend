# R7 Invarian saldo dilanggar

Severity: SEV1 (integritas saldo/ledger rusak). Alert menyala tanpa jeda (`for: 0m`).

Alert: BalanceInvariantViolation

## Gejala

`kotoki_balance_invariant_violations{check="R1".."R6"} > 0`, ditulis job `balance_reconcile` (per jam, read-only, tanpa auto-fix). Log ERROR `balance.reconcile.mismatch` memuat nama check dan ID baris (tanpa data pribadi). Arti check (spec §4.8):

| Check | Invarian |
|---|---|
| R1 | saldo `balance_accounts` = jumlah `balance_ledger` |
| R2 | `reserved_seconds` account = jumlah reservasi `HELD` |
| R3 | entri ledger terakhir = saldo dan versi account (penulisan di luar `ApplyMovement`) |
| R4 | status reservasi konsisten dengan status sesi |
| R5 | `charged` sesi = `consumed` reservasi; ledger per reservasi seimbang |
| R6 | reservasi `HELD` macet: sesi `RESERVED` lewat deadline > 10 menit (job abandon tidak jalan) |

## Diagnosis cepat

1. Dashboard `Business integrity`: "Balance invariant violations per check", "Seconds since last balance reconcile and reservation cleanup", "Session sweeper actions per hour".
2. Log job yang menemukan pelanggaran:

   ```bash
   kubectl -n kotoki logs deploy/kotoki-worker-core --since=2h | grep balance.reconcile.mismatch
   ```

3. Daftar akun/objek yang melanggar, dari DB lewat peran baca-saja (pemeriksa independen, tidak memakai paket `balance`):

   ```bash
   kotoki-verify --db "$READONLY_DB_URL"
   ```

   Keluaran menyebut ID pemeriksaan V1 (saldo vs ledger), V2 (saldo negatif), V3 dan seterusnya beserta baris yang melanggar; exit 1 bila ada pelanggaran.
4. R6 sering berarti `reservation_cleanup`/`session_sweeper` berhenti: lihat [R9](R9-jobs-stale.md).
5. Cari perubahan terbaru yang menyentuh saldo: deploy terakhir (`kubectl -n kotoki rollout history deployment/kotoki-api`), adjustment admin (`GET /api/v1/admin/users/{user_id}/balance/transactions`), dan restore/migrasi data.

## Mitigasi

- Tidak ada sakelar aplikasi untuk membekukan pembuatan sesi (rate limit `session_create_user` adalah konstanta kode). Bila pelanggaran menyebar (R1/R3/R5 pada banyak akun), tahan pembuatan sesi di gateway (HTTPRoute sementara yang menolak `POST /api/v1/sessions`; di luar repo ini dan belum diuji) atau, bila terpaksa, `kubectl -n kotoki scale deployment/kotoki-api --replicas=0` (seluruh API mati; pertimbangkan dampaknya).
- Investigasi ledger dengan SQL baca-saja pada akun yang disebut check. Jangan pernah `UPDATE`/`DELETE` manual pada `balance_accounts` atau `balance_ledger` (ledger dan audit di-`REVOKE UPDATE, DELETE`; koreksi lewat jalur admin).
- Koreksi hanya melalui adjustment admin (D-031), per akun, dengan alasan tertulis dan `Idempotency-Key` unik:

  ```bash
  curl -sS -X POST "https://api.<domain>/api/v1/admin/users/<user_id>/balance-adjustments" \
    -H "Authorization: Bearer $ADMIN_ACCESS_TOKEN" -H "Content-Type: application/json" \
    -H "Idempotency-Key: incident-<tanggal>-<n>" \
    -d '{"seconds_delta": <detik, boleh negatif>, "reason": "koreksi insiden <id>: <alasan>"}'
  ```

  Token admin diperoleh dengan login admin biasa (jangan menempelkannya ke kanal insiden).
- R6: jalankan ulang job pembersih bila akar masalah (worker-core mati) sudah beres: `kubectl -n kotoki exec deploy/kotoki-worker-core -- /usr/local/bin/worker run-job reservation_cleanup`.
- Catat insiden sebagai SEV1 dan jadwalkan postmortem dalam 5 hari kerja.

## Verifikasi pulih

- Jalankan ulang reconcile: `kubectl -n kotoki exec deploy/kotoki-worker-core -- /usr/local/bin/worker run-job balance_reconcile` mencetak `job=balance_reconcile ran=true`; tunggu scrape berikutnya sampai `max by (check) (kotoki_balance_invariant_violations)` = 0 dan alert padam.
- `kotoki-verify --db "$READONLY_DB_URL"` -> exit 0.
- Entri ledger koreksi tercatat dengan `reason` dan admin yang benar (`GET /api/v1/admin/users/{user_id}/balance/transactions`).
