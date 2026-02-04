---
sidebar_position: 2
title: Quick Start
---

# Quick Start

Get your node online in 2 commands.

## Step 1: Join the AceTeam Network

```bash
citadel init
```

This starts the device authorization flow:

1. The CLI displays a one-time code and a URL.
2. Open [aceteam.ai/device](https://aceteam.ai/device) in your browser.
3. Enter the code shown in your terminal.
4. Once approved, your node joins the AceTeam Network automatically.

```
Your device code is: ABCD-1234

Open https://aceteam.ai/device in your browser and enter the code above.
Waiting for authorization...

Connected! Node registered as "gpu-server-01"
```

No sudo required. The CLI uses userspace networking -- no system-level VPN or driver installation needed.

## Step 2: Start the worker

```bash
citadel work
```

This starts your node's services (defined in `citadel.yaml`) and begins accepting AI workloads from the job queue. The worker runs continuously, polling for new jobs and reporting status back to the control plane.

```
Starting services...
  vllm: running
Worker connected to job queue
Listening for jobs...
```

## Step 3: Verify

```bash
citadel status
```

This displays a health dashboard showing system vitals, GPU status, network connectivity, and running services:

```
Node:       gpu-server-01
Status:     Online
Network:    Connected (100.64.0.12)
Uptime:     2m 34s

CPU:        12 cores, 8% usage
Memory:     32 GiB total, 4.2 GiB used
GPU:        NVIDIA RTX 4090 (24 GiB VRAM)

Services:
  vllm      running   port 8000
```

## What just happened?

In those two commands, your node:

1. **Joined the AceTeam Network** -- an encrypted secure mesh network connecting your hardware to the AceTeam control plane. All traffic is end-to-end encrypted.
2. **Announced its capabilities** -- CPU, memory, GPU, and available services are reported to the platform so workloads can be routed to the right hardware.
3. **Started accepting AI workloads** -- the worker listens for inference requests, model downloads, and other jobs dispatched by the AceTeam platform.

Your node is now part of the Sovereign Compute Fabric, running AI workloads on your own hardware while being managed through the AceTeam cloud.

## Next steps

- [Full Provisioning](./provisioning.md) -- install Docker and GPU drivers on a fresh server
- [Managing Services](/guides/managing-services) -- configure which AI inference engines to run
- [Monitoring](/guides/monitoring) -- set up alerts and health monitoring
- [Networking](/architecture/mesh-network) -- understand the secure mesh network
