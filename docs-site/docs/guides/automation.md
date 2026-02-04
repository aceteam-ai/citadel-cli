---
sidebar_position: 4
title: Automation
---

# Automation

Citadel supports fully non-interactive setup for automated deployments, fleet provisioning, and CI/CD pipelines.

## Non-Interactive Setup

Use an authkey to skip the interactive device authorization flow:

```bash
citadel init --authkey <key> --service vllm --node-name gpu-01
```

Generate authkeys from the AceTeam web dashboard or the Nexus admin panel. Each key is single-use.

## Full Provisioning

For fresh servers that need Docker, GPU drivers, and system dependencies installed:

```bash
sudo citadel init --provision --authkey <key> --service vllm --node-name gpu-01
```

The `--provision` flag installs everything needed to run AI workloads:

- Core dependencies (curl, gpg, ca-certificates)
- Docker Engine (via the official install script)
- NVIDIA Container Toolkit (skipped automatically on non-GPU systems)
- Docker daemon configuration for GPU runtime

This requires sudo because it modifies system packages and configuration.

## Running as a System Service

Install Citadel as a system service so it starts automatically on boot:

```bash
sudo citadel service install
sudo citadel service start
```

> **Note:** System service installation requires root/administrator privileges.

Manage the service lifecycle:

```bash
citadel service status    # Check if the service is running
citadel service stop      # Stop the service
citadel service uninstall # Remove the service
```

## Auto-Update

Citadel checks for updates daily by default. When a new version is available, it is downloaded and applied automatically.

Manage auto-update behavior:

```bash
citadel update check      # Check for updates now
citadel update install    # Install an available update
citadel update status     # Show current update state
citadel update rollback   # Roll back to the previous version
citadel update disable    # Disable automatic updates
citadel update enable     # Re-enable automatic updates
```

## Environment Variables

These environment variables can be used to configure Citadel in automated environments:

| Variable | Default | Description |
|----------|---------|-------------|
| `REDIS_URL` | `redis://redis.aceteam.ai:6379` | Redis connection URL for the job queue |
| `WORKER_QUEUE` | `jobs:v1:gpu-general` | Redis Stream queue name to consume from |
| `CITADEL_AUTH_HOST` | `https://aceteam.ai` | Auth service URL for device authorization |
| `CITADEL_DEVICE_CODE` | (none) | Device code from the authorization flow |

## Scripted Health Checks

When running the worker with a status port, you can use standard HTTP tools for health monitoring:

```bash
# Simple liveness check
curl -s http://<node-ip>:8080/ping

# Readiness check with version
curl -s http://<node-ip>:8080/health

# Full system metrics
curl -s http://<node-ip>:8080/status | jq .
```

These endpoints integrate with any monitoring system that supports HTTP checks (Prometheus, Datadog, Nagios, custom scripts, etc.).
