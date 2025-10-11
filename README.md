# Citadel CLI

The `citadel` CLI is the on-premise agent and administrator's toolkit for the AceTeam Sovereign Compute Fabric. It allows you to securely connect your own hardware to your AceTeam account, making your resources available to your private workflows.

## Core Concepts

- **AceTeam:** The cloud-based control plane where you design and manage workflows.
- **Citadel:** The on-premise agent you run on your own hardware (the "node").
- **Nexus:** The secure coordination server (e.g., `nexus.aceteam.ai`) that manages the network.
- **citadel.yaml:** The manifest file that declares a node's identity and the services it provides.

## Quick Start

1.  **Login:** Authenticate the CLI with your AceTeam account.
    ```bash
    ./citadel login --nexus https://nexus.aceteam.ai
    ```

2.  **Configure:** Create a `citadel.yaml` file to define your node and its services.

3.  **Launch:** Bring the node online.
    ```bash
    ./citadel up
    ```

## Building from Source

To build the binary:

```bash
./build.sh
```

This will create binaries for Linux (amd64 and arm64) in the `./build` directory.
