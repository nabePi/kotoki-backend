#!/usr/bin/env bash
# Sesi e2e PROVIDER_MODE=real skala kecil (M9-T17, MANUAL GATE: kunci API vendor + voice ID nyata).
#
# Menyalakan Postgres/Redis/RabbitMQ (docker compose), migrasi + seed, bin/api dan bin/realtime
# dengan PROVIDER_MODE=real, lalu satu sesi `kotoki-cli converse` per karakter (default HARUKA =
# ELEVENLABS dan AOI = CARTESIA, lihat seed/0050_characters.sql) memakai rekaman penutur asli
# testdata/audio/ja_self_intro.wav.
#
# Lingkungan (nilai kunci tidak pernah dicetak):
#   OPENAI_API_KEY        wajib (embedding selalu OpenAI)
#   ELEVENLABS_API_KEY    wajib (TTS ElevenLabs enabled)
#   CARTESIA_API_KEY      wajib (TTS Cartesia enabled)
#   DEEPSEEK_API_KEY      wajib hanya bila E2E_LLM=DEEPSEEK
#   E2E_STT=OPENAI|ELEVENLABS|CARTESIA   vendor STT (tepat satu flag true; default OPENAI)
#   E2E_LLM=OPENAI|DEEPSEEK              vendor LLM+Evaluation (default OPENAI)
#   E2E_CHARACTERS="HARUKA AOI"          kode karakter, satu sesi masing-masing
#   OUT_DIR=out/e2e-real                 keluaran: $OUT_DIR/<karakter huruf kecil>/{turn-*-gen-*.wav,summary.json}
#   E2E_ALLOW_RESET=1                    izinkan menimpa stack compose yang sudah berjalan (down -v saat selesai)
#   E2E_KEEP=1                           jangan `docker compose down -v` saat selesai
# Model, endpoint, dan sakelar lain dibaca dari .env (bila ada) atau default config.
# Voice ID nyata: seed/env/local/0900_voice_ids.sql (tidak di-commit; contoh: seed/env/example/).
#
# Exit: 0 lulus, 1 gagal, 2 prasyarat hilang (kunci/berkas/alat), 3 menolak menimpa stack yang berjalan.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

# .env hanya untuk kunci/model operator; proses anak tetap mendapat nilai eksplisit di bawah.
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

E2E_STT="${E2E_STT:-OPENAI}"
E2E_LLM="${E2E_LLM:-OPENAI}"
E2E_CHARACTERS="${E2E_CHARACTERS:-HARUKA AOI}"
OUT_DIR="${OUT_DIR:-out/e2e-real}"
WAV="testdata/audio/ja_self_intro.wav"
VOICE_SQL="seed/env/local/0900_voice_ids.sql"

die() { echo "e2e-real: $1" >&2; exit "${2:-1}"; }

# ---- prasyarat (sebelum menyentuh docker) -------------------------------------------------
case "$E2E_STT" in OPENAI | ELEVENLABS | CARTESIA) ;; *) die "E2E_STT harus OPENAI, ELEVENLABS, atau CARTESIA" 2 ;; esac
case "$E2E_LLM" in OPENAI | DEEPSEEK) ;; *) die "E2E_LLM harus OPENAI atau DEEPSEEK" 2 ;; esac

required=(OPENAI_API_KEY ELEVENLABS_API_KEY CARTESIA_API_KEY "${E2E_STT}_API_KEY" "${E2E_LLM}_API_KEY")
missing=""
for name in "${required[@]}"; do
  case " $missing " in *" $name "*) continue ;; esac
  [ -n "${!name:-}" ] || missing="$missing $name"
done
if [ -n "$missing" ]; then
  die "variabel lingkungan wajib kosong:$missing (isi di .env atau shell; nilai tidak dicetak)" 2
fi

[ -f "$WAV" ] || die "rekaman penutur asli $WAV belum ada (MANUAL GATE; testdata/audio/synth/ tidak dipakai)" 2
[ -f "$VOICE_SQL" ] || die "$VOICE_SQL belum ada: mode real menolak voice ID PLACEHOLDER_ (salin dari seed/env/example/0900_voice_ids.sql dan isi voice ID nyata; berkas ini tidak di-commit)" 2
for tool in jq curl make; do
  command -v "$tool" >/dev/null 2>&1 || die "alat '$tool' tidak ditemukan" 2
done
if command -v docker >/dev/null 2>&1; then ENGINE=docker; elif command -v podman >/dev/null 2>&1; then ENGINE=podman; else die "docker/podman tidak ditemukan" 2; fi
COMPOSE=("$ENGINE" compose)

if [ -n "$("${COMPOSE[@]}" ps -q 2>/dev/null || true)" ] && [ "${E2E_ALLOW_RESET:-}" != "1" ]; then
  die "stack compose lokal sedang berjalan; skrip ini menjalankan 'down -v' saat selesai (data lokal hilang). Hentikan stack, atau set E2E_ALLOW_RESET=1 / E2E_KEEP=1" 3
fi

# ---- cleanup ------------------------------------------------------------------------------
WORK="$(mktemp -d "${TMPDIR:-/tmp}/kotoki-e2e-real.XXXXXX")"
PIDS=()
COMPOSE_UP=0
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
  done
  if [ "$rc" -ne 0 ] && [ -d "$WORK" ]; then
    for f in api realtime; do
      [ -f "$WORK/$f.log" ] && { echo "--- $f.log (30 baris terakhir) ---" >&2; tail -n 30 "$WORK/$f.log" >&2; }
    done
  fi
  if [ "$COMPOSE_UP" = 1 ] && [ "${E2E_KEEP:-}" != "1" ]; then
    "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT INT TERM

# ---- infrastruktur ------------------------------------------------------------------------
COMPOSE_UP=1
"${COMPOSE[@]}" up -d --wait
export KOTOKI_ENV=local
make migrate-up seed

make build

# ---- proses real --------------------------------------------------------------------------
API_ADDR=127.0.0.1:18080 API_OPS=127.0.0.1:19090
RT_ADDR=127.0.0.1:18081 RT_OPS=127.0.0.1:19091
ORIGIN=http://localhost:3000
flag() { [ "$1" = "$2" ] && echo true || echo false; }

launch() { # launch <nama> <biner> <http_addr> <ops_addr>
  (
    export DB_URL="${DB_URL:-postgres://kotoki:kotoki@localhost:5432/kotoki?sslmode=disable}"
    export REDIS_URL="${REDIS_URL:-redis://localhost:6379/0}"
    export APP_ENV=local LOG_FORMAT=json LOG_LEVEL=info LOG_TRANSCRIPTS=false
    export PROVIDER_MODE=real INITIAL_BALANCE_SECONDS=3600
    export WS_ALLOW_EMPTY_ORIGIN=true WS_ALLOWED_ORIGINS="$ORIGIN" CORS_ALLOWED_ORIGINS="$ORIGIN"
    export OPENAI_STT_ENABLED="$(flag "$E2E_STT" OPENAI)" ELEVENLABS_STT_ENABLED="$(flag "$E2E_STT" ELEVENLABS)" CARTESIA_STT_ENABLED="$(flag "$E2E_STT" CARTESIA)"
    export OPENAI_LLM_ENABLED="$(flag "$E2E_LLM" OPENAI)" DEEPSEEK_LLM_ENABLED="$(flag "$E2E_LLM" DEEPSEEK)"
    export ELEVENLABS_TTS_ENABLED=true CARTESIA_TTS_ENABLED=true OPENAI_EMBEDDING_ENABLED=true
    export HTTP_ADDR="$3" OPS_ADDR="$4"
    exec "$2" >"$WORK/$1.log" 2>&1
  ) &
  PIDS+=("$!")
}

wait_ready() { # wait_ready <nama> <url>
  local i
  for i in $(seq 1 60); do
    curl -fsS "$2" >/dev/null 2>&1 && return 0
    sleep 1
  done
  die "$1 tidak siap di $2 dalam 60 s" 1
}

launch api bin/api "$API_ADDR" "$API_OPS"
launch realtime bin/realtime "$RT_ADDR" "$RT_OPS"
wait_ready api "http://$API_OPS/readyz"
wait_ready realtime "http://$RT_OPS/readyz"

# ---- sesi ---------------------------------------------------------------------------------
export KOTOKI_CLI_API_URL="http://$API_ADDR" KOTOKI_CLI_WS_URL="ws://$RT_ADDR/api/v1/realtime" KOTOKI_CLI_ORIGIN="$ORIGIN"
export XDG_CONFIG_HOME="$WORK/cfg"
EMAIL="e2e-real-$(date +%s)-$RANDOM@example.test"
PASSWORD="Kotoki-e2e-Real-2026!"
bin/kotoki-cli register --email "$EMAIL" --password "$PASSWORD" 2>&1 | tee -a "$WORK/cli.log"
bin/kotoki-cli login --email "$EMAIL" --password "$PASSWORD" 2>&1 | tee -a "$WORK/cli.log"

for code in $E2E_CHARACTERS; do
  dir="$OUT_DIR/$(echo "$code" | tr '[:upper:]' '[:lower:]')"
  rm -rf "$dir"
  mkdir -p "$dir"
  echo "== sesi $code (STT=$E2E_STT LLM=$E2E_LLM) =="
  bin/kotoki-cli converse --character "$code" --wav "$WAV" --duration 300 --end after-last \
    --ai-timeout 60s --out-dir "$dir" 2>&1 | tee -a "$WORK/cli.log"
  compgen -G "$dir/turn-*-gen-*.wav" >/dev/null || die "$code: tidak ada $dir/turn-*-gen-*.wav" 1
  [ "$(jq '.turn_latency_ms | length' "$dir/summary.json")" -ge 1 ] || die "$code: summary.json tanpa turn_latency_ms" 1
  echo "$code: turn_latency_ms=$(jq -c '.turn_latency_ms' "$dir/summary.json") (dengarkan $dir/turn-001-gen-1.wav dan catat verdict)"
done

# ---- kunci tidak boleh bocor ke log/keluaran ----------------------------------------------
for name in OPENAI_API_KEY ELEVENLABS_API_KEY CARTESIA_API_KEY DEEPSEEK_API_KEY; do
  [ -n "${!name:-}" ] || continue
  if grep -Fq -f <(printf '%s' "${!name}") "$WORK/api.log" "$WORK/realtime.log" "$WORK/cli.log"; then
    die "nilai $name ditemukan di log (kebocoran rahasia)" 1
  fi
done
echo "e2e-real: OK"
