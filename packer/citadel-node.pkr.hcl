packer {
  required_plugins {
    qemu = {
      version = "~> 1"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

# -----------------------------------------------------------------------------
# Variables
# -----------------------------------------------------------------------------

variable "ubuntu_version" {
  type        = string
  default     = "24.04"
  description = "Ubuntu LTS version to use as the base image"
}

variable "ubuntu_image_url" {
  type        = string
  default     = "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
  description = "URL of the Ubuntu cloud image (qcow2)"
}

variable "ubuntu_image_checksum" {
  type        = string
  default     = "file:https://cloud-images.ubuntu.com/noble/current/SHA256SUMS"
  description = "Checksum or checksum file URL for the cloud image"
}

variable "disk_size" {
  type        = string
  default     = "50G"
  description = "Disk size for the output image"
}

variable "memory" {
  type        = number
  default     = 4096
  description = "Memory in MB for the build VM"
}

variable "cpus" {
  type        = number
  default     = 4
  description = "Number of CPUs for the build VM"
}

variable "ssh_username" {
  type        = string
  default     = "citadel"
  description = "SSH username configured via cloud-init"
}

variable "ssh_password" {
  type        = string
  default     = "citadel-build"
  description = "SSH password for the build (overwritten on deploy)"
  sensitive   = true
}

# -----------------------------------------------------------------------------
# Source: QEMU builder using Ubuntu cloud image
# -----------------------------------------------------------------------------

source "qemu" "citadel-node" {
  # Cloud image as base disk (not an ISO installer)
  iso_url      = var.ubuntu_image_url
  iso_checksum = var.ubuntu_image_checksum
  disk_image   = true

  # Output
  output_directory = "output"
  vm_name          = "citadel-node.qcow2"
  format           = "qcow2"
  disk_size        = var.disk_size

  # VM resources
  memory       = var.memory
  cpus         = var.cpus
  accelerator  = "kvm"
  machine_type = "q35"

  # Cloud-init seed ISO so the VM boots with SSH access
  cd_label = "cidata"
  cd_content = {
    "meta-data" = ""
    "user-data" = templatefile("${path.root}/cloud-init/user-data.tpl", {
      username = var.ssh_username
      password = var.ssh_password
    })
  }

  # SSH communicator
  communicator     = "ssh"
  ssh_username     = var.ssh_username
  ssh_password     = var.ssh_password
  ssh_timeout      = "10m"
  ssh_wait_timeout = "10m"

  # Display
  headless         = true
  vnc_bind_address = "0.0.0.0"

  # Boot
  boot_wait = "30s"

  # Shutdown
  shutdown_command = "sudo shutdown -P now"
}

# -----------------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------------

build {
  sources = ["source.qemu.citadel-node"]

  # Base packages
  provisioner "shell" {
    script          = "${path.root}/scripts/01-base.sh"
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }

  # NVIDIA driver + CUDA
  provisioner "shell" {
    script          = "${path.root}/scripts/02-nvidia.sh"
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }

  # Docker CE + NVIDIA container toolkit
  provisioner "shell" {
    script          = "${path.root}/scripts/03-docker.sh"
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
    environment_vars = [
      "BUILD_USER=${var.ssh_username}"
    ]
  }

  # Citadel CLI + systemd service
  provisioner "shell" {
    script          = "${path.root}/scripts/04-citadel.sh"
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }

  # Pre-pull vLLM Docker image
  provisioner "shell" {
    script          = "${path.root}/scripts/05-vllm.sh"
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }

  # First-boot script
  provisioner "shell" {
    script          = "${path.root}/scripts/06-firstboot.sh"
    execute_command = "chmod +x {{ .Path }}; sudo {{ .Vars }} {{ .Path }}"
  }

  # Clean up cloud-init and SSH keys for templating
  provisioner "shell" {
    inline = [
      "sudo cloud-init clean --logs --seed",
      "sudo truncate -s 0 /etc/machine-id",
      "sudo rm -f /etc/ssh/ssh_host_*",
      "sudo rm -f /home/${var.ssh_username}/.ssh/authorized_keys",
      "sudo sync"
    ]
  }
}
