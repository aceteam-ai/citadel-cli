#cloud-config
# Cloud-init user-data for Packer build. Creates a user with password SSH
# access so the Packer SSH communicator can connect and run provisioners.

users:
  - name: ${username}
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: false
    plain_text_passwd: "${password}"

ssh_pwauth: true

# Disable automatic package upgrades during build (Packer scripts handle this)
package_update: false
package_upgrade: false

# Signal cloud-init completion
runcmd:
  - [ systemctl, restart, sshd ]
