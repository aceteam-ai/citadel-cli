#!/bin/bash
# 04-citadel.sh - Install citadel-cli binary and create systemd service
#
# Downloads the latest release from GitHub. Creates a systemd service unit
# for the worker, but does NOT enable it -- the first-boot script enables
# it after citadel init has created the manifest.
set -euo pipefail

echo "==> Installing Citadel CLI..."

REPO="aceteam-ai/citadel-cli"
BINARY_NAME="citadel"
INSTALL_DIR="/usr/local/bin"

# ---------------------------------------------------------------------------
# Detect architecture
# ---------------------------------------------------------------------------

get_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) echo "ERROR: Unsupported architecture: $arch" >&2; exit 1 ;;
    esac
}

ARCH="linux_$(get_arch)"

# ---------------------------------------------------------------------------
# Fetch latest version from GitHub API
# ---------------------------------------------------------------------------

echo "==> Fetching latest release version..."
LATEST_URL="https://api.github.com/repos/${REPO}/releases/latest"
VERSION=$(curl -sSL "${LATEST_URL}" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')

if [ -z "$VERSION" ]; then
    echo "ERROR: Could not determine latest version."
    exit 1
fi

echo "==> Latest version: ${VERSION}"

# ---------------------------------------------------------------------------
# Download, verify, and install
# ---------------------------------------------------------------------------

ARCHIVE="${BINARY_NAME}_${VERSION}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"

TMP_DIR=$(mktemp -d)
trap 'rm -rf "${TMP_DIR}"' EXIT

echo "==> Downloading ${ARCHIVE}..."
curl -sSL -o "${TMP_DIR}/${ARCHIVE}" "${DOWNLOAD_URL}"
curl -sSL -o "${TMP_DIR}/checksums.txt" "${CHECKSUMS_URL}"

# Verify checksum
echo "==> Verifying checksum..."
EXPECTED=$(grep "${ARCHIVE}" "${TMP_DIR}/checksums.txt" | cut -d ' ' -f 1)
ACTUAL=$(sha256sum "${TMP_DIR}/${ARCHIVE}" | cut -d ' ' -f 1)

if [ "${EXPECTED}" != "${ACTUAL}" ]; then
    echo "ERROR: Checksum mismatch!"
    echo "  Expected: ${EXPECTED}"
    echo "  Got:      ${ACTUAL}"
    exit 1
fi

echo "==> Checksum valid."

# Extract and install
tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "${TMP_DIR}"
install -m 755 "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"

echo "==> Citadel CLI installed: $(${INSTALL_DIR}/${BINARY_NAME} version)"

# ---------------------------------------------------------------------------
# Create directories
# ---------------------------------------------------------------------------

mkdir -p /etc/citadel
chmod 755 /etc/citadel

# ---------------------------------------------------------------------------
# Create systemd service (disabled -- first-boot enables it)
# ---------------------------------------------------------------------------

echo "==> Creating citadel-worker.service..."

cat > /etc/systemd/system/citadel-worker.service << 'UNIT'
[Unit]
Description=Citadel Worker - AceTeam Sovereign Compute Agent
Documentation=https://github.com/aceteam-ai/citadel-cli
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

# Only start if citadel init has been run (manifest exists)
ConditionPathExists=/etc/citadel/citadel.yaml

[Service]
Type=simple
ExecStart=/usr/local/bin/citadel work
Restart=always
RestartSec=10
User=citadel
Group=docker
Environment=HOME=/home/citadel

# Working directory where citadel looks for its manifest
WorkingDirectory=/home/citadel

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=citadel-worker

# Generous timeout for service startup (Docker containers may take a while)
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload

echo "==> citadel-worker.service created (not enabled -- first-boot will enable it)."
echo "==> Citadel CLI installation complete."
