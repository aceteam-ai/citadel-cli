---
sidebar_position: 4
title: Testing
---

# Testing

Citadel uses three levels of testing: unit tests for isolated logic, integration tests for full command lifecycle, and end-to-end tests for multi-node scenarios.

## Unit Tests

Run all unit tests:

```bash
go test ./...
```

Run with verbose output:

```bash
go test -v ./...
```

Run a specific test:

```bash
go test -v ./cmd -run TestReadManifest
```

Unit tests focus on:

- Manifest parsing and validation
- Utility functions and platform abstractions
- Job handler logic with mock inputs
- Configuration loading and defaults

## Integration Tests

Integration tests spin up a mock Nexus server using Docker and test the full command lifecycle:

```bash
./tests/integration.sh
```

This script:

1. Starts a mock Nexus server via `docker-compose.test.yml`
2. Runs the Citadel agent against the mock server
3. Verifies job dispatch, execution, and status reporting
4. Tears down the test environment

**Requirements:** Docker must be installed and running.

## End-to-End Tests

E2E tests validate multi-node scenarios and require a running AceTeam environment:

```bash
go test ./e2e/... -v
```

**Required environment variables:**

| Variable | Description |
|----------|-------------|
| `ACETEAM_URL` | URL of the AceTeam API |
| `REDIS_URL` | Redis connection URL |

These tests exercise the full stack: network connectivity, job queue processing, service management, and status reporting.

## Windows E2E Tests

A remote E2E test script validates the first-time user experience on a Windows machine via WinRM:

```bash
./scripts/windows-e2e-test.sh \
  --host 192.168.2.207 \
  --user acewin \
  --password '***' \
  --authkey tskey-auth-xxx
```

This tests four phases on the remote Windows machine:

| Phase | What It Does |
|-------|--------------|
| **Clean** | Removes Docker, Citadel directories, PATH entries, WSL |
| **Install** | Runs the install script, verifies binary placement |
| **Provision** | Runs `citadel init` with authkey, verifies network join |
| **Verify** | Checks status, service health, and connectivity |

**Requirements:**

- `pywinrm` Python package (`pip install pywinrm`)
- WinRM enabled on the target machine
- Network access to the target on port 5985

Additional options:

```bash
# Run a single phase
./scripts/windows-e2e-test.sh verify --host ...

# Skip the clean phase
./scripts/windows-e2e-test.sh --skip-clean --host ...

# Test a specific version
./scripts/windows-e2e-test.sh --version v2.3.0 --host ...

# Preview without executing
./scripts/windows-e2e-test.sh --dry-run --host ...
```

## Test Philosophy

- **Unit tests** cover parsing, utilities, and pure logic. They run fast and have no external dependencies.
- **Integration tests** cover command lifecycle with a mock server. They require Docker but no real infrastructure.
- **E2E tests** cover real-world multi-node scenarios. They require a running AceTeam environment and are typically run before releases.

When adding new features, write unit tests for the core logic and integration tests for the command-level behavior. E2E tests are added for features that involve cross-node communication or infrastructure changes.
