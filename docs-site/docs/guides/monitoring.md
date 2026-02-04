---
sidebar_position: 2
title: Monitoring
---

# Monitoring

Citadel provides multiple ways to monitor your node: an interactive CLI dashboard, HTTP health endpoints, automatic heartbeat reporting, and a network-wide node listing.

## Status Dashboard

Run the status command for a comprehensive snapshot of your node:

```bash
citadel status
```

This displays:

- **Version** -- the Citadel CLI version running on the node
- **CPU** -- core count and current usage
- **Memory** -- total and used RAM
- **Disk** -- available storage
- **GPU** -- detected NVIDIA GPUs with VRAM (or "No GPU detected")
- **Network** -- AceTeam Network connection status and assigned IP
- **Services** -- running Docker containers and their ports

```
Node:       gpu-server-01
Status:     Online
Network:    Connected (100.64.0.12)
Uptime:     4h 12m

CPU:        12 cores, 14% usage
Memory:     64 GiB total, 12.3 GiB used
GPU:        NVIDIA A100 (80 GiB VRAM)

Services:
  vllm      running   port 8000
  ollama    running   port 11434
```

## HTTP Health Endpoints

When running the worker with a status port, Citadel exposes HTTP endpoints for programmatic health checks:

```bash
citadel work --status-port=8080
```

| Endpoint | Response | Purpose |
|----------|----------|---------|
| `GET /ping` | `{"status":"pong"}` | Simple liveness check |
| `GET /health` | `{"status":"ok","version":"..."}` | Readiness check with version |
| `GET /status` | Full system metrics JSON | Detailed monitoring data |

Example health check:

```bash
curl http://100.64.0.12:8080/health
```

```json
{"status": "ok", "version": "v2.3.0"}
```

:::note
Use HTTP health checks rather than ICMP ping. Citadel uses userspace networking, which does not support ICMP. Standard `ping` commands will not reach the node through the mesh network.
:::

## Heartbeat Reporting

When the worker is running, Citadel automatically publishes a status heartbeat every 30 seconds. This heartbeat is sent to:

- **AceTeam API** -- so the cloud control plane knows the node is online and can display its status in the web dashboard
- **Redis Pub/Sub and Streams** -- for real-time UI updates and reliable processing by backend services

Heartbeat data includes system metrics (CPU, memory, GPU utilization), running services, and node metadata. This is how the AceTeam platform maintains an up-to-date view of all nodes in the fabric.

## Viewing All Nodes

List all nodes connected to your network:

```bash
citadel nodes
```

Or use the service discovery command:

```bash
citadel peers
```

Both commands show the nodes on your AceTeam Network along with their connection status, IP addresses, and capabilities.
