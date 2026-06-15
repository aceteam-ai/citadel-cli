#!/bin/bash
# deploy-to-proxmox.sh - Import a Citadel node qcow2 image into Proxmox
#
# Takes the Packer-built qcow2, imports it as a VM template in Proxmox,
# clones a new VM from the template, injects the authkey, and starts it.
#
# Prerequisites:
#   - SSH access to the Proxmox host (key-based recommended)
#   - qm and pvesh available on the Proxmox host
#   - The qcow2 image built by Packer (output/citadel-node.qcow2)
#
# Usage:
#   ./deploy-to-proxmox.sh \
#     --host <proxmox-host> \
#     --storage <storage-name> \
#     --template-id <vm-id> \
#     --authkey <citadel-authkey> \
#     [--vm-id <clone-id>] \
#     [--vm-name <name>] \
#     [--cores <n>] \
#     [--memory <mb>] \
#     [--image <path>]

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------

PROXMOX_HOST=""
STORAGE="local-lvm"
TEMPLATE_ID="9000"
VM_ID=""
VM_NAME=""
AUTHKEY=""
CORES="4"
MEMORY="8192"
IMAGE="output/citadel-node.qcow2"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
DRY_RUN=false

# ---------------------------------------------------------------------------
# Colors
# ---------------------------------------------------------------------------

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

msg()  { echo -e "${GREEN}==>${NC} ${1}"; }
warn() { echo -e "${YELLOW}WARNING:${NC} ${1}"; }
err()  { echo -e "${RED}ERROR:${NC} ${1}" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------

print_help() {
    cat << 'HELP'
Usage: deploy-to-proxmox.sh [OPTIONS]

Import a Citadel node qcow2 into Proxmox and deploy a VM.

Required:
  --host <host>          Proxmox host (SSH destination, e.g., root@pve.local)
  --authkey <key>        Citadel authkey for network registration

Optional:
  --storage <name>       Proxmox storage target (default: local-lvm)
  --template-id <id>     VM ID for the template (default: 9000)
  --vm-id <id>           VM ID for the clone (default: auto-assign)
  --vm-name <name>       Name for the cloned VM (default: citadel-node-<id>)
  --cores <n>            CPU cores (default: 4)
  --memory <mb>          Memory in MB (default: 8192)
  --image <path>         Path to qcow2 image (default: output/citadel-node.qcow2)
  --dry-run              Show commands without executing
  -h, --help             Show this help
HELP
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --host)       PROXMOX_HOST="$2"; shift 2 ;;
        --storage)    STORAGE="$2"; shift 2 ;;
        --template-id) TEMPLATE_ID="$2"; shift 2 ;;
        --vm-id)      VM_ID="$2"; shift 2 ;;
        --vm-name)    VM_NAME="$2"; shift 2 ;;
        --authkey)    AUTHKEY="$2"; shift 2 ;;
        --cores)      CORES="$2"; shift 2 ;;
        --memory)     MEMORY="$2"; shift 2 ;;
        --image)      IMAGE="$2"; shift 2 ;;
        --dry-run)    DRY_RUN=true; shift ;;
        -h|--help)    print_help; exit 0 ;;
        *)            err "Unknown option: $1" ;;
    esac
done

# Validate required args
[ -z "${PROXMOX_HOST}" ] && err "Missing --host (Proxmox SSH destination)"
[ -z "${AUTHKEY}" ] && err "Missing --authkey (Citadel authkey)"
[ ! -f "${IMAGE}" ] && err "Image not found: ${IMAGE}"

# ---------------------------------------------------------------------------
# Helper: run on Proxmox host
# ---------------------------------------------------------------------------

pve_ssh() {
    if [ "${DRY_RUN}" = true ]; then
        echo -e "${BLUE}[DRY-RUN] ssh ${PROXMOX_HOST}: $*${NC}"
    else
        ssh ${SSH_OPTS} "${PROXMOX_HOST}" "$@"
    fi
}

pve_scp() {
    if [ "${DRY_RUN}" = true ]; then
        echo -e "${BLUE}[DRY-RUN] scp $1 -> ${PROXMOX_HOST}:$2${NC}"
    else
        scp ${SSH_OPTS} "$1" "${PROXMOX_HOST}:$2"
    fi
}

# ---------------------------------------------------------------------------
# Step 1: Upload the qcow2 image
# ---------------------------------------------------------------------------

msg "Step 1/5: Uploading image to Proxmox host..."
REMOTE_IMAGE="/tmp/citadel-node.qcow2"
pve_scp "${IMAGE}" "${REMOTE_IMAGE}"

# ---------------------------------------------------------------------------
# Step 2: Create VM template
# ---------------------------------------------------------------------------

msg "Step 2/5: Creating VM template (ID: ${TEMPLATE_ID})..."

# Destroy existing template if it exists (idempotent)
pve_ssh "qm status ${TEMPLATE_ID} 2>/dev/null && qm destroy ${TEMPLATE_ID} --purge || true"

# Create the VM with basic hardware config
pve_ssh "qm create ${TEMPLATE_ID} \
    --name citadel-node-template \
    --memory ${MEMORY} \
    --cores ${CORES} \
    --net0 virtio,bridge=vmbr0 \
    --ostype l26 \
    --scsihw virtio-scsi-single \
    --machine q35 \
    --agent enabled=1 \
    --cpu host \
    --bios ovmf"

# Import the disk
pve_ssh "qm importdisk ${TEMPLATE_ID} ${REMOTE_IMAGE} ${STORAGE}"

# Attach the imported disk as the boot drive
# The import creates the disk as unused0
pve_ssh "qm set ${TEMPLATE_ID} \
    --scsi0 ${STORAGE}:vm-${TEMPLATE_ID}-disk-0 \
    --boot order=scsi0"

# Add cloud-init drive for authkey injection
pve_ssh "qm set ${TEMPLATE_ID} --ide2 ${STORAGE}:cloudinit"

# Convert to template
pve_ssh "qm template ${TEMPLATE_ID}"

# Clean up remote image
pve_ssh "rm -f ${REMOTE_IMAGE}"

msg "Template ${TEMPLATE_ID} created."

# ---------------------------------------------------------------------------
# Step 3: Clone the template to a new VM
# ---------------------------------------------------------------------------

# Auto-assign VM ID if not specified
if [ -z "${VM_ID}" ]; then
    VM_ID=$(pve_ssh "pvesh get /cluster/nextid")
    VM_ID=$(echo "${VM_ID}" | tr -d '[:space:]"')
    msg "Auto-assigned VM ID: ${VM_ID}"
fi

if [ -z "${VM_NAME}" ]; then
    VM_NAME="citadel-node-${VM_ID}"
fi

msg "Step 3/5: Cloning template to VM ${VM_ID} (${VM_NAME})..."

pve_ssh "qm clone ${TEMPLATE_ID} ${VM_ID} \
    --name ${VM_NAME} \
    --full true"

# ---------------------------------------------------------------------------
# Step 4: Inject authkey
# ---------------------------------------------------------------------------

msg "Step 4/5: Injecting authkey..."

# Use cloud-init custom config to write the authkey file. Proxmox supports
# cicustom for user-data. We create a snippet that writes the authkey.
SNIPPET_CONTENT=$(cat << CIEOF
#cloud-config
write_files:
  - path: /etc/citadel/authkey
    content: "${AUTHKEY}"
    owner: root:root
    permissions: '0600'

# Ensure the citadel user has required group access
runcmd:
  - usermod -aG docker,systemd-journal,adm citadel || true
CIEOF
)

# Upload the snippet to the Proxmox snippets storage
SNIPPET_FILE="citadel-init-${VM_ID}.yml"
if [ "${DRY_RUN}" = true ]; then
    echo -e "${BLUE}[DRY-RUN] Would upload cloud-init snippet${NC}"
else
    echo "${SNIPPET_CONTENT}" | ssh ${SSH_OPTS} "${PROXMOX_HOST}" \
        "mkdir -p /var/lib/vz/snippets && cat > /var/lib/vz/snippets/${SNIPPET_FILE}"
fi

# Point the VM's cloud-init at our custom user-data
pve_ssh "qm set ${VM_ID} --cicustom 'user=local:snippets/${SNIPPET_FILE}'"

# Regenerate the cloud-init ISO
pve_ssh "qm cloudinit update ${VM_ID}" 2>/dev/null || true

# ---------------------------------------------------------------------------
# Step 5: Start the VM
# ---------------------------------------------------------------------------

msg "Step 5/5: Starting VM ${VM_ID}..."
pve_ssh "qm start ${VM_ID}"

echo ""
msg "Deployment complete!"
echo ""
echo "  Template ID:  ${TEMPLATE_ID}"
echo "  VM ID:        ${VM_ID}"
echo "  VM Name:      ${VM_NAME}"
echo "  Cores:        ${CORES}"
echo "  Memory:       ${MEMORY} MB"
echo ""
echo "The VM is booting. On first boot it will:"
echo "  1. Read the authkey from /etc/citadel/authkey"
echo "  2. Run 'citadel init' to join the network"
echo "  3. Start the citadel-worker service"
echo ""
echo "Monitor progress:"
echo "  ssh ${PROXMOX_HOST} qm guest exec ${VM_ID} -- journalctl -u citadel-firstboot -f"
echo "  ssh ${PROXMOX_HOST} qm guest exec ${VM_ID} -- journalctl -u citadel-worker -f"
