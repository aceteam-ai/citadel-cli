#!/bin/bash
# 03-docker.sh - Install Docker CE + NVIDIA Container Toolkit
set -euo pipefail

echo "==> Installing Docker CE..."

export DEBIAN_FRONTEND=noninteractive

# Use the build user from Packer env var, fall back to "citadel"
DOCKER_USER="${BUILD_USER:-citadel}"

# ---------------------------------------------------------------------------
# Docker CE
# ---------------------------------------------------------------------------

# Add Docker's official GPG key
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | \
    gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg

# Set up the Docker apt repository
CODENAME=$(. /etc/os-release && echo "$VERSION_CODENAME")
cat > /etc/apt/sources.list.d/docker.list << EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu ${CODENAME} stable
EOF

apt-get update -y
apt-get install -y --no-install-recommends \
    docker-ce \
    docker-ce-cli \
    containerd.io \
    docker-buildx-plugin \
    docker-compose-plugin

# Add the build user to the docker group
usermod -aG docker "${DOCKER_USER}"

# Enable and start Docker
systemctl enable docker
systemctl start docker

echo "==> Docker CE installed."
docker --version

# ---------------------------------------------------------------------------
# NVIDIA Container Toolkit
# ---------------------------------------------------------------------------

echo "==> Installing NVIDIA Container Toolkit..."

# Add NVIDIA container toolkit repo
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
    gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg

DIST=$(. /etc/os-release; echo "$ID$VERSION_ID")
curl -fsSL "https://nvidia.github.io/libnvidia-container/${DIST}/libnvidia-container.list" | \
    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
    tee /etc/apt/sources.list.d/nvidia-container-toolkit.list > /dev/null

apt-get update -y
apt-get install -y --no-install-recommends nvidia-container-toolkit

# Configure Docker daemon for NVIDIA runtime
nvidia-ctk runtime configure --runtime=docker

# Set NVIDIA as the default runtime so GPU containers just work
# nvidia-ctk sets up the runtime but we also want it as default
cat > /etc/docker/daemon.json << 'EOF'
{
  "default-runtime": "nvidia",
  "runtimes": {
    "nvidia": {
      "path": "nvidia-container-runtime",
      "runtimeArgs": []
    }
  },
  "log-driver": "json-file",
  "log-opts": {
    "max-size": "50m",
    "max-file": "3"
  }
}
EOF

# Restart Docker to pick up new runtime config
systemctl restart docker

echo "==> Docker + NVIDIA Container Toolkit installed."

# Clean up
apt-get clean
rm -rf /var/lib/apt/lists/*
