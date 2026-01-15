#!/bin/bash
#
# Citadel CLI Installer
#
# Fetches the latest stable version of the Citadel CLI from GitHub,
# verifies its checksum, and installs it to /usr/local/bin.
#
# This script is designed to be idempotent and safe.
#
# Usage:
#   curl -fsSL https://get.aceteam.ai/citadel.sh | sudo bash
#

# --- Configuration ---
set -e -u -o pipefail

REPO="aceteam-ai/citadel-cli"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="citadel"

# --- Helper Functions ---
msg() {
  echo -e "\033[1;32m=>\033[0m \033[1m$1\033[0m" >&2
}

err() {
  echo -e "\033[1;31mERROR:\033[0m $1" >&2
  exit 1
}

# --- Pre-flight Checks ---
check_root() {
  if [ "$(id -u)" -ne 0 ]; then
    err "This script must be run with sudo or as root. Try: curl ... | sudo bash"
  fi
}

check_deps() {
  local deps=("curl" "tar" "grep" "cut")

  # On Linux, check for sha256sum; on macOS, use shasum
  local os
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  if [ "$os" = "linux" ]; then
    deps+=("sha256sum")
  elif [ "$os" = "darwin" ]; then
    deps+=("shasum")
  fi

  for dep in "${deps[@]}"; do
    if ! command -v "$dep" &>/dev/null; then
      err "Required command '$dep' is not installed. Please install it and try again."
    fi
  done
}

# --- Main Logic ---
get_arch() {
  local os
  local arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)

  case "$os" in
    linux) os="linux" ;;
    darwin) os="darwin" ;;
    *) err "Unsupported operating system: $os" ;;
  esac

  case "$arch" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *) err "Unsupported architecture: $arch" ;;
  esac

  echo "${os}_${arch}"
}

get_latest_version() {
  # IMPORTANT: The /latest endpoint fetches the most recent *non-prerelease, non-draft* release.
  # Your 'v1.0.1-rc1' will be ignored by this. This is generally what you want for a stable installer.
  local latest_url="https://api.github.com/repos/${REPO}/releases/latest"
  local version
  
  msg "Fetching latest stable version information..."
  version=$(curl -sSL "$latest_url" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')

  if [ -z "$version" ]; then
    err "Could not determine the latest stable version. Check the repository releases."
  fi

  echo "$version"
}

install_citadel() {
  local arch="$1"
  local version="$2"
  local binary_archive="${BINARY_NAME}_${version}_${arch}.tar.gz"
  local checksum_file="checksums.txt" # Matches your release asset name
  
  local download_url_base="https://github.com/${REPO}/releases/download/${version}"
  
  local tmp_dir
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT # Ensure cleanup on exit/error

  msg "Downloading Citadel CLI ${version} for ${arch}..."
  
  # Download the archive and checksums
  if ! curl -sSL -o "${tmp_dir}/${binary_archive}" "${download_url_base}/${binary_archive}"; then
    err "Failed to download binary archive. Check version and architecture."
  fi
  if ! curl -sSL -o "${tmp_dir}/${checksum_file}" "${download_url_base}/${checksum_file}"; then
    err "Failed to download checksums file."
  fi

  msg "Verifying checksum..."
  # checksums.txt contains hashes for all architectures, so we need to filter for our specific file.
  local os
  os=$(uname -s | tr '[:upper:]' '[:lower:]')

  # Extract just the checksum line for our binary
  local expected_checksum
  expected_checksum=$(grep "$binary_archive" "${tmp_dir}/${checksum_file}" | cut -d ' ' -f 1)

  if [ -z "$expected_checksum" ]; then
    err "Could not find checksum for ${binary_archive} in checksums file."
  fi

  local actual_checksum
  if [ "$os" = "darwin" ]; then
    # On macOS, use shasum with -a 256 for SHA-256
    actual_checksum=$(shasum -a 256 "${tmp_dir}/${binary_archive}" | cut -d ' ' -f 1)
  else
    # On Linux, use sha256sum
    actual_checksum=$(sha256sum "${tmp_dir}/${binary_archive}" | cut -d ' ' -f 1)
  fi

  if [ "$expected_checksum" != "$actual_checksum" ]; then
    err "Checksum validation failed! The downloaded file may be corrupt or tampered with."
  fi
  msg "Checksum valid."

  msg "Extracting and installing..."
  tar -xzf "${tmp_dir}/${binary_archive}" -C "$tmp_dir"

  local extracted_binary
  extracted_binary=$(find "$tmp_dir" -type f -name "$BINARY_NAME")
  if [ -z "$extracted_binary" ]; then
    err "Could not find the '${BINARY_NAME}' binary in the downloaded archive."
  fi

  # `install` is the proper way to do this. It handles permissions and is atomic.
  if ! install -m 755 "$extracted_binary" "${INSTALL_DIR}/${BINARY_NAME}"; then
    err "Failed to install '${BINARY_NAME}' to ${INSTALL_DIR}. Check permissions."
  fi

  # Explicitly clean up the temp directory now that we're done.
  rm -rf "$tmp_dir"
  # Disarm the trap so it doesn't fire on script exit with an out-of-scope variable.
  trap - EXIT
}

# --- Execution ---
main() {
  msg "Starting Citadel CLI installation..."

  check_root
  check_deps

  local arch
  arch=$(get_arch)

  local version
  version=$(get_latest_version)

  install_citadel "$arch" "$version"

  msg "Citadel CLI installed successfully to ${INSTALL_DIR}/${BINARY_NAME}"

  local installed_version
  installed_version=$(${INSTALL_DIR}/${BINARY_NAME} version)

  echo "" >&2
  msg "Installation complete!"
  echo "  Version: ${installed_version}" >&2

  # Auto-run citadel init if running interactively
  if [ -t 0 ] && [ -t 1 ]; then
    echo "" >&2
    msg "Starting device provisioning..."
    echo "" >&2
    ${INSTALL_DIR}/${BINARY_NAME} init
  else
    echo "  Run 'citadel --help' to get started." >&2
    echo "  To provision this node, run: sudo citadel init" >&2
  fi
}

main