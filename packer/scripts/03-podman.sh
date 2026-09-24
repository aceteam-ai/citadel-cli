#!/bin/bash
# Bake the rootless Podman stack into Citadel OS.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

apt-get update -y
apt-get install -y --no-install-recommends podman podman-compose uidmap fuse-overlayfs crun slirp4netns dbus-user-session
if apt-cache show passt >/dev/null 2>&1; then
    apt-get install -y --no-install-recommends passt
fi
id citadel >/dev/null 2>&1 || useradd --create-home --shell /bin/bash citadel
test "$(getent passwd citadel | cut -d: -f6)" = /home/citadel
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
systemctl start "user@${citadel_uid}.service"
runuser -u citadel -- env HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${citadel_uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${citadel_uid}/bus" systemctl --user enable --now podman.socket
runuser -u citadel -- env HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${citadel_uid}" podman info >/dev/null

# The driver loads only on the cloned VM's first boot; firstboot generates CDI.
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey |
    gpg --batch --yes --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list |
    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt-get update -y
apt-get install -y --no-install-recommends nvidia-container-toolkit
usermod -aG video,render citadel
cat > /etc/udev/rules.d/70-citadel-nvidia.rules <<'RULE'
KERNEL=="nvidia[0-9]*", GROUP="video", MODE="0660"
KERNEL=="nvidiactl", GROUP="video", MODE="0660"
KERNEL=="nvidia-uvm*", GROUP="video", MODE="0660"
KERNEL=="nvidia-cap*", GROUP="video", MODE="0660"
RULE
udevadm control --reload-rules
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
systemctl enable citadel-nvidia-cdi.service
podman --version
apt-get clean
rm -rf /var/lib/apt/lists/*
