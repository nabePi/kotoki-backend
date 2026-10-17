#!/usr/bin/env bash
# G1 lokal (M10-T19, spec 13.12): gate g1-local terhadap stack docker compose + biner lokal, lalu
# kotoki-verify dan laporan docs/loadtest/<tanggal>-g1-local.md.
#
#   make load-local                         # 100 VU, ramp 1 m, hold 5 m (gate g1-local)
#   make load-local ARGS="--vus 50 --hold 2m"   # argumen tambahan diteruskan ke `kotoki-load run`
#
# Urutan: compose up (proyek tersendiri) -> migrate-up + seed -> build -> bin/api, bin/realtime,
# bin/worker -> /readyz (<= 60 s) -> admin seed + promote-admin -> kotoki-load seed-users ->
# kotoki-load run (+ metric-contract, + kotoki-verify --await-evaluations) -> laporan -> matikan proses.
#
# Lingkungan:
#   KOTOKI_LOAD_PROJECT   nama proyek compose (default kotoki-g1local). Skrip menolak berjalan bila proyek
#                         itu sudah punya kontainer, dan HANYA proyek ini yang di-`down -v` saat selesai;
#                         proyek `kotoki` (stack dev Anda) tidak disentuh.
#   KOTOKI_LOAD_KEEP=1    jangan `down -v` saat selesai
#   SEED_COUNT            jumlah user load (default 110; harus >= --vus)
#   REPORT_DATE           tanggal di nama laporan (default hari ini, UTC)
#   AWAIT_EVALUATIONS     tunggu evaluasi sesudah run (default 5m, K13)
# Kata sandi admin dibuat acak per run, hanya hidup di lingkungan proses ini (KOTOKI_ADMIN_PASSWORD),
# tidak pernah di argv atau log.
#
# Exit: 0 gate lulus, 1 gate gagal, 2 prasyarat/setup gagal, 3 menolak menimpa proyek compose yang berjalan.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

PROJECT="${KOTOKI_LOAD_PROJECT:-kotoki-g1local}"
SEED_COUNT="${SEED_COUNT:-110}"
AWAIT_EVALUATIONS="${AWAIT_EVALUATIONS:-5m}"
REPORT_DATE="${REPORT_DATE:-$(date -u +%F)}"
OUT_DIR="out/load"
RESULT="$OUT_DIR/g1-local.json"
REPORT="docs/loadtest/${REPORT_DATE}-g1-local.md"
GATE=g1-local

die() { echo "load-local: $1" >&2; exit "${2:-2}"; }

for tool in jq curl make openssl; do
  command -v "$tool" >/dev/null 2>&1 || die "alat '$tool' tidak ditemukan"
done
if command -v docker >/dev/null 2>&1; then ENGINE=docker; elif command -v podman >/dev/null 2>&1; then ENGINE=podman; else die "docker/podman tidak ditemukan"; fi
# Proyek compose tersendiri: env mengalahkan `name:` di docker-compose.yml, dan setiap target make yang
# memakai $(COMPOSE) mengikutinya. Jaringan migrate mengikuti nama proyek.
export COMPOSE_PROJECT_NAME="$PROJECT"
COMPOSE=("$ENGINE" compose)
MAKE_ARGS=("COMPOSE_NETWORK=${PROJECT}_default")

if [ -n "$("${COMPOSE[@]}" ps -aq 2>/dev/null || true)" ]; then
  die "proyek compose '$PROJECT' sudah punya kontainer; hentikan lebih dulu (docker compose -p $PROJECT down -v) atau pilih KOTOKI_LOAD_PROJECT lain" 3
fi

mkdir -p "$OUT_DIR/g1-local-logs" docs/loadtest
LOGS="$OUT_DIR/g1-local-logs"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/kotoki-load-local.XXXXXX")"
PIDS=()
COMPOSE_UP=0
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && wait "$pid" 2>/dev/null || true; done
  if [ "$rc" -ge 2 ]; then
    for f in api realtime worker; do
      [ -f "$LOGS/$f.log" ] && { echo "--- $f.log (30 baris terakhir) ---" >&2; tail -n 30 "$LOGS/$f.log" >&2; }
    done
  fi
  if [ "$COMPOSE_UP" = 1 ] && [ "${KOTOKI_LOAD_KEEP:-}" != "1" ]; then
    "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT INT TERM

# ---- infrastruktur -------------------------------------------------------------------------
COMPOSE_UP=1
"${COMPOSE[@]}" up -d --wait
export KOTOKI_ENV=local
make "${MAKE_ARGS[@]}" migrate-up
make "${MAKE_ARGS[@]}" seed
make build
mkdir -p bin
CGO_ENABLED=1 go build -o bin/ ./test/load/cmd/kotoki-load ./test/load/cmd/kotoki-verify
make fixtures16k >/dev/null

# ---- proses ----------------------------------------------------------------------------------
DB_URL="postgres://kotoki:kotoki@localhost:5432/kotoki?sslmode=disable"
API_URL="http://localhost:8080"
WS_URL="ws://localhost:8081/api/v1/realtime"
ORIGIN="http://localhost:3000"

launch() { # launch <nama> <biner> [argumen]
  (
    # Env eksplisit: tidak ada .env yang dibaca, tidak ada kunci vendor (PROVIDER_MODE=fake).
    export APP_ENV=local LOG_LEVEL=info LOG_FORMAT=json LOG_TRANSCRIPTS=false
    export DB_URL REDIS_URL="redis://localhost:6379/0" RABBITMQ_URL="amqp://kotoki:kotoki@localhost:5672/"
    export AUTH_JWT_SECRET="bG9jYWwtZGV2LW9ubHktMDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" AUTH_REFRESH_COOKIE_SECURE=false
    export INITIAL_BALANCE_SECONDS=3600 PRIVACY_POLICY_VERSION=2026-10-01
    export CORS_ALLOWED_ORIGINS="$ORIGIN" WS_ALLOWED_ORIGINS="$ORIGIN" WS_ALLOW_EMPTY_ORIGIN=true
    # Satu mesin = satu alamat: limiter per-IP (login 20/mnt, register 5/jam, ws_connect_ip 30/mnt) akan
    # menahan generator. Pengguna nyata punya alamat sendiri; kotoki-load --spread-ips meniru itu lewat
    # X-Forwarded-For dan server mempercayainya hanya dari loopback.
    export TRUSTED_PROXY_CIDRS="127.0.0.0/8,::1/128"
    export PROVIDER_MODE=fake FAKE_STT_MODE=energy
    export FAKE_PRIMARY_STT_LATENCY_MS=250 FAKE_PRIMARY_LLM_LATENCY_MS=500 FAKE_LLM_TOKEN_INTERVAL_MS=30 FAKE_PRIMARY_TTS_LATENCY_MS=300
    export ACCOUNT_PURGE_DELAY=1h WORKER_HTTP_ADDR=:9092
    exec "$2" ${3:+"$3"} >"$LOGS/$1.log" 2>&1
  ) &
  PIDS+=("$!")
}

wait_ready() { # wait_ready <nama> <url>
  local i
  for i in $(seq 1 60); do
    curl -fsS "$2" >/dev/null 2>&1 && return 0
    sleep 1
  done
  die "$1 tidak siap di $2 dalam 60 s" 2
}

launch api bin/api
launch realtime bin/realtime
launch worker bin/worker "--handlers=outbox,evaluation,memory,learner,summary,sweeper,cleanup"
wait_ready api "http://localhost:9090/readyz"
wait_ready realtime "http://localhost:9091/readyz"
wait_ready worker "http://localhost:9092/readyz"

# ---- admin seed -> promote -> user load ----------------------------------------------------------
ADMIN_EMAIL="load-admin-$(date +%s)@load.test"
KOTOKI_ADMIN_PASSWORD="$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)Aa1"
export KOTOKI_ADMIN_EMAIL="$ADMIN_EMAIL" KOTOKI_ADMIN_PASSWORD
TERMS="$(curl -fsS "$API_URL/api/v1/auth/terms" | jq -r .terms_version)"
# kata sandi lewat stdin, bukan argv
jq -n --arg e "$ADMIN_EMAIL" --arg p "$KOTOKI_ADMIN_PASSWORD" --arg t "$TERMS" \
  '{email:$e,password:$p,full_name:"Load Admin",terms_version:$t}' |
  curl -fsS -X POST "$API_URL/api/v1/auth/register" -H 'Content-Type: application/json' -H 'X-Forwarded-For: 10.255.255.254' \
    --data @- >/dev/null || die "register admin gagal"
make "${MAKE_ARGS[@]}" promote-admin EMAIL="$ADMIN_EMAIL" >/dev/null

USERS="$WORK/users.csv"
bin/kotoki-load seed-users --api-url "$API_URL" --count "$SEED_COUNT" --balance 36000 --spread-ips --out "$USERS"

# ---- gate ----------------------------------------------------------------------------------------
SINCE="$(date -u +%FT%TZ)"
rc=0
bin/kotoki-load run --gate "$GATE" --api-url "$API_URL" --ws-url "$WS_URL" --origin "$ORIGIN" \
  --users-file "$USERS" --fixtures testdata/audio --spread-ips \
  --metrics-urls http://localhost:9090,http://localhost:9091,http://localhost:9092 \
  --metrics-file deploy/k8s/hack/metrics.txt \
  --verify-db "$DB_URL" --await-evaluations "$AWAIT_EVALUATIONS" \
  --out "$RESULT" "$@" | tee "$OUT_DIR/g1-local.txt" || rc=$?
[ -f "$RESULT" ] || die "kotoki-load tidak menulis $RESULT (rc=$rc)"

# kotoki-verify mandiri (V1-V8) atas run yang sama; evaluasi sudah ditunggu di atas.
verify_rc=0
bin/kotoki-verify --db "$DB_URL" --since "$SINCE" --await-evaluations 1m | tee "$OUT_DIR/g1-local-verify.txt" || verify_rc=$?

# ---- laporan -------------------------------------------------------------------------------------
{
  echo "# G1 lokal ($REPORT_DATE)"
  echo
  echo "Gate \`$GATE\` terhadap docker compose (proyek \`$PROJECT\`) dan biner lokal; \`PROVIDER_MODE=fake\`, FAKE_STT_MODE=energy."
  echo "Dihasilkan oleh \`test/load/local/run.sh\` dari \`$RESULT\`. Angka di bawah dikutip apa adanya."
  echo
  jq -r '"- Verdict: **" + (if .pass then "PASS" else "FAIL" end) + "**\n- VU: \(.vus), ramp \(.ramp), hold \(.hold)\n- Mulai: \(.meta.started_at), selesai: \(.meta.finished_at)\n- Sesi di sisi klien: \(.client.sessions)"' "$RESULT"
  echo "- Mesin: $(uname -sm), $(sysctl -n hw.ncpu 2>/dev/null || nproc) CPU"
  echo "- kotoki-verify mandiri: rc=$verify_rc"
  echo
  echo "## Kriteria"
  echo
  echo "| ID | Kriteria | Hasil | Nilai | Ambang | Catatan |"
  echo "|---|---|---|---|---|---|"
  jq -r '.results[] | "| \(.id) | \(.name) | \(if .skipped then "SKIP" elif .pass then "PASS" else "FAIL" end) | \(if .skipped or .value == null then "-" else (.value|tostring) end) | `\(.threshold)` | \(.reason // "") |"' "$RESULT"
  echo
  echo "## Kontrak metrik (di luar K1-K15)"
  echo
  jq -r 'if .metric_contract == null then "Tidak dijalankan." elif .metric_contract.pass then "PASS: semua metrik di `deploy/k8s/hack/metrics.txt` tanpa `?` muncul di scrape akhir." else "FAIL: " + (.metric_contract.error // "") end' "$RESULT"
  echo
  echo "## Observasi mentah"
  echo
  echo '```json'
  jq '.observation' "$RESULT"
  echo '```'
  echo
  echo "## Ringkasan klien"
  echo
  echo "Ukuran sisi klien, bukan kriteria: K1 mengukur sisi server (histogram \`turn_end_to_first_audio\`); \`turn_p95_s\` klien memuat juga pacing dan jaringan lokal."
  echo
  echo '```json'
  jq '.client' "$RESULT"
  echo '```'
  if [ "$(jq '.errors | length' "$RESULT")" -gt 0 ]; then
    echo
    echo "## Galat pengumpulan"
    echo
    jq -r '.errors[] | "- " + .' "$RESULT"
  fi
} >"$REPORT"
echo "load-local: laporan $REPORT"

[ "$verify_rc" -eq 0 ] || [ "$rc" -ne 0 ] || rc=1
exit "$rc"
