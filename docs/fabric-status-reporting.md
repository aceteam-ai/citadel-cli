# Fabric Status Reporting: Distributed Node Telemetry System

## Abstract

This document describes the design and implementation of a distributed telemetry system that enables real-time visibility into Citadel nodes on the AceTeam Fabric infrastructure page. The system employs a hybrid push/pull architecture combining periodic heartbeats with on-demand queries, a pattern commonly used in distributed systems to balance freshness with resource efficiency.

## 1. Introduction

### 1.1 Problem Statement

In distributed computing environments, a central control plane needs visibility into the state of distributed worker nodes. Without this visibility, operators cannot:

- Identify available compute resources (GPUs, services)
- Monitor system health and utilization
- Route workloads to appropriate nodes
- Diagnose failures or degraded performance

Currently, the AceTeam `/fabric` page shows nodes from Headscale (VPN mesh) but lacks insight into what services each Citadel node is running, which models are loaded, or resource utilization.

### 1.2 Design Goals

1. **Near-real-time visibility**: Status updates within 30 seconds of state changes
2. **Low overhead**: Minimize network and compute cost on worker nodes
3. **Fault tolerance**: Graceful handling of network partitions and node failures
4. **Scalability**: Support hundreds of nodes without overwhelming the control plane
5. **Security**: All communication over authenticated Tailscale mesh

## 2. Background: Distributed Telemetry Patterns

### 2.1 Push vs Pull Models

Distributed systems use two fundamental approaches for telemetry:

**Pull Model (Polling)**
- Control plane periodically queries each node
- Simple to implement; nodes are stateless regarding reporting
- Scales poorly: O(n) queries for n nodes, per interval
- Node failures detected only at next poll

**Push Model (Heartbeats)**
- Nodes periodically send status to control plane
- Scales well: control plane is passive receiver
- Failure detection: missed heartbeats indicate problems
- Requires state management on control plane

### 2.2 Hybrid Approach

Production systems often combine both patterns:

```
┌─────────────────┐     Heartbeat (push, 30s)     ┌─────────────────┐
│  Worker Node    │ ───────────────────────────▶  │  Control Plane  │
│                 │                               │                 │
│                 │  ◀─────────────────────────── │                 │
│                 │     On-demand query (pull)    │                 │
└─────────────────┘                               └─────────────────┘
```

- **Heartbeats**: Lightweight, frequent updates for basic health/presence
- **On-demand queries**: Detailed status when user views a specific node

This is the pattern used by Kubernetes (kubelet → API server), Consul (agents → servers), and other production systems.

### 2.3 Failure Detection

Heartbeat-based systems use timeout-based failure detection:

```
T_failure = T_heartbeat × N_missed + T_clock_skew
```

With a 30-second heartbeat interval and 3 missed heartbeats tolerance:
- Failure detection time: ~90 seconds + clock skew margin

This is an acceptable tradeoff between detection speed and false positive rate.

## 3. Architecture

### 3.1 System Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Citadel Node                                │
│                                                                     │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                │
│  │   vLLM      │  │   Ollama    │  │  Postgres   │  ... services  │
│  │  :8000      │  │  :11434     │  │  :5432      │                │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘                │
│         │                │                │                        │
│         └────────────────┼────────────────┘                        │
│                          ▼                                         │
│                 ┌─────────────────┐                                │
│                 │ Status Collector│  ← Gathers metrics from all    │
│                 │                 │    services, GPU, system       │
│                 └────────┬────────┘                                │
│                          │                                         │
│         ┌────────────────┼────────────────┐                        │
│         ▼                ▼                ▼                        │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                │
│  │ HTTP Server │  │   Redis     │  │   Worker    │                │
│  │   :8080     │  │  Publisher  │  │   (Redis)   │                │
│  └──────┬──────┘  └──────┬──────┘  └─────────────┘                │
│         │                │                                         │
└─────────┼────────────────┼─────────────────────────────────────────┘
          │                │
          │ Tailscale      │ Redis (via API or direct)
          │ Mesh           │
          ▼                ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      AceTeam Control Plane                          │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │                     Next.js API Routes                       │   │
│  │                                                              │   │
│  │  GET /api/fabric/nodes           ← List nodes with status    │   │
│  │  GET /api/fabric/nodes/:id       ← Node details (cached)     │   │
│  │  GET /api/fabric/nodes/:id/live  ← Query node directly       │   │
│  │                                                              │   │
│  └───────────────────────────────┬──────────────────────────────┘   │
│                                  │                                  │
│                       ┌──────────┴──────────┐                       │
│                       ▼                     ▼                       │
│              ┌─────────────────┐   ┌─────────────────┐              │
│              │   PostgreSQL    │   │   Redis         │              │
│              │  (node_status)  │   │  (pub/sub +     │              │
│              └─────────────────┘   │   streams)      │              │
│                                    └─────────────────┘              │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 Data Flow

**Redis Status Flow (Push)**
1. Citadel node collects status every 30 seconds
2. Publishes to Redis via Pub/Sub (direct Redis or API proxy)
3. Python worker validates, looks up org, upserts into `fabric_node_status` table
4. Worker republishes to org-scoped Pub/Sub channel for real-time dashboard

**On-Demand Flow (Pull)**
1. User clicks on node in Fabric page
2. Frontend calls `/api/fabric/nodes/{nodeId}/live`
3. Backend queries Citadel's HTTP server via Tailscale IP
4. Returns detailed status including model lists

### 3.3 Status Payload Structure

```json
{
  "version": "1.0",
  "timestamp": "2024-01-12T10:30:00Z",
  "node": {
    "name": "gpu-node-01",
    "tailscale_ip": "100.64.0.1",
    "uptime_seconds": 86400
  },
  "system": {
    "cpu_percent": 45.2,
    "memory_used_gb": 24.5,
    "memory_total_gb": 64.0,
    "disk_used_gb": 450,
    "disk_total_gb": 1000
  },
  "gpu": [{
    "index": 0,
    "name": "NVIDIA RTX 3090",
    "memory_used_mb": 4200,
    "memory_total_mb": 24576,
    "utilization_percent": 85,
    "temperature_celsius": 72
  }],
  "services": [{
    "name": "vllm",
    "type": "llm",
    "status": "running",
    "port": 8000,
    "health": "healthy",
    "models": ["meta-llama/Llama-3-8b", "mistralai/Mistral-7B"]
  }, {
    "name": "postgres",
    "type": "database",
    "status": "running",
    "port": 5432,
    "health": "healthy"
  }]
}
```

## 4. Implementation Plan

### 4.1 Phase 1: HTTP Status Server

**Goal**: Enable on-demand queries from the control plane.

Create an HTTP server that exposes node status on the Tailscale interface:

```go
// internal/status/server.go
type StatusServer struct {
    collector *Collector
    port      int
}

// Endpoints:
// GET /status  - Full status payload
// GET /health  - Simple health check (200 OK)
// GET /metrics - Prometheus-compatible metrics (future)
```

**Files**:
- `internal/status/server.go` - HTTP server
- `internal/status/collector.go` - Gathers metrics

### 4.2 Phase 2: Status Collector

**Goal**: Unified interface for gathering system, GPU, and service metrics.

```go
// internal/status/collector.go
type Collector struct {
    manifest *Manifest
}

func (c *Collector) Collect() (*NodeStatus, error) {
    // 1. System metrics (CPU, memory, disk)
    // 2. GPU metrics (nvidia-smi)
    // 3. Service status (docker inspect)
    // 4. Model discovery (service-specific)
}
```

**Model Discovery**:
| Service | API | Response |
|---------|-----|----------|
| vLLM | `GET http://localhost:8000/v1/models` | OpenAI-compatible model list |
| Ollama | `GET http://localhost:11434/api/tags` | Ollama tag list |

### 4.3 Phase 3: Redis Status Publisher

**Goal**: Periodic status reporting to control plane via Redis.

The original design proposed an HTTP heartbeat client that would POST to
`/api/fabric/nodes/{nodeId}/heartbeat`. This was superseded by Redis-based
publishers that use either direct Redis or the authenticated API proxy:

- **RedisPublisher** (`internal/heartbeat/redis.go`): Direct Redis Pub/Sub + Streams
- **APIPublisher** (`internal/heartbeat/api.go`): Redis via authenticated API proxy (preferred)

**Integration**: Started as goroutine when `citadel work` runs with `--redis-status` (enabled by default).

### 4.4 Phase 4: Model Discovery

**Goal**: Query running LLM services for loaded models.

```go
// internal/status/models.go

func DiscoverVLLMModels(port int) ([]string, error) {
    resp, _ := http.Get(fmt.Sprintf("http://localhost:%d/v1/models", port))
    // Parse OpenAI-compatible response
}

func DiscoverOllamaModels(port int) ([]string, error) {
    resp, _ := http.Get(fmt.Sprintf("http://localhost:%d/api/tags", port))
    // Parse Ollama tag list
}
```

## 5. Security Considerations

### 5.1 Network Security

All communication occurs over the Tailscale mesh:
- Encrypted (WireGuard)
- Authenticated (Tailscale ACLs)
- No public internet exposure

### 5.2 Status Publisher Authentication

- **API mode**: Uses device API token obtained during `citadel init`, authenticated via the API proxy
- **Direct Redis**: Connects directly to Redis with credentials from config

## 6. Testing Strategy

### 6.1 Unit Tests

- `internal/status/collector_test.go` - Mock system calls
- `internal/status/server_test.go` - HTTP handler tests
- `internal/heartbeat/redis_test.go` - Redis publisher tests
- `internal/heartbeat/config_consumer_test.go` - Config consumer tests
- `internal/heartbeat/config_subscriber_test.go` - Config subscriber tests

### 6.2 Integration Tests

- Start status server, verify endpoints
- Mock AceTeam API, verify heartbeat delivery
- Verify graceful shutdown

### 6.3 End-to-End Tests

- Deploy Citadel node with services
- Verify status appears on Fabric page
- Test model discovery with real vLLM/Ollama

## 7. Future Enhancements

1. **Prometheus metrics**: Expose `/metrics` endpoint for monitoring
2. **Event streaming**: WebSocket for real-time updates
3. **Health checks**: Deeper service health validation
4. **Alerts**: Trigger notifications on degraded status

## 8. References

- [Kubernetes Kubelet Architecture](https://kubernetes.io/docs/concepts/overview/components/#kubelet)
- [Consul Agent Architecture](https://developer.hashicorp.com/consul/docs/architecture)
- [Tailscale WireGuard Security](https://tailscale.com/blog/how-tailscale-works)
- [Heartbeat Failure Detection](https://www.distributed-systems.net/index.php/papers/failure-detection/)

---

## Appendix A: Files to Create/Modify

### Citadel-CLI

| File | Action | Purpose |
|------|--------|---------|
| `internal/status/server.go` | Create | HTTP status server |
| `internal/status/collector.go` | Create | Metrics collection |
| `internal/status/models.go` | Create | Model discovery |
| `internal/status/types.go` | Create | Status data types |
| `internal/heartbeat/redis.go` | Create | Redis status publisher (direct) |
| `internal/heartbeat/api.go` | Create | Redis status publisher (via API proxy) |
| `cmd/work.go` | Modify | Start status server + Redis publisher |

### Tests

| File | Purpose |
|------|---------|
| `internal/status/collector_test.go` | Collector unit tests |
| `internal/status/server_test.go` | HTTP server tests |
| `internal/heartbeat/redis_test.go` | Redis publisher tests |
| `internal/heartbeat/config_consumer_test.go` | Config consumer tests |
| `internal/heartbeat/config_subscriber_test.go` | Config subscriber tests |
