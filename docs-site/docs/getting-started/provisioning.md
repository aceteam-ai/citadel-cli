---
sidebar_position: 3
title: Full Provisioning
---

# Full Provisioning

Use full provisioning when you have a fresh server that needs Docker, GPU drivers, and other dependencies installed. This is the recommended path for new bare-metal machines or VMs.

:::note
If Docker is already installed and you only need to join the network, `citadel init` (without `--provision`) is sufficient and does not require sudo. See the [Quick Start](./quick-start.md).
:::

## Interactive provisioning

```bash
sudo citadel init --provision
```

This walks you through an interactive setup:

1. **Network** -- prompts for device authorization (or accepts an authkey)
2. **Service selection** -- choose which AI inference engine to run (vLLM, Ollama, llama.cpp, LM Studio)
3. **Node naming** -- set a display name for your node
4. **System provisioning** -- installs dependencies (only what is missing):
   - Docker Engine (Linux) or Docker Desktop (macOS/Windows)
   - NVIDIA Container Toolkit and GPU runtime configuration (Linux with NVIDIA GPU only)
   - Core system dependencies (curl, gpg, ca-certificates)
5. **Config generation** -- creates the `citadel.yaml` manifest and service compose files
6. **Service startup** -- starts the selected services

## Non-interactive provisioning (automation)

For scripted deployments, CI/CD pipelines, or fleet provisioning, pass all options as flags:

```bash
sudo citadel init --provision \
  --authkey tskey-auth-xxxxx \
  --service vllm \
  --node-name gpu-node-01
```

| Flag | Description |
|---|---|
| `--authkey <key>` | Pre-generated single-use auth key from the AceTeam admin panel. Skips interactive device authorization. |
| `--service <name>` | Inference engine to configure: `vllm`, `ollama`, `llamacpp`, or `lmstudio`. |
| `--node-name <name>` | Display name for this node. |
| `--verbose` | Show detailed output during provisioning. |

## What gets installed

| Component | Linux | macOS | Windows |
|---|---|---|---|
| Docker | Docker Engine (via official install script) | Docker Desktop (via Homebrew) | Docker Desktop (via winget, requires WSL2) |
| NVIDIA Container Toolkit | Installed and configured if NVIDIA GPU detected | Skipped (Metal handled by Docker Desktop) | Skipped (handled by Docker Desktop + WSL2) |
| GPU runtime config | `/etc/docker/daemon.json` updated for NVIDIA runtime | Not needed | Not needed |
| System dependencies | curl, gpg, ca-certificates (via apt) | Installed via Homebrew | Installed via winget |

## The citadel.yaml manifest

After provisioning, Citadel generates a `citadel.yaml` manifest in `~/citadel-node/`. This file is the source of truth for your node's identity and service configuration.

```yaml
node:
  name: gpu-node-01
  id: abc123def456

network:
  nexus: https://nexus.aceteam.ai
  auth_service: https://aceteam.ai

services:
  - name: vllm
    compose_file: services/vllm.yml
    enabled: true
    gpu_required: true
```

Citadel looks for this file in the following order:

1. Current working directory (`./citadel.yaml`)
2. Global system config (`/etc/citadel/citadel.yaml`)
3. User home directory (`~/citadel-node/citadel.yaml`)

## After provisioning

Once provisioning is complete, your node is connected and services are running. You can verify with:

```bash
citadel status
```

To start the job worker (if it is not already running):

```bash
citadel work
```

:::tip
After provisioning, you may need to log out and log back in (or run `exec su -l $USER`) for Docker group membership to take effect on Linux.
:::

## Next steps

- [Quick Start](./quick-start.md) -- the minimal two-command setup for nodes that already have Docker
- [Managing Services](/guides/managing-services) -- add, remove, or reconfigure inference engines
- [Command Reference](/reference/commands) -- full list of `citadel init` flags and options
