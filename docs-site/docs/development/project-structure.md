---
sidebar_position: 2
title: Project Structure
---

# Project Structure

An overview of the Citadel CLI repository layout, key files, and internal package responsibilities.

## Top-Level Directories

| Directory | Description |
|-----------|-------------|
| `cmd/` | Cobra command implementations (one file per command: `init.go`, `work.go`, `status.go`, etc.) |
| `internal/` | Private Go packages (see [Internal Packages](#internal-packages) below) |
| `services/` | Embedded Docker Compose files for inference engines (`vllm.yml`, `ollama.yml`, etc.) |
| `e2e/` | End-to-end integration tests |
| `tests/` | Integration test scripts |
| `scripts/` | Build, release, and testing scripts |
| `packaging/` | Platform-specific packaging (macOS app bundle) |
| `homebrew-tap/` | Homebrew formula |
| `.winget/` | Windows Package Manager manifest |
| `docs/` | Technical documentation |
| `docs-site/` | Docusaurus documentation site (this site) |

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
