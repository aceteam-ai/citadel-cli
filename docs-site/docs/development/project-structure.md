---
sidebar_position: 2
title: Project Structure
---

# Project Structure

An overview of the Citadel CLI repository layout, key files, and internal package responsibilities.

## Directory Tree

```
citadel-cli/
├── main.go                    # Entry point
├── build.sh                   # Build script (single or all platforms)
├── release.sh                 # Release automation
├── go.mod / go.sum            # Go module definition
│
├── cmd/                       # Cobra command implementations
│   ├── root.go                # Base command and global flags
│   ├── version.go             # Version variable (injected at build time)
│   ├── init.go                # Node provisioning and network join
│   ├── work.go                # Worker mode (Redis Streams job processing)
│   ├── run.go                 # Start services from manifest
│   ├── stop.go                # Stop services
│   ├── status.go              # Health dashboard
│   ├── login.go               # AceTeam Network authentication
│   ├── logout.go              # Disconnect from network
│   ├── logs.go                # Service log streaming
│   ├── test.go                # Service diagnostic testing
│   ├── job_handlers.go        # Job handler registration
│   ├── peers.go               # Service discovery
│   ├── call.go                # Inter-node HTTP calls
│   ├── ping.go                # Node reachability check
│   ├── ssh.go                 # SSH to peer nodes
│   ├── proxy.go               # HTTP proxy to remote services
│   ├── expose.go              # Expose local services to fabric
│   ├── update.go              # Auto-update management
│   ├── service_cmd.go         # System service management
│   ├── manifest.go            # Manifest loading and parsing
│   └── ...                    # Additional command files
│
├── internal/                  # Private packages
│   ├── network/               # Embedded tsnet wrapper
│   ├── worker/                # Redis Streams job runner
│   ├── jobs/                  # Job handler implementations
│   ├── nexus/                 # Nexus API client and device auth
│   ├── platform/              # Cross-platform utilities
│   ├── heartbeat/             # Status publishing (HTTP + Redis)
│   ├── status/                # System metrics collection
│   ├── redis/                 # Redis Streams client
│   ├── redisapi/              # Redis API utilities
│   ├── terminal/              # WebSocket terminal server
│   ├── discovery/             # Service discovery client
│   ├── fabricserver/          # Fabric server for inter-node communication
│   ├── capabilities/          # Node capability detection
│   ├── services/              # Service management utilities
│   ├── tui/                   # Terminal UI components
│   ├── ui/                    # Interactive prompts (survey)
│   ├── update/                # Auto-update logic
│   ├── usage/                 # Usage tracking
│   ├── recommend/             # Service recommendations
│   ├── compose/               # Docker Compose helpers
│   └── demo/                  # Demo mode utilities
│
├── services/
│   ├── embed.go               # go:embed directives for compose files
│   └── compose/               # Embedded Docker Compose files
│       ├── vllm.yml
│       ├── ollama.yml
│       ├── llamacpp.yml
│       ├── lmstudio.yml
│       └── extraction.yml
│
├── e2e/                       # End-to-end integration tests
├── tests/                     # Integration test scripts
│   └── integration.sh
├── scripts/                   # Build, release, and testing scripts
│   └── windows-e2e-test.sh
├── packaging/                 # Platform-specific packaging (macOS app bundle)
├── homebrew-tap/              # Homebrew formula
├── .winget/                   # Windows Package Manager manifest
├── docs/                      # Technical documentation
└── docs-site/                 # Docusaurus documentation site
```

## Key Files

| File | Purpose |
|------|---------|
| `main.go` | Program entry point; calls `cmd.Execute()` |
| `cmd/root.go` | Root Cobra command, global flags, and initialization |
| `cmd/version.go` | `Version` variable, injected via ldflags at build time |
| `services/embed.go` | `go:embed` directives that bundle compose files into the binary |
| `cmd/manifest.go` | `CitadelManifest` struct and manifest discovery logic |
| `internal/worker/handler.go` | `JobHandler` and `StreamWriter` interfaces for the worker |
| `internal/jobs/handler.go` | `JobHandler` interface for Nexus-mode job execution |

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `network` | Wraps embedded tsnet for AceTeam Network connectivity |
| `worker` | Redis Streams job runner with consumer groups and retry logic |
| `jobs` | Concrete job handler implementations (shell, inference, config) |
| `nexus` | HTTP client for Nexus API, device authorization, SSH key sync |
| `platform` | Cross-platform abstractions for OS, packages, Docker, GPU, users |
| `heartbeat` | Publishes node status to AceTeam API and Redis at regular intervals |
| `status` | Collects system metrics (CPU, memory, disk, GPU, services) |
| `redis` | Low-level Redis Streams client for job queue operations |
| `redisapi` | Higher-level Redis API utilities |
| `terminal` | WebSocket terminal server with PTY management and token auth |
| `discovery` | Service discovery client for finding peer nodes |
| `fabricserver` | Fabric server enabling inter-node HTTP communication |
| `capabilities` | Detects and reports node capabilities (GPU, services, resources) |
| `services` | Service management utilities for Docker containers |
| `tui` | Terminal UI components for interactive displays |
| `ui` | Interactive prompts using the survey library |
| `update` | Auto-update check, download, install, and rollback logic |
| `usage` | Usage tracking and reporting |
| `recommend` | Service recommendation engine based on hardware capabilities |
| `compose` | Docker Compose command helpers |
| `demo` | Demo mode utilities for showcasing functionality |
