#!/usr/bin/env bash
#
# Citadel Node Installer
#
# One-liner setup for fresh Ubuntu machines. Installs NVIDIA drivers (if GPU),
# rootless Podman, NVIDIA CDI, the citadel binary, a systemd user service,
# and pre-pulls inference and hosted-app runtime images.
#
# Usage:
#   curl -fsSL https://get.aceteam.ai/citadel | sudo -E CITADEL_AUTHKEY=xxx bash
#
# Idempotent - safe to run multiple times.

set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
REPO="aceteam-ai/citadel-cli"
BINARY_NAME="citadel"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/citadel"
LOG_FILE="/var/log/citadel-install.log"
VLLM_IMAGE="vllm/vllm-openai:latest"
SERVICE_NAME="citadel-worker"
SERVICE_USER="citadel"
SERVICE_HOME="/home/${SERVICE_USER}"
SERVICE_FILE="/etc/systemd/user/${SERVICE_NAME}.service"
SERVICE_USER_MARKER="/etc/citadel/rootless-worker-user"
ALREADY_READY=false

# ---------------------------------------------------------------------------
# Color helpers (only for terminal, plain text for log)
# ---------------------------------------------------------------------------
_color_ok=false
if [ -t 1 ]; then
    _color_ok=true
fi

_c_reset="\033[0m"
_c_bold="\033[1m"
_c_green="\033[1;32m"
_c_yellow="\033[1;33m"
_c_red="\033[1;31m"
_c_cyan="\033[1;36m"
_c_dim="\033[2m"

_ts() { date "+%Y-%m-%d %H:%M:%S"; }

log() {
    # Always write plain text to the log file
    echo "[$(_ts)] $*" >> "$LOG_FILE" 2>/dev/null
}

msg() {
    log "INFO  $1"
    if $_color_ok; then
        printf "${_c_green}==>${_c_reset} ${_c_bold}%s${_c_reset}\n" "$1" >&2
    else
        printf "==> %s\n" "$1" >&2
    fi
}

warn() {
    log "WARN  $1"
    if $_color_ok; then
        printf "${_c_yellow}WARNING:${_c_reset} %s\n" "$1" >&2
    else
        printf "WARNING: %s\n" "$1" >&2
    fi
}

err() {
    log "ERROR $1"
    if $_color_ok; then
        printf "${_c_red}ERROR:${_c_reset} %s\n" "$1" >&2
    else
        printf "ERROR: %s\n" "$1" >&2
    fi
}

die() {
    err "$1"
    exit 1
}

step() {
    log "STEP  $1"
    if $_color_ok; then
        printf "\n${_c_cyan}--- %s ---${_c_reset}\n" "$1" >&2
    else
        printf "\n--- %s ---\n" "$1" >&2
    fi
}

ok() {
    log "OK    $1"
    if $_color_ok; then
        printf "  ${_c_green}OK${_c_reset} %s\n" "$1" >&2
    else
        printf "  OK %s\n" "$1" >&2
    fi
}

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------
# resolve_node_config_dir mirrors network.GetNodeConfigDir()'s resolution
# priority for a root caller: the machine-global state pointer, then a global
# config.yaml node_config_dir (machine-wide, then SUDO_USER-local), then the
# owner-consistent home fallback. Read-only; used by preflight to refuse a
# populated foreign node dir before any host change (the Go path's guard in
# prepareLinuxPodmanProvision).
resolve_node_config_dir() {
    local pointer="${CONFIG_DIR}/state-dir" val cfg sudo_home=""
    if [ -f "$pointer" ] && [ ! -L "$pointer" ]; then
        val=$(tr -d '[:space:]' < "$pointer" 2>/dev/null)
        [ -n "$val" ] && { printf '%s\n' "$val"; return 0; }
    fi
    if [ -n "${SUDO_USER:-}" ]; then
        sudo_home=$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6)
    fi
    for cfg in "${CONFIG_DIR}/config.yaml" "${sudo_home:+${sudo_home}/.citadel-cli/config.yaml}"; do
        [ -n "$cfg" ] && [ -f "$cfg" ] || continue
        val=$(sed -n 's/^node_config_dir:[[:space:]]*//p' "$cfg" 2>/dev/null | head -1 | tr -d "\"' ")
        [ -n "$val" ] && { printf '%s\n' "$val"; return 0; }
    done
    if [ -n "$sudo_home" ]; then
        printf '%s/citadel-node\n' "$sudo_home"
    else
        printf '%s/citadel-node\n' "${HOME:-/root}"
    fi
}

preflight() {
    # Read-only E5 migration guard: do not even create a log or alter HOME on
    # an existing system worker. Both historical managed unit names count.
    local legacy_unit
    for legacy_unit in /etc/systemd/system/citadel-worker.service /etc/systemd/system/citadel.service; do
        if [ -e "$legacy_unit" ] || [ -L "$legacy_unit" ]; then
            printf 'ERROR: Existing system worker %s requires E5 migration; fresh installer made no changes.\n' "$legacy_unit" >&2
            return 1
        fi
    done
    # Must be root
    if [ "$(id -u)" -ne 0 ]; then
        die "This installer must be run as root. Try: curl -fsSL https://get.aceteam.ai/citadel | sudo -E CITADEL_AUTHKEY=xxx bash"
    fi

    # A healthy serving worker is a read-only no-op. An unsafe account or a
    # running but unhealthy worker needs an explicit drained repair, before
    # package, log, enrollment, or unit changes.
    if id "$SERVICE_USER" >/dev/null 2>&1; then
        verify_service_account
        if existing_rootless_worker; then
            ALREADY_READY=true
            return 0
        fi
    fi

    # A Jetson/L4T box needs its JetPack-matched nvidia-ctk; the generic upstream
    # toolkit does not fit. Fail here, before any package or host change, rather
    # than mid-install (moved out of install_nvidia_toolkit).
    if is_jetson && ! command -v nvidia-ctk >/dev/null 2>&1; then
        printf 'ERROR: Jetson/L4T needs its JetPack-matched NVIDIA toolkit (nvidia-ctk) installed before provisioning; refusing the generic upstream package. Install it, then re-run.\n' >&2
        return 1
    fi

    # Mirror prepareLinuxPodmanProvision's populated-foreign-state refusal
    # (init_podman_linux.go): if the node config dir a citadel process resolves
    # on this box is NOT the dedicated worker's dir and already holds state,
    # refuse rather than diverge or clobber. Read-only, before any change.
    local resolved_node_dir want_node_dir="${SERVICE_HOME}/citadel-node"
    resolved_node_dir=$(resolve_node_config_dir)
    if [ "$resolved_node_dir" != "$want_node_dir" ] && [ -d "$resolved_node_dir" ] && [ -n "$(ls -A "$resolved_node_dir" 2>/dev/null)" ]; then
        printf 'ERROR: Existing node state at %s requires explicit E5 migration before provisioning %s; fresh installer made no changes.\n' "$resolved_node_dir" "$want_node_dir" >&2
        return 1
    fi

    # Ensure HOME is /root so network state, config, and systemd service all agree
    export HOME=/root

    # Ensure log directory exists
    mkdir -p "$(dirname "$LOG_FILE")"
    touch "$LOG_FILE"
    log "--- Citadel installer started ---"

    # Check OS
    if [ ! -f /etc/os-release ]; then
        die "Cannot detect OS - /etc/os-release missing. This installer supports Ubuntu 22.04 and 24.04."
    fi

    . /etc/os-release

    if [ "$ID" != "ubuntu" ]; then
        die "Unsupported OS: $ID. This installer supports Ubuntu 22.04 and 24.04 only."
    fi

    case "$VERSION_ID" in
        22.04|24.04) ;;
        *) die "Unsupported Ubuntu version: $VERSION_ID. Supported: 22.04, 24.04." ;;
    esac

    ok "Ubuntu $VERSION_ID detected"

    # Check required commands
    for cmd in curl tar grep; do
        if ! command -v "$cmd" &>/dev/null; then
            die "Required command '$cmd' not found. Install it and retry."
        fi
    done

    # Detect architecture
    case "$(uname -m)" in
        x86_64|amd64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "Unsupported architecture: $(uname -m). Supported: amd64, arm64." ;;
    esac

    ok "Architecture: $ARCH"

}

# ---------------------------------------------------------------------------
# Authkey
# ---------------------------------------------------------------------------
resolve_authkey() {
    if [ -n "${CITADEL_AUTHKEY:-}" ]; then
        ok "Authkey provided via environment"
        return
    fi

    # When piped (curl | bash), stdin is the script. Read from /dev/tty.
    if [ -t 0 ] || [ -e /dev/tty ]; then
        printf "\n  Enter your Citadel authkey (from aceteam.ai/fabric): " >&2
        read -r CITADEL_AUTHKEY < /dev/tty || true
    fi

    if [ -z "${CITADEL_AUTHKEY:-}" ]; then
        die "No authkey provided. Set CITADEL_AUTHKEY or run interactively.\n  Usage: curl -fsSL https://get.aceteam.ai/citadel | sudo -E CITADEL_AUTHKEY=xxx bash"
    fi
}

# ---------------------------------------------------------------------------
# GPU detection
# ---------------------------------------------------------------------------
HAS_GPU=false
IS_JETSON=false

is_jetson() {
    if [ -s /etc/nv_tegra_release ]; then return 0; fi
    local model
    for model in /proc/device-tree/model /sys/firmware/devicetree/base/model; do
        if [ -r "$model" ] && grep -qiE 'jetson|tegra' "$model"; then return 0; fi
    done
    return 1
}

detect_gpu() {
    step "Detecting GPU"

    if is_jetson; then
        HAS_GPU=true
        IS_JETSON=true
        ok "Jetson/L4T GPU detected (no PCI or nvidia-smi required)"
    elif grep -qs '^0x10de$' /sys/bus/pci/devices/*/vendor 2>/dev/null; then
        HAS_GPU=true
        ok "NVIDIA GPU detected (via PCI sysfs)"
    elif lspci 2>/dev/null | grep -qi nvidia; then
        HAS_GPU=true
        ok "NVIDIA GPU detected"
    elif [ -d /proc/driver/nvidia/gpus ] && [ "$(ls /proc/driver/nvidia/gpus 2>/dev/null | wc -l)" -gt 0 ]; then
        HAS_GPU=true
        ok "NVIDIA GPU detected (via /proc)"
    else
        warn "No NVIDIA GPU detected - skipping GPU-related setup"
    fi
}

# ---------------------------------------------------------------------------
# NVIDIA drivers
# ---------------------------------------------------------------------------
install_nvidia_drivers() {
    if ! $HAS_GPU; then return 0; fi

    if $IS_JETSON; then
        ok "Keeping JetPack-provided NVIDIA driver and userspace on Jetson/L4T"
        return 0
    fi

    step "Installing NVIDIA drivers"

    if command -v nvidia-smi &>/dev/null; then
        local driver_ver
        driver_ver=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -1 || true)
        if [ -n "$driver_ver" ]; then
            ok "NVIDIA driver already installed (v${driver_ver})"
            return 0
        fi
    fi

    msg "Installing NVIDIA drivers via ubuntu-drivers..."
    if ! apt-get update -qq >> "$LOG_FILE" 2>&1; then
        warn "apt-get update failed, continuing anyway"
    fi

    if ! apt-get install -y -qq ubuntu-drivers-common >> "$LOG_FILE" 2>&1; then
        warn "Failed to install ubuntu-drivers-common - skipping NVIDIA driver install"
        return 0
    fi

    if ! ubuntu-drivers autoinstall >> "$LOG_FILE" 2>&1; then
        warn "NVIDIA driver install failed - GPU may not be usable until drivers are installed manually"
        warn "You can retry later: sudo ubuntu-drivers autoinstall && sudo reboot"
        return 0
    fi

    ok "NVIDIA drivers installed (reboot may be required to activate)"
}

# ---------------------------------------------------------------------------
# Rootless Podman
# ---------------------------------------------------------------------------
as_service_user() {
    local uid
    uid=$(id -u "$SERVICE_USER") || return 1
    runuser -u "$SERVICE_USER" -- env -u SUDO_USER -u SUDO_UID -u SUDO_GID HOME="$SERVICE_HOME" \
        XDG_RUNTIME_DIR="/run/user/${uid}" \
        DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" "$@"
}

verify_service_account() {
    local uid group sudo_listing marker_owner marker_mode marker_dir_owner marker_dir_mode
    uid=$(id -u "$SERVICE_USER") || die "Cannot inspect dedicated worker UID"
    [ "$uid" -ne 0 ] || die "Dedicated worker must not be root"
    [ "$(getent passwd "$SERVICE_USER" | cut -d: -f6)" = "$SERVICE_HOME" ] || die "Dedicated worker has an unexpected home"
    [ -d /etc/citadel ] && [ ! -L /etc/citadel ] || die "Worker marker directory is unsafe"
    marker_dir_owner=$(stat -c %u /etc/citadel) || die "Cannot inspect worker marker directory"
    marker_dir_mode=$(stat -c %a /etc/citadel) || die "Cannot inspect worker marker directory"
    [ "$marker_dir_owner" = 0 ] && [ "$((8#$marker_dir_mode & 8#22))" -eq 0 ] || die "Worker marker directory is writable by non-root"
    [ -f "$SERVICE_USER_MARKER" ] && [ ! -L "$SERVICE_USER_MARKER" ] || die "Dedicated worker has no trusted provisioning marker"
    marker_owner=$(stat -c %u "$SERVICE_USER_MARKER") || die "Cannot inspect worker marker ownership"
    marker_mode=$(stat -c %a "$SERVICE_USER_MARKER") || die "Cannot inspect worker marker mode"
    [ "$marker_owner" = 0 ] && [ "$marker_mode" = 600 ] &&
        [ "$(<"$SERVICE_USER_MARKER")" = "$uid" ] || die "Dedicated worker provisioning marker is invalid"
    for group in $(id -nG "$SERVICE_USER"); do
        case "$group" in sudo|wheel|docker) die "Dedicated ${SERVICE_USER} user has privileged ${group} membership" ;; esac
    done
    [ ! -e "/etc/sudoers.d/99-citadel-${SERVICE_USER}" ] || die "Dedicated worker has a legacy sudo grant"
    if command -v sudo >/dev/null 2>&1; then
        if sudo_listing=$(runuser -u "$SERVICE_USER" -- env LC_ALL=C sudo -n -l 2>&1); then
            die "Dedicated worker has effective sudo privileges"
        fi
        case "${sudo_listing,,}" in
            *"may run the following commands"*|*"nopasswd:"*) die "Dedicated worker has effective sudo privileges" ;;
            *"not allowed to run sudo"*|*"may not run sudo"*) ;;
            *) die "Cannot prove dedicated worker has no sudo privileges: ${sudo_listing}" ;;
        esac
    fi
}

host_gpu_present() {
    if is_jetson; then return 0; fi
    if grep -qs '^0x10de$' /sys/bus/pci/devices/*/vendor 2>/dev/null; then return 0; fi
    [ -d /proc/driver/nvidia/gpus ] && [ "$(ls /proc/driver/nvidia/gpus 2>/dev/null | wc -l)" -gt 0 ]
}

existing_rootless_worker() {
    local uid state active=0 unit manifest_owner
    uid=$(id -u "$SERVICE_USER") || die "Cannot inspect dedicated worker UID"
    state=$(systemctl show "user@${uid}.service" -p ActiveState --value) || die "Cannot inspect dedicated user manager"
    [ "$state" = active ] || return 1
    as_service_user systemctl --user show-environment >/dev/null || die "Cannot inspect dedicated user bus"
    for unit in "$SERVICE_NAME" citadel.service; do
        if as_service_user systemctl --user is-active --quiet "$unit"; then
            active=$((active+1))
        fi
    done
    [ "$active" -gt 0 ] || return 1
    [ "$active" -eq 1 ] || die "Multiple Citadel workers are active; drain before provisioning"
    [ -s "${SERVICE_HOME}/citadel-node/citadel.yaml" ] &&
        [ ! -L "${SERVICE_HOME}/citadel-node/citadel.yaml" ] || die "Active worker manifest is missing or unsafe"
    manifest_owner=$(stat -c %u "${SERVICE_HOME}/citadel-node/citadel.yaml") || die "Cannot inspect active worker manifest"
    [ "$manifest_owner" = "$uid" ] || die "Active worker manifest is not owned by ${SERVICE_USER}"
    if [ -e "${CONFIG_DIR}/state-dir" ] || [ -L "${CONFIG_DIR}/state-dir" ]; then
        [ -f "${CONFIG_DIR}/state-dir" ] && [ ! -L "${CONFIG_DIR}/state-dir" ] &&
            [ "$(<"${CONFIG_DIR}/state-dir")" = "${SERVICE_HOME}/citadel-node" ] ||
            die "Active worker state pointer does not resolve to the dedicated account; drain before repair"
    fi
    if host_gpu_present; then
        verify_user_manager "$uid" video render
    else
        verify_user_manager "$uid"
    fi
    as_service_user systemctl --user is-active --quiet podman.socket || die "Active worker has no rootless Podman socket"
    as_service_user podman info >/dev/null || die "Active worker cannot use rootless Podman"
    return 0
}

require_service_groups() {
    local group
    for group in "$@"; do
        getent group "$group" >/dev/null || die "Required GPU group ${group} is missing"
    done
}

verify_user_manager() {
    local uid="$1" group pid gid groups controllers
    shift
    controllers=$(<"/sys/fs/cgroup/user.slice/user-${uid}.slice/user@${uid}.service/cgroup.controllers") || die "Cannot read active user-manager delegation"
    for group in cpu memory pids; do
        [[ " ${controllers} " == *" ${group} "* ]] || die "Active user manager lacks delegated ${group} controller"
    done
    if [ "$#" -eq 0 ]; then return 0; fi
    pid=$(systemctl show "user@${uid}.service" -p MainPID --value) || die "Cannot inspect active user manager"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || die "Dedicated user manager is inactive"
    groups=$(sed -n 's/^Groups:[[:space:]]*//p' "/proc/${pid}/status") || die "Cannot inspect active user-manager groups"
    for group in "$@"; do
        gid=$(getent group "$group" | cut -d: -f3) || die "Required group ${group} is missing"
        [[ " ${groups} " == *" ${gid} "* ]] || die "Active user manager lacks supplementary group ${group}; restart it before worker startup"
    done
}

refresh_user_manager() {
    local uid="$1"
    shift
    if systemctl is-active --quiet "user@${uid}.service"; then
        if as_service_user systemctl --user is-active --quiet "$SERVICE_NAME" ||
           as_service_user systemctl --user is-active --quiet citadel.service; then
            die "Existing ${SERVICE_USER} worker is active; refusing to restart its user manager during reprovision"
        fi
        systemctl restart "user@${uid}.service" || die "Could not refresh ${SERVICE_USER} user manager"
    else
        systemctl start "user@${uid}.service" || die "Could not start ${SERVICE_USER} user manager"
    fi
    verify_user_manager "$uid" "$@"
}

ensure_subid_range() {
    local map_file="$1" kind="$2" start
    if awk -F: -v user="$SERVICE_USER" '$1 == user && $3 >= 65536 {found=1} END {exit !found}' "$map_file"; then
        return 0
    fi
    # Allocate beyond every range in both maps to avoid another user's IDs.
    start=$(awk -F: 'NF == 3 && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ {end=$2+$3; if (end>max) max=end} END {if (max<100000) max=100000; print int((max+65535)/65536)*65536}' /etc/subuid /etc/subgid)
    if [ "$kind" = uid ]; then
        usermod --add-subuids "${start}-$((start+65535))" "$SERVICE_USER" || die "Could not allocate subordinate UIDs"
    else
        usermod --add-subgids "${start}-$((start+65535))" "$SERVICE_USER" || die "Could not allocate subordinate GIDs"
    fi
}

# Rootless CDI GPU injection (nvidia.com/gpu=all) needs Podman >= 4.1. Ubuntu
# 22.04 ships 3.4.4, which passes `podman info` but cannot inject CDI devices
# rootless, so a GPU node would pass every check and then fail at first GPU
# container start. Parse the actual version and gate rather than trusting the OS
# or `podman info`.
podman_meets_cdi_floor() {
    local ver major minor
    ver=$(podman --version 2>/dev/null | awk '{print $3}')
    [ -n "$ver" ] || return 1
    major=${ver%%.*}
    minor=${ver#"${major}."}
    minor=${minor%%.*}
    case "$major" in ''|*[!0-9]*) return 1 ;; esac
    case "$minor" in ''|*[!0-9]*) minor=0 ;; esac
    [ "$major" -gt 4 ] && return 0
    [ "$major" -eq 4 ] && [ "$minor" -ge 1 ] && return 0
    return 1
}

install_podman() {
    step "Installing rootless Podman"
    apt-get update -qq >> "$LOG_FILE" 2>&1 || die "apt-get update failed"
    apt-get install -y -qq podman podman-compose uidmap fuse-overlayfs crun slirp4netns dbus-user-session git gnupg >> "$LOG_FILE" 2>&1 || die "Podman dependency installation failed"
    # Ubuntu 22.04 can use slirp4netns when passt/pasta is unavailable.
    if apt-cache show passt >/dev/null 2>&1; then
        apt-get install -y -qq passt >> "$LOG_FILE" 2>&1 || die "passt installation failed"
    fi
    # GPU nodes need Podman >= 4.1 for rootless CDI. Gate before creating the
    # dedicated user, subordinate IDs, delegation, or linger, so a refused GPU
    # node has only packages installed. CPU-only nodes provision on 3.4.4.
    if $HAS_GPU && ! podman_meets_cdi_floor; then
        die "GPU node needs Podman >= 4.1 for rootless CDI GPU injection (nvidia.com/gpu), but this system has Podman $(podman --version 2>/dev/null | awk '{print $3}' || echo unknown). Ubuntu 22.04 ships 3.4.4; provision GPU nodes on Ubuntu 24.04 (Podman 4.9+) or install Podman >= 4.1 manually, then re-run."
    fi
    if ! id "$SERVICE_USER" >/dev/null 2>&1; then
        useradd --create-home --shell /bin/bash "$SERVICE_USER" || die "Could not create ${SERVICE_USER} user"
        install -d -m 755 /etc/citadel
        [ -d /etc/citadel ] && [ ! -L /etc/citadel ] && [ "$(stat -c %u /etc/citadel)" = 0 ] || die "Worker marker directory is unsafe"
        ( set -C; id -u "$SERVICE_USER" > "$SERVICE_USER_MARKER" ) || die "Could not record dedicated worker provenance"
        chmod 600 "$SERVICE_USER_MARKER"
    fi
    verify_service_account
    ensure_subid_range /etc/subuid uid
    ensure_subid_range /etc/subgid gid

    install -d -m 755 /etc/systemd/system/user@.service.d
    cat > /etc/systemd/system/user@.service.d/50-citadel-delegate.conf <<'UNIT'
[Service]
Delegate=cpu memory pids
UNIT
    systemctl daemon-reload || die "Could not load cgroup delegation unit"
    loginctl enable-linger "$SERVICE_USER" || die "Could not enable linger for ${SERVICE_USER}"
    local uid
    uid=$(id -u "$SERVICE_USER")
    refresh_user_manager "$uid"
    as_service_user systemctl --user enable --now podman.socket >> "$LOG_FILE" 2>&1 || die "Could not enable rootless Podman socket"
    as_service_user podman info >> "$LOG_FILE" 2>&1 || die "Rootless Podman is not usable"
    ok "Rootless Podman ready for ${SERVICE_USER}"
}

# ---------------------------------------------------------------------------
# NVIDIA Container Toolkit
# ---------------------------------------------------------------------------
install_nvidia_toolkit() {
    if ! $HAS_GPU; then return 0; fi

    step "Installing NVIDIA Container Toolkit"

    if command -v nvidia-ctk >/dev/null 2>&1; then
        ok "NVIDIA Container Toolkit already installed"
    else
        if $IS_JETSON; then
            die "Jetson/L4T needs its JetPack-matched NVIDIA toolkit (nvidia-ctk); refusing generic upstream package"
        fi
        msg "Adding NVIDIA container toolkit repository..."

        # Add NVIDIA GPG key and repo
        if ! curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
             gpg --batch --yes --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg 2>> "$LOG_FILE"; then
            warn "Failed to add NVIDIA GPG key - skipping toolkit install"
            return 0
        fi

        if ! curl -fsSL "https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list" | \
             sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
             tee /etc/apt/sources.list.d/nvidia-container-toolkit.list > /dev/null 2>> "$LOG_FILE"; then
            warn "Failed to add NVIDIA repo - skipping toolkit install"
            return 0
        fi

        if ! apt-get update -qq >> "$LOG_FILE" 2>&1; then
            warn "apt-get update failed after adding NVIDIA repo"
        fi

        if ! apt-get install -y -qq nvidia-container-toolkit >> "$LOG_FILE" 2>&1; then
            warn "NVIDIA Container Toolkit installation failed - GPU containers may not work"
            return 0
        fi

        ok "NVIDIA Container Toolkit installed"
    fi

    command -v nvidia-ctk >/dev/null || die "NVIDIA toolkit installed without nvidia-ctk"
    # The worker owns only its own GPU access. The CDI spec is shared read-only
    # with Podman; never configure a Docker default runtime on a fresh node.
    require_service_groups video render
    usermod -aG video,render "$SERVICE_USER" || die "Could not grant ${SERVICE_USER} GPU groups"
    refresh_user_manager "$(id -u "$SERVICE_USER")" video render
    install -d -m 755 /etc/udev/rules.d /etc/cdi
    cat > /etc/udev/rules.d/70-citadel-nvidia.rules <<'RULE'
KERNEL=="nvidia[0-9]*", GROUP="video", MODE="0660"
KERNEL=="nvidiactl", GROUP="video", MODE="0660"
KERNEL=="nvidia-uvm*", GROUP="video", MODE="0660"
KERNEL=="nvidia-cap*", GROUP="video", MODE="0660"
RULE
    udevadm control --reload-rules || die "Could not reload GPU device rules"
    install -d -m 755 /usr/local/libexec
    cat > /usr/local/libexec/citadel-nvidia-cdi-refresh <<'SCRIPT'
#!/bin/sh
set -eu
for attempt in $(seq 1 30); do
    if nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml &&
       nvidia-ctk cdi list | grep -F 'nvidia.com/gpu' >/dev/null; then
        exit 0
    fi
    sleep 2
done
echo 'NVIDIA CDI devices unavailable after readiness retries' >&2
exit 1
SCRIPT
    chmod 755 /usr/local/libexec/citadel-nvidia-cdi-refresh
    cat > /etc/systemd/system/citadel-nvidia-cdi.service <<'UNIT'
[Unit]
Description=Refresh Citadel NVIDIA CDI devices after driver initialization
After=systemd-udev-settle.service

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/citadel-nvidia-cdi-refresh

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable citadel-nvidia-cdi.service >> "$LOG_FILE" 2>&1 || die "Could not enable GPU CDI refresh"
    if nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml >> "$LOG_FILE" 2>&1 &&
       nvidia-ctk cdi list | tee -a "$LOG_FILE" | grep -F 'nvidia.com/gpu' >/dev/null; then
        ok "NVIDIA CDI devices verified"
    else
        warn "GPU driver is not ready; boot refresh retries NVIDIA CDI without nvidia-smi"
    fi
    ok "NVIDIA CDI configured"
}

# ---------------------------------------------------------------------------
# Node-local tools the agent shells out to
# ---------------------------------------------------------------------------
# poppler-utils provides pdftoppm, the renderer behind the document_rasterize
# job (issue #675): a scanned PDF page has to be an image before an OCR model
# will look at it. The agent detects its absence and fails that job with an
# install instruction, so a node without it stays healthy for everything else.
install_node_tools() {
    step "Installing node tools"

    if command -v pdftoppm >/dev/null 2>&1; then
        ok "PDF renderer already installed ($(pdftoppm -v 2>&1 | head -1))"
        return 0
    fi

    if ! apt-get install -y -qq poppler-utils >> "$LOG_FILE" 2>&1; then
        warn "Failed to install poppler-utils - PDF rasterization jobs will report it as missing"
        return 0
    fi

    ok "PDF renderer installed (poppler-utils)"
}

# ---------------------------------------------------------------------------
# Download and install citadel binary
# ---------------------------------------------------------------------------
install_citadel_binary() {
    step "Installing Citadel CLI"

    # Check if already installed and up to date
    if [ -x "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        local current_ver
        current_ver=$("${INSTALL_DIR}/${BINARY_NAME}" version 2>/dev/null || echo "unknown")
        msg "Citadel already installed (${current_ver}) - checking for updates..."
    fi

    # Get latest version
    msg "Fetching latest release..."
    local latest_url="https://api.github.com/repos/${REPO}/releases/latest"
    local version
    version=$(curl -sSL "$latest_url" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')

    if [ -z "$version" ]; then
        die "Could not determine latest version. Check https://github.com/${REPO}/releases"
    fi

    msg "Latest version: ${version}"

    # Check if current version matches
    if [ -x "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        local current_ver
        current_ver=$("${INSTALL_DIR}/${BINARY_NAME}" version 2>/dev/null || echo "")
        if [ "$current_ver" = "$version" ] || [ "$current_ver" = "${version#v}" ]; then
            ok "Already at latest version (${version})"
            return 0
        fi
    fi

    local archive="${BINARY_NAME}_${version}_linux_${ARCH}.tar.gz"
    local checksum_file="checksums.txt"
    local base_url="https://github.com/${REPO}/releases/download/${version}"

    local tmp_dir
    tmp_dir=$(mktemp -d)

    msg "Downloading ${archive}..."
    if ! curl -fsSL -o "${tmp_dir}/${archive}" "${base_url}/${archive}"; then
        rm -rf "$tmp_dir"
        die "Failed to download ${archive}"
    fi

    if ! curl -fsSL -o "${tmp_dir}/${checksum_file}" "${base_url}/${checksum_file}"; then
        rm -rf "$tmp_dir"
        die "Failed to download checksums"
    fi

    # Verify checksum
    msg "Verifying checksum..."
    local expected
    expected=$(grep "$archive" "${tmp_dir}/${checksum_file}" | cut -d ' ' -f 1)
    if [ -z "$expected" ]; then
        rm -rf "$tmp_dir"
        die "No checksum found for ${archive}"
    fi

    local actual
    actual=$(sha256sum "${tmp_dir}/${archive}" | cut -d ' ' -f 1)
    if [ "$expected" != "$actual" ]; then
        rm -rf "$tmp_dir"
        die "Checksum mismatch - download may be corrupted"
    fi
    ok "Checksum verified"

    # Extract and install
    tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir"
    local binary
    binary=$(find "$tmp_dir" -type f -name "$BINARY_NAME" | head -1)
    if [ -z "$binary" ]; then
        rm -rf "$tmp_dir"
        die "Binary not found in archive"
    fi

    install -m 755 "$binary" "${INSTALL_DIR}/${BINARY_NAME}"
    rm -rf "$tmp_dir"

    ok "Citadel ${version} installed to ${INSTALL_DIR}/${BINARY_NAME}"
}

# ---------------------------------------------------------------------------
# Create config directory and run citadel init
# ---------------------------------------------------------------------------
# The dedicated worker enrolls as the non-root citadel user, which never writes
# the machine-global node-dir pointer. Without it a root caller (sudo citadel
# status/whoami, or a later sudo citadel init --provision) resolves via
# owner-home and diverges from the worker's /home/citadel/citadel-node. Write
# both convergence sources as root so every context on this box agrees:
#   - state-dir  : network.GetStateDir()'s highest-priority pointer (also the
#                  file existing_rootless_worker validates on a healthy rerun).
#   - config.yaml: node_config_dir, which findAndReadManifest reads (status/
#                  whoami manifest resolution). Merged, never clobbering keys.
write_machine_state_pointer() {
    local node_dir="${SERVICE_HOME}/citadel-node" cfg="${CONFIG_DIR}/config.yaml"
    install -d -m 755 "$CONFIG_DIR"
    printf '%s\n' "$node_dir" > "${CONFIG_DIR}/state-dir"
    chmod 644 "${CONFIG_DIR}/state-dir"
    if [ -f "$cfg" ] && grep -q '^node_config_dir:' "$cfg"; then
        sed -i "s#^node_config_dir:.*#node_config_dir: ${node_dir}#" "$cfg"
    else
        printf 'node_config_dir: %s\n' "$node_dir" >> "$cfg"
    fi
    chmod 600 "$cfg"
}

setup_citadel() {
    step "Configuring Citadel node"

    mkdir -p "$CONFIG_DIR"

    # Converge every invocation context on the dedicated worker's node dir
    # before any early return, so an already-enrolled pre-fix node still gets
    # the pointer on a re-run.
    write_machine_state_pointer

    # Skip init if already connected (idempotent)
    if as_service_user "${INSTALL_DIR}/${BINARY_NAME}" status --json 2>/dev/null | grep -q '"connected":true' 2>/dev/null; then
        ok "Node already connected to AceTeam Network"
        return 0
    fi

    # Check if there's existing network state (already initialized once).
    #
    # Always initialize under the same uid and HOME as the user worker.
    if [ -d "${SERVICE_HOME}/citadel-node/network" ] && [ "$(ls "${SERVICE_HOME}/citadel-node/network" 2>/dev/null | wc -l)" -gt 0 ]; then
        ok "Existing network state found - skipping init (authkey may be single-use)"
        return 0
    fi

    msg "Running citadel init..."
    if ! as_service_user "${INSTALL_DIR}/${BINARY_NAME}" init --authkey "${CITADEL_AUTHKEY}" >> "$LOG_FILE" 2>&1; then
        die "citadel init failed; the worker was not started. Check ${LOG_FILE} and retry enrollment."
    fi

    ok "Node initialized and connected to AceTeam Network"
}

# ---------------------------------------------------------------------------
# Systemd service
# ---------------------------------------------------------------------------
setup_systemd_service() {
    step "Setting up systemd service"

    install -d -m 755 /etc/systemd/user
    cat > "$SERVICE_FILE" <<UNIT
[Unit]
Description=Citadel Worker - AceTeam Sovereign Compute
After=podman.socket
Wants=podman.socket
# Defense in depth against a crash-loop self-DoS (#443): if the process keeps
# failing fast, enter a cooldown instead of a 10s restart storm.
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${BINARY_NAME} work
Restart=on-failure
# Exponential restart backoff (10s -> 5m). The worker also backs off in-process
# on a failed control-plane connect, so this is a secondary safety net (#443).
RestartSec=10
RestartSteps=5
RestartMaxDelaySec=300
Environment=HOME=${SERVICE_HOME}
WorkingDirectory=${SERVICE_HOME}

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=citadel-worker

# Resource limits
LimitNOFILE=65535
LimitNPROC=65535

[Install]
WantedBy=default.target
UNIT

    systemctl daemon-reload
    as_service_user systemctl --user daemon-reload >> "$LOG_FILE" 2>&1 || die "Could not reload user units"
    as_service_user systemctl --user enable "$SERVICE_NAME" >> "$LOG_FILE" 2>&1 || die "Could not enable user worker"

    ok "Systemd service ${SERVICE_NAME} created and enabled"
}

# ---------------------------------------------------------------------------
# Pre-pull inference and trusted hosted-app runtime images
# ---------------------------------------------------------------------------
prepull_vllm() {
    if ! $HAS_GPU; then
        warn "No GPU detected - skipping vLLM image pull"
        return 0
    fi

    step "Pre-pulling vLLM Podman image"

    if as_service_user podman image inspect "$VLLM_IMAGE" &>/dev/null; then
        ok "vLLM image already present"
        return 0
    fi

    msg "Pulling ${VLLM_IMAGE} (this may take a while)..."
    if ! as_service_user podman pull "$VLLM_IMAGE" >> "$LOG_FILE" 2>&1; then
        warn "Failed to pull vLLM image - you can pull it later as ${SERVICE_USER}: podman pull ${VLLM_IMAGE}"
        return 0
    fi

    ok "vLLM image pulled"
}

prepull_app_runtimes() {
    step "Pre-pulling trusted hosted-app runtime images"
    if ! as_service_user "${INSTALL_DIR}/${BINARY_NAME}" service catalog update >> "$LOG_FILE" 2>&1; then
        warn "Trusted catalog update unavailable; retry when connected"
        return 0
    fi
    if ! as_service_user "${INSTALL_DIR}/${BINARY_NAME}" service catalog pre-pull-runtimes >> "$LOG_FILE" 2>&1; then
        warn "Trusted runtime pre-pull unavailable; retry after catalog publication with: sudo -u ${SERVICE_USER} citadel service catalog pre-pull-runtimes"
    fi
}

# ---------------------------------------------------------------------------
# Start the worker
# ---------------------------------------------------------------------------
start_worker() {
    step "Starting Citadel worker"

    if as_service_user systemctl --user is-active --quiet "$SERVICE_NAME"; then
        die "Worker became active during provisioning; drain before an explicit update"
    else
        as_service_user systemctl --user start "$SERVICE_NAME" >> "$LOG_FILE" 2>&1 || die "Could not start worker"
        ok "Worker started"
    fi

    # Brief wait for service to settle
    sleep 2

    if as_service_user systemctl --user is-active --quiet "$SERVICE_NAME"; then
        ok "Worker is running"
    else
        warn "Worker may not have started cleanly. Check: journalctl --user -u ${SERVICE_NAME} -f"
    fi
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
    local node_name gpu_info ip_addr worker_status

    node_name=$(hostname)
    ip_addr=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "unknown")

    if as_service_user systemctl --user is-active --quiet "$SERVICE_NAME"; then
        worker_status="running"
    else
        worker_status="not running"
    fi

    if $HAS_GPU; then
        if command -v nvidia-smi &>/dev/null; then
            gpu_info=$(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | head -4 || echo "detected (driver may need reboot)")
        else
            gpu_info="detected (driver not yet loaded - reboot required)"
        fi
    else
        gpu_info="none"
    fi

    if $_color_ok; then
        printf "\n${_c_cyan}=====================================${_c_reset}\n" >&2
        printf "${_c_bold}  Citadel Node Setup Complete${_c_reset}\n" >&2
        printf "${_c_cyan}=====================================${_c_reset}\n" >&2
        printf "  ${_c_dim}Node:${_c_reset}    %s\n" "$node_name" >&2
        printf "  ${_c_dim}IP:${_c_reset}      %s\n" "$ip_addr" >&2
        printf "  ${_c_dim}GPU:${_c_reset}     %s\n" "$gpu_info" >&2
        printf "  ${_c_dim}Worker:${_c_reset}  %s\n" "$worker_status" >&2
        printf "  ${_c_dim}Log:${_c_reset}     %s\n" "$LOG_FILE" >&2
        printf "${_c_cyan}=====================================${_c_reset}\n\n" >&2
    else
        printf "\n=====================================\n" >&2
        printf "  Citadel Node Setup Complete\n" >&2
        printf "=====================================\n" >&2
        printf "  Node:    %s\n" "$node_name" >&2
        printf "  IP:      %s\n" "$ip_addr" >&2
        printf "  GPU:     %s\n" "$gpu_info" >&2
        printf "  Worker:  %s\n" "$worker_status" >&2
        printf "  Log:     %s\n" "$LOG_FILE" >&2
        printf "=====================================\n\n" >&2
    fi

    if [ "$worker_status" = "running" ]; then
        msg "Node is online and ready for work."
    fi
    msg "Useful commands:"
    printf "  citadel status        # check node health\n" >&2
    printf "  sudo -u %s journalctl --user -u %s -f  # follow worker logs\n" "$SERVICE_USER" "$SERVICE_NAME" >&2
    printf "  sudo -u %s systemctl --user restart %s # restart worker\n" "$SERVICE_USER" "$SERVICE_NAME" >&2

    if $HAS_GPU && ! $IS_JETSON && ! nvidia-smi &>/dev/null; then
        printf "\n" >&2
        warn "NVIDIA drivers were installed but may need a reboot to activate."
        warn "Run: sudo reboot"
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    preflight || exit 1
    if $ALREADY_READY; then
        printf 'Citadel rootless worker is healthy; provisioning is already complete.\n' >&2
        return 0
    fi
    resolve_authkey
    detect_gpu
    install_nvidia_drivers
    install_podman
    install_nvidia_toolkit
    install_node_tools
    install_citadel_binary
    setup_citadel
    setup_systemd_service
    prepull_vllm
    prepull_app_runtimes
    start_worker
    print_summary

    log "--- Citadel installer completed successfully ---"
}

main "$@"
