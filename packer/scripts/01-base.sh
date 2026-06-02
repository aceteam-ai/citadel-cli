#!/bin/bash
# 01-base.sh - Install essential system packages
set -euo pipefail

echo "==> Installing base packages..."

# Wait for any dpkg locks to clear (common in cloud images on first boot)
wait_for_apt() {
    local attempts=0
    while fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 || \
          fuser /var/lib/apt/lists/lock >/dev/null 2>&1; do
        if [ $attempts -ge 30 ]; then
            echo "ERROR: Timed out waiting for apt locks"
            exit 1
        fi
        echo "Waiting for apt locks to clear... ($attempts)"
        sleep 5
        attempts=$((attempts + 1))
    done
}

wait_for_apt

export DEBIAN_FRONTEND=noninteractive

apt-get update -y
apt-get install -y --no-install-recommends \
    curl \
    wget \
    git \
    jq \
    htop \
    tmux \
    unzip \
    ca-certificates \
    gnupg \
    lsb-release \
    software-properties-common \
    apt-transport-https \
    linux-headers-$(uname -r) \
    build-essential \
    dkms \
    pciutils \
    net-tools \
    iotop \
    sysstat \
    nvme-cli

# Clean apt cache to reduce image size
apt-get clean
rm -rf /var/lib/apt/lists/*

echo "==> Base packages installed."
