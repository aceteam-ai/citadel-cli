#!/bin/bash
# Bake the rootless Podman stack into Citadel OS.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

worker_marker_dir_safe() {
    local dir="$1" owner="$2" mode
    [ -d "$dir" ] && [ ! -L "$dir" ] || return 1
    [ "$(stat -c %u "$dir")" = "$owner" ] || return 1
    mode=$(stat -c %a "$dir") || return 1
    [ "$((8#$mode & 8#22))" -eq 0 ]
}
worker_marker_safe() {
    local marker="$1" uid="$2" owner="$3"
    worker_marker_dir_safe "$(dirname "$marker")" "$owner" || return 1
    [ -f "$marker" ] && [ ! -L "$marker" ] || return 1
    [ "$(stat -c %u "$marker")" = "$owner" ] &&
        [ "$(stat -c %a "$marker")" = 600 ] &&
        [ "$(<"$marker")" = "$uid" ]
}
# End independently testable marker-boundary functions.
if id citadel >/dev/null 2>&1; then
    worker_marker_safe /etc/citadel/rootless-worker-user "$(id -u citadel)" 0 || {
            echo 'ERROR: Refusing pre-existing citadel account without trusted provenance' >&2; exit 1;
        }
else
    if [ -e /etc/citadel ] || [ -L /etc/citadel ]; then
        worker_marker_dir_safe /etc/citadel 0 || { echo 'ERROR: Worker marker directory is unsafe' >&2; exit 1; }
    else
        install -d -m 755 /etc/citadel
    fi
    useradd --create-home --shell /bin/bash citadel
    worker_marker_dir_safe /etc/citadel 0 || {
        echo 'ERROR: Worker marker directory is unsafe' >&2; exit 1;
    }
    ( umask 077; set -C; id -u citadel > /etc/citadel/rootless-worker-user )
    chmod 600 /etc/citadel/rootless-worker-user
fi
test "$(getent passwd citadel | cut -d: -f6)" = /home/citadel
for group in $(id -nG citadel); do
    case "$group" in sudo|wheel|docker) echo "ERROR: dedicated citadel user has privileged $group membership" >&2; exit 1 ;; esac
done
if command -v sudo >/dev/null 2>&1; then
    if sudo_listing=$(runuser -u citadel -- env LC_ALL=C sudo -n -l 2>&1); then
        echo 'ERROR: Dedicated citadel account has sudo grants' >&2; exit 1
    fi
    case "${sudo_listing,,}" in
        *"may run the following commands"*|*"nopasswd:"*) echo 'ERROR: Dedicated citadel account has sudo grants' >&2; exit 1 ;;
        *"not allowed to run sudo"*|*"may not run sudo"*) ;;
        *) echo 'ERROR: Cannot prove citadel account has no sudo grants' >&2; exit 1 ;;
    esac
fi
apt-get update -y
apt-get install -y --no-install-recommends podman podman-compose uidmap fuse-overlayfs crun slirp4netns dbus-user-session
if apt-cache show passt >/dev/null 2>&1; then
    apt-get install -y --no-install-recommends passt
fi
ensure_subid() {
    local file="$1" type="$2" start
    if awk -F: '$1=="citadel" && $3>=65536 {found=1} END {exit !found}' "$file"; then return; fi
    start=$(awk -F: 'NF==3 && $2~/^[0-9]+$/ && $3~/^[0-9]+$/ {end=$2+$3;if(end>max)max=end} END {if(max<100000)max=100000;print int((max+65535)/65536)*65536}' /etc/subuid /etc/subgid)
    if [ "$type" = uid ]; then
        usermod --add-subuids "${start}-$((start+65535))" citadel
    else
        usermod --add-subgids "${start}-$((start+65535))" citadel
    fi
}
ensure_subid /etc/subuid uid
ensure_subid /etc/subgid gid

install -d -m 755 /etc/systemd/system/user@.service.d
cat > /etc/systemd/system/user@.service.d/50-citadel-delegate.conf <<'UNIT'
[Service]
Delegate=cpu memory pids
UNIT
systemctl daemon-reload
loginctl enable-linger citadel
citadel_uid=$(id -u citadel)
verify_user_manager() {
    local controller controllers group gid pid groups
    controllers=$(<"/sys/fs/cgroup/user.slice/user-${citadel_uid}.slice/user@${citadel_uid}.service/cgroup.controllers")
    for controller in cpu memory pids; do
        [[ " ${controllers} " == *" ${controller} "* ]] || { echo "ERROR: Missing delegated ${controller} controller" >&2; exit 1; }
    done
    if [ "$#" -eq 0 ]; then return; fi
    pid=$(systemctl show "user@${citadel_uid}.service" -p MainPID --value)
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || { echo 'ERROR: No active user manager PID' >&2; exit 1; }
    groups=$(sed -n 's/^Groups:[[:space:]]*//p' "/proc/${pid}/status")
    for group in "$@"; do
        gid=$(getent group "$group" | cut -d: -f3)
        [[ " ${groups} " == *" ${gid} "* ]] || { echo "ERROR: Active manager missing $group group" >&2; exit 1; }
    done
}
systemctl restart "user@${citadel_uid}.service"
verify_user_manager
runuser -u citadel -- env HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${citadel_uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${citadel_uid}/bus" systemctl --user enable --now podman.socket
runuser -u citadel -- env HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${citadel_uid}" podman info >/dev/null

# The driver loads only on the cloned VM's first boot; firstboot generates CDI.
is_jetson=false
if [ -s /etc/nv_tegra_release ] || grep -qiE 'jetson|tegra' /proc/device-tree/model /sys/firmware/devicetree/base/model 2>/dev/null; then
    is_jetson=true
fi
if [ "$is_jetson" = true ]; then
    command -v nvidia-ctk >/dev/null || { echo 'ERROR: Jetson needs its JetPack-matched nvidia-ctk toolkit' >&2; exit 1; }
elif [ "$(uname -m)" != aarch64 ] && [ "$(uname -m)" != arm64 ]; then
    curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey |
        gpg --batch --yes --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
    curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list |
        sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
        > /etc/apt/sources.list.d/nvidia-container-toolkit.list
    apt-get update -y
    apt-get install -y --no-install-recommends nvidia-container-toolkit
fi
has_nvidia_hardware() {
    if [ "$is_jetson" = true ]; then return 0; fi
    if grep -qs '^0x10de$' /sys/bus/pci/devices/*/vendor 2>/dev/null; then return 0; fi
    [ -d /proc/driver/nvidia/gpus ] && [ "$(ls /proc/driver/nvidia/gpus 2>/dev/null | wc -l)" -gt 0 ]
}
if command -v nvidia-ctk >/dev/null; then
if has_nvidia_hardware; then
for group in video render; do
    getent group "$group" >/dev/null || { echo "ERROR: Required GPU group $group is missing" >&2; exit 1; }
done
usermod -aG video,render citadel
systemctl restart "user@${citadel_uid}.service"
verify_user_manager video render
cat > /etc/udev/rules.d/70-citadel-nvidia.rules <<'RULE'
KERNEL=="nvidia[0-9]*", GROUP="video", MODE="0660"
KERNEL=="nvidiactl", GROUP="video", MODE="0660"
KERNEL=="nvidia-uvm*", GROUP="video", MODE="0660"
KERNEL=="nvidia-cap*", GROUP="video", MODE="0660"
RULE
udevadm control --reload-rules
install -d -m 755 /usr/local/libexec /etc/cdi
cat > /usr/local/libexec/citadel-nvidia-cdi-refresh <<'SCRIPT'
#!/bin/sh
set -eu
has_nvidia_hardware() {
    if [ -s /etc/nv_tegra_release ]; then return 0; fi
    if grep -qiE 'jetson|tegra' /proc/device-tree/model /sys/firmware/devicetree/base/model 2>/dev/null; then return 0; fi
    if grep -qs '^0x10de$' /sys/bus/pci/devices/*/vendor 2>/dev/null; then return 0; fi
    [ -d /proc/driver/nvidia/gpus ] && [ "$(ls /proc/driver/nvidia/gpus 2>/dev/null | wc -l)" -gt 0 ]
}
if ! has_nvidia_hardware; then
    exit 0
fi
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
systemctl enable citadel-nvidia-cdi.service
fi
fi
podman --version
apt-get clean
rm -rf /var/lib/apt/lists/*
