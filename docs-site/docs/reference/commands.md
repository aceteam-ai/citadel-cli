---
sidebar_position: 1
title: Command Reference
---

# Command Reference

All Citadel CLI commands organized by category. Run `citadel <command> --help` for full flag documentation, or `man citadel-<command>` if man pages are installed.

## Interactive Mode

Running `citadel` with no subcommand launches the interactive control center. This is the recommended way to use Citadel -- it handles login, network connection, service management, and job processing in a single unified TUI.

```bash
citadel
```

## Setup and Provisioning

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel init` | Join the AceTeam Network and optionally provision the system | `--authkey`, `--provision`, `--service`, `--node-name`, `--verbose` |
| `citadel login` | Authenticate with the AceTeam Network interactively | `--nexus`, `--auth-service` |
| `citadel logout` | Disconnect from the AceTeam Network and clear state | |

## Operation

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel work` | Start the worker (process jobs from the queue) | `--redis-url`, `--queue`, `--status-port`, `--terminal`, `--terminal-port` |
| `citadel run [service]` | Start services from the manifest (or a specific service) | `--restart` |
| `citadel stop [service]` | Stop running services (or a specific service) | |
| `citadel status` | Display the node health dashboard | |
| `citadel logs <service>` | Stream logs from a service | `-f` (follow) |
| `citadel test` | Run diagnostic tests against a service | `--service` |

## Networking

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel nodes` | List all nodes on the AceTeam Network | `--nexus` |
| `citadel peers` | Discover nodes and their capabilities | |
| `citadel call <node> <endpoint>` | Make an HTTP call to a peer node | |
| `citadel ping <node>` | Check if a peer node is reachable | |
| `citadel ssh <node>` | SSH into a peer node via the mesh network | |
| `citadel proxy <node>` | Proxy local traffic to a remote node | |
| `citadel expose` | Expose local services to the fabric network | |

## Update

| Command | Description |
|---------|-------------|
| `citadel update check` | Check if a new version is available |
| `citadel update install` | Download and install the latest version |
| `citadel update status` | Show current update state and version info |
| `citadel update rollback` | Roll back to the previous version |
| `citadel update enable` | Enable automatic daily updates |
| `citadel update disable` | Disable automatic updates |

## Service Management

| Command | Description |
|---------|-------------|
| `citadel service install` | Install Citadel as a system service |
| `citadel service uninstall` | Remove the system service |
| `citadel service start` | Start the system service |
| `citadel service stop` | Stop the system service |
| `citadel service status` | Check the system service status |

## Other

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel version` | Print the CLI version | |
| `citadel terminal-server` | Start a standalone WebSocket terminal server | `--port`, `--test` |

## Global Flags

These flags are available on all commands:

| Flag | Description |
|------|-------------|
| `--help` | Show help for any command |
| `--nexus <url>` | Override the coordination server URL (default: `https://nexus.aceteam.ai`) |
| `--auth-service <url>` | Override the auth service URL (default: `https://aceteam.ai`) |
