#!/usr/bin/env bash
# Mengunduh alat dev manifest ke bin/tools/ (versi dari tools.env). Idempoten: alat yang
# sudah berversi benar dilewati. Deteksi OS/arsitektur host; checksum rilis diverifikasi.
# Memakai yq dari bin/tools, tidak pernah yq sistem.
set -euo pipefail

HACK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HACK/../../.." && pwd)"
# shellcheck source=tools.env
source "$HACK/tools.env"
BIN="${TOOLS_BIN:-$ROOT/bin/tools}"
mkdir -p "$BIN"

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "install-tools: OS tidak didukung: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) echo "install-tools: arsitektur tidak didukung: $(uname -m)" >&2; exit 1 ;;
esac

sha256_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# fetch <url> <dest>
fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }

# verify <archive> <expected-sha256> <label>
verify() {
  local got
  got="$(sha256_of "$1")"
  if [ "$got" != "$2" ]; then
    echo "install-tools: checksum $3 TIDAK COCOK (want $2, got $got)" >&2
    exit 1
  fi
}

# have <binary> <substring-versi>
have() { [ -x "$BIN/$1" ] && "$BIN/$1" "${@:3}" 2>&1 | grep -qF -- "$2"; }

install_kustomize() {
  local v="$KUSTOMIZE_VERSION" f="kustomize_${KUSTOMIZE_VERSION}_${OS}_${ARCH}.tar.gz"
  have kustomize "$v" version && { echo "kustomize $v ok"; return; }
  local base="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F$v"
  fetch "$base/$f" "$WORK/$f"; fetch "$base/checksums.txt" "$WORK/kustomize.sums"
  verify "$WORK/$f" "$(awk -v f="$f" '$2==f{print $1}' "$WORK/kustomize.sums")" "$f"
  tar -xzf "$WORK/$f" -C "$WORK" kustomize && install -m 0755 "$WORK/kustomize" "$BIN/kustomize"
  have kustomize "$v" version || { echo "install-tools: kustomize terpasang tetapi versi salah" >&2; exit 1; }
  echo "kustomize $v terpasang"
}

install_kubeconform() {
  local v="$KUBECONFORM_VERSION" f="kubeconform-${OS}-${ARCH}.tar.gz"
  have kubeconform "$v" -v && { echo "kubeconform $v ok"; return; }
  local base="https://github.com/yannh/kubeconform/releases/download/$v"
  fetch "$base/$f" "$WORK/$f"; fetch "$base/CHECKSUMS" "$WORK/kubeconform.sums"
  verify "$WORK/$f" "$(awk -v f="$f" '$2==f{print $1}' "$WORK/kubeconform.sums")" "$f"
  tar -xzf "$WORK/$f" -C "$WORK" kubeconform && install -m 0755 "$WORK/kubeconform" "$BIN/kubeconform"
  have kubeconform "$v" -v || { echo "install-tools: kubeconform terpasang tetapi versi salah" >&2; exit 1; }
  echo "kubeconform $v terpasang"
}

install_promtool() {
  local v="${PROMETHEUS_VERSION#v}" d="prometheus-${PROMETHEUS_VERSION#v}.${OS}-${ARCH}"
  have promtool "version $v" --version && { echo "promtool $v ok"; return; }
  local base="https://github.com/prometheus/prometheus/releases/download/$PROMETHEUS_VERSION"
  fetch "$base/$d.tar.gz" "$WORK/$d.tar.gz"; fetch "$base/sha256sums.txt" "$WORK/prom.sums"
  verify "$WORK/$d.tar.gz" "$(awk -v f="$d.tar.gz" '$2==f{print $1}' "$WORK/prom.sums")" "$d.tar.gz"
  tar -xzf "$WORK/$d.tar.gz" -C "$WORK" "$d/promtool" && install -m 0755 "$WORK/$d/promtool" "$BIN/promtool"
  have promtool "version $v" --version || { echo "install-tools: promtool terpasang tetapi versi salah" >&2; exit 1; }
  echo "promtool $v terpasang"
}

# Berkas checksums yq berisi satu kolom per algoritma; urutannya di checksums_hashes_order.
install_yq() {
  local v="$YQ_VERSION" f="yq_${OS}_${ARCH}.tar.gz"
  have yq "$v" --version && { echo "yq $v ok"; return; }
  local base="https://github.com/mikefarah/yq/releases/download/$v"
  fetch "$base/$f" "$WORK/$f"; fetch "$base/checksums" "$WORK/yq.sums"; fetch "$base/checksums_hashes_order" "$WORK/yq.order"
  local idx want
  idx="$(grep -n -x 'SHA-256' "$WORK/yq.order" | cut -d: -f1)"
  [ -n "$idx" ] || { echo "install-tools: SHA-256 tidak ada di checksums_hashes_order yq" >&2; exit 1; }
  want="$(awk -v f="yq_${OS}_${ARCH}.tar.gz" -v i="$((idx + 1))" '$1==f{print $i}' "$WORK/yq.sums")"
  verify "$WORK/$f" "$want" "$f"
  tar -xzf "$WORK/$f" -C "$WORK" "./yq_${OS}_${ARCH}" && install -m 0755 "$WORK/yq_${OS}_${ARCH}" "$BIN/yq"
  have yq "$v" --version || { echo "install-tools: yq terpasang tetapi versi salah" >&2; exit 1; }
  echo "yq $v terpasang"
}

install_actionlint() {
  local v="${ACTIONLINT_VERSION#v}" f="actionlint_${ACTIONLINT_VERSION#v}_${OS}_${ARCH}.tar.gz"
  have actionlint "$v" -version && { echo "actionlint $v ok"; return; }
  local base="https://github.com/rhysd/actionlint/releases/download/$ACTIONLINT_VERSION"
  fetch "$base/$f" "$WORK/$f"; fetch "$base/actionlint_${v}_checksums.txt" "$WORK/al.sums"
  verify "$WORK/$f" "$(awk -v f="$f" '$2==f{print $1}' "$WORK/al.sums")" "$f"
  tar -xzf "$WORK/$f" -C "$WORK" actionlint && install -m 0755 "$WORK/actionlint" "$BIN/actionlint"
  have actionlint "$v" -version || { echo "install-tools: actionlint terpasang tetapi versi salah" >&2; exit 1; }
  echo "actionlint $v terpasang"
}

install_kustomize
install_kubeconform
install_promtool
install_yq
install_actionlint
