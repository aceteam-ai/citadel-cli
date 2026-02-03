# CLAUDE.md - E2E Tests

End-to-end integration tests for the Citadel + AceTeam platform pipeline.

## Overview

Go test suite that validates the full Fabric pipeline: device authentication, heartbeat reporting, and job distribution across Citadel nodes. Tests run against a live (or local) AceTeam platform instance with Redis.

## Structure

```
e2e/
├── main_test.go              # Test entrypoint, environment logging
├── device_auth_test.go       # Device registration and auth flow
├── heartbeat_test.go         # Citadel heartbeat/status reporting
├── job_distribution_test.go  # Job queue distribution across nodes
├── hello-citadel.sh          # Quick smoke test script
└── harness/                  # Test helpers
    ├── aceteam.go            # AceTeam API client
    ├── citadel.go            # Citadel process management
    ├── k8s.go                # Kubernetes helpers
    └── redis.go              # Redis client helpers
```

## Commands

```bash
# Run all e2e tests (from citadel-cli root)
go test ./e2e/... -v

# Run specific test
go test ./e2e/... -run TestDeviceAuth -v

# Run with custom environment
ACETEAM_URL=http://localhost:3000 REDIS_URL=redis://localhost:6379 go test ./e2e/... -v

# Quick smoke test
./e2e/hello-citadel.sh
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ACETEAM_URL` | `http://localhost:3000` | AceTeam platform URL |
| `REDIS_URL` | `redis://localhost:6379` | Redis connection URL |
| `WORKER_QUEUE` | `jobs:v1:e2e-test` | Queue name for test jobs |
| `CITADEL_BINARY` | (none) | Path to citadel binary |

## Prerequisites

- Go 1.22+
- Running AceTeam platform instance
- Running Redis instance
- Citadel binary (optional, for full integration tests)
