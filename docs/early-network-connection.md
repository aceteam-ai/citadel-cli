# Early Network Connection After Device Authorization

## Overview

This change improves the `citadel init` flow by connecting to the network immediately after device authorization succeeds, rather than waiting for service selection and system provisioning to complete.

## Problem

Previously, the `citadel init` flow would:
1. Run device authorization (user approves at aceteam.ai/device)
2. Prompt for service selection (vllm, ollama, etc.)
3. Prompt for node name
4. Run system provisioning (Docker, NVIDIA toolkit, Tailscale)
5. Connect to network
6. Start services

This caused friction because:
- Users had to wait through multiple prompts after authorization before seeing network connection
- If Tailscale wasn't installed, the connection would fail late in the process
- macOS users with App Store Tailscale installation couldn't use the CLI

## Solution

The new flow connects immediately after authorization:
1. Run device authorization
2. **Get node name (hostname default)**
3. **Install Tailscale if needed**
4. **Connect to network immediately**
5. Prompt for service selection
6. Run system provisioning (Tailscale step skipped)
7. Start services

## macOS Tailscale Detection

Added comprehensive Tailscale CLI detection for macOS:

```go
// Check order:
1. PATH lookup (exec.LookPath)
2. /opt/homebrew/bin/tailscale     // Homebrew (Apple Silicon)
3. /usr/local/bin/tailscale        // Homebrew (Intel)
4. /Applications/Tailscale.app/Contents/MacOS/Tailscale  // App Store
```

This allows users who installed Tailscale from the Mac App Store to use `citadel init` without needing to install via Homebrew.

## Files Changed

| File | Changes |
|------|---------|
| `cmd/login.go` | Added `getTailscalePath()` helper, updated all tailscale commands to use detected path |
| `cmd/init.go` | Restructured flow for early network connection after device auth |
| `internal/nexus/network_helpers.go` | Updated `getTailscaleCLI()` with macOS path detection |

## Behavior Changes

### Device Authorization Flow
- After "Authorization Successful", immediately connects to network
- No longer waits for service selection prompt

### Authkey Flag (`--authkey`)
- Also connects immediately when authkey is provided via flag
- Consistent behavior with device auth flow

### Network-Only Mode (`--network-only`)
- Now respects early connection state
- If already connected via device auth, exits immediately

### System Provisioning
- Tailscale installation step is skipped if already installed during early connection
- Other provisioning steps (Docker, NVIDIA) unchanged

## Testing

To test the changes:

1. **macOS with App Store Tailscale**:
   ```bash
   sudo citadel init
   # Should detect /Applications/Tailscale.app/Contents/MacOS/Tailscale
   ```

2. **macOS without Tailscale**:
   ```bash
   sudo citadel init
   # Should install via Homebrew, then connect immediately after device auth
   ```

3. **Device auth flow**:
   ```bash
   sudo citadel init
   # After "Authorization Successful", should see "Connecting to network"
   # BEFORE service selection prompt
   ```
