# Plan: Unify Citadel Worker Architecture

## Problem Statement

Currently, citadel-cli has multiple worker-like commands with overlapping concerns:

| Command | Job Source | Starts Services | Streaming | Use Case |
|---------|-----------|-----------------|-----------|----------|
| `citadel up` | Nexus HTTP | Yes | No | User's on-prem |
| `citadel agent` | Nexus HTTP | No | No | Standalone |
| `citadel worker` | Redis Streams | No | Redis Pub/Sub | AceTeam private cloud |

**Issues:**
1. **User confusion**: Which command to use?
2. **Code duplication**: Similar startup/shutdown, signal handling, loops
3. **Architecture**: No unified job processing abstraction
4. **Feature parity**: Status reporting only planned for `up`/`worker`, not `agent`

---

## Proposed Architecture

### Unified Command Structure

```
citadel run [--mode=MODE] [--services]

Modes:
  nexus   - Poll Nexus API for jobs (current citadel up/agent behavior)
  redis   - Consume from Redis Streams (current citadel worker behavior)
  hybrid  - Run both (future)

Flags:
  --services     Start services from manifest before running
  --status-port  Enable status HTTP server on port (default: 8080)
  --heartbeat    Enable heartbeat reporting to AceTeam API
```

### Simplified User Experience

| Old | New |
|-----|-----|
| `citadel up` | `citadel run --services --mode=nexus` |
| `citadel agent` | `citadel run --mode=nexus` |
| `citadel worker` | `citadel run --mode=redis` |

**Convenience aliases:**
- `citadel up` → Alias for `citadel run --services --mode=nexus`
- `citadel worker` → Alias for `citadel run --mode=redis`

---

## Core Abstraction

### JobSource Interface

```go
// internal/worker/source.go
type JobSource interface {
    // Name returns the source identifier (e.g., "nexus", "redis")
    Name() string

    // Connect establishes connection to the job source
    Connect(ctx context.Context) error

    // Next blocks until a job is available or context is cancelled
    Next(ctx context.Context) (*Job, error)

    // Ack acknowledges successful job completion
    Ack(ctx context.Context, job *Job) error

    // Nack indicates job failure (for retry/DLQ)
    Nack(ctx context.Context, job *Job, err error) error

    // Close cleanly disconnects
    Close() error
}
```

### Job Handler Interface

```go
// internal/worker/handler.go
type JobHandler interface {
    // CanHandle returns true if this handler can process the job type
    CanHandle(jobType string) bool

    // Execute processes the job, streaming results if supported
    Execute(ctx context.Context, job *Job, stream StreamWriter) error
}

type StreamWriter interface {
    WriteChunk(data []byte) error
    WriteEnd(result map[string]any) error
    WriteError(err error) error
}
```

### Worker Runner

```go
// internal/worker/runner.go
type Runner struct {
    source       JobSource
    handlers     []JobHandler
    statusServer *status.Server
    heartbeat    *heartbeat.Client
}

func (r *Runner) Run(ctx context.Context) error {
    // Start status server (if enabled)
    // Start heartbeat (if enabled)
    // Main loop: source.Next() → dispatch to handler → ack/nack
}
```

---

## Implementation

### New Files

```
internal/worker/
├── source.go       # JobSource interface
├── handler.go      # JobHandler interface
├── runner.go       # Main worker runner
├── nexus_source.go # Nexus HTTP polling (from cmd/agent.go)
├── redis_source.go # Redis Streams (from cmd/worker.go)
└── stream.go       # StreamWriter implementations
```

### Modified Files

| File | Changes |
|------|---------|
| `cmd/run.go` | New unified command |
| `cmd/up.go` | Thin wrapper → `citadel run --services --mode=nexus` |
| `cmd/worker.go` | Thin wrapper → `citadel run --mode=redis` |
| `cmd/agent.go` | Deprecate or remove (use `run --mode=nexus`) |

### Migration Path

1. **Phase 1**: Create `internal/worker/` package with abstractions
2. **Phase 2**: Implement NexusSource (extract from agent.go)
3. **Phase 3**: Implement RedisSource (extract from worker.go)
4. **Phase 4**: Create `cmd/run.go` using new abstractions
5. **Phase 5**: Refactor `up.go` and `worker.go` as thin wrappers
6. **Phase 6**: Add status server + heartbeat integration
7. **Phase 7**: Deprecate `citadel agent` (alias to `run`)

---

## Data Flow

```
┌─────────────────────────────────────────────────────────────┐
│                     citadel run                              │
│                                                             │
│  ┌─────────────────┐      ┌─────────────────────────────┐  │
│  │ Service Startup │──────▶│        Worker Runner        │  │
│  │ (if --services) │      │                             │  │
│  └─────────────────┘      │  ┌───────────┐  ┌────────┐  │  │
│                           │  │ JobSource │  │Handlers│  │  │
│                           │  │ (nexus/   │  │(shell, │  │  │
│                           │  │  redis)   │  │ llm,..)│  │  │
│                           │  └─────┬─────┘  └───┬────┘  │  │
│                           │        │            │       │  │
│                           │        ▼            ▼       │  │
│                           │     ┌─────────────────┐     │  │
│                           │     │  Job Dispatch   │     │  │
│                           │     └─────────────────┘     │  │
│                           └─────────────────────────────┘  │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              Background Services                     │   │
│  │  ┌─────────────────┐    ┌─────────────────────┐     │   │
│  │  │  Status Server  │    │  Heartbeat Client   │     │   │
│  │  │  GET /status    │    │  POST /heartbeat    │     │   │
│  │  │  GET /health    │    │  (30s interval)     │     │   │
│  │  └─────────────────┘    └─────────────────────┘     │   │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

---

## Files to Create/Modify

### Create

| File | Purpose |
|------|---------|
| `internal/worker/source.go` | JobSource interface |
| `internal/worker/handler.go` | JobHandler interface |
| `internal/worker/runner.go` | Main worker loop |
| `internal/worker/nexus_source.go` | Nexus HTTP polling |
| `internal/worker/redis_source.go` | Redis Streams consumer |
| `internal/worker/stream.go` | Stream writer implementations |
| `cmd/run.go` | Unified `citadel run` command |

### Modify

| File | Changes |
|------|---------|
| `cmd/up.go` | Delegate to `runCmd` |
| `cmd/worker.go` | Delegate to `runCmd` |
| `cmd/agent.go` | Deprecation notice, alias to run |

---

## Verification

1. **Nexus mode**: `citadel run --mode=nexus` polls Nexus correctly
2. **Redis mode**: `citadel run --mode=redis` consumes from Redis Streams
3. **Services**: `citadel run --services` starts Docker services first
4. **Backwards compat**: `citadel up` works identically to before
5. **Status server**: `curl localhost:8080/status` returns node status
6. **Tests**: Existing tests continue to pass

---

## Benefits

1. **User clarity**: One command with clear modes
2. **Code reuse**: Shared runner, handlers, status reporting
3. **Feature parity**: All modes get heartbeat + status server
4. **Extensibility**: Easy to add new job sources (e.g., NATS, Kafka)
5. **Testability**: JobSource interface makes testing easier
