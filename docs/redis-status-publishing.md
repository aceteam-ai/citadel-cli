# Redis-Based Status Publishing and Config Application

**Issue:** [#30](https://github.com/aceteam-ai/citadel-cli/issues/30)

## Architecture Overview

```
┌─────────────────┐                      ┌─────────────────┐
│   Citadel CLI   │                      │  Python Worker  │
│                 │                      │  (DB access)    │
└────────┬────────┘                      └────────┬────────┘
         │                                        │
         │  1. Publish status (with device_code)  │
         │ ─────────────────────────────────────▶ │
         │    (Pub/Sub: real-time)                │
         │    (Streams: reliable persistence)     │
         │                                        │
         │                                        │  2. Persist to DB
         │                                        │  3. Lookup device config
         │                                        │
         │  4. Push config job via Stream         │
         │ ◀───────────────────────────────────── │
         │                                        │
         │  5. Apply config (start services, etc) │
         │                                        │
```

**Data Flow:**
1. CLI publishes status to Redis (both Pub/Sub for real-time UI and Streams for reliable processing)
2. Python worker consumes status from Streams, persists to database
3. Python worker looks up device config (from onboarding wizard) and pushes config job
4. CLI receives config job via existing worker infrastructure
5. CLI applies configuration (starts services, sets tags, etc.)

## Implementation Components

### 1. Redis Status Publisher (`internal/heartbeat/redis.go`)

Publishes node status to Redis for real-time UI updates and reliable processing.

**Key Features:**
- Publishes to Pub/Sub for real-time UI updates (`node:status:{nodeId}`)
- Publishes to Streams for reliable processing (`node:status:stream`)
- Includes device_code in status payload for config lookup
- Reuses existing `status.Collector` for metrics

### 2. Device Config Job Handler (`internal/jobs/config_handler.go`)

Handles `APPLY_DEVICE_CONFIG` jobs to configure the node based on onboarding wizard selections.

**DeviceConfig Fields:**
- `deviceName`: Node display name
- `services`: Services to run (vllm, ollama, etc.)
- `autoStartServices`: Whether to auto-start services
- `sshEnabled`: Enable SSH access
- `customTags`: Tags for node classification
- `healthMonitoringEnabled`: Enable health monitoring
- `alertOnOffline`: Alert when node goes offline
- `alertOnHighTemp`: Alert on high GPU temperature

### 3. Status Message Schema

```go
type StatusMessage struct {
    Version    string             `json:"version"`
    Timestamp  string             `json:"timestamp"`
    NodeID     string             `json:"nodeId"`
    DeviceCode string             `json:"deviceCode,omitempty"`
    Status     *status.NodeStatus `json:"status"`
}
```

## Redis Keys/Channels

| Key Pattern | Type | Purpose |
|-------------|------|---------|
| `node:status:{nodeId}` | Pub/Sub | Real-time status updates for UI |
| `node:status:stream` | Stream | Reliable status processing |
| `jobs:v1:config` | Stream | Config jobs pushed by Python worker |

## Usage

### Starting the Worker with Redis Status Publishing

```bash
# Environment variables
export REDIS_URL=redis://localhost:6379
export CITADEL_DEVICE_CODE=abc123  # Optional, from device auth

# Run worker with Redis status publishing
citadel work --mode=redis --redis-url=$REDIS_URL
```

### Command-Line Flags

The Redis status publisher is automatically started when running in Redis mode:

```bash
citadel work --mode=redis \
  --redis-url=redis://localhost:6379 \
  --queue=jobs:v1:gpu-general
```

Device code can be set via:
- `CITADEL_DEVICE_CODE` environment variable
- Automatically captured during device auth flow

## Verification

### 1. Status Publishing

```bash
# Start citadel with Redis
REDIS_URL=redis://localhost:6379 citadel work --mode=redis

# Monitor Pub/Sub
redis-cli SUBSCRIBE "node:status:*"

# Check Stream
redis-cli XREAD STREAMS node:status:stream 0
```

### 2. Config Application

```bash
# Manually push a config job
redis-cli XADD jobs:v1:config '*' \
  jobId "test-123" \
  type "APPLY_DEVICE_CONFIG" \
  payload '{"services":["ollama"],"autoStartServices":true}'

# Verify service starts
docker ps | grep citadel-ollama
```

### 3. End-to-End

1. Run `citadel init` with device auth
2. Complete onboarding wizard in browser
3. Verify services auto-start based on wizard selection

## Related Issues

- **aceteam-ai/aceteam#1222**: Python Worker - Node Status Consumer (consume from `node:status:stream`, persist to DB)
- **aceteam-ai/aceteam#1223**: Python Worker - Device Config Job Publisher (push config jobs on new device registration)
- **aceteam-ai/aceteam#1224**: Frontend - Real-time Status Updates (subscribe to Pub/Sub for live dashboard)

## Files Changed

| File | Action | Description |
|------|--------|-------------|
| `internal/heartbeat/redis.go` | CREATE | Redis status publisher |
| `internal/jobs/config_handler.go` | CREATE | Device config job handler |
| `cmd/helpers.go` | MODIFY | Return device_code from auth flow |
| `cmd/work.go` | MODIFY | Start Redis publisher, integrate config handler |

## Python Worker Responsibilities (Out of Scope)

The Python worker will need to:
1. Subscribe to `node:status:stream` as a consumer
2. Persist status to database (fabric_nodes table)
3. On new device_code, lookup device_config and push job to `jobs:v1:config`
