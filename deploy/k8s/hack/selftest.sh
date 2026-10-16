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

if [ "$bad" -ne 0 ]; then echo "selftest FAILED"; exit 1; fi
echo "selftest OK"
