# Early Network Connection After Device Authorization

## Overview

`citadel init` connects to the AceTeam Network immediately after device
authorization succeeds, rather than waiting for service selection and system
provisioning to complete.

## Problem

An earlier version of the `citadel init` flow ran:

1. Device authorization (user approves at aceteam.ai/device)
2. Service selection prompt (vllm, ollama, etc.)
3. Node name prompt
4. System provisioning (Docker, NVIDIA toolkit)
5. Network connection
6. Service startup

Users had to sit through several prompts after approving the device before
anything confirmed that the node had actually joined the network, and a
connection failure surfaced late in a long-running flow.

## Solution

The flow connects as soon as the node has an authkey:

1. Device authorization
2. **Node name (hostname default)**
3. **Connect to the network immediately**
4. Service selection prompt
5. System provisioning (Docker, NVIDIA toolkit)
6. Service startup

## No external network client

Network connectivity is provided by the embedded `tsnet` library compiled into
the `citadel` binary (`internal/network/`, see `connectToNetwork` in
`cmd/init.go`). There is no daemon to install, no system VPN to configure, and
no Tailscale/Tailscale.app prerequisite — `citadel login` (or `citadel init`) is
the only step a user runs to put a node on the network. Because tsnet uses
userspace networking, joining the network also needs no root/Administrator
privileges.

The connection is process-scoped: the mesh identity belongs to the `citadel`
process, not to the whole host. Citadel's own subcommands (`citadel ssh`,
`citadel call`, `citadel ping`, `citadel proxy`) route over the mesh, and
`citadel proxy` is the documented way to reach a peer's service from an
unrelated host tool (see `docs-site/docs/guides/networking.md`).

## Behavior

### Device authorization flow

- After "Authorization Successful", the node connects to the network right away.
- It no longer waits for the service selection prompt.

### Authkey flag (`--authkey`)

- Also connects immediately when an authkey is supplied on the command line.
- Consistent with the device auth flow.

### Network-only mode (default)

- Respects the early connection state: if the node connected during device auth,
  `citadel init` exits once the manifest is written.

### System provisioning (`--provision`)

- Provisioning installs Docker and the NVIDIA Container Toolkit only. There is
  no network-client installation step (`cmd/init.go`,
  "Network connectivity is now handled via embedded tsnet library").

## Testing

```bash
# Interactive device auth: after "Authorization Successful" you should see
# "Connecting to network" BEFORE the service selection prompt.
citadel init

# Non-interactive: connects immediately using the supplied key.
citadel init --authkey <key>

# Full provisioning (Docker + NVIDIA toolkit) on a fresh Linux box.
sudo citadel init --provision
```

## Historical note

Before the network client was embedded, this flow detected or installed the
Tailscale CLI (Homebrew on macOS, with a fallback path for the Mac App Store
app) and shelled out to `tailscale up`. Those helpers
(`getTailscalePath`, `getTailscaleCLI`) and the Tailscale provisioning step were
removed when `internal/network/` took over; nothing in the onboarding path
depends on an external client any more.
