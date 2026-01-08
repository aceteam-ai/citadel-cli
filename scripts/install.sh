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
#   curl -fsSL https://aceteam.ai/citadel/install.sh | sudo bash
#

# --- Configuration ---
set -e -u -o pipefail

REPO="aceteam-ai/citadel-cli"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="citadel"

# --- Helper Functions ---
msg() {
  echo -e "\033[1;32m=>\033[0m \033[1m$1\033[0m"
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
  local deps=("curl" "tar" "grep" "cut" "sha256sum")
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
    darwin) err "macOS is not yet supported by this installer." ;;
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
  # Use a subshell to change directory temporarily.
  # The `--ignore-missing` flag is crucial because checksums.txt contains hashes for all architectures.
  (cd "$tmp_dir" && sha256sum -c --ignore-missing "$checksum_file")
  if [ $? -ne 0 ]; then
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
  
  echo ""
  msg "Installation complete!"
  echo "  Version: ${installed_version}"
  echo "  Run 'citadel --help' to get started."
  echo "  To provision this node, run: sudo citadel init"
}

main