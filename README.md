# Citadel CLI

The `citadel` CLI is the on-premise agent and administrator's toolkit for the AceTeam Sovereign Compute Fabric. It allows you to securely connect your own hardware to your AceTeam account, making your resources available to your private workflows.

## Core Concepts

- **AceTeam:** The cloud-based control plane where you design and manage workflows.
- **Citadel:** The on-premise agent you run on your own hardware (the "node").
- **Nexus:** The secure coordination server (e.g., `nexus.aceteam.ai`) that manages the network.
- **`citadel.yaml`:** The manifest file that declares a node's identity and the services it provides. This file is **automatically generated** by the `init` command.

## Installation

### One-Line Installer (Recommended)

#### Linux / macOS

```bash
curl -fsSL https://get.aceteam.ai/citadel.sh | bash
```

This installs to `~/.local/bin` and automatically configures your PATH. For system-wide install, use `sudo bash` instead.

### Manual Installation

#### Linux / macOS

1.  Go to the [**Releases Page**](https://github.com/aceteam-ai/citadel-cli/releases).
2.  Download the latest `.tar.gz` archive for your architecture (e.g., `citadel_vX.Y.Z_linux_amd64.tar.gz`).
3.  Extract the archive and place the `citadel` binary in your `PATH`.

    ```bash
    tar -xvf citadel_vX.Y.Z_linux_amd64.tar.gz
    mv citadel ~/.local/bin/    # User-local (no sudo)
    # or: sudo mv citadel /usr/local/bin/  # System-wide
    ```

#### Windows

**Option 1: One-Line Installer (Recommended)**

Open PowerShell and run:

```powershell
iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex
```

This will automatically download, install, and add Citadel to your PATH.

**Option 2: Windows Package Manager (winget)**

Once published to winget (coming soon):

```powershell
winget install AceTeam.Citadel
```

**Option 3: Manual Installation**

1.  Go to the [**Releases Page**](https://github.com/aceteam-ai/citadel-cli/releases).
2.  Download the latest `.zip` archive for Windows (e.g., `citadel_vX.Y.Z_windows_amd64.zip`).
3.  Extract the archive and place `citadel.exe` in your `PATH`.

    ```powershell
    # Extract the zip file
    Expand-Archive citadel_vX.Y.Z_windows_amd64.zip -DestinationPath C:\Tools\citadel

    # Add to PATH (PowerShell as Administrator)
    $env:Path += ";C:\Tools\citadel"
    [Environment]::SetEnvironmentVariable("Path", $env:Path, [System.EnvironmentVariableTarget]::Machine)
    ```

### Building from Source

#### Linux / macOS

If you need to build from the latest source code:

```bash
# This will create binaries for your platform in the ./build directory
./build.sh

# Build for all platforms (requires cross-compilation tools)
./build.sh --all
```

#### Windows

**Quick Setup (Automated):**

```powershell
# One-command setup: installs Go/Git, clones repo, builds, tests
iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/setup-dev-windows.ps1 | iex
```

See [**WINDOWS_QUICKSTART.md**](WINDOWS_QUICKSTART.md) for a 5-minute getting started guide.

**Manual Build:**

```powershell
# Build for Windows (native PowerShell)
.\build.ps1

# Build for all platforms (requires tar for cross-platform packages)
.\build.ps1 -All

# Quick development build
go build -o citadel.exe .
```

See [**WINDOWS_DEVELOPMENT.md**](WINDOWS_DEVELOPMENT.md) for detailed Windows development setup instructions.

## Releasing (For Maintainers)

The `release.sh` script automates the complete release process:

```bash
# Interactive mode - prompts for version
./release.sh

# Non-interactive mode - specify version
./release.sh v1.2.0
```

### Release Process

The script will:

1. **Validate Environment**
   - Check for uncommitted changes (working directory must be clean)
   - Verify GitHub CLI (`gh`) is installed
   - Validate version format (must be `vX.Y.Z` or `vX.Y.Z-rc1`)

2. **Create and Push Tag**
   - Create a git tag with the specified version
   - Push the tag to the remote repository

3. **Build Artifacts**
   - Run `build.sh` to create binaries for Linux (amd64 and arm64)
   - Generate SHA256 checksums

4. **Create GitHub Release**
   - Generate release notes from commits since the last tag
   - Upload binaries and checksums to GitHub Releases
   - Display the release URL

### Version Numbering

Follow semantic versioning (semver):
- **Major version** (`v2.0.0`): Breaking changes
- **Minor version** (`v1.1.0`): New features, backwards compatible
- **Patch version** (`v1.0.1`): Bug fixes, backwards compatible
- **Pre-release** (`v1.1.0-rc1`): Release candidates for testing

### Manual Release Process

If you need to release manually without the script:

```bash
# 1. Create and push tag
git tag v1.2.0
git push origin v1.2.0

# 2. Build artifacts
./build.sh

# 3. Create GitHub release
gh release create v1.2.0 \
  --title "v1.2.0" \
  --notes "Release notes here" \
  release/citadel_v1.2.0_linux_amd64.tar.gz \
  release/citadel_v1.2.0_linux_arm64.tar.gz \
  release/checksums.txt
```

## Command Reference

### Node Setup & Provisioning

| Command                                                                   | Description                                                                                                                                                                                            |
| :------------------------------------------------------------------------ | :----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel init`                                                            | **(Run with sudo)** The primary command for provisioning a new server. It installs dependencies, interactively prompts for configuration, generates all necessary files, and brings the node online.   |
| `citadel init --authkey <key> --service <name> --node-name <name> --test` | The non-interactive version of `init`, perfect for automation. Allows you to specify the service (`vllm`, `ollama`, `llamacpp`, `none`), set the node name, and run a diagnostic test upon completion. |
| `citadel run [service]`                                                   | Starts services. With no arguments, starts all manifest services. With a service name, adds it to the manifest and starts it.                                                                          |
| `citadel stop [service]`                                                  | Stops services. With no arguments, stops all manifest services. With a service name, stops that specific service.                                                                                       |
| `citadel login`                                                           | **(Run with sudo)** Connects the machine to the network. Interactive prompts by default, or use `--authkey <key>` for non-interactive automation.                                                       |

### Node Operation & Monitoring

| Command                         | Description                                                                                                                                  |
| :------------------------------ | :------------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel status`                | Provides a comprehensive health check dashboard, showing the CLI version, system vitals (CPU, RAM, Disk), GPU status, network, and services. |
| `citadel test --service <name>` | Runs a diagnostic test for a specific service to verify its functionality.                                                                   |
| `citadel logs <service-name>`   | Streams the logs for a specific service defined in `citadel.yaml`. Supports `-f` to follow the log output.                                   |
| `citadel nodes`                 | Connects to the Nexus API and lists all nodes in your compute fabric.                                                                        |
| `citadel run --restart`         | Restarts all services defined in `citadel.yaml`.                                                                                             |
| `citadel version`               | Prints the current version of the CLI.                                                                                                       |
| `citadel terminal-server`       | Starts a WebSocket terminal server for remote browser-based terminal access.                                                                 |

---

## Terminal Service

The Citadel Terminal Service provides WebSocket-based terminal access to nodes, enabling browser-based terminal sessions through the AceTeam web application.

### Quick Start

```bash
# Start the terminal server (requires org-id)
citadel terminal-server --org-id my-org-id

# Start on a custom port with 1-hour idle timeout
citadel terminal-server --org-id my-org-id --port 8080 --idle-timeout 60
```

### Configuration Options

| Flag | Description | Default |
|------|-------------|---------|
| `--org-id` | Organization ID for token validation (required) | - |
| `--port` | WebSocket server port | 7860 |
| `--idle-timeout` | Session idle timeout in minutes | 30 |
| `--shell` | Shell to use for sessions | Platform default |
| `--max-connections` | Maximum concurrent sessions | 10 |

For detailed documentation, see [**docs/terminal-service.md**](docs/terminal-service.md).

---

## Example Workflow: Provisioning a New GPU Node

This workflow shows how to take a fresh Ubuntu server and turn it into a fully operational Citadel node with a single command.

1.  **(Optional) Generate an Auth Key:**
    For automated deployments, log in to your Nexus admin panel and generate a new, single-use, non-expiring authentication key. For interactive setup, you can skip this and log in via your browser.

2.  **Initialize the Node:**
    Copy the `citadel` binary to the new server and run the `init` command. It will handle all system setup, configuration, and service deployment.

    **Interactive Example:**

    ```bash
    # The command will guide you through selecting a service, naming the node,
    # and choosing a connection method (browser or authkey).
    sudo ./citadel init
    ```

    **Automated Example:**
    For scripted deployments, you can provide all options as flags. The `--test` flag is highly recommended to verify the deployment.

    ```bash
    # This command will provision a vLLM node named 'gpu-node-01' and run a test.
    sudo ./citadel init \
      --authkey tskey-auth-k1A2b3C4d5E6f... \
      --service vllm \
      --node-name gpu-node-01 \
      --test
    ```

    After running, `init` will create a `~/citadel-node` directory containing the generated `citadel.yaml` and service files.

3.  **Verify the Status:**
    Once initialization is complete, you can check the node's health at any time.

    ```bash
    # Navigate to the generated directory to manage your node
    cd ~/citadel-node
    ./citadel status
    ```

    You should see `🟢 ONLINE` for the network connection and `🟢 RUNNING` for your configured service. Your node is now ready to accept jobs from the AceTeam control plane.

---

### The `citadel.yaml` Manifest

The `init` command generates this file for you. It defines the node's identity and the service it runs.

**Example `citadel.yaml` (generated for a vLLM node):**

```yaml
node:
  name: gpu-node-01
  tags:
    - gpu
    - provisioned-by-citadel
services:
  - name: vllm
    compose_file: ./services/vllm.yml
```
