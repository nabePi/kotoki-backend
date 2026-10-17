#!/usr/bin/env bash
# Helper assert manifest (di-source oleh deploy/k8s/assert/*.sh dan selftest.sh).
# Semua pembacaan YAML lewat yq dari bin/tools (bukan yq sistem). Tidak mengubah opsi shell
# pemanggil kecuali variabel FAILS/CHECKS.
#
#   render <overlay>                         -> path JSON ter-cache (out/k8s/<overlay>.json)
#   expect <overlay> <Kind> <name> '<yq>' '<want>'
#   expect_absent <overlay> <Kind> <name>
#   finish                                   -> exit 1 bila ada FAIL
#
# Catatan: `expect` menimpa /usr/bin/expect (Tcl) di dalam shell yang meng-source berkas ini.

_LIB_HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${ROOT:-$(cd "$_LIB_HACK/../../.." && pwd)}"
YQ="${YQ:-$ROOT/bin/tools/yq}"
K8S_RENDER_DIR="${K8S_RENDER_DIR:-$ROOT/out/k8s}"
FAILS=0
CHECKS=0

_fail() { printf 'FAIL %s\n' "$*" >&2; FAILS=$((FAILS + 1)); }
_ok()   { printf 'ok   %s\n' "$*"; }

if [ ! -x "$YQ" ]; then
  echo "lib.sh: yq tidak ada di $YQ; jalankan 'make k8s-tools'" >&2
  return 1 2>/dev/null || exit 1
fi

# render <overlay>: <dir>/<overlay>.yaml (keluaran `make k8s-build`) -> <dir>/<overlay>.json,
# array berisi semua dokumen. Cache dibuat ulang bila YAML lebih baru. Mencetak path JSON;
# gagal (rc 1) bila YAML tidak ada, tidak pernah diam-diam menghasilkan array kosong.
render() {
  local ov="$1" y="$K8S_RENDER_DIR/$1.yaml" j="$K8S_RENDER_DIR/$1.json"
  if [ ! -s "$y" ]; then
    echo "render: $y tidak ada atau kosong; jalankan 'make k8s-build'" >&2
    return 1
  fi
  if [ ! -s "$j" ] || [ "$y" -nt "$j" ]; then
    "$YQ" ea -o=json '[.]' "$y" >"$j.tmp" || { rm -f "$j.tmp"; echo "render: yq gagal pada $y" >&2; return 1; }
    mv "$j.tmp" "$j"
  fi
  printf '%s\n' "$j"
}

# _select <overlay> <Kind> <name> '<yq-expr>' -> hasil expr untuk objek yang cocok
_select() {
  local j
  j="$(render "$1")" || return 2
  K="$2" N="$3" "$YQ" -r ".[] | select(.kind == strenv(K) and .metadata.name == strenv(N)) | $4" "$j"
}

expect() {
  local ov="$1" kind="$2" name="$3" expr="$4" want="$5" got rc
  CHECKS=$((CHECKS + 1))
  got="$(_select "$ov" "$kind" "$name" "$expr")"; rc=$?
  if [ "$rc" -ne 0 ]; then _fail "$ov $kind/$name $expr: tidak bisa dibaca (rc=$rc)"; return 1; fi
  if [ -z "$got" ]; then _fail "$ov $kind/$name tidak ditemukan (atau $expr kosong)"; return 1; fi
  if [ "$got" != "$want" ]; then _fail "$ov $kind/$name $expr: want '$want', got '$got'"; return 1; fi
  _ok "$ov $kind/$name $expr = $want"
}

expect_absent() {
  local ov="$1" kind="$2" name="$3" got rc
  CHECKS=$((CHECKS + 1))
  got="$(_select "$ov" "$kind" "$name" '.metadata.name')"; rc=$?
  if [ "$rc" -ne 0 ]; then _fail "$ov $kind/$name: tidak bisa dibaca (rc=$rc)"; return 1; fi
  if [ -n "$got" ]; then _fail "$ov $kind/$name seharusnya tidak ada"; return 1; fi
  _ok "$ov $kind/$name absen"
}

finish() {
  if [ "$FAILS" -gt 0 ]; then
    printf '%d dari %d pemeriksaan GAGAL\n' "$FAILS" "$CHECKS" >&2
    exit 1
  fi
  printf '%d pemeriksaan lulus\n' "$CHECKS"
}
