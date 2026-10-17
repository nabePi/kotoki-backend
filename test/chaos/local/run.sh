#!/usr/bin/env bash
# Failure injection locally (M10-T21, spec 13.13): the stack of `make load-local` (docker compose project of
# its own + bin/api, bin/realtime, bin/worker with PROVIDER_MODE=fake), then `kotoki-chaos run` for each
# scenario named.
#
#   make chaos-local SCEN=F1 ARGS="--vus 20 --time-scale 0.1"
#   make chaos-local SCEN="F1 F2 F3" ARGS="--vus 20 --time-scale 0.1"
#
# Arguments that are scenario ids (F1..F16) select scenarios; every other argument goes to kotoki-chaos run
# (--vus, --time-scale, --inject-at, --revert-at, --tail, --await-evaluations ...). Only the scenarios that
# are LocalOK run here (kotoki-chaos list); F7, F9-F12 are MANUAL GATE (Task 24).
#
# Lingkungan:
#   KOTOKI_CHAOS_PROJECT  nama proyek compose (default kotoki-chaoslocal). Skrip menolak berjalan bila proyek itu
#                         sudah punya kontainer, dan HANYA proyek ini yang di-`down -v` saat selesai; proyek
#                         `kotoki` (stack dev) tidak disentuh. kotoki-chaos menolak perintah di luar proyek ini.
#   KOTOKI_CHAOS_KEEP=1   jangan `down -v` saat selesai
#   SEED_COUNT            jumlah user load (default 110; harus > --vus: user terakhir dipakai probe F16)
# Kata sandi admin dibuat acak per run, hanya hidup di lingkungan proses ini, tidak pernah di argv atau log.
#
# Exit: 0 semua skenario lulus, 1 ada yang gagal, 2 prasyarat/setup gagal, 3 menolak menimpa proyek compose.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

PROJECT="${KOTOKI_CHAOS_PROJECT:-kotoki-chaoslocal}"
SEED_COUNT="${SEED_COUNT:-110}"
OUT_DIR="out/chaos"

die() { echo "chaos-local: $1" >&2; exit "${2:-2}"; }

SCENS=()
PASS_ARGS=()
for a in "$@"; do
  if [[ "$a" =~ ^F[0-9]+$ ]]; then SCENS+=("$a"); else PASS_ARGS+=("$a"); fi
done
[ "${#SCENS[@]}" -gt 0 ] || die "tidak ada skenario; contoh: make chaos-local SCEN=F1" 2

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
  die "proyek compose '$PROJECT' sudah punya kontainer; hentikan lebih dulu (docker compose -p $PROJECT down -v) atau pilih KOTOKI_CHAOS_PROJECT lain" 3
fi

mkdir -p "$OUT_DIR/logs"
LOGS="$OUT_DIR/logs"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/kotoki-chaos-local.XXXXXX")"
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
  if [ "$COMPOSE_UP" = 1 ] && [ "${KOTOKI_CHAOS_KEEP:-}" != "1" ]; then
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
CGO_ENABLED=1 go build -o bin/ ./test/load/cmd/kotoki-load ./test/chaos/cmd/kotoki-chaos
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
    # limiter per-IP: kotoki-chaos/kotoki-load --spread-ips meniru pengguna dengan alamat sendiri lewat
    # X-Forwarded-For; server mempercayainya hanya dari loopback.
    export TRUSTED_PROXY_CIDRS="127.0.0.0/8,::1/128"
    export PROVIDER_MODE=fake FAKE_STT_MODE=energy
    export FAKE_PRIMARY_STT_LATENCY_MS=250 FAKE_PRIMARY_LLM_LATENCY_MS=500 FAKE_LLM_TOKEN_INTERVAL_MS=30 FAKE_PRIMARY_TTS_LATENCY_MS=300
    # fault runtime dibaca tiap 1 s (default spec 8.9.6)
    export FAKE_FAULTS_POLL_INTERVAL=1s
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
ADMIN_EMAIL="chaos-admin-$(date +%s)@load.test"
KOTOKI_ADMIN_PASSWORD="$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)Aa1"
export KOTOKI_ADMIN_EMAIL="$ADMIN_EMAIL" KOTOKI_ADMIN_PASSWORD
TERMS="$(curl -fsS "$API_URL/api/v1/auth/terms" | jq -r .terms_version)"
# kata sandi lewat stdin, bukan argv
jq -n --arg e "$ADMIN_EMAIL" --arg p "$KOTOKI_ADMIN_PASSWORD" --arg t "$TERMS" \
  '{email:$e,password:$p,full_name:"Chaos Admin",terms_version:$t}' |
  curl -fsS -X POST "$API_URL/api/v1/auth/register" -H 'Content-Type: application/json' -H 'X-Forwarded-For: 10.255.255.254' \
    --data @- >/dev/null || die "register admin gagal"
make "${MAKE_ARGS[@]}" promote-admin EMAIL="$ADMIN_EMAIL" >/dev/null

USERS="$WORK/users.csv"
bin/kotoki-load seed-users --api-url "$API_URL" --count "$SEED_COUNT" --balance 36000 --spread-ips --out "$USERS"

# ---- skenario ------------------------------------------------------------------------------------
worst=0
for id in "${SCENS[@]}"; do
  echo "=== chaos-local: $id ==="
  # tidak ada fault tersisa dari skenario sebelumnya
  "${COMPOSE[@]}" exec -T redis redis-cli del kotoki:fake:faults >/dev/null
  rc=0
  bin/kotoki-chaos run "$id" --env compose --compose-project "$PROJECT" \
    --api-url "$API_URL" --ws-url "$WS_URL" --origin "$ORIGIN" --users-file "$USERS" --fixtures testdata/audio \
    --metrics-urls http://localhost:9090,http://localhost:9091,http://localhost:9092 \
    --db "$DB_URL" --redis-url redis://localhost:6379/0 --load-bin bin/kotoki-load --out-dir "$OUT_DIR" \
    ${PASS_ARGS[@]+"${PASS_ARGS[@]}"} || rc=$?
  echo "=== chaos-local: $id exit $rc ==="
  [ "$rc" -le "$worst" ] || worst=$rc
done
exit "$worst"
