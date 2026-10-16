#!/usr/bin/env bash
# kubeconform strict atas keluaran `make k8s-build` (out/k8s/<env>.yaml).
# Pemakaian: validate.sh [env...]   (default: dev staging prod)
# Kind CRD yang tidak ada di katalog datreeio masuk SKIP_KINDS (dan dicatat di
# docs/implementation-deviations.md); jangan menambah kind di sini tanpa mencatatnya.
set -euo pipefail

HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HACK/../../.." && pwd)"
KUBECONFORM="${KUBECONFORM:-$ROOT/bin/tools/kubeconform}"
OUT="${K8S_RENDER_DIR:-$ROOT/out/k8s}"
CRD_CATALOG='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
SKIP_KINDS="${SKIP_KINDS:-}"

[ -x "$KUBECONFORM" ] || { echo "validate: kubeconform tidak ada; jalankan 'make k8s-tools'" >&2; exit 1; }
envs=("$@"); [ "${#envs[@]}" -gt 0 ] || envs=(dev staging prod)

args=(-strict -summary -ignore-missing-schemas=false
      -schema-location default -schema-location "$CRD_CATALOG")
[ -z "$SKIP_KINDS" ] || args+=(-skip "$SKIP_KINDS")

rc=0
for e in "${envs[@]}"; do
  f="$OUT/$e.yaml"
  if [ ! -s "$f" ]; then
    echo "validate: $f tidak ada atau kosong; jalankan 'make k8s-build'" >&2
    rc=1; continue
  fi
  echo "== kubeconform $e"
  "$KUBECONFORM" "${args[@]}" "$f" || rc=1
done
exit "$rc"
