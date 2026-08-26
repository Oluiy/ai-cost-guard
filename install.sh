#!/bin/sh
# Installs the fitguard CLI: detects OS/arch, downloads the matching
# GoReleaser archive from GitHub Releases, verifies it against the
# published checksums, and installs the binary.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Oluiy/ai-cost-guard/main/install.sh | sh
#
# Env overrides:
#   VERSION      release tag to install, e.g. v0.2.0 (default: latest)
#   INSTALL_DIR  where the binary is placed (default: /usr/local/bin)
#   BASE_URL     release asset base URL (default: GitHub releases for this
#                repo; overridable for testing against a local server)
#
# POSIX sh, not bash: this runs via `curl | sh` on whatever shell that
# invokes, which on some systems (Debian, Alpine) is dash, not bash.

set -eu

REPO="Oluiy/ai-cost-guard"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BASE_URL="${BASE_URL:-https://github.com/${REPO}/releases/download}"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

detect_os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    *) die "unsupported OS: $(uname -s). Prebuilt binaries cover Linux and macOS; see https://github.com/${REPO} for Windows and from-source options." ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    arm64|aarch64) echo arm64 ;;
    *) die "unsupported architecture: $(uname -m). Prebuilt binaries cover amd64 and arm64." ;;
  esac
}

# Resolves the "latest" tag without requiring jq: GitHub's API response
# has `"tag_name": "vX.Y.Z"` on its own line, so a plain grep/cut pulls it
# out without a JSON parser as a dependency.
latest_version() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' \
    | head -n1 \
    | cut -d'"' -f4
}

main() {
  need_cmd curl
  need_cmd tar
  need_cmd sha256sum_or_shasum

  os=$(detect_os)
  arch=$(detect_arch)

  version="${VERSION:-}"
  if [ -z "$version" ]; then
    log "Resolving latest release..."
    version=$(latest_version)
    [ -n "$version" ] || die "couldn't resolve the latest release. Set VERSION=vX.Y.Z to install a specific one."
  fi

  archive="fitguard_${os}_${arch}.tar.gz"
  archive_url="${BASE_URL}/${version}/${archive}"
  checksums_url="${BASE_URL}/${version}/checksums.txt"

  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT INT TERM

  log "Downloading ${archive} (${version})..."
  curl -fsSL "$archive_url" -o "${tmpdir}/${archive}" \
    || die "download failed: ${archive_url}. Does that version/platform combination have a release asset?"

  log "Downloading checksums..."
  curl -fsSL "$checksums_url" -o "${tmpdir}/checksums.txt" \
    || die "download failed: ${checksums_url}"

  log "Verifying checksum..."
  verify_checksum "$tmpdir" "$archive"

  log "Extracting..."
  tar -xzf "${tmpdir}/${archive}" -C "$tmpdir" fitguard \
    || die "archive did not contain a 'fitguard' binary at its root"
  chmod +x "${tmpdir}/fitguard"

  install_binary "${tmpdir}/fitguard"

  log ""
  log "fitguard ${version} installed to ${INSTALL_DIR}/fitguard"
  "${INSTALL_DIR}/fitguard" --version 2>&1 | sed 's/^/  /' >&2 || true
  log ""
  log "Next: fitguard init"
}

need_cmd() {
  case "$1" in
    sha256sum_or_shasum)
      command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 \
        || die "need 'sha256sum' or 'shasum' to verify the download, and neither is on PATH"
      ;;
    *)
      command -v "$1" >/dev/null 2>&1 || die "need '$1' on PATH"
      ;;
  esac
}

# Computes the archive's sha256 with whichever tool is available (GNU
# coreutils has sha256sum; macOS ships shasum instead) and checks it
# against the line for this exact filename in checksums.txt, so a
# corrupted or tampered download is rejected before it's ever executed.
verify_checksum() {
  dir="$1"
  file="$2"
  expected=$(grep " ${file}\$" "${dir}/checksums.txt" | cut -d' ' -f1)
  [ -n "$expected" ] || die "no checksum entry found for ${file} in checksums.txt"

  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${dir}/${file}" | cut -d' ' -f1)
  else
    actual=$(shasum -a 256 "${dir}/${file}" | cut -d' ' -f1)
  fi

  [ "$expected" = "$actual" ] || die "checksum mismatch for ${file}: expected ${expected}, got ${actual}"
}

# Writes straight to INSTALL_DIR if it's writable; otherwise re-runs the
# move through sudo, so this works both for a user-owned dir (e.g.
# ~/.local/bin) and the root-owned default (/usr/local/bin) without
# assuming which one applies.
install_binary() {
  src="$1"
  mkdir -p "$INSTALL_DIR" 2>/dev/null || true

  if [ -w "$INSTALL_DIR" ]; then
    mv "$src" "${INSTALL_DIR}/fitguard"
  elif command -v sudo >/dev/null 2>&1; then
    log "${INSTALL_DIR} isn't writable, requesting sudo to install there..."
    sudo mv "$src" "${INSTALL_DIR}/fitguard"
  else
    die "${INSTALL_DIR} isn't writable and sudo isn't available. Re-run with INSTALL_DIR set to a directory you own, e.g.: INSTALL_DIR=\$HOME/.local/bin sh install.sh"
  fi
}

main "$@"
