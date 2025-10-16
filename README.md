# Citadel CLI

The `citadel` CLI is the on-premise agent and administrator's toolkit for the AceTeam Sovereign Compute Fabric. It allows you to securely connect your own hardware to your AceTeam account, making your resources available to your private workflows.

## Core Concepts

- **AceTeam:** The cloud-based control plane where you design and manage workflows.
- **Citadel:** The on-premise agent you run on your own hardware (the "node").
- **Nexus:** The secure coordination server (e.g., `nexus.aceteam.ai`) that manages the network, built on Headscale.
- **`citadel.yaml`:** The manifest file that declares a node's identity and the services it provides.

## Installation

Currently, the `citadel` binary must be built from source.

```bash
# This will create binaries for Linux (amd64 and arm64) in the ./build directory.
./build.sh
```

You can then copy the appropriate binary (`./build/linux-amd64/citadel`) to your server.

## Command Reference

### Node Setup & Provisioning

These commands are used to prepare a new server and connect it to the network.

| Command                             | Description                                                                                                                                    |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel bootstrap --authkey <key>` | **(Run with sudo)** Provisions a fresh Ubuntu server. Installs Docker, NVIDIA drivers, and Tailscale, then brings the node online. Idempotent. |
| `citadel login`                     | Authenticates your machine interactively via a browser-based login flow. Useful for local machines or development environments.                |
| `citadel up`                        | Brings a node online. Reads `citadel.yaml`, starts all defined services, and launches the agent to listen for jobs from Nexus.                 |
| `citadel up --authkey <key>`        | Brings a node online non-interactively using a pre-authenticated key. Ideal for automated setups and scripts.                                  |
| `citadel down`                      | Stops and removes all services defined in `citadel.yaml`.                                                                                      |

### Node Operation & Monitoring

These commands are used to inspect and manage a running node.

| Command                       | Description                                                                                                                              |
| ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel status`              | Provides a comprehensive health check dashboard, showing system vitals (CPU, RAM, Disk), GPU status, network status, and service status. |
| `citadel logs <service-name>` | Streams the logs for a specific service defined in `citadel.yaml`. Supports `-f` to follow the log output.                               |
| `citadel nodes`               | Connects to the Nexus API and lists all nodes currently registered in your compute fabric, showing their status and network IP.          |
| `citadel run <service-name>`  | Runs a pre-packaged, ad-hoc service (e.g., `ollama`, `vllm`) without needing a `citadel.yaml` manifest. Great for quick tests.           |
| `citadel version`             | Prints the current version of the CLI.                                                                                                   |

---

## The `citadel.yaml` Manifest

The `citadel.yaml` file is the heart of a node's configuration. It defines the node's identity on the network and specifies which long-running services it should manage.

**Example `citadel.yaml`:**

```yaml
# The 'node' key defines the identity of this machine on the network.
node:
  # The human-readable name that will appear in the network (e.g., in `citadel nodes`).
  name: ubuntu-3090-node
  # Tags for filtering or identification in the AceTeam UI.
  tags: [gpu, 3090, training]

# 'services' is a list of long-running services for Citadel to manage.
# If this section is omitted, the node will connect to the network but run no services.
services:
  # Each service needs a unique name.
  - name: llamacpp
    # The path to the Docker Compose file for this service.
    compose_file: ./services/llamacpp.yml

  - name: ollama
    compose_file: ./services/ollama.yml

  - name: vllm
    compose_file: ./services/vllm.yml
```

---

## Example Workflow: Provisioning a New GPU Node

This workflow shows how to take a fresh Ubuntu server and turn it into a fully operational Citadel node.

1.  **Generate an Auth Key:**
    Log in to your Nexus/Headscale admin panel and generate a new, single-use, non-expiring authentication key.

2.  **Prepare the Server:**
    On the new server, copy the `citadel` binary and the service compose files (e.g., `llamacpp.yml`, `ollama.yml`) you intend to run. Create your `citadel.yaml` file as shown in the example above.

3.  **Bootstrap the Node:**
    Run the `bootstrap` command with your auth key. This single command handles all system-level setup.

    ```bash
    # Replace the authkey with the one you generated.
    sudo ./citadel bootstrap --authkey tskey-auth-k1A2b3C4d5E6f...
    ```

    This command will:

    - Install Docker, NVIDIA drivers, and other dependencies.
    - Add your user to the `docker` group.
    - Automatically call `citadel up` to join the network and start your services.

4.  **Verify the Status:**
    Once the bootstrap is complete, you can check the node's health at any time.

    ```bash
    ./citadel status
    ```

    You should see `🟢 ONLINE` for the network connection and `🟢 RUNNING` for all your configured services. Your node is now ready to accept jobs from the AceTeam control plane.
