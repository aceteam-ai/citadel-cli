# Citadel Node Packer Template

Packer configuration for building a Citadel GPU node VM image that can be imported into Proxmox as a template and cloned for deployment.

## What's included

The built image is an Ubuntu 24.04 LTS qcow2 with:

- NVIDIA driver (570.x) + CUDA 12.8 toolkit (via runfile, DKMS-registered)
- Docker CE + NVIDIA Container Toolkit (nvidia as default runtime)
- Citadel CLI (latest release from GitHub)
- vLLM Docker image pre-pulled
- A first-boot service that initializes the node when an authkey is provided
- systemd worker service gated on successful initialization

## Prerequisites

- [Packer](https://developer.hashicorp.com/packer/install) >= 1.9
- QEMU/KVM (`apt install qemu-system-x86 qemu-utils`)
- KVM access (`/dev/kvm` must be accessible)
- ~20 GB free disk for the build (cloud image + CUDA + vLLM image)

On a machine without KVM (e.g., inside a VM without nested virtualization), you can set `accelerator = "tcg"` but the build will be significantly slower.

## Build

```bash
cd packer/

# Initialize Packer plugins (first time only)
packer init citadel-node.pkr.hcl

# Build the image
packer build citadel-node.pkr.hcl

# Build with custom variables
packer build \
  -var 'disk_size=100G' \
  -var 'memory=8192' \
  -var 'cpus=8' \
  citadel-node.pkr.hcl
```

The output image lands at `output/citadel-node.qcow2`.

Build time depends on network speed (CUDA runfile is ~4 GB, vLLM image is ~8 GB). Expect 20-40 minutes on a reasonable connection.

## Deploy to Proxmox

The `deploy-to-proxmox.sh` script handles the full workflow: upload, template creation, clone, authkey injection, and VM start.

```bash
# Basic deployment
./deploy-to-proxmox.sh \
  --host root@pve.local \
  --authkey tskey-auth-xxxxx

# Full options
./deploy-to-proxmox.sh \
  --host root@pve.local \
  --storage local-lvm \
  --template-id 9000 \
  --vm-id 200 \
  --vm-name gpu-worker-01 \
  --cores 8 \
  --memory 32768 \
  --authkey tskey-auth-xxxxx

# Dry run to preview commands
./deploy-to-proxmox.sh --dry-run \
  --host root@pve.local \
  --authkey tskey-auth-xxxxx
```

### What the deploy script does

1. Uploads the qcow2 to the Proxmox host via SCP
2. Creates a VM and imports the disk, then converts it to a template
3. Clones the template to a new full VM
4. Injects the authkey via a cloud-init snippet
5. Starts the VM

### First boot sequence

On first boot, the `citadel-firstboot.service` runs once:

1. Reads the authkey from `/etc/citadel/authkey` (written by cloud-init)
2. Runs `citadel init --authkey <key>` to join the network (no `--provision` since Docker/NVIDIA are already installed)
3. Copies the generated manifest to `/etc/citadel/citadel.yaml`
4. Enables and starts `citadel-worker.service`
5. Removes the authkey file and disables itself

If no authkey is present, the service logs a warning and exits cleanly. You can manually initialize later:

```bash
ssh citadel@<vm-ip>
citadel init --authkey <your-key>
sudo systemctl enable --now citadel-worker.service
```

## Manual Proxmox import

If you prefer not to use the deploy script:

```bash
# 1. Upload the image
scp output/citadel-node.qcow2 root@pve:/tmp/

# 2. Create a VM
qm create 9000 --name citadel-template --memory 8192 --cores 4 \
  --net0 virtio,bridge=vmbr0 --scsihw virtio-scsi-single

# 3. Import the disk
qm importdisk 9000 /tmp/citadel-node.qcow2 local-lvm

# 4. Attach disk and set boot order
qm set 9000 --scsi0 local-lvm:vm-9000-disk-0 --boot order=scsi0

# 5. Convert to template
qm template 9000

# 6. Clone from template
qm clone 9000 200 --name gpu-worker-01 --full true

# 7. Write authkey and start
qm guest exec 200 -- bash -c 'echo "tskey-auth-xxxxx" > /etc/citadel/authkey'
qm start 200
```

## GPU passthrough

For GPU workloads, you need to pass through a physical GPU to the VM. This is done at the Proxmox level after cloning:

```bash
# Add PCI passthrough for GPU (adjust the address for your hardware)
qm set 200 --hostpci0 0000:01:00,pcie=1

# Ensure IOMMU is enabled in BIOS and Proxmox boot params
# Add to /etc/default/grub: GRUB_CMDLINE_LINUX_DEFAULT="intel_iommu=on"
# Or for AMD: GRUB_CMDLINE_LINUX_DEFAULT="amd_iommu=on"
```

## Provisioning scripts

| Script | Purpose |
|--------|---------|
| `scripts/01-base.sh` | apt update, essential tools (curl, git, jq, htop, tmux, build-essential, dkms) |
| `scripts/02-nvidia.sh` | NVIDIA driver + CUDA 12.8 via runfile installer with DKMS |
| `scripts/03-docker.sh` | Docker CE + NVIDIA Container Toolkit, configures nvidia as default runtime |
| `scripts/04-citadel.sh` | Downloads latest citadel-cli, creates systemd worker service (disabled) |
| `scripts/05-vllm.sh` | Pre-pulls `vllm/vllm-openai:latest` Docker image |
| `scripts/06-firstboot.sh` | Installs one-shot first-boot service for authkey-based initialization |

## Customization

### Using a different CUDA version

Edit `scripts/02-nvidia.sh` and update the `CUDA_VERSION` and `DRIVER_VERSION` variables. Find available versions at https://developer.nvidia.com/cuda-toolkit-archive.

### Skipping vLLM pre-pull

If you don't need vLLM or want a smaller image, comment out the 05-vllm.sh provisioner in `citadel-node.pkr.hcl`.

### Different base image

Change the `ubuntu_image_url` variable to use a different cloud image. The scripts assume Ubuntu/Debian package management.
