#!/usr/bin/env bash
# Checksum data Secret aplikasi -> anotasi kotoki.io/secrets-checksum pada pod template semua Deployment, sehingga
# rotasi secret (ESO menyegarkan Secret) memicu rollout (spec §13.8.4). Nilai Secret tidak pernah dicetak:
# hanya sha256 gabungan (nama secret, lalu pasangan kunci=nilai berurutan kunci) yang dipakai.
#   secrets-checksum.sh <staging|prod>        env: KUBECTL, NAMESPACE (kotoki)
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS="${NAMESPACE:-kotoki}"
case "${1:-}" in staging|prod) ;; *) echo "pemakaian: secrets-checksum.sh <staging|prod>" >&2; exit 2 ;; esac
SECRETS="kotoki-api-secrets kotoki-keda-secrets kotoki-migrate-secrets kotoki-realtime-secrets kotoki-worker-secrets"
DEPLOYMENTS="api realtime worker-core worker-evaluation worker-memory worker-learner"

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }

# go-template mengiterasi map berurutan kunci, jadi keluarannya deterministik.
sum="$(
  for s in $SECRETS; do
    printf '%s\n' "$s"
    "$KUBECTL" -n "$NS" get secret "$s" -o go-template='{{range $k, $v := .data}}{{$k}}={{$v}}{{"\n"}}{{end}}'
  done | sha
)"
[[ "$sum" =~ ^[0-9a-f]{64}$ ]] || { echo "secrets-checksum: checksum tidak valid" >&2; exit 1; }

for d in $DEPLOYMENTS; do
  "$KUBECTL" -n "$NS" patch deployment "kotoki-$d" --type merge \
    -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"kotoki.io/secrets-checksum\":\"$sum\"}}}}}"
done
echo "secrets-checksum: $1 ${sum:0:12}"
