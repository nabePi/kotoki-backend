#!/usr/bin/env bash
# Gagal bila placeholder tertinggal (M10 Task 11; dilengkapi Task 23).
#   pins-check.sh          mode default: tidak ada `<PIN>` di deploy/ dan .github/ (lulus rc 0 sekarang).
#   pins-check.sh --prod   ditambah: render staging dan prod tidak boleh memuat placeholder MANUAL GATE:
#                            REPLACE_*              (image tag/label versi, host SMTP, CIDR data store, ekspresi
#                                                    metrik cloud, keputusan vendor provider, ...)
#                            192.0.2.*              (CIDR data store TEST-NET-1 di base)
#                            198.51.100.*           (CIDR proxy tepercaya TEST-NET-2 di overlay)
#                            sha256:000...0         (digest image nol)
#                          Mode ini SENGAJA gagal sampai MANUAL GATE (Task 24) mengisi nilai nyata.
# Render dibuat dengan bin/tools/kustomize; PINS_RENDER_DIR=<dir> memakai <dir>/<env>.yaml siap pakai
# (dipakai selftest.sh). Keluaran hanya menyebut berkas/baris placeholder, tidak pernah nilai rahasia.
set -uo pipefail

HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HACK/../../.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-$ROOT/bin/tools/kustomize}"
ENVS="${PINS_ENVS:-staging prod}"

prod=0
case "${1:-}" in
  "") ;;
  --prod) prod=1 ;;
  *) echo "pemakaian: pins-check.sh [--prod]" >&2; exit 2 ;;
esac

rc=0

# 1) <PIN> di mana pun di artefak deploy (pin alat/versi yang belum diisi). Skrip ini dan selftest dikecualikan
#    karena menyebut literalnya.
dirs=()
for d in "$ROOT/deploy" "$ROOT/.github"; do [ -d "$d" ] && dirs+=("$d"); done
if [ "${#dirs[@]}" -gt 0 ]; then
  hits="$(grep -rIn --exclude=pins-check.sh --exclude=selftest.sh '<PIN>' "${dirs[@]}" 2>/dev/null || true)"
  if [ -n "$hits" ]; then
    echo "pins-check: <PIN> tertinggal:" >&2
    printf '%s\n' "$hits" | sed "s#$ROOT/##" >&2
    rc=1
  fi
fi

# 2) placeholder di render staging/prod (hanya --prod)
if [ "$prod" -eq 1 ]; then
  PATTERN='REPLACE_|192\.0\.2\.|198\.51\.100\.|sha256:0{64}'
  for e in $ENVS; do
    if [ -n "${PINS_RENDER_DIR:-}" ]; then
      f="$PINS_RENDER_DIR/$e.yaml"
      [ -s "$f" ] || { echo "pins-check: $f tidak ada atau kosong" >&2; rc=1; continue; }
    else
      [ -x "$KUSTOMIZE" ] || { echo "pins-check: kustomize tidak ada; jalankan 'make k8s-tools'" >&2; exit 1; }
      f="$(mktemp)"
      trap 'rm -f "$f"' EXIT
      if ! "$KUSTOMIZE" build "$ROOT/deploy/k8s/overlays/$e" >"$f" 2>/dev/null; then
        echo "pins-check: kustomize build overlays/$e gagal" >&2; rc=1; continue
      fi
    fi
    found="$(grep -nE "$PATTERN" "$f" || true)"
    if [ -n "$found" ]; then
      n="$(printf '%s\n' "$found" | wc -l | tr -d ' ')"
      echo "pins-check: overlay $e memuat $n baris placeholder (MANUAL GATE belum selesai):" >&2
      printf '%s\n' "$found" | sed -E 's/^([0-9]+):[[:space:]]*/  baris \1: /' | head -40 >&2
      [ "$n" -le 40 ] || echo "  ... ($((n - 40)) baris lagi)" >&2
      rc=1
    else
      echo "pins-check: overlay $e bersih"
    fi
  done
fi

if [ "$rc" -eq 0 ]; then echo "pins-check: OK$([ "$prod" -eq 1 ] && echo ' (--prod)')"; fi
exit "$rc"
