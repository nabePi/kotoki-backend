# Kaiwa Backend — M2 Identity, Auth & Admin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** User dapat register/login/refresh/logout/reset password, admin dapat grant/adjust saldo dengan ledger + audit, semua lewat REST `cmd/api` dengan fondasi `httpapi` bertipe.
**Architecture:** Paket fitur `auth`, `balance`, `admin` (domain + service + port, tanpa infrastruktur); adapter `postgres` (repo + view store), `redisstore` (ticket WS, rate limit), `mailer`; handler `httpapi` tipis (`Handle[B,R]`, empat grup route). Dependensi: `admin → auth, balance`; `auth` hanya menyentuh sesi/saldo lewat port (`TicketSessionReader`, `AccountOpener`, `AccountDeleter`); bagian sesi di `admin` lewat port `SessionGate` + fake sampai M3.
**Tech Stack:** chi, pgx + sqlc, golang-jwt/v5, `golang.org/x/crypto/argon2`, `golang.org/x/text/unicode/norm`, go-redis v9, `net/smtp`, otelhttp, testify, testcontainers (D-003).
**Spec:** docs/superpowers/specs/2026-10-12-kaiwa-backend-design.md — Bab 3 (§3.1–§3.15), Bab 4 §4.2/§4.3.1/§4.3.3/§4.6.0/§4.6.6/§4.6.7/§4.7/§4.8/§4.9 (grant/adjust), §5.8, §5.9.1/§5.9.2/§5.9.5/§5.9.8, §5.10, §13.2.7, §13.4.3, §13.7.1, §13.8.2/§13.8.6.
**Master plan:** docs/superpowers/plans/2026-10-13-kaiwa-backend-00-master.md (Global Constraints di sana berlaku untuk semua task)
**Depends on:** M1 (asumsi nama, sesuaikan di call-site bila M1 berbeda): `errs.New/Wrap/CodeOf/KindOf/LocationOf/WithDetails(err, any)/DetailsOf(err) any/Messages` + konstanta `errs.Code<PascalCase>` per kode §5.10 (mis. `errs.CodeInvalidCredentials`; konvensi dari plan, mengikuti `errs.CodeInternal`); `clock.Clock{Now()}`, `clock.NewFake(t0)` (+`Advance`); `idgen.Generator{New()}`, `idgen.NewSequence(t)`; `otel.Start/End`; `config` (katalog §13.2, `config.Secret`); `postgres.NewStore(pool, ids, clk)` dengan `(*Store).WithinTx(ctx, func(ctx, *Tx) error)` + `*Tx` memegang sqlc `Queries` ter-bind transaksi; migration 000001–000021; `testutil.NewPostgres/NewRedis/TestdataPath/NewTraceRecorder`; Makefile (`test`, `test-integration`, `sqlc`, `lint`, `arch-check`, `promote-admin`); `RedactingHandler`.

## Global Constraints

- Master Global Constraints berlaku. Entri `docs/implementation-log.md` per task ikut di commit task (tidak diulang di Files).
- Waktu & ID hanya dari `clock.Clock`/`idgen.Generator`; repo memakai waktu dari parameter, **bukan** `now()` DB (agar test dengan fake clock berlaku sampai PG). `crypto/rand` boleh di paket fitur (via `io.Reader` injeksi untuk test).
- Password: 12–128 code point setelah NFC, tidak di-trim; urutan cek TOO_LONG/TOO_SHORT → BLANK → CONTAINS_EMAIL (email penuh atau local-part, case-insensitive) → COMMON_PASSWORD (10.000 password umum, lowercase). Kode `PASSWORD_TOO_WEAK`, `details.reason`.
- Argon2id: m=19456 KiB, t=2, p=1, salt 16 B, key 32 B, PHC `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>` (b64 raw), `AUTH_ARGON2_MAX_CONCURRENT=8`, tunggu semaphore maks 2 s lalu `503 SERVICE_UNAVAILABLE`; email tak dikenal → `Verify` ke dummy hash.
- JWT HS256, header `kid` = 8 hex pertama sha256(secret), claims `iss`=`kaiwa-api` `aud`=`kaiwa` `sub` `role` `fam` `jti` `iat` `nbf` `exp`, TTL 15m, leeway 30 s, secret ≥ 32 byte setelah decode base64, `AUTH_JWT_PREVIOUS_SECRET` hanya verifikasi.
- Refresh token: 32 byte `crypto/rand` base64url tanpa padding (43 char), DB simpan SHA-256 (BYTEA 32), TTL 720h, grace 10 s (`revoked_at >= now − 10s`, inklusif) → `409 REFRESH_TOKEN_ROTATED`; reuse → revoke family `REUSE_DETECTED`. Cookie `kaiwa_refresh; Path=/api/v1/auth; HttpOnly; Secure; SameSite=Strict; Max-Age=2592000`; clear = `Max-Age=0`.
- Reset token: 32 byte, SHA-256, TTL 15m, sekali pakai; mail dispatcher: buffer 1.000, 4 worker, timeout 10 s, retry 3× (1 s, 5 s, 15 s); `forgot-password` selalu `202 {"status":"ACCEPTED"}`.
- Ticket WS: key `kaiwa:ws_ticket:{hex(sha256(ticket))}`, `SET NX EX 30`, `GETDEL`, JSON `{"user_id","session_id","issued_at"}`; Redis error → `503` + `Retry-After: 2`.
- Rate limit (nama: cap/refill): `auth_register_ip` 5/5 per jam; `auth_login_ip` 20/20 per menit; `auth_login_email` 5/5 per 15 menit; `auth_refresh_ip` 60/60 per menit; `auth_forgot_ip` 5/5 per jam; `auth_forgot_email` 3/3 per jam; `auth_reset_ip` 10/10 per jam; `ticket_user` 10/30 per menit; `session_create_user` 5/10 per jam; `account_delete_user` 3/3 per jam; `admin_user` 60/120 per menit; `api_user_default` 60/300 per menit; `api_ip_default` 120/600 per menit. Redis down → limiter lokal (LRU 100.000), fail-open.
- Ledger key: `grant:{Idempotency-Key}`, `grant:initial:{user_id}`, `adjust:{Idempotency-Key}`; header UUID wajib; `seconds` 1–360000, `|seconds_delta|` 1–360000, `reason` 3–500; adjustment tak boleh membuat `available_seconds` < 0 (`409 BALANCE_ADJUSTMENT_NEGATIVE`); durasi sesi `300,600,900,1800,2700,3600`.
- Urutan lock §4.6.0: `balance_accounts → users → sessions → balance_reservations`; audit admin di transaksi yang sama dengan perubahannya.
- REST: body ≤ 64 KiB, `Cache-Control: no-store` untuk `/auth/*` dan `/me*`, list `{"data","next_cursor"}`, cursor base64url `{"v":1,"k":…}`, limit default 20 / max 100, ID/timestamp format §5.8.1.
- Log: tanpa password/token/ticket/email mentah (email di-mask `h***@example.com` atau `email_hash` = 32 hex sha256); event audit `audit=true` + `audit_event` ∈ `register, login_succeeded, login_failed, refresh_reuse_detected, logout, password_reset_requested, password_reset_completed, consent_updated, account_deletion_requested, admin_access_denied`.

## Review Focus

1. **Refresh ganda / multi-tab:** dalam grace 10 s → 409 dan token terbaru tetap valid; di luar grace → seluruh family revoked, dan revoke tetap ter-commit walau respons 401 (commit-then-error). Test: Task 10 (unit), Task 16 (PG), Task 25 (e2e).
2. **Klik ganda / retry grant admin:** key sama → satu ledger + replay `200` + `Idempotent-Replayed: true`; key sama payload/user beda → `409 IDEMPOTENCY_KEY_REUSED`; 20 goroutine paralel → 1 ledger. Test: Task 7, 17, 24.
3. **Enumerasi akun & timing:** forgot-password identik untuk email terdaftar/tidak/ditolak limiter; login email tak dikenal tetap menjalankan `Verify`. Test: Task 9, 11, 23.
4. **Akun SUSPENDED/DELETED:** tidak bisa login-sukses/refresh/ticket; hapus akun dengan sesi `STARTING/ACTIVE/ENDING` → 409 dan tidak ada perubahan (rollback, token tak terhapus); urutan lock account → user → sesi. Test: Task 10, 12, 13, 15.
5. **Rahasia tidak bocor:** password, refresh token, reset token, ticket, email mentah tidak ada di log/respons/access log (`?ticket=` tidak tercatat). Test: Task 9, 10, 11, 22.

## File Structure

```text
internal/auth/            user.go auth.go tx.go errors.go normalize.go password.go common_passwords.txt
                          argon2.go jwt.go dispatcher.go service.go user_service.go ticket.go templates/password_reset.txt
internal/balance/         balance.go tx.go service.go            (charge.go/reservasi = M3)
internal/admin/           admin.go tx.go service.go
internal/mailer/          log.go smtp.go fake.go mailer.go
internal/platform/ratelimit/  policy.go local.go                 (Policy, Decision, Limiter, Catalog, limiter lokal)
internal/redisstore/      ticket_store.go ratelimit.go
internal/postgres/        user_repo.go refresh_token_repo.go password_reset_repo.go balance_repo.go admin_audit_repo.go
                          ticket_session_reader.go session_gate.go store_views.go
db/queries/               users.sql refresh_tokens.sql password_resets.sql balance.sql admin_audit.sql session_gate.sql
internal/httpapi/         handle.go errors.go cursor.go middleware*.go router.go deps.go
                          auth_{routes,handler,dto}.go me_{routes,handler,dto}.go
                          admin_{routes,handler,dto}.go balance_{routes,handler,dto}.go route_table_test.go
internal/testutil/        fakes/{auth_store,balance_store,admin_store,ticket}.go  repotest/{user,refresh,reset,balance,audit}.go  reconcile.go
testdata/api/rest/        errors/*.json auth/*.json me/*.json balance/*.json admin/*.json
cmd/api/main.go           wiring M2          Makefile promote-admin (verifikasi)   .env.example
```

---

### Task 1: Auth domain, port, normalisasi input, password policy

**Files:**
- Create: `internal/auth/{user.go,auth.go,tx.go,errors.go,normalize.go,password.go,common_passwords.txt}`
- Test: `internal/auth/normalize_test.go`, `internal/auth/password_test.go`

**Interfaces:**
- Produces (nama persis; spec §3.2, §4.2): `type Role string` (`RoleUser`,`RoleAdmin`), `type UserStatus string` (`UserStatusActive/Suspended/Deleted`), `User` (kolom §3.3.1 PascalCase; `PhoneNumber *string`, `LastLoginAt/AudioConsentUpdatedAt/DeletedAt/PurgedAt *time.Time`), `Principal{UserID uuid.UUID; Role Role; FamilyID uuid.UUID}` + `WithPrincipal(ctx,p)`/`PrincipalFrom(ctx)(Principal,bool)`, port `PasswordHasher`, `TokenIssuer`, `Mailer`+`MailMessage`, `TicketStore`+`WSTicket`, `TicketSessionReader`+`TicketTarget`, `AccountOpener`, `AccountDeleter` (verbatim §3.2), `Store{WithinTx(ctx, fn func(ctx, Tx) error) error}`, `Tx{Users() UserRepository; RefreshTokens() RefreshTokenRepository; PasswordResets() PasswordResetRepository; Accounts() AccountOpener}`.
- Produces (nama dari plan):
  ```go
  type UserFilter struct{ EmailPrefix string; Status *UserStatus; Role *Role }
  type UserRepository interface {
    Insert(ctx, u User) error                       // ErrEmailTaken | ErrPhoneTaken
    GetByID(ctx, id uuid.UUID) (User, error)        // ErrUserNotFound
    GetByEmail(ctx, email string) (User, error)
    LockByID(ctx, id uuid.UUID) (User, error)       // FOR UPDATE
    UpdateLastLogin(ctx, id uuid.UUID, at time.Time) error
    SetPasswordHash(ctx, id uuid.UUID, hash string, changedAt *time.Time) error // nil = password_changed_at tetap
    UpdateConsents(ctx, id uuid.UUID, termsVersion string, audioConsent bool, at time.Time) (User, error) // ErrUserNotActive
    UpdateStatus(ctx, id uuid.UUID, to UserStatus, at time.Time) error // DELETED mengisi deleted_at
    List(ctx, f UserFilter, after *uuid.UUID, limit int) ([]User, error) // id DESC
  }
  type RefreshToken struct{ ID, UserID, FamilyID uuid.UUID; ParentID *uuid.UUID; TokenHash []byte; UserAgent *string; IP *netip.Addr; CreatedAt, ExpiresAt time.Time; RevokedAt *time.Time; RevokeReason *string; ReplacedByID *uuid.UUID }
  type RefreshTokenRepository interface {
    Insert(ctx, t RefreshToken) error
    GetByID(ctx, id uuid.UUID) (RefreshToken, error)
    LockByHash(ctx, hash []byte) (RefreshToken, error) // FOR UPDATE; ErrRefreshTokenNotFound
    MarkRotated(ctx, id, replacedBy uuid.UUID, at time.Time) error
    RevokeFamily(ctx, familyID uuid.UUID, reason string, at time.Time) (int64, error)
    RevokeAllForUser(ctx, userID uuid.UUID, reason string, at time.Time) (int64, error)
    DeleteAllForUser(ctx, userID uuid.UUID) error
    CountActive(ctx, userID uuid.UUID, now time.Time) (int, error)
  }
  type PasswordResetToken struct{ ID, UserID uuid.UUID; TokenHash []byte; RequestedIP *netip.Addr; CreatedAt, ExpiresAt time.Time; UsedAt, InvalidatedAt *time.Time }
  type ResetLookup struct{ Token PasswordResetToken; Email string; UserStatus UserStatus }
  type PasswordResetRepository interface {
    InvalidateOpenForUser(ctx, userID uuid.UUID, at time.Time) error
    Insert(ctx, t PasswordResetToken) error
    FindByHash(ctx, hash []byte) (ResetLookup, error) // tanpa lock; ErrResetTokenNotFound
    LockByHash(ctx, hash []byte) (ResetLookup, error) // FOR UPDATE OF t
    MarkUsed(ctx, id uuid.UUID, at time.Time) error
    InvalidateOthers(ctx, userID, exceptID uuid.UUID, at time.Time) error
  }
  func NormalizeEmail(s string) (string, error); func NormalizePhone(s string) (string, error); func NormalizeFullName(s string) (string, error)
  func MaskEmail(s string) string; func EmailHash(s string) string
  func ValidatePassword(pw, email string) error; func NewPasswordPolicy(common io.Reader) *PasswordPolicy; func (*PasswordPolicy) Validate(pw, email string) error
  var ErrEmailTaken, ErrPhoneTaken, ErrUserNotFound, ErrUserNotActive, ErrRefreshTokenNotFound, ErrResetTokenNotFound, ErrTokenExpired, ErrTokenInvalid, ErrTicketInvalid, ErrSessionNotFound, ErrAccountHasActiveSession error
  ```

- [ ] **Step 1: Write the failing test** — `TestValidatePassword` (table, `errs.DetailsOf(err)` == `map[string]any{"reason": X}`, `errs.CodeOf` == `CodePasswordTooWeak`): 11 rune → `TOO_SHORT`; 129×`a` → `TOO_LONG`; 12 spasi → `BLANK`; `("HANATANAKA01","hanatanaka01@example.com")` dan password = email penuh → `CONTAINS_EMAIL`; `"QwertyUIOP12"` dengan `NewPasswordPolicy(strings.NewReader("qwertyuiop12\n"))` → `COMMON_PASSWORD`; 12×`e`+U+0301 (decomposed, NFC = 12) → valid, 11× → `TOO_SHORT`; `"correct horse battery"` dan 128×`a` valid. `TestCommonPasswordsFile`: file embed 9.000–10.500 baris, semua lowercase. `TestNormalizeEmail`: `" Hana@Example.com "`→`hana@example.com`; `"\"N\" <a@b.com>"`, `"a@b"`, `"a@@b.com"`, 255 char → error `VALIDATION_FAILED` `details` `{"fields":[{"field":"email","reason":"invalid_format"}]}`. `TestNormalizePhone`: `"+62 812-3456-789"`→`"+628123456789"`; `"08123456789"` error. `TestNormalizeFullName`: NFC, `"Ha\x00na"` error, 101 code point error, 100 ok. `TestMaskEmail("hana@example.com")=="h***@example.com"`; `TestEmailHash` 32 hex dari sha256 lowercase-trim.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth/... -race` → Expected: FAIL (build: undefined `ValidatePassword`, dst.).
- [ ] **Step 3: Implement** semua tipe/port di atas dan fungsi; `common_passwords.txt` = unduh `10k-most-common.txt` dari danielmiessler/SecLists (MIT), lowercase, dedupe, `go:embed`; dokumentasikan sumber di `docs/implementation-log.md`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/auth/... -race` → PASS.
- [ ] **Step 5: Commit** — `git add internal/auth && git commit -m "feat(auth): domain types, ports, input normalization, password policy"`

### Task 2: Argon2id hasher

**Files:**
- Create: `internal/auth/argon2.go`
- Test: `internal/auth/argon2_test.go` (white-box `package auth`)

**Interfaces:**
- Produces: `type Argon2Config struct{ MemoryKiB uint32; Iterations uint32; Parallelism uint8; MaxConcurrent int; MaxWait time.Duration }`; `func NewArgon2Hasher(cfg Argon2Config) *Argon2Hasher` (implements `PasswordHasher`; field `derive func(pw, salt []byte) []byte` dapat diganti test).

- [ ] **Step 1: Write the failing test** — `TestArgon2PHCFormat`: config default → regex `^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`. `TestArgon2VerifyRoundTrip` (m=64): benar true, salah false; `Verify(…, "!")` dan string malformed → `(false, nil)`. `TestArgon2NeedsRehash`: hash m=19456 vs hasher m=32768 → true; sama → false; garbage → true. `TestArgon2ConcurrencyLimit`: `MaxConcurrent:1, MaxWait:1ms`, `derive` memblok pada channel; panggilan kedua `Hash` → `errs.CodeOf(err)==errs.CodeServiceUnavailable`; setelah dilepas, panggilan sukses.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run Argon2` → Expected: FAIL (undefined `NewArgon2Hasher`).
- [ ] **Step 3: Implement** `Hash` (salt `crypto/rand`, semaphore `chan struct{}` + `context.WithTimeout(MaxWait)`), `Verify` (parse PHC, `subtle.ConstantTimeCompare`), `NeedsRehash` (bandingkan m/t/p), `golang.org/x/crypto/argon2.IDKey`, key 32 B.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/auth -race -run Argon2` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(auth): argon2id hasher with concurrency limit"` (setelah `git add`).

### Task 3: JWT TokenIssuer

**Files:**
- Create: `internal/auth/jwt.go`
- Test: `internal/auth/jwt_test.go`

**Interfaces:**
- Produces: `type AccessSubject struct{ UserID uuid.UUID; Role Role; FamilyID uuid.UUID }`; `type Claims struct{ UserID uuid.UUID; Role Role; FamilyID, JTI uuid.UUID; IssuedAt, NotBefore, ExpiresAt time.Time; KeyID string }`; `type JWTConfig struct{ Secret, PreviousSecret []byte; Issuer, Audience string; TTL time.Duration }`; `func NewJWTIssuer(cfg JWTConfig, ids idgen.Generator) (*JWTIssuer, error)` (implements `TokenIssuer`).

- [ ] **Step 1: Write the failing test** — `TestJWTRoundTrip` (claims sama, `exp-iat==15m`, header `kid` == 8 hex sha256(secret), `jti` dari `ids`); `TestJWTRejectsAlgNone` (token `jwt.SigningMethodNone` → `ErrTokenInvalid`); `TestJWTUnknownKid`; `TestJWTPreviousSecret` (token ditandatangani secret lama diterima oleh issuer {Secret baru, Previous lama}, ditolak bila Previous kosong); `TestJWTExpiryLeeway` (now = exp+29s ok, exp+31s → `ErrTokenExpired`; nbf = now+29s ok, +31s → `ErrTokenInvalid`); iss/aud salah → `ErrTokenInvalid`; `NewJWTIssuer` secret 31 byte → error.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run JWT` → Expected: FAIL (undefined).
- [ ] **Step 3: Implement** dengan `jwt.NewParser(WithValidMethods(["HS256"]), WithTimeFunc(→now), WithLeeway(30s), WithIssuer, WithAudience, WithExpirationRequired)`; pilih secret dari `kid` (aktif/sebelumnya, selain itu invalid); map error expired → `ErrTokenExpired`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/auth -race -run JWT` → PASS.
- [ ] **Step 5: Commit** — `feat(auth): HS256 access token issuer with key rotation`.

### Task 4: Mailer adapters

**Files:**
- Create: `internal/mailer/{mailer.go,log.go,smtp.go,fake.go}`
- Test: `internal/mailer/mailer_test.go`, `internal/mailer/smtp_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: `auth.Mailer`, `auth.MailMessage`.
- Produces: `type Config struct{ Mode, AppEnv, From, SMTPHost string; SMTPPort int; SMTPUsername, SMTPPassword string }`; `func New(cfg Config, log *slog.Logger) (auth.Mailer, error)`; `NewLog(log)`, `NewSMTP(cfg Config, tlsCfg *tls.Config)`, `type FakeMailer` (`Send`, `Messages() []auth.MailMessage`, `Last() auth.MailMessage`, `Reset()`, mutex).

- [ ] **Step 1: Write the failing test** — `TestNewFactory`: `log`→LogMailer; `smtp` tanpa host → error; mode `x` → error; `Mode:"log", AppEnv:"prod"` dan `"staging"` → error (§3.9.3), `local` ok. `TestLogMailerLogsMailSent`: handler penangkap → msg `mail.sent` dengan atribut `to`,`subject`,`body`,`tag`. `TestFakeMailerStoresMessagesConcurrent` (race). `TestSMTPBuildMessage`: header `From`, `To`, `Subject` (non-ASCII di-encode RFC 2047), `Content-Type` text/plain UTF-8. Integration `TestSMTPRequiresSTARTTLS`: server loopback yang tidak mengiklankan STARTTLS → `Send` error (tidak mengirim body).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/mailer/... -race` → Expected: FAIL (undefined).
- [ ] **Step 3: Implement** adapter; SMTP: `net/smtp` + `StartTLS` wajib (cek `Extension("STARTTLS")`), `PlainAuth` bila username diisi, timeout dari ctx.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/mailer/... -race` PASS; `go test -tags integration ./internal/mailer/... -race` PASS. (Pengiriman SMTP nyata tidak diuji otomatis; dicatat di log.)
- [ ] **Step 5: Commit** — `feat(mailer): log, smtp, fake adapters`.

### Task 5: Mail dispatcher & template reset password

**Files:**
- Create: `internal/auth/dispatcher.go`, `internal/auth/templates/password_reset.txt`
- Test: `internal/auth/dispatcher_test.go` (`package auth_test`)

**Interfaces:**
- Produces: `type MailQueue interface{ Enqueue(msg MailMessage) bool }`; `type DispatcherOptions struct{ Buffer, Workers int; Timeout time.Duration; Backoffs []time.Duration; Sleep func(ctx context.Context, d time.Duration) error }`; `func NewMailDispatcher(m Mailer, log *slog.Logger, o DispatcherOptions) *MailDispatcher` (`Enqueue`, `Close(ctx) error` menguras antrean); `func RenderPasswordReset(urlBase, rawToken, to string) (MailMessage, error)`.

- [ ] **Step 1: Write the failing test** — `TestDispatcherDelivers`: 3 pesan → FakeMailer 3 setelah `Close`. `TestDispatcherRetries`: mailer selalu gagal, opsi default (`Backoffs` 1s,5s,15s, `Sleep` fake mencatat) → 4 percobaan, sleep tercatat `[1s,5s,15s]`, 1 log ERROR "mail.dropped". `TestDispatcherSendTimeout`: ctx ke `Send` punya deadline ≤ 10 s. `TestDispatcherBufferFull`: Buffer=1, worker diblok → `Enqueue` kedua `false` + log ERROR. `TestRenderPasswordReset`: `Subject=="[Kaiwa] パスワード再設定 / Reset your password"`, `Tag=="password_reset"`, body memuat `https://app.example/reset-password?token=abc` dan "15 menit".
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run 'Dispatcher|RenderPasswordReset'` → Expected: FAIL.
- [ ] **Step 3: Implement** — goroutine pool tanpa goroutine yatim (`Close` menunggu `WaitGroup`); template `text/template` di-embed (isi dari §3.9.1, `{{.URL}}`). Default `Buffer 1000, Workers 4, Timeout 10s`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(auth): mail dispatcher and reset template`.

### Task 6: Balance domain, port, OpenAccount, pembacaan saldo (+ fake & contract suite)

**Files:**
- Create: `internal/balance/{balance.go,tx.go,service.go}`, `internal/testutil/fakes/balance_store.go`, `internal/testutil/repotest/balance.go`
- Test: `internal/balance/service_test.go` (`package balance_test`), `internal/testutil/fakes/balance_store_test.go`

**Interfaces:**
- Produces (nama spec §4.2): `Account{ID, UserID uuid.UUID; AvailableSeconds, ReservedSeconds, Version int64; CreatedAt, UpdatedAt time.Time}`, `LedgerEntry` (kolom §4.3.3 PascalCase), `EntryType` (`EntryGrant…EntryConsume`), `ReferenceType` (`RefSession/RefAdmin/RefRegistration`), `Movement` (verbatim §4.2), `Store{WithinTx}`, `Tx{Balance() Repository}`.
- Produces (nama dari plan): `Repository` M2 = `LockAccountByUserID`, `ApplyMovement`, `FindLedgerByIdempotencyKey` (verbatim §4.2) + `GetAccountByUserID(ctx, userID) (Account, error)` (tanpa lock) + `InsertAccount(ctx, Account) error` + `ListLedger(ctx, userID uuid.UUID, types []EntryType, after *uuid.UUID, limit int) ([]LedgerEntry, error)` (spec §4.2 belum punya filter `entry_type`; ditambah param). M3 menambah method reservasi lewat embed `ReservationRepository` ke `Repository` (hanya penambahan). Sentinel `ErrAccountNotFound`, `ErrDuplicateMovement`. `var AllowedDurations = []int64{300,600,900,1800,2700,3600}`; `type BalanceView struct{ Account; AllowedDurations []int64 }`; `func OpenAccount(ctx, tx Tx, ids idgen.Generator, userID uuid.UUID, initialSeconds int64, at time.Time) error`; `func Description(e LedgerEntry) string` (tabel §4.7); `NewService(store Store, clk clock.Clock, ids idgen.Generator) *Service` dengan `GetBalance(ctx, userID) (BalanceView, error)`, `ListTransactions(ctx, userID, types []EntryType, after *uuid.UUID, limit int) ([]LedgerEntry, *uuid.UUID, error)`.
- Fake: `fakes.NewBalanceStore() *BalanceStore` (state in-memory; `WithinTx` menyalin state & **mengembalikannya bila fn error** untuk simulasi rollback; hook `FailNext(method string, err error)`; mensimulasikan UNIQUE idempotency & `(account,version)`).

- [ ] **Step 1: Write the failing test** — `TestOpenAccountNoInitial` (0/0 version 0, 0 ledger); `TestOpenAccountInitialGrant` (initial 3600 → 1 ledger `GRANT`, `RefRegistration`, key `grant:initial:<uid>`, delta +3600, after 3600, `AccountVersion 1`, account.Version 1, reason `initial balance`); `TestGetBalanceAllowedDurations` (1200 → `[300,600,900]`; 299 → `[]`; 3600 → semua); `TestDescription` 6 baris §4.7 persis (`Welcome balance`, `Balance granted`, `Balance adjustment`, `Reserved for session`, `Used in session`, `Returned from session`); `TestListTransactionsPagination` (3 entry, limit 2 → 2 item id DESC, next = id item ke-2; halaman berikut 1 item, next nil). Suite `repotest.BalanceRepoSuite(t, newStore)`: lock/get account; `ApplyMovement` menaikkan version & mengisi `*_after`; key duplikat → `ErrDuplicateMovement`; `ListLedger` urut id DESC + filter tipe + cursor; user tanpa account → `ErrAccountNotFound`; `fakes_test` menjalankan suite.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/balance/... ./internal/testutil/... -race` → Expected: FAIL.
- [ ] **Step 3: Implement** — `OpenAccount` = `InsertAccount` lalu bila initial>0 `ApplyMovement(GRANT…)` (§4.6.6 registrasi). `ListTransactions` mengambil `limit+1`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(balance): account/ledger domain, OpenAccount, balance reads, fake + contract suite`.

### Task 7: Grant & Adjust (idempotent)

**Files:**
- Modify: `internal/balance/service.go`
- Test: `internal/balance/grant_test.go`

**Interfaces:**
- Consumes: Task 6.
- Produces: `GrantInput{UserID, AdminUserID uuid.UUID; Seconds int64; Reason, IdempotencyKey string; At time.Time}`, `AdjustInput{…; SecondsDelta int64}`, `MovementResult{Entry LedgerEntry; Account Account; Replayed bool}`; `(*Service).GrantTx(ctx, tx Tx, in GrantInput)`, `AdjustTx(ctx, tx Tx, in AdjustInput)`, `Grant(ctx, in)`, `Adjust(ctx, in)` (mandiri via `Store`; retry sekali bila `ErrDuplicateMovement`); `ValidateAdminIdempotencyKey(key string) error`.

- [ ] **Step 1: Write the failing test** — `TestGrantHappyPath` (1800 s, key UUID → entry key `grant:`+key, `RefAdmin`, after 1800, reason tersimpan); `TestGrantReplay` (panggilan kedua `Replayed==true`, 1 ledger); `TestGrantKeyReused` (key sama seconds 600 → `errs.CodeIdempotencyKeyReused`; key sama user lain → idem); `TestGrantValidation` (seconds 0 & 360001 → `CodeValidationFailed`; reason `"ab"` & 501 char gagal; key `""` → `CodeIdempotencyKeyRequired`; `"x"` → `CodeIdempotencyKeyInvalid`); `TestAdjust` (available 600: −601 → `CodeBalanceAdjustmentNegative` & saldo tetap 600; −600 → 0; delta 0 / 360001 → validation; replay; key `adjust:`+key); `TestGrantDuplicateMovementRetry` (`FailNext("ApplyMovement", ErrDuplicateMovement)` → ulang & hasil benar, tepat 1 ledger).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/balance -race -run 'Grant|Adjust'` → Expected: FAIL.
- [ ] **Step 3: Implement** mengikuti §4.6.6/§4.6.7: lock account → cek key (`grant:`/`adjust:`) → replay bila `user_id`, delta, tipe sama, selain itu reused → `ApplyMovement`. User tanpa account → `CodeUserNotFound`. Audit **bukan** di sini (Task 14).
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/balance -race` → PASS.
- [ ] **Step 5: Commit** — `feat(balance): idempotent admin grant and adjustment`.

### Task 8: Fake & contract suite repository auth

**Files:**
- Create: `internal/testutil/fakes/auth_store.go`, `internal/testutil/repotest/{user.go,refresh.go,reset.go}`
- Test: `internal/testutil/fakes/auth_store_test.go`

**Interfaces:**
- Consumes: Task 1, Task 6 (`BalanceStore` dibagi agar `Accounts()` memakai `balance.OpenAccount`).
- Produces: `fakes.NewAuthStore(bal *BalanceStore, ids idgen.Generator) *AuthStore` (`auth.Store`, state sama dengan semantik rollback & `FailNext`); `repotest.UserRepoSuite/RefreshTokenRepoSuite/PasswordResetRepoSuite(t, newStore func(t) auth.Store)`.

- [ ] **Step 1: Write the failing test** — suite User: duplikat email beda kapitalisasi → `ErrEmailTaken`; duplikat phone → `ErrPhoneTaken`; `GetByEmail` case-insensitive; `UpdateConsents` user `SUSPENDED` → `ErrUserNotActive`; `UpdateStatus(DELETED)` mengisi `DeletedAt`; `List` filter `EmailPrefix`/`Status`/`Role` + cursor id DESC. Suite Refresh: `LockByHash` tak ada → `ErrRefreshTokenNotFound`; `MarkRotated` set `RevokeReason="ROTATED"`+`ReplacedByID`; `RevokeFamily` mengembalikan jumlah yang baru direvoke; `CountActive` mengecualikan expired/revoked; `DeleteAllForUser`. Suite Reset: `FindByHash` mengembalikan email+status; `InvalidateOpenForUser`; `MarkUsed`; `InvalidateOthers` tidak menyentuh `exceptID`. `fakes_test` menjalankan ketiganya.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/testutil/... -race` → Expected: FAIL.
- [ ] **Step 3: Implement** fake (mutex, rollback snapshot) + suite (hanya impor `auth` & testify).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `test(auth): in-memory auth store and repository contract suites`.

### Task 9: auth.Service — Register & Login

**Files:**
- Create: `internal/auth/service.go`
- Test: `internal/auth/service_register_test.go` (`package auth_test`)

**Interfaces:**
- Consumes: Task 1–3, 5, 8.
- Produces: `type Config struct{ TermsVersion string; InitialBalanceSeconds int64; AccessTTL, RefreshTTL, RefreshReuseGrace, ResetTTL time.Duration; ResetURLBase string; Rand io.Reader }`; `NewService(store Store, hasher PasswordHasher, tokens TokenIssuer, mail MailQueue, clk clock.Clock, ids idgen.Generator, log *slog.Logger, cfg Config) (*Service, error)` (membuat dummy hash); `RegisterInput{Email, Password, FullName string; PhoneNumber *string; TermsVersion string; AudioConsent *bool; UserAgent string; IP netip.Addr}`; `LoginInput{Email, Password, UserAgent string; IP netip.Addr}`; `AuthResult{AccessToken string; ExpiresIn time.Duration; RefreshToken string; RefreshExpiresAt time.Time; User User}`; `(*Service).Register(ctx, RegisterInput) (AuthResult, error)`, `Login(ctx, LoginInput) (AuthResult, error)`, `TermsVersion() string`.

- [ ] **Step 1: Write the failing test** — `TestRegisterSuccess` (role USER, status ACTIVE, `TermsAcceptedAt==now`, `AudioConsent=false` bila nil, `AudioConsentUpdatedAt==now`, hash berawalan `$argon2id$`, account 0/0, refresh `FamilyID==ID`, `ExpiresIn==15*time.Minute`, `ParseAccess` role USER); `TestRegisterTermsVersion` (`""` → `CodeValidationFailed` `{"fields":[{"field":"terms_version","reason":"required"}]}`; `"2020-01-01"` → `not_current`); `TestRegisterConflicts` (email → `CodeEmailAlreadyRegistered`, phone → `CodePhoneAlreadyRegistered`, password lemah → `CodePasswordTooWeak`, state tidak berubah); `TestRegisterInitialBalance` (3600 → ledger GRANT & available 3600); `TestLoginSuccess` (`LastLoginAt==now`, family baru); `TestLoginInvalid` (salah password & email tak dikenal & status DELETED → `CodeInvalidCredentials`; `Verify` dipanggil tepat 1× pada kasus tak dikenal via hasher pembungkus penghitung); `TestLoginSuspended` (password benar → `CodeAccountSuspended`; password salah → `CodeInvalidCredentials`); `TestLoginRehash` (`NeedsRehash` true → `SetPasswordHash(…, nil)` tanpa mengubah `PasswordChangedAt`); `TestAuthLogsNoSecrets` (buffer log tidak memuat password/email mentah; berisi `audit_event` `register`, `login_succeeded`, `login_failed`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run 'Register|Login|AuthLogs'` → Expected: FAIL.
- [ ] **Step 3: Implement** §3.8.1/§3.8.2: hash di luar tx; tx: `Users().Insert` → `Accounts().OpenAccount(initial)` → issue refresh family (`newOpaqueToken(rand) (raw string, hash []byte)`); mapping `ErrEmailTaken/ErrPhoneTaken` → kode 409. Log audit lewat `slog` (`audit=true`). `Rand` default `crypto/rand.Reader`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(auth): register and login`.

### Task 10: auth.Service — Refresh rotation, reuse detection, Logout

**Files:**
- Modify: `internal/auth/service.go`
- Test: `internal/auth/service_refresh_test.go`

**Interfaces:**
- Produces: `RefreshInput{RefreshToken, UserAgent string; IP netip.Addr}`, `RefreshResult{AccessToken string; ExpiresIn time.Duration; RefreshToken string; RefreshExpiresAt time.Time}`; `(*Service).Refresh(ctx, RefreshInput) (RefreshResult, error)`; `Logout(ctx, refreshToken string) error` (selalu nil untuk token tak valid).

- [ ] **Step 1: Write the failing test** (fake clock t0) — `TestRefreshRotates` (token baru, `ParentID`=lama, family sama, lama `ROTATED`+`ReplacedByID`, `ExpiresAt=now+720h`; role diambil dari DB: user dipromosikan ADMIN → access role ADMIN); `TestRefreshReuseOutsideGrace` (A→B; `Advance(10s+1ms)`; A lagi → `CodeRefreshTokenInvalid`; **setelah error** B tercatat `REUSE_DETECTED` di store; refresh B → invalid; 1 log WARN `refresh_reuse_detected` tanpa token); `TestRefreshWithinGrace` (selisih tepat 10s → `CodeRefreshTokenRotated`, B masih bisa refresh; 10s+1ms → reuse); `TestRefreshGraceButReplacementRevoked` → reuse path; `TestRefreshExpired` (now == `ExpiresAt` → invalid); `TestRefreshSuspendedUser` (invalid + family revoked `USER_STATUS_CHANGED`, persisted); `TestRefreshUnknownToken` (garbage & string non-base64 → invalid); `TestRefreshConcurrent` (2 goroutine token sama → 1 sukses, 1 `CodeRefreshTokenRotated`); `TestLogout` (family `LOGOUT`; token tak valid → nil; refresh sesudahnya → invalid); `TestRefreshLogsNoToken`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run 'Refresh|Logout'` → Expected: FAIL.
- [ ] **Step 3: Implement** §3.8.3 dengan pola **commit-then-error**: closure `WithinTx` mengisi `var outErr error` dan mengembalikan nil untuk jalur reuse/status-nonaktif agar revoke ter-commit; setelah `WithinTx` kembalikan `outErr`. Jalur 409 dan 401-tanpa-revoke melakukan rollback (return error dari fn).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(auth): refresh rotation, reuse detection, logout`.

### Task 11: auth.Service — Forgot & Reset password

**Files:**
- Modify: `internal/auth/service.go`
- Test: `internal/auth/service_reset_test.go`

**Interfaces:**
- Produces: `ForgotInput{Email string; IP netip.Addr; EmailLimited bool}`; `(*Service).ForgotPassword(ctx, ForgotInput) error` (nil untuk semua hasil kecuali email invalid → `CodeValidationFailed`); `ResetInput{Token, NewPassword string}`; `(*Service).ResetPassword(ctx, ResetInput) error`.

- [ ] **Step 1: Write the failing test** — `TestForgotActiveUser` (1 baris token `ExpiresAt=now+15m`, `TokenHash==sha256(raw)`, mail ke email dengan URL `…?token=<raw>` di FakeMailer setelah `Close`; raw tidak ada di store); `TestForgotInvalidatesPrevious`; `TestForgotUnknownSuspendedLimited` (email tak dikenal / SUSPENDED / `EmailLimited:true` → nil, 0 token, 0 mail); `TestForgotInvalidEmailFormat`; `TestResetSuccess` (password baru `Verify` true, `PasswordChangedAt==now`, `UsedAt`, token lain invalidated, semua refresh `PASSWORD_RESET`); `TestResetTwice` → `CodeResetTokenInvalid`; `TestResetExpiredBoundary` (now == `ExpiresAt` → invalid; 1ms sebelum → ok); `TestResetWeakPasswordKeepsToken` (`CodePasswordTooWeak`, token lalu masih bisa dipakai); `TestResetMalformedToken` (bukan 43 char/32 byte → `CodeResetTokenInvalid`); `TestResetUserNotActive`; `TestForgotLogsNoSecrets` (audit `password_reset_requested`, tanpa token/email mentah).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run 'Forgot|Reset'` → Expected: FAIL.
- [ ] **Step 3: Implement** §3.9: forgot = tx (invalidate+insert) lalu `mail.Enqueue(RenderPasswordReset(...))`; reset = `FindByHash` → `ValidatePassword(pw, email)` → `Hash` di luar tx → tx `LockByHash` re-check → update seperti §3.9.2 → 204.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(auth): forgot and reset password`.

### Task 12: auth.UserService (Me, consents, hapus akun)

**Files:**
- Create: `internal/auth/user_service.go`
- Test: `internal/auth/user_service_test.go`

**Interfaces:**
- Produces: `NewUserService(store Store, hasher PasswordHasher, deleter AccountDeleter, clk clock.Clock, log *slog.Logger, termsVersion string) *UserService`; `Me(ctx, userID) (User, error)`; `ConsentsInput{UserID uuid.UUID; TermsVersion string; AudioConsent bool}`; `UpdateConsents(ctx, ConsentsInput) (User, error)`; `DeleteAccount(ctx, userID uuid.UUID, password string) error`; `CurrentTermsVersion() string`; `RequireActive(ctx, userID) (User, error)`.

- [ ] **Step 1: Write the failing test** — `TestMeNotFound` (`CodeUserNotFound`); `TestUpdateConsents` (terms salah → `{"fields":[{"field":"terms_version","reason":"not_current"}]}`; terms sama + audio sama → kedua timestamp tak berubah; audio berubah → `AudioConsentUpdatedAt==now` saja; terms user usang → `TermsAcceptedAt==now`; user SUSPENDED → `CodeAccountSuspended`); `TestDeleteAccount` (password salah → `CodeInvalidCredentials`, deleter tidak dipanggil; SUSPENDED/DELETED → `CodeAccountSuspended`; sukses → deleter dipanggil `(userID, now)`; error deleter `CodeAccountHasActiveSession` diteruskan; log `account_deletion_requested`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run 'Me|Consents|DeleteAccount'` → Expected: FAIL.
- [ ] **Step 3: Implement** §3.13.2/§3.13.3 langkah 1 & 5 (query UPDATE `…WHERE status='ACTIVE'` lewat `Users().UpdateConsents`, `ErrUserNotActive` → 403).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(auth): user service (me, consents, account deletion)`.

### Task 13: TicketService + redisstore.TicketStore

**Files:**
- Create: `internal/auth/ticket.go`, `internal/redisstore/ticket_store.go`, `internal/testutil/fakes/ticket.go`
- Test: `internal/auth/ticket_test.go`, `internal/redisstore/ticket_store_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces: `TicketConfig{TTL time.Duration; RealtimeURL string}`; `NewTicketService(store Store, reader TicketSessionReader, tickets TicketStore, clk clock.Clock, cfg TicketConfig) *TicketService`; `TicketResult{Ticket string; ExpiresIn int; RealtimeURL string}`; `(*TicketService).Issue(ctx, userID, sessionID uuid.UUID) (TicketResult, error)`; `redisstore.NewTicketStore(rdb *redis.Client, clk clock.Clock) *TicketStore` (`auth.TicketStore`); `fakes.TicketSessionReader`, `fakes.TicketStore`.

- [ ] **Step 1: Write the failing test** — unit `TestTicketIssue`: user SUSPENDED → `CodeAccountSuspended`; `ErrSessionNotFound` → `CodeSessionNotFound`; RESERVED sebelum deadline ok; RESERVED `now==deadline` → `CodeSessionStartDeadlinePassed`; STARTING/ACTIVE ok; ENDING/COMPLETED/CANCELLED/ABANDONED/FAILED → `CodeSessionNotConnectable` `details.status`; terms user usang tetap ok; hasil `ExpiresIn==30`, `RealtimeURL=="wss://rt.example/api/v1/realtime"`; store menerima `WSTicket{UserID,SessionID,IssuedAt:now}`; store error → `CodeServiceUnavailable`. Integration: `TestTicketStoreIssueConsume` (ticket 43 char; key `kaiwa:ws_ticket:`+hex sha256 ada, `0<TTL<=30s`, JSON tiga field, ticket mentah tidak ada di key manapun; Consume pertama ok, kedua `ErrTicketInvalid`); `TestTicketConsumeConcurrent` (10 goroutine → tepat 1 sukses); `TestTicketExpires` (ttl 1s → `require.Eventually` `ErrTicketInvalid`); `TestTicketRedisDown` (client ditutup → `CodeServiceUnavailable`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/auth -race -run Ticket` dan `go test -tags integration ./internal/redisstore -race -run Ticket` → Expected: FAIL.
- [ ] **Step 3: Implement** §3.11 langkah 2–7 (tabrakan `NX` → ulang sekali); endpoint HTTP `POST /sessions/{id}/realtime-ticket` ditambah di M3.
- [ ] **Step 4: Run tests to verify they pass** — kedua perintah → PASS.
- [ ] **Step 5: Commit** — `feat(auth): websocket ticket service and redis store`.

### Task 14: admin — port, Grant/Adjust dengan audit, daftar audit

**Files:**
- Create: `internal/admin/{admin.go,tx.go,service.go}`, `internal/testutil/fakes/admin_store.go`, `internal/testutil/repotest/audit.go`
- Test: `internal/admin/service_balance_test.go` (`package admin_test`), `internal/testutil/fakes/admin_store_test.go`

**Interfaces:**
- Consumes: `balance.Service` (`GrantTx/AdjustTx`), `auth.Tx`.
- Produces: `AuditLog` (kolom §3.3.3), `AuditFilter{AdminUserID, TargetUserID *uuid.UUID; Action string}`, `AuditRepository{Insert(ctx, AuditLog) error; List(ctx, AuditFilter, after *uuid.UUID, limit int) ([]AuditLog, error)}`; `RequestMeta{RequestID string; IP *netip.Addr; UserAgent string}`; `SessionGate{OpenSessions(ctx, userID) ([]OpenSession, error); CancelReserved(ctx, sessionID uuid.UUID, at time.Time) error}`, `OpenSession{ID uuid.UUID; Status string}`; `Tx{auth.Tx; balance.Tx; Audit() AuditRepository; SessionGate() SessionGate}`; `Store{WithinTx(ctx, fn func(ctx, Tx) error) error}`; `NewService(store Store, bal *balance.Service, clk clock.Clock, ids idgen.Generator, log *slog.Logger) *Service`; `Grant(ctx, in GrantRequest)`, `Adjust(ctx, in AdjustRequest)` → `balance.MovementResult` (`GrantRequest{balance.GrantInput; Meta RequestMeta}`); `ListTransactions(ctx, userID, types, after, limit)` (404 bila user tak ada); `ListAudit(ctx, AuditFilter, after, limit) ([]AuditLog, *uuid.UUID, error)`; `IsActiveAdmin(ctx, userID) (bool, error)`. Fake: `fakes.NewAdminStore(auth *AuthStore, bal *BalanceStore)` + `fakes.SessionGate` (`SetOpen`, `Cancelled()`, mencatat urutan panggilan).

- [ ] **Step 1: Write the failing test** — `TestAdminGrantWritesAudit` (1 audit `BALANCE_GRANT`, `TargetType USER`, `TargetID==TargetUserID`, `Details` = `{"seconds":1800,"ledger_entry_id":<id>,"available_after":1800}`, `IdempotencyKey`, `RequestID`); `TestAdminGrantReplayNoSecondAudit`; `TestAdminGrantAuditFailureRollsBack` (`FailNext("Audit.Insert")` → error, saldo & ledger kembali 0); `TestAdminGrantUnknownUser` (`CodeUserNotFound`); `TestAdminAdjustAuditDetails` (`{"seconds_delta":-300,"ledger_entry_id":…,"available_before":900,"available_after":600}`); `TestAdminListAuditFilters` (filter admin/target/action + cursor id DESC); `TestIsActiveAdmin` (ADMIN ACTIVE true; ADMIN SUSPENDED false; USER false). Suite `repotest.AuditRepoSuite`: insert/list/filter.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/admin/... ./internal/testutil/... -race` → Expected: FAIL.
- [ ] **Step 3: Implement** — satu `store.WithinTx`: `Users().GetByID` → `bal.GrantTx(ctx, tx, …)` → bila `!Replayed` `Audit().Insert`; retry sekali bila `ErrDuplicateMovement`. Rekam span `admin.Grant` dll via `otel.Start/End`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(admin): grant/adjust with audit, audit listing`.

### Task 15: admin — daftar/detail user, ubah status, DeleteAccount

**Files:**
- Modify: `internal/admin/service.go`
- Test: `internal/admin/service_users_test.go`

**Interfaces:**
- Produces: `UserView{User auth.User; Balance balance.Account}`; `UserDetail{UserView; OpenSessionID *uuid.UUID; ActiveRefreshTokenCount int}`; `ListUsers(ctx, auth.UserFilter, after *uuid.UUID, limit int) ([]UserView, *uuid.UUID, error)`; `GetUser(ctx, id) (UserDetail, error)`; `ChangeStatusInput{AdminID, TargetID uuid.UUID; To auth.UserStatus; Reason string; Meta RequestMeta}`; `ChangeStatus(ctx, ChangeStatusInput) (UserView, error)`; `DeleteAccount(ctx, userID uuid.UUID, at time.Time) error` (memenuhi `auth.AccountDeleter`).

- [ ] **Step 1: Write the failing test** — `TestChangeStatusSuspend` (audit `USER_STATUS_CHANGE` details `{"from":"ACTIVE","to":"SUSPENDED","revoked_refresh_tokens":2}`, 2 token `USER_STATUS_CHANGED`); `TestChangeStatusReactivate`; `TestChangeStatusRules` (DELETED→ACTIVE & DELETED→SUSPENDED → `CodeInvalidUserStatusTransition`; target==admin → `CodeCannotModifySelf`; status sama → 200 tanpa audit & tanpa perubahan; tak ada → `CodeUserNotFound`; reason 2/501 char → `CodeValidationFailed`); `TestChangeStatusToDeleted` (token refresh+reset dihapus, `DeletedAt==now`, sesi RESERVED fake → `CancelReserved` dipanggil, ledger/account tak berubah, audit ditulis); `TestDeleteAccountActiveSessionBlocks` (sesi ACTIVE/STARTING/ENDING → `CodeAccountHasActiveSession` `details.session_id`; state, token, audit **tidak berubah**); `TestDeleteAccountLockOrder` (urutan panggilan fake persis `Balance.LockAccountByUserID` → `Users.LockByID` → `SessionGate.OpenSessions`); `TestGetUserDetail` (`OpenSessionID`, `ActiveRefreshTokenCount`); `TestListUsersViews` (balance tergabung).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/admin -race -run 'Status|DeleteAccount|GetUser|ListUsers'` → Expected: FAIL.
- [ ] **Step 3: Implement** §3.12/§3.13.3 langkah 2–4 dalam satu rutin tx-bound privat `deleteAccountTx` dipakai `DeleteAccount` dan `ChangeStatus(DELETED)`.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/admin -race` → PASS.
- [ ] **Step 5: Commit** — `feat(admin): user status changes and account deletion routine`.

### Task 16: postgres — repo users, refresh token, password reset, AuthStore

**Files:**
- Create: `db/queries/{users,refresh_tokens,password_resets}.sql`, `internal/postgres/{user_repo.go,refresh_token_repo.go,password_reset_repo.go,store_views.go}`; Modify: `internal/postgres/sqlcgen/*` (hasil `make sqlc`)
- Test: `internal/postgres/auth_repos_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces: `(*Tx).Users() auth.UserRepository`, `RefreshTokens()`, `PasswordResets()`, `Accounts() auth.AccountOpener` (jembatan ke `balance.OpenAccount(ctx, tx, ids, …)`); `(*Store).AuthStore() auth.Store` (view: `WithinTx` membungkus callback `*Tx`→`auth.Tx`).
- Terjemah error di adapter: constraint `ux_users_email`→`ErrEmailTaken`, `ux_users_phone`→`ErrPhoneTaken`, `pgx.ErrNoRows`→sentinel; selain itu `errs.Wrap(err, errs.CodeInternal, "...")`. `LockByHash`/`LockByID` memakai `FOR UPDATE`; `email` perbandingan lewat CITEXT.

- [ ] **Step 1: Write the failing test** — `TestPGUserRepoSuite`, `TestPGRefreshTokenRepoSuite`, `TestPGPasswordResetRepoSuite` memanggil `repotest.*` dengan `testutil.NewPostgres(t)`; tambahan: `TestPGRefreshFamilyDeleteOrdering` (DELETE family dengan `parent_id`/`replaced_by_id` self-FK sukses dalam satu statement); `TestPGRegisterAtomic` (`AuthStore().WithinTx`: insert user + `Accounts().OpenAccount(…,3600,…)` lalu fn mengembalikan error → user, account, ledger tidak ada; sukses → `INITIAL` ledger `grant:initial:<uid>`); `TestPGRefreshReuseCommitsRevoke` (jalur Task 10 pada PG: reuse di luar grace → family revoked ter-commit walau error); `TestPGCountActive` memakai waktu dari parameter.
- [ ] **Step 2: Run it to verify it fails** — `make sqlc && go test -tags integration ./internal/postgres -race -run 'PGUser|PGRefresh|PGPassword|PGRegister'` → Expected: FAIL (undefined `AuthStore`).
- [ ] **Step 3: Implement** query sqlc + repo; setiap method repo membuka span lewat tracer pgx M1 (tanpa kode tambahan).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS; `make sqlc-check` bersih.
- [ ] **Step 5: Commit** — `feat(postgres): auth repositories and AuthStore view`.

### Task 17: postgres — repo balance (account & ledger) + BalanceStore

**Files:**
- Create: `db/queries/balance.sql`, `internal/postgres/balance_repo.go`; Modify: `internal/postgres/store_views.go`, `internal/postgres/sqlcgen/*`
- Create: `internal/testutil/reconcile.go` (`//go:build integration || e2e`)
- Test: `internal/postgres/balance_repo_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces: `(*Tx).Balance() balance.Repository`; `(*Store).BalanceStore() balance.Store`; `testutil.AssertReconciled(t *testing.T, pool *pgxpool.Pool)` (menjalankan R1–R5 §4.8 **verbatim**, `require.Empty` tiap hasil). `ApplyMovement` memakai `ids.New()` untuk id ledger, `ux_balance_ledger_idempotency`→`ErrDuplicateMovement`, CHECK violation → `errs.CodeInternal`.

- [ ] **Step 1: Write the failing test** — `TestPGBalanceRepoSuite` (suite Task 6); `TestPGLedgerAppendOnly` (`UPDATE balance_ledger SET reason='x'` dan `DELETE` → SQLSTATE `23001`/restrict_violation); `TestPGGrantConcurrentSameKey` (20 goroutine `balance.Service.Grant` key & user sama → tepat 1 baris ledger, 19 `Replayed`; saldo = seconds); `TestPGGrantSameKeyDifferentUsers` (20 user paralel, 1 key → 1 sukses, 19 `CodeIdempotencyKeyReused`); `TestPGAdjustNegativeRejected` (saldo tak berubah); `TestPGGrantSecondsMatchLedger` + `AssertReconciled` dipanggil di akhir tiap test.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres -race -run PGBalance\|PGLedger\|PGGrant\|PGAdjust` → Expected: FAIL.
- [ ] **Step 3: Implement** — urutan: `UPDATE balance_accounts … RETURNING` lalu `INSERT balance_ledger` (§4.6.0 ApplyMovement); `LockAccountByUserID` = `SELECT … FOR UPDATE`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(postgres): balance account/ledger repository and reconcile helper`.

### Task 18: postgres — audit repo, SessionGate, TicketSessionReader, AdminStore

**Files:**
- Create: `db/queries/{admin_audit,session_gate}.sql`, `internal/postgres/{admin_audit_repo.go,session_gate.go,ticket_session_reader.go}`; Modify: `internal/postgres/store_views.go`, `sqlcgen/*`
- Test: `internal/postgres/admin_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces: `(*Tx).Audit() admin.AuditRepository`, `(*Tx).SessionGate() admin.SessionGate` (`OpenSessions` = SELECT `sessions` status ∈ RESERVED/STARTING/ACTIVE/ENDING; `CancelReserved` mengembalikan `errs.New(errs.CodeInternal, "session cancel not wired before M3")` — **M3 wajib menggantinya** dengan `session.Service.CancelInTx`), `(*Store).AdminStore() admin.Store`, `postgres.NewTicketSessionReader(pool) auth.TicketSessionReader` (JOIN `sessions` × `balance_reservations`, §3.11 langkah 3).

- [ ] **Step 1: Write the failing test** — `TestPGAuditRepoSuite` (`repotest.AuditRepoSuite`); `TestPGAuditAppendOnly` (UPDATE/DELETE ditolak trigger); `TestPGAdminGrantAtomicWithAudit` (`admin.Service.Grant` pada PG: ledger + audit konsisten; audit gagal dipaksa dengan `action` tidak valid via repo langsung → tx rollback); `TestPGAdminDeleteAccountFlow` (user + tokens → `DeleteAccount` → status DELETED, `deleted_at`, refresh/reset terhapus, account & ledger utuh, `AssertReconciled`); `TestPGSessionGateNoSessions` (`OpenSessions` kosong); `TestPGTicketSessionReaderNotFound` (`ErrSessionNotFound`). Jalur "ditemukan" untuk reader/gate diuji di M3 saat ada seed sesi.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/postgres -race -run 'PGAudit|PGAdmin|PGSessionGate|PGTicket'` → Expected: FAIL.
- [ ] **Step 3: Implement** repo, gate, reader, `AdminStore()`; `admin.Tx` diimplementasikan `*postgres.Tx` (embed `auth.Tx`, `balance.Tx` sudah dipenuhi accessor-nya).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(postgres): audit repo, session gate, ticket reader, AdminStore`.

### Task 19: platform/ratelimit — Policy, Catalog, limiter lokal

**Files:**
- Create: `internal/platform/ratelimit/{policy.go,local.go}`
- Test: `internal/platform/ratelimit/ratelimit_test.go`

**Interfaces:**
- Produces (lokasi dari plan: spec menaruh tipe di `redisstore`/`httpapi` sehingga tidak saling memenuhi; satu paket netral): `Policy{Name string; Capacity, Refill int; Period time.Duration}`, `Decision{Allowed bool; Remaining int; RetryAfter time.Duration}`, `Limiter{Allow(ctx, p Policy, subject string) (Decision, error)}`, `var Catalog map[string]Policy` (semua policy §13.4.3 + helper `Get(name) Policy`), `NewLocal(clk clock.Clock, maxEntries int) *Local`, `SubjectEmail(email string) string` (= `EmailHash`), `SubjectIP`.

- [ ] **Step 1: Write the failing test** — `TestCatalog` (tiap nilai di Global Constraints: mis. `auth_login_email` {5, 5, 15m}, `admin_user` {60,120,1m}, `ticket_user` {10,30,1m}); `TestLocalBurstAndRefill` (fake clock: capacity 5 → 5 allowed, ke-6 ditolak `RetryAfter>0`; `Advance` sesuai refill → allowed lagi); `TestLocalPerSubject`; `TestLocalLRUEviction` (`maxEntries=2`, 3 subject → entry tertua dibuang tanpa panic).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/platform/ratelimit -race` → Expected: FAIL.
- [ ] **Step 3: Implement** token bucket in-memory (`sync.Mutex`, map + list LRU).
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `feat(ratelimit): policy catalog and local fallback limiter`.

### Task 20: redisstore.RateLimiter (Lua + fallback)

**Files:**
- Create: `internal/redisstore/ratelimit.go`
- Test: `internal/redisstore/ratelimit_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Produces: `NewRateLimiter(rdb *redis.Client, fallback ratelimit.Limiter, log *slog.Logger, rejected, backendErrors prometheus.Counter?) *RateLimiter` — parameter metrik: `*prometheus.CounterVec` `kaiwa_ratelimit_rejected_total{policy}` dan Counter `kaiwa_ratelimit_backend_errors_total`; implements `ratelimit.Limiter`. Script Lua §13.4.3 verbatim lewat `EVALSHA` (fallback `EVAL` bila `NOSCRIPT`).

- [ ] **Step 1: Write the failing test** — `TestRedisLimiterBurstThenDeny` (policy cap 3: 3 allowed, ke-4 `Allowed=false`, `RetryAfter>0`, `Remaining` turun 2,1,0); `TestRedisLimiterRefill` (`Period` 200ms → `require.Eventually` allowed lagi); `TestRedisLimiterKeyHasTTL` (`kaiwa:ratelimit:{policy}:{subject}` `PTTL>0`); `TestRedisLimiterSubjectIsolation`; `TestRedisLimiterFallbackWhenDown` (client ditutup → memakai `fallback`, tidak error, counter backend error naik, log warn maks 1× / 10 s untuk 5 panggilan cepat).
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/redisstore -race -run RedisLimiter` → Expected: FAIL.
- [ ] **Step 3: Implement** — waktu dari `redis.call('TIME')` (bukan clock app), log warn dibatasi 1× / 10 s memakai `clock.Clock`.
- [ ] **Step 4: Run tests to verify they pass** — PASS.
- [ ] **Step 5: Commit** — `feat(redisstore): token bucket rate limiter with local fallback`.

### Task 21: httpapi fondasi — Handle, Page, WriteError, cursor, golden error

**Files:**
- Create: `internal/httpapi/{handle.go,errors.go,cursor.go}`, `testdata/api/rest/errors/*.json` (satu per kode di `errs.Messages`)
- Test: `internal/httpapi/handle_test.go`, `internal/httpapi/errors_test.go` (`package httpapi_test`)

**Interfaces:**
- Produces (spec §5.8.6c verbatim): `Request[B]` (+`PathUUID`, `Query`), `Validator`, `NoContent`, `Page[T]`, `Handle[B,R]`, `HandleNoBody[R]`, `WriteError(w, r, err)`, `WriteJSON(w, status, v)`; tambahan **(nama dari plan, karena signature spec tak bisa set cookie/header/status)**: `Request.ClientIP netip.Addr`, `Request.UserAgent string`, `(Request).Cookie(name string) string`, `(Request).SetCookie(*http.Cookie)`, `(Request).SetHeader(k, v string)`, `(Request).SetStatus(int)`; efek ini ditulis pada jalur sukses **dan** error. `NewPage[T](items []T, next *string) Page[T]` (nil → `[]`); `EncodeCursor(key any) string`, `DecodeCursor(s string, key any) error` (→ `CodeInvalidCursor`); `ParseLimit(q url.Values) (int, error)`; `FieldError(field, reason string) error` (`VALIDATION_FAILED` + `details.fields`); `var statusByCode map[errs.Code]int`.

- [ ] **Step 1: Write the failing test** — `TestHandleDecodeAndStatus` (201, field nullable `null`, `Content-Type: application/json; charset=utf-8`); `TestHandleErrors` tabel: Content-Type bukan JSON → 415 `UNSUPPORTED_MEDIA_TYPE`; `{` → 400 `INVALID_JSON`; field asing → 400 `VALIDATION_FAILED` `{"fields":[{"field":"x","reason":"unknown_field"}]}`; tipe salah → `invalid_type`; body 64 KiB+1 → 413 `PAYLOAD_TOO_LARGE`; `Validate()` error diteruskan; `NoContent` → 204 tanpa body; `SetCookie` & `SetHeader` ikut pada respons error; `SetStatus(200)` menimpa 201; `TestPageEmpty` (`{"data":[],"next_cursor":null}`); `TestCursor` (roundtrip; base64 rusak & `{"v":2,"k":1}` → `CodeInvalidCursor`); `TestParseLimit` (kosong→20; 0 & 101 → `VALIDATION_FAILED` `out_of_range`; 100 ok); `TestWriteErrorGolden` — untuk **setiap** kode di `errs.Messages`: body = `{"error":{"code","message","request_id":"0192f1a0-1111-7000-8000-000000000001","details":null}}` persis golden `testdata/api/rest/errors/<CODE>.json` dan status = `wantStatus[code]` (salin kolom HTTP §5.10, satu baris per kode); kode tak dikenal → 500 `INTERNAL_ERROR` dengan pesan INTERNAL_ERROR (tanpa bocor); `SERVICE_UNAVAILABLE` menambah `Retry-After: 1`; error internal dicatat ERROR sekali dengan `error_code` & `error_location`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/httpapi -race -run 'Handle|Page|Cursor|Limit|WriteError'` → Expected: FAIL.
- [ ] **Step 3: Implement**; hasilkan golden dengan `go test ./internal/httpapi -run WriteErrorGolden -update`, lalu periksa tiap `status` vs §5.10.
- [ ] **Step 4: Run tests to verify they pass** — tanpa `-update` → PASS.
- [ ] **Step 5: Commit** — `feat(httpapi): typed handlers, pagination, error writer, golden error test`.

### Task 22: httpapi middleware (§3.7, §13.8.2)

**Files:**
- Create: `internal/httpapi/{middleware.go,middleware_cors.go,middleware_telemetry.go}`
- Test: `internal/httpapi/middleware_test.go`

**Interfaces:**
- Produces: `RequestID`, `Recoverer(log *slog.Logger)`, `AccessLog(log *slog.Logger)`, `Telemetry` (otelhttp, nama span `HTTP {method} {route}`), `CORS(origins []string)`, `BodyLimit(max int64)`, `SecurityHeaders`, `ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr`, `RateLimit(l ratelimit.Limiter, p ratelimit.Policy, subject SubjectFunc)` (`SubjectFunc func(*http.Request) string`; `IPSubject(trusted)`, `UserSubject`), `Authn(t auth.TokenIssuer, clk clock.Clock)`, `RequireAdmin(g AdminGate)` dengan `AdminGate{IsActiveAdmin(ctx, uuid.UUID)(bool, error)}`, `OriginCheck(origins []string)`; semua `func(http.Handler) http.Handler` kecuali yang ditulis lain.

- [ ] **Step 1: Write the failing test** — `TestRequestID` (UUID valid dipakai; selain itu UUIDv7 baru — §13.8.2 dipilih atas §3.7); `TestRecoverer` (panic → 500 `INTERNAL_ERROR`, 1 log ERROR dengan `error_location`); `TestCORS` (origin ok → `Allow-Origin`, `Vary: Origin`, `Allow-Credentials: true`; preflight 204 dengan methods `GET, POST, PUT, PATCH, DELETE, OPTIONS`, headers `Authorization, Content-Type, Idempotency-Key, X-Request-ID`, `Max-Age: 600`; origin asing → tanpa header); `TestBodyLimit` (>65536 → 413 `PAYLOAD_TOO_LARGE`); `TestSecurityHeaders` (`nosniff`, `no-referrer`, `DENY`, CSP; `Cache-Control: no-store` hanya `/api/v1/auth/*` & `/api/v1/me*`); `TestClientIP` (XFF `"203.0.113.5, 10.0.0.2"` + trusted `10.0.0.0/8` + peer `10.0.0.9` → `203.0.113.5`; peer tak tepercaya mengabaikan XFF; trusted kosong → RemoteAddr); `TestRateLimit` (ok → `RateLimit-Remaining`; ditolak `RetryAfter=1500ms` → 429 `RATE_LIMITED` `Retry-After: 2`; limiter error → lolos + log warn); `TestAuthn` (tanpa header/`Basic x` → `AUTH_REQUIRED`; expired → `TOKEN_EXPIRED`; sampah → `TOKEN_INVALID`; ok → `PrincipalFrom`); `TestRequireAdmin` (claim USER → `FORBIDDEN` tanpa panggil gate; claim ADMIN tapi gate false → `FORBIDDEN` + log `admin_access_denied`; gate error → 500; ok lolos); `TestOriginCheck` (asing → `ORIGIN_NOT_ALLOWED`; tidak ada → lolos); `TestAccessLogRedactsTicket` (`GET /api/v1/realtime?ticket=SECRET` → log memuat route, status, `duration_ms`, tanpa `SECRET`); `TestTelemetrySpanName` (`HTTP POST /api/v1/auth/login` lewat `testutil.NewTraceRecorder`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/httpapi -race -run 'Middleware|RequestID|Recoverer|CORS|BodyLimit|ClientIP|RateLimit|Authn|RequireAdmin|OriginCheck|AccessLog|Telemetry|SecurityHeaders'` → Expected: FAIL.
- [ ] **Step 3: Implement** (CORS tanpa library; tidak ada log token/query).
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(httpapi): middleware chain`.

### Task 23: Router, empat grup, rute auth & me, TestRouteTable

**Files:**
- Create: `internal/httpapi/{router.go,deps.go,auth_routes.go,auth_handler.go,auth_dto.go,me_routes.go,me_handler.go,me_dto.go,route_table_test.go}`, `testdata/api/rest/auth/*.json`, `testdata/api/rest/me/*.json`
- Test: `internal/httpapi/auth_http_test.go`, `internal/httpapi/me_http_test.go`

**Interfaces:**
- Produces: `Config{CORSOrigins []string; TrustedProxies []netip.Prefix; BodyLimit int64; Cookie CookieConfig{Secure bool; Domain string; MaxAge time.Duration}}`; `Deps{Log; Cfg Config; Limiter ratelimit.Limiter; Tokens auth.TokenIssuer; Clock; Auth authService; Users userService; Balance balanceService; Admin adminService; AdminGate AdminGate}` (interface kecil di tiap handler file, dipenuhi `*auth.Service` dst.); `NewRouter(d Deps) http.Handler` (grup publik/user/admin; `NotFound`→404 `NOT_FOUND`, `MethodNotAllowed`→405 via `WriteError`); `registerAuthRoutes(pub, user chi.Router, h *authHandler)`, `registerMeRoutes(user chi.Router, h *meHandler)` — policy IP per rute dipasang di `*_routes.go`, policy berbasis email (`auth_login_email`, `auth_forgot_email`) dipanggil di handler setelah decode (`ratelimit.SubjectEmail`). DTO: `registerRequest`, `loginRequest`, `authResponse{access_token, token_type:"Bearer", expires_in, user}`, `meResponse` (§5.9.2 termasuk `terms_current_version`).
- `routeRow{Method, Path, Group, Policies []string; Ready bool}` memuat seluruh indeks §5.8.5 (`/admin/content/*` sebagai baris `*`); `Ready` true untuk auth + me.

- [ ] **Step 1: Write the failing test** — `TestRouteTable`: `chi.Walk` set rute terdaftar == baris `Ready`; grup & policy ditentukan **perilaku** (request tanpa Authorization: kode ≠ `AUTH_REQUIRED` → publik; token USER di rute admin → `FORBIDDEN`; `limiter` fake merekam policy yang dipanggil == `Policies` baris). `TestInternalPathsNotServed` (`/healthz`,`/readyz`,`/metrics`,`/debug/pprof/`,`/internal/providers` → 404 `NOT_FOUND`). `TestAuthFlowRegisterLoginRefreshLogout` (httptest + `auth.Service` di atas fakes): register 201 (Set-Cookie persis `kaiwa_refresh=…; Path=/api/v1/auth; HttpOnly; Secure; SameSite=Strict; Max-Age=2592000`, `Cache-Control: no-store`, golden body) → login 200 → refresh dengan cookie 200 (cookie baru, tanpa field `user`) → refresh cookie lama dalam 10 s → 409 `REFRESH_TOKEN_ROTATED` tanpa Set-Cookie → logout 204 cookie `Max-Age=0` → refresh → 401 `REFRESH_TOKEN_INVALID` + clear cookie. `TestRefreshOriginCheck` (Origin asing → 403; tanpa Origin ok). `TestLoginEmailRateLimited` (limiter menolak `auth_login_email` → 429, `Verify` tidak dipanggil). `TestForgotAlways202` (terdaftar, tak terdaftar, `auth_forgot_email` ditolak → body identik `{"status":"ACCEPTED"}`, FakeMailer hanya 1 untuk terdaftar). `TestResetPassword204` / `400 RESET_TOKEN_INVALID`. `TestTerms` (`{"terms_version":"2026-10-01"}`, `api_ip_default`). Me: `GET /me` golden; `PUT /me/consents` (`audio_consent` bukan bool → 400 `invalid_type`); `DELETE /me` 202 `{"status":"DELETION_SCHEDULED"}` + cookie clear; user lain/tanpa token 401; `AUTH_REFRESH_COOKIE_SECURE=false` menghilangkan atribut `Secure`.
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/httpapi -race -run 'RouteTable|InternalPaths|AuthFlow|RefreshOrigin|LoginEmail|Forgot|Reset|Terms|Me'` → Expected: FAIL.
- [ ] **Step 3: Implement**; handler tidak memanggil `json`/`WriteHeader` (semua lewat `Handle`); cookie lewat `req.SetCookie`; refresh: clear cookie bila `errs.CodeOf(err)==CodeRefreshTokenInvalid`, jangan clear pada `REFRESH_TOKEN_ROTATED`.
- [ ] **Step 4: Run tests to verify they pass** — perintah sama → PASS.
- [ ] **Step 5: Commit** — `feat(httpapi): router, auth and me routes, route table test`.

### Task 24: Rute admin & balance user

**Files:**
- Create: `internal/httpapi/{admin_routes.go,admin_handler.go,admin_dto.go,balance_routes.go,balance_handler.go,balance_dto.go}`, `testdata/api/rest/{admin,balance}/*.json`; Modify: `internal/httpapi/router.go`, `route_table_test.go` (`Ready=true` baris balance + admin M2)
- Test: `internal/httpapi/admin_http_test.go`, `internal/httpapi/balance_http_test.go`

**Interfaces:**
- Produces: `registerAdminRoutes(admin chi.Router, h *adminHandler)` (rute: `GET /admin/users`, `GET|PATCH /admin/users/{user_id}`, `POST …/balance-grants`, `POST …/balance-adjustments`, `GET …/balance/transactions`, `GET /admin/audit-logs`; policy `admin_user`), `registerBalanceRoutes(user chi.Router, h *balanceHandler)` (`GET /balance`, `GET /balance/transactions`; `api_user_default`). DTO: `adminUserResponse` (§5.9.8; `phone_number`/`deleted_at`/`purged_at`/`last_login_at` selalu ada), `ledgerEntryAdminResponse`, `ledgerEntryUserResponse` (hanya 9 field §4.7; **tanpa** `reason`,`admin_user_id`,`idempotency_key`), `balanceResponse`, `auditLogResponse`; `Idempotent-Replayed: true` + `SetStatus(200)` pada replay.

- [ ] **Step 1: Write the failing test** — `TestAdminRBAC` (tanpa token 401 `AUTH_REQUIRED`; token USER → 403 `FORBIDDEN`; ADMIN ok); `TestAdminListUsersValidation` (`email=ab` → 400 `VALIDATION_FAILED`; `status=BOGUS`/`role=BOGUS`/`limit=101` → 400; `email=han` ok); `TestAdminGrant` (tanpa header → 400 `IDEMPOTENCY_KEY_REQUIRED`; header bukan UUID → `IDEMPOTENCY_KEY_INVALID`; 201 golden `ledger_entry`+`balance`; ulang → 200 + `Idempotent-Replayed: true`; payload beda → 409 `IDEMPOTENCY_KEY_REUSED`; user tak ada → 404 `USER_NOT_FOUND`; `seconds:360001` → 400); `TestAdminAdjust` (−999 dari saldo 600 → 409 `BALANCE_ADJUSTMENT_NEGATIVE`; `seconds_delta:0` → 400); `TestAdminPatchStatus` (200 `AdminUser`; diri sendiri 409 `CANNOT_MODIFY_SELF`; reason 2 char → 400); `TestAdminAuditLogsPagination` (`next_cursor` + filter `action=BALANCE_GRANT`); `TestAdminUserTransactionsIncludeReason`; `TestBalanceGolden` (`allowed_durations`); `TestBalanceTransactionsUserShape` (key tepat 9 field; `entry_type=GRANT,CONSUME` ok, `entry_type=BOGUS` → 400; cursor + `limit`).
- [ ] **Step 2: Run it to verify it fails** — `go test ./internal/httpapi -race -run 'Admin|Balance|RouteTable'` → Expected: FAIL.
- [ ] **Step 3: Implement** + flip `Ready`; `PATCH`/`grants`/`adjustments` meneruskan `RequestMeta{RequestID, ClientIP, UserAgent}` ke service.
- [ ] **Step 4: Run tests to verify they pass** — `go test ./internal/httpapi -race` → PASS.
- [ ] **Step 5: Commit** — `feat(httpapi): admin and balance routes`.

### Task 25: Wiring `cmd/api`, `make promote-admin`, test gerbang e2e M2

**Files:**
- Modify: `cmd/api/main.go`, `.env.example`, `Makefile` (hanya bila target `promote-admin` belum ada; isi persis §3.12)
- Create: `internal/httpapi/m2_gate_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: semua task. Wiring manual (D-004): `postgres.NewStore` → `AuthStore/BalanceStore/AdminStore`; `redisstore.NewRateLimiter(rdb, ratelimit.NewLocal(...))`; `redisstore.NewTicketStore`; `mailer.New`; `auth.NewMailDispatcher`; `admin.Service` dilewatkan sebagai `auth.AccountDeleter`; `httpapi.NewRouter`. Env tambahan dibaca di `internal/platform/config` bila belum ada (nama §13.2.7; field = PascalCase nama env, mis. `AuthJWTSecret`) + `.env.example`; validasi startup §13.2.13 untuk `AUTH_JWT_SECRET`, `MAIL_MODE=log` di staging/prod, `AUTH_REFRESH_COOKIE_SECURE`.

- [ ] **Step 1: Write the failing test** — `TestM2GateAuthFlow` (PG+Redis testcontainers, fake clock): register→login→refresh→refresh(token lama pada +9 s → 409; token terbaru valid)→logout→refresh 401; `TestM2GateRefreshReuse` (+11 s → family revoked, token terbaru juga 401); `TestM2GateResetFlow` (forgot via FakeMailer → reset → refresh lama 401 → reset token kedua kali 400; email tak terdaftar → 202 identik & 0 mail); `TestM2GateConsent` (`audio_consent_updated_at` hanya berubah bila nilai berubah); `TestM2GateAdminGrant` (promosi admin lewat SQL yang sama dengan `make promote-admin`; USER → 403; grant 1800 → `GET /balance` available 1800 + `GET /balance/transactions` memuat `GRANT`; replay tidak menambah ledger; audit log tercatat untuk grant/adjust/status; `testutil.AssertReconciled` bersih); `TestM2GateDeleteAccount` (password salah 401; sukses 202 → login 401, token terhapus, reconcile bersih); `TestM2GateTicketTTL` (via `redisstore`). Cek build: `TestPromoteAdminTargetExists` menjalankan `make -n promote-admin EMAIL=x DB_URL=y` dan menemukan `UPDATE users SET role='ADMIN'`.
- [ ] **Step 2: Run it to verify it fails** — `go test -tags integration ./internal/httpapi -race -run M2Gate` → Expected: FAIL (wiring/rute belum dirangkai di test harness).
- [ ] **Step 3: Implement** wiring + harness test (`httptest.NewServer(httpapi.NewRouter(deps))` dengan komponen nyata); verifikasi `CGO_ENABLED=0 go build ./cmd/api`.
- [ ] **Step 4: Run tests to verify they pass** — `go test -tags integration ./internal/httpapi -race -run M2Gate` → PASS; `make arch-check` → sukses.
- [ ] **Step 5: Commit** — `feat(api): wire M2 identity, auth, admin, balance endpoints`.

---

## Gerbang milestone

- [ ] `make lint` → 0 issue (depguard: `auth/balance/admin` tanpa impor adapter; `httpapi` tanpa pgx/go-redis).
- [ ] `make test` → hijau (`-race`; unit Bab 3 §3.15: password, normalisasi, PHC, JWT, rotasi/reuse/grace, forgot/reset, consent, hapus akun dengan fake; Bab 4: grant/adjust idempotent, adjust negatif).
- [ ] `make test-integration` → hijau (repo suite PG, ledger append-only, grant paralel, ticket GETDEL/TTL, limiter Lua, `TestM2Gate*`).
- [ ] `go test ./internal/httpapi -race -run 'AuthFlow|RouteTable|WriteErrorGolden'` → PASS (register→login→refresh→logout via httptest, rute sesuai §5.8.5 untuk baris M2, golden error semua kode §5.10).
- [ ] `TestM2GateAdminGrant` → saldo & ledger berubah oleh grant admin dan `AssertReconciled` (R1–R5 §4.8) = 0 baris.
- [ ] `make arch-check` dan `make sqlc-check migrations-check` → bersih.
- [ ] Hasil + catatan penyimpangan masuk `docs/implementation-log.md` / `implementation-deviations.md`.

## Self-Review

**Cakupan spec:** §3.1–§3.15 (kecuali endpoint `POST /sessions/{id}/realtime-ticket` dan cek `TERMS_NOT_ACCEPTED` di `POST /sessions` → M3; job `account_purge`/retensi token → M6; kolom `learner_profiles` n/a). Bab 4: §4.2, §4.3.1/4.3.3, §4.6.6/4.6.7, §4.7, §4.8 (helper test; job `balance_reconcile` → M6), §4.9 butir grant/adjust/append-only; **tidak** dicakup: reservasi/settle/`ComputeCharge`/§4.5/§4.6.1–4.6.3 (M3). §5.8 lengkap (empat grup, `Handle`, `Page`, `WriteError`, `TestRouteTable`), §5.9.1/5.9.2/5.9.5/5.9.8 (admin sesi/turns/events/konten → M3). §13.4.3, §13.8.2, §13.8.6.
**Konsistensi tipe:** `Repository` balance M2 adalah subset yang ditambah-saja oleh M3; `admin.SessionGate` mengganti kebutuhan `session.Tx` di `admin.Tx` sampai M3 (M3 mengimplementasikan `CancelReserved` via `session.Service.CancelInTx`, tidak mengubah port); `ratelimit.Limiter` dipakai `httpapi` & (M5) `wsgateway`.
**Kontrak untuk milestone lain:** `auth.Principal`, `auth.TokenIssuer`, `auth.TicketService.Issue`, `auth.TicketStore`/`redisstore.NewTicketStore` (M5 memakai `Consume`), `auth.UserService.RequireActive`/`CurrentTermsVersion`, `balance.Store/Tx/Repository/Movement/OpenAccount`, `balance.Service.GrantTx`, `admin.Service.DeleteAccount`, `httpapi.Handle/HandleNoBody/Page/Request/WriteError/NewRouter/Deps`, `postgres.Store.{Auth,Balance,Admin}Store()`, `testutil.AssertReconciled`, `fakes.NewBalanceStore`.
**Ambiguitas & keputusan:** (1) `Handle` tak bisa set cookie/header/status → `Request.Set*`; (2) lokasi `Policy/Limiter` kontradiktif → `internal/platform/ratelimit`; (3) `admin.Tx` meng-embed `session.Tx` yang belum ada → `SessionGate` + `CancelReserved` stub sampai M3; (4) `X-Request-ID`: §3.7 (regex) vs §13.8.2 (UUID) → UUID; (5) revoke reuse-detection harus ter-commit walau error → pola commit-then-error; (6) `ListLedger` spec tanpa filter tipe → param ditambah; (7) rate limit berbasis email butuh body → dipanggil di handler; (8) sumber `common_passwords.txt` tak ditentukan → SecLists 10k; (9) SMTP nyata tak diuji otomatis; (10) jalur "ditemukan" `TicketSessionReader`/`OpenSessions` baru bisa diuji integration di M3 (butuh seed sesi).
