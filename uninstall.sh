#!/usr/bin/env bash
#
# Citadel Node Uninstaller
#
# Removes the Citadel worker (rootless user worker or legacy system worker),
# the dedicated citadel user, binary, and configuration.
# Does NOT uninstall NVIDIA drivers, Podman, or Docker.
#
# Usage:
#   sudo bash uninstall.sh

set -uo pipefail

BINARY_NAME="citadel"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/citadel"
SERVICE_NAME="citadel-worker"
# Legacy Docker-era system unit (old installer).
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
# Rootless installer artifacts.
USER_SERVICE_FILE="/etc/systemd/user/${SERVICE_NAME}.service"
DELEGATE_DROPIN="/etc/systemd/system/user@.service.d/50-citadel-delegate.conf"
CDI_UNIT="/etc/systemd/system/citadel-nvidia-cdi.service"
CDI_SCRIPT="/usr/local/libexec/citadel-nvidia-cdi-refresh"
UDEV_RULE="/etc/udev/rules.d/70-citadel-nvidia.rules"
WORKER_MARKER="${CONFIG_DIR}/rootless-worker-user"
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

# Run a command as the dedicated citadel user against its rootless user bus.
as_citadel_user() {
    local uid
    uid=$(id -u citadel) || return 1
    runuser -u citadel -- env -u SUDO_USER -u SUDO_UID -u SUDO_GID \
        HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${uid}" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" "$@"
}

# The citadel account is safe to delete only when Citadel's trusted provenance
# marker proves the installer created it (a root-owned regular file whose
# content is the account's own UID). A pre-existing human "citadel" account has
# no such marker and must never be removed.
rootless_worker_marker_valid() {
    local uid content
    id citadel >/dev/null 2>&1 || return 1
    uid=$(id -u citadel) || return 1
    [ -f "$WORKER_MARKER" ] && [ ! -L "$WORKER_MARKER" ] || return 1
    [ "$(stat -c %u "$WORKER_MARKER" 2>/dev/null)" = "0" ] || return 1
    content=$(tr -d '[:space:]' < "$WORKER_MARKER" 2>/dev/null)
    [ "$content" = "$uid" ]
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

# Decide up front whether the citadel account is ours to remove -- BEFORE the
# config dir (which holds the marker) is deleted below.
MARKER_VALID=false
if rootless_worker_marker_valid; then
    MARKER_VALID=true
fi

# ---------------------------------------------------------------------------
# Stop the rootless user worker (dedicated citadel account)
# ---------------------------------------------------------------------------
if $MARKER_VALID; then
    citadel_uid=$(id -u citadel)
    msg "Stopping rootless citadel worker..."
    as_citadel_user systemctl --user disable --now "${SERVICE_NAME}.service" 2>/dev/null || true
    as_citadel_user systemctl --user disable --now citadel.service 2>/dev/null || true
    if [ -x "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        as_citadel_user "${INSTALL_DIR}/${BINARY_NAME}" logout 2>/dev/null || true
    fi
    # Clear rootless container storage so its fuse mounts don't block userdel -r.
    as_citadel_user systemctl --user disable --now podman.socket 2>/dev/null || true
    as_citadel_user podman system reset -f 2>/dev/null || true
    loginctl disable-linger citadel 2>/dev/null || true
    systemctl stop "user@${citadel_uid}.service" 2>/dev/null || true
    msg "Rootless worker stopped"
fi

# ---------------------------------------------------------------------------
# Stop and remove the legacy system worker (old Docker-era installer)
# ---------------------------------------------------------------------------
if systemctl list-unit-files "${SERVICE_NAME}.service" 2>/dev/null | grep -q "$SERVICE_NAME"; then
    msg "Stopping ${SERVICE_NAME} system service..."
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
fi
if [ -f "$SERVICE_FILE" ]; then
    rm -f "$SERVICE_FILE"
    msg "Removed ${SERVICE_FILE}"
fi

# ---------------------------------------------------------------------------
# Remove the rootless user unit and GPU provisioning artifacts
# ---------------------------------------------------------------------------
if [ -f "$USER_SERVICE_FILE" ]; then
    rm -f "$USER_SERVICE_FILE"
    msg "Removed ${USER_SERVICE_FILE}"
fi

if [ -f "$CDI_UNIT" ]; then
    systemctl disable citadel-nvidia-cdi.service 2>/dev/null || true
    rm -f "$CDI_UNIT"
    msg "Removed ${CDI_UNIT}"
fi
if [ -f "$CDI_SCRIPT" ]; then
    rm -f "$CDI_SCRIPT"
    msg "Removed ${CDI_SCRIPT}"
fi

if [ -f "$UDEV_RULE" ]; then
    rm -f "$UDEV_RULE"
    udevadm control --reload-rules 2>/dev/null || true
    msg "Removed ${UDEV_RULE}"
fi

if [ -f "$DELEGATE_DROPIN" ]; then
    rm -f "$DELEGATE_DROPIN"
    rmdir --ignore-fail-on-non-empty /etc/systemd/system/user@.service.d 2>/dev/null || true
    msg "Removed ${DELEGATE_DROPIN}"
fi

systemctl daemon-reload 2>/dev/null || true

# ---------------------------------------------------------------------------
# Disconnect a legacy root-owned install from AceTeam Network
# ---------------------------------------------------------------------------
if ! $MARKER_VALID && [ -x "${INSTALL_DIR}/${BINARY_NAME}" ]; then
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
# Remove the dedicated citadel user (only when it is provably Citadel's)
# ---------------------------------------------------------------------------
if $MARKER_VALID; then
    if userdel -r citadel 2>/dev/null; then
        msg "Removed dedicated citadel worker user and its home"
    else
        warn "Could not fully remove the citadel user (active processes or mounts); remove it manually: sudo userdel -r citadel"
    fi
    # userdel prunes /etc/subuid and /etc/subgid on modern shadow-utils; strip
    # any residual citadel ranges explicitly so the subordinate IDs are gone.
    sed -i '/^citadel:/d' /etc/subuid /etc/subgid 2>/dev/null || true
elif id citadel >/dev/null 2>&1; then
    warn "A 'citadel' user exists without Citadel's trusted provisioning marker; leaving the account untouched."
fi

# ---------------------------------------------------------------------------
# Remove config directory (holds the marker, so this runs after the check)
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
echo "    - Podman / Docker" >&2
echo "    - Container images (as citadel: 'podman rmi vllm/vllm-openai:latest')" >&2
echo "" >&2
