# CLAUDE.md

> See [aceteam-ai/.github](https://github.com/aceteam-ai/.github/blob/main/CLAUDE.md) for organization-wide conventions.

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## How to keep this file from lying

Three entries here were found stale on 2026-08-05 — the `ConfigDir()` paths, the
compose `-p` scheme (#693), and the manifest-loading algorithm — and each one had
already cost someone real time, because a confidently wrong doc is worse than a
missing one: it stops you looking at the code. The `ConfigDir()` entry nearly
turned #696's fix into a silent no-op.

All three failed the same way: **they restated a fact the code owns.** Restated
facts drift the moment the code changes, and nothing fails when they do.

So, when adding to this file:

- **Name the function that owns the behaviour, and say what it decides** — not the
  values it currently returns. `platform.resolveConfigDir` cannot go stale; a copied
  list of paths can, and did.
- **Exact values are worth writing down only when something else pins them** — a
  test, a const, a compose file. Say what pins them, so a reader can check in one
  step (see the port table in `services/ports.go`, guarded by the collision test).
- **Prefer the consequence over the mechanism.** "A bare `docker compose ps` is
  scoped to the shared project, so filter by container name" stays true across
  refactors; "we pass `-p citadel-<name>`" did not.
- **If you change behaviour this file describes, grep for the old value** — the
  `-p` claim survived #528 because nobody did.

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

## ⛔ CRITICAL: Git Workflow Policy

**NEVER commit directly to `main`, and NEVER work in the shared main checkout.** Both are hard rules with no exceptions.

**Always work in a dedicated git worktree.** This repo's main checkout is shared: a release (`./scripts/release.sh`) or another agent can `git checkout main`, pull, and tag *underneath you* at any time — orphaning your in-progress commit and silently moving your working tree onto `main`. (This happened on 2026-07-28: a concurrent `v2.92.0` release orphaned a commit and switched the tree to `main` mid-task.) A worktree isolates your branch from that churn.

Always:
1. Create an isolated worktree on a feature branch off the latest `main`:
   `git worktree add -b feat/description ../citadel-cli-wt-<name> main`
   (use `fix/`, `docs/`, or `chore/` prefixes as appropriate)
2. `cd` into the worktree and make all commits there — never in the shared main checkout.
3. Push the branch: `git push -u origin <branch-name>`
4. **Open a PR right after pushing** with `gh pr create` (use `--draft` when the test plan still needs manual or deploy-gated steps), then inform the user.

If you are about to run `git push origin main`, STOP. If you are about to commit while in the main checkout (i.e. `git rev-parse --git-common-dir` equals `git rev-parse --git-dir` — you are NOT in a worktree), STOP and move to a worktree first.

When the branch is done, remove the worktree with `git worktree remove <path>` (or leave it until the PR merges).

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

### Releasing

**ALWAYS use `scripts/release.sh` to create releases.** Never manually create tags, GitHub releases, or upload binaries. Manual releases break the auto-updater because they miss the `checksums.txt` asset.

```bash
# Standard minor bump release (most common)
./scripts/release.sh

# Dry run to preview what will happen
./scripts/release.sh --dry-run

# Patch release
./scripts/release.sh patch

# Check state of an in-progress release
./scripts/release.sh --status

# Resume after a failure
./scripts/release.sh --resume

# Clear stale state and start fresh
./scripts/release.sh --clean
```

The release script handles:
1. Version bump (reads latest `v*` tag, increments minor)
2. Running `go test ./...` and `go vet ./...`
3. Cross-platform builds (linux/darwin/windows, amd64/arm64)
4. **Generating `checksums.txt`** (sha256sum of all archives — required by auto-updater)
5. Creating annotated git tag and pushing
6. Creating GitHub Release with all binaries + checksums attached

**Why this matters:** The `citadel update` auto-updater downloads `checksums.txt` to verify binary integrity. If checksums are missing, nodes see `checksum fetch failed with status: 404 Not Found` and can't update.

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

### Windows E2E Test
```bash
# Full E2E test (clean → install → init → verify) on a Windows machine via WinRM
./scripts/windows-e2e-test.sh --host 192.168.2.207 --user aceteam --password 'P@ssword' --authkey tskey-auth-xxx

# Run a single phase
./scripts/windows-e2e-test.sh verify --host 192.168.2.207 --user aceteam --password 'P@ssword'

# Skip clean phase (test on already-dirty machine)
./scripts/windows-e2e-test.sh --skip-clean --host 192.168.2.207 ...

# Install a specific version
./scripts/windows-e2e-test.sh --version v2.3.0 --host 192.168.2.207 ...

# Dry run (show commands without executing)
./scripts/windows-e2e-test.sh --dry-run --host 192.168.2.207 ...
```

Requires `pywinrm` (`pip install pywinrm`) and WinRM enabled on the target machine. See `scripts/windows-e2e-test.sh` header for WinRM setup instructions.

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

**See "⛔ CRITICAL: Git Workflow Policy" above — it is the authority. NEVER push to main, and never commit from the shared main checkout.**

```bash
# Always: an isolated worktree on a branch off the latest main
git fetch origin
git worktree add -b fix/description ../citadel-cli-wt-<name> origin/main
cd ../citadel-cli-wt-<name>

git add <files>
git commit -m "message"
git push -u origin fix/description

# Open the PR right after pushing; --draft while the test plan
# still needs manual or deploy-gated steps.
gh pr create --title "fix: description" --body "..."
```

Even for "small fixes" or "quick changes" — always a worktree, a branch, and a PR.

Already on a feature branch in your own worktree? Keep committing to it; do not
branch again. Branch off `origin/main`, never off a local `main`, which is
frequently stale.

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
- `run.go`: Ad-hoc service execution without manifest; also owns `citadel stop` (despite the name, `stop.go` is where that command lives)
- `logs.go`: Service log streaming
- `test.go`: Service diagnostic testing

Stale-doc note found while fixing citadel#853/#854: there is no `down.go`.
`citadel down` (defined in `up.go`) reverses `citadel up`'s machine-wide TUN
mode -- it restores routing/DNS and never reads `citadel.yaml`. The
manifest-mutating "stop services" command is `citadel stop` (`cmd/stop.go`).

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

**Capability-Based Queue Routing**: Nodes auto-detect hardware (GPUs via `nvidia-smi`, engines via `docker ps`) and generate tags (e.g., `gpu:rtx3090`, `engine:vllm`). Tags map to Redis Streams queues (`jobs:v1:tag:gpu:rtx3090`) via `capabilities.TagQueueName()`. Capabilities can also be declared manually in the `capabilities:` section of `citadel.yaml`, which takes precedence over auto-detection.

**Node Installer**: `install.sh` is a standalone script served at `get.aceteam.ai/citadel` that provisions a fresh Ubuntu machine end-to-end (NVIDIA drivers, Docker, citadel binary, systemd service, vLLM pre-pull). `uninstall.sh` reverses it. The Packer template (`packer/`) bakes the same stack into a qcow2 VM image for Proxmox-based fleet deployment.

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
- **`internal/capabilities/`**: GPU and engine auto-detection, tag normalization, queue routing resolution
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

### Machine-wide network mode (`citadel up`, citadel #643)

`citadel login` connects the citadel **process** (userspace tsnet). `citadel up`
puts the **whole machine** on the mesh via a real kernel TUN, so `ssh`, `curl`,
a browser — anything on the box — reaches `100.64.x.x` directly. Root/admin
only, and strictly opt-in.

**Backends** (`internal/network`, all behind the `Backend` interface):

| Mode | Device | Privilege | Dial/Listen |
|---|---|---|---|
| `userspace` | none (gVisor netstack) | none | tsnet netstack |
| `tun` | real `utun` / `/dev/net/tun` / Wintun | root/admin | plain stdlib |
| `attached` | none — rides a running `citadel up` | none | plain stdlib |

**tsnet cannot do TUN, despite appearances.** `tsnet.Server` has a public
`Tun tun.Device` field, but tsnet builds `wgengine.Config{Tun: ...}` with
**neither `Router` nor `DNS`**, so wgengine substitutes `router.NewFake()` and
a no-op DNS configurator: packets cross a real interface while the OS routing
table is never touched and the resolver never configured. There is no hook to
supply them, so `backend_tun.go` assembles the tailscaled-style stack itself
(`tsd.System` → `netmon` → `tstun.New` → `router.New` →
`dns.NewOSConfigurator` → `wgengine` → `netstack` → `ipnlocal.LocalBackend`).

**Two non-obvious requirements in that stack** — omit either and the node
breaks in a way that is hard to trace:
- `netns.SetEnabled(true)`: binds outbound WireGuard packets to the physical
  interface so they do not re-enter the tunnel. tsnet never needs it (always
  netstack, so no tunnel to loop through).
- `dns.CleanUp` + `router.CleanUp` on **every** `Up`, not just teardown. A
  crash or reboot-without-shutdown strands routes and a rewritten resolver that
  restarting would not otherwise undo. tailscaled does the same.

**One machine is one node.** `citadel up` shares the state dir — and therefore
the node identity — with `citadel login`. `SelectBackend` checks for a live
`citadel up` FIRST, so `citadel work` on such a host **attaches** rather than
starting a second WireGuard endpoint on the same node key. Callers need no
changes: they already go through `network.Dial` / `network.Listen` /
`network.GetGlobalPeers`.

Selection deliberately does NOT refuse when another *userspace* citadel is
connected — several processes running tsnet against one state dir (background
`citadel work` + ad-hoc `citadel status`) is long-standing behavior. Only the
new userspace/TUN collision is blocked, in `ConnectMachineWide`.

**Windows needs citadel's own Wintun identity.** `tstun`'s init pins the tunnel
type to `"Tailscale"` AND a static adapter GUID, and Wintun keys on the GUID —
so citadel collided with an installed Tailscale (`0x800700B7`, "file already
exists") regardless of the interface name. `internal/network/tun_windows.go`
overrides both (its init runs after tstun's). The GUID is fixed so restarts
re-attach instead of leaking adapters. Verified on DESKTOP-6UKHJAN with
Tailscale running and undisturbed — but note that test (`--check`) installs no
routes, so it proves only that the **adapter** no longer collides. Both
products want to route `100.64.0.0/10`; route-level coexistence is untested.

**`citadel up --check`** creates the interface and immediately removes it
without starting the engine, installing routes, or touching DNS. It is the safe
way to answer "will machine-wide mode work here?" — including on a box already
running other VPN software — and the right thing to run under a remote exec
that might be killed, since `citadel up` itself runs in the foreground until
interrupted and a SIGKILL skips teardown.

**Two imports that are load-bearing and invisible to the compiler.**
`internal/network/backend_tun.go` blank-imports
`tailscale.com/wgengine/router/osrouter` — `router.New` dispatches through a
feature hook that ONLY that import populates, so without it machine-wide mode
fails at runtime on EVERY platform with `unsupported OS "..."`. Do not
"simplify" it to tailscaled's `feature/condregister`: that umbrella links the
AWS SSM client and 76 aws/smithy packages into citadel. Separately, the Windows
init calls `com.StartRuntime` because osrouter's `setPrivateNetwork` assumes
COM is already initialized process-wide (tailscaled does it in its own init);
without it the adapter is silently left in the Public firewall profile.

Design doc: [docs/machine-wide-tun.md](docs/machine-wide-tun.md).

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
# Start worker after 'citadel init' (recommended - uses API mode)
citadel work

# For development/debugging with direct Redis (hidden flag)
citadel work --debug-redis-url=redis://localhost:6379
```

**Worker Architecture:**
1. After `citadel init`, uses the secure Redis API proxy (API mode)
2. Fetches worker config (queue, org) from AceTeam API
3. Consumes jobs via XREADGROUP through the HTTP proxy
4. Routes jobs to private GPU cluster endpoints
5. Streams responses back via Pub/Sub (`stream:v1:{jobId}`)
6. ACKs messages on success, moves to DLQ on repeated failure

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

### Local MCP tools (`citadel mcp`, aceteam #8249 v1)

`citadel mcp` (`cmd/mcp.go`) is primarily a stdio-to-HTTP bridge: it proxies JSON-RPC to the AceTeam backend's hosted MCP tool set (fabric/node/agent tools, all REMOTE — a `node_id` argument picks the target). On top of that, `cmd/mcp_local.go` registers a second tool set, all `local_*` prefixed, that runs entirely on THIS node with no backend round-trip and no API key required: `local_module_stop/start/restart` (reuses `runModuleControl`, the #846 primitive `citadel module stop|start|restart` drives — not a second implementation), `local_list_models`/`local_chat` (reuses `status.DiscoverLocalEngines` + the exported `gateway.ResolveChatModel`, the same model→port resolver the gateway's `/v1/chat/completions` route uses), and `local_read_file`/`local_list_files` (reuses the `FILE_READ`/`FILE_LIST` job handlers' own `jobs.ValidateReadPath` sandbox, read-only). `mcpBridge.run()`'s JSON-RPC loop intercepts `tools/list` (merges local + backend) and `tools/call` (dispatches local names directly, falls through to backend forwarding otherwise) to wire this in.

**The local module-control tools cannot call the CLI helper directly — it writes to the same stdout the JSON-RPC transport owns.** `runModuleControl` (and, worse, the `docker compose up|down` subprocess it shells out to via `startService`/`stopServiceByCompose`, both of which hardwire `cmd.Stdout = os.Stdout`) prints human-readable progress straight to `os.Stdout`. Under `citadel mcp`, `os.Stdout` IS the JSON-RPC transport, so that output would corrupt every response for the rest of the session. `captureStdout` (`cmd/mcp_local.go`) swaps `os.Stdout` for a pipe for the duration of the call, with a background goroutine draining it the whole time so it can't deadlock regardless of output volume (a build-based service's first-start build log can be megabytes — see the Bonsai service notes above). Any new local tool that shells out or otherwise touches `os.Stdout` needs the same treatment; a writer parameter threaded through the Go call chain alone would NOT be enough, because the subprocess's own stdout is hardwired, not injected.

**"Never more than one redirection in flight" is now an enforced invariant, not an implied one (citadel #858).** The MCP stdio loop is still single-threaded and synchronous, but `callLocalToolWithTimeout` (`cmd/mcp.go`) gives every local tool call a real deadline (`localToolCallTimeout`, 5min): on timeout it returns immediately and the loop moves on, while the abandoned goroutine keeps running INSIDE `captureStdout`, still holding `os.Stdout` redirected, for however long the underlying compose call takes to actually finish. That makes two concurrent `captureStdout` calls a real possibility, not just a theoretical one. `stdoutCaptureInFlight` (an atomic guard, deliberately not a mutex — blocking would reintroduce the wedge #858 fixes) makes `captureStdout` refuse to nest: a second call while one is still in flight fails fast with a clear error instead of racing the shared `os.Stdout` variable. Separately, `mcpBridge` writes every JSON-RPC response through a `stdout` field captured ONCE at construction (`runMCP`, before any tool call can run) rather than reading the live `os.Stdout` variable at write time — otherwise a timeout response written while an orphaned call still has `os.Stdout` redirected would silently land in that orphan's pipe instead of reaching the client. Read both together before assuming either alone makes timeout handling safe.

Deliberately deferred to a follow-on (documented in the PR, not silently dropped): model deploy/evict and `run --exclusive`. Those need a VRAM-reservation/eviction primitive (aceteam #8248 part 2, citadel #832/#851) not merged as of v1.

### Terminal Service

The terminal service provides WebSocket-based terminal access to nodes. See [docs/terminal-service.md](docs/terminal-service.md) for full documentation.

Connections are tmux-backed by default (`internal/terminal/tmux.go:sessionCommand`
decides this per connection; `sessionDisabled` owns the disable-sentinel check).
A power user running their own tmux (console, ssh elsewhere, `tmux a`) can avoid
nesting by setting `CITADEL_TERMINAL_SESSION` to `none`/`off`/`disabled`/`false`/`0`
on the node (citadel #780) — this is node-wide. The CLI (`citadel ssh`/`citadel
connect`) is unaffected either way: it always defaults to a bare shell and only
opts into tmux persistence with `--tmux` (citadel #759).

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
5. `lookupCached` (citadel #792) also accepts a `previous_hash` still inside its
   `previous_hash_expires_at` window, so a client that fetched the new token
   just before rotation and a node that hasn't re-polled yet still agree. Both
   fields are optional on `TokenHashEntry` and inert today — the platform does
   not yet send them, so behavior is unchanged until it does.
6. `TokenHashEntry.UnmarshalJSON` (citadel #815) decodes `previous_hash_expires_at`
   leniently: a malformed value (empty string, wrong type, non-RFC3339) degrades
   that one entry to "no grace window" rather than erroring. This matters beyond
   the single entry — `TokensResponse.Tokens` decodes as one slice, so a plain
   `time.Time` field failing there would otherwise reject the ENTIRE token list,
   and on a cold start (`Start()` with no prior cache) that means every terminal
   connection is denied until the platform fixes the payload. The current-hash
   fields (`hash`/`user_id`/`org_id`/`expires_at`) are still parsed strictly.

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

### Mesh Model Discovery & Remote Chat (citadel #576, Phase 2)

`citadel mesh` discovers the models served by OTHER citadel nodes on the mesh and
routes chat to them node-to-node. Node->node mesh traffic is DIRECT (the Railway
SOCKS relay caveat only affects backend->node), so a node can probe peers itself.

**Discovery route (`internal/mesh`):** each node already exposes its full
heartbeat (`services[].models`, `services[].port`) at `GET /status` on its mesh
VPN listener. `mesh.Discover` enumerates online peers (`network.GetGlobalPeers`),
probes each peer's `/status` over the mesh (dialing via `network.Dial`), and
aggregates a fabric-wide `model -> (node, engine, port)` view. Unreachable peers
are skipped and recorded with their error (a probe never fails the whole call).

**Standalone by design:** `internal/mesh` does NOT import `internal/network` or
`internal/status`. Callers inject a `Dialer` and a `PeerLister`, and the payload
subset is decoded into a local struct — this keeps the aggregation pure and
unit-testable without a live mesh (`discover()` takes a mock `statusFetchFunc`;
the HTTP + chat paths are tested via `httptest` + an injected dialer). Phase 1's
local chat REPL (#575) can import the same `Inventory`/`Client` to add a
remote/peer selection mode. The CLI is deliberately `citadel mesh` (not
`citadel chat`) to avoid colliding with #575.

**Routing:** all engines (vLLM, llama.cpp, bonsai, Ollama's OpenAI shim) expose
`/v1/chat/completions`, so `mesh.Client.ChatCompletion(ip, port, body)` routes on
`(ip, port)` alone; the `engine` field is informational. The function is generic
and reusable (by #575's REPL too); it lights up the moment a node exposes a
mesh-reachable chat endpoint.

**Discovery reachability caveat:** a plain `citadel work` (default
`--status-port 0`) does NOT serve `/status` on the mesh — only `--gateway` (or the
provisioned gateway, which forces `:8080` + a VPN listener) does. Discovery
therefore sees only gateway-enabled peers; the probe port is configurable via
`--port` (`mesh.Options.Port`, default `services.GatewayPort` = 8080).

**Chat reachability (was the #581 gap, now IMPLEMENTED):** on **embedded-tsnet
nodes the engine's host port is not reachable over the mesh** — verified by
self-dialing this node's own mesh IP:8210 via `network.Dial` while bonsai served
fine on `localhost:8210` → `connection refused`: embedded tsnet's userspace
netstack does not forward inbound mesh traffic to a host-bound port lacking a
`srv.Listen()` (only ports citadel explicitly `ListenVPN`s — status 8080, gateway
8443, terminal, vnc, modules — answer over the mesh). The engine compose files
bind `0.0.0.0` and carry a "peers reach this engine directly over the mesh"
comment, but that reflects a full-Tailscale/kernel-TUN node, not embedded tsnet.

**Fix (#581, node-side complement of aceteam #6236):** the gateway now routes
`/v1/chat/completions` (plus `/v1/completions` and `/v1/models`) by model. Both
`citadel work --gateway` and `citadel serve` register a dynamic chat handler
(`internal/gateway/chat_route.go`) that reads the request body's `model`,
resolves it to the LOCAL serving engine's citadel-owned host port via
`status.DiscoverLocalEngines` (behind a short TTL cache in `cmd/gateway_chat.go`),
and reverse-proxies to `http://127.0.0.1:<port>/v1/chat/completions` — streaming
SSE included (`ReverseProxy.FlushInterval=-1`). Model matching mirrors
`mesh.FindModel` (exact case-insensitive → substring); an unknown model returns a
404 OpenAI-shaped `model_not_found`. Because these routes sit on the same gateway
mux, they ride the LAN listener AND the tsnet VPN listener, so `mesh.Client.
ChatCompletion(ip, gatewayPort, body)` reaches them over the mesh. `mesh chat`
now works end-to-end (target the node's gateway port, not the engine port).

Auth posture: `categoryForPath` returns `""` for `/v1/chat/completions`, so it is
always-allowed — same as `/v1/embeddings`. TLS + mesh membership are the only
gate; the gateway does NOT currently wire `SetMetering`, so chat is neither
metered nor authenticated per-request. A single chat response is bounded by the
gateway `http.Server` `WriteTimeout` (120s), a pre-existing server-wide cap.

```bash
citadel mesh models                 # list model -> node/engine/addr across the mesh
citadel mesh models --json          # includes unreachable nodes + their errors
citadel mesh chat --model M "hi"    # one-shot chat to a uniquely-named model
citadel mesh chat --node N "hi"     # pick a node (hostname or mesh IP) explicitly
```

### OpenAI tool calling through `llm_inference` (citadel #603, aceteam #6555)

`executeChatCompletionsAt` (`internal/worker/llm_inference.go` — vllm/
llamacpp/bonsai/unlimited-ocr) forwards `tools`/`tool_choice` on the request
and returns `tool_calls` on the response. **`executeOllamaChat` (ollama's
native `/api/chat` path) still silently drops tools** — a known, undocumented-
to-the-caller gap, not fixed here. `jobs.LLMInferencePayload.Tools`/
`ToolChoice` and `jobs.ChatMessage.ToolCalls`/`ToolCallID`/`Name` carry raw
JSON/strings rather than a typed Go struct, so an engine-specific
`function.parameters` JSON Schema is never lossily re-typed. Absent on a
text-only request, pinned by
`TestLLMInferenceHandler_ToolsRequestByteIdenticalWithoutTools` (asserts key
ABSENCE, not an empty value).

**Streaming tool-call deltas are NOT emitted per-chunk** — `StreamWriter.
WriteChunk(content string, index int)` has no field for structured data, and
widening it would ripple through both stream-writer implementations,
`internal/redis`/`internal/redisapi`'s `PublishChunk`, and every existing
caller (the #717 `SwapRecord.Pulled` "guessed field is worse than no field"
tradeoff). Verified against the real consumer before choosing this scope:
`aceteam/python-backend/agents/fabric_client.py`'s `_event_to_chunk` already
reads `tool_calls` off the terminal **`end` event's `data.result`** — exactly
what `bufferedChatCompletions`/`streamChatCompletions` return as
`JobResult.Output` (published via `stream.WriteEnd`) — so this is not a gap
for that consumer today. `streamChatCompletions`' `toolCallAccumulator` merges
the engine's own incremental deltas into one final array before returning it.

## Important Implementation Notes

### Manifest Loading
`cmd/manifest.go:findAndReadManifest()` is the authority. It is an **indirection**,
not a search path: it reads `<ConfigDir>/config.yaml` for a `node_config_dir` key
and loads `citadel.yaml` from there, falling back to `~/citadel-node/citadel.yaml`
(and self-healing the config) when the key is missing. It does NOT consult the
current directory, and it does not look for `/etc/citadel/citadel.yaml` directly —
`/etc/citadel` only enters via `ConfigDir()` (see below), and only for root.

Read the function for the exact order; do not re-copy it here. A restated
algorithm is what went stale before.

### Two ActualState reporters, two different module sets (#733)

A node ships `ActualState` to the control plane from TWO independent paths.
Both are wired in `cmd/work.go`, and (since #739) both now share the SAME
enumeration authority — the **lockfile** (`catalog.LoadLockfile`) — but still
differ in how they fill in per-module health:

- `nodestate.BuildActualState` decides the report the 60s `nodestate.Emitter`
  sends. It enumerates `lf.Modules` directly and determines health via a live
  `ModuleInspector.Inspect` per entry. No lockfile means it returns early with
  an EMPTY module list, and the control plane's ingest writes zero module rows.
- `liveModuleOps.ListInstalled` (`cmd/module_ops.go`) decides the report the
  reconcile loop sends AND the set the reconcile engine (`internal/reconcile`)
  is authoritative — i.e. eligible to UNINSTALL — over. It also enumerates
  `lf.Modules`, then consults the manifest `services:` list ONLY to read the
  durable stopped marker (`Service.DesiredStatus`) for health, falling back to
  a live container check when the manifest doesn't have the entry.

**The consequence that bites:** a node running services with no lockfile entries
(started by `citadel run`, provisioned by `citadel init`, or any embedded engine
brought up outside the module system) reports fine and still shows up as "has
not reported any modules" upstream, so that message says nothing about whether
the node is healthy or reporting. Check which path you expect to carry the data
before concluding a node is silent.

**Why this scoping matters more than it looks (#739, fixed):**
`liveModuleOps.ListInstalled` used to enumerate the manifest `services:` list
instead of the lockfile — every embedded/catalog service in `citadel.yaml`
counted as "installed" to the reconcile engine, whether or not the module
system ever installed it. `internal/reconcile.Reconcile` treats anything in
`actual` but not in the control plane's `desired` set as drift to UNINSTALL, and
that check is NOT the empty-desired-set full-wipe guard in
`internal/reconcile/loop.go` — that guard only fires when desired is entirely
empty. The FIRST non-empty desired state (even a single module) would have torn
down every OTHER manifest service alongside it: install/uninstall + drop from
the manifest + delete lockfile entries + remove materialized compose/env files.
Latent (nothing wrote durable desired rows at the time), but real the moment
something does. `TestOneDesiredModuleDoesNotWipeManifest`
(`cmd/module_ops_test.go`) pins the fixed contract directly against
`reconcile.Reconcile`.

**Every real install path must call `catalog.UpsertLockEntry`, or it's invisible
to converge.** The "present in lockfile ⇒ module-installed" direction only holds
if every code path that genuinely installs a module also records it: `citadel
module install <source>` (`cmd/module.go`, external git sources), `citadel
module update` (`cmd/module_update.go`), the reconcile engine's own `Install`
(`cmd/module_ops.go`), and `citadel catalog install <name>` / `citadel module
install <catalog-name>` (both funnel through `runCatalogInstall`, `cmd/catalog.go`
— this one was MISSING the lockfile write until the #739 follow-up that added
`recordCatalogModuleLock`; pre-#739 that gap was harmless because the manifest-wide
scan papered over it, but post-#739 it meant a catalog-CLI-installed module was
invisible to `ListInstalled`, so a later MODULE_SET retargeting it by name would
uninstall+reinstall instead of converging as a no-op — `TestCatalogInstalledModuleNoLongerReinstallsOnRetarget`
pins the fix). If you add a new install entry point, it needs this too, or this
same gap reopens under a new name.

**Residual, accepted gap:** even with every path wired, the reverse direction
isn't airtight — every write above is best-effort (a failed lockfile write is
logged, not fatal, so the install/compose-up still proceeds). A false negative
here means `ListInstalled` under-reports a genuinely module-installed service,
which the engine resolves with a harmless idempotent re-`Install`, not an
uninstall of something else — the direction of error this scoping fix cares
about. Hardening those writes to be non-best-effort is a documented,
low-priority follow-up, not part of #739's fix.

**MODULE_SET `absent` on a lockfile-less service is a silent no-op.** In
`internal/worker/module_set.go`'s `scopeToSingleModule`, a `desired_status:
absent` (remove) targeting a service with NO lockfile entry — an
operator-run/embedded service, or any install path that hasn't recorded one —
now converges to an empty desired + empty actual = an empty plan, rather than
the old compose-down/deregister/cleanup. This is the safe direction (nothing
gets destroyed that reconcile doesn't recognize as its own), but it means a
remote "remove" request for such a service does nothing and reports success.
`TestModuleSetAbsentNoOpForUnrecordedService` (`internal/worker/module_set_test.go`)
pins this.

Reporting is on the observability path, never the apply path. The full-wipe
guard in `internal/reconcile/loop.go` refuses the destructive converge and still
reports, deliberately leaving `AppliedRevision` empty because nothing was
applied. `TestRefuseFullWipeReportOmitsAppliedRevision` pins that.

### GPU Detection
Status command detects NVIDIA GPUs using:
1. `nvidia-smi` command output parsing
2. Falls back to checking `/proc/driver/nvidia/gpus/` directory
3. Displays "No GPU detected" if neither method succeeds

### Service Management
Services are started with `docker compose -f <path> up -d` — **no `-p`**. Compose
therefore derives the project name from the compose file's directory basename
(`services`), so every service shares ONE project rather than getting its own.

That is deliberate (#528): the per-service `-p citadel-<name>` form is legacy, and
`removeLegacyCitadelProject` (`cmd/service.go`) exists solely to clean up
containers left behind by it — a pinned `container_name` owned by another compose
project makes `up` fail, and `--force-recreate` does not clear a cross-project
name conflict.

**The consequence that bites:** a bare `docker compose ps` in a service's
directory is scoped to the shared project, not to that service, so it lists
sibling services too. Anything reasoning about "is THIS service up" must filter by
container name rather than trusting project scoping — see #692, which this stale
doc helped hide.

### WhatsApp bridge deploys must pull (#718)

The bridge compose pins a FLOATING tag, so `docker compose up -d` alone can never
upgrade it: with that tag already present locally, compose sees an unchanged
config and an unchanged image ID and does nothing, while `whatsapp.Provision`
went on to report `already_linked` success on the stale image. `startBridgeStack`
(`cmd/whatsapp.go`) owns the rule: pull the `bridge` service, then up. The pull is
best-effort (a node without `docker login` still serves its cached image) but
never silent: `whatsapp.ProvisionDeps.BridgeImageID` samples the RUNNING
CONTAINER's image before and after the deploy, and `ProvisionResult` carries
`Upgraded` + both IDs + `ImagePullError`. The `status` string deliberately still
has only two values (`provisioned` / `already_linked`): the aceteam backend
branches on `status == "already_linked"` by equality, so upgrade information is
additive, never a new status. `TestStartBridgeStackPullsBeforeUp` pins the argv.

### Canonical per-engine cache paths (citadel #682 P0/P1, #906, model-cache ownership design)

`services.EngineCacheDirs` (`services/caches.go`) is the single source of
truth for where EVERY embedded `services.ServiceMap` engine's weights live on
disk — one map from engine name to its canonical `~/citadel-cache/<dir>`
subdirectory and on-disk layout family (HF hub-cache blob layout, raw GGUF
directory, or an engine-native store), asserted against the embedded compose
files' actual volume mounts by `TestEngineCacheDirsMatchComposeMounts`
(`services/caches_test.go`) — string-matched, not hand-copied, so (unlike the
prior P0-only state this note used to describe) the table cannot silently
drift from what the compose files actually mount. See
`docs/design-cache-ownership.md` for the full ownership/GC design; this note
covers P0+P1 only (P2 durable index / P3 reporting / P5 GC are separate,
not-yet-built follow-ons).

**HF-hub-layout engines** (vllm, sglang, diffusers, extraction, transcribe,
unlimited-ocr, kokoro): `internal/jobs.canonicalHFCacheDir()` is the authority
for where a host-side HF pull must write so it agrees with what the engine
containers mount — sourced from `services.HFHubCacheDirName`, not a
hardcoded literal. `hfCacheBaseDir()` (the disk-preflight/no-op-detection/
`MODEL_CACHE_EVICT` resolver) and `hfDownloadEnv()` (the actual pull
subprocess's env) both resolve through it — that agreement is the P0 fix;
before #682 the pull subprocess had no `HF_HOME` set at all and silently
wrote to the CLI's own host default instead, which no container could see.

**GGUF engines are NOT routed through the HF-hub path (citadel #906, the P1
fix).** Before #906, llamacpp was dispatched identically to vllm
(`pullHuggingFace`), writing the HF hub-cache blob layout into
`~/citadel-cache/huggingface` — but `services/compose/llamacpp.yml` mounts
`~/citadel-cache/llamacpp:/models` expecting flat, raw GGUF files, a
directory and layout that path never touched. `internal/jobs.
pullLlamaCppGGUF`/`llamaCppCacheDir()` (sourced from
`services.LlamaCppCacheDirName`) fix this: a `--local-dir` download into the
correct directory, mirroring `pullBonsai`'s existing single-file idiom but
generalized to an arbitrary bring-your-own-GGUF repo, with its own disk
preflight (`runGGUFDiskPreflight`) pointed at the same directory. Its no-op
detection (`dirTotalSize` before/after) deliberately gates on the AFTER state
being non-empty, not on the delta being positive: `hf download` is idempotent
and MODEL_CACHE_PULL is dispatched on every deploy, so a redeploy of an
already-cached repo legitimately re-fetches nothing (delta == 0) without that
being a #566-style CLI no-op.
`runGGUFDiskPreflight` nets against `alreadyCachedGGUFBytes`, NOT
`hfCacheModelSizeFn` (#840's HF-hub netting) — the raw, flat directory has no
`models--org--repo` naming to trust, but a repo-relative PATH match (the repo
tree entry's own path, checked for existence under `llamaCppCacheDir()`) is
still exact provenance for THIS pull's own files (`--local-dir` preserves the
repo's file layout), so no durable cache index (#682 P2) is needed to net
correctly here. Skipping this netting entirely was tried and reverted during
review: it reintroduced, for llamacpp, the exact "redeploy of an
already-cached model fails closed on the full repo size" regression #840's
review caught and fixed for the HF-hub path. `MODEL_CACHE_EVICT` for llamacpp
(`evictLlamaCppGGUF`, `internal/jobs/model_cache_evict.go`) is similarly
routed to the raw directory instead of falling through to
`evictHuggingFace`'s HF hub-cache resolver (which always failed with "not
found" for llamacpp — safe, but didn't free any space); for the same
provenance reason it only supports removing an exact, existing cached
filename, not resolving a repo id to "whichever files belong to it".

**Known limitation, not yet fixed:** an unfiltered `MODEL_CACHE_PULL` for a
multi-quant GGUF repo (the common TheBloke-style shape: 10+ sibling
quantizations, several GB each) downloads EVERY sibling into the shared
`/models` mount, same as an unfiltered `pullHuggingFace` would for a
multi-checkpoint HF repo pre-#828. The disk preflight bounds the worst case
(refuses rather than fills the disk) but does not select a subset the way
`deriveDiffusersAllowPatterns` does for diffusers — deliberately, since no
GGUF-shape heuristic exists yet. The payload's `allow_patterns` (#828) is the
escape hatch for a caller that knows the desired quantization (e.g.
`["*Q4_K_M.gguf"]`); the aceteam backend does not send it yet.

`bonsaiCacheDir()` is unchanged in behavior but now also sources its
subdirectory name from the table (`services.BonsaiCacheDirName`) rather than
a second hardcoded `"bonsai"` literal, so all three engine families
(HF-hub, GGUF, bonsai's single-file GGUF) read their directory name from ONE
place.

### Bonsai service (PrismML Bonsai-27B, 1-bit)

Bonsai-27B is PrismML's 1-bit quantized Qwen3.6-27B. The `bonsai` service serves
`Bonsai-27B-Q1_0.gguf` (~3.8GB) via an OpenAI-compatible `llama-server`, fitting
an RTX 3090 (24GB) easily (~5-12GB VRAM by context length).

**Why it needs its own service (not `llamacpp`):** stock llama.cpp lacks the
`Q1_0_g128` hybrid-attention kernels the GGUF requires. Bonsai builds the
**PrismML fork** (`https://github.com/PrismML-Eng/llama.cpp`, default branch
`prism`) with `-DGGML_CUDA=ON`.

**Files:**
- `services/compose/bonsai.yml` — embedded compose (in `services.ServiceMap`).
  Serves on container `:8080`, host port `8210` (`services/ports.go`
  `BonsaiHostPort` / `CITADEL_BONSAI_HOST_PORT`). Mounts `~/citadel-cache/bonsai`
  at `/models` and serves `/models/${BONSAI_MODEL:-Bonsai-27B-Q1_0.gguf}` with
  `-ngl 99`.
- `services/compose/bonsai/Dockerfile` — self-contained; clones the fork and
  builds `llama-server`. Base `nvidia/cuda:12.4.0-devel-ubuntu22.04`.

**First build-based embedded service (important):** every other `ServiceMap`
entry uses a prebuilt `image:`; bonsai uses `build: {context: ./bonsai}`. On-node
materialization historically wrote only `<name>.yml`, so a `build:` context would
be missing its Dockerfile and `docker compose build` would fail. `services.
ServiceAuxFiles` + `services.WriteAuxFiles()` fix this: both materialization
sites (`cmd.ensureComposeFile`, `ServiceHandler.ensureEmbeddedComposeFile`)
now also write `services/bonsai/Dockerfile`. Any future build-based embedded
service should register its context files the same way.

**Model pull:** `MODEL_CACHE_PULL` with `engine: "bonsai"` (see
`internal/jobs/model_cache_pull.go`) runs
`huggingface-cli download prism-ml/Bonsai-27B-gguf Bonsai-27B-Q1_0.gguf
--local-dir ~/citadel-cache/bonsai` — the SINGLE GGUF file, not the whole repo
(which also carries a ~53GB F16 and a drafter). The `--local-dir` is a
deliberate deviation from a bare `huggingface-cli download` so the file lands at
a predictable path the compose mount serves (the HF hub cache path carries an
unpredictable snapshot hash). `bonsaiCacheDir()` and the compose mount must stay
in sync (guarded by a test).

**Worker inference routing:** the `llm_inference` handler routes
`backend: "bonsai"` to the bonsai host port via `executeLlamaCppAt` (the bonsai
llama-server exposes the identical llama.cpp-server API). See
`internal/worker/llm_inference.go` — the Redis-interface handler that used to
live at `internal/jobs/llm_inference.go` was ported to this native
`worker.JobHandler` by issue #590 (that old path no longer exists; see the
Worker Mode section above). Direct mesh HTTP to `:8210` also works.

**First start builds inline (~7min on Ampere):** because bonsai is build-based,
the first `SERVICE_START` (or `citadel run --service bonsai`) runs `docker
compose up -d`, which builds the image before returning. This is safe on the
deploy path: `SERVICE_START` is in the worker watchdog's *unbounded* tier (see
Consume-Loop Watchdog above) and the compose-up exec carries no context deadline,
so the inline build is not killed. Subsequent starts reuse the cached image.

**Known limitations:**
- **Ampere-only image (compute 8.6).** The Dockerfile defaults
  `CUDA_ARCHITECTURES=86` (RTX 3090, citadel's target node) — this also cuts
  build time. On a different GPU (4090=89, A100=80, ...) the built image starts
  but fails at inference with "no kernel image available for execution on the
  device". Override via the build ARG (`--build-arg CUDA_ARCHITECTURES=89` or a
  `;`-list) for other hardware.
- `MODEL_CACHE_EVICT` (`internal/jobs/model_cache_evict.go`) does not yet handle
  engine `bonsai` (only vllm/llamacpp/ollama), so a bonsai GGUF must be removed
  manually from `~/citadel-cache/bonsai`. Low-priority follow-up.

**VRAM tuning (default-on, citadel #567):** the compose now bounds context and
quantizes the KV cache: `--ctx-size ${BONSAI_CTX:-8192} --flash-attn on
--cache-type-k q4_0 --cache-type-v q4_0`. Without `--ctx-size`, llama-server
allocates Bonsai's full 262K training context, whose KV cache alone is ~17GB — the
3.9GB model then pins ~21GB VRAM. Bounding context to 8192 (32x smaller) is what
does ~all the VRAM work (~5-6GB total); the 4-bit KV quant trims a little more.
Quantized V-cache requires flash attention, so `--flash-attn on` is mandatory here.
Flag names verified against the PrismML fork's `common/arg.cpp` (`-fa` takes an
`[on|off|auto]` value). Override the context via the `BONSAI_CTX` env var.

**Live inference is a documented human step:** node 1084's GPU is VRAM-contended
(vLLM holds ~21GB); do NOT stop vLLM to validate. Bonsai fits alongside once VRAM
is free.

**AceTeam-side follow-up (different repo, NOT done here):** to make Bonsai
deployable from `/fabric/models` (`fabric_catalog_models`), add a catalog entry
in `aceteam` `data/model_catalog.json` (and the `fabric_catalog_models` source)
shaped like:
```json
{
  "id": "bonsai-27b",
  "name": "Bonsai-27B (1-bit)",
  "engine": "bonsai",
  "source": "prism-ml/Bonsai-27B-gguf",
  "gguf_file": "Bonsai-27B-Q1_0.gguf",
  "vram_gb": { "min": 5, "recommended": 8, "max_context": 12 },
  "context_length": 262144,
  "tags": ["gguf", "1-bit", "qwen3.6", "cuda", "llamacpp-fork"]
}
```
The `engine` must be `bonsai` so the node routes `MODEL_CACHE_PULL` to the
single-file GGUF pull and `SERVICE_START` to `services/compose/bonsai.yml`.

### Per-request Energy Receipt (footprint energy, aceteam#6635)

The footprint sampler can record a per-interval node energy estimate so the
sovereignty pitch has an auditable watt-hours signal. It is **opt-in, default
OFF**: when off, the sampler runs exactly as before (no power probe, no energy
columns), so a node with energy disabled produces byte-identical footprint CSVs.

**Columns (appended AFTER the core schema, only when enabled):** `power_w`,
`energy_wh`, `power_source`. Written on the node-level (`_node`) row only;
per-service/per-request attribution is a deliberate next increment.

**Estimation waterfall (`internal/footprint/energy.go`, pure + unit-tested).**
Tiers are never combined, so a `measured` label always means a real sensor:
1. GPU board power (`nvidia-smi power.draw`, summed) -> `measured`
2. GPU `util% x TDP` (TDP = `nvidia-smi power.limit`, or `CITADEL_GPU_TDP_WATTS`) -> `estimated`
3. CPU `util% x CPU_TDP` (`CITADEL_CPU_TDP_WATTS`, default 65W) -> `estimated`
4. nothing usable -> blank

`energy_wh = power_w x interval_hours`. Apple Silicon and CPU-only nodes fall to
tier 3 (powermetrics needs sudo and is never invoked). The `nvidia-smi` power
probe runs ONLY when energy is enabled and a GPU is present.

**Enabling (per node, three ways):**
- Env var: `CITADEL_ENERGY_SAMPLING=1` (truthy `1`/`true`/`yes`/`on`; wins when set).
- Node config file: `energy.yaml` (`sampling_enabled: true`) in `platform.ConfigDir()`,
  via `config.LoadEnergy` / `config.SaveEnergy` (mirrors telemetry.yaml).
- Platform: `APPLY_DEVICE_CONFIG` with `DeviceConfig.EnergySampling *bool`
  (`energySampling` in the JSON), which load-modify-saves the same energy.yaml
  (default-DENY pointer semantics, like ShellEnabled).

Resolution precedence (`cmd/work.go:resolveEnergySampling`): env var if set,
else energy.yaml, else OFF.

**Backward-compat:** `query.go` gates parsing on the core (pre-energy) column
count, so both 9-column (energy-off) and 12-column (energy-on) CSVs read cleanly.
A daily file can mix schemas if energy is toggled mid-day; the Go query path is
index-based and fine. A direct DuckDB glob over both schemas needs
`union_by_name=true`.

| Variable | Default | Purpose |
|----------|---------|---------|
| `CITADEL_ENERGY_SAMPLING` | unset (OFF) | Enable the energy estimate. Truthy: `1`/`true`/`yes`/`on`. Overrides energy.yaml when set. |
| `CITADEL_GPU_TDP_WATTS` | unset (use `power.limit`) | Override TDP for the GPU util estimate (tier 2). |
| `CITADEL_CPU_TDP_WATTS` | `65` | CPU package TDP for the coarse CPU floor (tier 3). Apple Silicon runs lower than 65W. |

### On-node Grounding Guardrail (`internal/trust`, aceteam #8253, guardrail half)

`trust.CheckGrounding(input, output string) GroundingResult` is a pure, local,
deterministic check (regex/tokenization, NOT an LLM judge — no network call)
for whether numeric/statistic claims in a model's OUTPUT are supported by its
INPUT. It exists because an insight-extraction run on llama3.1:8b turned the
source's "a majority" / "a small fraction" into fabricated "68%" / "7%" —
exactly the case `TestCheckGrounding_FabricatedPercentages_Flagged` pins.

`eligible(kind ClaimKind)` decides which extracted claims are even checked —
read it before assuming "every number is a claim": years and list-index/
ordinal tokens are classified but never flagged, everything else (percent,
ratio, currency, bare count) is. `isSupported` decides what counts as
grounded: exact numeric match after normalization, rounding-tolerance
equality, or an "N out of M" ratio and its derived N/M*100 percentage
matching each other — deliberately NO semantic derivation (no word→number
mapping like "majority" implies ">50%"); adding one would silently un-flag
the incident this guardrail exists to catch.

**`GroundingResult.Score`/`Grounded` are not a truthfulness signal.** A
claim-free reply is `Grounded=true, Score=1.0` — vacuously, because there was
nothing to check, not because anything was verified. `ClaimsChecked` is the
denominator that disambiguates "nothing to check" from "everything checked
out"; read it alongside Score, never Score alone.

**Wiring is opt-in and single-point, not pervasive.** `llm_inference` serves
general chat, code generation, and vision/OCR through the SAME handler
(`internal/worker/llm_inference.go`), and "a number in the output not in the
input" is normal for those (arithmetic answers, port numbers, facts recalled
from training data) — attaching the guardrail unconditionally would flag most
of that traffic. `groundingGuardrailEnabled()` gates it behind
`CITADEL_GROUNDING_GUARDRAIL` (default OFF, like every other advisory-signal
toggle in this codebase), and the ONE wired call site is
`bufferedChatCompletions` — the non-streaming chat-completions path, chosen
because it is the only place both the full input and full output already
exist as Go strings before anything is sent, so flag-only (the shipped
default; see `Policy`/`Block`) is safe and gating would be too (a streamed
reply is already sent token-by-token before the full text exists, so it can
be flagged post-hoc but not gated). The streaming and llamacpp/ollama
buffered/stream pairs in that file are documented, not-yet-wired hooks with
the identical shape.

**Known false negative:** `extractClaims`' regex priority gives years
(`ClaimYear`) precedence over bare counts, so a fabricated count that looks
like a plausible year ("1,950 respondents") slips through unflagged —
`TestCheckGrounding_YearPriorityIsAKnownFalseNegative` pins this as a
documented v1 tradeoff, not a bug to silently fix by making the guardrail
guess at year-vs-count from context.

**Receipt-signing (aceteam #8253's AEP half) is implemented — see
`internal/aep` and the Signed AEP Receipt section below.** This package
(`internal/trust`) still attaches only the plain `grounding` map to job
output (`groundingReceiptMap` in `internal/worker/llm_inference.go`,
mirroring `synthesizeReceiptFromHeaders` in
`internal/jobs/synthesize_speech.go`) and does not sign, persist, or
transmit anything itself — signing is a separate, additional opt-in layered
on top by the caller, described below.

| Variable | Default | Purpose |
|----------|---------|---------|
| `CITADEL_GROUNDING_GUARDRAIL` | unset (OFF) | Attach the `grounding` receipt to buffered chat-completion results. Truthy: `1`/`true`/`yes`/`on`. |

### Node identity persistence + signed AEP receipt (aceteam #8139/#8253, `internal/aep`)

Design doc: [docs/design-node-identity-receipts.md](docs/design-node-identity-receipts.md).
Two adjacent slices, both citadel-Go only — the aceteam-side halves (backend
echo of the fabric node ID, backend signature verification, public-key
registration) are separate, not-yet-built work this doc's §4 table tracks.

**#8139 — fabric node ID persistence.** `DeviceConfig.FabricNodeID`
(`cmd/work.go`) and `config.DeviceCreds.FabricNodeID`
(`internal/config/devicecreds.go`) read/write the numeric AceTeam
fabric/platform node ID from the SAME machine-convergent `config.yaml` as
`org_id`/`org_name`/... (see the Device/org config note under
`ConfigDir()`/`GetNodeConfigDir()` above). Two write paths: `nexus.
TokenResponse.FabricNodeID` (an additive, inert field on the device-auth
`/token` response, persisted by `saveDeviceConfigToFile`) for one candidate
backend echo point, and the standalone `saveFabricNodeIDToConfig` (`cmd/
init.go`) for the other (a future heartbeat-ack handler) — both use the same
read-existing-map/merge-one-key/write-back discipline, DELIBERATELY not
reusing `SSHSyncConfig.NodeID`'s writer pattern: that one clobbers
`api_token` to `""` when a caller supplies only the node ID (no non-test
callers exist today for exactly this reason — see
`docs/whoami-fabric-id-gap.md`). `cmd/whoami.go`'s `gatherIdentity` prefers
`DeviceConfig.FabricNodeID` over the legacy `SSHSyncConfig.NodeID` fallback
(`resolvePlatformNodeID`). **Inert today**: no backend process sends either
echo point yet, so this reads empty on every real node — the same "not
available locally" state as before this landed, just with a real read/write
path instead of no path at all.

**#8253 — signed AEP receipt.** `internal/aep` (`AEPReceiptV1`,
`Canonicalize`, `BuildSignedReceipt`) signs the `internal/trust` grounding
receipt (see above) with `internal/nodeidentity`'s ECDSA P-256 key — NOT
`internal/nodevault`: nodevault is a PIN-gated symmetric secrets vault
(`Session.DeriveSubkey` + AES-GCM) with no asymmetric primitive and no
unattended-unlock story, so it structurally cannot back signing under a
headless `citadel work` (design doc §1c). `nodeidentity.Store` already
generates an unattended-capable ECDSA key at every `citadel init`
(`ensureNodeIdentity`, for a currently-inert mTLS CSR/leaf flow, #4583) —
`Store.Sign`/`Store.PublicKeyFingerprint` (added here) are its second
consumer.

`Canonicalize` signs a fixed-shape, newline-delimited byte sequence over
nine explicit fields (`node_id`, `job_id`, `issued_at`, `engine`, `model`,
`grounded`, `score`, `claims_checked`, `flagged_hash`) — deliberately NOT
`json.Marshal` of the receipt or a generic map, because `Score` (a float64)
can change byte representation across an unrelated re-serialization (a Redis
hop, the backend's own re-marshal of the job-output envelope) without
changing value, which would silently break a naive byte-signature. A
verifier must recompute this SAME canonical form from the receipt's own
fields, not from a generic re-marshal. `Signature`/`PublicKeyFingerprint` are
populated AFTER signing and are excluded from what's canonicalized — a
signature covering its own field is the standard way this class of scheme
breaks silently.

**Wiring is nested inside the existing grounding gate, plus one more opt-in
on top** (`internal/worker/llm_inference.go`'s `bufferedChatCompletions`):
signing only runs when `CITADEL_GROUNDING_GUARDRAIL` has already computed a
`GroundingResult` (there's nothing to sign otherwise) AND
`CITADEL_SIGN_AEP_RECEIPTS` is separately on — a node can run the guardrail
without ever touching a private key. `LLMInferenceHandler.ToMap()`
(`internal/aep`) converts the signed `*AEPReceiptV1` to a plain
`map[string]any` before it's attached to job `Output["aep_receipt"]` —
Output crosses the wire via StreamWriter/Redis/API serialization elsewhere
in the worker, so attaching a typed Go pointer directly would be the only
one of its kind in that map and risks a downstream consumer that
stringifies rather than `json.Marshal`s it. Signing failure (e.g. the node's
key is unavailable) fails OPEN: the job still succeeds with `content`/
`grounding` intact, just without `aep_receipt` (`h.aepLogf` logs it,
non-fatally — an injectable field, not a bare `log.Printf`, since this
package otherwise imports no logger at all).

**Machine-convergent by construction — fixed, not a known hazard.** The
signing key is NOT `nodeidentity.Default()` (which roots at invoker-scoped
`platform.ConfigDir()`, see the `ConfigDir()`/`GetNodeConfigDir()` entry
above — still used, unchanged, by `cmd/device.go`'s device-mode enrollment
and `cmd/init.go`'s dormant mTLS CSR flow, both of which depend on staying
invoker-scoped/shared with `citadel init`'s own context). `defaultAEPSigner()`
(`internal/worker/llm_inference.go`) instead constructs a **separate**
`nodeidentity.Store` rooted at `aepSigningStoreDir(network.GetNodeConfigDir())`
— the SAME machine-convergent directory `citadel init`'s device-config write
(#845) and #726's heartbeat marker already use — so a systemd-root `citadel
work` and an interactive non-root process resolve the IDENTICAL signing key
file; a future Phase 2 backend registration of this node's public key can
never desync from what `citadel work` actually signs with.
`TestDefaultAEPSigner_MachineConvergentAcrossInvocationContexts`
(`internal/worker/llm_inference_test.go`) pins this directly: two
independently-constructed `Store` instances resolving the same converged
`nodeConfigDir` (standing in for two different invocation contexts) load/
create the identical key. `LLMInferenceHandler.WithSigner` remains the
override seam for tests and any future signer change.

**Inert until the aceteam-side lands** (design doc §4): signing is fully
wired citadel-side, but the backend does not yet hold this node's public key
to verify against, and does not yet echo a fabric node ID for `#8139`'s
`node_id` field to use (it falls back to the signer's own
`PublicKeyFingerprint` — `aep.ResolveNodeID`'s phasing rule — until it does).

| Variable | Default | Purpose |
|----------|---------|---------|
| `CITADEL_SIGN_AEP_RECEIPTS` | unset (OFF) | Attach a signed `aep_receipt` alongside `grounding` on buffered chat-completion results. Requires `CITADEL_GROUNDING_GUARDRAIL` also on. Truthy: `1`/`true`/`yes`/`on`. |

### Service Idle Detection and Auto-Stop (citadel #416)

Managed services carry a per-service usage/idle signal so a node can tell whether an engine is actually being used or is pinning VRAM/RAM while idle. This is surfaced on the heartbeat and to operators, and can optionally drive auto-eviction.

**Signal sources** (`internal/status/`):
- `idle.go`: `IdleTracker` scrapes the engine's own Prometheus `/metrics` (vLLM request counters + running/waiting gauges) for a precise `last_request_at` / `idle_seconds`. When it cannot scrape, it reports NO signal rather than a misleading "idle since startup". `idleCapableEngines` (`engines.go`) names which engines this covers — read it, don't assume it's "every engine" (citadel #691 is exactly that assumption failing).
- `footprint.go`: `FootprintIdleTracker` derives idle from CPU/GPU utilization for engines with no request counters, plus `ServiceFootprint` (CPU/RAM/VRAM) and the `IsHeavyAndIdle` eviction-candidate predicate; `RecordActivityCounter` (`idle.go`, used from `attachDerivedIdle`) derives a `last_request_at` from a container's NetIO counter (#433) when a real reading is available. Both are coarser than a scraped request counter and depend on the container runtime reporting usable stats.
- `request_recorder.go`: `RecordEngineRequest` (citadel #691) is a third, DIRECT source — not scraped or inferred. The gateway's chat router (`internal/gateway/chat_route.go`) and the worker's `llm_inference` handler (`internal/worker/llm_inference.go`) call it at the moment THIS node dispatches a request to a local engine, so an engine with no cooperating metric (ollama, bonsai, a backstop-reported diffusers/sglang) still gets a real `last_request_at` instead of "never". Coverage limit: it only sees requests that flow through this node's own routing paths (a peer or human hitting the engine's host port directly is invisible to it, same limit the other two signals have for non-vLLM engines) — and it is process-local, unpersisted memory, so a short-lived `citadel status`/`citadel services` invocation and a long-lived `citadel work`'s heartbeat can disagree about the same engine's `last_request_at` (see `request_recorder.go`'s package comment for why, before concluding either is wrong).
- `Collector.applyNodeRoutedRequestSignal` (`collector.go`) is what owns the merge order between these three, run once on the fully-assembled heartbeat status: it may only ADD a `last_request_at` or REDUCE reported idleness relative to what the scrape/footprint signals already decided, never manufacture idleness a more precise signal ruled out (`mergeNodeRoutedSignal` states the exact rule and why the direction is load-bearing, not cosmetic — `SERVICE_AUTO_STOP_WHEN_IDLE` reads the result).
- All three attach an `*IdleState` inline to each `ServiceInfo`/`AppInfo` in the heartbeat (`idle` / `idle_seconds` / `last_request_at`). Additive and back-compatible: absent when unknown. The platform reads these per-service fields directly; there is deliberately no separate `service_usage` map.

**Operator view:** `citadel services` runs one collection and prints each managed service/app with usage (busy / idle `<dur>` / unknown), footprint, and eviction-candidate notes. Distinct from `citadel service` (alias `svc`), which manages Citadel itself as a system service.

**Optional auto-stop-when-idle** (`autostop.go`, default OFF): when `SERVICE_AUTO_STOP_WHEN_IDLE=true`, `citadel work` stops any service confirmed idle past a threshold to reclaim GPU/RAM. Safety contract:
- Default OFF; never auto-evicts unless explicitly enabled.
- Acts ONLY on a concrete idle signal past threshold (`IdleState.Idle && idle_seconds >= threshold`); an absent/unknown signal is never treated as idle.
- Threshold: `SERVICE_AUTO_STOP_IDLE_SECONDS` (falls back to `SERVICE_IDLE_THRESHOLD_SECONDS`, default 300s; a zero/invalid value is clamped to the positive default).
- The reconciler runs off the heartbeat's existing collection (an `OnStatus` callback on the publisher), so enabling it adds no extra `docker stats` / `nvidia-smi` sweeps on an already-contended node.
- Routes stops by entity kind: manifest/embedded services via the compose `down` path (`ServiceHandler.StopServiceByName`), catalog apps via `apps.Stop`. Both are covered so the common idle-GPU-hog case (a catalog app) is not silently skipped. Every stop is logged.
- Runtime assumption: the stop calls use the `docker` runtime (matching the existing SERVICE_STOP job path and the docker-only `apps` package). On a podman-preferred node the engine idle *detection* is podman-aware but the auto-stop call would fail safe (logged warning, no eviction). Making the shared stop path runtime-aware is a documented follow-up.

Environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `SERVICE_IDLE_THRESHOLD_SECONDS` | `300` | Seconds without a request before a service is reported idle in the heartbeat. |
| `SERVICE_AUTO_STOP_WHEN_IDLE` | unset (OFF) | Opt-in to auto-stop idle services. Truthy: `1`/`true`/`yes`/`on`. |
| `SERVICE_AUTO_STOP_IDLE_SECONDS` | idle threshold | Idle seconds before auto-stop acts. |

### Dynamic Inference-Queue Resubscription (citadel #612)

`capabilities.InferenceQueues(caps, serving)` is evaluated ONCE, at `citadel
work` startup (API-mode queue resolution in `cmd/work.go`). A node with no
static GPU-derived queue (`GPUInferenceQueues` empty -- no discrete GPU) and no
serving engine yet at boot gets no inference queue at all there. Previously
that was permanent for the process's lifetime: a platform `SERVICE_START` that
starts an engine minutes later (the console model-deploy path onto a fresh
node) would never be picked up without a worker restart.

`worker.InferenceQueueReconciler` (`internal/worker/inference_queue_reconciler.go`)
closes the gap by re-checking `nodeIsServingModels` (the same
`status.DiscoverLocalEngines`-backed, #649-safe check startup already uses) on
every heartbeat tick and calling the source's existing `AddQueue` on the
false->true transition. It does not poll on its own -- it rides the
heartbeat's existing ~30s `OnStatus` tick, which now **fans out to multiple
registered callbacks** (`SetOnStatus` in `internal/heartbeat/redis.go` /
`api.go` appends rather than replaces) so this reconciler and #416's auto-stop
reconciler can both subscribe without clobbering each other.

Deliberately subscribe-only, matching what `JobSource.AddQueue` actually
offers (there is no `RemoveQueue`): once subscribed the reconciler stops
probing for good, and it never un-subscribes when an engine later stops --
direct-Redis mode already tolerates a node staying subscribed to
`jobs:v1:gpu-general` while idle unconditionally, so this is not a new
tradeoff. Wired in API mode only: direct-Redis mode's `ResolveQueues` already
joins `jobs:v1:gpu-general` unconditionally regardless of `serving` (see
`cmd/work.go`'s direct-Redis queue-resolution block), so it has no analogous
startup-snapshot gap for this reconciler to fix.

**Construction is itself gated on the gap existing** (`missingQueues` in
`cmd/work.go`), not just what it does once built: `GPUInferenceQueues` is
unconditional on `serving`, so a GPU node's boot-time queue set already equals
`InferenceQueues(caps, true)` -- diff them, and a reconciler for that node
would have nothing to add, ever. Without this gate it would still self-limit
to one in-flight probe (see below) but would burn a `docker ps` every ~30s
forever with no possible payoff, which is exactly the sweep OnStatus reuse
exists to avoid. Only a node whose boot set is missing something (no discrete
GPU, not yet serving) gets a reconciler at all.

### Service Preemption and Node Pinning (citadel #577)

A `SERVICE_START` that declares a VRAM budget on a full GPU auto-evicts
(preempts) other services to make room — UNLESS they are pinned. Generalizes the
#416 idle signal + #6018 VRAM-fit + durable `desired_status: stopped`.

**Manifest field — `pinned_services` (node-wide allowlist):**
```yaml
# citadel.yaml
pinned_services:
  - bonsai          # never preempted
services:
  - name: bonsai
  - name: vllm      # preemptible (the default for anything not pinned)
```
Modeled on both `CitadelManifest.PinnedServices` (`cmd/manifest.go`) and the
minimal `serviceManifest.PinnedServices` (`internal/jobs/service_handler.go`).
Empty/absent ⇒ every service is preemptible.

**Pure decision (`internal/status/preempt.go`, unit-tested):**
`PlanPreemption(candidates, requiredVRAM, availableVRAM) PreemptPlan` decides
which non-pinned services to stop. Contract:
- `requiredVRAM==0` (no declared budget) ⇒ Fits, no preemption. Never evict on an
  absent signal.
- Already fits (`availableVRAM >= requiredVRAM`) ⇒ Fits, no preemption.
- **Pinned candidates are NEVER stopped.** If the deploy can't fit after stopping
  every non-pinned service, `Fits=false` and `Blocked` names the pinned VRAM
  holders → the deploy is REJECTED (job FAILURE).
- Ordering is **idle-first** (stop idle before busy) then **largest-VRAM-first**,
  name-ascending tie-break. Busy non-pinned services ARE preemptible — idle is
  ordering, not a gate.

**Executor (`ServiceHandler.preemptForVRAM`):** runs inside the docker
`serviceStart` branch, gated on the target **not already running** (an
already-running / model-recreate start already holds its VRAM). Collects live
node status once (free VRAM from `GPUMetrics`, per-service `Footprint.VRAMBytes`,
instantaneous idle via `status.FootprintActive` — NOT the debounced
`FootprintIdleTracker`, which reads non-idle on a fresh collector), builds
candidates, and executes the plan. Each eviction is **durable**: it sets
`desired_status: stopped` THEN compose-downs (the SERVICE_STOP path), so the
manifest reconcile does not restart it out from under the incoming deploy (the
#528 VRAM-cascade gotcha). Preemption is therefore **sticky** — an evicted
service stays down across reboots until an explicit `SERVICE_START` (which clears
the marker) brings it back.

**VRAM budget is a NEW payload contract, currently INERT.** `preemptForVRAM`
reads `vram_mb` (preferred) or `vram_gb` from the `SERVICE_START` payload
(`parseRequiredVRAMBytes`). The aceteam backend does **not** yet forward it
(`fabric_provision` dispatches only `{service, model}`; DeployModel's
`vram_budget_gb` is explicitly "not yet forwarded to Citadel"), so preemption is
a no-op until the backend sends one of these keys. **AceTeam-side follow-up (different
repo):** forward #6018's VRAM-fit budget as `vram_mb` on the model-deploy
`SERVICE_START` dispatch.

**Surfacing:** `ServiceInfo.Pinned` rides the heartbeat (`pinned`, omitempty);
`citadel services` shows a `PIN` column (`pinned` / `preemptible`; `-` for apps,
which are not pinnable via this service-level allowlist). Collectors are fed the
allowlist via `CollectorConfig.PinnedServices` (the `citadel work` heartbeat path
and `citadel services`).

**Known limitations / follow-ups:**
- The decision dimension is VRAM only; RAM preemption is a documented follow-up.
- Catalog apps are not pinnable and are excluded from preemption candidates here
  (the eviction path is the service compose-down); the #416 auto-stop reconciler
  still handles idle catalog apps separately.
- The TUI control center collector is not yet fed `PinnedServices` (heartbeat and
  `citadel services` are); low-priority follow-up.

### Per-job resource isolation: RAM cgroup ceiling + citadel-side VRAM preflight estimate (citadel #831)

Design doc: [docs/design-resource-isolation.md](docs/design-resource-isolation.md)
(read it for the full RAM-vs-VRAM tractability analysis; MIG/MPS hard VRAM caps
are explicitly parked there — #842/#843 — and NOT part of this). Both halves
below are gated behind ONE opt-in flag, default OFF:

| Variable | Default | Purpose |
|----------|---------|---------|
| `CITADEL_RESOURCE_ISOLATION` | unset (OFF) | Enables both mechanisms below. Truthy: `1`/`true`/`yes`/`on`. |

Off by default because both can newly refuse or durably affect a GPU-service
start on a node that has never seen that happen — the same posture this
codebase already uses for every toggle that changes eviction/refusal behavior
(`SERVICE_AUTO_STOP_WHEN_IDLE`, `CITADEL_GROUNDING_GUARDRAIL`,
`CITADEL_ENERGY_SAMPLING`, ...). The design doc's owner decision (2026-08-25,
on citadel#831) resolved the POLICY ("real cgroup for RAM," "preflight-only,
refuse fast for VRAM," "no on-node wait queue") but left exact reserved-floor
SIZING an open question (§6 Q1) — this flag is the deliberate way to light the
mechanism up on a node an operator has reviewed, without waiting for the
sizing question to be fully closed or for the backend to start forwarding
`vram_mb`/`ram_mb`.

**1. RAM cgroup ceiling for GPU services (`internal/jobs.applyRAMIsolation`,
`internal/catalog/ram_override.go`, `internal/status/ram.go`).** Extends the
existing `.sandbox.yml` override delivery (`catalog.GenerateHardeningOverride`,
Service Management section above) to the population it deliberately EXEMPTS —
GPU/inference services — but with a NARROWER override (`mem_limit` only, no
cap-drop/read-only-rootfs, delivered as a separate `<name>.ram.yml` so the two
overrides never collide) and a generous, dynamically-derived ceiling instead of
the Tier-2 2GB default. `status.RAMBudgetBytes(availableRAMBytes,
pinnedRAMBytes)` computes it: `SystemMetrics.MemoryAvailableGB` minus the
RUNNING `pinned_services`' own measured `Footprint.RAMBytes` minus a 2GiB OS
headroom. When that would fall below a 2GiB viable minimum, it returns 0 ("no
safe ceiling") rather than clamping UP to that minimum — a clamped-up value
would be a fabricated number with no relationship to what's actually free,
and applying it as a real inference engine's `mem_limit` reproduces the exact
failure this mechanism exists to prevent, just reached a different way;
`applyRAMIsolation` treats 0 as "skip isolation for this start" (fail open),
same direction as every other decision here. Regenerated on every
`SERVICE_START` for a GPU service that is not already running
(`ServiceHandler.serviceStart`'s docker branch, alongside `preemptForVRAM`) —
a runaway process is then killed by ITS OWN cgroup `memory.max` before the
host's global OOM killer has to pick a victim across every container on the
box (the incident that motivated #831: a CPU-offloaded ~19GB text encoder
OOM-killed an unrelated production container). `cmd/service.go`'s boot-time
start path (`composeFileArgs`) deliberately does NOT apply this override, even
though `catalog.ExistingGPURAMOverride` exists and would resolve it: that path
has no per-call access to `CITADEL_RESOURCE_ISOLATION`, so a file left over
from an earlier opted-in run would keep applying after an operator turns the
flag back off, silently defeating the opt-out. Only the job-driven
`SERVICE_START` path applies (and can refuse on) this override today.

**RAM preflight is a separate, narrower check bundled into the same call**
(`status.PlanRAMPreflight`): mirrors `PlanPreemption`'s (#577) and
`planDiskPreflight`'s (#828, `internal/jobs/disk_space.go`) fail-open/
fail-closed contract exactly — a `SERVICE_START` payload's `ram_mb`/`ram_gb`
(mirroring `vram_mb`/`vram_gb`; the aceteam backend does not send either
today) of `0` (absent) ALWAYS fits (never refuse on an absent signal); a
declared requirement that exceeds the RAM budget is the ONLY refusing case — a
confirmed shortfall, never a guess, returned as a job FAILURE with a precise
"needs X, node has Y" message, per the owner's "refuse fast, clear error"
decision. Unlike VRAM (#577), RAM isolation does NOT preempt/evict other
services to make room — the design doc scopes RAM preemption as a documented
follow-up; RAM safety here comes entirely from the per-service cgroup ceiling,
never from stopping something else.

**2. VRAM preflight's citadel-side estimate (`internal/jobs.
resolveRequiredVRAMBytes`).** #577's `preemptForVRAM`/`PlanPreemption` has
been real, tested, wired code since #577 shipped — but inert on a live deploy,
because it only fires when the `SERVICE_START` payload carries `vram_mb`/
`vram_gb`, which the aceteam backend has never sent (see the Service
Preemption and Node Pinning section above). `resolveRequiredVRAMBytes` closes
that gap WITHOUT waiting on the backend: the payload-declared value still
wins unconditionally when present; only when it's absent AND resource
isolation is opted in does it fall back to `status.EngineVRAMEstimateMB`, the
SAME per-engine VRAM provisioning-budget table the model-hotswap swap planner
(`internal/worker/swap.go`) already uses as ITS OWN fallback when it has never
measured a given (engine, model) pair (citadel#689) — one table, two
consumers, no new numbers invented. `EngineVRAMEstimateMB` is already a
conservative PROVISIONING budget (sized above half a 24GB card for the big
engines — see its doc comment in `internal/status/hotswap.go`), so no
additional margin is applied on top here; the swap planner's OWN separate
1.15× margin on a MEASURED footprint (`vramFitMarginFactor`, `internal/worker/
swap.go`, citadel#874) is a different code path entirely and is untouched by
this change.

### Job-scoped GPU reserve/evict/restore (citadel #832)

`internal/jobs.ServiceHandler.Reserve` / `.Release` / `.ReconcileOrphanedReservations`
extend #577's preemption with an auto-RESTORE leg, so a caller (a future job type,
not wired up yet) can hold a guaranteed VRAM budget for its own lifetime and have
whatever it evicted come back automatically — including across a crash. It reuses
#577's decision unchanged (`buildPreemptCandidates` + `status.PlanPreemption`); the
new piece is durable per-service tagging.

**The marker IS the reservation — no separate ledger file.** `Reserve` durably
tags every service it stops with `evicted_by_job: <jobID>` (plus
`evicted_prior_status`, the service's `desired_status` immediately before
eviction) via `setEvictedMarkersInManifestFile`, the same yaml.Node-surgery
pattern as #577's `desired_status`. `Release(jobID)` restarts exactly the
services carrying that tag and, only on a successful restart, clears both
markers — restoring `evicted_prior_status` rather than unconditionally
clearing it, so a service an operator had already marked stopped (e.g. a prior
`SERVICE_STOP` whose compose-down failed, leaving it running and therefore a
preemption candidate) comes back with that same stopped intent, not silently
flipped to start-on-boot. A service whose restart fails KEEPS its tag, so a
retried `Release` (or the crash reconcile below) picks it up again — this is
what makes `Release` idempotent and safe to call more than once.

**Tag-scoped restore, not blanket restore.** `Execute()`'s `SERVICE_START` and
`SERVICE_STOP` branches both clear a service's reservation tag as part of
handling an explicit operator/platform action — operator intent is a stronger
signal than a pending reservation, and clearing it here is what keeps a later
`Release` for the now-irrelevant reserving job a harmless no-op instead of an
unexpected extra restart of a service the operator stopped for an unrelated
reason.

**Crash-safety is a call-site precondition, not an assumption — and the
precondition covers only ONE of two competing-consumer doors.**
`ReconcileOrphanedReservations` takes a required `holdsWorkerLock bool` and
refuses outright when false — it does not infer or trust call order. The
argument "any `evicted_by_job` tag found here is orphaned" is only true when
exactly one worker can be live for this node: this `ServiceHandler` has
created no reservations of its own yet at that point, so a tag can only be
left over from a previous process invocation that exited before releasing it.
The only correct call site is `cmd/work.go`'s `runWork`, immediately after a
successful `worklock.Acquire`, before the job consume loop starts.

`internal/worklock` guards `citadel work` against a SECOND `citadel work` — it
does NOT guard against the control-center TUI's own worker path. When no
dedicated `citadel work` holds the lock (`workerHeld == false` in
`cmd/controlcenter.go`), the control center runs its own consume loop off the
SAME `buildNodeJobHandlers` handler set **without ever calling
`worklock.Acquire`**. Reservation reconcile is wired only in `runWork` today,
so this is currently latent (nothing calls `Reserve` yet) — but a future
caller (e.g. #8248) wiring `Reserve`/`Release` into a handler reachable from
the control-center path reopens exactly the hazard `holdsWorkerLock` exists to
close, via the other door: a CC-held reservation, then a later `citadel work`
legitimately `Acquire`s (nobody holds the lock) and its startup reconcile
destructively restarts a service the still-live CC job is using.
`ReconcileOrphanedReservations`' doc comment states this gap and the two ways
to close it (make the CC path `Acquire` too, or add owner identity — pid +
start time — to the marker) in detail; read it before wiring such a caller.

**Reserve's fit-check divergence from #577 is deliberate.** `preemptForVRAM`
skips the check (logs and proceeds un-preempted) when free VRAM can't be
determined — safe there, since a `SERVICE_START` with no fit signal just
proceeds. `Reserve` is an explicit ask for a *guaranteed* hold, so an unknown
free-VRAM signal is a hard error instead: silently granting an unverifiable
reservation would defeat reserving at all.

**Heartbeat surfacing.** `NodeStatus.GPUReservations` (`gpu_reservations`,
omitempty) lists active reservations by job id, fed by
`ServiceHandler.ActiveReservations()` — a pure manifest read — via
`CollectorConfig.Reservations`, wired only in `cmd/work.go`'s two heartbeat
collector construction sites (same pattern as `PinnedServices`/`SwapStats`; the
TUI control-center collector has the same not-yet-wired gap noted above). The
projection lives in `cmd/work.go`'s `reservationsFrom`
(`jobs.ReservationSummary` → `status.GPUReservation`), mirroring #717's
`swapStatsFrom` for the same reason: `internal/status` cannot import
`internal/jobs`. `TestReservationShapeParity` pins the two shapes stay in sync.

**Scope boundary (deliberate, citadel #832):** this issue is the on-node
primitive only — reserve/evict/restore plus crash-safe reconcile plus
heartbeat state. It does **not** wire Reserve/Release to any job type (no
caller exists yet) and does **not** pick a dedicated node across the fabric
(that's the platform's scheduler, a heartbeat consumer of `GPUReservations`).
A routine platform `SERVICE_START` targeting a reservation-held service mid-
reservation is not refused here — it clears the tag (per "tag-scoped restore"
above) and proceeds, silently defeating the hold; whether to refuse a
`SERVICE_START` during an active reservation is a future caller's policy
decision, not this primitive's.

**Tag-clearing is a job-path-only guarantee, not a universal one.** Only the
`Execute()` job dispatch path (`SERVICE_START`/`SERVICE_STOP`, i.e. a remote
operator/platform action) clears a service's reservation tag. The LOCAL start
paths — `citadel run --service X`, and boot-time `startManagedServices` — call
into the service-start machinery directly, bypassing `Execute()`, so they do
NOT clear it. An operator manually running a reservation-held service back up
this way leaves it still tagged `evicted_by_job`; a later `Release` for that
job then still finds it and rewrites its `desired_status` (though not its
running state — the start-side helpers already short-circuit on
already-running, so this is a manifest-only side effect, not a second start).
Latent and low-severity today (no caller reserves anything yet), documented
alongside the CC/worklock gap above rather than fixed, for the same reason:
narrow, deliberate scope for the primitive PR.

### Model exclusivity: `run --exclusive` + local MCP deploy/evict (aceteam#8248/#8249, citadel#851's first caller)

Design: [docs/design-model-exclusivity.md](docs/design-model-exclusivity.md).
`internal/jobs/model_exclusivity.go` is #832's reserve/evict/restore
primitive's first real caller: `citadel run <service> --exclusive`
(`cmd/run_exclusive.go`) and three local MCP tools
(`local_model_deploy`/`local_run_exclusive`/`local_model_stop`,
`cmd/mcp_local.go`).

**The naive VRAM budget ("whole card minus a margin" fed into `Reserve`) is
unsatisfiable by construction** whenever VRAM is held by something
`status.PlanPreemption` cannot see as a candidate (an unmanaged process,
driver/CUDA overhead) — `Reserve` would refuse even though evicting
everything non-pinned is exactly what was asked and would free real VRAM.
`ServiceHandler.ReserveExclusive(ctx, jobID, exclude)` fixes this by skipping
the fit-check arithmetic entirely: it evicts every non-pinned RUNNING
candidate unconditionally (including one reporting `VRAMBytes==0` — a
genuinely exclusive ask does not selectively spare a service just because its
footprint measurement happened to read zero), tags each exactly like
`Reserve` does, and reports the ACTUAL resulting free VRAM on the returned
`Reservation` (`FreeVRAMBytes`/`FreeVRAMKnown`) rather than asking the caller
to have predicted it. It collects node status TWICE — once for candidates,
once after eviction for the real free-VRAM reading — so a test stub must
expect two `collectStatus` calls. Unlike `Reserve`, an unknown pre-eviction
free-VRAM signal is NOT a hard error: `Reserve`'s hard-error contract exists
to protect a fit CLAIM it is verifying; `ReserveExclusive` makes no fit claim
at all. `Reserve` itself is still used for the bounded alternative
(`--vram`/`vram_mb`): an ordinary, satisfiable ask for exactly N bytes.

**Ownership shape is deliberately (a) from the design doc's §2.3, not (b):**
the CLI process (or `citadel mcp`) calls `Reserve`/`ReserveExclusive`/
`Release` DIRECTLY — no worklock, no job dispatched into a running worker —
mirroring how `citadel module stop|start|restart` (#846) already calls
`liveModuleOps` directly. The design doc found no existing local
job-submission path into `internal/worker.Runner`; building one was judged
real, non-trivial plumbing, not a thin wrapper, so it was deliberately not
built for this. **Crash-safety consequence, stated plainly rather than
assumed away:** the `evicted_by_job` tag is durable, and `cmd/work.go`'s
`runWork` already calls `ReconcileOrphanedReservations` right after acquiring
`internal/worklock`, before its consume loop — so a CLI/MCP process that dies
mid-exclusive-run does not strand evicted services forever; the NEXT `citadel
work` boot restores them. But this is the EXACT hazard
`ReconcileOrphanedReservations`' own doc comment already names for a future
`#8248` caller: neither this CLI/MCP process nor the control-center's consume
loop holds `worklock`, so a `citadel work` that boots WHILE an exclusive
run/deploy is still legitimately in progress can conclude the tag is
orphaned and restore the evicted peers out from under it. This is a real,
live race on any node that might also run `citadel work` (or the
control-center) concurrently with a standalone exclusive run — not a
theoretical one closed off by this design. Closing it (owner identity on the
marker, mirroring `worklock`'s own stale-lock classification, or making
every job-consuming path acquire `worklock`) is deferred, tracked follow-up
work. `citadel module reservations release <jobID>` is the manual escape
hatch if this race (or any other stuck reservation) needs it.

**`citadel module reservations list|release`** (`cmd/module_reservations.go`,
the design's §2.6 escape hatch) are thin wrappers over the already-tested
`ActiveReservations`/`Release` primitives, with the same `--dry-run`/
`--expect-node` posture `citadel module stop|start|restart` has.

**`--node-dir` as a FLAG (not `CITADEL_NODE_DIR` as an env var) is refused**
by all three CLI/MCP entry points
(`refuseIfReservationNodeDirUnsupported`, `cmd/nodedir.go`): these paths call
`internal/jobs.ServiceHandler` directly, and that package's own
`ensureEmbeddedComposeFile` container-name-namespacing (citadel#860) reads
`CITADEL_NODE_DIR` from the environment ONLY — it cannot see cobra flags. The
flag form would silently namespace nothing while still durably
evicting/starting the real node's services — the exact citadel#853/#856/#860
incident class. Using the env var instead of the flag is fine (no
divergence) and is not refused.

**Naming collision, resolved:** the release-half tool is `local_model_stop`,
not `local_model_evict` — `MODEL_CACHE_EVICT` (`internal/worker/job.go`) is a
DIFFERENT, already-existing job type that deletes cached weights from disk;
`local_model_stop` only stops serving, consistent with `local_module_stop`'s
existing naming. It is deliberately overloaded to do two things depending on
state: always stops the engine serving the given model (resolved via the
SAME `gateway.ResolveChatModel` lookup `local_list_models`/`local_chat` use),
and — ONLY if an active reservation exists under `jobs.ExclusiveReservationJobID(engine)`
(a pure `HasActiveReservation` read) — ALSO releases it afterward, restoring
whatever `local_run_exclusive` evicted. **Order is load-bearing: stop the
target FIRST, then release.** Releasing first would restart evicted peers
while the target still holds its own VRAM, which can fail to fit or
needlessly contend for it — `TestLocalModelStopOrderingStopsBeforeRelease`
pins the call order directly.

The deterministic reservation id — `jobs.ExclusiveReservationJobID(serviceName)`
= `"exclusive:" + serviceName` — is the stable, documented contract every
caller (the CLI, both MCP tools, the escape hatch) computes independently
rather than passing an in-memory handle around, since #2.3(a)'s shape means
no handle survives between two separately-invoked calls.

### Per-engine tables are hand-synced, not generated (citadel #685, #686)

Engine identity (host port, request dialect, default model, idle capability,
...) is spread across roughly a dozen hand-maintained tables/switches instead
of one adapter interface — deliberate today, not an oversight; see
`docs/design-engine-adapter.md` for the full inventory and the migration plan
(explicitly deferred: this is a design doc only, no interface exists yet).
Three latent gaps that doc surfaced were fixed as bounded bug fixes (#685/#686
follow-up), each pinned by a test rather than restated here:

- `internal/status.resolveInstalledModel` (`hotswap.go`) is the read path for
  "what model would a stopped engine serve" — it now recognizes an
  `engineModelEnvVars` entry for `llamacpp`, so a persisted `LLAMACPP_MODEL`
  resolves and the engine becomes a swap candidate (`TestResolveInstalledModel_
  LlamaCppEnvOverrideResolves`). It deliberately still has NO `engineDefaultModel`
  entry: unlike bonsai/unlimited-ocr, llamacpp's compose has no single stable
  default GGUF (bring-your-own-model, deferred-load/router mode when unset) —
  fabricating one would advertise a model the engine cannot actually serve.
  `services/compose/llamacpp.yml`'s `${LLAMACPP_MODEL:+...}` idiom is what makes
  the env var meaningful; it did not exist before.
- `internal/status.managedProbeEngines` (`engines.go`) is the list the
  heartbeat's model/health probe iterates; it was missing `sglang` entirely,
  making it invisible to that probe, `EngineTypeFromName`/`DiscoverModels`/
  `CheckServiceHealth` (`models.go`), and `mesh.EngineTypeFromName`'s standalone
  duplicate (`internal/mesh/discovery.go`) all at once — one root cause, several
  consumers. Fixed together in one change (a partial landing, e.g. adding the
  engine to the probe list without the `DiscoverModels` case, converts a silent
  gap into a live per-heartbeat error instead).
- `internal/gateway.Server.SetModelSwapper` makes the node's model-hotswap
  manager REACHABLE from the gateway's chat-route path — it was constructed
  (`buildNodeJobHandlers`, `cmd/work.go`) after the chat router was wired and
  after the gateway had already started serving, so there was no point in
  startup at which a caller could hand it a valid reference at all. The fix
  (`cmd/gateway_swap.go`'s `swapManagerAdapter`) wraps the SAME
  `nodeSwapManager atomic.Pointer[worker.SwapManager]` the heartbeat's swap-stats
  reporting already reads, so `SetModelSwapper` can be called at
  gateway-construction time and resolve correctly once that pointer is
  populated later — no reordering of the existing startup sequence. This does
  NOT make the chat route call `EnsureResident`; wiring the swapper INTO the
  routing decision (the `resolveChatModel` installed-but-stopped fallback, the
  `model_warming` response contract) is #686's larger, still-open scope.

### Model Hotswap: residency invariant and swap rate bound (citadel #632, #687)

With `CITADEL_MODEL_HOTSWAP` on, an inference request for an installed-but-absent
engine triggers a swap (`internal/worker/swap.go`, `SwapManager`). Two rules
decide whether a swap may take VRAM from a resident engine, both in
`filterResidencyProtected`:

- The min-residency floor, and on top of it the **served-once invariant**: an
  engine that has had no request dispatched to it since it became ready is not
  evictable yet. A load that served nothing was pure waste, and the floor alone
  was shorter than a real load (a measured 78s load under a 60s floor), so a
  model could become evictable before it finished loading.
- The ceiling on that protection is the engine's **own load time** —
  `unservedResidencyCeilingLocked` prefers a load this node has actually MEASURED
  (`MeasuredLoad`) over the coarse table in `defaultLoadEstimate`.

An engine with no `readyAt` record — operator-started, or resident since before
the worker started — is protected by neither, deliberately: "we have no record"
must not read as "recently loaded", or a worker restart would make every
long-resident engine unevictable.

**The rate bound** (`checkSwapRate`, ledger in `swap_ledger.go`) counts swaps that
actually EVICTED something, so a node starting engines into its own free VRAM is
never limited. At the ceiling the node REFUSES: `SwapRateLimitedError` becomes a
job **failure** carrying `reason: "swap_rate_limited"`, not a `model_warming`
success — refusing honestly beats thrashing politely, and a node that quietly
warmed forever is indistinguishable from broken hardware.

The knobs are package vars (so tests need not sleep an hour) with the shipped
values pinned by `TestSwapAccountingDefaults` — change them there. The ledger is
in-process: the bound resets on restart, so a crash-looping worker can exceed it.

**LRU is real, not absent — what blocked it was durability, not the sort itself
(citadel #688).** `sortByLRU` (`swap.go`) already orders `preempt`'s candidates
least-recently-used first, and `touch` (called from every `EnsureResident`)
already keeps `m.lastUsed` current — but that map was purely in-process, so a
worker restart zeroed it and every engine looked equally "never used" until
fresh touches rebuilt real signal. `WithPersistence` (`internal/worker/
swap_persist.go`, a `SwapManagerOption`) closes that: `cmd/hotswap.go`'s
`newModelSwapManager` wires it to `<network.GetNodeConfigDir()>/swap-lru.json`
— the machine-convergent node config dir, NOT the invoker-scoped `configDir`
parameter also threaded through that function (see the `ConfigDir()` note
above for why those two differ) — so `lastUsed` survives the exact restart
that motivated this. Writes are debounced (`persistMinGap`, default 5s;
`touch` fires on every inference request, not just swaps) and best-effort
(a failed write is logged via `persistLogf`, never surfaced as a swap error);
reads degrade a missing/corrupt file to "no persisted recency" rather than
failing startup, mirroring the `TokenHashEntry.UnmarshalJSON` (#815)
lenient-parse reasoning. `pruneStaleLastUsedLocked` bounds the file against
entries for engines that are gone for good (uninstalled/renamed) — harmless to
eviction ordering either way (`PreemptInputs` only ever lists currently-running
engines as candidates) but otherwise unbounded growth once the map is durable.

`forget()` (called on every eviction) has never deleted `lastUsed` — verified
against git history before touching it — and #688 is explicit that it must
stay that way: an evicted engine should re-enter as "used recently", not
"never used", or an LRU-ordered candidate set thrashes (evict A, forget A, A
now looks coldest, evict A again before it ever serves). The comment on
`forget()` pins this so a future edit doesn't add that line back believing it
is symmetric cleanup with `readyAt`/`startedAt`/`servedAt` (which DO get
cleared there, deliberately — different lifecycle, not an oversight to match).
Promoting LRU to preemption's PRIMARY sort key (ahead of idle) is #688's
suggested fix #3 and is explicitly NOT part of this — only the two durability
defects blocking it are fixed here.

**Swap activity on the heartbeat (citadel #717).** `SwapManager.SwapStats()`
(swaps-per-hour, the evicting subset, the ceiling, and recent records) is
attached to the heartbeat as `NodeStatus.Swap` — `internal/status.SwapActivity`
— so "is this node thrashing?" is answerable without shell access. The swap
manager is constructed inside `buildNodeJobHandlers` (`cmd/nodejobs.go`), not at
the collector site, so `buildNodeJobHandlers` returns it as a second value and
`cmd/work.go` threads it to the collector via `nodeSwapManager`, an
`atomic.Pointer[worker.SwapManager]` — deliberately NOT a plain var like
`pubSubTransportFn`: the status-publisher goroutines (Redis, API, the `/status`
HTTP server) are already running by the time `buildNodeJobHandlers` returns and
`Store`s it, so `swapStatsFn`'s `Load()` on every heartbeat collection is racing
that one write. `pubSubTransportFn` gets away with a plain var only because it
is assigned before any reader goroutine starts — an earlier version of this
wiring copied that pattern without checking the ordering and had a real data
race as a result; if you touch this again, check which side of goroutine
startup the assignment falls on before choosing plain-var vs atomic.
`internal/status` cannot import `internal/worker` (worker already imports
status), so `SwapActivity`/`SwapRecord` are a hand-maintained mirror of
`worker.SwapStats`/`SwapRecord`; the projection is `swapStatsFrom` in
`cmd/work.go`, tested by `TestSwapStatsFrom` plus a reflection-based shape-parity
test (`TestSwapShapeParity`) that fails loudly if either side gains a field the
other doesn't mirror — e.g. #835's `Pulled`. Additive/omitempty: nil when no
swap manager is wired (hotswap disabled via `CITADEL_MODEL_HOTSWAP`, or no
config dir), so a hotswap-off heartbeat is unchanged. The control-center-only
collector (`cmd/controlcenter.go`) does NOT get this field yet — it also
doesn't wire `WorkerLiveness`/`PinnedServices`/`ModelHotswap`, a pre-existing
gap this doesn't widen; low-priority follow-up alongside the other
TUI-collector gaps noted under Service Preemption above.

**"Whether a swap pulled" is still NOT reported (#717 part 2, deferred).**
`SwapRecord` has no `pulled` field. For docker-based engines (vLLM, bonsai,
llama.cpp) a weights pull, if any, happens opaquely inside the container's own
startup during `docker compose up` — invisible to the Go code driving it — so
observing it honestly needs new engine-specific instrumentation (e.g. sampling
each engine's model-cache directory before/after `Start`), not just a return
value threaded through `SwapController.Start`. Native ollama IS observable
(`ensureOllamaModel` in `internal/jobs/service_handler.go` already knows whether
a pull ran), but that alone would cover a minority of swap targets. Per #717's
explicit instruction, a guessed field is worse than no field, so this stays
open rather than half-implemented; a follow-up issue should scope the
docker-side instrumentation before adding the field.

### Consume-Loop Watchdog, Self-Heal & Liveness (citadel #548)

A hung job handler must never silently stall the whole node. The wedge that
motivated this: a meeting/transcribe handler stuck in a `permission denied` +
"waiting for service to become ready" retry loop blocked the sequential consume
goroutine for 4+ hours. Heartbeats kept flowing from a separate goroutine, so the
node showed green while executing nothing. Three defenses (`internal/worker/`):

1. **Per-job watchdog (root fix, `deadline.go` + `runner.go`).** Every handler
   dispatch runs in its own goroutine bounded by a deadline (`executeWithDeadline`).
   On timeout the loop abandons the job and advances. `LegacyHandlerAdapter.Execute`
   now threads the worker's `context.Context` into `jobs.JobContext.Ctx` (landed
   after this section was first written; read via `JobContext.Context()`, which
   falls back to `context.Background()` for a caller that predates threading), so a
   context-cancel CAN unblock a legacy handler that actually selects on it —
   `internal/jobs`' three MEETING_JOIN wait loops are the first to do so
   (citadel#488). Most legacy handlers still don't select on it, though, so for
   THEM the goroutine+select here remains what keeps the loop alive on a timeout:
   the orphaned handler goroutine leaks until it finishes on its own. Timeout
   precedence: an explicit payload `timeout_ms` (backend budget, PR #552) wins;
   otherwise a **generous per-class fallback** applies so the wedge is bounded even
   when the backend sends no budget (the exact wedge condition). Classes:
   - Default 60min (`WORKER_JOB_TIMEOUT_SECONDS`): inference, shell, file, VNC,
     transcribe (its own single-shot self-bound is ~32min, comfortably under).
   - Long 4h (`WORKER_JOB_TIMEOUT_LONG_SECONDS`): `MEETING_JOIN`, `COBROWSE` —
     real human-session length; 4h catches a wedge without killing a live meeting.
   - Unbounded (no fallback cap): model pulls/downloads, builds, `SERVICE_START`,
     `INSTANCE_PROVISION`, `AGENT_UPDATE`, `WHATSAPP_PROVISION` — opaque long
     progress; a blanket cap would risk killing a legit job. Set either env to `0`
     to make that tier unbounded. A watchdog abandon is terminal: it routes to
     `source.Fail` (DLQ), not `Nack`, so a hung job isn't retried into a repeat
     wedge (esp. important in sequential mode).

2. **Self-heal monitor (`selfheal.go`, default ON).** Backstop for a wedge the
   per-job watchdog can't catch (outside a handler, or watchdog disabled). Reads
   the shared `WorkerState` and exits non-zero (systemd `Restart=on-failure`
   restarts clean) only on a clear loss of progress: no poll for
   `WORKER_SELF_HEAL_STALL_SECONDS` (default 600) **while `in_flight==0`** (a busy
   long job legitimately stops polling, so in-flight gates it), or a single job
   in flight past `WORKER_SELF_HEAL_STUCK_SECONDS` (default 18000/5h, above the
   long cap). Skips while draining (auto-update) and during a startup grace.
   Disable with `WORKER_SELF_HEAL=false`.

3. **Heartbeat liveness (`internal/status/` `WorkerLiveness`).** The heartbeat's
   `NodeStatus.worker` now carries `consuming`, `last_job_consumed_at`,
   `last_poll_at`, `in_flight` so the platform can flag a "green but wedged" node.
   Read them together: `consuming==false && in_flight==0` ⇒ wedged;
   `consuming==false && in_flight>0` ⇒ possibly a legit long job. `consuming`
   (poll freshness) is the alarm; `last_job_consumed_at` alone is ambiguous
   (stale on an idle node). Additive/omitempty; the same `WorkerState` pointer
   feeds both the runner and the collector.

**Remote restart break-glass (`cmd/agent_tools.go`).** `/agent/worker-restart`
(the `citadel_worker_restart` MCP tool) now performs a real restart — it exits
non-zero for the service manager to restart, but **only when
`managedByServiceManager()` is true** (systemd sets `INVOCATION_ID`); otherwise it
degrades to guidance so it can never exit into a node nothing will bring back. The
aceteam MCP side already POSTs to this endpoint (no wiring change needed); it
should render the new `{ok:true, restarting:true}` response instead of the old
"not yet supported" guidance.

| Variable | Default | Purpose |
|----------|---------|---------|
| `WORKER_JOB_TIMEOUT_SECONDS` | `3600` | Fallback per-job deadline for ordinary job types. `0` = unbounded. |
| `WORKER_JOB_TIMEOUT_LONG_SECONDS` | `14400` | Fallback deadline for long-session types (MEETING_JOIN, COBROWSE). `0` = unbounded. |
| `WORKER_SELF_HEAL` | on | Set falsey (`0`/`false`/`no`/`off`) to disable the self-heal monitor. |
| `WORKER_SELF_HEAL_STALL_SECONDS` | `600` | No-poll gap (with nothing in flight) before self-heal restarts. |
| `WORKER_SELF_HEAL_STUCK_SECONDS` | `18000` | Single-job in-flight ceiling before self-heal restarts. `0` = disabled. |

### Node execution model: claim/execute decoupling + bounded lanes (citadel #908, aceteam#8254)

`Runner.Run`'s fetch loop no longer conflates "claim a job" with "execute it"
(this is what `processJob` used to do inline, blocking `source.Next()` for the
whole handler run). Design doc:
[docs/design-node-execution-queue.md](docs/design-node-execution-queue.md). The
split, all in `internal/worker/runner.go`:

- **`claimJob`** runs SYNCHRONOUSLY in the fetch-loop goroutine: target-node
  filter, `WriteClaimed` (the claim-ack the backend's ~12s window waits on), and
  the cancellation check. So the claim-ack fires the instant a job is read,
  independent of how long execution takes or waits — the direct #908 fix.
- **`executeJob`** is everything after: handler lookup, the #825 GPU-slot gate
  (unchanged), the #548 watchdog/deadline (still started AT execution, never
  counting lane queue-wait), and terminal Ack/Nack/Fail. `finishSuccess` is the
  ONE success-terminal implementation (usage record → `WriteEnd` → `Ack`), reused
  by the inference queue-wait path so there is never a second place that could
  Ack without a terminal event (#559).

**Lanes** (`internal/worker/lane.go`, the `lane` type): a bounded `admit` channel
(non-blocking send in the fetch loop → the loop NEVER blocks; at the bound it
Nacks, the #825-shaped transparent retry) plus a bounded `exec` channel (all
waiting happens inside the spawned goroutine). Two instances, constructed in
`NewRunner`:

- **unbounded lane**, exec-concurrency **1**, for `serializedLaneJobTypes`
  (`deadline.go` — the authority, pinned by `TestSerializedLaneJobTypes`; it is
  `unboundedJobTypes` PLUS `MODULE_SET`/`SERVICE_STOP`/`APPLY_DEVICE_CONFIG`, the
  manifest/lockfile writers that aren't unbounded). Exec-concurrency 1 REPRODUCES
  today's implicit single-writer safety over the unlocked
  `citadel.yaml`/`modules.lock` read-modify-write paths EXACTLY — that is why v1
  needs no manifest locking (Phase 3 in the doc is deferred). `needsSerializedLane`,
  not the routing check, is where a new manifest writer is added — every job type
  that does a full read-modify-write of `citadel.yaml` (incl.
  `ConfigHandler.updateManifest` behind `APPLY_DEVICE_CONFIG`, which is neither
  gpu-bound nor long-session, so absent from this set it would fall to the inline
  branch and corrupt the manifest concurrently with a lane writer) must be a member.
- **inference lane**, exec-concurrency = `GPUTracker.Total()`, for
  `gpuBoundJobTypes`, and ONLY when a real discrete GPU exists (nil when
  `GPUTracker` is nil or `Total()<1` — a GPU-less node keeps the #903 inline
  fallback). On queue-wait exceeded (`runLaneJob`'s bounded select,
  `WORKER_INFERENCE_QUEUE_WAIT_SECONDS`) it returns the EXISTING `model_warming`
  success (the platform already retries it — zero backend change), NOT a Nack.
  The in-`executeJob` GPU-slot gate is retained but is effectively unreachable in
  production (lane exec-cap == `Total()` keeps them in lockstep), so the
  queue-wait is the real backpressure — the aceteam#8254 fix.

**WorkerState now tracks executing separately from in-flight**
(`RecordJobExecuting`/`RecordJobExecuteDone`, alongside the unchanged
`RecordJobReceived`/`RecordJobDone`). `InFlight = queued + executing`;
`Queued`/`Executing`/`OldestExecutingAt` are additive. **Self-heal STUCK
(`selfheal.go`) now gates on `Executing>0` and reads `OldestExecutingAt`**, so a
job legitimately QUEUED for hours behind a long pull on the exec-1 lane does not
false-trip a restart; STALL still reads `InFlight==0`.

**Heartbeat `LaneActivity`** (`internal/status.NodeStatus.Lanes`, projected from
`worker.LaneSnapshot` by `cmd/work.go`'s `laneActivityFrom`, pinned by
`TestLaneShapeParity`): per-lane queued/executing/exec-capacity + `BusySince`
(stamped when executing hits capacity). Wired via `nodeRunner atomic.Pointer`
(the runner is built AFTER the status-publisher goroutines start — plain var
would race, the #717 lesson). The control-center collector does NOT get it yet,
consistent with the WorkerLiveness/Swap/Reservations gaps there.

Env: `WORKER_INFERENCE_QUEUE_WAIT_SECONDS` (default 120), `WORKER_UNBOUNDED_LANE_QUEUE`
(admission depth, default 8).

Deliberate counter change: a foreign-`target_node` job (Ack-and-skip) and a
pre-cancelled job no longer touch `processed`/`failed` (claimJob has no counters,
per the design's exit-path table) — before #908 those bumped `failed`/`processed`
respectively.

### GPU-slot gate is job-type-scoped, not global (citadel #825)

`executeJob`'s GPU-slot acquire (`internal/worker/runner.go`, guarding the
`gpuTracker.Acquire()` Nack branch; was `processJob` before #908) only runs for
job types `needsGPUSlot`
(`internal/worker/gpu_tracker.go`) says actually dispatch to a node-local GPU
inference engine. `needsGPUSlot`/`gpuBoundJobTypes` is the authority for that
set — `TestGPUBoundJobTypes` (`gpu_tracker_test.go`) pins its exact membership,
so read the test rather than trusting a doc copy of it. Everything NOT in that
set — `SERVICE_START`, shell, file, config, etc. — skips the gate entirely and
can never be Nacked by GPU contention.

This matters because `--max-concurrency` (`cmd/work.go`) can be set above the
node's GPU count, so more jobs can be in flight than GPU slots. Before #825, the
gate applied unconditionally: a non-GPU job racing concurrent inference jobs
could hit "no GPU slots available" and Nack with **zero published terminal
events** (the same-job-ID-redelivery reasoning at that Nack deliberately omits a
terminal publish — see the #559 note inline), reproducing #559's
"backend waiter times out, degrades to polling" symptom via GPU contention
instead of a stream-publish failure. `needsGPUSlot` is the authority for which
job types can reach that Nack at all; extend `gpuBoundJobTypes`, not the check
site, if another job type turns out to genuinely contend for engine VRAM.

### Long-session and GPU-bound jobs get a dedicated always-async lane (citadel #489, extended by #903 Stage 1)

`Runner.Run` (`internal/worker/runner.go`) dispatches a job whose type is in
`longSessionJobTypes` (`internal/worker/deadline.go` — MEETING_JOIN, COBROWSE)
on its own goroutine UNCONDITIONALLY, checked before the `concurrency > 1`
branch. It **also** dispatches a job satisfying `needsGPUSlot`
(`internal/worker/gpu_tracker.go` — `llm_inference`, `LLAMACPP_INFERENCE`,
`VLLM_INFERENCE`, `OLLAMA_INFERENCE`) the same way, but ONLY when
`r.gpuTracker` is non-nil — see the nil-tracker gate below before assuming
every GPU-bound job takes this lane. This is a dedicated lane, not a special
case of the semaphore pool: it never touches `sem`, so it can never occupy a
pool slot either, and it applies regardless of `--max-concurrency`.

**#903 Stage 1 (GPU-bound extension):** on a `--max-concurrency=1` GPU node
(the node-1297 incident node — a discrete GPU, so `cmd/work.go` constructs a
non-nil `r.gpuTracker`), a long-running `llm_inference` job used to run INLINE
in the fetch loop exactly like any other job — blocking `source.Next()` until
it returned, so a *targeted* dispatch to that node (e.g. `FILE_READ_BYTES` in
a `file_parse` pipeline that alternates file reads and GPU jobs) was never
even claimed within the backend's short claim-ack window and fast-failed as
"unreachable," even though the node was healthy and simply busy. On a node
WITH a tracker, this is a **fetch-loop fix, not a concurrency change**:
`r.gpuTracker` (#825) still gates actual execution inside `executeJob` (was
`processJob` before #908's claim/execute split) exactly
as before — it admits up to its slot count and Nacks (non-terminal,
redelivered) the rest. What changes is which jobs can even REACH that gate on
a maxConcurrency=1 node: previously a second GPU-bound job was never fetched
while the first ran, so the gate was unreachable there; now it's reachable,
and `TestRunnerGPUBoundAsyncLaneStillNacksUnderSlotContention`
(`runner_test.go`) pins that the gate's own behavior is unchanged under that
newly-reachable contention.

**The nil-tracker gate (caught by PR review before merge, not part of the
initial patch).** `r.gpuTracker` is only constructed when
`platform.GetGPUCountSimple() > 0` (`cmd/work.go`) — i.e. a discrete NVIDIA
GPU. Citadel explicitly supports a first-class node class with NO discrete
GPU that still serves GPU-bound-typed inference (CPU-only, native ollama,
Apple Silicon; #606/#612/aceteam#6634), where `r.gpuTracker` is nil. On such a
node nothing else throttles concurrent GPU-bound dispatch — the sequential
fetch loop (a GPU-bound job falling through to the inline `else` branch) was
the ONLY thing serializing inference there, accidental but real backpressure
against a single CPU-serving engine. An unconditional async lane for
GPU-bound jobs would remove that backpressure in the DEFAULT config
(`MaxConcurrency: 1`, no tracker), letting N concurrently-arriving inference
jobs hit one engine with zero throttle. The dispatch condition is therefore
`needsGPUSlot(job.Type) && r.gpuTracker != nil`: a tracker-less node keeps the
old sequential fallback for GPU-bound jobs, preserving its only safety net.
`TestRunnerGPUBoundJobsSequentialWithoutTracker` (`runner_test.go`) pins this
— it fails against the unconditional-async version of the fix. Real admission
control for the tracker-less node class (a semaphore/lock actually sized to
that node's serving capacity, not this GPU-count-shaped tracker) is
out-of-scope follow-up work, not part of this fix.

The general/unbounded case — every OTHER job type (`SERVICE_START`, model
pulls, `MODULE_SET`, ...) still blocking the fetch loop inline on a
maxConcurrency=1 node — is a separate, deliberately out-of-scope Stage 2
design issue (decoupling job receipt from execution generally).

This matters because `cmd/work.go` defaults `maxConcurrency` to 1 on a
GPU-less node — meeting nodes are typically GPU-less — and before #489 a
`concurrency == 1` job ran INLINE in the main loop, blocking the next
`source.Next()` poll until it returned. A 4h `MEETING_JOIN` (the long-tier
deadline; see Consume-Loop Watchdog above) therefore monopolized the node's
only slot and starved every other job (deploys, shell, transcription) for up
to 4 hours — head-of-line blocking, not a wedge (the #548 watchdog/self-heal
machinery doesn't catch this: the loop was doing exactly what it was told,
just serially).

The async-dispatched job still runs through the SAME claim/execute path
(`claimJob`+`executeJob`, `processJob` before #908) every other job takes, so the per-job watchdog/deadline, terminal-event
publishing, cancellation, `WorkerState` in-flight accounting, and DLQ/ack
semantics are unchanged — only the sequential/semaphore gate is bypassed.
Mirrors the job-type-scoped pattern `needsGPUSlot`/`gpuBoundJobTypes`
established for the GPU-slot gate (#825, directly above): a `map[string]struct{}`
membership check decides which job types skip a concurrency gate entirely,
rather than threading a new special case through the gate itself.
`TestRunnerLongSessionJobDoesNotBlockOtherJobs` (`runner_test.go`) pins the
regression directly: with `MaxConcurrency: 1`, a blocked MEETING_JOIN handler
does not prevent a queued SHELL_COMMAND from completing.
`TestRunnerGPUBoundJobDoesNotBlockOtherJobs` is the #903 analogue for a
blocked `llm_inference` handler and a queued `FILE_READ_BYTES` (on a node
WITH a `GPUTracker` — see the nil-tracker gate above);
`TestRunnerGPUBoundJobsSequentialWithoutTracker` is its nil-tracker
counterpart, asserting the OPPOSITE (no async dispatch) when none is set.

**The async lane exposed a SEPARATE, pre-existing single-instance assumption
that #489's PR review caught before merge: the host meeting-browser profile.**
Before #489, a maxConcurrency=1 node's sequential dispatch INCIDENTALLY
enforced "at most one MEETING_JOIN at a time" as a side effect of serializing
every job. Making MEETING_JOIN always-async removes that incidental
serialization, so two overlapping meetings (back-to-back/overlapping calendar
joins) can now genuinely run concurrently — and `hostMedia`
(`internal/jobs/meeting_media.go`) launches Chrome against a FIXED, shared,
persistent `--user-data-dir` (`platform.preparePersistentProfileDir`,
`internal/platform/meeting_browser.go`), which Chrome locks to ONE process: a
second launch against an already-open profile silently FORWARDS into the
first, still-live instance instead of starting independently — the forwarded
launch can navigate/click inside the FIRST meeting's browser, disrupting an
in-progress call, while the second caller's own CDP port never comes up (an
opaque `waitForCDPReady` timeout).

The fix is a process-wide guard on the RESOURCE itself, not the dispatch lane
(so it protects against collision from ANY caller, not just the async lane):
`acquireMeetingProfileLock`/`meetingProfileLockFor` (`internal/platform/
meeting_browser.go`) key a `sync.Mutex` by the resolved, absolute profile
directory, mirroring `GetCobrowseManager()`'s process-wide-singleton pattern
but keyed rather than a single instance (the meeting bot legitimately supports
more than one profile dir via `EnvMeetingProfileDir`/per-browser override).
`MeetingBrowser.Start()` claims it with `TryLock` — never a blocking `Lock` —
so a collision fails FAST with a clear "meeting bot profile already in use"
error rather than blocking up to the 4h long-session deadline (a queued
meeting would be over by the time its turn came) or silently proceeding into
Chrome's own forwarding collision. `closeLocked()` releases it (idempotently —
the release func is `sync.Once`-wrapped), so Close() on every exit path
(success, join-flow error, the `defer media.Close()` in
`MeetingJoinHandler.Execute`, which fires on cancellation and panic-unwind
alike) frees the profile for the next meeting.

**Scope: HOST backend only.** The container backend (`containerMedia`,
meetingd) is NOT exposed the same way — it already enforces "one meeting per
node" server-side (meetingd's own session state returns 409 on a second
`POST /sessions`, and `containerMedia.createSession` surfaces that as an
immediate, clear error), so it needed no change here.

`TestAcquireMeetingProfileLock`/`TestAcquireMeetingProfileLock_Concurrent`/
`TestAcquireMeetingProfileLock_ReleaseIsIdempotent`/
`TestMeetingBrowser_CloseReleasesProfileLock` (`meeting_browser_test.go`) pin
this hermetically — the lock-acquisition seam is tested directly, with no real
Chrome/Xvfb launched.

**The lock is acquired before ANY other Start() setup work, not just before
Xvfb/Chrome exec (citadel#896 hardening).** `Start()` originally called
`findChromium()` and `preparePersistentProfileDir()` (a real `mkdir`/`chmod`)
BEFORE `acquireMeetingProfileLock` — those two still ran unconditionally even
against an already-locked profile, so only Xvfb/Chrome were actually skipped
on a collision, not the doc's stated "before touching anything". Reordered so
the lock (keyed by the same `resolveMeetingProfileDir` resolution
`preparePersistentProfileDir` uses internally, so the key is unchanged) is
claimed first and everything else — including the two calls above — sits
inside the deferred-release span. This closes a real gap
(`TestMeetingBrowser_CloseReleasesProfileLock` hand-constructs a
`MeetingBrowser` with a pre-set `profileLockRelease` and so would stay green
even if a future refactor dropped the `acquireMeetingProfileLock` call from
`Start()` entirely) and is what makes
`TestMeetingBrowser_StartAcquiresProfileLockFirst` — which calls the REAL
`Start()` against a pre-locked profile and asserts it fails fast with no
Chrome/Xvfb process and no profile-dir creation — hermetic on a host with no
Chromium/Xvfb installed at all.

**Self-heal STUCK detection hardening (non-blocking review WANT, also fixed
here).** `LivenessMonitor`'s STUCK check (`internal/worker/selfheal.go`) used
to measure `WorkerState.LastJobAt` — the time the MOST RECENT job started,
overwritten by every `RecordJobReceived()` call. On a maxConcurrency=1 node
with the async lane, a wedged MEETING_JOIN can sit in-flight for hours while a
stream of ordinary SHELL_COMMAND jobs completes beside it; each one used to
reset `LastJobAt`, so the STUCK ceiling would never trip for the wedged
meeting as long as short jobs kept arriving. `WorkerState` now also tracks
`oldestInFlightUnixNano` (surfaced as `WorkerSnapshot.OldestInFlightAt`): the
time the CURRENT in-flight streak began (the last `0 -> >0` transition),
untouched by jobs that start after it and cleared only when in-flight fully
drains back to 0. `RecordJobReceived`/`RecordJobDone` serialize the
increment/decrement against this conditional stamp under a small
`inFlightMu` — without that serialization, a decrement-to-zero and a
concurrent increment-to-one could apply their stamps out of order and
silently erase a legitimate in-flight start time. The STUCK check now reads
`OldestInFlightAt`, not `LastJobAt` (later narrowed to `OldestExecutingAt`,
its `Executing`-scoped analogue — see #908 above).
`TestLivenessMonitorCheck_StuckUsesOldestInFlightNotLastJob`
(`selfheal_test.go`) pins the fix by driving real `WorkerState` transitions
(a wedged 6h job plus an interleaved short job) through the actual
`RecordJobReceived`/`RecordJobDone` API.

**`Snapshot()` reads each pair under that same mutex too (citadel#896).**
`inFlight`/`oldestInFlightUnixNano` (and the `executing`/
`oldestExecutingUnixNano` pair `inFlightMu`'s comment above describes) were
originally read here as two INDEPENDENT lock-free atomic loads apiece, so a
snapshot could land between a writer's count bump and its conditional
timestamp store/clear and observe a torn pair (e.g. `Executing==1` with
`OldestExecutingAt==nil`) — harmless for the pair the STUCK check actually
reads today (it already gates on both being set), but with no such safety
net for `InFlight`/`OldestInFlightAt`, which is surfaced on the heartbeat
directly with no internal gating. `Snapshot()` now takes `inFlightMu`/
`executingMu` around each paired read, closing the window instead of relying
on gating to keep it harmless.
`TestWorkerState_SnapshotInFlightPairingIsConsistent` (`state_test.go`) pins
the resulting invariant (`InFlight>0 <=> OldestInFlightAt!=nil`, and the
`Executing` analogue) under concurrent writers.

### Node self-identity and the missing numeric fabric ID (`citadel whoami`, aceteam #8139)

`citadel whoami` (alias `id`, `cmd/whoami.go`) answers "am I on a Citadel node,
and what is its identity?" from local/persisted state and caches the answer at
`<node config dir>/identity.json`. `gatherIdentity` (`cmd/whoami.go`) is the
authority for which sources feed which field — read it, don't assume.

**A real persistence path for the numeric AceTeam fabric node ID now exists
(citadel-Go side), but no backend process sends one yet, so it still reads
empty on every real node.** See the "Node identity persistence + signed AEP
receipt" section above for the implementation
(`DeviceConfig.FabricNodeID`/`config.DeviceCreds.FabricNodeID`,
`nexus.TokenResponse.FabricNodeID`, `saveFabricNodeIDToConfig`) and
[docs/design-node-identity-receipts.md](docs/design-node-identity-receipts.md)
for the design. `gatherIdentity` prefers `DeviceConfig.FabricNodeID` over the
legacy `SSHSyncConfig.NodeID` slot ("Node ID in AceTeam platform",
`internal/nexus/sshkeys.go`), which is kept only as a documented last-resort
fallback: its writer (`SaveSSHSyncConfig`) has zero non-test callers AND, if
ever wired up naively, would clobber `api_token` to `""` for a caller that
supplies only the node ID (see the design doc §1a — this is exactly why the
new persistence uses `DeviceConfig`/`config.yaml` instead). The only
fabric-adjacent identifier ALSO resolvable LIVE (no persistence needed) is
the Headscale/mesh numeric node ID, the same saved-state `VerifyOrReconnect`
+ `GetGlobalStatus` probe `citadel status` already performs. Full trail on
the original gap, including why `internal/devicemode`'s superficially-similar
`NodeUID` is NOT this (different identity, different — non-overlapping —
population of hosts):
[docs/whoami-fabric-id-gap.md](docs/whoami-fabric-id-gap.md).

### Safe node targeting: `--node-dir`, `--dry-run`, `--expect-node` (citadel#853, #854)

Motivated by a real incident: a subagent smoke-testing `citadel module` had set
an isolated `$HOME` for one shell call, but a LATER call in the same session
ran without it (shell state does not persist between an agent's tool calls),
so `citadel module stop/restart` fell through to the default
`$HOME`/`ConfigDir` resolution and cycled a LIVE production container.

**`--node-dir` (also `CITADEL_NODE_DIR`, global persistent flag,
`resolveNodeDirOverride` in `cmd/nodedir.go`)** redirects manifest + services-dir
resolution. It is wired at the single choke point almost every manifest command
already goes through — `findAndReadManifest`/`findOrCreateManifest`
(`cmd/manifest.go`) — so it is honored consistently by every command that
reads the node manifest through those two functions (module stop/start/
restart, `run`, `stop`, `status`, `services`, `logs`, module/catalog install,
...) with no per-command checklist to keep in sync or fall out of. When set,
the `$HOME`/`platform.ConfigDir()`/global-`config.yaml` indirection is bypassed
ENTIRELY: `citadel.yaml` is read directly from the override dir, and a missing
manifest there errors rather than silently falling back to `$HOME`.

**Scope — what it does NOT redirect (corrected by the citadel#856 review — the
original PR overclaimed isolation here; read this before assuming
`--node-dir` alone makes a target "safe to run against"):** network/mesh
state (`internal/network.GetStateDir`/`GetNodeConfigDir`, the tsnet identity),
the module lockfile (`catalog.LockfilePath`, still hardcoded to
`platform.ConfigDir()`), anything under `nodevault`/`worklock` — and, the
sharp one, **Docker container identity**. Every embedded compose file pins a
GLOBAL `container_name: citadel-<svc>` (`services/compose/*.yml`), unaffected
by which directory citadel materialized/read the compose file from. On a
machine whose Docker daemon ALSO runs a real citadel node — the exact
production topology `--node-dir` exists to be used safely against —
`citadel run vllm --node-dir /tmp/x` materializes a `vllm.yml` in `/tmp/x`
naming the SAME `citadel-vllm` container the real node manages. `--node-dir`
alone does not stop a compose action against that file from touching the real
container; see "Compose-project scoping" below for what closes (part of) that
gap, and what still doesn't.

This means `citadel module install <source>` / `module update` / `catalog
install` (which write BOTH the manifest and the lockfile) are deliberately NOT
override-aware end to end: `refuseIfLockfileWriteUnsupported`
(`cmd/nodedir.go`), called from their RunE functions, REFUSES rather than
silently splitting a module's manifest entry (override dir) from its
provenance (real machine's `modules.lock`) — half-honoring an override here
would be worse than not honoring it. `citadel work` refuses the SAME way, but
at boot rather than per-call: its reconcile loop and MODULE_SET handler drive
the identical `liveModuleOps.Install`/`Uninstall` from inside a long-running
process, so a per-job refusal there would just be an infinite quiet converge
failure (a silently-dark node) rather than a fixable operator error — `runWork`
(`cmd/work.go`) checks the override once, before any job source connects, and
exits loudly if set. `citadel module stop|start|restart` never touch the
lockfile (only the `desired_status` marker + compose up/down), so they — and
the service startup `citadel work` does before reaching its reconcile loop —
are fully override-aware for MANIFEST resolution. Threading the override into
the catalog lockfile path is a deferred follow-up.

**Compose-project scoping (citadel#856).** `composeProjectOverride`/
`composeArgsWithProject` (`cmd/nodedir.go`) derive a `-p`/`--project-name`
compose flag from a hash of the resolved override directory, and every
compose invocation `module stop|start|restart`/`run`/`stop` drive
(`composeCommandFor`, `startServiceComposeArgs`, `stopComposeArgs`) applies it
— when no override is active this is a byte-identical no-op, preserving the
#528 no-`-p` default exactly. Under an override, this converts the failure
mode from "silent" to "safe and loud": `down` selects containers by the
`com.docker.compose.project` label, so it cannot match a DIFFERENT project's
container and becomes a no-op; `up` on a `container_name` already owned by a
different project fails outright (`composeFailureMessage` detects this and
names the cause instead of leaking raw docker output).

**Container-name namespacing for EMBEDDED services (citadel#860).** #856
alone was project-identity isolation, not container-identity isolation: two
override dirs both materializing the same `services.ServiceMap` entry still
pinned the identical `container_name: citadel-<svc>` and collided with each
other (loudly, but still) on `up`. `embeddedContainerName` (`cmd/nodedir.go`)
closes that: under an active override it returns `citadel-<hash>-<svc>`,
`<hash>` the SAME value `composeProjectOverride` derives for `-p`, so the
compose project and the container it starts always agree on which override
owns them. `cmd.ensureComposeFile` / `internal/jobs.ServiceHandler.
ensureEmbeddedComposeFile` write that name at materialization time — both
sites reconcile it on EVERY call, not just first-write, because the
"the .yml already exists, leave it alone" fast path predates #860 and would
otherwise leave a file materialized before an override was set (or by a
pre-#860 binary) carrying the unnamespaced name forever, which is worse than
cosmetic: a later `up` against that stale file targets the REAL node's global
`citadel-<svc>` container, and #856's "safe and loud" guarantee only holds
while that real container currently exists — `internal/compose.
EnsureNamespacedContainerName` owns the exact reconcile-vs-refuse rule
(rewrite the unnamespaced default in place; refuse loudly on anything else,
e.g. a hand-edited file or a different override's hash). `containerIsRunning`
(`cmd/module_ops.go`) and `dryRunContainerNames`'s fallback
(`cmd/module_control.go`) resolve the same namespaced name via
`embeddedContainerName`/`resolveModuleContainerName`, so materialization and
lookup can never disagree. Scoped to `services.ServiceMap` entries ONLY (the
ServiceMap-membership check every call site applies first): catalog/
third-party module compose files author their own `container_name` and are
NOT namespaced by `--node-dir` — a documented follow-up-of-the-follow-up, not
done here. A few read-only/display-only sites were deliberately left
un-namespace-aware (informational only, never decide what to start/stop):
`internal/status/footprint.go`'s container-name candidates (feeds `citadel
services`/`citadel status`, which — since those ARE override-aware for
manifest resolution — could misattribute the REAL node's footprint to the
override node's row; nothing acts on this, since auto-stop only runs inside
`citadel work`, which refuses `--node-dir` outright), and the TUI control
center's footprint enrichment (`cmd/controlcenter.go`), consistent with the
pre-existing TUI-collector gaps noted under Service Preemption and Model
Hotswap above.

**`citadel service diagnose` refuses, rather than silently disclosing the
REAL node's container, under a not-yet-materialized override (citadel#863,
follow-up to the paragraph above).** Even though diagnose is read-only
(inspect + log tail, never start/stop), its pre-materialization fallback
(`resolveComposeContent` returning the embedded `services.ServiceMap`
template, or nothing at all) left `in.ContainerName` at the bare
`citadel-<name>` convention — and `servicediag.Diagnose` inspects and tails
logs from that name UNCONDITIONALLY, whether or not compose content was
found. Under `--node-dir` before a service has ever been started under that
override, that silently rendered the REAL node's container state and log
tail to an operator who believed they were diagnosing an isolated override
service — a disclosure/misdiagnosis hazard, not a helpful fallback.
`diagnoseNodeDirRefusalError` (`cmd/service_diagnose.go`) closes this the
same way `stopServiceByContainer` does (`cmd/stop.go`, citadel#856 review):
refuse outright when an override is active and `resolveComposeContent`'s
source isn't `"manifest"` (i.e., no compose file has actually been
materialized inside the override's resolved config dir yet — `"embedded"`
or `""` both mean the container name in play is still the unnamespaced
global default). A `"manifest"` source is safe to proceed with because
`configDir` is itself `--node-dir`-aware and citadel's own materialization
already namespaces the `container_name` (citadel#860) the moment the service
is actually started under that override. No-op when no override is active.

Two RAW (non-compose) container-name paths cannot be protected by `-p` at all,
because plain `docker inspect`/`stop`/`rm` take a bare name with no
project-scoping mechanism: `startService`'s pre-flight "does this container
already exist" check (`cmd/service.go`) SKIPS entirely under an active
override — the project-scoped `up -d` alone is sufficient for idempotency and
fails loudly on a real conflict instead of this code reading/deleting the
wrong container first — and `stopServiceByContainer`'s fallback (`cmd/stop.go`,
used when a service isn't in the resolved manifest) REFUSES outright under an
active override rather than stop/remove a container it cannot verify belongs
to this node.

**`citadel module stop|start|restart --dry-run`** (`cmd/module_control.go`)
prints the resolved node dir, the compose file, the compose project (when an
override is active), and the container name(s) — read from the compose
file's own `container_name:` fields where possible (`dryRunContainerNames`),
not just the `citadel-<name>` convention (see the Service Management section
above on why that convention alone isn't trustworthy) — and returns before
`newLiveModuleOps` is ever constructed, so nothing is touched.

**`--expect-node <name-or-id>` is the actual cross-node guarantee — not
`--node-dir` alone.** It refuses (fails CLOSED) unless the resolved node's
identity matches, checked BEFORE anything else runs — including before
printing a `--dry-run` plan, since a preview a real run would refuse is the
wrong direction of error — and regardless of what the Docker/compose layer
above would have done. It reuses citadel#844's identity resolution
(`gatherIdentity`, `cmd/whoami.go`) rather than reinventing node-identity
logic; `nodeIdentityMatches` compares case-insensitively against the manifest
node name (itself `--node-dir`-aware), OS hostname, and the live Headscale
mesh node ID. If a caller genuinely needs "refuse unless this is definitely
the intended node" (rather than "a friendlier failure mode if it isn't"),
`--expect-node` is that primitive; `--node-dir` by itself is a targeting
convenience with improved failure modes, not isolation.

`citadel whoami --node-dir ...` deliberately skips writing `identity.json`
(which lives at the REAL machine's `network.GetNodeConfigDir()`, not the
override) rather than caching an overridden node's identity into the real
machine's cache file.

### Meeting-bot wait-loop cancellation (citadel#488, cancellation half)

Every poll loop in the MEETING_JOIN join/wait chain now `select`s on
`ctx.Context().Done()` each tick instead of a bare `time.Sleep`: the pre-join
click loops (`pollForJoinClick`, Meet; `pollForTeamsJoinClick`, Teams — both
run BEFORE their platform's admission wait), the admission waits
(`waitUntilAdmitted`, Meet; `waitUntilTeamsAdmitted`, Teams), and the two
in-call wait loops (`waitForMeetingEnd`, the plain path; and
`waitForMeetingEndInteractive`, `internal/jobs/meeting_interactive.go`, the
DEFAULT loop since `config.Meeting.StreamingEnabled` defaults true) — the
latter two alongside the existing recorder-death signal (citadel#490) they
already watched. All are in `internal/jobs/meeting_join.go` except the two
Teams functions (`meeting_join_teams.go`) and the interactive loop. On
cancellation each returns within one poll tick with a distinct error
(`errors.Is`-compatible with `context.Canceled`/`context.DeadlineExceeded` via
`%w`); the two in-call loops additionally report a distinct `"cancelled"`
reason rather than the misleading `recorder_died`/`recording died` shape —
`meeting_join.go`'s `Execute` branches on `outcome.endReason == "cancelled"` to
log and wrap it distinctly. Cleanup is unaffected either way: `Execute`'s
`defer media.Close()` and the `media.StopRecording()` call after
`runMeetingLoop` returns both already ran regardless of *why* the loop
returned (recorder death, cancellation, or a clean end) — this fix only makes
that return happen promptly instead of after up to `p.maxDuration()` (default
4h) or, for the pre-join/admission loops, `joinButtonTimeout`/`admitTimeout`.

**What actually cancels the context in production, and what still doesn't:**
`cmd/work.go`'s own top-level `signal.Notify` goroutine (separate from
`internal/worker.Runner.Run`'s internal one) calls the root `cancel()` on
SIGINT/SIGTERM; that root context is what flows into
`Runner.Run(ctx)` -> `executeJob(ctx, job)` (was `processJob` pre-#908) ->
`LegacyHandlerAdapter.Execute`'s
`jobs.JobContext{Ctx: ctx}` -- so it reaches these loops even when
`Runner.Run`'s OWN internal signal handling can't (in sequential mode,
`concurrency<=1`, a non-laned job's `executeJob` is called inline in the poll
loop's `select`,
so `Runner.Run`'s internal `sigs` case can't be reached — and thus can't itself
call `cancel()` — until the in-flight job returns; `cmd/work.go`'s separate
goroutine has no such gate). A **drain** (`Runner.Drain`, e.g. the auto-updater
readying a new binary) is a DIFFERENT signal and deliberately does NOT cancel
any job's context — `internal/update.AutoUpdater.runOnce` only stops new job
pickup and waits (bounded, deferring to the next cycle on timeout) for natural
completion, matching the general worker convention that a drain waits rather
than kills. This fix does not change that: a live meeting still blocks an
auto-update until it ends on its own or the duration cap trips. Only true
process-level cancellation (SIGINT/SIGTERM, or a future explicit per-job
cancel) is observed here.

**A side effect of returning promptly: `backupAndPrune`'s upload now reliably
no-ops on a cancelled shutdown.** `Execute` still calls
`h.backupAndPrune(ctx, ...)` after `runMeetingLoop` returns, and
`uploadAudioBackup` (`meeting_audio_backup.go`) derives both its transcode and
HTTP-upload deadlines from the SAME job `ctx.Context()` via
`context.WithTimeout`. On the cancellation path that parent is already Done,
so the derived contexts are too, and the docker-exec/HTTP calls fail
immediately — logged as an ordinary "non-fatal" backup failure, same as any
other transcode/upload error. This was already possible before this fix (a
cancellation reaching Execute at all was the rare/slow case); this fix just
makes it the COMMON case on shutdown. No data loss: `backupAndPrune`'s
`uploadConfirmed=false` path protects the local WAV from the retention sweep
either way, and the WAV is what `citadel#488`'s orphan-reaper half (next
paragraph) would need to reconcile regardless.

**Orphan-reaper half is a separate, not-yet-built follow-up.** A SIGKILL (the
watchdog's grace-period force-exit, `citadel#312`, or an external `kill -9`)
still skips every defer, so the null-sink module, Xvfb display, Chrome
process, and `citadel-meeting-profile-*` temp dir it left behind are not
reclaimed by anything in this fix — cobrowse's `cobrowse_orphan.go` reaper
only sweeps the fixed cobrowse `:9222`, not the meeting bot's random CDP port
or `citadel_meeting_*` sink naming. Tracked separately under citadel#488.

### Manual `citadel update install` vs the two automatic update paths (citadel #454)

Three code paths swap the citadel binary; only two of them restart the process
that needs to run it. `internal/worker/agent_update.go` (the `AGENT_UPDATE` job)
and `internal/update/autoupdater.go` (the hourly background check) both run
*inside* the worker they update, so they drain in-flight jobs, wait for idle,
and `syscall.Exec`-restart themselves. `cmd/update.go`'s `installUpdate()`
(`citadel update install`, run by an operator) is a separate, short-lived CLI
process — swapping the on-disk binary there does nothing to the already-running
managed worker, which keeps executing the pre-swap code indefinitely
(citadel#454's split-brain incident).

**`service.ActiveManagedUnit`** (`internal/service/detect.go`, Linux; a no-op
stub on `!linux`) is the authority for "is a managed citadel service running
**on this host**", not "is *this process* service-managed". It scans the
citadel-owned systemd units on disk (`candidateManagedUnits`,
`isCitadelManagedUnit` — the same enumeration `RematerializeManagedUnits`
already uses) and their live `systemctl` state directly, so it gives the right
answer from an unrelated process. `managedByServiceManager`
(`cmd/agent_tools.go`) answers the different question "is *this* process
running under systemd" via `INVOCATION_ID`, and is only correct from inside
the worker being restarted — using it (or a `CITADEL_SERVICE` env check) from
`citadel update install` would read the operator's own SSH shell, which never
inherited the worker's environment, so the gate would silently never fire in
exactly the scenario #454 reported. `resolveManagedServiceRestartTarget`
(`cmd/update.go`) layers `ActiveManagedUnit` (the only signal that sees the
install.sh/packer fleet unit, `citadel-worker.service` — how citadel actually
ships on most nodes) with the cross-platform `service.Manager.Status()` (the
only signal on macOS/Windows, and for a `citadel service install`-managed
Linux node).

`citadel update install` now warns loudly by default when a managed service is
detected, and restarts it only with an explicit `--restart` flag. That restart
is a blunt `systemctl restart` / `Stop()+Start()` with **no drain** — unlike
the two automatic paths, it can drop in-flight jobs. This is an accepted
tradeoff for an interactive, explicitly-opted-in flag: the CLI process has no
way to observe the *other* (worker) process's in-flight job count, which is
exactly why draining is owned by the paths that run inside that process.
Known, accepted gaps: `ActiveManagedUnit` returns only the first active unit
found, so a host running both a fleet unit and a `citadel service install`
unit gets one warned/restarted and the other left stale (a narrower version of
the same split-brain); and detection does not verify the swapped binary is the
one the unit's `ExecStart=` actually runs (a dev binary at a different path
would produce a spurious warning).

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

**Reading one field off the running worker**: use `GET /worker`, not `GET /status`.
`/status` runs a full collection (docker stats per running service plus
`nvidia-smi`), which on a busy gateway node takes seconds; `/worker` serves the
same `worker` envelope from an in-memory snapshot and shells out to nothing. A 404
on `/worker` means the *serving* worker predates it (the probing binary is
whatever `citadel status` you just ran; the serving one is a long-lived `citadel
work`), so callers fall back to `/status`. See `probeWorkerPubSubTransport`
(#735). Related: `httpGetBodyErr` (`cmd/work_attach.go`) derives its deadline from
the caller's `client.Timeout`; it used to impose its own constant instead, so
raising a caller's timeout was a silent no-op.

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
- `ConfigDir()` - **User-local by default, system-wide only for root.** Not root
  (the common case, including a `User=`-scoped systemd worker): `~/.citadel-cli`
  on Linux/macOS. Windows: `%LOCALAPPDATA%\Citadel`, *regardless of elevation*.
  Root on Linux/macOS: `/etc/citadel` or `/usr/local/etc/citadel`, but only after
  trying to resolve the invoking user's `~/.citadel-cli` via `HOME` and
  `SUDO_USER` (so `sudo citadel ...` keeps using the config `citadel init` wrote).
  `platform.resolveConfigDir` is the authority — read it before assuming a path.

  This entry previously claimed the root paths unconditionally. That is wrong for
  almost every real invocation, and it is not a harmless docs nit: anything that
  writes under `ConfigDir()` and reasons about writability from the doc will
  conclude it needs root and quietly no-op. It nearly did exactly that to #696's
  pidfile.

  **`ConfigDir()` is invoker-scoped; do not use it for cross-context state.**
  Anything one invocation context writes and another reads (a systemd-root
  `citadel work` writing, an interactive non-root `citadel status` reading) must
  use `network.GetNodeConfigDir()` instead — the machine-convergent node config
  dir, hardened for exactly this divergence by #383 and already used by
  `worklock.LockPathForStateDir`. `ConfigDir()` silently resolves to different
  directories for those two callers, so the reader sees nothing and reports
  "unknown" forever rather than erroring. #726's cross-process heartbeat
  freshness marker (`internal/heartbeat/marker.go`) uses `GetNodeConfigDir()`
  for this reason, not `ConfigDir()`.

  **Device/org config (`device_api_token`, `org_id`, `org_name`, `user_email`,
  `user_name`, `redis_url`, `aceteam_api_key`) is machine-convergent state too
  (#845), and had the identical bug until fixed:** `cmd.getDeviceConfigFromFile`
  (the single read path behind every `deviceConfig := getDeviceConfigFromFile()`
  call site) and its writers (`saveDeviceConfigToFile`, `saveRedisURLToConfig`)
  now agree on `network.GetNodeConfigDir()`, with a read-only fallback to the
  legacy `ConfigDir()` location for a node registered before #845.
  `cmd.deviceConfigDirs` (`cmd/devicecreds_hooks.go`) is the authority for the
  search order — read it before assuming a path. `internal/config` is a leaf
  that must not import `internal/network` (see `cmd/nodevault_hooks.go`'s
  comment on `config.VaultConfigured`/`VaultVerify`, the identical pattern),
  so `internal/worker`'s `config.LoadDeviceCredsConverged` reaches the same
  order through `config.DeviceConfigDirsHook`, wired at `cmd` init
  (`cmd/devicecreds_hooks.go`) — not by importing `network` directly.

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
- Global config path differs (see `ConfigDir()` above) — but only when running as root; a normal invocation uses `~/.citadel-cli` like everywhere else

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
- Config lives under `%LOCALAPPDATA%\Citadel` — user-local even when elevated (see `ConfigDir()` above)
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
- **tsnet syspolicy Access Denied (#92)**: Fixed. `citadel init` previously failed on Windows in non-interactive sessions (WinRM, services) because tsnet's `syspolicy` package tried to acquire a Group Policy read lock via `EnterCriticalPolicySection()`, which requires an interactive logon session. The fix calls `gp.RestrictPolicyLocks()` before `tsnet.Server.Start()` (same approach as `tailscaled_windows.go`), then lifts the restriction after startup. See `internal/network/syspolicy_windows.go`.

### Windows E2E Test Infrastructure

A remote E2E test script (`scripts/windows-e2e-test.sh`) validates the full first-time user experience on a Windows machine via WinRM. It tests: clean → install → init → verify.

**Test machine**: `192.168.2.207` (DESKTOP-6UKHJAN, Windows 11, 8 GiB RAM, user: `acewin`)

**WinRM prerequisites** (one-time on the Windows machine, elevated PowerShell):
```powershell
Enable-PSRemoting -Force
winrm set winrm/config/service/auth '@{Basic="true"}'
winrm set winrm/config/service '@{AllowUnencrypted="true"}'
New-NetFirewallRule -Name "WinRM-HTTP" -DisplayName "WinRM HTTP" -Protocol TCP -LocalPort 5985 -Action Allow
```

**Running**:
```bash
# Requires pywinrm in a venv
PYTHON=~/.venvs/winrm/bin/python3 ./scripts/windows-e2e-test.sh \
  --host 192.168.2.207 --user acewin --password '***' --authkey tskey-auth-xxx
```

**Current test status** (as of v2.3.0):
| Phase | Status | Notes |
|-------|--------|-------|
| Clean | PASS | Removes Docker, Citadel dirs, PATH, WSL |
| Install | PASS | install.ps1 works, binary at `%LOCALAPPDATA%\Citadel` |
| Provision | PASS | Fixed in #92 (syspolicy lock restriction) |
| Verify | PENDING | Needs re-run after #92 fix |

### Build Script Updates

**Windows Binary Packaging** (`build.sh`):
- Detects Windows via `mingw*|msys*|cygwin*` in uname output
- Builds with `.exe` extension: `citadel.exe`
- Packages using `.zip` instead of `.tar.gz`
- Cross-compilation: `GOOS=windows GOARCH=amd64 go build -o citadel.exe`
- Release artifacts: `citadel_VERSION_windows_amd64.zip`
