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
| `citadel work` | Start the worker (process jobs from the queue) | `--redis-url`, `--queue`, `--status-port`, `--terminal`, `--terminal-port`, `--egress-relay` |
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
| `citadel proxy [local-port] [peer:port]` | Forward a local port to a service on another node | `--bind`, `--max-conns`, `-v`/`--verbose` |
| `citadel socks [port]` | Run a local SOCKS5 proxy that dials out over the AceTeam Network | `--bind`, `--auth`, `--max-conns`, `-v`/`--verbose` |
| `citadel expose <name>` | Expose a local service to other nodes on the network | |
| `citadel unexpose <name>` | Stop exposing a previously exposed service | |
| `citadel up` | Put the whole machine on the network (not just this process) | `--check` |
| `citadel down` | Take the machine back off the network | |

See [Networking](/guides/networking) and [Machine-Wide Network Mode](/guides/machine-wide-mode) for details.

## Egress Relay

Lets another citadel node tunnel its outbound traffic through this node's own internet connection ("your exit, your IP"). Off by default, mesh-only, same-org verified peers only.

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel egress-relay status` | Show the current egress-relay configuration | |
| `citadel egress-relay enable` | Enable the relay (takes effect on the next `citadel work` start) | |
| `citadel egress-relay disable` | Disable the relay (takes effect on the next `citadel work` start) | |
| `citadel egress-relay allow-lan <on\|off>` | Allow (or deny) the relay from CONNECTing into this node's own LAN/mesh | |
| `citadel egress-relay serve` | Run this node as a pure egress relay -- no worker, no Redis -- until interrupted | |

See [Egress Relay](/guides/egress-relay) for full setup instructions and the security model.

## Model Serving Across the Fleet

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel mesh models` | List models served by other nodes on the network | `--json`, `--port` |
| `citadel mesh chat [prompt]` | Send a one-shot chat request to a model on another node | `--model`, `--node` |

See [Model Serving & Mesh Chat](/guides/model-serving-and-mesh-chat) for details.

## Modules and Catalog Services

A "module" is any service Citadel installs and manages by name -- a built-in
engine (`vllm`, `bonsai`, ...) or a third-party catalog service.

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel module install <source>` | Install a module from the catalog or a git source | |
| `citadel module update [name]` | Update an installed module | |
| `citadel module list` | List installed modules | |
| `citadel module info <source>` | Show details for a catalog module | |
| `citadel module start\|stop\|restart <name>` | Recover a single stopped/crashed module without touching its siblings | `--dry-run`, `--expect-node` |
| `citadel module search [query]` | Search the module catalog | |
| `citadel module reservations list` | List active GPU reservations held by exclusive runs | |
| `citadel module reservations release <jobID>` | Manually release a stuck GPU reservation | |

## Identity, Diagnostics, and AI Tool Access

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel whoami` | Show this node's identity (name, network address, platform node ID) | `--json` |
| `citadel services` | Show usage/idle status and footprint for every managed service | |
| `citadel service diagnose <name>` | Inspect a service's container and tail its logs | |
| `citadel pairing-code` | Read back a pending on-node access pairing code | `--json` |
| `citadel mcp` | Bridge this node's tools to an MCP-compatible AI assistant | |

`citadel mcp` lets an AI coding assistant manage this node directly. Run
`citadel init` once, then `claude mcp add aceteam -- citadel mcp` (or the
equivalent for your MCP client) to let an AI agent manage nodes, deploy
models, and run inference on your own hardware.

## Trust Receipts

| Command | Description | Key Flags |
|---------|-------------|-----------|
| `citadel aep verify <receipt.json>` | Verify the ECDSA signature of a signed AEP (AceTeam Execution Proof) receipt, entirely offline | `--json`, `--pubkey`, `--cert`, `--show-canonical` |

See [Trust Receipts & Grounding Checks](/guides/trust-receipts) for the full receipt format and how signing is enabled.

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
| `--node-dir <path>` | Point a command at a specific node directory instead of the default location. Also settable via `CITADEL_NODE_DIR`. Useful for scripts and automation that manage more than one node config from a single machine. |
