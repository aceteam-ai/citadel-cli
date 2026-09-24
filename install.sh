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
preflight() {
    # Must be root
    if [ "$(id -u)" -ne 0 ]; then
        die "This installer must be run as root. Try: curl -fsSL https://get.aceteam.ai/citadel | sudo -E CITADEL_AUTHKEY=xxx bash"
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

    # E5 owns the data-preserving migration of running Docker nodes. Replacing
    # the old root unit here would strand its root-owned volumes and state.
    if [ -e "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
        die "A legacy system worker unit exists. Keep it running and use 'citadel runtime migrate' when E5 is available; this fresh-node installer will not replace it."
    fi
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

detect_gpu() {
    step "Detecting GPU"

    if grep -qs '^0x10de$' /sys/bus/pci/devices/*/vendor 2>/dev/null; then
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

install_podman() {
    step "Installing rootless Podman"
    apt-get update -qq >> "$LOG_FILE" 2>&1 || die "apt-get update failed"
    apt-get install -y -qq podman podman-compose uidmap fuse-overlayfs crun slirp4netns dbus-user-session git gnupg >> "$LOG_FILE" 2>&1 || die "Podman dependency installation failed"
    # Ubuntu 22.04 can use slirp4netns when passt/pasta is unavailable.
    if apt-cache show passt >/dev/null 2>&1; then
        apt-get install -y -qq passt >> "$LOG_FILE" 2>&1 || die "passt installation failed"
    fi
    if ! id "$SERVICE_USER" >/dev/null 2>&1; then
        useradd --create-home --shell /bin/bash "$SERVICE_USER" || die "Could not create ${SERVICE_USER} user"
    fi
    [ "$(getent passwd "$SERVICE_USER" | cut -d: -f6)" = "$SERVICE_HOME" ] || die "${SERVICE_USER} must have home ${SERVICE_HOME}"
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
    systemctl start "user@${uid}.service" || die "Could not start ${SERVICE_USER} user manager"
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
    usermod -aG video,render "$SERVICE_USER" || die "Could not grant ${SERVICE_USER} GPU groups"
    install -d -m 755 /etc/udev/rules.d /etc/cdi
    cat > /etc/udev/rules.d/70-citadel-nvidia.rules <<'RULE'
KERNEL=="nvidia[0-9]*", GROUP="video", MODE="0660"
KERNEL=="nvidiactl", GROUP="video", MODE="0660"
KERNEL=="nvidia-uvm*", GROUP="video", MODE="0660"
KERNEL=="nvidia-cap*", GROUP="video", MODE="0660"
RULE
    udevadm control --reload-rules || die "Could not reload GPU device rules"
    cat > /etc/systemd/system/citadel-nvidia-cdi.service <<'UNIT'
[Unit]
Description=Refresh Citadel NVIDIA CDI devices after driver initialization
After=systemd-udev-settle.service
ConditionPathExists=/dev/nvidiactl

[Service]
Type=oneshot
ExecStart=/usr/bin/nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable citadel-nvidia-cdi.service >> "$LOG_FILE" 2>&1 || die "Could not enable GPU CDI refresh"
    if nvidia-smi >/dev/null 2>&1; then
        udevadm trigger --subsystem-match=misc || true
        nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml >> "$LOG_FILE" 2>&1 || die "Could not generate NVIDIA CDI spec"
        nvidia-ctk cdi list >> "$LOG_FILE" 2>&1 || die "NVIDIA CDI devices unavailable"
    else
        warn "GPU driver is not active yet; generate /etc/cdi/nvidia.yaml after reboot with sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
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
setup_citadel() {
    step "Configuring Citadel node"

    mkdir -p "$CONFIG_DIR"

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
        as_service_user systemctl --user restart "$SERVICE_NAME" >> "$LOG_FILE" 2>&1 || die "Could not restart worker"
        ok "Worker restarted"
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

    if $HAS_GPU && ! nvidia-smi &>/dev/null; then
        printf "\n" >&2
        warn "NVIDIA drivers were installed but may need a reboot to activate."
        warn "Run: sudo reboot"
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    preflight
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
