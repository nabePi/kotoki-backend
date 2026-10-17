# Checklist OWASP ASVS Level 2 (spec 13.8)

Dokumen ini dibuat di M10 Task 13 dan direview sebelum go-live (MG-5 f di plan M10 Task 24). Tiap butir menunjuk bukti: nama tes atau berkas yang ada di repositori. `TestAsvsChecklistEvidenceExists` (`test/arch/supplychain_test.go`) memeriksa bahwa setiap nama tes dan berkas yang ditulis dalam backtick di tabel memang ada, dan bahwa butir `AUTO` selalu menyebut bukti.

**Penomoran.** Spec 13.8 menyebut ASVS 5.0; plan M10 dan tabel ini memakai bab ASVS 4.0.x (V1 Arsitektur, V2 Autentikasi, V3 Sesi, V4 Akses kontrol, V5 Validasi, V6 Kriptografi, V7 Error dan log, V8 Data, V9 Komunikasi, V10 Kode berbahaya, V11 Logika bisnis, V12 File, V13 API, V14 Konfigurasi) karena petanya ke kontrol yang diuji lebih langsung. Pemetaan ke nomor 5.0 dilakukan reviewer saat tanda tangan.

**Status.**
- `AUTO`: dijaga tes otomatis (unit, integrasi, e2e `-tags e2e`, atau tes arsitektur/manifest) yang jalan di CI.
- `MANUAL`: butuh klaster, vendor, atau penilaian manusia; didaftarkan di plan M10 Task 24 (MG-n) dan daftar kesiapan produksi (Task 23).
- `N/A`: tidak berlaku untuk sistem ini; alasan tertulis.

Tes e2e `TestSecurity_*` memakai biner `cmd/api` dan `cmd/realtime` asli (`test/e2e/security_e2e_test.go`). Pemeriksaan rantai pasok: `make sec-check` (`govulncheck`, `gitleaks detect --config .gitleaks.toml`, `trivy fs`), `renovate.json`, `.gitleaks.toml` (dijaga `TestRenovateConfig` dan `TestGitleaksConfigAllowsOnlyExamples`).

## V1 Arsitektur, desain, pemodelan ancaman

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V1.1 | Batas kepercayaan dan arah dependensi antar paket ditegakkan; handler tipis, tanpa import terlarang | AUTO | `TestForbiddenImportsTable`, `TestFeaturePackagesRespectLayout`, `TestCmdAPIWorkerGraphHasNoCgo` |
| V1.2 | Pemisahan hak per biner: secret per binary (least privilege), role DB terpisah untuk migrasi | AUTO | `TestNoSecretsInConfigMaps`, `TestProdRejectsForbiddenConfig` |
| V1.4 | Kontrol akses diterapkan di server pada lapisan service/query, bukan di klien | AUTO | `TestSecurity_HorizontalAuthz`, `TestAdminRBAC` |
| V1.7 | Pemodelan ancaman dan review desain keamanan oleh manusia | MANUAL | MG-5 (f), daftar kesiapan produksi (Task 23) |

## V2 Autentikasi

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V2.1 | Kebijakan password: panjang minimum, daftar password umum ditolak, tanpa aturan komposisi sewenang-wenang | AUTO | `TestValidatePassword`, `TestCommonPasswordsFile`, `internal/auth/common_passwords.txt` |
| V2.1 | Password lemah ditolak saat registrasi dan reset | AUTO | `TestRegisterValidationErrors`, `TestResetWeakPasswordKeepsToken` |
| V2.4 | Password disimpan sebagai hash argon2id berparameter, format PHC, rehash saat parameter naik | AUTO | `TestArgon2PHCFormat`, `TestArgon2VerifyRoundTrip`, `TestArgon2NeedsRehash`, `TestLoginRehash` |
| V2.4 | Hashing dibatasi konkurensinya (tanpa DoS lewat argon2) | AUTO | `TestArgon2ConcurrencyLimit` |
| V2.2 | Anti-brute-force: login dibatasi per IP dan per email, dengan `Retry-After` dan metrik | AUTO | `TestSecurity_LoginRateLimit`, `TestLoginEmailRateLimited`, `TestRateLimit` |
| V2.2 | Respons login tidak membedakan email tak dikenal dan password salah; forgot-password selalu 202 | AUTO | `TestLoginInvalid`, `TestForgotAlways202` |
| V2.3 | Token reset sekali pakai, kedaluwarsa, token lama dibatalkan saat token baru dibuat | AUTO | `TestResetTwice`, `TestResetExpiredBoundary`, `TestForgotInvalidatesPrevious` |
| V2.5 | Reset password tidak membocorkan keberadaan akun; secret tidak masuk log | AUTO | `TestForgotUnknownSuspendedLimited`, `TestForgotLogsNoSecrets` |
| V2.7 | Akun SUSPENDED atau DELETED tidak dapat login, refresh, atau memakai ticket | AUTO | `TestLoginSuspended`, `TestRefreshSuspendedUser`, `TestTicketIssueInactiveUser`, `TestRequireActive` |
| V2.9 | Kredensial kriptografis: JWT HS256 dengan secret >= 32 byte, rotasi lewat secret sebelumnya | AUTO | `TestNewJWTIssuerShortSecret`, `TestJWTPreviousSecret`, `TestAuthConfigValidate` |
| V2.10 | Tidak ada kredensial bawaan di prod; secret berasal dari secret manager | AUTO | `TestNoSecretsInConfigMaps`, `TestAuthConfigSecretsNeverPrint` |
| V2.8 | MFA | N/A | Tidak ada di lingkup v1 (spec Bab 3); keputusan produk |

## V3 Manajemen sesi dan token

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V3.2 | Access token JWT berumur pendek, `jti` unik, `alg=none` dan kid tak dikenal ditolak, issuer dan audience diperiksa | AUTO | `TestJWTUniqueJTI`, `TestJWTRejectsAlgNone`, `TestJWTUnknownKid`, `TestJWTWrongIssuerOrAudience`, `TestJWTExpiryLeeway` |
| V3.3 | Refresh token dirotasi; pemakaian ulang di luar masa grace mencabut seluruh keluarga | AUTO | `TestRefreshRotates`, `TestRefreshReuseOutsideGrace`, `TestRefreshWithinGrace`, `TestRefreshConcurrent` |
| V3.3 | Logout mencabut refresh token | AUTO | `TestLogout` |
| V3.4 | Cookie refresh: `HttpOnly`, `SameSite=Strict`, path `/api/v1/auth`, `Secure` di luar lokal | AUTO | `TestSecurity_ResponseHeaders`, `TestCookieWithoutSecureInLocal`, `TestAuthConfigValidate` |
| V3.5 | Token tidak di URL: access token di query WebSocket ditolak; ticket WS sekali pakai, 30 detik, terikat sesi | AUTO | `TestSecurity_WebSocketOriginAndQueryToken`, `TestTicketIssue`, `TestTicketInvalidCloses4001` |
| V3.5 | Ticket tidak pernah tercatat di log atau span | AUTO | `TestAccessLogRedactsTicket`, `TestRequestLogNeverHoldsTheTicket`, `TestConnectionSpanHoldsNoTicket`, `TestRedactTicket` |
| V3.7 | Perlindungan CSRF untuk endpoint ber-cookie: cek Origin dan SameSite=Strict | AUTO | `TestSecurity_CSRFRefreshNeedsAllowedOrigin`, `TestRefreshOriginCheck`, `TestOriginCheck` |

## V4 Kontrol akses

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V4.1 | Otorisasi di sisi server pada setiap objek: user B tidak dapat membaca, membatalkan, atau meminta ticket sesi user A, dan respons tidak memuat data A | AUTO | `TestSecurity_HorizontalAuthz` |
| V4.1 | Memori pengguna terisolasi antar pengguna | AUTO | `TestE2E_MemoryIsolationBetweenUsers` |
| V4.2 | Operasi pada objek memakai `WHERE id = $1 AND user_id = $2`, bukan hanya UUID | AUTO | `TestSecurity_HorizontalAuthz`, `TestE2E_MemoryIsolationBetweenUsers` |
| V4.3 | Rute admin hanya untuk ADMIN aktif yang dicek ulang di DB; akses ditolak diaudit | AUTO | `TestRequireAdmin`, `TestAdminRBAC` |
| V4.3 | Aksi admin atas data pengguna (membaca transcript) menulis tepat satu baris audit | AUTO | `TestAdminTurnsWritesExactlyOneAudit` |

## V5 Validasi, sanitasi, encoding

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V5.1 | JSON ketat: field tak dikenal ditolak (`VALIDATION_FAILED`), data berlebih ditolak, tipe konten diperiksa | AUTO | `TestSecurity_StrictJSONAndBodyLimit`, `TestHandleDecodeAndStatus`, `TestHandleErrors` |
| V5.1 | Batas ukuran body (413 dengan envelope D-013), juga untuk body chunked | AUTO | `TestSecurity_StrictJSONAndBodyLimit`, `TestBodyLimit` |
| V5.1 | Parameter path dan query divalidasi (UUID, cursor, limit); nilai buruk menjadi 4xx tanpa membocorkan detail | AUTO | `TestSecurity_ErrorBodiesLeakNothing`, `TestParseLimit`, `TestCursor` |
| V5.3 | Seluruh SQL lewat sqlc (terparameter); `make sqlc-check` gagal bila kode hasil generate usang | AUTO | `sqlc.yaml`, `db/queries` |
| V5.2 | Output API berupa JSON dengan `X-Content-Type-Options: nosniff` dan CSP `default-src 'none'` | AUTO | `TestSecurity_ResponseHeaders`, `TestSecurityHeaders` |

## V6 Kriptografi

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V6.2 | Algoritma standar: argon2id, HMAC-SHA256 untuk JWT, SHA-256 untuk kunci ticket dan hash email di log; acak dari `crypto/rand` | AUTO | `TestArgon2PHCFormat`, `TestJWTRoundTrip`, `TestEmailHash`, `TestTicketIssue` |
| V6.4 | Manajemen secret: ESO dari secret manager, rotasi 90 hari, checksum anotasi memicu rollout | AUTO | `TestSecretsChecksum`, `deploy/k8s/hack/secrets-checksum.sh` |
| V6.4 | Rotasi kunci JWT tanpa memutus pengguna | AUTO | `TestJWTPreviousSecret` |
| V6.4 | Kunci KMS envelope encryption cluster aktif | MANUAL | MG-1 (konfigurasi klaster), daftar kesiapan produksi (Task 23) |

## V7 Penanganan error dan logging

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V7.4 | Respons error memakai envelope D-013 dan tidak memuat SQL, stack trace, nama paket, atau alamat internal | AUTO | `TestSecurity_ErrorBodiesLeakNothing`, `TestWriteErrorGolden`, `TestWriteErrorUnknownAndWSOnlyCodes` |
| V7.4 | Panic menjadi 500 `INTERNAL_ERROR` tanpa membocorkan nilai panic ke klien | AUTO | `TestRecoverer` |
| V7.4 | `X-Request-ID` valid dipantulkan, yang tak valid diganti UUIDv7, dan ada di `error.request_id` | AUTO | `TestSecurity_RequestID`, `TestRequestID`, `TestWriteErrorGeneratesRequestID` |
| V7.1 | Kunci sensitif (password, token, ticket, secret, teks percakapan) disunting oleh logger | AUTO | `TestRedactAllKeys`, `TestRedactNestedGroup`, `TestSensitiveGroups`, `TestLogTranscriptsExempt` |
| V7.1 | Alur auth tidak menulis password atau token ke log | AUTO | `TestAuthLogsNoSecrets`, `TestRefreshLogsNoToken` |
| V7.2 | Event keamanan memakai log audit (`audit=true`): register, login, refresh reuse, logout, reset, consent, hapus akun | AUTO | `TestAuthEventsRegisterLogin`, `TestRefreshEvents`, `TestResetEvents` |
| V7.3 | Log aplikasi 30 hari dan audit 400 hari di Loki; sampel log bersih dari PII | MANUAL | MG-5 (d) |

## V8 Perlindungan data

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V8.1 | Respons berisi data sensitif tidak di-cache (`Cache-Control: no-store` pada `/auth/*` dan `/me*`) | AUTO | `TestSecurity_ResponseHeaders` |
| V8.2 | Data sensitif tidak di URL atau query log (rute, bukan path mentah, yang dicatat) | AUTO | `TestAccessLogRedactsTicket` |
| V8.3 | Persetujuan privasi tercatat dan versi terms dipaksakan | AUTO | `TestRegisterTermsVersion`, `TestUpdateConsents` |
| V8.3 | Hak hapus akun dan purge: lihat bukti end-to-end di Task 14 | AUTO | `TestDeleteAccount` |
| V8.3 | Backup DB <= 14 hari, enkripsi at rest, restore drill | MANUAL | MG-5 (a) |

## V9 Komunikasi

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V9.1 | TLS >= 1.2 di Gateway, HTTP dialihkan 301, HSTS `max-age=31536000; includeSubDomains` | AUTO | `deploy/k8s/assert/50-edge.sh` (manifest); bukti pada koneksi nyata ada di baris berikut |
| V9.1 | Pemeriksaan TLS pada host staging (`testssl.sh`: TLS 1.0/1.1 mati, sertifikat sah, HSTS, 301) | MANUAL | MG-1 |
| V9.2 | Koneksi ke data store terenkripsi: konfigurasi staging dan prod menolak `sslmode` selain verify-full/verify-ca dan `REDIS_URL` bukan `rediss://`; overlay memakai `amqps://` | AUTO | `TestOverlaysPassConfigValidate`, `TestProdRejectsForbiddenConfig` |
| V9.2 | NetworkPolicy membatasi trafik pod ke pod dan egress | AUTO | `deploy/k8s/assert/50-edge.sh` |
| V9.1 | WebSocket memeriksa Origin persis sebelum upgrade (403 tanpa upgrade) | AUTO | `TestSecurity_WebSocketOriginAndQueryToken`, `TestOriginRejected`, `TestOriginAllowed` |

## V10 Kode berbahaya dan rantai pasok

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V10.3 | Dependensi Go dipindai kerentanan yang terpanggil (`govulncheck`) di CI | AUTO | `.github/workflows/ci.yml`, `Makefile` |
| V10.3 | Pemindaian rahasia pada repositori dan riwayat; allowlist hanya untuk contoh dan fixture | AUTO | `.gitleaks.toml`, `TestGitleaksConfigAllowsOnlyExamples`, `TestCIGitleaksStep`, `TestSecCheckTarget` |
| V10.3 | Pemutakhiran dependensi mingguan dengan pin digest | AUTO | `renovate.json`, `TestRenovateConfig` |
| V10.3 | Action CI dipin ke commit SHA; workflow deploy tanpa kredensial literal dan berizin minimum | AUTO | `TestCIWorkflowShape`, `TestDeployWorkflowsShape` |
| V10.3 | Image dipindai Trivy (CRITICAL/HIGH dengan fix), SBOM CycloneDX per image, deploy prod per digest | AUTO | `TestCIBuildImagesMatrix`, `TestDeployScriptOrder` |
| V10.3 | `trivy fs` dan `make sec-check` penuh di runner dengan seluruh alat terpasang | MANUAL | MG-5 (f); alat tidak terpasang di lingkungan pengembangan |

## V11 Logika bisnis

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V11.1 | Saldo tidak dapat negatif, ledger konsisten; invarian R1 sampai R5 | AUTO | `TestM6_PodKill_SweeperCompletesOnce` |
| V11.1 | Satu sesi terbuka per pengguna | AUTO | `TestCreateSecondSessionDifferentKeySessionAlreadyOpen` |
| V11.1 | Rate limit per kebijakan pada endpoint mahal (login, register, sesi, ticket) | AUTO | `TestCatalog`, `TestRateLimit` |

## V12 File

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V12.1 | Tidak ada unggahan file oleh pengguna; audio hanya lewat WebSocket dengan batas frame | N/A | Tidak ada endpoint unggah; batas baca WS di `TestReadLimit` |

## V13 API dan layanan web

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V13.1 | CORS: pencocokan Origin persis, `Vary: Origin`, credentials hanya untuk Origin terdaftar, preflight 204 | AUTO | `TestSecurity_CORSExactMatch`, `TestCORS` |
| V13.2 | Metode dan rute tak dikenal ditolak dengan envelope yang sama (404/405) | AUTO | `TestSecurity_ErrorBodiesLeakNothing`, `TestStatusOf` |
| V13.2 | Katalog kode error REST tertutup | AUTO | `TestRestCatalogHas50Codes` |
| V13.3 | Header keamanan di semua respons API | AUTO | `TestSecurity_ResponseHeaders`, `TestSecurityAndRequestIDHeaders` |
| V13.4 | Timeout dan batas ukuran header dan body pada server HTTP | AUTO | `TestBodyLimit`, `TestServeClosesAConnectionThatNeverSentARequest` |

## V14 Konfigurasi

| Bab ASVS | Kontrol | Status | Bukti |
|---|---|---|---|
| V14.1 | Overlay prod tidak dapat boot dengan konfigurasi terlarang (`PROVIDER_MODE=fake`, origin `http`, secret di ConfigMap, `LOG_TRANSCRIPTS`) | AUTO | `TestOverlaysPassConfigValidate`, `TestProdRejectsForbiddenConfig`, `TestProdHasNoFake`, `TestProviderModeFakeRejectedInProd` |
| V14.1 | Container non-root, rootfs read-only, tanpa privilege escalation, `drop: [ALL]`, seccomp RuntimeDefault | AUTO | `deploy/k8s/assert/00-common.sh`, `TestDockerImagesBuild` |
| V14.2 | Dependensi dan image dipindai dan diperbarui (lihat V10) | AUTO | `renovate.json`, `Makefile` |
| V14.3 | Nilai contoh dan dummy hanya di `.env.example`, `testdata/`, dan overlay dev | AUTO | `TestGitleaksConfigAllowsOnlyExamples`, `.env.example` |
| V14.4 | Header respons keamanan pada tepi (HSTS di Gateway) | AUTO | `deploy/k8s/assert/50-edge.sh` |
| V14.5 | Sakelar yang membocorkan data (`LOG_TRANSCRIPTS`) ditolak di prod | AUTO | `TestProdRejectsForbiddenConfig` |
| V14.5 | Penilaian penetrasi atau review keamanan oleh pihak independen | MANUAL | MG-5 (f) |

## Butir MANUAL ke Task 24

| Butir | MG |
|---|---|
| testssl.sh pada host staging, HSTS dan 301 pada koneksi nyata | MG-1 |
| Enkripsi KMS Secret dan konfigurasi klaster lain yang bukan manifest | MG-1 |
| Restore drill PITR dan `kotoki-verify` | MG-5 (a) |
| Sampel log Loki 1 jam bebas PII, retensi 30 dan 400 hari | MG-5 (d) |
| `make sec-check` penuh (trivy) di runner, review dan tanda tangan checklist ini | MG-5 (f) |
