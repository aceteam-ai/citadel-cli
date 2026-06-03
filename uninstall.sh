#!/usr/bin/env bash
#
# Citadel Node Uninstaller
#
# Removes the Citadel worker service, binary, and configuration.
# Does NOT uninstall NVIDIA drivers or Docker.
#
# Usage:
#   sudo bash uninstall.sh

set -uo pipefail

BINARY_NAME="citadel"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/citadel"
SERVICE_NAME="citadel-worker"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
NODE_DIR="/root/citadel-node"
LOG_FILE="/var/log/citadel-install.log"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
_c_reset="\033[0m"
_c_bold="\033[1m"
_c_green="\033[1;32m"
_c_yellow="\033[1;33m"
_c_red="\033[1;31m"

msg() {
    if [ -t 1 ]; then
        printf "${_c_green}==>${_c_reset} ${_c_bold}%s${_c_reset}\n" "$1" >&2
    else
        printf "==> %s\n" "$1" >&2
    fi
}

warn() {
    if [ -t 1 ]; then
        printf "${_c_yellow}WARNING:${_c_reset} %s\n" "$1" >&2
    else
        printf "WARNING: %s\n" "$1" >&2
    fi
}

err() {
    if [ -t 1 ]; then
        printf "${_c_red}ERROR:${_c_reset} %s\n" "$1" >&2
    else
        printf "ERROR: %s\n" "$1" >&2
    fi
}

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
    err "This script must be run as root. Try: sudo bash uninstall.sh"
    exit 1
fi

echo ""
msg "Citadel Node Uninstaller"
echo ""

# ---------------------------------------------------------------------------
# Stop and disable systemd service
# ---------------------------------------------------------------------------
if systemctl list-unit-files "${SERVICE_NAME}.service" &>/dev/null && \
   systemctl list-unit-files "${SERVICE_NAME}.service" 2>/dev/null | grep -q "$SERVICE_NAME"; then
    msg "Stopping ${SERVICE_NAME} service..."
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
    msg "Service stopped and disabled"
else
    msg "Service ${SERVICE_NAME} not found (already removed)"
fi

# ---------------------------------------------------------------------------
# Remove systemd unit file
# ---------------------------------------------------------------------------
if [ -f "$SERVICE_FILE" ]; then
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
    msg "Removed ${SERVICE_FILE}"
else
    msg "Unit file already removed"
fi

# ---------------------------------------------------------------------------
# Disconnect from AceTeam Network
# ---------------------------------------------------------------------------
if [ -x "${INSTALL_DIR}/${BINARY_NAME}" ]; then
    msg "Disconnecting from AceTeam Network..."
    "${INSTALL_DIR}/${BINARY_NAME}" logout 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Remove binary
# ---------------------------------------------------------------------------
if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
    rm -f "${INSTALL_DIR}/${BINARY_NAME}"
    msg "Removed ${INSTALL_DIR}/${BINARY_NAME}"
else
    msg "Binary already removed"
fi

# ---------------------------------------------------------------------------
# Remove config directory
# ---------------------------------------------------------------------------
if [ -d "$CONFIG_DIR" ]; then
    rm -rf "$CONFIG_DIR"
    msg "Removed ${CONFIG_DIR}"
else
    msg "Config directory already removed"
fi

# ---------------------------------------------------------------------------
# Remove node state directory
# ---------------------------------------------------------------------------
if [ -d "$NODE_DIR" ]; then
    rm -rf "$NODE_DIR"
    msg "Removed ${NODE_DIR}"
else
    msg "Node directory already removed"
fi

# ---------------------------------------------------------------------------
# Also check for user-local installs
# ---------------------------------------------------------------------------
for user_home in /home/*; do
    local_node_dir="${user_home}/citadel-node"
    local_bin="${user_home}/.local/bin/${BINARY_NAME}"

    if [ -d "$local_node_dir" ]; then
        rm -rf "$local_node_dir"
        msg "Removed ${local_node_dir}"
    fi

    if [ -f "$local_bin" ]; then
        rm -f "$local_bin"
        msg "Removed ${local_bin}"
    fi
done

# ---------------------------------------------------------------------------
# Remove install log
# ---------------------------------------------------------------------------
if [ -f "$LOG_FILE" ]; then
    rm -f "$LOG_FILE"
    msg "Removed ${LOG_FILE}"
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
echo ""
msg "Citadel has been removed from this machine."
echo ""
echo "  Not removed (by design):" >&2
echo "    - NVIDIA drivers" >&2
echo "    - Docker CE" >&2
echo "    - Docker images (run 'docker rmi vllm/vllm-openai:latest' to remove)" >&2
echo "" >&2
