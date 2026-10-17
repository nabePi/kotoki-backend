#!/usr/bin/env bash
# Uji mandiri untuk lib.sh: expect/expect_absent harus lulus saat benar DAN gagal saat salah.
set -uo pipefail
HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HACK/../../.." && pwd)"
export K8S_RENDER_DIR="$(mktemp -d)"
trap 'rm -rf "$K8S_RENDER_DIR"' EXIT
cp "$HACK/testdata/deploy.yaml" "$K8S_RENDER_DIR/selftest.yaml"
# shellcheck source=lib.sh
source "$HACK/lib.sh"

bad=0
# run_case <want-rc: 0|nonzero> <want-FAIL-line: yes|no> <fn> args...
run_case() {
  local want="$1" wantline="$2" out rc
  shift 2
  out="$( ( "$@"; finish ) 2>&1 )"; rc=$?
  if [ "$want" = 0 ] && [ "$rc" -ne 0 ]; then echo "selftest: '$*' want rc 0, got $rc: $out"; bad=1; fi
  if [ "$want" != 0 ] && [ "$rc" -eq 0 ]; then echo "selftest: '$*' want rc != 0, got 0: $out"; bad=1; fi
  if [ "$wantline" = yes ] && ! grep -q '^FAIL' <<<"$out"; then echo "selftest: '$*' want a FAIL line, got: $out"; bad=1; fi
  if [ "$wantline" = no ] && grep -q '^FAIL' <<<"$out"; then echo "selftest: '$*' unexpected FAIL line: $out"; bad=1; fi
}

run_case 0 no  expect selftest Deployment x '.spec.replicas' 2
run_case 1 yes expect selftest Deployment x '.spec.replicas' 3
run_case 1 yes expect selftest Deployment missing '.spec.replicas' 2
run_case 0 no  expect_absent selftest Deployment y
run_case 1 yes expect_absent selftest Deployment x

# ---- pins-check.sh: render bersih lulus; tiap jenis placeholder menggagalkan --prod
PINS="$HACK/pins-check.sh"
PINS_DIR="$(mktemp -d)"
trap 'rm -rf "$K8S_RENDER_DIR" "$PINS_DIR"' EXIT
clean_render() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {A: "1", CIDR: 10.20.0.0/16}\n---\nimage: ghcr.io/nabepi/kotoki-backend/api@sha256:%s\n' "$(printf 'a%.0s' $(seq 1 64))"; }
pins_case() { # <want-rc 0|1> <desc> <staging-content> <prod-content>
  local want="$1" desc="$2" rc
  printf '%s' "$3" >"$PINS_DIR/staging.yaml"; printf '%s' "$4" >"$PINS_DIR/prod.yaml"
  PINS_RENDER_DIR="$PINS_DIR" "$PINS" --prod >/dev/null 2>&1; rc=$?
  if [ "$want" = 0 ] && [ "$rc" -ne 0 ]; then echo "selftest: pins-check --prod ($desc) want rc 0, got $rc"; bad=1; fi
  if [ "$want" = 1 ] && [ "$rc" -eq 0 ]; then echo "selftest: pins-check --prod ($desc) want rc != 0, got 0"; bad=1; fi
}
C="$(clean_render)"
pins_case 0 "render bersih" "$C" "$C"
pins_case 1 "REPLACE_ di staging" "$C
tag: REPLACE_SHA" "$C"
pins_case 1 "REPLACE_ di prod" "$C" "$C
cidr: REPLACE_DATA_STORE_CIDR"
pins_case 1 "keputusan vendor" "$C
kotoki.io/provider-selection: REPLACE_PRODUCT_OWNER_DECISION" "$C"
pins_case 1 "CIDR TEST-NET-1" "$C" "$C
cidr: 192.0.2.0/24"
pins_case 1 "CIDR TEST-NET-2" "$C
TRUSTED_PROXY_CIDRS: 198.51.100.0/24" "$C"
pins_case 1 "digest nol" "$C" "$C
image: x@sha256:$(printf '0%.0s' $(seq 1 64))"
pins_case 1 "staging hilang" "" "$C"
# mode default: <PIN> di artefak deploy menggagalkan; tanpa <PIN> lulus
PIN_FIX="$ROOT/deploy/k8s/.selftest-pin.tmp"
printf 'v: <%s>\n' PIN >"$PIN_FIX"
if "$PINS" >/dev/null 2>&1; then echo "selftest: pins-check default harus gagal bila <PIN> tertinggal"; bad=1; fi
rm -f "$PIN_FIX"
if ! "$PINS" >/dev/null 2>&1; then echo "selftest: pins-check default harus lulus tanpa <PIN>"; bad=1; fi

# --list: rc 0, mencantumkan versi alat dan tag image, menandai tag mengambang
LIST_OUT="$("$PINS" --list 2>&1)"; LIST_RC=$?
[ "$LIST_RC" -eq 0 ] || { echo "selftest: pins-check --list want rc 0, got $LIST_RC"; bad=1; }
for want in "KUSTOMIZE_VERSION=" "image (docker-compose.yml, deploy/k8s):" "tag mengambang"; do
  case "$LIST_OUT" in *"$want"*) ;; *) echo "selftest: pins-check --list tanpa '$want'"; bad=1 ;; esac
done

if [ "$bad" -ne 0 ]; then echo "selftest FAILED"; exit 1; fi
echo "selftest OK"
