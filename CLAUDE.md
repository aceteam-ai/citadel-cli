# CLAUDE.md

> See [aceteam-ai/.github](https://github.com/aceteam-ai/.github/blob/main/CLAUDE.md) for organization-wide conventions.

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Citadel CLI is an on-premise agent for the AceTeam Sovereign Compute Fabric. It connects self-hosted hardware (nodes) to the AceTeam cloud control plane, enabling users to run AI workloads (LLM inference via vLLM, Ollama, llama.cpp) on their own infrastructure while managing them through AceTeam's cloud platform.

**Key Components:**
- **Citadel**: The CLI agent that runs on user hardware
- **Nexus**: The cloud coordination server (nexus.aceteam.ai) that manages the distributed compute network
- **Node**: A physical/virtual machine running the Citadel agent
- **Services**: Dockerized AI inference engines (vLLM, Ollama, llama.cpp, LM Studio)

**User-Facing Terminology Convention:**
When writing user-facing content (CLI help text, README, error messages), use these terms:
| Internal/Technical | User-Facing |
|-------------------|-------------|
| tsnet, Tailscale | "AceTeam Network" |
| WireGuard | "secure mesh network" or omit |
| Headscale | "coordination server" or omit |
| TailscaleIP | "network IP" (keep `tailscale_ip` in JSON for backwards compat) |

This keeps implementation details hidden from users while maintaining technical accuracy in code comments and internal documentation (like this file).

## Build and Development Commands

### Building
```bash
# Build for current platform only (default) - creates binary in ./build/
./build.sh

# Build for all platforms (linux/darwin/windows, amd64/arm64) - for releases
./build.sh --all

# Quick local build (current architecture only, no packaging)
go build -o citadel .
```

### Testing
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run specific test
go test -v ./cmd -run TestReadManifest

# Integration tests (requires Docker)
./tests/integration.sh
```

### Running Locally
```bash
# Most commands require the citadel.yaml manifest in the current directory
# See citadel.yaml for an example configuration

# Check status
go run . status

# View node information
go run . nodes --nexus https://nexus.aceteam.ai

# Test a service
go run . test --service vllm
```

## Development Workflow

### Git Workflow

**IMPORTANT**: Never commit and push directly to `main`. Always create a feature branch and submit changes via a pull request.

```bash
# Create a branch for your work
git checkout -b fix/description-of-change

# Make changes, commit, then push
git push -u origin fix/description-of-change

# Create PR via GitHub CLI
gh pr create --title "fix: description" --body "..."
```

### Future Work and TODOs

When identifying future work or improvements during development, create GitHub issues instead of leaving TODO comments in the code. This ensures:
- Visibility and tracking of all planned work
- Ability to prioritize and assign tasks
- Discussion and context in one place

```bash
# Create an issue for future work
gh issue create --title "feat: description" --body "Context and details..."
```

### Multi-Phase Implementation Plans

When working on features with multiple implementation phases, follow this process:

1. **Create a branch** for the work (if on main)
2. **Create a PR** containing the plan as a markdown document in `docs/`
3. **For each phase**: make a commit, push, and add a PR comment explaining what the commit did
4. **Add tests** at the end or alongside each phase

This ensures:
- Clear documentation of the implementation approach
- Reviewable progress with context for each change
- Easy rollback if needed

### Bug Fix and Issue-Driven Development

When fixing bugs or implementing changes based on user feedback/issues:

1. **Document the problem**: In the PR description, include:
   - **Context**: Why this change is needed (link to issue, user feedback, onboarding problems)
   - **Root Cause Analysis**: What was causing the issue
   - **Solution**: What changes were made and why

2. **Include verification steps**: Always document how to test the changes:
   - Manual testing commands
   - Expected behavior before/after
   - Edge cases to verify

3. **Update CLAUDE.md**: If the fix reveals architectural patterns or important implementation details that future developers should know, add them to this file.

**Example PR structure:**
```markdown
## Context
[Link to issue or description of the problem encountered]

## Root Cause
[Technical explanation of what was wrong]

## Changes
- [List of changes with file paths]

## Testing
[Commands and steps to verify the fix]
```

## Architecture

### Command Structure
Built with Cobra. Main command files are in `cmd/`:
- `init.go`: Provisions fresh servers (installs deps, generates config, connects to network)
- `up.go`: Brings node online, starts services, runs agent loop
- `agent.go`: Long-running job dispatcher that polls Nexus for work
- `work.go`: Unified worker for Redis Streams (private cloud) or Nexus (on-prem), with optional terminal server
- `terminal_server.go`: Standalone WebSocket terminal server for remote access
- `status.go`: Health check dashboard (system vitals, GPU, network, services)
- `login.go`: Interactive AceTeam Network authentication
- `logout.go`: Disconnect from AceTeam Network
- `down.go`: Stops services defined in citadel.yaml
- `run.go`: Ad-hoc service execution without manifest
- `logs.go`: Service log streaming
- `test.go`: Service diagnostic testing

### Core Architecture Patterns

**Manifest-Driven Configuration**: The `citadel.yaml` file defines node identity and services. Generated by `citadel init`, it's the source of truth for node configuration.

**Embedded Network (tsnet)**: Network connectivity uses the embedded tsnet library (Tailscale's Go library) for secure WireGuard mesh networking. This eliminates the need for external Tailscale installation - everything is built into the citadel binary.

**Network Package**: All network operations use `internal/network/` package which wraps tsnet:
- `network.Connect()` - Establish connection to AceTeam Network
- `network.Logout()` - Disconnect and clear state
- `network.IsGlobalConnected()` - Check connection status
- `network.GetGlobalIPv4()` - Get assigned network IP

**No Root Required**: Unlike external Tailscale, tsnet uses userspace networking and doesn't require root/admin privileges for network operations.

**Job Handler Pattern**: The agent uses a pluggable handler system for remote job execution:
```go
type JobHandler interface {
    Execute(ctx JobContext, job *nexus.Job) (output []byte, err error)
}
```
Handlers in `internal/jobs/` implement specific job types (shell commands, model downloads, inference requests). The agent polls Nexus, dispatches to appropriate handler, and reports status back.

**Embedded Services**: Docker Compose files for services are embedded in the binary at `services/compose/*.yml` using Go's `embed` package. The `services.ServiceMap` provides lookup by name (vllm, ollama, llamacpp, lmstudio).

**Docker Compose Management**: Services are managed through `docker compose` commands. The code uses subprocess calls to docker/docker-compose CLI for container lifecycle.

### Key Packages

- **`cmd/`**: Cobra command implementations
- **`internal/network/`**: Embedded tsnet wrapper for AceTeam Network connectivity
- **`internal/nexus/`**: HTTP client for Nexus API, SSH key sync, device authentication
- **`internal/platform/`**: Cross-platform utilities (OS detection, package managers, Docker, GPU)
- **`internal/jobs/`**: Job handler implementations (shell, inference, model download, config)
- **`internal/worker/`**: Unified job runner for Redis Streams and Nexus sources
- **`internal/heartbeat/`**: Status publishing (HTTP to AceTeam API, Redis Pub/Sub + Streams)
- **`internal/status/`**: System metrics collection (CPU, memory, GPU, services)
- **`internal/redis/`**: Redis Streams client for job queue
- **`internal/terminal/`**: WebSocket terminal server with PTY management and token caching
- **`internal/ui/`**: Interactive prompts using survey library
- **`services/`**: Embedded Docker Compose files and service registry

### Network Architecture

Citadel uses embedded tsnet (Tailscale's Go library) to create a secure WireGuard mesh network:
1. Node authenticates using device authorization flow or pre-generated authkey
2. tsnet establishes WireGuard tunnel to Nexus (Headscale coordination server)
3. All traffic between nodes is encrypted end-to-end via WireGuard
4. No external Tailscale installation required - everything is embedded in the binary

**Network State**: Connection state is stored in `~/.citadel-node/network/` and persists across restarts.

**Tailscale/tsnet Interoperability**: Citadel nodes (using embedded tsnet) work alongside regular Tailscale CLI clients on the same Headscale network. Both implement the same Tailscale v2 control protocol:

```
                    Nexus (Headscale)
                   nexus.aceteam.ai
                          │
          ┌───────────────┼───────────────┐
          │               │               │
          ▼               ▼               ▼
    Citadel Node    Tailscale CLI    Other tsnet
    (embedded tsnet) (system-wide)    applications
```

| | Citadel (tsnet) | Tailscale CLI |
|---|---|---|
| Root required | No (userspace networking) | Yes (system VPN) |
| Scope | Per-application | System-wide |
| State directory | `~/.citadel-node/network/` | `/var/lib/tailscale/` |
| Protocol | Tailscale v2 + WireGuard | Tailscale v2 + WireGuard |

Both can coexist on the same machine (separate state directories) and reach each other on the mesh network.

### Provisioning Flow (`citadel init`)

By default, `citadel init` only joins the network (no sudo required). Use `--provision` for full system provisioning.

```bash
citadel init                    # Default: network-only (no sudo)
sudo citadel init --provision   # Full provisioning (requires sudo)
```

**Default Mode (network-only):**
1. Prompts for device authorization or accepts `--authkey`
2. Connects to AceTeam Network using embedded tsnet
3. Services can be configured later via AceTeam web management page

**Full Provisioning Mode (`--provision`):**
1. **Network Choice**: Checks if already connected to AceTeam Network, prompts for device authorization/authkey/skip
2. **Service Selection**: Interactive prompt or `--service` flag to choose inference engine
3. **Node Naming**: Prompts for node name or uses `--node-name` flag
4. **System Provisioning**: Smart installation of dependencies (skips already-installed packages):
   - Installs core dependencies (curl, gpg, ca-certificates) only if missing
   - Installs Docker (using official Docker install script if needed)
   - Configures user permissions for Docker access
   - Installs NVIDIA Container Toolkit (silently skips on non-GPU systems)
   - Configures Docker daemon for NVIDIA runtime

   **Note**: Package manager operations include retry logic for apt lock conflicts.
   **Note**: No external Tailscale installation is needed - network is handled by embedded tsnet.

5. **Config Generation**: Creates `~/citadel-node/` directory with:
   - `citadel.yaml` manifest
   - `services/*.yml` Docker Compose files
6. **Network Connection**: Connects to AceTeam Network using tsnet if authkey provided
7. **Service Startup**: Runs `citadel run` to start configured services

Use `--verbose` flag to see detailed output during provisioning.

### Agent Loop (Nexus HTTP Polling)

The agent (`citadel up` or `citadel agent`) runs continuously:
1. Polls Nexus `/api/v1/jobs/next` every 5 seconds
2. Receives job with `{id, type, payload}` structure
3. Dispatches to registered handler based on job type
4. Executes handler and captures output
5. Reports status back to Nexus with `{status: "SUCCESS"|"FAILURE", output: "..."}`

Job handlers registered in `cmd/agent.go:init()` map job types to handler implementations.

### Worker Mode (Redis Streams) - High-Performance Private Cloud

The worker (`citadel worker`) is the high-performance job queue mode designed for AceTeam's private GPU cloud infrastructure. Written in Go for maximum concurrency and throughput, it routes inference requests to private vLLM/Ollama/llama.cpp clusters.

**Why Go?** The Citadel worker is intentionally written in Go (not Python) for:
- High concurrency via goroutines (thousands of concurrent jobs)
- Low memory overhead per connection
- Fast startup and minimal latency
- Designed for high-throughput GPU cluster routing

**Architecture: Python Worker vs Citadel Worker**
```
                         Redis Streams
                              │
          ┌───────────────────┴───────────────────┐
          │                                       │
          ▼                                       ▼
┌─────────────────────┐               ┌─────────────────────┐
│   Python Worker     │               │   Citadel Worker    │
│   (lightweight)     │               │   (high-perf Go)    │
│                     │               │                     │
│   → OpenAI API      │               │   → Private vLLM    │
│   → Anthropic API   │               │   → Private Ollama  │
│   → Google API      │               │   → GPU clusters    │
└─────────────────────┘               └─────────────────────┘
      Superscalers                     AceTeam Private Cloud
```

- **Python Worker**: Lightweight proxy for external API calls (OpenAI, Anthropic, etc.)
- **Citadel Worker**: High-performance router for AceTeam's private GPU infrastructure

```bash
# Start worker with defaults (connects to redis.aceteam.ai)
citadel work

# Or with local Redis for development
citadel work --redis-url=redis://localhost:6379

# Environment variables can override defaults
export REDIS_URL=redis://localhost:6379
export WORKER_QUEUE=jobs:v1:gpu-general
citadel work
```

**Worker Architecture:**
1. Connects to Redis Streams as a consumer in a consumer group
2. Uses XREADGROUP to claim and process jobs (high-throughput)
3. Routes jobs to private GPU cluster endpoints
4. Streams responses back via Redis Pub/Sub (`stream:v1:{jobId}`)
5. ACKs messages on success, moves to DLQ on repeated failure

**Supported Job Types:**
- `llm_inference` - Routes to private vLLM, Ollama, or llama.cpp clusters

**Job Handler Pattern (Worker):**
```go
type WorkerJobHandler interface {
    Execute(ctx context.Context, client *redis.Client, job *redis.Job) error
    CanHandle(jobType string) bool
}
```

**Worker Features:**
| Feature | Description |
|---------|-------------|
| Job source | Redis Streams |
| Streaming | Redis Pub/Sub |
| Retry handling | Consumer groups + DLQ |
| Scaling | Horizontal via consumer groups |
| Default endpoint | redis.aceteam.ai (AceTeam private cloud) |

### Redis Status Publishing

The worker supports real-time status publishing to Redis for live dashboard updates and reliable status processing.

**Key Packages:**
- **`internal/heartbeat/redis.go`**: Redis status publisher
- **`internal/jobs/config_handler.go`**: Device configuration job handler

**Architecture:**
```
Citadel Node                                Redis
┌─────────────┐    PUBLISH node:status:X   ┌─────────────┐
│   Redis     │ ────────────────────────▶  │  Pub/Sub    │ → Real-time UI
│  Publisher  │                            └─────────────┘
│   (30s)     │    XADD node:status:stream ┌─────────────┐
│             │ ────────────────────────▶  │  Streams    │ → Python Worker
└─────────────┘                            └─────────────┘
```

**Usage:**
```bash
# Status publishing is enabled by default
citadel work

# With device code for config lookup (from device auth flow)
CITADEL_DEVICE_CODE=abc123 citadel work

# Disable status publishing if needed
citadel work --redis-status=false
```

**Redis Keys:**
| Key Pattern | Type | Purpose |
|-------------|------|---------|
| `node:status:{nodeId}` | Pub/Sub | Real-time status updates for UI |
| `node:status:stream` | Stream | Reliable status processing by Python worker |
| `jobs:v1:config` | Stream | Config jobs pushed by Python worker |

**Device Configuration Flow:**
1. User runs `citadel init` with device authorization
2. Citadel publishes status with `deviceCode` to Redis
3. Python worker consumes status, looks up config from onboarding wizard
4. Python worker pushes `APPLY_DEVICE_CONFIG` job
5. Citadel applies config (starts services, updates manifest)

**APPLY_DEVICE_CONFIG Job Handler:**
Handles device configuration from onboarding wizard. Config fields:
- `deviceName`: Node display name
- `services`: Services to run (vllm, ollama, etc.)
- `autoStartServices`: Auto-start services after config
- `sshEnabled`: Enable SSH access
- `customTags`: Tags for node classification
- `healthMonitoringEnabled`, `alertOnOffline`, `alertOnHighTemp`: Monitoring settings

### Terminal Service

The terminal service provides WebSocket-based terminal access to nodes. See [docs/terminal-service.md](docs/terminal-service.md) for full documentation.

**Key Packages:**
- **`internal/terminal/server.go`**: WebSocket server with rate limiting
- **`internal/terminal/session.go`**: PTY session management (creack/pty)
- **`internal/terminal/auth.go`**: Token validation (HTTP and caching validators)
- **`internal/terminal/protocol.go`**: JSON message protocol

**Running the Terminal Server:**
```bash
# Standalone (for testing)
citadel terminal-server --test --port 7860

# Integrated with work command (production)
citadel work --terminal --terminal-port 7860
```

**Token Caching (CachingTokenValidator):**

The terminal server uses a caching validator to avoid API calls per connection:
1. Fetches token hashes from API at startup and hourly
2. Validates tokens locally via SHA-256 hash comparison
3. Refreshes on cache miss before rejecting
4. Exponential backoff (1s → 5min) on API failures

```go
// Create caching validator
auth := terminal.NewCachingTokenValidator(baseURL, orgID, time.Hour)
auth.Start()  // Starts background refresh
defer auth.Stop()

// Validate locally (no API call)
info, err := auth.ValidateToken(token, orgID)
```

**Platform Support:**
- Linux/macOS: Full PTY support via `creack/pty`
- Windows: Not yet supported (requires ConPTY implementation)

## Important Implementation Notes

### Manifest Loading
`citadel.yaml` location discovery (in `cmd/up.go:findAndReadManifest()`):
1. Checks current directory
2. Checks `/etc/citadel/citadel.yaml` (global system config)
3. Falls back to `~/citadel-node/citadel.yaml`

### GPU Detection
Status command detects NVIDIA GPUs using:
1. `nvidia-smi` command output parsing
2. Falls back to checking `/proc/driver/nvidia/gpus/` directory
3. Displays "No GPU detected" if neither method succeeds

### Service Management
Services are started with `docker compose -f <path> -p citadel-<name> up -d`. The `-p` flag ensures consistent naming: `citadel-vllm`, `citadel-ollama`, etc.

### Docker Runtime Requirements
vLLM and llama.cpp require NVIDIA runtime configured in `/etc/docker/daemon.json`:
```json
{
  "default-runtime": "nvidia",
  "runtimes": {
    "nvidia": {
      "path": "nvidia-container-runtime",
      "runtimeArgs": []
    }
  }
}
```
The `init` command configures this automatically.

### Authentication Patterns
Two auth flows supported:
1. **Device Authorization** (RFC 8628): OAuth 2.0 device flow with code display (Claude Code-style)
   - User runs `citadel init` → CLI displays device code → User enters code at aceteam.ai/device
   - Implemented in `internal/nexus/deviceauth.go` and `internal/ui/devicecode.go`
   - Default/recommended flow for interactive use
   - Sends machine hostname to API for device name auto-fill in web UI
2. **Authkey**: Non-interactive, uses pre-generated single-use keys from Nexus admin panel
   - Supported via `--authkey` flag for automation/CI/CD

The device flow polls `/api/fabric/device-auth/token` endpoint until user approves at aceteam.ai/device.

**Configuration:**
- `--auth-service <url>` flag or `CITADEL_AUTH_HOST` env var sets auth service URL (default: https://aceteam.ai)
- `--nexus <url>` flag sets Headscale server URL (default: https://nexus.aceteam.ai)

## Testing Philosophy

Integration tests in `tests/integration.sh` use `docker-compose.test.yml` to spin up a mock Nexus server and test the full agent lifecycle.

Unit tests focus on manifest parsing and utility functions. Most command logic is tested through integration tests since it requires Docker/Tailscale.

## Common Gotchas

**Sudo Requirements**: `citadel init` requires sudo only for full provisioning (Docker, NVIDIA toolkit, system user setup). Use `--network-only` to skip system provisioning and run without sudo. `citadel login` does NOT require sudo (uses embedded tsnet for userspace networking).

**Docker Group Membership**: After `init`, users must log out and back in (or run `exec su -l $USER`) for Docker group membership to take effect.

**Compose File Paths**: Service compose files in citadel.yaml use relative paths from the manifest location, not from the current working directory.

**Version Injection**: The `build.sh` script injects version via linker flags: `-ldflags="-X '${MODULE_PATH}/cmd.Version=${VERSION}'"`. Version is set as global var in `cmd/version.go`.

**Mock Mode**:
- The Nexus client in `internal/nexus/client.go` has a mock mode using `mock_jobs.json` for local testing
- Device auth client has mock server in `internal/nexus/deviceauth_mock.go` for testing without backend
  - Usage: `mock := nexus.StartMockDeviceAuthServer(3); defer mock.Close()`
  - Returns `authorization_pending` for N polls, then returns success

## Cross-Platform Support (Linux, macOS, Windows)

Citadel CLI has full cross-platform support for Linux, macOS (darwin), and Windows. The codebase uses platform abstraction layers in `internal/platform/` to handle OS-specific operations.

### Platform Abstractions

**Core Platform Utilities** (`internal/platform/platform.go`):
- `IsLinux()`, `IsDarwin()`, `IsWindows()` - OS detection
- `IsRoot()` - Privilege checking (works on Linux, macOS, and Windows Administrator)
- `HomeDir(username)` - Cross-platform home directory resolution
- `ConfigDir()` - Returns `/etc/citadel` on Linux, `/usr/local/etc/citadel` on macOS, `C:\ProgramData\Citadel` on Windows

**Package Management** (`internal/platform/packages.go`):
- `GetPackageManager()` - Returns apt (Linux), brew (macOS), or winget (Windows) manager
- `EnsureHomebrew()` - Installs Homebrew if not present on macOS

**User Management** (`internal/platform/users.go`):
- `GetUserManager()` - Returns Linux (useradd/usermod), Darwin (dscl), or Windows (net user) manager
- Handles user and group creation across platforms

**Docker Management** (`internal/platform/docker.go`):
- `GetDockerManager()` - Returns Docker Engine (Linux) or Docker Desktop (macOS/Windows) manager
- Handles installation, startup, and permissions appropriately per platform

**GPU Detection** (`internal/platform/gpu.go`):
- `GetGPUDetector()` - Returns NVIDIA (Linux/Windows) or Metal (macOS) detector
- Linux: Uses `nvidia-smi` and `lspci` for NVIDIA GPU detection
- macOS: Uses `system_profiler SPDisplaysDataType` for Metal-compatible GPU detection
- Windows: Uses `nvidia-smi.exe` (primary) and WMI queries (fallback) for NVIDIA GPU detection

### Platform-Specific Behavior

**Linux**:
- Uses apt package manager for dependencies
- Installs Docker Engine via official script
- Configures NVIDIA Container Toolkit for GPU support
- Uses systemctl for service management
- Creates system users with useradd/usermod

**macOS**:
- Uses Homebrew for package management (auto-installs if missing)
- Installs Docker Desktop via `brew install --cask docker`
- GPU support handled automatically by Docker Desktop (especially on Apple Silicon)
- No NVIDIA Container Toolkit (not applicable)
- Creates users with dscl (Directory Service command line)
- Uses `/usr/local/etc/citadel` for global config instead of `/etc/citadel`

### GPU Support Notes

**Linux**: Full NVIDIA GPU support via NVIDIA Container Toolkit. Compose files use `driver: nvidia` specification.

**macOS**:
- Docker Desktop on Apple Silicon (M1/M2/M3) has built-in GPU support via Metal framework
- Intel Macs do not have GPU acceleration for containers
- The `driver: nvidia` specifications in compose files are Linux-specific and ignored on macOS
- Services will still run on macOS but without explicit GPU device reservations
- Docker Desktop automatically handles GPU access for Metal-compatible workloads

### Known Limitations on macOS

- NVIDIA Container Toolkit steps are skipped (not applicable)
- systemctl commands are not used (Docker Desktop manages the daemon)
- User/group management uses different commands (dscl vs useradd)
- Passwordless sudo configuration only applies to Linux
- GPU device reservations in compose files are Linux-specific

## Windows Support

Citadel CLI has full Windows 10/11 support using Windows-specific platform implementations.

### Windows Platform Abstractions

**WingetPackageManager** (`internal/platform/packages.go`):
- Uses Windows Package Manager (winget) for software installation
- Built-in on Windows 10 1809+ and Windows 11 (no bootstrap required)
- Silently handles already-installed packages
- Package IDs: `Docker.DockerDesktop`, `Tailscale.Tailscale`, etc.

**WindowsDockerManager** (`internal/platform/docker.go`):
- Manages Docker Desktop for Windows with WSL2 backend
- Checks for WSL2 availability before installation
- Waits up to 60 seconds for Docker Desktop to start and be ready
- No group management needed (Docker Desktop uses Windows ACLs)
- Installs via: `winget install Docker.DockerDesktop`
- Starts via: `C:\Program Files\Docker\Docker\Docker Desktop.exe`

**WindowsUserManager** (`internal/platform/users.go`):
- Uses `net user` and `net localgroup` commands for user/group management
- Generates secure random passwords for user creation (required by Windows)
- Sets passwords to never expire for system accounts
- Error code 1378 indicates user already in group (treated as success)

**WindowsGPUDetector** (`internal/platform/gpu.go`):
- Primary: Uses `nvidia-smi.exe` from `C:\Program Files\NVIDIA Corporation\NVSMI\`
- Fallback: Uses PATH to find nvidia-smi
- Final fallback: WMI query via `wmic path win32_VideoController get name`
- Same CSV output format as Linux for GPU info parsing

**Windows Privilege Detection** (`internal/platform/platform_windows.go`):
- Uses Windows API via `golang.org/x/sys/windows` package
- Checks if current process token is member of Administrators group
- Build-tagged file (only compiles on Windows)

### Platform-Specific Behavior

**Windows**:
- Uses winget (Windows Package Manager) for dependencies
- Installs Docker Desktop for Windows (requires WSL2)
- GPU support via WSL2 integration with NVIDIA drivers
- No NVIDIA Container Toolkit (handled by Docker Desktop + WSL2)
- No group management needed (ACL-based permissions)
- Uses `C:\ProgramData\Citadel` for global config
- Uses `%USERPROFILE%\citadel-node` for user config
- Administrator elevation required (no sudo equivalent)

### WSL2 Requirements

Docker Desktop on Windows requires WSL2:
- **Minimum**: Windows 10 version 2004 (May 2020) or Windows 11
- **Installation**: `wsl --install` (requires restart)
- **Detection**: Checks for "WSL 2" in `wsl --status` output
- **Error handling**: Clear error message with installation instructions if WSL2 not found

### GPU Support on Windows

**NVIDIA GPUs**:
- Requires NVIDIA driver 470.76+ on Windows host
- Requires Docker Desktop 3.1.0+ with WSL2 backend
- Docker Desktop automatically handles GPU passthrough to WSL2
- No NVIDIA Container Toolkit needed (Linux-only)
- Services use same compose files with `driver: nvidia` (handled by Docker Desktop)

**Detection**:
1. `nvidia-smi.exe` in standard path: `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
2. `nvidia-smi` in PATH
3. WMI query: `wmic path win32_VideoController get name` (checks for "nvidia")

### Known Limitations on Windows

- WSL2 is required (not available on older Windows versions)
- NVIDIA Container Toolkit steps are skipped (not applicable)
- systemctl commands are not used (Docker Desktop self-manages)
- User/group management uses net user commands instead of useradd
- Passwordless sudo configuration is skipped (Windows uses different privilege model)
- GPU device reservations in compose files rely on Docker Desktop's WSL2 integration

### Build Script Updates

**Windows Binary Packaging** (`build.sh`):
- Detects Windows via `mingw*|msys*|cygwin*` in uname output
- Builds with `.exe` extension: `citadel.exe`
- Packages using `.zip` instead of `.tar.gz`
- Cross-compilation: `GOOS=windows GOARCH=amd64 go build -o citadel.exe`
- Release artifacts: `citadel_VERSION_windows_amd64.zip`
