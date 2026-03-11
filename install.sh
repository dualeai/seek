#!/bin/sh
# Install script for seek -- https://github.com/dualeai/seek
# Usage:
#   curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh
#   curl -sSfL https://raw.githubusercontent.com/dualeai/seek/main/install.sh | sh -s -- -b /usr/local/bin v1.2.3

set -e

REPO="dualeai/seek"
BINARY="seek"
GITHUB="https://github.com"

usage() {
  cat <<EOF
Usage: install.sh [-b bin_dir] [version]

Options:
  -b <dir>    Install directory (default: ~/.local/bin)
  -h          Show this help

Arguments:
  version     Version to install (e.g. v1.2.3). Default: latest release.

Examples:
  # Install latest to ~/.local/bin
  curl -sSfL https://raw.githubusercontent.com/${REPO}/develop/install.sh | sh

  # Install specific version to /usr/local/bin
  curl -sSfL https://raw.githubusercontent.com/${REPO}/develop/install.sh | sh -s -- -b /usr/local/bin v1.2.3
EOF
}

main() {
  bin_dir="${HOME}/.local/bin"
  version=""

  # Parse arguments
  while [ $# -gt 0 ]; do
    case "$1" in
      -b)
        if [ -z "${2:-}" ]; then
          echo "Error: -b requires a directory argument" >&2
          exit 1
        fi
        bin_dir="$2"
        shift 2
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      v*)
        version="$1"
        shift
        ;;
      [0-9]*)
        version="v$1"
        shift
        ;;
      *)
        echo "Error: unknown argument '$1'" >&2
        usage >&2
        exit 1
        ;;
    esac
  done

  # Detect OS
  os="$(uname -s)"
  case "$os" in
    Darwin) os="darwin" ;;
    Linux)  os="linux" ;;
    *)
      echo "Error: unsupported OS '${os}'" >&2
      exit 1
      ;;
  esac

  # Detect architecture
  arch="$(uname -m)"
  case "$arch" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *)
      echo "Error: unsupported architecture '${arch}'" >&2
      exit 1
      ;;
  esac

  # Resolve version
  if [ -z "$version" ]; then
    version="$(curl -sSf -o /dev/null -w '%{redirect_url}' "${GITHUB}/${REPO}/releases/latest")"
    version="${version##*/}"
    if [ -z "$version" ]; then
      echo "Error: could not determine latest version" >&2
      exit 1
    fi
  fi

  archive="${BINARY}_${os}_${arch}.tar.gz"
  base_url="${GITHUB}/${REPO}/releases/download/${version}"

  echo "Installing ${BINARY} ${version} (${os}/${arch}) to ${bin_dir}"

  # Create temporary directory
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT

  # Download archive and checksums
  echo "Downloading ${base_url}/${archive}"
  curl -sSfL -o "${tmp}/${archive}" "${base_url}/${archive}"
  curl -sSfL -o "${tmp}/checksums.txt" "${base_url}/checksums.txt"

  # Verify checksum
  expected="$(grep -F "${archive}" "${tmp}/checksums.txt" | cut -d ' ' -f 1)"
  if [ -z "$expected" ]; then
    echo "Error: archive '${archive}' not found in checksums.txt" >&2
    exit 1
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "${tmp}/${archive}" | cut -d ' ' -f 1)"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "${tmp}/${archive}" | cut -d ' ' -f 1)"
  else
    echo "Warning: sha256sum/shasum not found, skipping checksum verification" >&2
    actual="$expected"
  fi

  if [ "$actual" != "$expected" ]; then
    echo "Error: checksum mismatch" >&2
    echo "  expected: ${expected}" >&2
    echo "  actual:   ${actual}" >&2
    exit 1
  fi

  # Extract and install
  tar -xzf "${tmp}/${archive}" -C "${tmp}"
  mkdir -p "${bin_dir}"
  install "${tmp}/${BINARY}" "${bin_dir}/${BINARY}"

  echo "Installed ${bin_dir}/${BINARY}"

  # Warn if not in PATH
  case ":${PATH}:" in
    *":${bin_dir}:"*) ;;
    *)
      echo
      echo "Note: ${bin_dir} is not in your PATH. Add it with:"
      echo "  export PATH=\"${bin_dir}:\${PATH}\""
      ;;
  esac
}

main "$@"
