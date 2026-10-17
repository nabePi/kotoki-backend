#!/usr/bin/env bash
# Deploy satu lingkungan (M10 Task 12, spec §14.12.2). Urutan tetap, berhenti di kegagalan pertama:
#   migrate (hapus Job lama, apply, wait complete 600s) -> seed -> apply sisa -> secrets-checksum
#   -> rollout status tiap Deployment -> smoke (hanya staging, bila SMOKE_EMAIL diisi).
#
#   deploy.sh staging <git-sha>                  image bertag SHA
#   deploy.sh prod    <git-sha> <digests-dir>    image berdigest: <digests-dir>/<image>.digest (sha256:<64 hex>)
#
# Env: KUBECTL, KUSTOMIZE (default bin/tools/kustomize), KOTOKI_CLI (default bin/kotoki-cli), NAMESPACE (kotoki),
#      SMOKE_EMAIL/SMOKE_PASSWORD/SMOKE_API_URL/SMOKE_WS_URL/SMOKE_ORIGIN (smoke staging; rahasia dari environment).
# Bekerja di salinan sementara deploy/k8s: pohon kerja tidak pernah diubah. Tidak ada yang dicetak dari rahasia.
set -euo pipefail

HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HACK/../../.." && pwd)"
KUBECTL="${KUBECTL:-kubectl}"
KUSTOMIZE="${KUSTOMIZE:-$ROOT/bin/tools/kustomize}"
KOTOKI_CLI="${KOTOKI_CLI:-$ROOT/bin/kotoki-cli}"
NS="${NAMESPACE:-kotoki}"
REGISTRY="ghcr.io/nabepi/kotoki-backend"
IMAGES="api realtime worker migrate seed"
DEPLOYMENTS="api realtime worker-core worker-evaluation worker-memory worker-learner"

usage() { echo "pemakaian: deploy.sh staging <git-sha> | deploy.sh prod <git-sha> <digests-dir>" >&2; exit 2; }

[ "$#" -ge 2 ] || usage
ENV_NAME="$1"; SHA="$2"; DIGESTS="${3:-}"
case "$ENV_NAME" in staging|prod) ;; *) usage ;; esac
[[ "$SHA" =~ ^[0-9a-f]{40}$ ]] || { echo "deploy: <git-sha> harus 40 hex, bukan '$SHA'" >&2; exit 2; }

# Validasi seluruh input sebelum menyentuh klaster.
declare -A DIGEST
if [ "$ENV_NAME" = prod ]; then
  [ -n "$DIGESTS" ] && [ -d "$DIGESTS" ] || { echo "deploy: prod butuh <digests-dir>" >&2; exit 2; }
  for i in $IMAGES; do
    f="$DIGESTS/$i.digest"
    [ -s "$f" ] || { echo "deploy: $f tidak ada" >&2; exit 2; }
    d="$(tr -d '[:space:]' <"$f")"
    [[ "$d" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "deploy: digest $i tidak valid" >&2; exit 2; }
    DIGEST[$i]="$d"
  done
elif [ -n "$DIGESTS" ]; then
  echo "deploy: staging memakai tag SHA; <digests-dir> hanya untuk prod" >&2; exit 2
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cp -R "$ROOT/deploy/k8s" "$WORK/k8s"
OVERLAY="$WORK/k8s/overlays/$ENV_NAME"
[ -d "$OVERLAY" ] || { echo "deploy: overlay $ENV_NAME tidak ada" >&2; exit 2; }

# Label versi (base) dan image (overlay) diisi di salinan.
while IFS= read -r f; do
  sed -e "s/app.kubernetes.io\/version: REPLACE_SHA/app.kubernetes.io\/version: $SHA/" "$f" >"$f.new" && mv "$f.new" "$f"
done < <(grep -rl 'app.kubernetes.io/version: REPLACE_SHA' "$WORK/k8s" || true)
for i in $IMAGES; do
  if [ "$ENV_NAME" = prod ]; then ref="$REGISTRY/$i@${DIGEST[$i]}"; else ref="$REGISTRY/$i:$SHA"; fi
  (cd "$OVERLAY" && "$KUSTOMIZE" edit set image "$REGISTRY/$i=$ref")
done
RENDERED="$WORK/rendered.yaml"
"$KUSTOMIZE" build "$OVERLAY" >"$RENDERED"

k() { "$KUBECTL" -n "$NS" "$@"; }

# STAGE migrate
echo "== migrate"
k delete job kotoki-migrate --ignore-not-found --wait=true
k apply -f "$RENDERED" -l app.kubernetes.io/component=migrate
if ! k wait --for=condition=complete job/kotoki-migrate --timeout=600s; then
  echo "deploy: migrate gagal atau melebihi 600s; log:" >&2
  k logs job/kotoki-migrate --tail=50 >&2 || true
  exit 1
fi

# STAGE seed
echo "== seed"
k delete job kotoki-seed --ignore-not-found --wait=true
k apply -f "$RENDERED" -l app.kubernetes.io/component=seed
if ! k wait --for=condition=complete job/kotoki-seed --timeout=300s; then
  echo "deploy: seed gagal atau melebihi 300s; log:" >&2
  k logs job/kotoki-seed --tail=50 >&2 || true
  exit 1
fi

# STAGE apply
echo "== apply"
k apply -f "$RENDERED" -l 'app.kubernetes.io/component notin (migrate,seed)'
KUBECTL="$KUBECTL" NAMESPACE="$NS" "$HACK/secrets-checksum.sh" "$ENV_NAME"

# STAGE rollout
echo "== rollout"
for d in $DEPLOYMENTS; do
  k rollout status "deployment/kotoki-$d" --timeout=600s
done

# STAGE smoke
if [ "$ENV_NAME" = staging ] && [ -n "${SMOKE_EMAIL:-}" ]; then
  echo "== smoke"
  : "${SMOKE_PASSWORD:?SMOKE_PASSWORD wajib bila SMOKE_EMAIL diisi}"
  gl=()
  [ -n "${SMOKE_API_URL:-}" ] && gl+=(--api-url "$SMOKE_API_URL")
  [ -n "${SMOKE_WS_URL:-}" ] && gl+=(--ws-url "$SMOKE_WS_URL")
  [ -n "${SMOKE_ORIGIN:-}" ] && gl+=(--origin "$SMOKE_ORIGIN")
  "$KOTOKI_CLI" ${gl[@]+"${gl[@]}"} login --email "$SMOKE_EMAIL" --password "$SMOKE_PASSWORD" >/dev/null
  "$KOTOKI_CLI" ${gl[@]+"${gl[@]}"} converse --wav "$ROOT/testdata/audio/synth/ja_greeting_short.wav" --quiet
fi
echo "deploy: $ENV_NAME $SHA selesai"
