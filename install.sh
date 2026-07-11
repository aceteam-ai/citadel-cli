#!/usr/bin/env bash
#
# Citadel Node Installer
#
# One-liner setup for fresh Ubuntu machines. Installs NVIDIA drivers (if GPU),
# Docker CE, NVIDIA Container Toolkit, the citadel binary, systemd service,
# and pre-pulls the vLLM image.
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
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

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

    if lspci 2>/dev/null | grep -qi nvidia; then
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
# Docker CE
# ---------------------------------------------------------------------------
install_docker() {
    step "Installing Docker CE"

    if command -v docker &>/dev/null; then
        ok "Docker already installed ($(docker --version 2>/dev/null | head -1))"
    else
        msg "Installing Docker CE..."

        # Use Docker's official convenience script
        if ! apt-get update -qq >> "$LOG_FILE" 2>&1; then
            warn "apt-get update had errors, continuing"
        fi

        if ! apt-get install -y -qq ca-certificates curl gnupg >> "$LOG_FILE" 2>&1; then
            die "Failed to install Docker prerequisites"
        fi

        local docker_script
        docker_script=$(mktemp)
        if ! curl -fsSL https://get.docker.com -o "$docker_script"; then
            die "Failed to download Docker install script"
        fi

        if ! sh "$docker_script" >> "$LOG_FILE" 2>&1; then
            rm -f "$docker_script"
            die "Docker installation failed. Check $LOG_FILE for details."
        fi
        rm -f "$docker_script"

        ok "Docker CE installed"
    fi

    # Ensure Docker is running
    if ! systemctl is-active --quiet docker; then
        systemctl start docker >> "$LOG_FILE" 2>&1 || true
        systemctl enable docker >> "$LOG_FILE" 2>&1 || true
    fi

    ok "Docker daemon is running"
}

# ---------------------------------------------------------------------------
# NVIDIA Container Toolkit
# ---------------------------------------------------------------------------
install_nvidia_toolkit() {
    if ! $HAS_GPU; then return 0; fi

    step "Installing NVIDIA Container Toolkit"

    if dpkg -l 2>/dev/null | grep -q nvidia-container-toolkit; then
        ok "NVIDIA Container Toolkit already installed"
    else
        msg "Adding NVIDIA container toolkit repository..."

        # Add NVIDIA GPG key and repo
        if ! curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
             gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg 2>> "$LOG_FILE"; then
            warn "Failed to add NVIDIA GPG key - skipping toolkit install"
            return 0
        fi

        local dist
        dist=$(. /etc/os-release && echo "$ID$VERSION_ID")
        if ! curl -fsSL "https://nvidia.github.io/libnvidia-container/${dist}/libnvidia-container.list" | \
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

    # Configure Docker daemon for NVIDIA runtime
    msg "Configuring Docker NVIDIA runtime..."
    local daemon_json="/etc/docker/daemon.json"

    if [ -f "$daemon_json" ] && grep -q '"nvidia"' "$daemon_json" 2>/dev/null; then
        ok "Docker NVIDIA runtime already configured"
    else
        # Use nvidia-ctk to configure (handles merging with existing daemon.json)
        if command -v nvidia-ctk &>/dev/null; then
            nvidia-ctk runtime configure --runtime=docker >> "$LOG_FILE" 2>&1 || true
            # Set nvidia as default runtime for GPU workloads
            if [ -f "$daemon_json" ]; then
                # Add default-runtime if not present
                if ! grep -q '"default-runtime"' "$daemon_json"; then
                    local tmp
                    tmp=$(mktemp)
                    python3 -c "
import json, sys
with open('$daemon_json') as f:
    d = json.load(f)
d['default-runtime'] = 'nvidia'
with open('$tmp', 'w') as f:
    json.dump(d, f, indent=2)
" 2>/dev/null && mv "$tmp" "$daemon_json" || rm -f "$tmp"
                fi
            fi

            # Restart Docker to pick up changes
            systemctl restart docker >> "$LOG_FILE" 2>&1 || warn "Docker restart failed"
            ok "Docker configured with NVIDIA runtime"
        else
            warn "nvidia-ctk not found - Docker NVIDIA runtime not configured"
        fi
    fi
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
    if "${INSTALL_DIR}/${BINARY_NAME}" status --json 2>/dev/null | grep -q '"connected":true' 2>/dev/null; then
        ok "Node already connected to AceTeam Network"
        return 0
    fi

    # Check if there's existing network state (already initialized once)
    if [ -d "/root/citadel-node/network" ] && [ "$(ls /root/citadel-node/network 2>/dev/null | wc -l)" -gt 0 ]; then
        ok "Existing network state found - skipping init (authkey may be single-use)"
        return 0
    fi

    msg "Running citadel init..."
    if ! "${INSTALL_DIR}/${BINARY_NAME}" init --authkey "${CITADEL_AUTHKEY}" >> "$LOG_FILE" 2>&1; then
        warn "citadel init failed - you may need to run 'citadel init' manually after install"
        return 0
    fi

    ok "Node initialized and connected to AceTeam Network"
}

# ---------------------------------------------------------------------------
# Systemd service
# ---------------------------------------------------------------------------
setup_systemd_service() {
    step "Setting up systemd service"

    cat > "$SERVICE_FILE" <<UNIT
[Unit]
Description=Citadel Worker - AceTeam Sovereign Compute
After=network-online.target docker.service
Wants=network-online.target docker.service
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
Environment=HOME=/root
WorkingDirectory=/root

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=citadel-worker

# Resource limits
LimitNOFILE=65535
LimitNPROC=65535

[Install]
WantedBy=multi-user.target
UNIT

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" >> "$LOG_FILE" 2>&1

    ok "Systemd service ${SERVICE_NAME} created and enabled"
}

# ---------------------------------------------------------------------------
# Pre-pull vLLM image
# ---------------------------------------------------------------------------
prepull_vllm() {
    if ! $HAS_GPU; then
        warn "No GPU detected - skipping vLLM image pull"
        return 0
    fi

    step "Pre-pulling vLLM Docker image"

    if docker image inspect "$VLLM_IMAGE" &>/dev/null; then
        ok "vLLM image already present"
        return 0
    fi

    msg "Pulling ${VLLM_IMAGE} (this may take a while)..."
    if ! docker pull "$VLLM_IMAGE" >> "$LOG_FILE" 2>&1; then
        warn "Failed to pull vLLM image - you can pull it later: docker pull ${VLLM_IMAGE}"
        return 0
    fi

    ok "vLLM image pulled"
}

# ---------------------------------------------------------------------------
# Start the worker
# ---------------------------------------------------------------------------
start_worker() {
    step "Starting Citadel worker"

    if systemctl is-active --quiet "$SERVICE_NAME"; then
        systemctl restart "$SERVICE_NAME" >> "$LOG_FILE" 2>&1
        ok "Worker restarted"
    else
        systemctl start "$SERVICE_NAME" >> "$LOG_FILE" 2>&1 || true
        ok "Worker started"
    fi

    # Brief wait for service to settle
    sleep 2

    if systemctl is-active --quiet "$SERVICE_NAME"; then
        ok "Worker is running"
    else
        warn "Worker may not have started cleanly. Check: journalctl -u ${SERVICE_NAME} -f"
    fi
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
    local node_name gpu_info ip_addr worker_status

    node_name=$(hostname)
    ip_addr=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "unknown")

    if systemctl is-active --quiet "$SERVICE_NAME"; then
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
    printf "  journalctl -u %s -f  # follow worker logs\n" "$SERVICE_NAME" >&2
    printf "  systemctl restart %s # restart worker\n" "$SERVICE_NAME" >&2

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
    install_docker
    install_nvidia_toolkit
    install_citadel_binary
    setup_citadel
    setup_systemd_service
    prepull_vllm
    start_worker
    print_summary

    log "--- Citadel installer completed successfully ---"
}

main "$@"
