#!/bin/sh
# Flint POSIX installer for macOS (darwin) and Linux (amd64 / arm64).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/flint-fhir/flint/main/install.sh | sh
#
# Environment overrides:
#   FLINT_VERSION      Release tag to install (e.g. "v0.4.0"; defaults to latest GitHub release)
#   FLINT_INSTALL_DIR  Destination directory for flintd, seed, and certify
#   FLINT_REPO         GitHub owner/repo (default: "flint-fhir/flint")
#   FLINT_BASE_URL     Release asset base URL override (for mirrors / air-gapped testing)

set -eu

REPO="${FLINT_REPO:-flint-fhir/flint}"
VERSION="${FLINT_VERSION:-}"

detect_os() {
  uname_s="$(uname -s)"
  case "${uname_s}" in
    Linux*)  printf 'linux' ;;
    Darwin*) printf 'darwin' ;;
    *)
      printf 'Error: unsupported operating system "%s" (supported: Linux, macOS)\n' "${uname_s}" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  uname_m="$(uname -m)"
  case "${uname_m}" in
    x86_64|amd64)  printf 'amd64' ;;
    arm64|aarch64) printf 'arm64' ;;
    *)
      printf 'Error: unsupported CPU architecture "%s" (supported: amd64, arm64)\n' "${uname_m}" >&2
      exit 1
      ;;
  esac
}

fetch_file() {
  src_url="$1"
  dest_path="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${src_url}" -o "${dest_path}"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "${dest_path}" "${src_url}"
  else
    printf 'Error: curl or wget is required to download Flint releases\n' >&2
    exit 1
  fi
}

resolve_version() {
  if [ -n "${VERSION}" ]; then
    printf '%s' "${VERSION}"
    return
  fi
  api_url="https://api.github.com/repos/${REPO}/releases/latest"
  tmp_json="$1/latest.json"
  fetch_file "${api_url}" "${tmp_json}"
  resolved="$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "${tmp_json}" | head -n 1)"
  if [ -z "${resolved}" ]; then
    printf 'Error: unable to determine latest release tag from %s\n' "${api_url}" >&2
    exit 1
  fi
  printf '%s' "${resolved}"
}

compute_sha256() {
  target_file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${target_file}" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${target_file}" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "${target_file}" | awk '{print $NF}'
  else
    printf 'Error: sha256sum, shasum, or openssl is required to verify release checksums\n' >&2
    exit 1
  fi
}

resolve_install_dir() {
  if [ -n "${FLINT_INSTALL_DIR:-}" ]; then
    printf '%s' "${FLINT_INSTALL_DIR}"
    return
  fi
  if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
    printf '/usr/local/bin'
    return
  fi
  printf '%s/.local/bin' "${HOME}"
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'flint-install')"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT INT TERM

TAG="$(resolve_version "${TMP_DIR}")"
BASE_URL="${FLINT_BASE_URL:-https://github.com/${REPO}/releases/download/${TAG}}"
PKG_NAME="flint_${TAG}_${OS}_${ARCH}"
ARCHIVE_NAME="${PKG_NAME}.tar.gz"

printf '==> Downloading Flint %s (%s/%s)...\n' "${TAG}" "${OS}" "${ARCH}"
fetch_file "${BASE_URL}/${ARCHIVE_NAME}" "${TMP_DIR}/${ARCHIVE_NAME}"
fetch_file "${BASE_URL}/checksums.txt" "${TMP_DIR}/checksums.txt"

EXPECTED_SHA="$(awk -v f="${ARCHIVE_NAME}" '$2 == f || $2 == "*" f { print $1; exit }' "${TMP_DIR}/checksums.txt")"
if [ -z "${EXPECTED_SHA}" ]; then
  printf 'Error: %s not found in checksums.txt\n' "${ARCHIVE_NAME}" >&2
  exit 1
fi

ACTUAL_SHA="$(compute_sha256 "${TMP_DIR}/${ARCHIVE_NAME}")"
if [ "${EXPECTED_SHA}" != "${ACTUAL_SHA}" ]; then
  printf 'Error: SHA-256 checksum mismatch for %s\n  expected: %s\n  actual:   %s\n' \
    "${ARCHIVE_NAME}" "${EXPECTED_SHA}" "${ACTUAL_SHA}" >&2
  exit 1
fi
printf '==> Verified SHA-256 checksum (%s)\n' "${ACTUAL_SHA}"

tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "${TMP_DIR}"

INSTALL_DIR="$(resolve_install_dir)"
mkdir -p "${INSTALL_DIR}"

for bin_name in flintd seed certify; do
  src_bin="${TMP_DIR}/${PKG_NAME}/${bin_name}"
  if [ -f "${src_bin}" ]; then
    cp "${src_bin}" "${INSTALL_DIR}/${bin_name}"
    chmod 0755 "${INSTALL_DIR}/${bin_name}"
  fi
done

printf '==> Installed flintd, seed, and certify to %s\n' "${INSTALL_DIR}"
case ":${PATH:-}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    printf 'Note: add %s to your PATH:\n  export PATH="%s:$PATH"\n' "${INSTALL_DIR}" "${INSTALL_DIR}"
    ;;
esac
printf '==> Quickstart (zero-dependency SQLite WAL mode):\n  %s/flintd --store=sqlite --sqlite-dsn=flint.db\n' "${INSTALL_DIR}"
