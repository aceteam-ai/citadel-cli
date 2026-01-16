# Redis-Based Status Publishing and Config Application

**Issue:** [#30](https://github.com/aceteam-ai/citadel-cli/issues/30)

## Overview

This feature enables real-time node status publishing via Redis for live dashboard updates and reliable device configuration from the onboarding wizard. It bridges the gap between the Citadel CLI agent and the AceTeam web platform.

## Architecture

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

### Data Flow

1. **Status Publishing**: Citadel publishes node status to Redis every 30 seconds
   - Pub/Sub channel for real-time UI updates
   - Stream for reliable processing by Python worker
2. **Status Persistence**: Python worker consumes from stream, persists to `fabric_nodes` table
3. **Config Lookup**: When status contains `deviceCode`, Python worker looks up onboarding config
4. **Config Push**: Python worker pushes `APPLY_DEVICE_CONFIG` job to Redis
5. **Config Application**: Citadel applies configuration (updates manifest, starts services)

## Components

### 1. Redis Status Publisher

**Location:** `internal/heartbeat/redis.go`

Publishes node status to Redis for real-time UI updates and reliable processing.

### 1a. Config Queue Consumer

**Location:** `internal/heartbeat/config_consumer.go`

Consumes device configuration jobs from the `jobs:v1:config` Redis stream. This runs in parallel with the main status publisher to apply configurations from the onboarding wizard.

```go
type ConfigConsumer struct {
    client        *redis.Client
    workerID      string
    queueName     string      // "jobs:v1:config"
    consumerGroup string      // "citadel-config-consumers"
    configHandler *jobs.ConfigHandler
}
```

**Key Methods:**
- `NewConfigConsumer(cfg)` - Create new config consumer
- `Start(ctx)` - Start consuming config jobs (blocking)
- `Close()` - Close Redis connection

**Behavior:**
1. Connects to Redis and creates consumer group `citadel-config-consumers`
2. Reads jobs from `jobs:v1:config` stream using XREADGROUP
3. Parses `APPLY_DEVICE_CONFIG` jobs and delegates to ConfigHandler
4. ACKs successfully processed jobs

```go
type RedisPublisher struct {
    client     *redis.Client
    nodeID     string
    deviceCode string  // Set after device auth
    interval   time.Duration
    collector  *status.Collector
}

type RedisPublisherConfig struct {
    RedisURL      string        // Redis connection URL
    RedisPassword string        // Optional password
    NodeID        string        // Node identifier (hostname)
    DeviceCode    string        // Device auth code for config lookup
    Interval      time.Duration // Publish interval (default: 30s)
}
```

**Key Methods:**
- `NewRedisPublisher(cfg, collector)` - Create new publisher
- `Start(ctx)` - Start periodic publishing (blocking)
- `PublishOnce(ctx)` - Send single status update
- `SetDeviceCode(code)` - Update device code after auth
- `Close()` - Close Redis connection

### 2. Status Message Schema

```go
type StatusMessage struct {
    Version    string             `json:"version"`
    Timestamp  string             `json:"timestamp"`      // RFC3339
    NodeID     string             `json:"nodeId"`
    DeviceCode string             `json:"deviceCode,omitempty"`
    Status     *status.NodeStatus `json:"status"`
}
```

**Example Payload:**
```json
{
  "version": "1.0",
  "timestamp": "2024-01-15T12:00:00Z",
  "nodeId": "gpu-server-1",
  "deviceCode": "abc123def456",
  "status": {
    "version": "1.0",
    "timestamp": "2024-01-15T12:00:00Z",
    "node": {
      "name": "gpu-server-1",
      "tailscale_ip": "100.64.0.5",
      "uptime_seconds": 86400
    },
    "system": {
      "cpu_percent": 45.2,
      "memory_used_gb": 24.5,
      "memory_total_gb": 64.0,
      "memory_percent": 38.3,
      "disk_used_gb": 250.0,
      "disk_total_gb": 1000.0,
      "disk_percent": 25.0
    },
    "gpu": [
      {
        "index": 0,
        "name": "NVIDIA RTX 4090",
        "memory_total_mb": 24576,
        "utilization_percent": 85.0,
        "temperature_celsius": 72
      }
    ],
    "services": [
      {
        "name": "vllm",
        "type": "llm",
        "status": "running",
        "port": 8000,
        "health": "ok",
        "models": ["meta-llama/Llama-2-7b-chat-hf"]
      }
    ]
  }
}
```

### 3. Device Config Handler

**Location:** `internal/jobs/config_handler.go`

Handles `APPLY_DEVICE_CONFIG` jobs to configure the node based on onboarding wizard selections.

```go
type DeviceConfig struct {
    DeviceName              string   `json:"deviceName"`
    Services                []string `json:"services"`
    AutoStartServices       bool     `json:"autoStartServices"`
    SSHEnabled              bool     `json:"sshEnabled"`
    CustomTags              []string `json:"customTags"`
    HealthMonitoringEnabled bool     `json:"healthMonitoringEnabled"`
    AlertOnOffline          bool     `json:"alertOnOffline"`
    AlertOnHighTemp         bool     `json:"alertOnHighTemp"`
}
```

**Handler Behavior:**
1. Parse config from job payload
2. Update `citadel.yaml` manifest with new settings
3. Write service compose files to `~/citadel-node/services/`
4. Start services if `autoStartServices` is true
5. Return success/failure status

## Redis Keys/Channels

| Key Pattern | Type | Purpose | TTL/MaxLen |
|-------------|------|---------|------------|
| `node:status:{nodeId}` | Pub/Sub | Real-time status updates for UI | N/A |
| `node:status:stream` | Stream | Reliable status processing | ~10,000 entries |
| `jobs:v1:config` | Stream | Config jobs from Python worker | Varies |

### Stream Entry Format

**node:status:stream:**
```
XADD node:status:stream * \
  nodeId "gpu-server-1" \
  timestamp "2024-01-15T12:00:00Z" \
  deviceCode "abc123" \
  payload '{"version":"1.0",...}'
```

**jobs:v1:config:**
```
XADD jobs:v1:config * \
  jobId "uuid-here" \
  type "APPLY_DEVICE_CONFIG" \
  payload '{"config":{"services":["ollama"],"autoStartServices":true}}'
```

## Usage

### Command-Line Flags

```bash
# Full command with all flags
citadel work \
  --mode=redis \
  --redis-url=redis://localhost:6379 \
  --redis-password=secret \
  --redis-status \
  --device-code=abc123 \
  --queue=jobs:v1:gpu-general \
  --group=citadel-workers
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `REDIS_URL` | Redis connection URL | Required |
| `REDIS_PASSWORD` | Redis password | None |
| `CITADEL_DEVICE_CODE` | Device auth code | None |
| `WORKER_QUEUE` | Queue to consume from | `jobs:v1:gpu-general` |
| `CONSUMER_GROUP` | Consumer group name | `citadel-workers` |
| `CITADEL_NODE_NAME` | Node identifier | Hostname |

### Example: Full Worker Startup

```bash
export REDIS_URL=redis://redis.aceteam.ai:6379
export REDIS_PASSWORD=your-password
export CITADEL_DEVICE_CODE=abc123def456
export CITADEL_NODE_NAME=my-gpu-server

citadel work --mode=redis --redis-status
```

## Verification & Testing

### 1. Status Publishing

```bash
# Terminal 1: Start citadel worker
REDIS_URL=redis://localhost:6379 citadel work --mode=redis --redis-status

# Terminal 2: Monitor Pub/Sub
redis-cli PSUBSCRIBE "node:status:*"

# Terminal 3: Check Stream
redis-cli XREAD STREAMS node:status:stream 0

# Check stream length
redis-cli XLEN node:status:stream
```

### 2. Config Application

```bash
# Push a test config job
redis-cli XADD jobs:v1:config '*' \
  jobId "test-$(date +%s)" \
  type "APPLY_DEVICE_CONFIG" \
  payload '{"config":{"deviceName":"test-node","services":["ollama"],"autoStartServices":true,"customTags":["test"]}}'

# Verify manifest was updated
cat ~/citadel-node/citadel.yaml

# Verify services started
docker ps | grep citadel-
```

### 3. End-to-End Flow

1. Run `citadel init` with device authorization
2. Note the device code displayed during auth
3. Complete onboarding wizard in browser at aceteam.ai
4. Watch worker logs for `APPLY_DEVICE_CONFIG` job
5. Verify services auto-start based on wizard selection

## Troubleshooting

### Common Issues

**Redis Connection Failed:**
```
Error: failed to connect to Redis: dial tcp: lookup redis: no such host
```
- Verify `REDIS_URL` is correct
- Check Redis is running: `redis-cli ping`
- Check network connectivity

**Status Not Publishing:**
```
Warning: Redis status publish failed: ...
```
- Check Redis connection
- Verify worker has `--redis-status` flag
- Check Redis memory usage

**Config Job Not Applied:**
```
Error: missing 'config' field in job payload
```
- Verify job payload format matches expected schema
- Check `config` is a JSON string in the payload

### Debug Commands

```bash
# Check Redis connection
redis-cli -u $REDIS_URL ping

# Monitor all Redis traffic
redis-cli -u $REDIS_URL MONITOR

# Check pending messages in stream
redis-cli XPENDING node:status:stream citadel-workers

# View recent stream entries
redis-cli XREVRANGE node:status:stream + - COUNT 5
```

## Related Issues

- **aceteam-ai/aceteam#1222**: Python Worker - Node Status Consumer
- **aceteam-ai/aceteam#1223**: Python Worker - Device Config Job Publisher
- **aceteam-ai/aceteam#1224**: Frontend - Real-time Status Updates

## Files Changed

| File | Action | Description |
|------|--------|-------------|
| `internal/heartbeat/redis.go` | CREATE | Redis status publisher |
| `internal/heartbeat/redis_test.go` | CREATE | Publisher tests |
| `internal/heartbeat/config_consumer.go` | CREATE | Config queue consumer for device config jobs |
| `internal/heartbeat/config_consumer_test.go` | CREATE | Config consumer tests |
| `internal/jobs/config_handler.go` | CREATE | Device config job handler |
| `internal/worker/job.go` | MODIFY | Add `JobTypeApplyDeviceConfig` |
| `internal/worker/handler_adapter.go` | MODIFY | Register config handler |
| `cmd/helpers.go` | MODIFY | Return device_code from auth flow |
| `cmd/work.go` | MODIFY | Add `--redis-status`, `--device-code` flags, start config consumer |

## Security Considerations

1. **Redis Authentication**: Always use password authentication in production
2. **Device Code Handling**: Device codes are single-use and expire after 10 minutes
3. **Config Validation**: Config handler validates service names against known services
4. **No Sensitive Data in Status**: Status payloads contain metrics only, no credentials
