#!/bin/bash
# 02-nvidia.sh - Install NVIDIA driver + CUDA toolkit via runfile
#
# Installs the driver with DKMS so the kernel module rebuilds automatically
# on kernel updates. The install succeeds even without a GPU present at build
# time -- only nvidia-smi verification is skipped.
set -euo pipefail

echo "==> Installing NVIDIA driver + CUDA toolkit..."

export DEBIAN_FRONTEND=noninteractive

# CUDA version config
CUDA_MAJOR="12"
CUDA_MINOR="8"
CUDA_PATCH="1"
CUDA_VERSION="${CUDA_MAJOR}.${CUDA_MINOR}.${CUDA_PATCH}"
DRIVER_VERSION="570.124.06"
RUNFILE="cuda_${CUDA_VERSION}_${DRIVER_VERSION}_linux.run"
RUNFILE_URL="https://developer.download.nvidia.com/compute/cuda/${CUDA_VERSION}/local_installers/${RUNFILE}"

# Ensure kernel headers and DKMS are present (should be from 01-base.sh)
apt-get update -y
apt-get install -y --no-install-recommends \
    linux-headers-$(uname -r) \
    dkms \
    build-essential \
    pkg-config \
    libglvnd-dev

echo "==> Downloading CUDA ${CUDA_VERSION} runfile..."
cd /tmp
if [ ! -f "${RUNFILE}" ]; then
    wget -q --show-progress "${RUNFILE_URL}" -O "${RUNFILE}"
fi

echo "==> Running CUDA installer..."
# --silent           : non-interactive
# --driver           : install driver
# --toolkit          : install CUDA toolkit
# --no-opengl-libs   : skip OpenGL (server, no display)
# --dkms             : register with DKMS for kernel updates
# The installer returns 0 on success even without GPU hardware.
chmod +x "${RUNFILE}"
./"${RUNFILE}" \
    --silent \
    --driver \
    --toolkit \
    --no-opengl-libs \
    --dkms \
    || {
        # Exit code 1 from the runfile can mean "no supported GPU found" which
        # is expected during image build. Check if the toolkit was installed.
        if [ -d "/usr/local/cuda-${CUDA_MAJOR}.${CUDA_MINOR}" ]; then
            echo "WARN: Installer returned non-zero but CUDA toolkit is present."
            echo "      GPU driver module will load on first boot with actual hardware."
        else
            echo "ERROR: CUDA toolkit installation failed."
            exit 1
        fi
    }

# Set up environment for CUDA
cat > /etc/profile.d/cuda.sh << 'EOF'
export PATH=/usr/local/cuda/bin${PATH:+:${PATH}}
export LD_LIBRARY_PATH=/usr/local/cuda/lib64${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}
EOF

# Make the dynamic linker aware of CUDA libs
echo "/usr/local/cuda/lib64" > /etc/ld.so.conf.d/cuda.conf
ldconfig

# Verify toolkit installation (not the driver -- no GPU at build time)
if command -v nvcc &>/dev/null || [ -x /usr/local/cuda/bin/nvcc ]; then
    echo "==> CUDA toolkit installed: $(/usr/local/cuda/bin/nvcc --version | grep release)"
else
    echo "WARN: nvcc not found. CUDA toolkit may not have installed correctly."
fi

# GPU driver verification is deferred to first boot
if nvidia-smi &>/dev/null; then
    echo "==> nvidia-smi works (GPU present at build time):"
    nvidia-smi --query-gpu=name,driver_version --format=csv,noheader
else
    echo "==> No GPU detected at build time (expected). Driver will activate on first boot."
fi

# Clean up the runfile
rm -f /tmp/"${RUNFILE}"

echo "==> NVIDIA driver + CUDA installation complete."
