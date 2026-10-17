#!/usr/bin/env bash
# Mengekstrak .spec PrometheusRule (grup aturan murni) ke out/rules.yaml untuk promtool
# (check rules / test rules). Gagal bila berkas tidak ada atau tidak berisi grup.
set -euo pipefail
HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HACK/../../.." && pwd)"
YQ="${YQ:-$ROOT/bin/tools/yq}"
SRC="${1:-$ROOT/deploy/k8s/base/observability/prometheusrule.yaml}"
OUT="${2:-$ROOT/out/rules.yaml}"
[ -x "$YQ" ] || { echo "extract-rules: yq tidak ada di $YQ; jalankan 'make k8s-tools'" >&2; exit 1; }
[ -s "$SRC" ] || { echo "extract-rules: $SRC tidak ada atau kosong" >&2; exit 1; }
mkdir -p "$(dirname "$OUT")"
"$YQ" e '.spec' "$SRC" >"$OUT.tmp"
n="$("$YQ" e '[.groups[].rules[]] | length' "$OUT.tmp")"
[ "${n:-0}" -gt 0 ] || { rm -f "$OUT.tmp"; echo "extract-rules: tidak ada aturan di $SRC" >&2; exit 1; }
mv "$OUT.tmp" "$OUT"
echo "extract-rules: $n aturan -> ${OUT#"$ROOT"/}"
