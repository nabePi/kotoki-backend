#!/bin/sh
# Job seed (spec §13.10 / M10 Task 5): jalankan $SEED_DIR/*.sql lalu $SEED_DIR/env/$KOTOKI_ENV/*.sql
# (opsional; mis. 0900_voice_ids.sql, D-014), masing-masing berurutan nama, berhenti pada kegagalan
# pertama. Seed idempoten; DB_MIGRATE_URL dibaca dari env (Secret kotoki-migrate-secrets) dan tidak dicetak.
set -eu
LC_ALL=C
export LC_ALL

: "${DB_MIGRATE_URL:?DB_MIGRATE_URL wajib diisi}"
: "${KOTOKI_ENV:?KOTOKI_ENV wajib diisi}"
SEED_DIR="${SEED_DIR:-/seed}"

run_dir() {
	for f in "$1"/*.sql; do
		[ -f "$f" ] || continue
		echo "seed $f"
		psql "$DB_MIGRATE_URL" -v ON_ERROR_STOP=1 -f "$f"
	done
}

run_dir "$SEED_DIR"
run_dir "$SEED_DIR/env/$KOTOKI_ENV"
