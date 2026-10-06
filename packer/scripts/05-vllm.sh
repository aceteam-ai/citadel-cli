#!/bin/bash
# 05-vllm.sh - Cache inference and trusted app images as the service user
#
# Pulling at build time saves 5-10 minutes on first VM boot. The image is
# large (~8 GB) so this is one of the slower provisioning steps.
set -euo pipefail

citadel_uid=$(id -u citadel)
as_citadel() {
    runuser -u citadel -- env HOME=/home/citadel XDG_RUNTIME_DIR="/run/user/${citadel_uid}" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${citadel_uid}/bus" "$@"
}
as_citadel podman info >/dev/null
as_citadel podman pull vllm/vllm-openai:latest
# The trusted catalog is registry-gated; report a missing catalog at image
# build time so operators can retry after its images are published.
if ! as_citadel /usr/local/bin/citadel service catalog update; then
    echo "WARNING: trusted catalog update was unavailable during image build" >&2
elif ! as_citadel /usr/local/bin/citadel service catalog pre-pull-runtimes; then
    echo "WARNING: trusted hosted-app runtime images are not yet available" >&2
fi
