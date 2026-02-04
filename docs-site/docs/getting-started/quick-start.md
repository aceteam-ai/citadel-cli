---
sidebar_position: 2
title: Quick Start
---

# Quick Start

Get your node online in 1 command.

## Run Citadel

```bash
citadel
```

The interactive control center launches and walks you through everything:

1. **Login** -- If this is your first time, Citadel displays a one-time code and prompts you to open [aceteam.ai/device](https://aceteam.ai/device) in your browser. Enter the code to authorize your node.
2. **Connect** -- Once authorized, your node joins the AceTeam Network automatically. No sudo required -- the CLI uses userspace networking with no system-level VPN or driver installation.
3. **Work** -- The worker starts automatically, accepting AI workloads from the job queue.

The control center shows a live dashboard with system vitals, GPU status, network connectivity, running services, and worker activity -- all in one place.

```
 ╭─────────────────────────────────────────╮
 │  CITADEL CONTROL CENTER                 │
 │                                         │
 │  Node:     gpu-server-01                │
 │  Network:  Connected (100.64.0.12)      │
 │  Worker:   Running                      │
 │                                         │
 │  CPU: 8%    MEM: 4.2/32 GiB    GPU: 3%  │
 │                                         │
 │  Services:                              │
 │    vllm     running   port 8000         │
 │                                         │
 │  Activity:                              │
 │    Connected to AceTeam Network         │
 │    Worker started, listening for jobs... │
 ╰─────────────────────────────────────────╯
```

No subcommands to memorize. Everything is managed interactively from the control center.

## What just happened?

By running `citadel`, your node:

1. **Joined the AceTeam Network** -- an encrypted secure mesh network connecting your hardware to the AceTeam control plane. All traffic is end-to-end encrypted.
2. **Announced its capabilities** -- CPU, memory, GPU, and available services are reported to the platform so workloads can be routed to the right hardware.
3. **Started accepting AI workloads** -- the worker listens for inference requests, model downloads, and other jobs dispatched by the AceTeam platform.

Your node is now part of the Sovereign Compute Fabric, running AI workloads on your own hardware while being managed through the AceTeam cloud.

## Next steps

- [Full Provisioning](./provisioning.md) -- install Docker and GPU drivers on a fresh server
- [Managing Services](/guides/managing-services) -- configure which AI inference engines to run
- [Monitoring](/guides/monitoring) -- set up alerts and health monitoring
- [Networking](/architecture/mesh-network) -- understand the secure mesh network
