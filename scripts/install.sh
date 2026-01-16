#!/bin/bash
#
# Citadel CLI Installer
#
# Fetches the latest stable version of the Citadel CLI from GitHub,
# verifies its checksum, and installs it.
#
# This script is designed to be idempotent and safe.
#
# Usage:
#   curl -fsSL https://get.aceteam.ai/citadel.sh | bash           # User-local install
#   curl -fsSL https://get.aceteam.ai/citadel.sh | sudo bash      # System-wide install
#

# --- Configuration ---
set -e -u -o pipefail

REPO="aceteam-ai/citadel-cli"
BINARY_NAME="citadel"

# Determine install location based on privileges
if [ "$(id -u)" -eq 0 ]; then
  INSTALL_DIR="/usr/local/bin"
  INSTALL_MODE="system"
else
  INSTALL_DIR="${HOME}/.local/bin"
  INSTALL_MODE="user"
fi

# --- Helper Functions ---
msg() {
  echo -e "\033[1;32m=>\033[0m \033[1m$1\033[0m" >&2
}

warn() {
  echo -e "\033[1;33mWARNING:\033[0m $1" >&2
}

err() {
  echo -e "\033[1;31mERROR:\033[0m $1" >&2
  exit 1
}

# --- Pre-flight Checks ---
setup_install_dir() {
  if [ "$INSTALL_MODE" = "user" ]; then
    # Create ~/.local/bin if it doesn't exist
    if [ ! -d "$INSTALL_DIR" ]; then
      msg "Creating ${INSTALL_DIR}..."
      mkdir -p "$INSTALL_DIR"
    fi
  fi

  # Verify we can write to the install directory
  if [ ! -w "$INSTALL_DIR" ] && [ -d "$INSTALL_DIR" ]; then
    err "Cannot write to ${INSTALL_DIR}. Try running with sudo for system-wide install."
  fi
}

detect_shell_profile() {
  # Detect the user's shell and return the appropriate profile file
  local shell_name
  shell_name=$(basename "$SHELL")

  case "$shell_name" in
    zsh)
      echo "${HOME}/.zshrc"
      ;;
    bash)
      # Prefer .bashrc on Linux, .bash_profile on macOS
      if [ -f "${HOME}/.bashrc" ]; then
        echo "${HOME}/.bashrc"
      elif [ -f "${HOME}/.bash_profile" ]; then
        echo "${HOME}/.bash_profile"
      else
        echo "${HOME}/.bashrc"
      fi
      ;;
    fish)
      echo "${HOME}/.config/fish/config.fish"
      ;;
    *)
      # Default to .profile for unknown shells
      echo "${HOME}/.profile"
      ;;
  esac
}

is_in_path() {
  # Check if a directory is in PATH
  local dir="$1"
  case ":${PATH}:" in
    *":${dir}:"*) return 0 ;;
    *) return 1 ;;
  esac
}

add_to_path() {
  # Add directory to shell profile if not already in PATH
  local profile_file
  profile_file=$(detect_shell_profile)
  local shell_name
  shell_name=$(basename "$SHELL")

  # Check if the export line already exists in the profile
  local export_line
  if [ "$shell_name" = "fish" ]; then
    export_line="fish_add_path ${INSTALL_DIR}"
  else
    export_line="export PATH=\"\${HOME}/.local/bin:\${PATH}\""
  fi

  # Create profile directory if needed (for fish)
  if [ "$shell_name" = "fish" ] && [ ! -d "$(dirname "$profile_file")" ]; then
    mkdir -p "$(dirname "$profile_file")"
  fi

  # Check if already added
  if [ -f "$profile_file" ] && grep -qF ".local/bin" "$profile_file" 2>/dev/null; then
    return 0  # Already configured
  fi

  # Add to profile
  msg "Adding ${INSTALL_DIR} to PATH in ${profile_file}..."
  echo "" >> "$profile_file"
  echo "# Added by Citadel CLI installer" >> "$profile_file"
  echo "$export_line" >> "$profile_file"

  return 1  # Indicates we added it (needs source)
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
  if [ "$INSTALL_MODE" = "system" ]; then
    msg "Starting Citadel CLI installation (system-wide)..."
  else
    msg "Starting Citadel CLI installation (user-local)..."
  fi

  setup_install_dir
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

  # Handle PATH configuration for user-local installs
  if [ "$INSTALL_MODE" = "user" ]; then
    if is_in_path "$INSTALL_DIR"; then
      echo "  Location: ${INSTALL_DIR}/${BINARY_NAME}" >&2
    else
      # Add to PATH in shell profile
      if add_to_path; then
        echo "  Location: ${INSTALL_DIR}/${BINARY_NAME}" >&2
      else
        local profile_file
        profile_file=$(detect_shell_profile)
        echo "" >&2
        warn "To use citadel, restart your terminal or run:"
        echo "  source ${profile_file}" >&2
      fi
    fi
    echo "" >&2
    echo "  Run 'citadel --help' to get started." >&2
    echo "  To provision this node, run: citadel init" >&2
    echo "" >&2
    echo "  Note: Full provisioning (Docker, GPU toolkit) requires sudo." >&2
    echo "  You can run 'citadel init --network-only' for network setup without sudo." >&2
  else
    # System-wide install: auto-run citadel init if running interactively
    if [ -t 0 ] && [ -t 1 ]; then
      echo "" >&2
      msg "Starting device provisioning..."
      echo "" >&2
      ${INSTALL_DIR}/${BINARY_NAME} init
    else
      echo "  Run 'citadel --help' to get started." >&2
      echo "  To provision this node, run: sudo citadel init" >&2
    fi
  fi
}

main
