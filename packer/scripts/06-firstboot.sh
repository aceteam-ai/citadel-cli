#!/bin/bash
# 06-firstboot.sh - Install a one-shot first-boot service
#
# On first boot of a cloned VM, this service:
#   1. Reads the authkey from /etc/citadel/authkey (injected by deploy script)
#   2. Runs "citadel init --authkey <key>" to join the network
#   3. Copies the generated manifest to /etc/citadel/ for the worker service
#   4. Generates GPU CDI and starts the rootless user worker
#   5. Disables itself so it never runs again
set -euo pipefail

echo "==> Installing first-boot service..."

# ---------------------------------------------------------------------------
# The first-boot script itself
# ---------------------------------------------------------------------------

mkdir -p /opt/citadel

cat > /opt/citadel/firstboot.sh << 'SCRIPT'
#!/bin/bash
# Citadel first-boot initialization
# Runs once on first VM clone boot, then disables itself.
set -euo pipefail

AUTHKEY_FILE="/etc/citadel/authkey"
MANIFEST_DIR="/etc/citadel"
LOG_TAG="citadel-firstboot"

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $*" | tee -a /var/log/citadel-firstboot.log
    logger -t "${LOG_TAG}" "$*"
}

log "Starting Citadel first-boot initialization..."

# -----------------------------------------------------------------------
# 1. Read authkey
# -----------------------------------------------------------------------
AUTHKEY=""

if [ -f "${AUTHKEY_FILE}" ]; then
    AUTHKEY=$(tr -d '[:space:]' < "${AUTHKEY_FILE}")
    log "Authkey found in ${AUTHKEY_FILE}"
fi

if [ -z "${AUTHKEY}" ]; then
    log "WARNING: No authkey found at ${AUTHKEY_FILE}."
    log "The node cannot join the network automatically."
    log "To initialize manually, run: citadel init --authkey <your-key>"
    log "Then: sudo -u citadel systemctl --user enable --now citadel-worker.service"
    # Don't fail -- the VM is still usable, just needs manual init
    systemctl disable citadel-firstboot.service
    exit 0
fi

# Validate authkey format to prevent shell injection.
# Tailscale/Headscale authkeys contain only alphanumerics and hyphens.
if ! echo "${AUTHKEY}" | grep -qE '^[A-Za-z0-9-]+$'; then
    log "ERROR: Authkey contains invalid characters. Refusing to proceed."
    exit 1
fi

# -----------------------------------------------------------------------
# 2. Run citadel init (network join only, no --provision)
# -----------------------------------------------------------------------
log "Running citadel init..."

# Run as the citadel user so state goes to /home/citadel/.citadel-node/
if su - citadel -c "citadel init --authkey '${AUTHKEY}'" 2>&1 | tee -a /var/log/citadel-firstboot.log; then
    log "citadel init succeeded."
else
    log "ERROR: citadel init failed (exit code $?)."
    log "Check /var/log/citadel-firstboot.log for details."
    exit 1
fi

# -----------------------------------------------------------------------
# 3. Copy manifest to /etc/citadel/ for the systemd worker service
# -----------------------------------------------------------------------
CITADEL_HOME="/home/citadel"
if [ -f "${CITADEL_HOME}/citadel-node/citadel.yaml" ]; then
    cp "${CITADEL_HOME}/citadel-node/citadel.yaml" "${MANIFEST_DIR}/citadel.yaml"
    chown citadel:citadel "${MANIFEST_DIR}/citadel.yaml"
    log "Manifest copied to ${MANIFEST_DIR}/citadel.yaml"
elif [ -f "${CITADEL_HOME}/citadel.yaml" ]; then
    cp "${CITADEL_HOME}/citadel.yaml" "${MANIFEST_DIR}/citadel.yaml"
    chown citadel:citadel "${MANIFEST_DIR}/citadel.yaml"
    log "Manifest copied to ${MANIFEST_DIR}/citadel.yaml"
else
    log "ERROR: No manifest found after init. Keeping the authkey for a retry."
    exit 1
fi

# -----------------------------------------------------------------------
# 4. Enable and start the worker
# -----------------------------------------------------------------------
log "Preparing rootless Podman and starting the Citadel user worker..."
citadel_uid=$(id -u citadel)
loginctl enable-linger citadel
systemctl start "user@${citadel_uid}.service"
as_citadel() {
    runuser -u citadel -- env HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${citadel_uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${citadel_uid}/bus" "$@"
}
as_citadel systemctl --user enable --now podman.socket
as_citadel podman info >/dev/null
has_nvidia_hardware() {
    if [ -s /etc/nv_tegra_release ]; then return 0; fi
    if grep -qiE 'jetson|tegra' /proc/device-tree/model /sys/firmware/devicetree/base/model 2>/dev/null; then return 0; fi
    if grep -qs '^0x10de$' /sys/bus/pci/devices/*/vendor 2>/dev/null; then return 0; fi
    [ -d /proc/driver/nvidia/gpus ] && [ "$(ls /proc/driver/nvidia/gpus 2>/dev/null | wc -l)" -gt 0 ]
}
if has_nvidia_hardware; then
    command -v nvidia-ctk >/dev/null 2>&1 || { log "ERROR: GPU present but NVIDIA CDI toolkit is missing."; exit 1; }
    getent group video >/dev/null && getent group render >/dev/null || { log "ERROR: GPU access groups are missing."; exit 1; }
    usermod -aG video,render citadel
    # The image build may have had no passthrough GPU. Refresh the user
    # manager now, before first enabling its worker, so it inherits GPU groups.
    systemctl restart "user@${citadel_uid}.service"
    as_citadel systemctl --user enable --now podman.socket
    install -d -m 755 /etc/udev/rules.d
    cat > /etc/udev/rules.d/70-citadel-nvidia.rules <<'RULE'
KERNEL=="nvidia[0-9]*", GROUP="video", MODE="0660"
KERNEL=="nvidiactl", GROUP="video", MODE="0660"
KERNEL=="nvidia-uvm*", GROUP="video", MODE="0660"
KERNEL=="nvidia-cap*", GROUP="video", MODE="0660"
RULE
    udevadm control --reload-rules
    install -d -m 755 /etc/cdi
    if nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml &&
       nvidia-ctk cdi list | grep -F 'nvidia.com/gpu' >/dev/null; then
        log "NVIDIA CDI devices verified."
    else
        log "NVIDIA CDI pending; boot refresh service retries driver readiness."
    fi
fi
as_citadel systemctl --user daemon-reload
as_citadel systemctl --user enable --now citadel-worker.service
log "Citadel user worker started."

# -----------------------------------------------------------------------
# 5. Clean up authkey and disable this service
# -----------------------------------------------------------------------
rm -f "${AUTHKEY_FILE}"
log "Authkey file removed."

systemctl disable citadel-firstboot.service
log "First-boot service disabled. Initialization complete."
SCRIPT

chmod 755 /opt/citadel/firstboot.sh

# ---------------------------------------------------------------------------
# Systemd one-shot service
# ---------------------------------------------------------------------------

cat > /etc/systemd/system/citadel-firstboot.service << 'UNIT'
[Unit]
Description=Citadel First-Boot Initialization
After=network-online.target cloud-init.target
Wants=network-online.target

# Only run if the authkey file exists or if the manifest hasn't been created yet
ConditionPathExists=!/etc/citadel/.firstboot-done

[Service]
Type=oneshot
ExecStart=/opt/citadel/firstboot.sh
ExecStartPost=/usr/bin/touch /etc/citadel/.firstboot-done
RemainAfterExit=false
StandardOutput=journal
StandardError=journal
SyslogIdentifier=citadel-firstboot

[Install]
WantedBy=multi-user.target
UNIT

# Enable the first-boot service so it runs on first real boot
systemctl daemon-reload
systemctl enable citadel-firstboot.service

echo "==> First-boot service installed and enabled."
