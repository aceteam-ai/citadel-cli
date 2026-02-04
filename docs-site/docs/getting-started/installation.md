---
sidebar_position: 1
title: Installation
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Installation

Install the Citadel CLI on your platform of choice. The binary is self-contained with no external dependencies required for basic operation.

## Install Citadel

<Tabs groupId="platform">
<TabItem value="linux-macos" label="Linux / macOS" default>

**One-liner (recommended)**

```bash
curl -fsSL https://get.aceteam.ai/citadel.sh | bash
```

This downloads the latest release, places the binary in your PATH, and verifies the checksum.

**Homebrew (macOS)**

```bash
brew install aceteam-ai/tap/citadel
```

</TabItem>
<TabItem value="windows" label="Windows">

Open PowerShell as Administrator and run:

```powershell
iwr -useb https://raw.githubusercontent.com/aceteam-ai/citadel-cli/main/install.ps1 | iex
```

The installer places `citadel.exe` in `%LOCALAPPDATA%\Citadel` and adds it to your PATH.

</TabItem>
<TabItem value="go" label="Go Install">

If you have Go 1.21+ installed:

```bash
go install github.com/aceteam-ai/citadel-cli/cmd/citadel@latest
```

The binary is placed in your `$GOPATH/bin` directory.

</TabItem>
<TabItem value="source" label="From Source">

Clone the repository and build:

```bash
git clone https://github.com/aceteam-ai/citadel-cli.git
cd citadel-cli
```

<Tabs groupId="build-platform">
<TabItem value="unix" label="Linux / macOS" default>

```bash
./build.sh
```

The binary is created in `./build/`.

</TabItem>
<TabItem value="win" label="Windows">

```powershell
.\build.ps1
```

</TabItem>
</Tabs>

</TabItem>
</Tabs>

## Verify the installation

```bash
citadel version
```

You should see output showing the installed version, for example:

```
citadel version v2.3.0
```

## System requirements

| Requirement | Details |
|---|---|
| **Operating system** | Linux (amd64/arm64), macOS (amd64/arm64), or Windows 10+ (amd64) |
| **Docker** | Required for running AI services. Installed automatically by `citadel init --provision`. |
| **GPU** | Optional but recommended. NVIDIA GPUs supported on Linux and Windows (via WSL2). Apple Silicon supported on macOS via Metal. |
| **Network** | Outbound HTTPS access to `aceteam.ai` and `nexus.aceteam.ai` |

## Next steps

Once installed, head to the [Quick Start](./quick-start.md) to connect your node to the AceTeam Network.
