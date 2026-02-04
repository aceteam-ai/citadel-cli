---
sidebar_position: 1
title: Architecture Overview
---

# Architecture Overview

Citadel CLI is a self-contained Go binary that bridges user-owned hardware to the AceTeam cloud platform. It embeds its own network stack, service definitions, and platform abstractions -- no external dependencies beyond Docker. This page covers the high-level system topology, package structure, data flows, and state management.

## System Topology

The AceTeam Sovereign Compute Fabric separates orchestration (cloud) from compute (your hardware). Citadel is the agent that runs on the compute side.

```mermaid
graph TB
    Browser["Browser"] --> NextJS["Next.js Frontend"]
    NextJS --> Python["Python Backend"]
    Python --> Redis["Redis Streams"]
    Redis --> Worker1["Citadel Node A"]
    Redis --> Worker2["Citadel Node B"]
    Redis --> Worker3["Citadel Node N"]

    Nexus["Nexus (Headscale)"]
    Worker1 <--> Nexus
    Worker2 <--> Nexus
    Worker3 <--> Nexus

    Worker1 --> Docker1["Docker: vLLM / Ollama"]
    Worker2 --> Docker2["Docker: llama.cpp"]
    Worker3 --> Docker3["Docker: vLLM"]

    subgraph cloud["AceTeam Cloud"]
        Browser
        NextJS
        Python
        Redis
        Nexus
    end

    subgraph infra["Your Infrastructure"]
        Worker1
        Worker2
        Worker3
        Docker1
        Docker2
        Docker3
    end

    style cloud fill:#4A90D9,color:#fff
    style infra fill:#E67E22,color:#fff
```

**Key points:**

- The browser never talks directly to Citadel nodes. All requests flow through Next.js server-side routes to the Python backend.
- Redis Streams is the high-throughput job queue connecting the cloud to on-premise workers.
- Nexus (Headscale) manages the encrypted mesh network but does not carry job traffic -- it handles coordination, key exchange, and NAT traversal.
- Each Citadel node runs Docker containers for the actual inference engines.

## Package Dependency Graph

The binary is organized as a standard Go project with Cobra commands in `cmd/` and domain logic in `internal/` packages. The dependency flow is intentionally top-down -- `cmd` depends on `internal`, never the reverse.

```mermaid
graph TD
    cmd["cmd/ (Cobra commands)"]

    cmd --> worker["internal/worker"]
    cmd --> network["internal/network"]
    cmd --> jobs["internal/jobs"]
    cmd --> platform["internal/platform"]
    cmd --> heartbeat["internal/heartbeat"]
    cmd --> nexus["internal/nexus"]
    cmd --> terminal["internal/terminal"]
    cmd --> tui["internal/tui"]
    cmd --> update["internal/update"]
    cmd --> status["internal/status"]

    worker --> redisclient["internal/redis"]
    worker --> jobs
    heartbeat --> redisclient
    heartbeat --> status

    jobs --> platform
    nexus --> network
    terminal --> network

    status --> platform

    subgraph embedded["Embedded Assets"]
        services["services/ (Docker Compose YAMLs)"]
    end

    cmd --> services

    style cmd fill:#4A90D9,color:#fff
    style embedded fill:#2ECC71,color:#fff
```

**Package responsibilities:**

| Package | Purpose |
|---------|---------|
| `cmd/` | Cobra command definitions -- `init`, `up`, `work`, `status`, `login`, `logout`, `down`, `run`, `logs`, `test`, `agent`, `terminal-server` |
| `internal/worker/` | Unified job runner for Redis Streams and Nexus sources |
| `internal/network/` | Wrapper around tsnet for mesh connectivity |
| `internal/jobs/` | Job handler implementations (LLM inference, shell, config, extraction) |
| `internal/platform/` | OS-specific abstractions (packages, Docker, GPU, users) for Linux/macOS/Windows |
| `internal/heartbeat/` | Status publishing to HTTP API, Redis Pub/Sub, and Redis Streams |
| `internal/status/` | System metrics collection (CPU, memory, GPU, services) |
| `internal/nexus/` | HTTP client for the Nexus coordination API |
| `internal/redis/` | Redis Streams client for job queue operations |
| `internal/terminal/` | WebSocket terminal server with PTY management |
| `internal/tui/` | Bubble Tea terminal UI components |
| `internal/update/` | Self-update with A/B binary rollback |
| `internal/ui/` | Interactive prompts (survey library) |

## Self-Contained Binary

The Citadel binary embeds everything it needs to operate. There are no sidecar processes, no config files to ship, and no runtime downloads (except Docker images for inference engines).

**What is embedded:**

- **Docker Compose files** for all supported services (vLLM, Ollama, llama.cpp, LM Studio) via Go's `embed` package. The `services/compose/` directory is compiled into the binary.
- **Network stack** via tsnet -- the entire WireGuard implementation runs in userspace within the process.
- **Platform abstractions** -- OS detection, package manager selection, Docker management, GPU detection all happen at runtime with no external tooling.

This means a single `citadel` binary (or `citadel.exe` on Windows) is the complete deployment artifact. No installers, no package managers, no configuration management.

## Data Flows

### Job Lifecycle

The primary data flow is an AI inference request traveling from a user's browser to a GPU node and back. Redis Streams provides at-least-once delivery with consumer group tracking.

```mermaid
sequenceDiagram
    participant Browser
    participant NextJS as Next.js
    participant Python as Python Backend
    participant Redis as Redis Streams
    participant Worker as Citadel Worker
    participant Engine as Inference Engine

    Browser->>NextJS: POST /api/inference
    NextJS->>Python: Forward request
    Python->>Redis: XADD jobs:v1:gpu-general
    Redis-->>Worker: XREADGROUP (consumer group)
    Worker->>Engine: HTTP request to vLLM/Ollama
    Engine-->>Worker: Streaming tokens
    Worker->>Redis: PUBLISH stream:v1:{jobId}
    Redis-->>Python: SUBSCRIBE stream:v1:{jobId}
    Python-->>NextJS: SSE chunks
    NextJS-->>Browser: SSE stream
    Worker->>Redis: XACK (message processed)
```

**Design rationale:** Redis Streams gives us consumer groups (horizontal scaling), message persistence (crash recovery), and delivery tracking (at-least-once semantics) -- all critical for GPU workloads that are expensive to retry.

### Heartbeat and Status Reporting

Every node publishes its health on two channels simultaneously: HTTP for the AceTeam API (reliable, 30-second interval) and Redis Pub/Sub for real-time dashboard updates.

```mermaid
sequenceDiagram
    participant Collector as Status Collector
    participant API as AceTeam API
    participant RedisPubSub as Redis Pub/Sub
    participant RedisStreams as Redis Streams
    participant Dashboard as Web Dashboard
    participant PyWorker as Python Worker

    loop Every 30 seconds
        Collector->>Collector: Collect CPU, memory, GPU, services
        Collector->>API: POST /api/fabric/nodes/{id}/status
        Collector->>RedisPubSub: PUBLISH node:status:{nodeId}
        Collector->>RedisStreams: XADD node:status:stream
    end

    RedisPubSub-->>Dashboard: Real-time updates
    RedisStreams-->>PyWorker: Reliable processing
```

### Device Authorization

New nodes authenticate using the OAuth 2.0 Device Authorization Grant (RFC 8628). The CLI displays a code, the user approves in their browser, and the CLI receives credentials to join the mesh.

```mermaid
sequenceDiagram
    participant CLI as Citadel CLI
    participant API as AceTeam API
    participant Browser as User's Browser
    participant Nexus as Nexus (Headscale)

    CLI->>API: POST /api/fabric/device-auth/start
    API-->>CLI: {device_code, user_code, verification_uri}
    CLI->>CLI: Display code to user

    Browser->>API: User enters code at aceteam.ai/device
    API->>API: Approve device, generate authkey

    loop Poll every 5 seconds
        CLI->>API: POST /api/fabric/device-auth/token
        API-->>CLI: authorization_pending / {authkey}
    end

    CLI->>Nexus: Connect with authkey
    Nexus-->>CLI: Assign network IP, join mesh
```

**Why device auth instead of API keys?** Device auth eliminates the need to copy-paste secrets. The user authenticates in their browser where they already have a session, and the CLI receives credentials automatically. This matches the pattern used by GitHub CLI and Docker Desktop.

## State Management

Citadel stores state in three locations, each serving a distinct purpose.

| Location | Purpose | Contents |
|----------|---------|----------|
| `~/.citadel-node/network/` | Network identity and connection state | WireGuard keys, tsnet state, cached node info |
| `~/citadel-node/citadel.yaml` | Node manifest (services, identity) | Node name, selected services, service configs |
| Platform config dir | Global system config (fallback) | `/etc/citadel/` (Linux), `/usr/local/etc/citadel/` (macOS), `C:\ProgramData\Citadel` (Windows) |

**Manifest discovery order** (in `cmd/up.go:findAndReadManifest()`):

1. Current working directory -- `./citadel.yaml`
2. Global system config -- `/etc/citadel/citadel.yaml`
3. User home -- `~/citadel-node/citadel.yaml`

The manifest is the source of truth for node configuration. It is generated by `citadel init` and can be version-controlled or templated for fleet deployments.

**Network state** is managed entirely by tsnet and persists across process restarts. Deleting `~/.citadel-node/network/` forces re-authentication on next connect.
